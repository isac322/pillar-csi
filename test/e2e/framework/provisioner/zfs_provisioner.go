package provisioner

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ZFSProvisioner is a [BackendProvisioner] implementation that creates an
// ephemeral ZFS pool inside a Kind container node.
//
// It implements the soft-skip semantics: when the "zfs" kernel module is not
// loaded on the host or the "zpool" binary is absent from the container,
// Provision returns (nil, nil) instead of an error.
//
// All storage operations run inside the container via "docker exec". The
// DOCKER_HOST environment variable is forwarded from the caller's environment.
type ZFSProvisioner struct {
	// NodeContainer is the Docker container name of the Kind node in which the
	// ZFS pool should be created (e.g. "pillar-csi-e2e-p12345-abcd1234-control-plane").
	// Must not be empty.
	NodeContainer string

	// PoolName is the ZFS pool name to pass to "zpool create". Must be unique
	// within the container for the lifetime of the test. Must not be empty.
	PoolName string

	// SizeMiB is the size of the loop-device image in mebibytes.
	// Values ≤ 0 default to 512 MiB.
	SizeMiB int

	// ModuleCheckFn is an optional function that reports whether the "zfs"
	// kernel module is loaded. When nil, the production implementation
	// (reading /proc/modules) is used. Set this in tests to inject a fake.
	ModuleCheckFn func(name string) bool
}

// BackendType returns the string "zfs", identifying this provisioner.
func (z *ZFSProvisioner) BackendType() string {
	return "zfs"
}

// Provision creates an ephemeral ZFS pool inside the Kind container node.
//
// Soft-skip conditions (return nil, nil):
//   - The "zfs" kernel module is not loaded on the host.
//   - The "zpool" binary is absent from the container.
//
// Hard error conditions (return nil, err):
//   - NodeContainer or PoolName is empty.
//   - zfs.CreatePool fails for reasons other than a missing binary.
//   - The created pool is not in the ONLINE state.
func (z *ZFSProvisioner) Provision(ctx context.Context) (registry.Resource, error) {
	if strings.TrimSpace(z.NodeContainer) == "" {
		return nil, fmt.Errorf("zfs provisioner: NodeContainer must not be empty")
	}
	if strings.TrimSpace(z.PoolName) == "" {
		return nil, fmt.Errorf("zfs provisioner: PoolName must not be empty")
	}

	// ── Soft-skip: kernel module check ──────────────────────────────────────
	checkFn := z.ModuleCheckFn
	if checkFn == nil {
		checkFn = isKernelModuleLoaded
	}
	if !checkFn("zfs") {
		return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent module
	}

	// ── Provision ────────────────────────────────────────────────────────────
	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
		NodeContainer: z.NodeContainer,
		PoolName:      z.PoolName,
		SizeMiB:       z.SizeMiB,
	})
	if err != nil {
		if isContainerToolNotFoundError(err) {
			return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent tool
		}
		return nil, fmt.Errorf("zfs provisioner: create pool %q in %s: %w",
			z.PoolName, z.NodeContainer, err)
	}

	// ── Verify ONLINE state ──────────────────────────────────────────────────
	if verifyErr := zfs.VerifyOnline(ctx, z.NodeContainer, z.PoolName); verifyErr != nil {
		_ = pool.Destroy(ctx)
		return nil, fmt.Errorf("zfs provisioner: pool %q not ONLINE after creation in %s: %w",
			z.PoolName, z.NodeContainer, verifyErr)
	}

	return pool, nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// isKernelModuleLoaded reports whether the given kernel module is currently
// loaded by parsing /proc/modules. Hyphens are normalised to underscores to
// match the kernel's internal representation.
//
// Returns false when /proc/modules cannot be read (e.g. non-Linux OS).
func isKernelModuleLoaded(name string) bool {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return false
	}
	target := strings.ReplaceAll(name, "-", "_")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			loaded := strings.ReplaceAll(fields[0], "-", "_")
			if loaded == target {
				return true
			}
		}
	}
	return false
}

// isContainerToolNotFoundError reports whether err indicates that a required
// binary was absent from the container's PATH.
func isContainerToolNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "executable file not found in $path") ||
		strings.Contains(msg, "executable file not found") ||
		(strings.Contains(msg, "exec:") && strings.Contains(msg, "not found")) ||
		strings.Contains(msg, "command not found") ||
		(strings.Contains(msg, "no such file or directory") &&
			(strings.Contains(msg, "zpool") || strings.Contains(msg, "vgcreate") ||
				strings.Contains(msg, "pvcreate") || strings.Contains(msg, "tgtadm")))
}
