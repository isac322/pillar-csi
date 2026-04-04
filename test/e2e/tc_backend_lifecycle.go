package e2e

// tc_backend_lifecycle.go — per-TC backend provisioning for ZFS, LVM, and
// iSCSI resources.
//
// Each Provision* function:
//  1. Derives a globally unique resource name from scope.ScopeTag.
//  2. Calls the corresponding framework package to create the resource inside
//     the Kind container via docker exec.
//  3. Registers teardown with the TC scope via TrackBackendRecord so that the
//     resource is cleaned up and verified absent when the scope closes.
//
// No backend state is shared across parallel test cases because every name is
// derived from the TC-private ScopeTag which embeds a monotonically increasing
// sequence number.

import (
	"context"
	"fmt"
	"strings"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/iscsi"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// BackendHandle is a generic handle for a provisioned backend resource.
// It contains the provisioned resource's name and metadata for observability.
type BackendHandle struct {
	// Kind is "zfs-pool", "lvm-vg", or "iscsi-target".
	Kind string

	// Name is the unique resource name (pool name, VG name, or IQN).
	Name string

	// NodeContainer is the Kind container hosting the resource.
	NodeContainer string

	// cleanup destroys the backend resource. Called by TrackBackendRecord
	// during scope teardown.
	cleanup func(context.Context) error
}

// ProvisionZFSPool provisions an ephemeral ZFS pool inside the given Kind
// container node and registers its teardown with the TC scope.
//
// The pool name is derived from scope.ScopeTag to ensure global uniqueness
// across parallel test runs. ZFS pool names have a 256-character limit; the
// derived name uses the SSOT-mandated prefix "e2e-tank-" (ZFS.md §4) followed
// by the last 8 characters of the scope tag (always well under the limit).
//
// Teardown is registered via TrackBackendRecord so that the pool is destroyed
// and verified absent when the scope closes, regardless of spec outcome.
func ProvisionZFSPool(ctx context.Context, scope *TestCaseScope, nodeContainer string) (*BackendHandle, error) { //nolint:dupl
	if scope == nil {
		return nil, fmt.Errorf("ProvisionZFSPool: scope is required")
	}
	if strings.TrimSpace(nodeContainer) == "" {
		return nil, fmt.Errorf("ProvisionZFSPool: nodeContainer is required")
	}

	poolName := zfsPoolName(scope.ScopeTag)

	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
		NodeContainer: nodeContainer,
		PoolName:      poolName,
		SizeMiB:       512,
	})
	if err != nil {
		return nil, fmt.Errorf("ProvisionZFSPool: create pool %q in %s: %w",
			poolName, nodeContainer, err)
	}

	handle := &BackendHandle{
		Kind:          "zfs-pool",
		Name:          poolName,
		NodeContainer: nodeContainer,
		cleanup:       func(cleanupCtx context.Context) error { return pool.Destroy(cleanupCtx) },
	}

	if err := scope.TrackBackendRecord("zfs-pool:"+poolName, PathResourceSpec{
		Path: poolName, // logical identifier, not a filesystem path
		Cleanup: func() error {
			return handle.cleanup(context.Background())
		},
		IsPresent: func() (bool, error) {
			return zfs.PoolExists(context.Background(), nodeContainer, poolName)
		},
	}); err != nil {
		// Provisioning succeeded but registration failed; destroy to avoid leak.
		_ = pool.Destroy(context.Background())
		return nil, fmt.Errorf("ProvisionZFSPool: register teardown for %q: %w", poolName, err)
	}

	return handle, nil
}

// ProvisionLVMVG provisions an ephemeral LVM Volume Group inside the given
// Kind container node and registers its teardown with the TC scope.
//
// The VG name is derived from scope.ScopeTag to ensure global uniqueness
// across parallel test runs. LVM VG names have a 127-character limit; the
// derived name uses the SSOT-mandated prefix "e2e-vg-" (LVM.md §4) followed
// by the last 8 characters of the scope tag (always well under the limit).
func ProvisionLVMVG(ctx context.Context, scope *TestCaseScope, nodeContainer string) (*BackendHandle, error) { //nolint:dupl
	if scope == nil {
		return nil, fmt.Errorf("ProvisionLVMVG: scope is required")
	}
	if strings.TrimSpace(nodeContainer) == "" {
		return nil, fmt.Errorf("ProvisionLVMVG: nodeContainer is required")
	}

	vgName := lvmVGName(scope.ScopeTag)

	vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
		NodeContainer: nodeContainer,
		VGName:        vgName,
		SizeMiB:       512,
	})
	if err != nil {
		return nil, fmt.Errorf("ProvisionLVMVG: create VG %q in %s: %w",
			vgName, nodeContainer, err)
	}

	handle := &BackendHandle{
		Kind:          "lvm-vg",
		Name:          vgName,
		NodeContainer: nodeContainer,
		cleanup:       func(cleanupCtx context.Context) error { return vg.Destroy(cleanupCtx) },
	}

	if err := scope.TrackBackendRecord("lvm-vg:"+vgName, PathResourceSpec{
		Path: vgName, // logical identifier, not a filesystem path
		Cleanup: func() error {
			return handle.cleanup(context.Background())
		},
		IsPresent: func() (bool, error) {
			return lvm.VGExists(context.Background(), nodeContainer, vgName)
		},
	}); err != nil {
		_ = vg.Destroy(context.Background())
		return nil, fmt.Errorf("ProvisionLVMVG: register teardown for %q: %w", vgName, err)
	}

	return handle, nil
}

