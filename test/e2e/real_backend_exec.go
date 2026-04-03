package e2e

// real_backend_exec.go — helper for creating docker-exec-based exec functions
// that allow in-process agent tests to use real ZFS/LVM backends inside the
// Kind cluster container.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// realContainerExecFn returns an exec function that runs commands inside the
// given Kind container via "docker exec". This allows the ZFS and LVM backends
// (created with zfsb.NewWithExecFn / lvmb.NewWithExecFn) to operate against
// real storage infrastructure inside the Kind cluster.
//
// DOCKER_HOST is always read from the environment variable, never hardcoded.
//
// If container is empty, returned function immediately errors.
func realContainerExecFn(container string) func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if container == "" {
			return nil, fmt.Errorf("real_backend_exec: container name is empty")
		}
		dockerArgs := append([]string{"exec", container, name}, args...)
		cmd := exec.CommandContext(ctx, "docker", dockerArgs...) //nolint:gosec
		// Forward all environment variables including DOCKER_HOST.
		cmd.Env = os.Environ()
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			// Return combined output (stderr preferred, falling back to stdout) so
			// that callers such as isNotExistOutput can inspect the command output
			// text — mirroring the behaviour of os/exec CombinedOutput used in
			// production. Without this, "dataset does not exist" in stderr would
			// be invisible to the ZFS backend's isNotExistOutput check.
			combined := append(stderr.Bytes(), stdout.Bytes()...) //nolint:gocritic
			errText := strings.TrimSpace(stderr.String())
			if errText == "" {
				errText = strings.TrimSpace(stdout.String())
			}
			if errText == "" {
				errText = err.Error()
			}
			return combined, fmt.Errorf("docker exec %s %s: %s", container, strings.Join(append([]string{name}, args...), " "), errText)
		}
		return stdout.Bytes(), nil
	}
}

// requireSuiteBackendEnv reads the suite backend environment variables set by
// bootstrapSuiteBackends and returns (container, zfsPool, lvmVG).
//
// If any env var is missing, it immediately panics with a clear message
// explaining which env var is missing and how to fix it.
// Per the constraint: NEVER soft-skip — always FAIL with actionable message.
func requireSuiteBackendEnv() (container, zfsPool, lvmVG string) {
	container = os.Getenv(suiteBackendContainerEnvVar)
	if container == "" {
		panic(fmt.Sprintf(
			"[MISSING PREREQUISITE] %s env var is not set.\n"+
				"The agent tests require real ZFS/LVM backends provisioned inside a Kind cluster.\n"+
				"Run the full E2E suite: go test ./test/e2e/ -v\n"+
				"This starts a Kind cluster and provisions real ZFS/LVM backends before running tests.",
			suiteBackendContainerEnvVar,
		))
	}
	zfsPool = os.Getenv(suiteZFSPoolEnvVar)
	if zfsPool == "" {
		panic(fmt.Sprintf(
			"[MISSING PREREQUISITE] %s env var is not set.\n"+
				"The ZFS pool was not provisioned. Check that the zfs kernel module is loaded:\n"+
				"  modprobe zfs\n"+
				"Then re-run: go test ./test/e2e/ -v",
			suiteZFSPoolEnvVar,
		))
	}
	lvmVG = os.Getenv(suiteLVMVGEnvVar)
	if lvmVG == "" {
		panic(fmt.Sprintf(
			"[MISSING PREREQUISITE] %s env var is not set.\n"+
				"The LVM Volume Group was not provisioned. Check that dm_thin_pool kernel module is loaded:\n"+
				"  modprobe dm_thin_pool\n"+
				"Then re-run: go test ./test/e2e/ -v",
			suiteLVMVGEnvVar,
		))
	}
	return container, zfsPool, lvmVG
}
