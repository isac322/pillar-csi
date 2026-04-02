package provisioner

import (
	"context"
	"fmt"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/registry"
)

// LVMProvisioner is a [BackendProvisioner] implementation that creates an
// ephemeral LVM Volume Group inside a Kind container node.
//
// It implements the soft-skip semantics: when the "dm_thin_pool" kernel module
// is not loaded on the host or the LVM userspace tools (pvcreate, vgcreate) are
// absent from the container, Provision returns (nil, nil) instead of an error.
//
// All storage operations run inside the container via "docker exec". The
// DOCKER_HOST environment variable is forwarded from the caller's environment.
type LVMProvisioner struct {
	// NodeContainer is the Docker container name of the Kind node in which the
	// LVM Volume Group should be created
	// (e.g. "pillar-csi-e2e-p12345-abcd1234-control-plane").
	// Must not be empty.
	NodeContainer string

	// VGName is the LVM Volume Group name to pass to "vgcreate". Must be unique
	// within the container for the lifetime of the test. Must not be empty.
	VGName string

	// SizeMiB is the size of the loop-device image in mebibytes.
	// Values ≤ 0 default to 512 MiB.
	SizeMiB int

	// ModuleCheckFn is an optional function that reports whether the
	// "dm_thin_pool" kernel module is loaded. When nil, the production
	// implementation (reading /proc/modules) is used. Set this in tests to
	// inject a fake.
	ModuleCheckFn func(name string) bool
}

// BackendType returns the string "lvm", identifying this provisioner.
func (l *LVMProvisioner) BackendType() string {
	return "lvm"
}

// Provision creates an ephemeral LVM Volume Group inside the Kind container node.
//
// Soft-skip conditions (return nil, nil):
//   - The "dm_thin_pool" kernel module is not loaded on the host.
//   - The LVM userspace tools (pvcreate, vgcreate) are absent from the container.
//
// Hard error conditions (return nil, err):
//   - NodeContainer or VGName is empty.
//   - lvm.CreateVG fails for reasons other than a missing binary.
//   - The created VG is not in an active/writable state.
func (l *LVMProvisioner) Provision(ctx context.Context) (registry.Resource, error) {
	if strings.TrimSpace(l.NodeContainer) == "" {
		return nil, fmt.Errorf("lvm provisioner: NodeContainer must not be empty")
	}
	if strings.TrimSpace(l.VGName) == "" {
		return nil, fmt.Errorf("lvm provisioner: VGName must not be empty")
	}

	// ── Soft-skip: kernel module check ──────────────────────────────────────
	checkFn := l.ModuleCheckFn
	if checkFn == nil {
		checkFn = isKernelModuleLoaded
	}
	if !checkFn("dm_thin_pool") {
		return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent module
	}

	// ── Provision ────────────────────────────────────────────────────────────
	vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
		NodeContainer: l.NodeContainer,
		VGName:        l.VGName,
		SizeMiB:       l.SizeMiB,
	})
	if err != nil {
		if isContainerToolNotFoundError(err) || isDockerExecSystemError(err) {
			return nil, nil //nolint:nilnil // soft skip: BackendProvisioner contract (nil,nil) = absent or unreachable tool
		}
		return nil, fmt.Errorf("lvm provisioner: create VG %q in %s: %w",
			l.VGName, l.NodeContainer, err)
	}

	// ── Verify active state ──────────────────────────────────────────────────
	if verifyErr := lvm.VerifyActive(ctx, l.NodeContainer, l.VGName); verifyErr != nil {
		_ = vg.Destroy(ctx)
		return nil, fmt.Errorf("lvm provisioner: VG %q not active after creation in %s: %w",
			l.VGName, l.NodeContainer, verifyErr)
	}

	return vg, nil
}
