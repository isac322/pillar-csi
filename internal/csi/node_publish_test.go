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

package csi

// Tests for NodePublishVolume and NodeUnpublishVolume.
//
// All tests use injectable mock Connector and Mounter implementations so no
// NVMe-oF kernel modules, real block devices, or root privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodePublish

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — happy-path tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodePublishVolume_MountAccess verifies that NodePublishVolume performs a
// bind mount from the staging path to the target path for MOUNT access type.
func TestNodePublishVolume_MountAccess(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-publish-test"

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	// Target path must be mounted.
	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if !mounted {
		t.Error("target path not mounted after NodePublishVolume")
	}

	// Mount must have been called once with correct source, target, and bind option.
	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	if mc.source != stagingPath {
		t.Errorf("Mount source = %q, want %q", mc.source, stagingPath)
	}
	if mc.target != targetPath {
		t.Errorf("Mount target = %q, want %q", mc.target, targetPath)
	}
	// Options must include "bind".
	hasBind := slices.Contains(mc.options, "bind")
	if !hasBind {
		t.Errorf("Mount options %v do not include \"bind\"", mc.options)
	}
}

// TestNodePublishVolume_BlockAccess verifies that NodePublishVolume performs a
// bind mount for BLOCK access type from the in-staging device sentinel file
// (blockStagingDevicePath) rather than the staging directory itself, because
// the kernel rejects bind of a regular file onto a directory target.
func TestNodePublishVolume_BlockAccess(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-block-publish"

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  blockCap(),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if !mounted {
		t.Error("target path not mounted after NodePublishVolume (block)")
	}

	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	wantSource := blockStagingDevicePath(stagingPath)
	if mc.source != wantSource {
		t.Errorf("Mount source = %q, want %q (in-staging device sentinel)", mc.source, wantSource)
	}
}

// TestNodePublishVolume_Readonly verifies that the "ro" option is added when
// the request has Readonly=true, and that publishing a read-only bind of a
// healthy staged filesystem succeeds — the write probe runs against the
// read-write staged mount, not the read-only bind (issue #168).
func TestNodePublishVolume_Readonly(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	const volumeID = "tank/pvc-readonly"
	const nqn = "nqn.test:readonly"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
		Readonly:          true,
	})
	if err != nil {
		t.Fatalf("read-only NodePublishVolume on a healthy stage: %v", err)
	}
	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	hasRO := slices.Contains(mc.options, "ro")
	if !hasRO {
		t.Errorf("Mount options %v do not include \"ro\" for readonly volume", mc.options)
	}
}

