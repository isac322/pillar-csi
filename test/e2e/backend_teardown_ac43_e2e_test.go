//go:build e2e

package e2e

// backend_teardown_ac43_e2e_test.go — Sub-AC 4.3: teardown cleanup for ZFS
// pools and LVM VGs inside Kind containers, verified by asserting absence of
// resources after cleanup.
//
// Acceptance criteria verified here:
//
//  1. ZFS pool created inside a Kind container is completely absent after
//     teardown: Pool.Destroy destroys the zpool, detaches the loop device,
//     and removes the image file — and PoolExists returns false.
//  2. LVM VG created inside a Kind container is completely absent after
//     teardown: VG.Destroy removes the VG (vgremove), PV (pvremove), loop
//     device (losetup -d), and image file — and VGExists returns false.
//  3. Per-TC teardown (ProvisionZFSPool + scope.Close) verifies absence via
//     the IsPresent probe registered in TrackBackendRecord: scope.Close returns
//     nil only when PoolExists returns false after cleanup.
//  4. Per-TC LVM teardown (ProvisionLVMVG + scope.Close) behaves the same.
//  5. Suite-level teardown (suiteBackendState.teardown) verifies absence via
//     PoolExists / VGExists after successful Destroy, erroring if still present.
//
// Prerequisites:
//   - Kind cluster bootstrapped (PILLAR_E2E_BACKEND_CONTAINER env var set)
//   - ZFS tests: PILLAR_E2E_ZFS_POOL set OR zfs kernel module loaded and
//     zpool binary present in the Kind container
//   - LVM tests: PILLAR_E2E_LVM_VG set OR dm_thin_pool kernel module loaded
//     and pvcreate/vgcreate binaries present in the Kind container
//   - DOCKER_HOST from environment only (never hardcoded)
//
// Run with:
//
//	go test -tags=e2e ./test/e2e/ -run "TestAC43"
//	go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="ac:4.3"

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/lvm"
	"github.com/bhyoo/pillar-csi/test/e2e/framework/zfs"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// ac43BackendContainer returns the Kind container name from the environment.
// Returns "" when the variable is not set (tests will be skipped).
func ac43BackendContainer() string {
	return os.Getenv(suiteBackendContainerEnvVar)
}

// ac43RequireContainer fails the current test when the Kind container env var
// is not set, meaning the E2E backend environment was not provisioned.
func ac43RequireContainer(t *testing.T) {
	t.Helper()
	if ac43BackendContainer() == "" {
		t.Fatalf("[AC4.3] %s not set — Kind cluster not available; run via 'make test-e2e' to provision the backend",
			suiteBackendContainerEnvVar)
	}
}

// ─── ZFS teardown E2E — plain testing.T ──────────────────────────────────────

// TestAC43ZFSPoolDestroyAndVerifyAbsent creates an ephemeral ZFS pool inside
// the Kind container, destroys it via Pool.Destroy, and asserts that
// PoolExists returns false.
//
// This test directly exercises the framework-level ZFS teardown path
// (zfs.CreatePool → Pool.Destroy → PoolExists) without going through Ginkgo.
func TestAC43ZFSPoolDestroyAndVerifyAbsent(t *testing.T) {
	t.Parallel()
	ac43RequireContainer(t)
	nodeContainer := ac43BackendContainer()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// ── Create the ZFS pool ─────────────────────────────────────────────────
	poolName := fmt.Sprintf("ac43-zfs-%d", time.Now().UnixNano()%100000)
	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
		NodeContainer: nodeContainer,
		PoolName:      poolName,
		SizeMiB:       128, // small pool sufficient for teardown verification
	})
	if err != nil {
		if isContainerToolNotFoundError(err) {
			t.Fatalf("[AC4.3] ZFS userspace tools not available in container %s — "+
				"install zfsutils-linux in the Kind node image", nodeContainer)
		}
		t.Fatalf("[AC4.3] CreatePool %q: %v", poolName, err)
	}

	// ── Verify pool exists before teardown ──────────────────────────────────
	exists, err := zfs.PoolExists(ctx, nodeContainer, poolName)
	if err != nil {
		t.Fatalf("[AC4.3] PoolExists before teardown: %v", err)
	}
	if !exists {
		t.Fatalf("[AC4.3] PoolExists before teardown = false, want true — pool was not created")
	}
	t.Logf("[AC4.3] ZFS pool %q exists in container %s before teardown", poolName, nodeContainer)

	// ── Destroy the pool ─────────────────────────────────────────────────────
	if err := pool.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] Pool.Destroy %q: %v", poolName, err)
	}
	t.Logf("[AC4.3] Pool.Destroy %q completed", poolName)

	// ── Assert absence after teardown (Sub-AC 4.3) ──────────────────────────
	exists, err = zfs.PoolExists(ctx, nodeContainer, poolName)
	if err != nil {
		t.Fatalf("[AC4.3] PoolExists after teardown: %v", err)
	}
	if exists {
		t.Errorf("[AC4.3] ZFS pool %q still present in container %s after Pool.Destroy — "+
			"teardown did not fully clean up the resource",
			poolName, nodeContainer)
	} else {
		t.Logf("[AC4.3] ZFS pool %q confirmed absent in container %s after teardown ✓",
			poolName, nodeContainer)
	}
}

