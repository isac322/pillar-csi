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

// Tests for ControllerUnpublishVolume revocation behavior.
//
// ControllerUnpublishVolume revokes exactly the publications recorded in the
// volume's PillarVolumeState:
//
//   - no record → success without contacting the agent
//   - recorded initiator (NQN / IQN / NFS node ID) → DenyInitiator, then the
//     record is removed, even when the node's CSINode is gone
//   - empty node_id → every recorded publication is revoked
//   - DenyInitiator failure → record kept (fail-closed)
//   - storage node (PillarAgent) gone → records dropped
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestControllerUnpublishVolume

import (
	"context"
	"io"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/testutil/fakeuid"
)

// newUnpublishTestEnv builds a ControllerServer wired to a fake k8s client
// that has a PillarAgent but no CSINode and no PillarVolumeState by default.
// Callers seed the volume's PillarVolumeState (see volumeStateFor) with the
// publication records each test case needs.
func newUnpublishTestEnv(t *testing.T, objs ...ctrlclient.Object) *controllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme storagev1: %v", err)
	}

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status: v1alpha1.PillarAgentStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	allObjs := append([]ctrlclient.Object{target}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(fakeuid.Assign(allObjs...)...).
		WithStatusSubresource(&v1alpha1.PillarAgent{}, &v1alpha1.PillarVolumeState{}).
		Build()

	agent := &mockAgentClient{}
	dialer := func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, nopCloser{}, nil
	}

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &controllerTestEnv{srv: srv, agent: agent, scheme: scheme}
}

// baseUnpublishRequest returns a minimal valid ControllerUnpublishVolumeRequest
// for the nvmeof-tcp protocol targeting "worker-node-1".
func baseUnpublishRequest() *csi.ControllerUnpublishVolumeRequest {
	return &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123",
		NodeId:   "worker-node-1",
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Revocation driven by publication records
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerUnpublishVolume_NoPublicationRecord_Succeeds verifies that a
// volume without a matching publication record (or without a
// PillarVolumeState at all) is already unpublished: success, no agent call.
func TestControllerUnpublishVolume_NoPublicationRecord_Succeeds(t *testing.T) {
	t.Parallel()

	volumeID := baseUnpublishRequest().GetVolumeId()
	other := exclPub(exclNode2, exclNQN(exclNode2), csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, true)
	for name, objs := range map[string][]ctrlclient.Object{
		"no PillarVolumeState":   nil,
		"no record for the node": {volumeStateFor(volumeID, other)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			env := newUnpublishTestEnv(t, objs...)
			if _, err := env.srv.ControllerUnpublishVolume(context.Background(), baseUnpublishRequest()); err != nil {
				t.Fatalf("ControllerUnpublishVolume: %v", err)
			}
			if env.agent.denyInitiatorCalls != 0 {
				t.Errorf("DenyInitiator calls = %d, want 0", env.agent.denyInitiatorCalls)
			}
		})
	}
}

// TestControllerUnpublishVolume_DeniesRecordedInitiator verifies that the
// initiator recorded at publish time is revoked for every protocol, without
// consulting the CSINode (none is seeded: the node may already be deleted),
// and that the record is removed afterwards.
func TestControllerUnpublishVolume_DeniesRecordedInitiator(t *testing.T) {
	t.Parallel()

	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	testCases := []struct {
		name      string
		volumeID  string
		initiator string
	}{
		{"nvmeof host NQN", baseUnpublishRequest().GetVolumeId(), exclNQN(exclNode1)},
		{"iscsi IQN", "storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123", "iqn.1993-08.org.debian:01:worker-node-1"},
		{"nfs node ID", exclNFSID, exclNode1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := newUnpublishTestEnv(t, volumeStateFor(tc.volumeID, exclPub(exclNode1, tc.initiator, mode, false)))
			_, err := env.srv.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
				VolumeId: tc.volumeID, NodeId: exclNode1,
			})
			if err != nil {
				t.Fatalf("ControllerUnpublishVolume: %v", err)
			}
			if env.agent.denyInitiatorCalls != 1 {
				t.Fatalf("DenyInitiator calls = %d, want 1", env.agent.denyInitiatorCalls)
			}
			if got := env.agent.lastDenyInitiator.GetInitiatorId(); got != tc.initiator {
				t.Errorf("DenyInitiator.InitiatorId = %q, want %q", got, tc.initiator)
			}
			if got := exclPublishedNodes(t, env.srv.k8sClient, tc.volumeID); len(got) != 0 {
				t.Errorf("publishedNodes after unpublish = %+v, want none", got)
			}
		})
	}
}

