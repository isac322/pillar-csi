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

// Package component_test — CSI Node/Controller service gap test cases.
//
// This file implements C-NEW-15 through C-NEW-18, covering PRD gaps identified
// in the CSI Node and Controller service areas:
//
//   - C-NEW-15: NodePublishVolume edge cases (partial-stage guard, re-publish
//     after unpublish)
//   - C-NEW-16: NodeUnpublishVolume edge cases (lost mount state after node
//     crash, unpublish when not published)
//   - C-NEW-17: Node failure scenarios (agent dial failures during publish /
//     unpublish, context cancellation during stage)
//   - C-NEW-18: Quota enforcement (ResourceExhausted propagation from agent
//     CreateVolume, ExportVolume, and ExpandVolume)
//
// All tests use the existing mock infrastructure (csiMockConnector,
// csiMockMounter, csiMockAgent) defined in csi_node_test.go and
// csi_controller_test.go.  No stubs, no t.Skip().
package component_test

import (
	"context"
	"errors"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-15: NodePublishVolume edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodePublishVolume_StagePartial_FailedPrecondition verifies that
// NodePublishVolume returns FailedPrecondition when the volume is in
// StateNodeStagePartial (i.e. a previous NodeStageVolume attempt failed mid-way).
//
// C-NEW-15-1: PRD requirement — the state machine must reject NodePublishVolume
// for any state other than StateNodeStaged or StateNodePublished.
// StateNodeStagePartial represents a broken staging attempt and must not
// allow publish to proceed, because the connector may not be established and
// the device path may not exist.
func TestCSINode_NodePublishVolume_StagePartial_FailedPrecondition(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-stage-partial"
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// Force the volume into StateNodeStagePartial — simulates a staging
	// attempt that wrote the state file but failed before the connector
	// completed the NVMe-oF handshake.
	env.sm.ForceState(volumeID, pillarcsi.StateNodeStagePartial)

	_, err := env.node.NodePublishVolume(ctx, &csipb.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	if err == nil {
		t.Fatal("NodePublishVolume: expected FailedPrecondition for StateNodeStagePartial, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want FailedPrecondition", st.Code())
	}
	// Mount must not be called — publish should be blocked before I/O.
	if env.mounter.mountCalls != 0 {
		t.Errorf("mounter.Mount calls = %d, want 0 (blocked by state machine)", env.mounter.mountCalls)
	}
}

// TestCSINode_NodePublishVolume_RepublishAfterUnpublish_Succeeds verifies that
// after a NodePublishVolume / NodeUnpublishVolume cycle, NodePublishVolume can
// be called again on the same volume.
//
// C-NEW-15-2: PRD requirement — the state machine must allow NodePublishVolume
// to be re-entered after NodeUnpublishVolume transitions the volume back to
// StateNodeStaged.  This models the typical Pod restart scenario:
//
//	ControllerPublished → NodeStaged → NodePublished → NodeStaged → NodePublished
func TestCSINode_NodePublishVolume_RepublishAfterUnpublish_Succeeds(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-republish"
	stagingPath := t.TempDir()
	targetPath := t.TempDir()

	// Precondition: ControllerPublishVolume has already been called.
	env.sm.ForceState(volumeID, pillarcsi.StateControllerPublished)

	stageReq := &csipb.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetID: "nqn.2026-01.com.pillar-csi:" + volumeID,
			pillarcsi.VolumeContextKeyAddress:  "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:     "4420",
		},
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	publishReq := &csipb.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	unpublishReq := &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	}

	// Step 1: Stage the volume.
	if _, err := env.node.NodeStageVolume(ctx, stageReq); err != nil {
		t.Fatalf("NodeStageVolume (initial): %v", err)
	}

	// Step 2: First publish.
	if _, err := env.node.NodePublishVolume(ctx, publishReq); err != nil {
		t.Fatalf("NodePublishVolume (1st): %v", err)
	}

	// Step 3: Unpublish.
	if _, err := env.node.NodeUnpublishVolume(ctx, unpublishReq); err != nil {
		t.Fatalf("NodeUnpublishVolume: %v", err)
	}

	// Step 4: Re-publish.  The volume must be in NodeStaged after unpublish,
	// so this second publish should succeed without FailedPrecondition.
	if _, err := env.node.NodePublishVolume(ctx, publishReq); err != nil {
		t.Fatalf("NodePublishVolume (2nd / re-publish): unexpected error: %v", err)
	}

	// Verify state machine reflects NodePublished.
	if got := env.sm.GetState(volumeID); got != pillarcsi.StateNodePublished {
		t.Errorf("final SM state = %v, want NodePublished", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-16: NodeUnpublishVolume edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestCSINode_NodeUnpublishVolume_LostMountState_TreatedAsSuccess verifies that
// NodeUnpublishVolume returns success when the mounter reports the target path
// as not mounted.
//
// C-NEW-16-1: PRD requirement — after a node crash or kubelet restart, the
// in-kernel mount table may be rebuilt without the pod's bind mount.
// NodeUnpublishVolume must treat an already-unmounted target as idempotent
// success (CSI spec §5.4.2) and must not call Unmount.
//
// This test simulates the post-crash scenario by overriding IsMounted to
// return false from the first call, as if the mount state was lost.
func TestCSINode_NodeUnpublishVolume_LostMountState_TreatedAsSuccess(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	ctx := context.Background()

	targetPath := t.TempDir()

	// Override IsMounted to return false unconditionally, simulating a node
	// crash that wiped the kernel mount table.
	env.mounter.isMountedFn = func(_ string) (bool, error) {
		return false, nil // mount state was lost
	}

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-crash-test",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume: expected success for lost mount state, got: %v", err)
	}
	// Unmount must not be called — the mount is already gone.
	if env.mounter.unmountCalls != 0 {
		t.Errorf("Unmount calls = %d, want 0 (mount state was already lost)", env.mounter.unmountCalls)
	}
}

// TestCSINode_NodeUnpublishVolume_VolumeStagedNotPublished_IdempotentSuccess
// verifies that NodeUnpublishVolume returns success (idempotent) when the
// volume is in NodeStaged state (published to zero pods).
//
// C-NEW-16-2: PRD requirement — CSI spec §5.4.2 requires NodeUnpublishVolume
// to succeed when the volume is not currently NodePublished.  A common case is
// when the CO calls NodeUnpublishVolume after a Pod eviction, but the Pod's
// volume was staged yet never published (e.g. crashed before mount).  The
// implementation must not call Unmount and must not return FailedPrecondition.
func TestCSINode_NodeUnpublishVolume_VolumeStagedNotPublished_IdempotentSuccess(t *testing.T) {
	t.Parallel()
	env := newCSINodeSMTestEnv(t)
	ctx := context.Background()

	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-staged-only"
	targetPath := t.TempDir()

	// Volume was staged but never published.
	env.sm.ForceState(volumeID, pillarcsi.StateNodeStaged)

	_, err := env.node.NodeUnpublishVolume(ctx, &csipb.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		// CSI spec §5.4.2: must succeed.
		t.Fatalf("NodeUnpublishVolume: expected success for staged-not-published state, got: %v", err)
	}
	// Unmount must never be called — target path was never mounted.
	if env.mounter.unmountCalls != 0 {
		t.Errorf("Unmount calls = %d, want 0 (volume was never published)", env.mounter.unmountCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-17: Node failure scenarios
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_ControllerPublishVolume_AgentDialFails_Unavailable verifies
// that ControllerPublishVolume returns Unavailable when the agent dialer fails
// to establish a connection.
//
// C-NEW-17-1: PRD requirement — when the pillar-agent is unreachable at
// ControllerPublishVolume time (e.g. network partition or agent crash), the
// CSI controller must return Unavailable so that the CO can retry the
// ControllerPublishVolume call later.  AllowInitiator must not be called
// (because there is no agent connection).
func TestCSIController_ControllerPublishVolume_AgentDialFails_Unavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dialErr := errors.New("dial tcp 192.168.1.10:9500: connect: connection refused")
	env := newCSIControllerTestEnvWithDialErr(t, dialErr)

	const nodeID = "worker-node-agent-down"
	seedCSINodeForNVMeOF(ctx, t, env.k8sClient, nodeID,
		"nqn.2014-08.org.nvmexpress:uuid:agent-dial-fail")

	_, err := env.srv.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeID,
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
		VolumeContext: map[string]string{
			pillarcsi.VolumeContextKeyTargetID: "nqn.2026-01.com.pillar-csi:test",
			pillarcsi.VolumeContextKeyAddress:  "192.168.1.10",
			pillarcsi.VolumeContextKeyPort:     "4420",
		},
	})
	if err == nil {
		t.Fatal("ControllerPublishVolume: expected Unavailable error for dial failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %v, want Unavailable", st.Code())
	}
}

// TestCSIController_ControllerUnpublishVolume_AgentDialFails_Unavailable
// verifies that ControllerUnpublishVolume returns Unavailable when the agent
// dialer fails.
//
// C-NEW-17-2: PRD requirement — like ControllerPublishVolume, the unpublish
// path must not silently succeed when the agent is unreachable.  Returning
// Unavailable causes the CO to retry so that DenyInitiator is eventually
// called and the initiator ACL entry is properly revoked.
func TestCSIController_ControllerUnpublishVolume_AgentDialFails_Unavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dialErr := errors.New("dial tcp 192.168.1.10:9500: connect: connection refused")
	env := newCSIControllerTestEnvWithDialErr(t, dialErr)

	const nodeID = "worker-node-agent-down"
	seedCSINodeForNVMeOF(ctx, t, env.k8sClient, nodeID,
		"nqn.2014-08.org.nvmexpress:uuid:unpublish-dial-fail")

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeID,
	})
	if err == nil {
		t.Fatal("ControllerUnpublishVolume: expected Unavailable error for dial failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %v, want Unavailable", st.Code())
	}
}

