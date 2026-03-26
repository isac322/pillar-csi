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

// Package e2e — cross-component CSI ordering constraint tests.
//
// This file implements Sub-AC 2c: per-operation ordering guards that return
// gRPC FailedPrecondition when a CSI step is invoked out of order.
//
// The tests use newCSILifecycleEnvWithSM, which wires a shared
// VolumeStateMachine between the ControllerServer and the NodeServer.  With
// a shared SM the node's ordering guards can detect that ControllerPublish was
// never called (or that NodeStage was never completed) and reject the
// out-of-order RPC with codes.FailedPrecondition.
//
// # Covered negative cases
//
//	NodeStageVolume   before ControllerPublishVolume → FailedPrecondition
//	NodePublishVolume before NodeStageVolume          → FailedPrecondition
//	NodeUnstageVolume before NodeUnpublishVolume      → FailedPrecondition
//	NodePublishVolume after  NodeUnstageVolume        → FailedPrecondition
//
// # Covered positive (guard-pass) cases
//
//	Full ordered lifecycle with shared SM succeeds end-to-end.
//	Each operation is idempotent when repeated in the correct state.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIOrdering
package e2e

import (
	"context"
	"path/filepath"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_NodeStageBeforeControllerPublish
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_NodeStageBeforeControllerPublish verifies that calling
// NodeStageVolume without first calling ControllerPublishVolume returns
// gRPC FailedPrecondition.
//
// The shared VolumeStateMachine drives this: after CreateVolume the SM records
// the volume in StateCreated.  NodeStageVolume requires StateControllerPublished
// and therefore rejects the call.
func TestCSIOrdering_NodeStageBeforeControllerPublish(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-stage-before-publish"
	stagingPath := filepath.Join(t.TempDir(), "staging")

	// ── Step 1: CreateVolume (SM: NonExistent → Created) ─────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	// ── Step 2: NodeStageVolume WITHOUT ControllerPublishVolume ──────────────
	// The SM is in StateCreated (not StateControllerPublished), so NodeStage
	// must return FailedPrecondition.
	_, err = env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext:     volumeContext,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	})
	assertGRPCCode(t, err, codes.FailedPrecondition,
		"NodeStageVolume before ControllerPublishVolume should return FailedPrecondition")

	// Verify the connector was NOT called (the guard rejected before any work).
	env.Connector.mu.Lock()
	nConnects := len(env.Connector.ConnectCalls)
	env.Connector.mu.Unlock()
	if nConnects != 0 {
		t.Errorf("expected 0 NVMe-oF Connect calls (operation rejected by SM guard), got %d", nConnects)
	}

	t.Logf("TestCSIOrdering_NodeStageBeforeControllerPublish: "+
		"NodeStageVolume correctly returned FailedPrecondition: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_NodePublishBeforeNodeStage
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_NodePublishBeforeNodeStage verifies that calling
// NodePublishVolume before NodeStageVolume returns gRPC FailedPrecondition.
//
// After ControllerPublishVolume the SM is in StateControllerPublished.
// NodePublishVolume requires StateNodeStaged and therefore rejects the call.
func TestCSIOrdering_NodePublishBeforeNodeStage(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-publish-before-stage"
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── Step 1: CreateVolume (SM: NonExistent → Created) ─────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	// ── Step 2: ControllerPublishVolume (SM: Created → ControllerPublished) ──
	if _, cpErr := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.NodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}); cpErr != nil {
		t.Fatalf("ControllerPublishVolume: %v", cpErr)
	}

	// ── Step 3: NodePublishVolume WITHOUT NodeStageVolume ─────────────────────
	// The SM is in StateControllerPublished (not StateNodeStaged), so
	// NodePublishVolume must return FailedPrecondition.
	stagingPath := filepath.Join(t.TempDir(), "staging")
	_ = volumeContext // unused here — staging was skipped intentionally
	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	})
	assertGRPCCode(t, err, codes.FailedPrecondition,
		"NodePublishVolume before NodeStageVolume should return FailedPrecondition")

	// Verify the mounter was NOT called (operation rejected by SM guard).
	env.Mounter.mu.Lock()
	nMounts := len(env.Mounter.MountCalls)
	env.Mounter.mu.Unlock()
	if nMounts != 0 {
		t.Errorf("expected 0 Mount calls (operation rejected by SM guard), got %d", nMounts)
	}

	t.Logf("TestCSIOrdering_NodePublishBeforeNodeStage: "+
		"NodePublishVolume correctly returned FailedPrecondition: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_NodeUnstageBeforeNodeUnpublish
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_NodeUnstageBeforeNodeUnpublish verifies that calling
// NodeUnstageVolume while the volume is still in NodePublished state (i.e.
// before NodeUnpublishVolume) returns gRPC FailedPrecondition.
//
// The SM is in StateNodePublished after NodePublishVolume.  NodeUnstageVolume
// requires StateNodeStaged or StateNodeStagePartial and therefore rejects the
// call when NodePublished is encountered.
func TestCSIOrdering_NodeUnstageBeforeNodeUnpublish(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-unstage-before-unpublish"
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── Phase 1: CreateVolume ─────────────────────────────────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	// ── Phase 2: ControllerPublishVolume (SM → ControllerPublished) ───────────
	if _, cpErr := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.NodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}); cpErr != nil {
		t.Fatalf("ControllerPublishVolume: %v", cpErr)
	}

	// ── Phase 3: NodeStageVolume (SM → NodeStaged) ────────────────────────────
	if _, stageErr := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext:     volumeContext,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}); stageErr != nil {
		t.Fatalf("NodeStageVolume: %v", stageErr)
	}

	// ── Phase 4: NodePublishVolume (SM → NodePublished) ───────────────────────
	if _, pubErr := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}); pubErr != nil {
		t.Fatalf("NodePublishVolume: %v", pubErr)
	}

	// ── Phase 5: NodeUnstageVolume WITHOUT NodeUnpublishVolume ────────────────
	// The SM is in StateNodePublished.  NodeUnstageVolume requires NodeStaged
	// or NodeStagePartial and must therefore return FailedPrecondition.
	_, err = env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	assertGRPCCode(t, err, codes.FailedPrecondition,
		"NodeUnstageVolume before NodeUnpublishVolume should return FailedPrecondition")

	t.Logf("TestCSIOrdering_NodeUnstageBeforeNodeUnpublish: "+
		"NodeUnstageVolume correctly returned FailedPrecondition: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_NodePublishAfterUnstage
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_NodePublishAfterUnstage verifies that calling
// NodePublishVolume after the volume has been cleanly unstaged (SM in
// StateControllerPublished) returns gRPC FailedPrecondition, because the
// node is no longer connected to the storage target.
func TestCSIOrdering_NodePublishAfterUnstage(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-publish-after-unstage"
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── Phase 1: Full stage/unstage cycle ────────────────────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	if _, cpErr := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.NodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}); cpErr != nil {
		t.Fatalf("ControllerPublishVolume: %v", cpErr)
	}

	if _, stageErr := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext:     volumeContext,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}); stageErr != nil {
		t.Fatalf("NodeStageVolume: %v", stageErr)
	}

	// Unstage (no publish): SM → ControllerPublished
	if _, unstageErr := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}); unstageErr != nil {
		t.Fatalf("NodeUnstageVolume: %v", unstageErr)
	}

	// ── Phase 2: NodePublishVolume after NodeUnstageVolume ───────────────────
	// The SM is now in StateControllerPublished (node is not staged).
	// NodePublishVolume must return FailedPrecondition.
	_, err = env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	})
	assertGRPCCode(t, err, codes.FailedPrecondition,
		"NodePublishVolume after NodeUnstageVolume should return FailedPrecondition")

	t.Logf("TestCSIOrdering_NodePublishAfterUnstage: "+
		"NodePublishVolume correctly returned FailedPrecondition: %v", err)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_FullLifecycleWithSM
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_FullLifecycleWithSM verifies that the complete CSI volume
// lifecycle succeeds when all operations are called in the correct order,
// even with the shared VolumeStateMachine actively enforcing ordering
// constraints.
//
// This is the positive counterpart to the negative ordering tests above:
// it demonstrates that the SM guards do not prevent correct usage.
func TestCSIOrdering_FullLifecycleWithSM(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-full-lifecycle-with-sm"
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// ── 1. CreateVolume ───────────────────────────────────────────────────────
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("[1] CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()
	t.Logf("[1] CreateVolume: volumeID=%q", volumeID)

	// ── 2. ControllerPublishVolume ────────────────────────────────────────────
	if _, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.NodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}); err != nil {
		t.Fatalf("[2] ControllerPublishVolume: %v", err)
	}

	// ── 3. NodeStageVolume ────────────────────────────────────────────────────
	if _, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext:     volumeContext,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}); err != nil {
		t.Fatalf("[3] NodeStageVolume: %v", err)
	}
	if staged, _ := env.Mounter.IsMounted(stagingPath); !staged { //nolint:errcheck // non-actionable in test assertion
		t.Error("[3] staging path should be mounted after NodeStageVolume")
	}

	// ── 4. NodePublishVolume ──────────────────────────────────────────────────
	if _, err := env.Node.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}); err != nil {
		t.Fatalf("[4] NodePublishVolume: %v", err)
	}
	//nolint:errcheck // non-actionable in test assertion
	if published, _ := env.Mounter.IsMounted(targetPath); !published {
		t.Error("[4] target path should be mounted after NodePublishVolume")
	}

	// ── 5. NodeUnpublishVolume ────────────────────────────────────────────────
	if _, err := env.Node.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}); err != nil {
		t.Fatalf("[5] NodeUnpublishVolume: %v", err)
	}
	//nolint:errcheck // non-actionable in test assertion
	if stillPublished, _ := env.Mounter.IsMounted(targetPath); stillPublished {
		t.Error("[5] target path should be unmounted after NodeUnpublishVolume")
	}

	// ── 6. NodeUnstageVolume ──────────────────────────────────────────────────
	if _, err := env.Node.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}); err != nil {
		t.Fatalf("[6] NodeUnstageVolume: %v", err)
	}
	//nolint:errcheck // non-actionable in test assertion
	if stillStaged, _ := env.Mounter.IsMounted(stagingPath); stillStaged {
		t.Error("[6] staging path should be unmounted after NodeUnstageVolume")
	}

	// ── 7. ControllerUnpublishVolume ──────────────────────────────────────────
	if _, err := env.Controller.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   env.NodeID,
	}); err != nil {
		t.Fatalf("[7] ControllerUnpublishVolume: %v", err)
	}

	// ── 8. DeleteVolume ───────────────────────────────────────────────────────
	if _, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("[8] DeleteVolume: %v", err)
	}

	// ── Verify agent RPC call counts ─────────────────────────────────────────
	env.AgentMock.mu.Lock()
	summary := map[string]int{
		"CreateVolume":   len(env.AgentMock.CreateVolumeCalls),
		"ExportVolume":   len(env.AgentMock.ExportVolumeCalls),
		"AllowInitiator": len(env.AgentMock.AllowInitiatorCalls),
		"DenyInitiator":  len(env.AgentMock.DenyInitiatorCalls),
		"UnexportVolume": len(env.AgentMock.UnexportVolumeCalls),
		"DeleteVolume":   len(env.AgentMock.DeleteVolumeCalls),
	}
	env.AgentMock.mu.Unlock()

	for rpc, n := range summary {
		if n != 1 {
			t.Errorf("agent.%s: expected 1 call, got %d", rpc, n)
		}
	}

	t.Log("TestCSIOrdering_FullLifecycleWithSM: all lifecycle steps succeeded with SM guards active")
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIOrdering_IdempotencyWithSM
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIOrdering_IdempotencyWithSM verifies that the SM guards do not break
// idempotency: each node operation can be called a second time with the same
// arguments and returns success.
func TestCSIOrdering_IdempotencyWithSM(t *testing.T) {
	t.Parallel()
	env := newCSILifecycleEnvWithSM(t, "storage-1", "worker-1")
	ctx := context.Background()

	const volName = "pvc-ordering-idempotency-with-sm"
	stagingPath := filepath.Join(t.TempDir(), "staging")
	targetPath := filepath.Join(t.TempDir(), "target")

	// CreateVolume.
	createResp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultLifecycleCreateParams(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	// ControllerPublish.
	pubReq := &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.NodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	}
	for i := range 2 {
		if _, err := env.Controller.ControllerPublishVolume(ctx, pubReq); err != nil {
			t.Fatalf("ControllerPublishVolume (call %d): %v", i+1, err)
		}
	}

	// NodeStage (twice — second call must be idempotent).
	stageReq := &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext:     volumeContext,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}
	for i := range 2 {
		if _, err := env.Node.NodeStageVolume(ctx, stageReq); err != nil {
			t.Fatalf("NodeStageVolume (call %d): %v", i+1, err)
		}
	}

	// NodePublish (twice).
	publishReq := &csi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability:  defaultVolumeCapabilities()[0],
	}
	for i := range 2 {
		if _, err := env.Node.NodePublishVolume(ctx, publishReq); err != nil {
			t.Fatalf("NodePublishVolume (call %d): %v", i+1, err)
		}
	}

	// NodeUnpublish (twice).
	unpubReq := &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}
	for i := range 2 {
		if _, err := env.Node.NodeUnpublishVolume(ctx, unpubReq); err != nil {
			t.Fatalf("NodeUnpublishVolume (call %d): %v", i+1, err)
		}
	}

	// NodeUnstage (twice).
	unstageReq := &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	}
	for i := range 2 {
		if _, err := env.Node.NodeUnstageVolume(ctx, unstageReq); err != nil {
			t.Fatalf("NodeUnstageVolume (call %d): %v", i+1, err)
		}
	}

	t.Log("TestCSIOrdering_IdempotencyWithSM: all idempotency checks passed with SM guards active")
}