// TestNodePublishVolume_Idempotent verifies that calling NodePublishVolume a
// second time when the target is already mounted returns success without calling
// Mount again.
func TestNodePublishVolume_Idempotent(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	req := &csi.NodePublishVolumeRequest{
		VolumeId:          "tank/pvc-idempotent",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
	}

	// First call — mounts.
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("first NodePublishVolume: %v", err)
	}
	// Second call — must succeed without re-mounting.
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("second NodePublishVolume: %v", err)
	}
	if len(env.mounter.mountCalls) != 1 {
		t.Errorf("Mount called %d times after 2 NodePublishVolume calls, want 1",
			len(env.mounter.mountCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — staged filesystem shutdown (issue #168)
// ─────────────────────────────────────────────────────────────────────────────.

// stageForPublish stages a MOUNT volume and returns the staging request so
// publish tests exercise a real (mock) staged filesystem whose superblock
// identity the publish bind shares.
func stageForPublish(t *testing.T, env *nodeTestEnv, stagingPath, volumeID, nqn string) {
	t.Helper()
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
}

// TestNodePublishVolume_DeadBindRecoversViaTeardown walks the full recovery
// path from issue #168: a bind mount onto a kernel-shutdown filesystem can
// never become healthy while it exists (the bind pins the dead superblock),
// so recovery is unpublish → NodeStageVolume re-stages (unpin + remount) →
// a fresh publish binds the live filesystem.
func TestNodePublishVolume_DeadBindRecoversViaTeardown(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-dead-bind"
	const nqn = "nqn.test:dead-bind"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	// The filesystem shuts down under both the staged mount and the bind.
	env.mounter.markUnhealthy(env.connector.devicePath)

	// While the bind exists the staged filesystem cannot be re-staged:
	// unmount+mount would re-attach the same dead superblock.
	volCtx := mountVolumeContext(nqn, testStorageAddr)
	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     volCtx,
	}
	_, err := env.srv.NodeStageVolume(context.Background(), stageReq)
	requireGRPCCode(t, err, codes.Internal)

	// Pod teardown removes the bind; the stage then repairs itself.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}
	if _, err := env.srv.NodeStageVolume(context.Background(), stageReq); err != nil {
		t.Fatalf("NodeStageVolume after unpublish: %v", err)
	}

	// The replacement pod's publish binds the healed filesystem.
	req.VolumeContext = volCtx
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume after repair: %v", err)
	}
	if err := env.mounter.CheckMountHealth(targetPath); err != nil {
		t.Errorf("re-published bind still unhealthy: %v", err)
	}
}

// TestNodePublishVolume_BindOntoStillDeadStage verifies the issue #172
// repair path: publishing against a staged filesystem that entered kernel
// shutdown drops the dead staged mount, re-mounts the still-attached
// device so the journal replays, and then binds the healed filesystem —
// kubelet's pod-delete flow never re-calls NodeStageVolume, so the publish
// must repair in place.
func TestNodePublishVolume_BindOntoStillDeadStage(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-dead-stage"
	const nqn = "nqn.test:dead-stage"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	// The staged filesystem enters kernel shutdown before this pod's first
	// publish (e.g. the pool backing the zvol ran out of space).
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume did not repair the dead staged filesystem: %v", err)
	}

	// The dead staged mount was dropped once and the device re-mounted.
	if len(env.mounter.unmountCalls) != 1 || env.mounter.unmountCalls[0] != stagingPath {
		t.Errorf("Unmount calls = %v, want exactly [%s]", env.mounter.unmountCalls, stagingPath)
	}
	if got := len(env.mounter.formatAndMountCalls); got != 2 {
		t.Errorf("FormatAndMount calls = %d, want 2 (stage + repair re-mount)", got)
	} else if got := env.mounter.formatAndMountCalls[1]; got.source != env.connector.devicePath ||
		got.target != stagingPath {
		t.Errorf("repair re-mount = (%q -> %q), want (%q -> %q)",
			got.source, got.target, env.connector.devicePath, stagingPath)
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("re-mounted staged filesystem still unhealthy: %v", err)
	}
	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("target path not bind-mounted after the repaired publish")
	}
}

// TestNodePublishVolume_IdempotentDeadBindNotSuccess covers the pod-delete
// sequence from issue #172: after the filesystem shuts down, an idempotent
// publish drops its own dead bind, then repairs the dead staged mount in
// place (unmount + re-mount, which replays the journal) and re-binds the
// healed filesystem — the replacement pod reaches Running without a fresh
// NodeStageVolume.
func TestNodePublishVolume_IdempotentDeadBindNotSuccess(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-idem-dead"
	const nqn = "nqn.test:idem-dead"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	// The filesystem shuts down; both the staged mount and the bind keep
	// their mount table entries, which is exactly what made the incident wedge.
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("NodePublishVolume did not repair the dead staged filesystem: %v", err)
	}

	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("bind not re-mounted after the repaired publish")
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("re-mounted staged filesystem still unhealthy: %v", err)
	}
}

// TestNodePublishVolume_DeadBindProbeError covers an inconclusive health
// probe of an existing bind mount: nothing is torn down and the error is
// reported to the CO.
func TestNodePublishVolume_DeadBindProbeError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-probe-bind"
	const nqn = "nqn.test:probe-bind"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	env.mounter.checkHealthErr = errors.New("procfs unreadable")

	_, err := env.srv.NodePublishVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)

	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("bind was dropped on an inconclusive health probe")
	}
}

// TestNodePublishVolume_ReadonlyBindProbesStagedFilesystem covers the
// read-only publish of a volume whose staged filesystem is dead: the
// read-only bind cannot be write-probed (EROFS is its expected answer), so
// the verdict and the repair come from the staged mount that shares the
// superblock — the dead staged mount is dropped and re-mounted, then the
// read-only bind is restored.
func TestNodePublishVolume_ReadonlyBindProbesStagedFilesystem(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-ro-dead"
	const nqn = "nqn.test:ro-dead"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	req := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
		Readonly:          true,
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("read-only NodePublishVolume on a healthy stage: %v", err)
	}

	// The health of the read-only bind is read from the staged mount, not
	// the bind itself: probing the bind would answer EROFS for a healthy
	// filesystem too.
	if !slices.Contains(env.mounter.checkHealthCalls, stagingPath) {
		t.Errorf("CheckMountHealth calls = %v, want the staged mount %q probed "+
			"(the read-only bind cannot be write-probed)", env.mounter.checkHealthCalls, stagingPath)
	}

	// The filesystem shuts down under the healthy-looking bind.
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodePublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("read-only publish did not repair the dead staged filesystem: %v", err)
	}

	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("read-only bind not restored after the repaired publish")
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("re-mounted staged filesystem still unhealthy: %v", err)
	}
}