// TestControllerUnpublishVolume_EmptyNodeID_RevokesAll verifies CSI §4.5.2:
// an empty node_id unpublishes the volume from every recorded node.
func TestControllerUnpublishVolume_EmptyNodeID_RevokesAll(t *testing.T) {
	t.Parallel()

	volumeID := baseUnpublishRequest().GetVolumeId()
	mode := csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	env := newUnpublishTestEnv(t, volumeStateFor(volumeID,
		exclPub(exclNode1, exclNQN(exclNode1), mode, true),
		exclPub(exclNode2, exclNQN(exclNode2), mode, true)))
	env.srv.GetStateMachine().ForceState(volumeID, StateControllerPublished)

	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if env.agent.denyInitiatorCalls != 2 {
		t.Errorf("DenyInitiator calls = %d, want 2", env.agent.denyInitiatorCalls)
	}
	if got := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(got) != 0 {
		t.Errorf("publishedNodes = %+v, want none", got)
	}
	if got := env.srv.GetStateMachine().GetState(volumeID); got != StateCreated {
		t.Errorf("state = %v, want Created", got)
	}
}

// TestControllerUnpublishVolume_OneOfTwoNodes_StaysPublished verifies that
// unpublishing one of two reader nodes keeps the other record and leaves the
// volume in the ControllerPublished state.
func TestControllerUnpublishVolume_OneOfTwoNodes_StaysPublished(t *testing.T) {
	t.Parallel()

	volumeID := baseUnpublishRequest().GetVolumeId()
	mode := csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	env := newUnpublishTestEnv(t, volumeStateFor(volumeID,
		exclPub(exclNode1, exclNQN(exclNode1), mode, true),
		exclPub(exclNode2, exclNQN(exclNode2), mode, true)))
	env.srv.GetStateMachine().ForceState(volumeID, StateControllerPublished)

	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), baseUnpublishRequest()); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	got := exclPublishedNodes(t, env.srv.k8sClient, volumeID)
	if len(got) != 1 || got[0].NodeID != exclNode2 {
		t.Fatalf("publishedNodes = %+v, want only %s", got, exclNode2)
	}
	if state := env.srv.GetStateMachine().GetState(volumeID); state != StateControllerPublished {
		t.Errorf("state = %v, want ControllerPublished", state)
	}
}

// TestControllerUnpublishVolume_DenyFailureKeepsRecord verifies fail-closed
// revocation: when DenyInitiator fails the record stays, so another node
// still cannot publish the SINGLE_NODE_WRITER volume.
func TestControllerUnpublishVolume_DenyFailureKeepsRecord(t *testing.T) {
	t.Parallel()

	volumeID := baseUnpublishRequest().GetVolumeId()
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	env := newUnpublishTestEnv(t, append(exclCSINodes(),
		volumeStateFor(volumeID, exclPub(exclNode1, exclNQN(exclNode1), mode, false)))...)
	env.agent.denyInitiatorErr = status.Error(codes.Internal, "configfs unlink failed")
	ctx := context.Background()

	if _, err := env.srv.ControllerUnpublishVolume(ctx, baseUnpublishRequest()); status.Code(err) != codes.Internal {
		t.Fatalf("ControllerUnpublishVolume code = %v (err=%v), want Internal", status.Code(err), err)
	}
	if got := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(got) != 1 {
		t.Fatalf("publishedNodes = %+v, want the record kept", got)
	}
	_, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode2, mode, false))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("publish to node 2 code = %v (err=%v), want FailedPrecondition", status.Code(err), err)
	}
}