// TestAC43ZFSPoolDestroyIdempotentAndAbsent verifies that calling Pool.Destroy
// twice on the same pool succeeds (idempotent) and that PoolExists returns
// false after both calls.
func TestAC43ZFSPoolDestroyIdempotentAndAbsent(t *testing.T) {
	t.Parallel()
	ac43RequireContainer(t)
	nodeContainer := ac43BackendContainer()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	poolName := fmt.Sprintf("ac43-zfs-idem-%d", time.Now().UnixNano()%100000)
	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
		NodeContainer: nodeContainer,
		PoolName:      poolName,
		SizeMiB:       128,
	})
	if err != nil {
		if isContainerToolNotFoundError(err) {
			t.Fatalf("[AC4.3] ZFS tools not available in container %s — install zfsutils-linux in the Kind node image", nodeContainer)
		}
		t.Fatalf("[AC4.3] CreatePool %q: %v", poolName, err)
	}

	// First destroy.
	if err := pool.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] Pool.Destroy first call: %v", err)
	}

	// Second destroy — must be a safe no-op.
	if err := pool.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] Pool.Destroy second call (idempotency): %v", err)
	}

	// Absence must hold after both calls.
	exists, err := zfs.PoolExists(ctx, nodeContainer, poolName)
	if err != nil {
		t.Fatalf("[AC4.3] PoolExists after double Destroy: %v", err)
	}
	if exists {
		t.Errorf("[AC4.3] ZFS pool %q still present after double Destroy — teardown not idempotent",
			poolName)
	} else {
		t.Logf("[AC4.3] ZFS pool %q absent after double Destroy ✓", poolName)
	}
}

// ─── LVM teardown E2E — plain testing.T ──────────────────────────────────────

// TestAC43LVMVGDestroyAndVerifyAbsent creates an ephemeral LVM Volume Group
// inside the Kind container, destroys it via VG.Destroy, and asserts that
// VGExists returns false.
//
// This test directly exercises the framework-level LVM teardown path
// (lvm.CreateVG → VG.Destroy → VGExists) without going through Ginkgo.
func TestAC43LVMVGDestroyAndVerifyAbsent(t *testing.T) {
	t.Parallel()
	ac43RequireContainer(t)
	nodeContainer := ac43BackendContainer()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// ── Create the LVM VG ───────────────────────────────────────────────────
	vgName := fmt.Sprintf("ac43vg%d", time.Now().UnixNano()%100000)
	vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
		NodeContainer: nodeContainer,
		VGName:        vgName,
		SizeMiB:       128,
	})
	if err != nil {
		if isContainerToolNotFoundError(err) {
			t.Fatalf("[AC4.3] LVM userspace tools not available in container %s — "+
				"install lvm2 in the Kind node image", nodeContainer)
		}
		t.Fatalf("[AC4.3] CreateVG %q: %v", vgName, err)
	}

	// ── Verify VG exists before teardown ────────────────────────────────────
	exists, err := lvm.VGExists(ctx, nodeContainer, vgName)
	if err != nil {
		t.Fatalf("[AC4.3] VGExists before teardown: %v", err)
	}
	if !exists {
		t.Fatalf("[AC4.3] VGExists before teardown = false, want true — VG was not created")
	}
	t.Logf("[AC4.3] LVM VG %q exists in container %s before teardown", vgName, nodeContainer)

	// ── Destroy the VG ──────────────────────────────────────────────────────
	if err := vg.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] VG.Destroy %q: %v", vgName, err)
	}
	t.Logf("[AC4.3] VG.Destroy %q completed", vgName)

	// ── Assert absence after teardown (Sub-AC 4.3) ──────────────────────────
	exists, err = lvm.VGExists(ctx, nodeContainer, vgName)
	if err != nil {
		t.Fatalf("[AC4.3] VGExists after teardown: %v", err)
	}
	if exists {
		t.Errorf("[AC4.3] LVM VG %q still present in container %s after VG.Destroy — "+
			"teardown did not fully clean up the resource",
			vgName, nodeContainer)
	} else {
		t.Logf("[AC4.3] LVM VG %q confirmed absent in container %s after teardown ✓",
			vgName, nodeContainer)
	}
}