// TestNodePublishVolume_ReadonlyStageDeadBindFails covers the read-only side
// of issue #175 at publish time: with the filesystem staged "ro" no write
// probe runs, so an already-published bind of a kernel-shutdown filesystem
// (entry kept, stat EIO) must fail the idempotent retry through the
// non-writing stat probe instead of reporting success, and the bind stays.
func TestNodePublishVolume_ReadonlyStageDeadBindFails(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	volCtx := mountVolumeContext("nqn.test:publish-ro-dead", testStorageAddr)
	if _, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId: "tank/pvc-publish-ro-dead", StagingTargetPath: stagingPath,
		VolumeCapability: mountCapRO("xfs"), VolumeContext: volCtx,
	}); err != nil {
		t.Fatalf("read-only NodeStageVolume: %v", err)
	}
	req := &csi.NodePublishVolumeRequest{
		VolumeId: "tank/pvc-publish-ro-dead", StagingTargetPath: stagingPath, TargetPath: targetPath,
		VolumeCapability: mountCapRO("xfs"), VolumeContext: volCtx,
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), req); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodePublishVolume(context.Background(), req)
	requireGRPCCode(t, err, codes.Internal)
	if ok, _ := env.mounter.MountEntryExists(targetPath); !ok { //nolint:errcheck // mock never errors here
		t.Error("bind was dropped although nothing repairs a read-only stage")
	}
	if len(env.mounter.checkHealthCalls) != 0 {
		t.Errorf("write probe ran on a read-only stage: %v", env.mounter.checkHealthCalls)
	}
}

// TestNodePublishVolume_RepairDespiteStatEIO exercises the live pod-delete
// failure of issues #172 and #175 end to end: the staged filesystem enters
// kernel shutdown — its mount table entry stays while stat(2) answers EIO
// (on the live kernel 6.12 node and in the fake alike) — the old pod's bind
// is unpublished, and kubelet retries NodePublishVolume for the replacement
// pod without ever issuing NodeStageVolume.  Publish must find the dead
// mount through the mount table, drop it, re-mount the still-attached
// device (journal replay), and bind the healed filesystem.  Release v0.5.2
// decided "mounted" with a stat-based check and failed every retry with EIO.
func TestNodePublishVolume_RepairDespiteStatEIO(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	oldTarget := t.TempDir()
	newTarget := t.TempDir()
	const volumeID = "tank/pvc-stat-eio"
	const nqn = "nqn.test:stat-eio"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	volCtx := mountVolumeContext(nqn, testStorageAddr)
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagingPath, TargetPath: oldTarget,
		VolumeCapability: mountCap("xfs"), VolumeContext: volCtx,
	}); err != nil {
		t.Fatalf("old pod NodePublishVolume: %v", err)
	}
	env.mounter.markUnhealthy(env.connector.devicePath)

	// Precondition: the fake reproduces the live kernel signature.
	if err := env.mounter.CheckMountReadable(stagingPath); !errors.Is(err, syscall.EIO) {
		t.Fatalf("stat probe on a dead staged mount = %v, want EIO", err)
	}
	if ok, _ := env.mounter.MountEntryExists(stagingPath); !ok { //nolint:errcheck // mock never errors here
		t.Fatal("dead staged mount must stay in the mount table")
	}

	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: oldTarget,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagingPath, TargetPath: newTarget,
		VolumeCapability: mountCap("xfs"), VolumeContext: volCtx,
	}); err != nil {
		t.Fatalf("replacement pod NodePublishVolume with stat EIO on the staged mount: %v", err)
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("staged filesystem not repaired: %v", err)
	}
	if mounted, err := env.mounter.MountEntryExists(newTarget); err != nil || !mounted {
		t.Errorf("replacement bind: mounted=%v err=%v, want mounted", mounted, err)
	}
	if got := len(env.mounter.formatAndMountCalls); got != 2 {
		t.Errorf("FormatAndMount calls = %d, want 2 (stage + repair re-mount)", got)
	}
}

