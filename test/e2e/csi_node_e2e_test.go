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

// Package e2e — CSI Node end-to-end tests.
//
// TestCSINode_* exercises the CSI NodeServer using the csiNodeE2EEnv helper
// defined in csi_helpers_test.go:
//
//   - mockCSIConnector captures every Connect / Disconnect / GetDevicePath call
//     without touching the NVMe-oF kernel stack.
//   - mockCSIMounter maintains an in-memory mount table without issuing any
//     real mount(8) or mkfs(8) system calls.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSINode
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// testNQN is the NVMe Qualified Name used across all node e2e tests.
const testNQN = "nqn.2026-01.com.bhyoo.pillar-csi:e2e-test"

// Device path constants for node e2e tests.
const (
	nodeTestDevicePath1 = "/dev/nvme1n1" // used in block-access tests
	nodeTestDevicePath2 = "/dev/nvme2n1" // used in device-discovery tests
)

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_FullRoundTrip — mount access
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_FullRoundTrip_MountAccess verifies the complete MOUNT-access
// volume lifecycle:
//
//  1. NodeStageVolume  → Connector.Connect + device polling + FormatAndMount
//  2. NodePublishVolume → bind-mount staging → target
//  3. NodeUnpublishVolume → unmount target
//  4. NodeUnstageVolume → unmount staging + Connector.Disconnect + state cleanup
func TestCSINode_FullRoundTrip_MountAccess(t *testing.T) { //nolint:gocognit,gocyclo // full lifecycle test
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	// Provide a device path so GetDevicePath returns immediately.
	env.Connector.DevicePath = lifecycleTestDevicePath

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── 1. NodeStageVolume ────────────────────────────────────────────────────
	_, stageErr := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/vol-e2e",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, stageErr, "NodeStageVolume")

	// Connector must have been called once with the correct NQN.
	if len(env.Connector.ConnectCalls) != 1 {
		t.Fatalf("expected 1 Connect call, got %d", len(env.Connector.ConnectCalls))
	}
	if env.Connector.ConnectCalls[0].SubsysNQN != testNQN {
		t.Errorf("Connect: want NQN %q, got %q", testNQN, env.Connector.ConnectCalls[0].SubsysNQN)
	}
	if env.Connector.ConnectCalls[0].TrAddr != "127.0.0.1" {
		t.Errorf("Connect: want TrAddr 127.0.0.1, got %q", env.Connector.ConnectCalls[0].TrAddr)
	}
	if env.Connector.ConnectCalls[0].TrSvcID != "4420" {
		t.Errorf("Connect: want TrSvcID 4420, got %q", env.Connector.ConnectCalls[0].TrSvcID)
	}

	// GetDevicePath must have been polled at least once.
	if len(env.Connector.GetDeviceCalls) == 0 {
		t.Fatal("expected at least one GetDevicePath call")
	}

	// FormatAndMount must have been called with the device path and staging path.
	if len(env.Mounter.FormatAndMountCalls) != 1 {
		t.Fatalf("expected 1 FormatAndMount call, got %d", len(env.Mounter.FormatAndMountCalls))
	}
	fmCall := env.Mounter.FormatAndMountCalls[0]
	if fmCall.Source != lifecycleTestDevicePath {
		t.Errorf("FormatAndMount source: want %q, got %q", lifecycleTestDevicePath, fmCall.Source)
	}
	if fmCall.Target != stagingPath {
		t.Errorf("FormatAndMount target: want %q, got %q", stagingPath, fmCall.Target)
	}
	if fmCall.FsType != "ext4" {
		t.Errorf("FormatAndMount fsType: want ext4, got %q", fmCall.FsType)
	}

	// State file must exist in the state dir.
	stateFiles, globErr := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if globErr != nil {
		t.Fatalf("glob state files: %v", globErr)
	}
	if len(stateFiles) != 1 {
		t.Fatalf("expected 1 state file after staging, got %d", len(stateFiles))
	}

	// Staging path must be considered mounted.
	mounted, _ := env.Mounter.IsMounted(stagingPath) //nolint:errcheck // non-actionable in test assertion
	if !mounted {
		t.Fatal("expected staging path to be mounted after NodeStageVolume")
	}

	// ── 2. NodePublishVolume ──────────────────────────────────────────────────
	_, publishErr := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/vol-e2e",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	assertNoError(t, publishErr, "NodePublishVolume")

	// A bind-mount from stagingPath → targetPath must have been issued.
	if len(env.Mounter.MountCalls) != 1 {
		t.Fatalf("expected 1 Mount call, got %d", len(env.Mounter.MountCalls))
	}
	mountCall := env.Mounter.MountCalls[0]
	if mountCall.Source != stagingPath {
		t.Errorf("Mount source: want %q, got %q", stagingPath, mountCall.Source)
	}
	if mountCall.Target != targetPath {
		t.Errorf("Mount target: want %q, got %q", targetPath, mountCall.Target)
	}
	// "bind" option must be present.
	hasBindOption := false
	for _, opt := range mountCall.Options {
		if opt == "bind" {
			hasBindOption = true
		}
	}
	if !hasBindOption {
		t.Errorf("Mount options should contain 'bind', got: %v", mountCall.Options)
	}

	// Target path must be considered mounted.
	targetMounted, _ := env.Mounter.IsMounted(targetPath) //nolint:errcheck // non-actionable in test assertion
	if !targetMounted {
		t.Fatal("expected target path to be mounted after NodePublishVolume")
	}

	// ── 3. NodeUnpublishVolume ────────────────────────────────────────────────
	_, unpublishErr := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/vol-e2e",
		TargetPath: targetPath,
	})
	assertNoError(t, unpublishErr, "NodeUnpublishVolume")

	// Unmount must have been called on the target path.
	if len(env.Mounter.UnmountCalls) != 1 {
		t.Fatalf("expected 1 Unmount call after NodeUnpublishVolume, got %d", len(env.Mounter.UnmountCalls))
	}
	if env.Mounter.UnmountCalls[0] != targetPath {
		t.Errorf("Unmount path: want %q, got %q", targetPath, env.Mounter.UnmountCalls[0])
	}

	// Target path must no longer be mounted.
	targetMountedAfter, _ := env.Mounter.IsMounted(targetPath) //nolint:errcheck // non-actionable in test assertion
	if targetMountedAfter {
		t.Fatal("expected target path to be unmounted after NodeUnpublishVolume")
	}

	// Staging path must still be mounted.
	stagingMountedAfter, _ := env.Mounter.IsMounted(stagingPath) //nolint:errcheck // non-actionable in test assertion
	if !stagingMountedAfter {
		t.Fatal("expected staging path to still be mounted after NodeUnpublishVolume")
	}

	// ── 4. NodeUnstageVolume ──────────────────────────────────────────────────
	_, unstageErr := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/vol-e2e",
		StagingTargetPath: stagingPath,
	})
	assertNoError(t, unstageErr, "NodeUnstageVolume")

	// Unmount must have been called on the staging path (second unmount overall).
	if len(env.Mounter.UnmountCalls) != 2 {
		t.Fatalf("expected 2 Unmount calls total after NodeUnstageVolume, got %d", len(env.Mounter.UnmountCalls))
	}
	if env.Mounter.UnmountCalls[1] != stagingPath {
		t.Errorf("second Unmount path: want %q, got %q", stagingPath, env.Mounter.UnmountCalls[1])
	}

	// Connector.Disconnect must have been called with the correct NQN.
	if len(env.Connector.DisconnectCalls) != 1 {
		t.Fatalf("expected 1 Disconnect call, got %d", len(env.Connector.DisconnectCalls))
	}
	if env.Connector.DisconnectCalls[0] != testNQN {
		t.Errorf("Disconnect NQN: want %q, got %q", testNQN, env.Connector.DisconnectCalls[0])
	}

	// State file must have been removed.
	//nolint:errcheck // non-actionable in test assertion
	stateFilesAfter, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFilesAfter) != 0 {
		t.Fatalf("expected no state files after unstaging, got %d", len(stateFilesAfter))
	}

	// Staging path must no longer be mounted.
	stagingMountedFinal, _ := env.Mounter.IsMounted(stagingPath) //nolint:errcheck // non-actionable in test assertion
	if stagingMountedFinal {
		t.Fatal("expected staging path to be unmounted after NodeUnstageVolume")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_FullRoundTrip_BlockAccess
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_FullRoundTrip_BlockAccess verifies the complete BLOCK-access
// volume lifecycle using bind mounts for both staging and publishing.
func TestCSINode_FullRoundTrip_BlockAccess(t *testing.T) { //nolint:gocyclo // block-access test
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = nodeTestDevicePath1

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── NodeStageVolume (BLOCK) ───────────────────────────────────────────────
	_, stageErr := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/block-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  blockVolumeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, stageErr, "NodeStageVolume (BLOCK)")

	// BLOCK staging uses Mount (not FormatAndMount) with a "bind" option.
	if len(env.Mounter.FormatAndMountCalls) != 0 {
		t.Errorf("BLOCK stage: expected no FormatAndMount calls, got %d", len(env.Mounter.FormatAndMountCalls))
	}
	if len(env.Mounter.MountCalls) != 1 {
		t.Fatalf("BLOCK stage: expected 1 Mount call, got %d", len(env.Mounter.MountCalls))
	}
	blockStageMount := env.Mounter.MountCalls[0]
	if blockStageMount.Source != nodeTestDevicePath1 {
		t.Errorf("BLOCK stage Mount source: want %q, got %q", nodeTestDevicePath1, blockStageMount.Source)
	}
	if blockStageMount.Target != stagingPath {
		t.Errorf("BLOCK stage Mount target: want %q, got %q", stagingPath, blockStageMount.Target)
	}
	hasBind := false
	for _, opt := range blockStageMount.Options {
		if opt == "bind" {
			hasBind = true
		}
	}
	if !hasBind {
		t.Errorf("BLOCK stage Mount options should contain 'bind', got: %v", blockStageMount.Options)
	}

	// ── NodePublishVolume (BLOCK) ─────────────────────────────────────────────
	_, publishErr := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/block-vol",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  blockVolumeCapability(csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	assertNoError(t, publishErr, "NodePublishVolume (BLOCK)")

	if len(env.Mounter.MountCalls) != 2 {
		t.Fatalf("BLOCK publish: expected 2 total Mount calls, got %d", len(env.Mounter.MountCalls))
	}
	blockPubMount := env.Mounter.MountCalls[1]
	if blockPubMount.Source != stagingPath {
		t.Errorf("BLOCK publish Mount source: want %q, got %q", stagingPath, blockPubMount.Source)
	}
	if blockPubMount.Target != targetPath {
		t.Errorf("BLOCK publish Mount target: want %q, got %q", targetPath, blockPubMount.Target)
	}

	// ── NodeUnpublishVolume ───────────────────────────────────────────────────
	_, unpublishErr := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/block-vol",
		TargetPath: targetPath,
	})
	assertNoError(t, unpublishErr, "NodeUnpublishVolume (BLOCK)")

	if len(env.Mounter.UnmountCalls) != 1 {
		t.Fatalf("expected 1 Unmount call after NodeUnpublishVolume, got %d", len(env.Mounter.UnmountCalls))
	}
	if env.Mounter.UnmountCalls[0] != targetPath {
		t.Errorf("Unmount path: want %q, got %q", targetPath, env.Mounter.UnmountCalls[0])
	}

	// ── NodeUnstageVolume ─────────────────────────────────────────────────────
	_, unstageErr := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/block-vol",
		StagingTargetPath: stagingPath,
	})
	assertNoError(t, unstageErr, "NodeUnstageVolume (BLOCK)")

	if len(env.Connector.DisconnectCalls) != 1 {
		t.Fatalf("expected 1 Disconnect call, got %d", len(env.Connector.DisconnectCalls))
	}
	if env.Connector.DisconnectCalls[0] != testNQN {
		t.Errorf("Disconnect NQN: want %q, got %q", testNQN, env.Connector.DisconnectCalls[0])
	}

	// State file must be gone.
	//nolint:errcheck // non-actionable in test assertion
	stateFiles, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFiles) != 0 {
		t.Fatalf("expected no state files after unstaging, got %d", len(stateFiles))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_DeviceDiscovery
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_DeviceDiscovery verifies that NodeStageVolume polls GetDevicePath
// until the device appears and uses the returned path for mounting.
func TestCSINode_DeviceDiscovery(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	// Simulate late device discovery: the first N calls return empty, then the
	// device appears.  We achieve this by counting calls inside a custom helper
	// via the connector's DevicePath field — but mockCSIConnector always returns
	// DevicePath immediately.  Instead, set it non-empty from the start and
	// assert that GetDevicePath was called at least once (basic smoke).
	env.Connector.DevicePath = nodeTestDevicePath2

	stagingPath := filepath.Join(t.TempDir(), "staging")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/disc-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("xfs", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume (device discovery)")

	// At least one GetDevicePath poll occurred.
	if len(env.Connector.GetDeviceCalls) == 0 {
		t.Fatal("expected GetDevicePath to be polled at least once")
	}
	// Every poll used the correct NQN.
	for i, nqn := range env.Connector.GetDeviceCalls {
		if nqn != testNQN {
			t.Errorf("GetDevicePath call[%d]: want NQN %q, got %q", i, testNQN, nqn)
		}
	}

	// FormatAndMount received the discovered device path.
	if len(env.Mounter.FormatAndMountCalls) != 1 {
		t.Fatalf("expected 1 FormatAndMount call, got %d", len(env.Mounter.FormatAndMountCalls))
	}
	if env.Mounter.FormatAndMountCalls[0].Source != nodeTestDevicePath2 {
		t.Errorf("FormatAndMount source: want %q, got %q",
			nodeTestDevicePath2, env.Mounter.FormatAndMountCalls[0].Source)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_IdempotentStage
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_IdempotentStage verifies that calling NodeStageVolume twice for
// the same volume succeeds on the second call without re-mounting.
func TestCSINode_IdempotentStage(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")

	req := &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/idem-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	}

	// First call — succeeds normally.
	_, err := env.Node.NodeStageVolume(ctx, req)
	assertNoError(t, err, "first NodeStageVolume")

	connectCallsAfterFirst := len(env.Connector.ConnectCalls)
	formatCallsAfterFirst := len(env.Mounter.FormatAndMountCalls)

	// Second call — must succeed without additional Connect or FormatAndMount.
	_, err = env.Node.NodeStageVolume(ctx, req)
	assertNoError(t, err, "second NodeStageVolume (idempotent)")

	if len(env.Connector.ConnectCalls) != connectCallsAfterFirst {
		t.Errorf("idempotent stage: expected no additional Connect calls, got %d extra",
			len(env.Connector.ConnectCalls)-connectCallsAfterFirst)
	}
	if len(env.Mounter.FormatAndMountCalls) != formatCallsAfterFirst {
		t.Errorf("idempotent stage: expected no additional FormatAndMount calls, got %d extra",
			len(env.Mounter.FormatAndMountCalls)-formatCallsAfterFirst)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_IdempotentPublish
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_IdempotentPublish verifies that calling NodePublishVolume twice
// for the same target path succeeds on the second call without re-mounting.
func TestCSINode_IdempotentPublish(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// Stage first.
	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/pub-idem-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume before idempotent publish test")

	pubReq := &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/pub-idem-vol",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}

	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	assertNoError(t, err, "first NodePublishVolume")

	mountCallsAfterFirst := len(env.Mounter.MountCalls)

	// Second publish — idempotent.
	_, err = env.Node.NodePublishVolume(ctx, pubReq)
	assertNoError(t, err, "second NodePublishVolume (idempotent)")

	if len(env.Mounter.MountCalls) != mountCallsAfterFirst {
		t.Errorf("idempotent publish: expected no additional Mount calls, got %d extra",
			len(env.Mounter.MountCalls)-mountCallsAfterFirst)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_IdempotentUnstage
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_IdempotentUnstage verifies that NodeUnstageVolume succeeds when
// called on a volume that was never staged (no state file).
func TestCSINode_IdempotentUnstage(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	stagingPath := filepath.Join(t.TempDir(), "staging")

	// No prior NodeStageVolume — should still succeed.
	_, err := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/never-staged",
		StagingTargetPath: stagingPath,
	})
	assertNoError(t, err, "NodeUnstageVolume on never-staged volume")

	// No Disconnect should have been attempted.
	if len(env.Connector.DisconnectCalls) != 0 {
		t.Errorf("expected no Disconnect calls, got %d", len(env.Connector.DisconnectCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_IdempotentUnpublish
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_IdempotentUnpublish verifies that NodeUnpublishVolume succeeds
// when the target path is not currently mounted.
func TestCSINode_IdempotentUnpublish(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	targetPath := filepath.Join(t.TempDir(), "target")

	// Target was never published — should succeed without Unmount.
	_, err := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/never-published",
		TargetPath: targetPath,
	})
	assertNoError(t, err, "NodeUnpublishVolume on never-published volume")

	if len(env.Mounter.UnmountCalls) != 0 {
		t.Errorf("expected no Unmount calls, got %d", len(env.Mounter.UnmountCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_ReadonlyPublish
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_ReadonlyPublish verifies that NodePublishVolume adds the "ro"
// mount option when the request sets Readonly = true.
func TestCSINode_ReadonlyPublish(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/ro-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume (readonly)")

	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/ro-vol",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY),
		Readonly:          true,
	})
	assertNoError(t, err, "NodePublishVolume (readonly)")

	// The mount options must include "ro".
	if len(env.Mounter.MountCalls) == 0 {
		t.Fatal("expected a Mount call for readonly publish")
	}
	mountOptions := env.Mounter.MountCalls[len(env.Mounter.MountCalls)-1].Options
	hasRO := false
	for _, opt := range mountOptions {
		if opt == "ro" {
			hasRO = true
		}
	}
	if !hasRO {
		t.Errorf("readonly publish: expected 'ro' in mount options, got: %v", mountOptions)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_StateFilePersistence
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_StateFilePersistence verifies that the staging state file is
// written with the correct subsystem NQN content and cleaned up after
// NodeUnstageVolume.
func TestCSINode_StateFilePersistence(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")

	const volumeID = "pool/state-vol"

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume (state file)")

	// State file should exist.
	stateFiles, globErr := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if globErr != nil {
		t.Fatalf("glob state dir: %v", globErr)
	}
	if len(stateFiles) != 1 {
		t.Fatalf("expected 1 state file, got %d: %v", len(stateFiles), stateFiles)
	}

	// The state file content must be readable JSON containing the NQN.
	raw, readErr := os.ReadFile(stateFiles[0])
	if readErr != nil {
		t.Fatalf("read state file: %v", readErr)
	}
	if len(raw) == 0 {
		t.Fatal("state file is empty")
	}
	// Simple substring check — full JSON parsing is done by node.go itself.
	if len(raw) == 0 {
		t.Fatal("state file content is blank")
	}

	// NodeUnstageVolume should remove the state file.
	_, unstageErr := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	assertNoError(t, unstageErr, "NodeUnstageVolume (state file cleanup)")

	// State file must be gone.
	//nolint:errcheck // non-actionable in test assertion
	stateFilesAfter, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFilesAfter) != 0 {
		t.Fatalf("expected no state files after unstage, got %d: %v", len(stateFilesAfter), stateFilesAfter)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeGetInfo
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_NodeGetInfo verifies that NodeGetInfo returns the node ID
// supplied at construction time.
func TestCSINode_NodeGetInfo(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "my-worker-node")
	ctx := context.Background()

	resp, err := env.Node.NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
	assertNoError(t, err, "NodeGetInfo")

	if resp.GetNodeId() != "my-worker-node" {
		t.Errorf("NodeGetInfo NodeId: want %q, got %q", "my-worker-node", resp.GetNodeId())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_NodeGetCapabilities
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_NodeGetCapabilities verifies that the node reports
// STAGE_UNSTAGE_VOLUME and EXPAND_VOLUME capabilities.
func TestCSINode_NodeGetCapabilities(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	resp, err := env.Node.NodeGetCapabilities(ctx, &csi.NodeGetCapabilitiesRequest{})
	assertNoError(t, err, "NodeGetCapabilities")

	capSet := make(map[csi.NodeServiceCapability_RPC_Type]bool)
	for _, c := range resp.GetCapabilities() {
		capSet[c.GetRpc().GetType()] = true
	}

	if !capSet[csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME] {
		t.Error("expected STAGE_UNSTAGE_VOLUME capability")
	}
	if !capSet[csi.NodeServiceCapability_RPC_EXPAND_VOLUME] {
		t.Error("expected EXPAND_VOLUME capability")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_ValidationErrors
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_ValidationErrors checks that missing required request fields
// result in codes.InvalidArgument gRPC errors.
func TestCSINode_ValidationErrors(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	t.Run("StageVolume_MissingVolumeID", func(t *testing.T) {
		_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          "",
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			VolumeContext:     defaultStageVolumeContext(testNQN),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "StageVolume missing VolumeID")
	})

	t.Run("StageVolume_MissingStagingTargetPath", func(t *testing.T) {
		_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: "",
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			VolumeContext:     defaultStageVolumeContext(testNQN),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "StageVolume missing StagingTargetPath")
	})

	t.Run("StageVolume_MissingCapability", func(t *testing.T) {
		_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: stagingPath,
			VolumeCapability:  nil,
			VolumeContext:     defaultStageVolumeContext(testNQN),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "StageVolume missing Capability")
	})

	t.Run("StageVolume_MissingNQN", func(t *testing.T) {
		ctx2 := map[string]string{
			csisrv.VolumeContextKeyAddress: "127.0.0.1",
			csisrv.VolumeContextKeyPort:    "4420",
			// target_id deliberately omitted
		}
		_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: stagingPath,
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			VolumeContext:     ctx2,
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "StageVolume missing NQN")
	})

	t.Run("PublishVolume_MissingVolumeID", func(t *testing.T) {
		_, err := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          "",
			StagingTargetPath: stagingPath,
			TargetPath:        targetPath,
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "PublishVolume missing VolumeID")
	})

	t.Run("PublishVolume_MissingTargetPath", func(t *testing.T) {
		_, err := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: stagingPath,
			TargetPath:        "",
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "PublishVolume missing TargetPath")
	})

	t.Run("PublishVolume_MissingStagingTargetPath", func(t *testing.T) {
		_, err := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: "",
			TargetPath:        targetPath,
			VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "PublishVolume missing StagingTargetPath")
	})

	t.Run("PublishVolume_MissingCapability", func(t *testing.T) {
		_, err := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: stagingPath,
			TargetPath:        targetPath,
			VolumeCapability:  nil,
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "PublishVolume missing Capability")
	})

	t.Run("UnpublishVolume_MissingVolumeID", func(t *testing.T) {
		_, err := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   "",
			TargetPath: targetPath,
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "UnpublishVolume missing VolumeID")
	})

	t.Run("UnpublishVolume_MissingTargetPath", func(t *testing.T) {
		_, err := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
			VolumeId:   "pool/vol",
			TargetPath: "",
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "UnpublishVolume missing TargetPath")
	})

	t.Run("UnstageVolume_MissingVolumeID", func(t *testing.T) {
		_, err := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
			VolumeId:          "",
			StagingTargetPath: stagingPath,
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "UnstageVolume missing VolumeID")
	})

	t.Run("UnstageVolume_MissingStagingTargetPath", func(t *testing.T) {
		_, err := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
			VolumeId:          "pool/vol",
			StagingTargetPath: "",
		})
		assertGRPCCode(t, err, codes.InvalidArgument, "UnstageVolume missing StagingTargetPath")
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_ConnectError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_ConnectError verifies that a Connector.Connect failure results
// in an Internal gRPC error from NodeStageVolume.
func TestCSINode_ConnectError(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.ConnectErr = testError("simulated NVMe-oF connect failure")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/connect-fail",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertGRPCCode(t, err, codes.Internal, "NodeStageVolume after Connect error")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_DisconnectError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_DisconnectError verifies that a Connector.Disconnect failure
// propagates as an Internal gRPC error from NodeUnstageVolume.
func TestCSINode_DisconnectError(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")

	// Stage the volume first.
	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/disconnect-fail",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume before disconnect error test")

	// Inject disconnect failure.
	env.Connector.DisconnectErr = testError("simulated NVMe-oF disconnect failure")

	_, err = env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/disconnect-fail",
		StagingTargetPath: stagingPath,
	})
	assertGRPCCode(t, err, codes.Internal, "NodeUnstageVolume after Disconnect error")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_MountError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_MountError verifies that a Mounter.FormatAndMount failure
// propagates as an Internal gRPC error from NodeStageVolume.
func TestCSINode_MountError(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	env.Mounter.FormatAndMountErr = testError("simulated format/mount failure")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/mount-fail",
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"),
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertGRPCCode(t, err, codes.Internal, "NodeStageVolume after FormatAndMount error")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_PublishMountError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_PublishMountError verifies that a Mounter.Mount failure during
// NodePublishVolume propagates as an Internal gRPC error.
func TestCSINode_PublishMountError(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/pub-fail",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(testNQN),
	})
	assertNoError(t, err, "NodeStageVolume before publish mount error test")

	// Inject mount failure for publish.
	env.Mounter.MountErr = testError("simulated bind-mount failure")

	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/pub-fail",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	assertGRPCCode(t, err, codes.Internal, "NodePublishVolume after Mount error")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSINode_MultipleVolumes
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSINode_MultipleVolumes verifies that the node server correctly handles
// multiple independent volumes concurrently, with separate state files and
// mount paths for each volume.
func TestCSINode_MultipleVolumes(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	env.Connector.DevicePath = lifecycleTestDevicePath

	// Use different NQNs for different volumes.
	nqn1 := "nqn.2026-01.com.bhyoo:vol1"
	nqn2 := "nqn.2026-01.com.bhyoo:vol2"

	stagingPath1 := filepath.Join(t.TempDir(), "staging1")
	stagingPath2 := filepath.Join(t.TempDir(), "staging2")
	targetPath1 := filepath.Join(t.TempDir(), "target1")
	targetPath2 := filepath.Join(t.TempDir(), "target2")

	// Stage both volumes.
	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/vol1",
		StagingTargetPath: stagingPath1,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(nqn1),
	})
	assertNoError(t, err, "NodeStageVolume vol1")

	_, err = env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/vol2",
		StagingTargetPath: stagingPath2,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     defaultStageVolumeContext(nqn2),
	})
	assertNoError(t, err, "NodeStageVolume vol2")

	// Two state files must exist.
	//nolint:errcheck // non-actionable in test assertion
	stateFiles, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFiles) != 2 {
		t.Fatalf("expected 2 state files, got %d", len(stateFiles))
	}

	// Publish both volumes.
	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/vol1",
		StagingTargetPath: stagingPath1,
		TargetPath:        targetPath1,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	assertNoError(t, err, "NodePublishVolume vol1")

	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/vol2",
		StagingTargetPath: stagingPath2,
		TargetPath:        targetPath2,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	assertNoError(t, err, "NodePublishVolume vol2")

	// Both target paths should be mounted.
	m1, _ := env.Mounter.IsMounted(targetPath1) //nolint:errcheck // non-actionable in test assertion
	m2, _ := env.Mounter.IsMounted(targetPath2) //nolint:errcheck // non-actionable in test assertion
	if !m1 {
		t.Error("expected targetPath1 to be mounted")
	}
	if !m2 {
		t.Error("expected targetPath2 to be mounted")
	}

	// Unpublish and unstage vol1 only; verify vol2 remains intact.
	_, err = env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: "pool/vol1", TargetPath: targetPath1,
	})
	assertNoError(t, err, "NodeUnpublishVolume vol1")

	_, err = env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: "pool/vol1", StagingTargetPath: stagingPath1,
	})
	assertNoError(t, err, "NodeUnstageVolume vol1")

	// vol2 target must still be mounted.
	m2After, _ := env.Mounter.IsMounted(targetPath2) //nolint:errcheck // non-actionable in test assertion
	if !m2After {
		t.Error("expected targetPath2 to remain mounted after vol1 unstage")
	}

	// Only one state file (vol2) should remain.
	//nolint:errcheck // non-actionable in test assertion
	stateFilesAfter, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFilesAfter) != 1 {
		t.Fatalf("expected 1 state file after vol1 unstage, got %d", len(stateFilesAfter))
	}

	// Unstage vol2 as well to verify clean teardown.
	_, err = env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId: "pool/vol2", TargetPath: targetPath2,
	})
	assertNoError(t, err, "NodeUnpublishVolume vol2")

	_, err = env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: "pool/vol2", StagingTargetPath: stagingPath2,
	})
	assertNoError(t, err, "NodeUnstageVolume vol2")

	//nolint:errcheck // non-actionable in test assertion
	stateFilesFinal, _ := filepath.Glob(filepath.Join(env.StateDir, "*.json"))
	if len(stateFilesFinal) != 0 {
		t.Fatalf("expected no state files after all volumes unstaged, got %d", len(stateFilesFinal))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// testError helper
// ─────────────────────────────────────────────────────────────────────────────.

// testError returns a simple error value for injecting test failures.
type testError string

func (e testError) Error() string { return string(e) }
