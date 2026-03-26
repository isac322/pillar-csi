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

// dockerexec.go — Remote Docker exec helper for ZFS + NVMe-oF e2e tests.
//
// DockerHostExec lets e2e tests run privileged shell commands on the remote
// Docker host (DOCKER_HOST=tcp://10.111.0.1:2375) without requiring root on
// the test-runner process itself.  It works by:
//
//  1. Starting a single, long-lived "host-exec" container on the remote Docker
//     host with the following flags:
//       --privileged        full capabilities including CAP_SYS_ADMIN
//       --pid=host          shares the host PID namespace (required for nsenter)
//       --network=host      shares the host network namespace
//       -v /:/host:rslave   read-write access to the host root filesystem
//
//  2. Forwarding every Exec call as a `docker exec … sh -c <command>` call
//     into that container.
//
//  3. For operations that need to run in the host's mount namespace (e.g.
//     `zfs`, `modprobe`, `nvmetcli`), ExecOnHost wraps the command in
//     `nsenter -t 1 -m -u -i -n -p -- sh -c <command>` so it executes in the
//     host's namespaces, not the container's.
//
// Each exec call returns an ExecResult with separate Stdout, Stderr strings
// and the integer ExitCode.  A non-zero ExitCode is never a Go error; it is
// the caller's responsibility to check.  A Go error is returned only when the
// exec mechanism itself breaks (daemon unreachable, container gone, etc.).
//
// Typical usage in a Ginkgo BeforeSuite / AfterSuite:
//
//	var hostExec *framework.DockerHostExec
//
//	var _ = BeforeSuite(func() {
//	    var err error
//	    hostExec, err = framework.NewDockerHostExec(ctx, "tcp://10.111.0.1:2375")
//	    Expect(err).NotTo(HaveOccurred())
//	})
//
//	var _ = AfterSuite(func() {
//	    Expect(hostExec.Close()).To(Succeed())
//	})
//
//	It("lists ZFS datasets", func() {
//	    res, err := hostExec.ExecOnHost(ctx, "zfs list")
//	    Expect(err).NotTo(HaveOccurred())
//	    Expect(res.ExitCode).To(BeZero())
//	    Expect(res.Stdout).To(ContainSubstring("NAME"))
//	})

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

// hostExecImage is the container image used for the privileged helper.
// debian:bookworm-slim ships nsenter (util-linux) on both amd64 and arm64.
const hostExecImage = "debian:bookworm-slim"

// hostExecContainerName is a deterministic name for the helper container.
// A fixed name means that interrupted test runs leave at most one stale
// container behind, which NewDockerHostExec removes unconditionally.
const hostExecContainerName = "pillar-csi-host-exec"

// ─────────────────────────────────────────────────────────────────────────────
// ExecResult
// ─────────────────────────────────────────────────────────────────────────────

// ExecResult holds the complete output of one remote shell command.
type ExecResult struct {
	// Stdout is everything written to the command's standard-output stream.
	Stdout string

	// Stderr is everything written to the command's standard-error stream.
	Stderr string

	// ExitCode is the exit status returned by the command (0 = success).
	// A non-zero ExitCode is not represented as a Go error; callers must
	// inspect this field themselves.
	ExitCode int
}

// Success returns true when ExitCode is zero.
func (r ExecResult) Success() bool { return r.ExitCode == 0 }