// TestNodePublishVolume_BindFailureRepairsDeadStage covers kernels that
// reject a bind-mount of a shut-down superblock outright (mount exit 32 on
// the live homelab, kernel 6.12): the failed bind triggers the same
// staged-mount repair and the bind is retried once.
func TestNodePublishVolume_BindFailureRepairsDeadStage(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-bind-reject"
	const nqn = "nqn.test:bind-reject"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	env.mounter.markUnhealthy(env.connector.devicePath)
	env.mounter.mountErrOnce = errors.New(
		"mount: wrong fs type, bad option, bad superblock on " + stagingPath)

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     mountVolumeContext(nqn, testStorageAddr),
	})
	if err != nil {
		t.Fatalf("NodePublishVolume did not recover after the rejected bind: %v", err)
	}

	if len(env.mounter.mountCalls) != 2 {
		t.Errorf("Mount calls = %d, want 2 (rejected bind + retry)", len(env.mounter.mountCalls))
	}
	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never errors
	if !mounted {
		t.Error("target path not bound after repair + retry")
	}
}

// TestNodePublishVolume_DeadStagePinnedByBindFails verifies the pinned
// branch: while another target's bind still references the dead
// superblock the publish must not unmount the staged mount — the RPC
// fails retryably until teardown removes the pinning bind.
func TestNodePublishVolume_DeadStagePinnedByBindFails(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetA := t.TempDir()
	targetB := t.TempDir()
	const volumeID = "tank/pvc-pinned"
	const nqn = "nqn.test:pinned"
	stageForPublish(t, env, stagingPath, volumeID, nqn)

	volCtx := mountVolumeContext(nqn, testStorageAddr)
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetA,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     volCtx,
	}); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	env.mounter.markUnhealthy(env.connector.devicePath)

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetB,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     volCtx,
	})
	requireGRPCCode(t, err, codes.Internal)

	mounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never errors here
	if !mounted {
		t.Error("dead staged mount unmounted while another bind still pins it")
	}
	mounted, _ = env.mounter.MountEntryExists(targetB) //nolint:errcheck // mock never errors here
	if mounted {
		t.Error("target B mounted despite the repair being refused")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — validation error tests
// ─────────────────────────────────────────────────────────────────────────────.

func TestNodePublishVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		StagingTargetPath: "/staging",
		TargetPath:        "/target",
		VolumeCapability:  mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodePublishVolume_MissingStagingTargetPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:         "vol-1",
		TargetPath:       "/target",
		VolumeCapability: mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodePublishVolume_MissingTargetPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: "/staging",
		VolumeCapability:  mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodePublishVolume_MissingVolumeCapability(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: "/staging",
		TargetPath:        "/target",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestNodePublishVolume_MountError verifies that a mounter error propagates as
// an Internal gRPC code.
func TestNodePublishVolume_MountError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	env.mounter.mountErr = errors.New("mount failed")
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.Internal)
}

// TestNodePublishVolume_MountEntryExistsError verifies that a mount-table
// lookup error propagates as an Internal gRPC code.
func TestNodePublishVolume_MountEntryExistsError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	env.mounter.mountEntryExistsErr = errors.New("mountinfo unreadable")
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-1",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnpublishVolume — happy-path tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeUnpublishVolume_Unmounts verifies that NodeUnpublishVolume unmounts
// a previously published volume.
func TestNodeUnpublishVolume_Unmounts(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-unpublish"

	// Publish first.
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	mounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if !mounted {
		t.Fatal("expected target path to be mounted after NodePublishVolume")
	}

	// Unpublish.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	mounted, _ = env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if mounted {
		t.Error("target path still mounted after NodeUnpublishVolume")
	}
	if len(env.mounter.unmountCalls) != 1 {
		t.Errorf("Unmount called %d times, want 1", len(env.mounter.unmountCalls))
	}
	if env.mounter.unmountCalls[0] != targetPath {
		t.Errorf("Unmount path = %q, want %q", env.mounter.unmountCalls[0], targetPath)
	}
}