// TestControllerUnpublishVolume_AgentGone_KeepsRecords verifies that when the
// storage node's PillarAgent object no longer exists the unpublish fails
// closed: the node's ACL may still exist on that storage node, so the record
// is kept (the volume stays unavailable to other SINGLE_NODE_* publishers) and
// no agent is contacted.
func TestControllerUnpublishVolume_AgentGone_KeepsRecords(t *testing.T) {
	t.Parallel()

	volumeID := "gone-node/nvmeof-tcp/zfs-zvol/tank/pvc-gone"
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	env := newUnpublishTestEnv(t, volumeStateFor(volumeID, exclPub(exclNode1, exclNQN(exclNode1), mode, false)))

	_, err := env.srv.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: exclNode1,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ControllerUnpublishVolume: %v, want FailedPrecondition", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator calls = %d, want 0", env.agent.denyInitiatorCalls)
	}
	if got := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(got) != 1 || got[0].NodeID != exclNode1 {
		t.Errorf("publishedNodes = %+v, want the %s record kept", got, exclNode1)
	}
}

// TestControllerUnpublishVolume_HandoverToAnotherNode verifies the normal
// failover sequence: once node 1 is unpublished, node 2 may publish.
func TestControllerUnpublishVolume_HandoverToAnotherNode(t *testing.T) {
	t.Parallel()

	volumeID := baseUnpublishRequest().GetVolumeId()
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	env := newUnpublishTestEnv(t, append(exclCSINodes(), volumeStateFor(volumeID))...)
	ctx := context.Background()

	if _, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode1, mode, false)); err != nil {
		t.Fatalf("publish node 1: %v", err)
	}
	if _, err := env.srv.ControllerUnpublishVolume(ctx, baseUnpublishRequest()); err != nil {
		t.Fatalf("unpublish node 1: %v", err)
	}
	if _, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode2, mode, false)); err != nil {
		t.Fatalf("publish node 2 after handover: %v", err)
	}
	got := exclPublishedNodes(t, env.srv.k8sClient, volumeID)
	if len(got) != 1 || got[0].NodeID != exclNode2 || got[0].InitiatorID != exclNQN(exclNode2) {
		t.Errorf("publishedNodes = %+v, want only %s", got, exclNode2)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Additional ControllerPublishVolume cases
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerPublishVolume_ISCSI_SuccessWithAnnotation verifies that
// ControllerPublishVolume resolves the IQN from the CSINode annotation and
// passes it as initiator_id to AllowInitiator for the iSCSI protocol.
func TestControllerPublishVolume_ISCSI_SuccessWithAnnotation(t *testing.T) {
	t.Parallel()

	const initiatorIQN = "iqn.1993-08.org.debian:01:worker-node-1"

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationISCSIInitiatorIQN: initiatorIQN,
			},
		},
	}
	env := newPublishTestEnv(t, csiNode, volumeStateFor("storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123"))

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123",
		NodeId:   "worker-node-1",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{
				Block: &csi.VolumeCapability_BlockVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	_, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume iSCSI: unexpected error: %v", err)
	}

	if env.agent.allowInitiatorCalls != 1 {
		t.Errorf("AllowInitiator call count = %d, want 1", env.agent.allowInitiatorCalls)
	}
	if got := env.agent.lastAllowInitiator.InitiatorId; got != initiatorIQN {
		t.Errorf("AllowInitiator.InitiatorId = %q, want %q", got, initiatorIQN)
	}
}

// TestControllerPublishVolume_NFS_PassthroughNodeID verifies that for the NFS
// protocol the nodeID is passed directly to AllowInitiator without reading any
// CSINode annotation.  RFC §5.2: NFS annotation-based resolution is Phase 2.
func TestControllerPublishVolume_NFS_PassthroughNodeID(t *testing.T) {
	t.Parallel()

	// No CSINode seeded.
	env := newPublishTestEnv(t, volumeStateFor(exclNFSID))

	const nodeID = "worker-node-1"
	req := &csi.ControllerPublishVolumeRequest{
		VolumeId: "storage-node-1/nfs/nfs-share/tank/pvc-abc123",
		NodeId:   nodeID,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{
				Block: &csi.VolumeCapability_BlockVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
	_, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume NFS: unexpected error: %v", err)
	}

	if env.agent.allowInitiatorCalls != 1 {
		t.Errorf("AllowInitiator call count = %d, want 1", env.agent.allowInitiatorCalls)
	}
	if got := env.agent.lastAllowInitiator.InitiatorId; got != nodeID {
		t.Errorf("AllowInitiator.InitiatorId = %q, want nodeID %q", got, nodeID)
	}
}
