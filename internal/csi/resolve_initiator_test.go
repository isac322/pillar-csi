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

// Unit tests for resolveInitiatorID — the private method that looks up the
// protocol-specific initiator identity from CSINode annotations.
//
// These tests exercise the annotation read/parse logic in isolation without
// going through the full ControllerPublishVolume/ControllerUnpublishVolume path,
// covering all protocol cases defined in RFC §5.2.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestResolveInitiatorID

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// newMinimalControllerServer builds a ControllerServer backed by a fake
// k8s client pre-seeded with the given objects.  The AgentDialer is nil
// because resolveInitiatorID never dials an agent.
func newMinimalControllerServer(t *testing.T, objs ...ctrlclient.Object) *ControllerServer {
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	// dialAgent is nil intentionally — resolveInitiatorID never dials.
	return NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// Annotation key contract
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveInitiatorID_AnnotationKeyValues pins the annotation key constants
// to the RFC-specified wire values.  Any accidental rename will fail this test
// before any cluster-level behavior changes.
func TestResolveInitiatorID_AnnotationKeyValues(t *testing.T) {
	t.Parallel()

	if AnnotationNVMeOFHostNQN != "pillar-csi.bhyoo.com/nvmeof-host-nqn" {
		t.Errorf("AnnotationNVMeOFHostNQN = %q, want \"pillar-csi.bhyoo.com/nvmeof-host-nqn\"",
			AnnotationNVMeOFHostNQN)
	}
	if AnnotationISCSIInitiatorIQN != "pillar-csi.bhyoo.com/iscsi-initiator-iqn" {
		t.Errorf("AnnotationISCSIInitiatorIQN = %q, want \"pillar-csi.bhyoo.com/iscsi-initiator-iqn\"",
			AnnotationISCSIInitiatorIQN)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NVMe-oF TCP protocol
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveInitiatorID_NVMeoF_CSINodeNotFound verifies that
// resolveInitiatorID returns FailedPrecondition when the CSINode does not
// exist for an NVMe-oF TCP request (node plugin not yet registered).
func TestResolveInitiatorID_NVMeoF_CSINodeNotFound(t *testing.T) {
	t.Parallel()

	// No CSINode objects seeded.
	srv := newMinimalControllerServer(t)
	_, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "nvmeof-tcp")
	if err == nil {
		t.Fatal("expected FailedPrecondition error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// TestResolveInitiatorID_NVMeoF_AnnotationMissing verifies that
// resolveInitiatorID returns FailedPrecondition when the CSINode exists but
// the nvmeof-host-nqn annotation is absent.  This is the "annotation write
// race" described in RFC §5.2 — node plugin has not yet written its identity.
func TestResolveInitiatorID_NVMeoF_AnnotationMissing(t *testing.T) {
	t.Parallel()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// Annotations deliberately omitted.
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	_, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "nvmeof-tcp")
	if err == nil {
		t.Fatal("expected FailedPrecondition error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// TestResolveInitiatorID_NVMeoF_EmptyAnnotationValue verifies that an
// empty string annotation value (key present, value "") is treated the same
// as absent — the node plugin must write a non-empty NQN.
func TestResolveInitiatorID_NVMeoF_EmptyAnnotationValue(t *testing.T) {
	t.Parallel()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN: "", // empty value
			},
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	_, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "nvmeof-tcp")
	if err == nil {
		t.Fatal("expected FailedPrecondition error for empty annotation value, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// TestResolveInitiatorID_NVMeoF_Success verifies that resolveInitiatorID
// returns the NQN verbatim from the CSINode annotation when present.
func TestResolveInitiatorID_NVMeoF_Success(t *testing.T) {
	t.Parallel()

	const wantNQN = "nqn.2014-08.org.nvmexpress:uuid:aaaabbbb-cccc-dddd-eeee-ffffgggghhhh"
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN: wantNQN,
			},
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	got, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "nvmeof-tcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantNQN {
		t.Errorf("resolveInitiatorID = %q, want %q", got, wantNQN)
	}
}

// TestResolveInitiatorID_NVMeoF_OnlyNVMeoFAnnotationRead verifies that
// when a CSINode has both NVMe-oF and iSCSI annotations, an NVMe-oF TCP
// request reads only the NVMe-oF annotation.
func TestResolveInitiatorID_NVMeoF_OnlyNVMeoFAnnotationRead(t *testing.T) {
	t.Parallel()

	const wantNQN = "nqn.2014-08.org.nvmexpress:uuid:only-nvmeof"
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-proto-node",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN:     wantNQN,
				AnnotationISCSIInitiatorIQN: "iqn.1993-08.org.debian:01:multi-proto-node",
			},
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	got, err := srv.resolveInitiatorID(context.Background(), "multi-proto-node", "nvmeof-tcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantNQN {
		t.Errorf("resolveInitiatorID = %q, want NVMe-oF NQN %q", got, wantNQN)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// iSCSI protocol
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveInitiatorID_ISCSI_CSINodeNotFound verifies FailedPrecondition
// for the iSCSI protocol when the CSINode is absent.
func TestResolveInitiatorID_ISCSI_CSINodeNotFound(t *testing.T) {
	t.Parallel()

	srv := newMinimalControllerServer(t) // no CSINode seeded
	_, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "iscsi")
	if err == nil {
		t.Fatal("expected FailedPrecondition error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// TestResolveInitiatorID_ISCSI_AnnotationMissing verifies FailedPrecondition
// for iSCSI when the CSINode exists but iscsi-initiator-iqn is absent.
func TestResolveInitiatorID_ISCSI_AnnotationMissing(t *testing.T) {
	t.Parallel()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// iscsi-initiator-iqn deliberately omitted.
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	_, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "iscsi")
	if err == nil {
		t.Fatal("expected FailedPrecondition error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// TestResolveInitiatorID_ISCSI_Success verifies that the IQN is returned
// verbatim from the CSINode annotation.
func TestResolveInitiatorID_ISCSI_Success(t *testing.T) {
	t.Parallel()

	const wantIQN = "iqn.1993-08.org.debian:01:worker-node-1"
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationISCSIInitiatorIQN: wantIQN,
			},
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	got, err := srv.resolveInitiatorID(context.Background(), "worker-node-1", "iscsi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantIQN {
		t.Errorf("resolveInitiatorID = %q, want %q", got, wantIQN)
	}
}

// TestResolveInitiatorID_ISCSI_OnlyISCSIAnnotationRead verifies that when
// a node has both NVMe-oF and iSCSI annotations, an iSCSI request reads only
// the iSCSI annotation.
func TestResolveInitiatorID_ISCSI_OnlyISCSIAnnotationRead(t *testing.T) {
	t.Parallel()

	const wantIQN = "iqn.1993-08.org.debian:01:multi-proto-node"
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-proto-node",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN:     "nqn.2014-08.org.nvmexpress:uuid:multi-proto-node",
				AnnotationISCSIInitiatorIQN: wantIQN,
			},
		},
	}
	srv := newMinimalControllerServer(t, csiNode)
	got, err := srv.resolveInitiatorID(context.Background(), "multi-proto-node", "iscsi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantIQN {
		t.Errorf("resolveInitiatorID = %q, want iSCSI IQN %q", got, wantIQN)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol passthrough (NFS and unknown)
// ─────────────────────────────────────────────────────────────────────────────

// TestResolveInitiatorID_NFS_PassthroughNodeID verifies that for the NFS
// protocol resolveInitiatorID returns the nodeID as-is without reading any
// CSINode annotation.  RFC §5.2: NFS annotation-based resolution is Phase 2.
func TestResolveInitiatorID_NFS_PassthroughNodeID(t *testing.T) {
	t.Parallel()

	// No CSINode seeded — any CSINode lookup would fail with NotFound, proving
	// the function does NOT attempt a CSINode lookup for NFS.
	srv := newMinimalControllerServer(t)
	const nodeID = "worker-node-1"
	got, err := srv.resolveInitiatorID(context.Background(), nodeID, "nfs")
	if err != nil {
		t.Fatalf("NFS passthrough: unexpected error: %v", err)
	}
	if got != nodeID {
		t.Errorf("NFS passthrough: resolveInitiatorID = %q, want nodeID %q", got, nodeID)
	}
}

// TestResolveInitiatorID_SMB_PassthroughNodeID verifies that an "smb" protocol
// string returns nodeID as-is (unknown/unsupported protocols are passthrough).
func TestResolveInitiatorID_SMB_PassthroughNodeID(t *testing.T) {
	t.Parallel()

	srv := newMinimalControllerServer(t)
	const nodeID = "worker-node-42"
	got, err := srv.resolveInitiatorID(context.Background(), nodeID, "smb")
	if err != nil {
		t.Fatalf("SMB passthrough: unexpected error: %v", err)
	}
	if got != nodeID {
		t.Errorf("SMB passthrough: resolveInitiatorID = %q, want nodeID %q", got, nodeID)
	}
}

// TestResolveInitiatorID_EmptyProtocol_PassthroughNodeID verifies that an
// empty protocol string returns nodeID as-is.
func TestResolveInitiatorID_EmptyProtocol_PassthroughNodeID(t *testing.T) {
	t.Parallel()

	srv := newMinimalControllerServer(t)
	const nodeID = "worker-node-99"
	got, err := srv.resolveInitiatorID(context.Background(), nodeID, "")
	if err != nil {
		t.Fatalf("empty protocol passthrough: unexpected error: %v", err)
	}
	if got != nodeID {
		t.Errorf("empty protocol passthrough: resolveInitiatorID = %q, want nodeID %q", got, nodeID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Table-driven matrix
// ─────────────────────────────────────────────────────────────────────────────

// checkResolveResult is a test helper that validates a single resolveInitiatorID
// result against the expected outcome.  Extracting the assertion logic keeps
// TestResolveInitiatorID_TableDriven within the cognitive-complexity budget.
func checkResolveResult(t *testing.T, got string, err error, wantErr bool, wantCode codes.Code, wantResult string) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Fatalf("expected error (code %v), got nil", wantCode)
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("expected gRPC status error, got: %v", err)
		}
		if st.Code() != wantCode {
			t.Errorf("error code = %v, want %v", st.Code(), wantCode)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantResult {
		t.Errorf("resolveInitiatorID = %q, want %q", got, wantResult)
	}
}

// TestResolveInitiatorID_TableDriven exercises the full matrix of
// (protocol, annotation-state) combinations in a single table-driven test.
// This supplements the focused single-case tests above.
func TestResolveInitiatorID_TableDriven(t *testing.T) {
	t.Parallel()

	const (
		testNQN  = "nqn.2014-08.org.nvmexpress:uuid:table-test-uuid"
		testIQN  = "iqn.1993-08.org.debian:01:table-test"
		testNode = "node-table-test"
	)

	csiNodeWith := func(annotations map[string]string) *storagev1.CSINode {
		return &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name:        testNode,
				Annotations: annotations,
			},
		}
	}

	cases := []struct {
		name       string
		protocol   string
		seedObjs   []ctrlclient.Object
		wantErr    bool
		wantCode   codes.Code
		wantResult string
	}{
		// NVMe-oF TCP cases
		{
			name:     "nvmeof: CSINode not found → FailedPrecondition",
			protocol: "nvmeof-tcp",
			wantErr:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "nvmeof: annotation absent → FailedPrecondition",
			protocol: "nvmeof-tcp",
			seedObjs: []ctrlclient.Object{csiNodeWith(nil)},
			wantErr:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "nvmeof: annotation present → NQN returned",
			protocol:   "nvmeof-tcp",
			seedObjs:   []ctrlclient.Object{csiNodeWith(map[string]string{AnnotationNVMeOFHostNQN: testNQN})},
			wantResult: testNQN,
		},
		// iSCSI cases
		{
			name:     "iscsi: CSINode not found → FailedPrecondition",
			protocol: "iscsi",
			wantErr:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:     "iscsi: annotation absent → FailedPrecondition",
			protocol: "iscsi",
			seedObjs: []ctrlclient.Object{csiNodeWith(nil)},
			wantErr:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "iscsi: annotation present → IQN returned",
			protocol:   "iscsi",
			seedObjs:   []ctrlclient.Object{csiNodeWith(map[string]string{AnnotationISCSIInitiatorIQN: testIQN})},
			wantResult: testIQN,
		},
		// Passthrough cases
		{
			name:       "nfs: no CSINode needed → nodeID passthrough",
			protocol:   "nfs",
			wantResult: testNode,
		},
		{
			name:       "smb: no CSINode needed → nodeID passthrough",
			protocol:   "smb",
			wantResult: testNode,
		},
		{
			name:       "empty protocol: nodeID passthrough",
			protocol:   "",
			wantResult: testNode,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newMinimalControllerServer(t, tc.seedObjs...)
			got, err := srv.resolveInitiatorID(context.Background(), testNode, tc.protocol)
			checkResolveResult(t, got, err, tc.wantErr, tc.wantCode, tc.wantResult)
		})
	}
}
