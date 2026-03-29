//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

// nvmeof_configfs.go — Configfs verification helpers for NVMe-oF e2e tests.
//
// NVMeConfigfs provides methods for inspecting the NVMe-oF target entries that
// pillar-agent writes into the Linux nvmet configfs tree.  Because the agent
// is started with --configfs-root=/tmp in e2e tests (to avoid requiring a real
// kernel nvmet module), entries live at <configfsRoot>/nvmet/ inside the agent
// Docker container rather than at /sys/kernel/config/nvmet on the host.
//
// The helpers reach the agent container by running `docker exec <container>
// …` against the remote Docker daemon, using the same mechanism as
// DockerHostExec.
//
// # Configfs layout (abbreviated)
//
//	<configfsRoot>/nvmet/
//	  subsystems/<nqn>/
//	    attr_allow_any_host      "0" or "1"
//	    namespaces/<nsid>/
//	      device_path            path to the block device
//	      enable                 "1" when active
//	  ports/<portID>/
//	    addr_trtype              "tcp"
//	    addr_trsvcid             TCP port number
//	    subsystems/<nqn>/        → symlink to ../../subsystems/<nqn>
//
// # Port ID derivation
//
// NVMe-oF configfs port IDs are arbitrary integers.  pillar-agent derives a
// stable ID from the bind address and TCP port number using a 32-bit FNV-1a
// hash (see stablePortID in internal/agent/nvmeof/configfs.go).  The test
// helper StablePortID replicates that algorithm so tests can predict which
// port directory to check without querying the agent.
//
// # Typical usage in a Ginkgo It block
//
//	agentCfg := framework.NewNVMeConfigfs(testEnv.DockerHost, containerName, "/tmp")
//
//	nqn    := exportResp.GetExportInfo().GetTargetId()
//	nsid   := uint32(1)
//	portID := framework.StablePortID("0.0.0.0", 4421)
//
//	exists, err := agentCfg.SubsystemExists(ctx, nqn)
//	Expect(err).NotTo(HaveOccurred())
//	Expect(exists).To(BeTrue())
//
//	exists, err = agentCfg.NamespaceExists(ctx, nqn, nsid)
//	Expect(err).NotTo(HaveOccurred())
//	Expect(exists).To(BeTrue())
//
//	exists, err = agentCfg.PortExists(ctx, portID)
//	Expect(err).NotTo(HaveOccurred())
//	Expect(exists).To(BeTrue())

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NVMeConfigfs
// ─────────────────────────────────────────────────────────────────────────────

// NVMeConfigfs inspects NVMe-oF configfs entries inside a named Docker
// container by executing `docker exec` commands against the remote Docker
// daemon.
//
// The configfsRoot field specifies the root directory where the nvmet subtree
// lives inside the container.  In production this is "/sys/kernel/config"; in
// e2e tests the agent is started with --configfs-root=/tmp so the subtree
// lives at /tmp/nvmet/.
//
// Construct with NewNVMeConfigfs; all methods are safe to call concurrently.
type NVMeConfigfs struct {
	// dockerHost is the Docker daemon endpoint, e.g. "tcp://localhost:2375".
	// Injected as DOCKER_HOST into every docker exec sub-process.
	dockerHost string

	// container is the Docker container name or ID to exec into.
	// The agent container is typically named "<clusterName>-agent".
	container string

	// configfsRoot is the base path of the configfs mount inside the container,
	// e.g. "/tmp" or "/sys/kernel/config".
	// The nvmet subtree is located at <configfsRoot>/nvmet/.
	configfsRoot string
}