// TestCSINode_NodeStageVolume_ContextCancelled_ErrorPropagated verifies that
// when the caller cancels the context during NodeStageVolume, the error is
// propagated (not swallowed) and the operation fails cleanly.
//
// C-NEW-17-3: PRD requirement — node plugins must respect context cancellation.
// When a NodeStageVolume call is canceled (e.g. kubelet timeout), the
// underlying connector's Connect call receives the canceled context and
// returns an error.  The NodeServer must propagate that error rather than
// silently succeeding, so that the CO knows to retry or give up.
func TestCSINode_NodeStageVolume_ContextCancelled_ErrorPropagated(t *testing.T) {
	t.Parallel()
	env := newCSINodeTestEnv(t)
	stagingPath := t.TempDir()

	// Create a context that is already canceled to simulate an expired deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Configure the connector to return the context error, as a real connector
	// would when it detects a canceled context during TCP dial.
	env.connector.connectFn = func(ctx context.Context, _, _, _ string) error {
		return ctx.Err() // propagate the cancellation
	}

	_, err := env.node.NodeStageVolume(ctx, baseStageRequest(stagingPath))
	if err == nil {
		t.Fatal("NodeStageVolume: expected error from canceled context, got nil")
	}
	// The error should be non-OK; exact code depends on how the implementation
	// wraps context.Canceled.
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("error code = OK, want non-OK for canceled context")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// C-NEW-18: Quota enforcement
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_QuotaExceeded_ResourceExhausted verifies that
// when the agent returns ResourceExhausted for CreateVolume (storage pool full
// or per-tenant quota exceeded), the CSI controller propagates the same gRPC
// status code to the CO.
//
// C-NEW-18-1: PRD requirement — the CO (Kubernetes provisioner) uses the gRPC
// status code to decide whether to retry.  ResourceExhausted is a permanent
// failure for this volume creation; returning it allows the CO to surface a
// human-readable "quota exceeded" event on the PVC instead of retrying
// indefinitely.
func TestCSIController_CreateVolume_QuotaExceeded_ResourceExhausted(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// Agent reports that the storage pool quota is exhausted.
	env.agent.createVolumeFn = func(
		_ context.Context, _ *agentv1.CreateVolumeRequest,
	) (*agentv1.CreateVolumeResponse, error) {
		return nil, status.Errorf(codes.ResourceExhausted,
			"pool tank: quota exceeded (used 10 GiB / limit 10 GiB)")
	}

	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("CreateVolume: expected ResourceExhausted error for quota exceeded, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("error code = %v, want ResourceExhausted", st.Code())
	}
	// ExportVolume must not be called — creation failed before export.
	if env.agent.exportVolumeCalls != 0 {
		t.Errorf("agent.ExportVolume calls = %d, want 0 (create failed)", env.agent.exportVolumeCalls)
	}
}

// TestCSIController_CreateVolume_ExportVolumeQuotaExceeded_ResourceExhausted
// verifies that when agent.CreateVolume succeeds but agent.ExportVolume returns
// ResourceExhausted (e.g. NVMe-oF subsystem namespace table full), the CSI
// controller propagates ResourceExhausted.
//
// C-NEW-18-2: PRD requirement — the export step can also fail with
// ResourceExhausted if the NVMe-oF target's namespace table is exhausted (a
// configfs limit).  The CO must see ResourceExhausted so that it does not
// retry indefinitely.  A partial-creation cleanup is expected (PillarVolume
// CRD records the failure), but the gRPC status code must be preserved.
func TestCSIController_CreateVolume_ExportVolumeQuotaExceeded_ResourceExhausted(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// CreateVolume succeeds (volume provisioned on the backend).
	env.agent.createVolumeFn = nil // default: success

	// ExportVolume fails because the NVMe-oF subsystem namespace table is full.
	env.agent.exportVolumeFn = func(
		_ context.Context, _ *agentv1.ExportVolumeRequest,
	) (*agentv1.ExportVolumeResponse, error) {
		return nil, status.Errorf(codes.ResourceExhausted,
			"nvmet: subsystem namespace table full (max 1024 namespaces)")
	}

	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("CreateVolume: expected ResourceExhausted for export failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("error code = %v, want ResourceExhausted", st.Code())
	}
	// CreateVolume was called but ExportVolume failed.
	if env.agent.createVolumeCalls != 1 {
		t.Errorf("agent.CreateVolume calls = %d, want 1", env.agent.createVolumeCalls)
	}
	if env.agent.exportVolumeCalls != 1 {
		t.Errorf("agent.ExportVolume calls = %d, want 1", env.agent.exportVolumeCalls)
	}
}

// TestCSIController_ControllerExpandVolume_QuotaExceeded_ResourceExhausted
// verifies that when agent.ExpandVolume returns ResourceExhausted (storage pool
// quota prevents resize), ControllerExpandVolume propagates ResourceExhausted.
//
// C-NEW-18-3: PRD requirement — volume expansion can hit a per-pool quota limit
// even if the initial provisioning was within quota.  The CO must receive
// ResourceExhausted so that it can surface a "quota exceeded" event on the PVC
// and halt the expansion rather than retrying.
func TestCSIController_ControllerExpandVolume_QuotaExceeded_ResourceExhausted(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.expandVolumeFn = func(
		_ context.Context, _ *agentv1.ExpandVolumeRequest,
	) (*agentv1.ExpandVolumeResponse, error) {
		return nil, status.Errorf(codes.ResourceExhausted,
			"pool tank: expansion refused — would exceed pool quota")
	}

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: 100 << 30, // 100 GiB — beyond quota
		},
	})
	if err == nil {
		t.Fatal("ControllerExpandVolume: expected ResourceExhausted for quota exceeded, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("error code = %v, want ResourceExhausted", st.Code())
	}
	if env.agent.expandVolumeCalls != 1 {
		t.Errorf("agent.ExpandVolume calls = %d, want 1", env.agent.expandVolumeCalls)
	}
}
