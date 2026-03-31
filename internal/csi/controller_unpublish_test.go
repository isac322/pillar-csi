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

// Tests for ControllerUnpublishVolume annotation lookup behavior.
//
// These tests cover the RFC §5.2 annotation-based initiator resolution path
// for ControllerUnpublishVolume:
//
//   - Missing CSINode → success (idempotent: storage node is decommissioned)
//   - Missing annotation → success (idempotent: identity gone, nothing to revoke)
//   - NVMe-oF annotation present → DenyInitiator called with resolved NQN
//   - iSCSI annotation present → DenyInitiator called with resolved IQN
//   - NFS protocol → nodeID passed directly to DenyInitiator
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestControllerUnpublishVolume

import (
	"context"
	"io"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// newUnpublishTestEnv builds a ControllerServer wired to a fake k8s client
// that has a PillarTarget but no CSINode by default.  Callers can seed CSINode
// objects as needed for each test case.
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

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	allObjs := append([]ctrlclient.Object{target}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(allObjs...).
		WithStatusSubresource(&v1alpha1.PillarTarget{}).
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
// Idempotency: missing CSINode or annotation
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerUnpublishVolume_CSINodeNotFound_Succeeds verifies that
// ControllerUnpublishVolume returns success when the CSINode does not exist.
//
// RFC §5.2: if the node's identity is gone (CSINode absent), there is no
// ACL entry to revoke, so the operation succeeds idempotently.
func TestControllerUnpublishVolume_CSINodeNotFound_Succeeds(t *testing.T) {
	t.Parallel()

	// No CSINode seeded — the lookup will return NotFound.
	env := newUnpublishTestEnv(t)

	_, err := env.srv.ControllerUnpublishVolume(context.Background(), baseUnpublishRequest())
	if err != nil {
		t.Errorf("ControllerUnpublishVolume with missing CSINode: want nil error, got %v", err)
	}
	// DenyInitiator must NOT have been called because we returned early.
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator call count = %d, want 0 (no CSINode = nothing to revoke)", env.agent.denyInitiatorCalls)
	}
}

// TestControllerUnpublishVolume_AnnotationMissing_Succeeds verifies that
// ControllerUnpublishVolume returns success when the CSINode exists but the
// nvmeof-host-nqn annotation is absent.
//
// RFC §5.2: annotation absence during unpublish means the node's identity
// was never written (or was cleared), so no ACL entry can exist.
func TestControllerUnpublishVolume_AnnotationMissing_Succeeds(t *testing.T) {
	t.Parallel()

	// CSINode exists but has no annotations.
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// Annotations deliberately omitted.
		},
	}
	env := newUnpublishTestEnv(t, csiNode)

	_, err := env.srv.ControllerUnpublishVolume(context.Background(), baseUnpublishRequest())
	if err != nil {
		t.Errorf("ControllerUnpublishVolume with missing annotation: want nil error, got %v", err)
	}
	// DenyInitiator must NOT have been called.
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator call count = %d, want 0 (missing annotation = nothing to revoke)",
			env.agent.denyInitiatorCalls)
	}
}

// TestControllerUnpublishVolume_ISCSIAnnotationMissing_Succeeds verifies the
// same idempotent behavior for the iSCSI protocol when the annotation is absent.
func TestControllerUnpublishVolume_ISCSIAnnotationMissing_Succeeds(t *testing.T) {
	t.Parallel()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// No iSCSI annotation.
		},
	}
	env := newUnpublishTestEnv(t, csiNode)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123",
		NodeId:   "worker-node-1",
	}
	_, err := env.srv.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Errorf("ControllerUnpublishVolume iSCSI with missing annotation: want nil error, got %v", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator call count = %d, want 0", env.agent.denyInitiatorCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Success paths: annotation present → DenyInitiator called
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerUnpublishVolume_NVMeoF_SuccessWithAnnotation verifies that
// ControllerUnpublishVolume resolves the NQN from the CSINode annotation and
// passes it as initiator_id to DenyInitiator when the annotation is present.
func TestControllerUnpublishVolume_NVMeoF_SuccessWithAnnotation(t *testing.T) {
	t.Parallel()

	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:worker-node-1-unpublish"

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN: hostNQN,
			},
		},
	}
	env := newUnpublishTestEnv(t, csiNode)

	_, err := env.srv.ControllerUnpublishVolume(context.Background(), baseUnpublishRequest())
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: unexpected error: %v", err)
	}

	// DenyInitiator must have been called exactly once with the resolved NQN.
	if env.agent.denyInitiatorCalls != 1 {
		t.Errorf("DenyInitiator call count = %d, want 1", env.agent.denyInitiatorCalls)
	}
	if env.agent.lastDenyInitiator == nil {
		t.Fatal("lastDenyInitiator is nil")
	}
	if got := env.agent.lastDenyInitiator.InitiatorId; got != hostNQN {
		t.Errorf("DenyInitiator.InitiatorId = %q, want %q", got, hostNQN)
	}
}

// TestControllerUnpublishVolume_ISCSI_SuccessWithAnnotation verifies that
// ControllerUnpublishVolume passes the IQN to DenyInitiator for iSCSI.
func TestControllerUnpublishVolume_ISCSI_SuccessWithAnnotation(t *testing.T) {
	t.Parallel()

	const initiatorIQN = "iqn.1993-08.org.debian:01:worker-node-1-unpublish"

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationISCSIInitiatorIQN: initiatorIQN,
			},
		},
	}
	env := newUnpublishTestEnv(t, csiNode)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123",
		NodeId:   "worker-node-1",
	}
	_, err := env.srv.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume iSCSI: unexpected error: %v", err)
	}

	if env.agent.denyInitiatorCalls != 1 {
		t.Errorf("DenyInitiator call count = %d, want 1", env.agent.denyInitiatorCalls)
	}
	if got := env.agent.lastDenyInitiator.InitiatorId; got != initiatorIQN {
		t.Errorf("DenyInitiator.InitiatorId = %q, want %q", got, initiatorIQN)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol passthrough: NFS uses nodeID directly
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerUnpublishVolume_NFS_PassthroughNodeID verifies that for the
// NFS protocol the nodeID is passed directly to DenyInitiator without reading
// any CSINode annotation.  RFC §5.2: NFS annotation-based resolution is Phase 2.
func TestControllerUnpublishVolume_NFS_PassthroughNodeID(t *testing.T) {
	t.Parallel()

	// No CSINode seeded — any CSINode lookup would fail, proving the function
	// does NOT attempt one for the NFS protocol.
	env := newUnpublishTestEnv(t)

	const nodeID = "worker-node-1"
	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "storage-node-1/nfs/nfs-share/tank/pvc-abc123",
		NodeId:   nodeID,
	}
	_, err := env.srv.ControllerUnpublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume NFS: unexpected error: %v", err)
	}

	// DenyInitiator must have been called with the nodeID as-is.
	if env.agent.denyInitiatorCalls != 1 {
		t.Errorf("DenyInitiator call count = %d, want 1", env.agent.denyInitiatorCalls)
	}
	if got := env.agent.lastDenyInitiator.InitiatorId; got != nodeID {
		t.Errorf("DenyInitiator.InitiatorId = %q, want nodeID %q", got, nodeID)
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
	env := newPublishTestEnv(t, csiNode)

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
	env := newPublishTestEnv(t)

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
