// Package provisioner provides E2E test helpers for provisioning backend resources.
//
//nolint:dupl // zfs_provisioner and lvm_provisioner share intentionally parallel structure
package provisioner

import (
	"context"
	"fmt"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ZFSProvisioner is a [BackendProvisioner] implementation that creates an
// ephemeral ZFS pool inside a Kind container node.
//
// All storage operations run inside the container via "docker exec". The
// DOCKER_HOST environment variable is forwarded from the caller's environment.
//
// The "zfs" kernel module and "zpool" binary must be present before calling
// Provision. Call [kind.CheckBackendKernelModules] upfront to enforce this —
// missing modules cause a hard FAIL, not a soft skip.
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
}

// BackendType returns the string "zfs", identifying this provisioner.
func (z *ZFSProvisioner) BackendType() string {
	return "zfs"
}

// Provision creates an ephemeral ZFS pool inside the Kind container node.
//
// Hard error conditions (return nil, err):
//   - NodeContainer or PoolName is empty.
//   - zfs.CreatePool fails (the "zfs" kernel module and "zpool" binary must be
//     present — call [kind.CheckBackendKernelModules] before Provision).
//   - The created pool is not in the ONLINE state.
//
// Soft-skip (nil, nil) is not supported: all failures are hard errors.
func (z *ZFSProvisioner) Provision(ctx context.Context) (registry.Resource, error) {
	if strings.TrimSpace(z.NodeContainer) == "" {
		return nil, fmt.Errorf("zfs provisioner: NodeContainer must not be empty")
	}
	if strings.TrimSpace(z.PoolName) == "" {
		return nil, fmt.Errorf("zfs provisioner: PoolName must not be empty")
	}

	// ── Provision ────────────────────────────────────────────────────────────
	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
		NodeContainer: z.NodeContainer,
		PoolName:      z.PoolName,
		SizeMiB:       z.SizeMiB,
	})
	if err != nil {
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
