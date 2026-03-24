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
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume — happy-path tests
// ─────────────────────────────────────────────────────────────────────────────

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
	mounted, _ := env.mounter.IsMounted(targetPath)
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
	hasBind := false
	for _, o := range mc.options {
		if o == "bind" {
			hasBind = true
			break
		}
	}
	if !hasBind {
		t.Errorf("Mount options %v do not include \"bind\"", mc.options)
	}
}

// TestNodePublishVolume_BlockAccess verifies that NodePublishVolume performs a
// bind mount for BLOCK access type using the same staging path as source.
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

	mounted, _ := env.mounter.IsMounted(targetPath)
	if !mounted {
		t.Error("target path not mounted after NodePublishVolume (block)")
	}

	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	if mc.source != stagingPath {
		t.Errorf("Mount source = %q, want %q", mc.source, stagingPath)
	}
}

// TestNodePublishVolume_Readonly verifies that the "ro" option is added when
// the request has Readonly=true.
func TestNodePublishVolume_Readonly(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	_, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "tank/pvc-readonly",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCap("ext4"),
		Readonly:          true,
	})
	if err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	if len(env.mounter.mountCalls) != 1 {
		t.Fatalf("Mount called %d times, want 1", len(env.mounter.mountCalls))
	}
	mc := env.mounter.mountCalls[0]
	hasRO := false
	for _, o := range mc.options {
		if o == "ro" {
			hasRO = true
			break
		}
	}
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
// NodePublishVolume — validation error tests
// ─────────────────────────────────────────────────────────────────────────────

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

// TestNodePublishVolume_IsMountedError verifies that an IsMounted error
// propagates as an Internal gRPC code.
func TestNodePublishVolume_IsMountedError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	env.mounter.isMountedErr = errors.New("isMounted failed")
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
// ─────────────────────────────────────────────────────────────────────────────

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

	mounted, _ := env.mounter.IsMounted(targetPath)
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

	mounted, _ = env.mounter.IsMounted(targetPath)
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
	// Unmount must NOT have been called.
	if len(env.mounter.unmountCalls) != 0 {
		t.Errorf("Unmount called %d times on unmounted path, want 0", len(env.mounter.unmountCalls))
	}
}

// TestNodeUnpublishVolume_TwiceMountsOnce verifies the full publish→unpublish→
// unpublish cycle: the second unpublish is a no-op.
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
	for i := 0; i < 2; i++ {
		if _, err := env.srv.NodeUnpublishVolume(context.Background(), unpubReq); err != nil {
			t.Fatalf("NodeUnpublishVolume call %d: %v", i+1, err)
		}
	}
	// Unmount called exactly once.
	if len(env.mounter.unmountCalls) != 1 {
		t.Errorf("Unmount called %d times, want 1", len(env.mounter.unmountCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnpublishVolume — validation error tests
// ─────────────────────────────────────────────────────────────────────────────

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

// TestNodeUnpublishVolume_UnmountError verifies that a mounter Unmount error
// propagates as Internal.
func TestNodeUnpublishVolume_UnmountError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	targetPath := t.TempDir()
	const volumeID = "tank/pvc-unmount-err"

	// Pre-mark path as mounted so Unmount is attempted.
	env.mounter.mountedPaths[targetPath] = true
	env.mounter.unmountErr = errors.New("device busy")

	_, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	requireGRPCCode(t, err, codes.Internal)
}

// TestNodeUnpublishVolume_IsMountedError verifies that an IsMounted error
// propagates as Internal.
func TestNodeUnpublishVolume_IsMountedError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	env.mounter.isMountedErr = errors.New("isMounted failed")
	targetPath := t.TempDir()

	_, err := env.srv.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol-1",
		TargetPath: targetPath,
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// Full lifecycle: Stage → Publish → Unpublish → Unstage
// ─────────────────────────────────────────────────────────────────────────────

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
		port     = "4420"
	)
	volCtx := mountVolumeContext(nqn, addr, port)
	cap := mountCap("ext4")

	// 1. Stage.
	if _, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  cap,
		VolumeContext:     volCtx,
	}); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// 2. Publish.
	if _, err := env.srv.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  cap,
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	targetMounted, _ := env.mounter.IsMounted(targetPath)
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

	targetMounted, _ = env.mounter.IsMounted(targetPath)
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

	stagingMounted, _ := env.mounter.IsMounted(stagingPath)
	if stagingMounted {
		t.Error("staging path still mounted after NodeUnstageVolume")
	}
	if len(env.connector.disconnectCalls) != 1 {
		t.Errorf("Disconnect called %d times, want 1", len(env.connector.disconnectCalls))
	}
}