// String returns a human-readable summary useful for test failure messages.
func (r ExecResult) String() string {
	return fmt.Sprintf("exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
}

// ─────────────────────────────────────────────────────────────────────────────
// DockerHostExec
// ─────────────────────────────────────────────────────────────────────────────

// DockerHostExec runs privileged shell commands on a remote Docker host by
// forwarding them into a long-lived privileged container via `docker exec`.
//
// Construct with NewDockerHostExec; release with Close.
type DockerHostExec struct {
	// containerID is the Docker container ID returned by `docker run`.
	// We also use hostExecContainerName for exec calls so that the object
	// remains functional even if the caller's view of the ID drifts.
	containerID string

	// dockerHost is the Docker daemon endpoint (e.g. "tcp://10.111.0.1:2375").
	// It is injected as DOCKER_HOST into every sub-process environment.
	dockerHost string
}

// NewDockerHostExec starts a privileged "host-exec" container on the remote
// Docker host and returns a DockerHostExec ready to accept Exec / ExecOnHost
// calls.
//
// The container is started with:
//
//	--privileged        full Linux capabilities (CAP_SYS_ADMIN, …)
//	--pid=host          see host PID namespace (required by nsenter)
//	--network=host      use host network stack directly
//	-v /:/host:rslave   host root accessible at /host inside the container
//
// Any pre-existing container named hostExecContainerName is removed first so
// that stale containers from previous interrupted runs do not block creation.
func NewDockerHostExec(ctx context.Context, dockerHost string) (*DockerHostExec, error) {
	env := buildDockerEnv(dockerHost)

	// ── 1. Remove any stale container from a previous run ─────────────────
	rmCmd := exec.CommandContext(ctx, "docker", "rm", "-f", hostExecContainerName)
	rmCmd.Env = env
	_ = rmCmd.Run() // intentionally ignore error; container may not exist

	// ── 2. Start the privileged helper container ───────────────────────────
	runArgs := []string{
		"run", "--detach",
		"--name", hostExecContainerName,
		"--privileged",
		"--pid=host",
		"--network=host",
		// Mount the host root filesystem so that host binaries are accessible
		// via /host, and so that chroot / nsenter can reach them.
		"-v", "/:/host:rslave",
		hostExecImage,
		// Keep the container alive with a minimal sleep loop.
		"sleep", "infinity",
	}
	runCmd := exec.CommandContext(ctx, "docker", runArgs...)
	runCmd.Env = env
	out, err := runCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf(
			"dockerexec: start privileged container %q (image %s): %s: %w",
			hostExecContainerName, hostExecImage,
			strings.TrimSpace(string(out)), err,
		)
	}

	containerID := strings.TrimSpace(string(out))
	return &DockerHostExec{
		containerID: containerID,
		dockerHost:  dockerHost,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Exec
// ─────────────────────────────────────────────────────────────────────────────

// Exec runs the given shell command inside the privileged helper container and
// returns an ExecResult with separate Stdout, Stderr, and ExitCode fields.
//
// The command is executed via `sh -c <command>` so that shell constructs
// (pipes, redirections, variable expansions) work as expected.
//
// Exec returns a Go error only when the exec mechanism itself fails (container
// not running, Docker daemon unreachable, etc.).  Command failures are
// represented as a non-zero ExecResult.ExitCode, not as Go errors.
func (h *DockerHostExec) Exec(ctx context.Context, command string) (ExecResult, error) {
	return h.execArgs(ctx, "sh", "-c", command)
}

// ExecOnHost runs the given shell command on the Docker host system (not
// inside the container's namespaces) by nsenter-ing into all host namespaces
// from within the privileged container.
//
// Use ExecOnHost for commands that must run directly on the host, such as:
//   - zfs create / zfs destroy (host ZFS kernel module)
//   - modprobe nvmet (load kernel modules)
//   - reading/writing to /sys/kernel/config/nvmet (configfs)
//
// The implementation wraps <command> in:
//
//	nsenter -t 1 -m -u -i -n -p -- sh -c <command>
//
// which enters the mount (-m), UTS (-u), IPC (-i), network (-n) and PID (-p)
// namespaces of PID 1 (the host init process) before executing the command.
// With --pid=host on the container, PID 1 is always the host's init process.
func (h *DockerHostExec) ExecOnHost(ctx context.Context, command string) (ExecResult, error) {
	// nsenter flags:
	//   -t 1   : target PID 1 (host init / systemd)
	//   -m     : enter host mount namespace
	//   -u     : enter host UTS namespace (hostname)
	//   -i     : enter host IPC namespace
	//   -n     : enter host network namespace
	//   -p     : enter host PID namespace
	return h.execArgs(ctx,
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		"sh", "-c", command,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Close
// ─────────────────────────────────────────────────────────────────────────────

// Close stops and removes the privileged helper container.
//
// Close is idempotent: calling it on an already-removed container is silently
// ignored.  It is safe to call Close from AfterSuite even when the container
// was never successfully created (containerID is empty).
func (h *DockerHostExec) Close() error {
	if h == nil || h.containerID == "" {
		return nil
	}
	cmd := exec.Command("docker", "rm", "-f", h.containerID) //nolint:gosec
	cmd.Env = buildDockerEnv(h.dockerHost)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dockerexec: remove container %q: %s: %w",
			h.containerID, strings.TrimSpace(string(out)), err)
	}
	h.containerID = ""
	return nil
}

// ContainerID returns the Docker container ID of the running helper container.
// Returns an empty string if the container has not been started or has been
// closed.
func (h *DockerHostExec) ContainerID() string {
	if h == nil {
		return ""
	}
	return h.containerID
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// execArgs runs `docker exec <containerName> <argv...>` and returns an
// ExecResult with separate Stdout, Stderr, and ExitCode.
//
// Stdout and Stderr are captured independently; -t (pseudo-TTY) is NOT
// passed so that Docker does not merge the two streams.
//
// A Go error is returned only when the docker exec mechanism itself fails
// (container gone, daemon unreachable).  A non-zero exit code from the
// command is reflected in ExecResult.ExitCode only.
func (h *DockerHostExec) execArgs(ctx context.Context, argv ...string) (ExecResult, error) {
	// Build: docker exec <containerName> <argv...>
	// We use the well-known name rather than the ID so that the object
	// continues to work even when containerID was not captured (e.g. test
	// helpers that reuse a pre-existing container by name).
	args := append([]string{"exec", hostExecContainerName}, argv...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec
	cmd.Env = buildDockerEnv(h.dockerHost)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The command inside the container exited non-zero.
			// This is not a mechanism error; propagate as ExitCode.
			exitCode = exitErr.ExitCode()
			err = nil
		} else {
			// Something else went wrong (context cancelled, docker binary
			// not found, …). Return a proper Go error.
			return ExecResult{}, fmt.Errorf(
				"dockerexec: exec in container %q: %w (stderr: %s)",
				hostExecContainerName, err,
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

// buildDockerEnv returns os.Environ() with DOCKER_HOST set to dockerHost.
// Any pre-existing DOCKER_HOST entry in the current environment is replaced so
// that sub-processes always reach the correct remote Docker daemon, regardless
// of what the caller's shell exported.
func buildDockerEnv(dockerHost string) []string {
	const prefix = "DOCKER_HOST="
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+dockerHost)
}