// TestAC43LVMVGDestroyIdempotentAndAbsent verifies that calling VG.Destroy
// twice on the same VG succeeds (idempotent) and that VGExists returns false
// after both calls.
func TestAC43LVMVGDestroyIdempotentAndAbsent(t *testing.T) {
	t.Parallel()
	ac43RequireContainer(t)
	nodeContainer := ac43BackendContainer()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	vgName := fmt.Sprintf("ac43vgidem%d", time.Now().UnixNano()%100000)
	vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
		NodeContainer: nodeContainer,
		VGName:        vgName,
		SizeMiB:       128,
	})
	if err != nil {
		if isContainerToolNotFoundError(err) {
			t.Fatalf("[AC4.3] LVM tools not available in container %s — install lvm2 in the Kind node image", nodeContainer)
		}
		t.Fatalf("[AC4.3] CreateVG %q: %v", vgName, err)
	}

	// First destroy.
	if err := vg.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] VG.Destroy first call: %v", err)
	}

	// Second destroy — must be a safe no-op.
	if err := vg.Destroy(ctx); err != nil {
		t.Fatalf("[AC4.3] VG.Destroy second call (idempotency): %v", err)
	}

	// Absence must hold after both calls.
	exists, err := lvm.VGExists(ctx, nodeContainer, vgName)
	if err != nil {
		t.Fatalf("[AC4.3] VGExists after double Destroy: %v", err)
	}
	if exists {
		t.Errorf("[AC4.3] LVM VG %q still present after double Destroy — teardown not idempotent",
			vgName)
	} else {
		t.Logf("[AC4.3] LVM VG %q absent after double Destroy ✓", vgName)
	}
}

// ─── Per-TC lifecycle teardown E2E — Ginkgo ──────────────────────────────────

// These Ginkgo specs exercise the per-TC provisioning and teardown path through
// ProvisionZFSPool / ProvisionLVMVG + TestCaseScope.Close().
//
// ProvisionZFSPool/ProvisionLVMVG register TrackBackendRecord entries with
// IsPresent probes (PoolExists/VGExists). When scope.Close() runs, it calls
// teardownTrackedResources which invokes both the Cleanup func (Destroy) and
// the IsPresent probe. If the resource is still present, scope.Close() returns
// a non-nil error — this is the per-TC absence assertion.