// TestNodeUnpublishVolume_Idempotent verifies that calling NodeUnpublishVolume
// when the target is not mounted succeeds without error (idempotent).
// The Unmount call itself is contractually idempotent, so the RPC delegates
// the "already unmounted" decision to it rather than pre-probing.
func TestNodeUnpublishVolume_Idempotent(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-unpublish-idempotent"

	// targetPath is NOT mounted — simulate a repeat call after successful unpublish.
	_, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume on unmounted path: %v", err)
	}
	if env.mounter.mountedPaths[targetPath] {
		t.Error("unmounted target marked mounted after idempotent unpublish")
	}
}

// TestNodeUnpublishVolume_TwiceMountsOnce verifies the full publish→unpublish→
// unpublish cycle: the repeat unpublish converges idempotently and leaves the
// volume unpublished.
func TestNodeUnpublishVolume_TwiceMountsOnce(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-double-unpublish"

	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("xfs"),
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	unpubReq := &csi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: targetPath}
	for i := range 2 {
		if _, err := env.srv.NodeUnpublishVolume(context.Background(), unpubReq); err != nil {
			t.Fatalf("NodeUnpublishVolume call %d: %v", i+1, err)
		}
	}
	if env.mounter.mountedPaths[targetPath] {
		t.Error("target path still mounted after repeated NodeUnpublishVolume")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnpublishVolume — validation error tests
// ─────────────────────────────────────────────────────────────────────────────.

func TestNodeUnpublishVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		TargetPath: "/target",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

func TestNodeUnpublishVolume_MissingTargetPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	_, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "vol-1",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestNodeUnpublishVolume_UnmountError verifies that a failed unmount
// propagates as Internal and leaves teardown state untouched: the mount
// stays mounted and the volume state machine is not reverted to
// NodeStaged, so the retried call still takes the unpublish path.
// (Probe failures surface through Unmount itself — NodeUnpublishVolume does
// not gate on a mount check — so a separate probe-error test would pin a
// code path that does not exist.)
func TestNodeUnpublishVolume_UnmountError(t *testing.T) {
	t.Parallel()

	mnt := newMockMounter()
	sm := NewVolumeStateMachine()
	srv := NewNodeServerWithStateMachine("test-node",
		&mockConnector{devicePath: "/dev/nvme0n1"}, mnt, t.TempDir(), sm)
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-unmount-err"

	// Model a published volume: state machine says NodePublished and the
	// target path is still mounted.
	sm.ForceState(volumeID, StateNodePublished)
	mnt.mountedPaths[targetPath] = true
	mnt.unmountErr = errors.New("device busy")

	_, err := srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	requireGRPCCode(t, err, codes.Internal)

	if got := sm.GetState(volumeID); got != StateNodePublished {
		t.Errorf("volume state after failed unpublish = %v, want %v",
			got, StateNodePublished)
	}
	if !mnt.mountedPaths[targetPath] {
		t.Error("target path marked unmounted after failed unmount")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Full lifecycle: Stage → Publish → Unpublish → Unstage
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeFullLifecycle exercises the complete node-side volume lifecycle:
// NodeStageVolume → NodePublishVolume → NodeUnpublishVolume → NodeUnstageVolume.
func TestNodeFullLifecycle(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()
	const (
		volumeID = "tank/pvc-lifecycle"
		nqn      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-lifecycle"
		addr     = "192.0.2.10"
	)
	volCtx := mountVolumeContext(nqn, addr)
	volumeCap := mountCap("ext4")

	// 1. Stage.
	if _, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  volumeCap,
		VolumeContext:     volCtx,
	}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// 2. Publish.
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  volumeCap,
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	targetMounted, _ := env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if !targetMounted {
		t.Error("target path not mounted after NodePublishVolume")
	}

	// 3. Unpublish.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	targetMounted, _ = env.mounter.MountEntryExists(targetPath) //nolint:errcheck // mock never returns an error
	if targetMounted {
		t.Error("target path still mounted after NodeUnpublishVolume")
	}

	// 4. Unstage.
	if _, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	stagingMounted, _ := env.mounter.MountEntryExists(stagingPath) //nolint:errcheck // mock never returns an error
	if stagingMounted {
		t.Error("staging path still mounted after NodeUnstageVolume")
	}
	if len(env.connector.disconnectCalls) != 1 {
		t.Errorf("Disconnect called %d times, want 1", len(env.connector.disconnectCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume/NodeUnpublishVolume — multiple publish targets (issue #168)
// ─────────────────────────────────────────────────────────────────────────────.

// newNodeTestEnvWithSM returns the mock-mounter test env wired to a real
// VolumeStateMachine, for tests that assert the per-volume state across
// several publish targets.
func newNodeTestEnvWithSM(t *testing.T) (*nodeTestEnv, *VolumeStateMachine) {
	t.Helper()
	conn := &mockConnector{devicePath: "/dev/nvme0n1"}
	mnt := newMockMounter()
	sm := NewVolumeStateMachine()
	stateDir := t.TempDir()
	srv := NewNodeServerWithStateMachine("test-node", conn, mnt, stateDir, sm)
	return &nodeTestEnv{srv: srv, connector: conn, mounter: mnt, stateDir: stateDir}, sm
}

// TestNodePublishVolume_MultiTargetRepairAfterBothUnpublish covers the
// multi-target case of issue #168: one staged filesystem serving two
// publish binds enters kernel shutdown.  While either bind survives the
// state machine must stay NodePublished — the aggregate still has live
// publishes — and the stage repair must stay refused.  Only after both
// targets are unpublished may the state demote and the re-stage repair
// proceed.
func TestNodePublishVolume_MultiTargetRepairAfterBothUnpublish(t *testing.T) {
	t.Parallel()

	env, sm := newNodeTestEnvWithSM(t)
	stagingPath := t.TempDir()
	targetA := t.TempDir()
	targetB := t.TempDir()
	const volumeID = "tank/pvc-multi-target"
	const nqn = "nqn.test:multi-target"
	volCtx := mountVolumeContext(nqn, testStorageAddr)

	sm.ForceState(volumeID, StateControllerPublished)
	if _, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     volCtx,
	}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	publishReq := func(target string) *csi.NodePublishVolumeRequest {
		return &csi.NodePublishVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagingPath,
			TargetPath:        target,
			VolumeCapability:  mountCap("xfs"),
			VolumeContext:     volCtx,
		}
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), publishReq(targetA)); err != nil {
		t.Fatalf("NodePublishVolume target A: %v", err)
	}
	if _, err := env.srv.NodePublishVolume(context.Background(), publishReq(targetB)); err != nil {
		t.Fatalf("NodePublishVolume target B: %v", err)
	}

	// The staged filesystem enters kernel shutdown under both binds.
	env.mounter.markUnhealthy(env.connector.devicePath)

	// While both binds pin the dead superblock the re-stage fails honestly
	// and the aggregate state stays NodePublished.
	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("xfs"),
		VolumeContext:     volCtx,
	}
	_, stageErr := env.srv.NodeStageVolume(context.Background(), stageReq)
	requireGRPCCode(t, stageErr, codes.Internal)
	if got := sm.GetState(volumeID); got != StateNodePublished {
		t.Fatalf("state after refused repair = %v, want %v", got, StateNodePublished)
	}

	// Teardown of the first target: the second bind still pins the staged
	// filesystem, so the volume must remain NodePublished and the repair
	// must stay refused.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetA,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume target A: %v", err)
	}
	if got := sm.GetState(volumeID); got != StateNodePublished {
		t.Fatalf("state while target B still bound = %v, want %v", got, StateNodePublished)
	}
	_, stageErr = env.srv.NodeStageVolume(context.Background(), stageReq)
	requireGRPCCode(t, stageErr, codes.Internal)

	// Teardown of the second target removes the last mount sharing the
	// staged filesystem: only now may the state demote and the next stage
	// repair the dead mount.
	if _, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetB,
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume target B: %v", err)
	}
	if env.mounter.mountedPaths[targetB] {
		t.Error("target B still mounted after its NodeUnpublishVolume succeeded")
	}
	if got := sm.GetState(volumeID); got != StateNodeStaged {
		t.Fatalf("state after last unpublish = %v, want %v", got, StateNodeStaged)
	}

	if _, err := env.srv.NodeStageVolume(context.Background(), stageReq); err != nil {
		t.Fatalf("NodeStageVolume repair after teardown: %v", err)
	}
	if err := env.mounter.CheckMountHealth(stagingPath); err != nil {
		t.Errorf("staged filesystem still unhealthy after repair: %v", err)
	}

	// A replacement pod can bind the healed filesystem again.
	if _, err := env.srv.NodePublishVolume(context.Background(), publishReq(t.TempDir())); err != nil {
		t.Fatalf("NodePublishVolume after repair: %v", err)
	}
}