// ProvisionISCSITarget provisions an ephemeral iSCSI target inside the given
// Kind container node and registers its teardown with the TC scope.
//
// The IQN is derived from scope.ScopeTag to ensure global uniqueness across
// parallel test runs. The IQN uses the SSOT-mandated prefix per ISCSI.md §2,4:
// "iqn.2026-01.com.bhyoo.pillar-csi:" followed by the last 12 characters of
// the scope tag.
func ProvisionISCSITarget(ctx context.Context, scope *TestCaseScope, nodeContainer string) (*BackendHandle, error) {
	if scope == nil {
		return nil, fmt.Errorf("ProvisionISCSITarget: scope is required")
	}
	if strings.TrimSpace(nodeContainer) == "" {
		return nil, fmt.Errorf("ProvisionISCSITarget: nodeContainer is required")
	}

	iqn := iscsiIQN(scope.ScopeTag)

	target, err := iscsi.CreateTarget(ctx, iscsi.CreateTargetOptions{
		NodeContainer: nodeContainer,
		IQN:           iqn,
		SizeMiB:       512,
	})
	if err != nil {
		return nil, fmt.Errorf("ProvisionISCSITarget: create target %q in %s: %w",
			iqn, nodeContainer, err)
	}

	handle := &BackendHandle{
		Kind:          "iscsi-target",
		Name:          iqn,
		NodeContainer: nodeContainer,
		cleanup:       func(cleanupCtx context.Context) error { return target.Destroy(cleanupCtx) },
	}

	tid := target.TID
	if err := scope.TrackBackendRecord("iscsi-target:"+iqn, PathResourceSpec{
		Path: iqn, // logical identifier, not a filesystem path
		Cleanup: func() error {
			return handle.cleanup(context.Background())
		},
		IsPresent: func() (bool, error) {
			return iscsi.TargetExists(context.Background(), nodeContainer, tid)
		},
	}); err != nil {
		_ = target.Destroy(context.Background())
		return nil, fmt.Errorf("ProvisionISCSITarget: register teardown for %q: %w", iqn, err)
	}

	return handle, nil
}

// ─── name derivation helpers ─────────────────────────────────────────────────

// zfsPoolName derives a ZFS pool name from a TC scope tag.
//
// Format: "e2e-tank-<uniqueSuffix>" per SSOT ZFS.md §4 mandate. The suffix is
// the last 8 characters of the scope tag (always unique per TC due to the hash
// embedded by NewTestCaseScope). ZFS pool names have a 256-character limit;
// this name is always ≤ 17 chars. The "e2e-" prefix ensures the pool is caught
// by the suite scavenger cleanup (grep '^e2e-').
func zfsPoolName(scopeTag string) string {
	return "e2e-tank-" + scopeTagSuffix(scopeTag, 8)
}

// lvmVGName derives an LVM Volume Group name from a TC scope tag.
//
// Format: "e2e-vg-<uniqueSuffix>" per SSOT LVM.md §4 mandate. The suffix is
// the last 8 characters of the scope tag. LVM VG names have a 127-character
// limit; this name is always ≤ 15 chars. The "e2e-vg-" prefix ensures the VG
// is caught by the suite scavenger cleanup (grep 'e2e-vg-').
func lvmVGName(scopeTag string) string {
	return "e2e-vg-" + scopeTagSuffix(scopeTag, 8)
}

// iscsiIQN derives an iSCSI IQN from a TC scope tag.
//
// Format: "iqn.2026-01.com.bhyoo.pillar-csi:<uniqueSuffix>" per SSOT ISCSI.md
// §2,4 mandate. The uniqueSuffix is the last 12 characters of the scope tag.
func iscsiIQN(scopeTag string) string {
	return "iqn.2026-01.com.bhyoo.pillar-csi:" + scopeTagSuffix(scopeTag, 12)
}

// scopeTagSuffix returns the last n characters of the scope tag.
//
// The scope tag produced by NewTestCaseScope embeds a process-unique
// monotonic sequence number and a random temp-directory suffix (e.g.
// "553b2a52") at the tail, so the last few characters are globally unique
// across concurrent TCs. Using the tail rather than the head avoids the
// common prefix that all TCs with the same TC ID share.
//
// The result is stripped of leading/trailing hyphens to satisfy OS naming
// constraints for ZFS pools, LVM VGs, and iSCSI IQN suffixes.
func scopeTagSuffix(scopeTag string, n int) string {
	token := dnsLabelToken(scopeTag)
	if len(token) <= n {
		return strings.Trim(token, "-")
	}
	suffix := token[len(token)-n:]
	return strings.Trim(suffix, "-")
}