var _ = Describe("Sub-AC 4.3: backend teardown absence verification", Label("ac:4.3", "teardown", "default-profile"), func() {

	// ── ZFS per-TC teardown ───────────────────────────────────────────────────

	Describe("ZFS pool per-TC teardown", func() {
		It("provisions a ZFS pool, closes the scope, and asserts pool is absent", func() {
			nodeContainer := ac43BackendContainer()
			if nodeContainer == "" {
				Fail("[AC4.3] " + suiteBackendContainerEnvVar + " not set — Kind cluster not available; run via 'make test-e2e'")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			// Create a fresh TC scope.
			scope, err := NewTestCaseScope("ac43-zfs-tc-teardown")
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] NewTestCaseScope")

			// Provision a real ZFS pool inside the Kind container and register
			// teardown via TrackBackendRecord + PoolExists probe.
			handle, provErr := ProvisionZFSPool(ctx, scope, nodeContainer)
			if provErr != nil {
				if isContainerToolNotFoundError(provErr) {
					_ = scope.Close()
					Fail("[AC4.3] ZFS userspace tools not in container " + nodeContainer +
						" — install zfsutils-linux in the Kind node image")
				}
				Expect(provErr).NotTo(HaveOccurred(), "[AC4.3] ProvisionZFSPool")
			}

			poolName := handle.Name

			// Confirm existence before teardown.
			exists, err := zfs.PoolExists(ctx, nodeContainer, poolName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] PoolExists before teardown")
			Expect(exists).To(BeTrue(),
				"[AC4.3] pool %q should exist in container %s before scope.Close", poolName, nodeContainer)

			// Close the scope: this triggers teardown (Destroy + IsPresent probe).
			// scope.Close returns a non-nil error only if the resource is still
			// present after cleanup — the per-TC absence assertion (Sub-AC 4.3).
			Expect(scope.Close()).To(Succeed(),
				"[AC4.3] scope.Close should succeed: pool absent after teardown")

			// Independent post-teardown absence check outside the scope.
			exists, err = zfs.PoolExists(ctx, nodeContainer, poolName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] PoolExists after scope.Close")
			Expect(exists).To(BeFalse(),
				"[AC4.3] ZFS pool %q must be absent after scope.Close in container %s",
				poolName, nodeContainer)
		})
	})

	// ── LVM per-TC teardown ───────────────────────────────────────────────────

	Describe("LVM VG per-TC teardown", func() {
		It("provisions an LVM VG, closes the scope, and asserts VG is absent", func() {
			nodeContainer := ac43BackendContainer()
			if nodeContainer == "" {
				Fail("[AC4.3] " + suiteBackendContainerEnvVar + " not set — Kind cluster not available; run via 'make test-e2e'")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			// Create a fresh TC scope.
			scope, err := NewTestCaseScope("ac43-lvm-tc-teardown")
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] NewTestCaseScope")

			// Provision a real LVM VG inside the Kind container.
			handle, provErr := ProvisionLVMVG(ctx, scope, nodeContainer)
			if provErr != nil {
				if isContainerToolNotFoundError(provErr) {
					_ = scope.Close()
					Fail("[AC4.3] LVM userspace tools not in container " + nodeContainer +
						" — install lvm2 in the Kind node image")
				}
				Expect(provErr).NotTo(HaveOccurred(), "[AC4.3] ProvisionLVMVG")
			}

			vgName := handle.Name

			// Confirm existence before teardown.
			exists, err := lvm.VGExists(ctx, nodeContainer, vgName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] VGExists before teardown")
			Expect(exists).To(BeTrue(),
				"[AC4.3] VG %q should exist in container %s before scope.Close", vgName, nodeContainer)

			// Close the scope to trigger teardown with absence verification.
			Expect(scope.Close()).To(Succeed(),
				"[AC4.3] scope.Close should succeed: VG absent after teardown")

			// Independent post-teardown absence check.
			exists, err = lvm.VGExists(ctx, nodeContainer, vgName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] VGExists after scope.Close")
			Expect(exists).To(BeFalse(),
				"[AC4.3] LVM VG %q must be absent after scope.Close in container %s",
				vgName, nodeContainer)
		})
	})

	// ── Suite-level teardown (suiteBackendState) ──────────────────────────────

	Describe("Suite-level backend teardown", func() {
		It("suiteBackendState.teardown destroys ZFS pool and confirms absence via PoolExists", func() {
			nodeContainer := ac43BackendContainer()
			if nodeContainer == "" {
				Fail("[AC4.3] " + suiteBackendContainerEnvVar + " not set — Kind cluster not available; run via 'make test-e2e'")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			poolName := fmt.Sprintf("ac43-suite-zfs-%d", time.Now().UnixNano()%10000)
			pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
				NodeContainer: nodeContainer,
				PoolName:      poolName,
				SizeMiB:       128,
			})
			if err != nil {
				if isContainerToolNotFoundError(err) {
					Fail("[AC4.3] ZFS tools not in container " + nodeContainer + " — install zfsutils-linux in the Kind node image")
				}
				Expect(err).NotTo(HaveOccurred(), "[AC4.3] zfs.CreatePool")
			}

			// Build a suiteBackendState carrying the live pool.
			state := &suiteBackendState{
				NodeContainer: nodeContainer,
				ZFSPool:       pool,
				LVMVG:         nil,
			}

			// Run suite-level teardown — it calls Destroy then PoolExists (AC4.3).
			Expect(state.teardown(ctx, GinkgoWriter)).To(Succeed(),
				"[AC4.3] suiteBackendState.teardown should succeed and confirm pool absent")

			// Independent verification that the pool is gone.
			exists, err := zfs.PoolExists(ctx, nodeContainer, poolName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] PoolExists after suiteBackendState.teardown")
			Expect(exists).To(BeFalse(),
				"[AC4.3] ZFS pool %q must be absent after suiteBackendState.teardown", poolName)
		})

		It("suiteBackendState.teardown destroys LVM VG and confirms absence via VGExists", func() {
			nodeContainer := ac43BackendContainer()
			if nodeContainer == "" {
				Fail("[AC4.3] " + suiteBackendContainerEnvVar + " not set — Kind cluster not available; run via 'make test-e2e'")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			vgName := fmt.Sprintf("ac43svg%d", time.Now().UnixNano()%10000)
			vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
				NodeContainer: nodeContainer,
				VGName:        vgName,
				SizeMiB:       128,
			})
			if err != nil {
				if isContainerToolNotFoundError(err) {
					Fail("[AC4.3] LVM tools not in container " + nodeContainer + " — install lvm2 in the Kind node image")
				}
				Expect(err).NotTo(HaveOccurred(), "[AC4.3] lvm.CreateVG")
			}

			state := &suiteBackendState{
				NodeContainer: nodeContainer,
				ZFSPool:       nil,
				LVMVG:         vg,
			}

			// Run suite-level teardown — it calls Destroy then VGExists (AC4.3).
			Expect(state.teardown(ctx, GinkgoWriter)).To(Succeed(),
				"[AC4.3] suiteBackendState.teardown should succeed and confirm VG absent")

			// Independent verification.
			exists, err := lvm.VGExists(ctx, nodeContainer, vgName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] VGExists after suiteBackendState.teardown")
			Expect(exists).To(BeFalse(),
				"[AC4.3] LVM VG %q must be absent after suiteBackendState.teardown", vgName)
		})
	})

	// ── Both backends in one teardown ─────────────────────────────────────────

	Describe("Suite-level teardown with both ZFS and LVM", func() {
		It("destroys both ZFS pool and LVM VG and confirms both absent", func() {
			nodeContainer := ac43BackendContainer()
			if nodeContainer == "" {
				Fail("[AC4.3] " + suiteBackendContainerEnvVar + " not set — Kind cluster not available; run via 'make test-e2e'")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			poolName := fmt.Sprintf("ac43-both-zfs-%d", time.Now().UnixNano()%10000)
			vgName := fmt.Sprintf("ac43bothvg%d", time.Now().UnixNano()%10000)

			pool, zfsErr := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
				NodeContainer: nodeContainer,
				PoolName:      poolName,
				SizeMiB:       128,
			})
			vg, lvmErr := lvm.CreateVG(ctx, lvm.CreateVGOptions{
				NodeContainer: nodeContainer,
				VGName:        vgName,
				SizeMiB:       128,
			})

			// Fail when tools are unavailable — these are required backend tools.
			zfsToolMissing := zfsErr != nil && isContainerToolNotFoundError(zfsErr)
			lvmToolMissing := lvmErr != nil && isContainerToolNotFoundError(lvmErr)
			if zfsToolMissing || lvmToolMissing {
				if pool != nil {
					_ = pool.Destroy(ctx)
				}
				if vg != nil {
					_ = vg.Destroy(ctx)
				}
				reasons := []string{}
				if zfsToolMissing {
					reasons = append(reasons, "ZFS tools missing — install zfsutils-linux")
				}
				if lvmToolMissing {
					reasons = append(reasons, "LVM tools missing — install lvm2")
				}
				Fail("[AC4.3] required tools unavailable in container " + nodeContainer + ": " + strings.Join(reasons, ", "))
			}

			Expect(zfsErr).NotTo(HaveOccurred(), "[AC4.3] zfs.CreatePool")
			Expect(lvmErr).NotTo(HaveOccurred(), "[AC4.3] lvm.CreateVG")

			state := &suiteBackendState{
				NodeContainer: nodeContainer,
				ZFSPool:       pool,
				LVMVG:         vg,
			}

			Expect(state.teardown(ctx, GinkgoWriter)).To(Succeed(),
				"[AC4.3] suiteBackendState.teardown (both) should succeed")

			// ZFS pool must be absent.
			zfsExists, err := zfs.PoolExists(ctx, nodeContainer, poolName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] PoolExists after combined teardown")
			Expect(zfsExists).To(BeFalse(),
				"[AC4.3] ZFS pool %q must be absent after combined teardown", poolName)

			// LVM VG must be absent.
			lvmExists, err := lvm.VGExists(ctx, nodeContainer, vgName)
			Expect(err).NotTo(HaveOccurred(), "[AC4.3] VGExists after combined teardown")
			Expect(lvmExists).To(BeFalse(),
				"[AC4.3] LVM VG %q must be absent after combined teardown", vgName)
		})
	})
})