// NewNVMeConfigfs creates a new NVMeConfigfs that inspects entries inside the
// named container via the Docker daemon at dockerHost.
//
// Parameters:
//
//	dockerHost   – Docker daemon endpoint (e.g. "tcp://localhost:2375")
//	container    – Docker container name or ID (e.g. "pillar-csi-e2e-agent")
//	configfsRoot – root of the configfs mount inside the container
//	               ("/tmp" for e2e tests; "/sys/kernel/config" in production)
func NewNVMeConfigfs(dockerHost, container, configfsRoot string) *NVMeConfigfs {
	return &NVMeConfigfs{
		dockerHost:   dockerHost,
		container:    container,
		configfsRoot: configfsRoot,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Path helpers
// ─────────────────────────────────────────────────────────────────────────────

// nvmetRoot returns the path to the nvmet subtree within the container, e.g.
// "/tmp/nvmet" or "/sys/kernel/config/nvmet".
func (c *NVMeConfigfs) nvmetRoot() string {
	return filepath.Join(c.configfsRoot, "nvmet")
}

// SubsystemPath returns the configfs directory path for the given subsystem NQN.
//
//	e.g. /tmp/nvmet/subsystems/nqn.2026-01.io.pillar-csi:pvc-abc123
func (c *NVMeConfigfs) SubsystemPath(nqn string) string {
	return filepath.Join(c.nvmetRoot(), "subsystems", nqn)
}

// NamespacePath returns the configfs directory path for the given namespace.
//
//	e.g. /tmp/nvmet/subsystems/<nqn>/namespaces/1
func (c *NVMeConfigfs) NamespacePath(nqn string, nsid uint32) string {
	return filepath.Join(c.SubsystemPath(nqn), "namespaces", fmt.Sprintf("%d", nsid))
}

// PortPath returns the configfs directory path for the given port ID.
//
//	e.g. /tmp/nvmet/ports/12345
func (c *NVMeConfigfs) PortPath(portID uint32) string {
	return filepath.Join(c.nvmetRoot(), "ports", fmt.Sprintf("%d", portID))
}

// ─────────────────────────────────────────────────────────────────────────────
// Existence checks
// ─────────────────────────────────────────────────────────────────────────────

// SubsystemExists returns (true, nil) when the configfs subsystem directory
// for the given NQN exists inside the container.
//
// It executes `test -d <path>` via docker exec.  A non-zero exit code (path
// absent) returns (false, nil); a Go error is returned only when the docker
// exec mechanism itself fails.
func (c *NVMeConfigfs) SubsystemExists(ctx context.Context, nqn string) (bool, error) {
	return c.pathIsDir(ctx, c.SubsystemPath(nqn))
}

// NamespaceExists returns (true, nil) when the configfs namespace directory
// for the given subsystem NQN and namespace ID exists inside the container.
func (c *NVMeConfigfs) NamespaceExists(ctx context.Context, nqn string, nsid uint32) (bool, error) {
	return c.pathIsDir(ctx, c.NamespacePath(nqn, nsid))
}

// PortExists returns (true, nil) when the configfs port directory for the
// given port ID exists inside the container.
func (c *NVMeConfigfs) PortExists(ctx context.Context, portID uint32) (bool, error) {
	return c.pathIsDir(ctx, c.PortPath(portID))
}

// ─────────────────────────────────────────────────────────────────────────────
// Attribute readers
// ─────────────────────────────────────────────────────────────────────────────

// ReadNamespaceDevicePath reads the device_path pseudo-file of the given
// namespace and returns its trimmed content (e.g. "/dev/zvol/e2e-pool/myvol").
//
// The file is written by the agent during ExportVolume and contains the path
// to the backing block device.
func (c *NVMeConfigfs) ReadNamespaceDevicePath(ctx context.Context, nqn string, nsid uint32) (string, error) {
	path := filepath.Join(c.NamespacePath(nqn, nsid), "device_path")
	out, err := c.readFile(ctx, path)
	if err != nil {
		return "", fmt.Errorf("ReadNamespaceDevicePath(%q, %d): %w", nqn, nsid, err)
	}
	return strings.TrimSpace(out), nil
}

// ReadSubsystemAllowAnyHost reads the attr_allow_any_host pseudo-file of the
// given subsystem and returns true when its value is "1" (any initiator may
// connect) or false when it is "0" (ACL enforcement is active).
//
// This file is written by the agent during ExportVolume.
func (c *NVMeConfigfs) ReadSubsystemAllowAnyHost(ctx context.Context, nqn string) (bool, error) {
	path := filepath.Join(c.SubsystemPath(nqn), "attr_allow_any_host")
	out, err := c.readFile(ctx, path)
	if err != nil {
		return false, fmt.Errorf("ReadSubsystemAllowAnyHost(%q): %w", nqn, err)
	}
	val := strings.TrimSpace(out)
	switch val {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("ReadSubsystemAllowAnyHost(%q): unexpected value %q (want \"0\" or \"1\")", nqn, val)
	}
}

// ReadPortTrsvcid reads the addr_trsvcid pseudo-file of the given port entry
// and returns its trimmed content (the TCP port number as a string, e.g. "4421").
func (c *NVMeConfigfs) ReadPortTrsvcid(ctx context.Context, portID uint32) (string, error) {
	path := filepath.Join(c.PortPath(portID), "addr_trsvcid")
	out, err := c.readFile(ctx, path)
	if err != nil {
		return "", fmt.Errorf("ReadPortTrsvcid(portID=%d): %w", portID, err)
	}
	return strings.TrimSpace(out), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// StablePortID — public counterpart of internal/agent/nvmeof stablePortID
// ─────────────────────────────────────────────────────────────────────────────

// StablePortID derives the deterministic configfs port ID that pillar-agent
// assigns to the given bind address and TCP port number.
//
// The formula mirrors stablePortID in internal/agent/nvmeof/configfs.go:
// a 32-bit FNV-1a hash of the bind address bytes, XOR'd with the port value,
// then folded into [1, 65535].
//
// E2E tests call this function to predict which port directory to inspect
// without having to query the agent:
//
//	portID := framework.StablePortID("0.0.0.0", 4421)
//	exists, err := agentCfg.PortExists(ctx, portID)
func StablePortID(addr string, port int32) uint32 {
	// FNV-1a 32-bit constants.
	const (
		fnvOffset uint32 = 2166136261
		fnvPrime  uint32 = 16777619
	)
	h := fnvOffset
	for i := range len(addr) {
		h ^= uint32(addr[i]) //nolint:gosec
		h *= fnvPrime
	}
	h ^= uint32(port) //nolint:gosec // G115: port in [1,65535]; overflow intentional for FNV hash
	h *= fnvPrime
	return h%65535 + 1 // [1, 65535]
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// pathIsDir runs `docker exec <container> test -d <path>` and returns
// (true, nil) on exit 0, (false, nil) on exit 1 (path absent), or
// (false, err) when the docker exec mechanism fails.
func (c *NVMeConfigfs) pathIsDir(ctx context.Context, path string) (bool, error) {
	res, err := c.containerExec(ctx, "test", "-d", path)
	if err != nil {
		return false, fmt.Errorf("pathIsDir %q: %w", path, err)
	}
	return res.ExitCode == 0, nil
}

// readFile runs `docker exec <container> cat <path>` and returns the combined
// stdout as a string.  A non-zero exit code from cat is returned as an error.
func (c *NVMeConfigfs) readFile(ctx context.Context, path string) (string, error) {
	res, err := c.containerExec(ctx, "cat", path)
	if err != nil {
		return "", fmt.Errorf("readFile %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("readFile %q: exit %d stderr=%q", path, res.ExitCode, res.Stderr)
	}
	return res.Stdout, nil
}

// containerExec runs `docker exec <container> <argv...>` against the remote
// Docker daemon and returns an ExecResult with separate Stdout, Stderr, and
// ExitCode.
//
// A Go error is returned only when the docker exec mechanism itself fails
// (container not running, Docker daemon unreachable, etc.).  Command failures
// are reflected as a non-zero ExecResult.ExitCode, not as Go errors.
func (c *NVMeConfigfs) containerExec(ctx context.Context, argv ...string) (ExecResult, error) {
	args := append([]string{"exec", c.container}, argv...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	cmd.Env = buildDockerEnv(c.dockerHost)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The command inside the container exited non-zero.
			// Report as ExitCode only — not a mechanism error.
			exitCode = exitErr.ExitCode()
			err = nil
		} else {
			return ExecResult{}, fmt.Errorf(
				"nvmeof_configfs: exec in container %q: %w (stderr: %s)",
				c.container, err,
				strings.TrimSpace(stderr.String()),
			)
		}
	}

	return ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}
