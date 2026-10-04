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

// Tests for protocol-specific node identities: block initiators use CSINode
// annotations, while NFS uses numeric Node InternalIP addresses and matches
// the storage address family before recording or granting a publication.
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

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/testutil/fakeuid"
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
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(objs...).
		Build()

	// dialAgent is nil intentionally — resolveInitiatorID never dials.
	return NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)
}

func nodeWithStatus(name string, nodeStatus corev1.NodeStatus) *corev1.Node {
	node := &corev1.Node{Status: nodeStatus}
	node.Name = name
	return node
}

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
		Name: "worker-node-1",
		Annotations: map[string]string{
			AnnotationNVMeOFHostNQN: "", // empty value
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
		Name: "worker-node-1",
		Annotations: map[string]string{
			AnnotationNVMeOFHostNQN: wantNQN,
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
// when a CSINode carries other annotations too, an NVMe-oF TCP request reads
// only the NVMe-oF annotation.
func TestResolveInitiatorID_NVMeoF_OnlyNVMeoFAnnotationRead(t *testing.T) {
	t.Parallel()

	const wantNQN = "nqn.2014-08.org.nvmexpress:uuid:only-nvmeof"
	csiNode := &storagev1.CSINode{
		Name: "multi-proto-node",
		Annotations: map[string]string{
			AnnotationNVMeOFHostNQN:     wantNQN,
			AnnotationISCSIInitiatorIQN: "iqn.1993-08.org.debian:01:multi-proto-node",
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

// TestResolveInitiatorID_ISCSI_OnlyISCSIAnnotationRead verifies that an iSCSI
// request resolves the initiator IQN from the iSCSI annotation, never the
// NVMe-oF host NQN published on the same CSINode.
func TestResolveInitiatorID_ISCSI_OnlyISCSIAnnotationRead(t *testing.T) {
	t.Parallel()

	const wantIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
	csiNode := &storagev1.CSINode{
		Name: "multi-proto-node",
		Annotations: map[string]string{
			AnnotationNVMeOFHostNQN:     "nqn.2014-08.org.nvmexpress:uuid:only-nvmeof",
			AnnotationISCSIInitiatorIQN: wantIQN,
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

// TestResolveInitiatorID_NFS_InternalIP verifies that NFS uses the node's
// InternalIP, not its name or ExternalIP, without requiring a CSINode.
func TestResolveInitiatorID_NFS_InternalIP(t *testing.T) {
	t.Parallel()

	const wantIP = "10.0.0.12"
	node := nodeWithStatus("worker-node-1", corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeExternalIP, Address: "192.0.2.12"},
		{Type: corev1.NodeInternalIP, Address: wantIP},
	}})
	srv := newMinimalControllerServer(t, node)
	got, err := srv.resolveInitiatorID(context.Background(), node.Name, ProtocolNFS)
	if err != nil {
		t.Fatalf("resolve NFS InternalIP: %v", err)
	}
	if got != wantIP {
		t.Errorf("NFS initiator = %q, want InternalIP %q", got, wantIP)
	}
}

func TestResolveInitiatorID_NFS_InvalidNodeAddress(t *testing.T) {
	t.Parallel()

	const nodeID = "worker-node-1"
	for _, tc := range []struct {
		name     string
		node     *corev1.Node
		publish  bool
		wantCode codes.Code
	}{
		{name: "missing Node", wantCode: codes.FailedPrecondition},
		{name: "missing Node at publish", publish: true, wantCode: codes.NotFound},
		{
			name: "no InternalIP",
			node: nodeWithStatus(nodeID, corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "192.0.2.12"},
			}}),
			publish:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name: "nonnumeric InternalIP",
			node: nodeWithStatus(nodeID, corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "worker-node-1.example.test"},
			}}),
			wantCode: codes.FailedPrecondition,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var objs []ctrlclient.Object
			if tc.node != nil {
				objs = append(objs, tc.node)
			}
			srv := newMinimalControllerServer(t, objs...)
			var err error
			if tc.publish {
				_, err = srv.resolvePublishInitiator(context.Background(), nodeID, ProtocolNFS)
			} else {
				_, err = srv.resolveInitiatorID(context.Background(), nodeID, ProtocolNFS)
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("error = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestResolvePublishGrant_NFS_AddressFamily(t *testing.T) {
	t.Parallel()

	const (
		nodeID = "dual-stack-worker"
		v4     = "10.0.0.12"
		v6     = "fd00::12"
	)
	for _, tc := range []struct {
		name        string
		addresses   []corev1.NodeAddress
		bindAddress string
		wantIP      string
		wantCode    codes.Code
	}{
		{
			name: "IPv4 storage selects second address",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: v6},
				{Type: corev1.NodeInternalIP, Address: v4},
			},
			bindAddress: "10.0.0.2",
			wantIP:      v4,
		},
		{
			name: "IPv6 storage selects second address",
			addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: v4},
				{Type: corev1.NodeInternalIP, Address: v6},
			},
			bindAddress: "fd00::2",
			wantIP:      v6,
		},
		{
			name:        "IPv4-only node cannot use IPv6 storage",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: v4}},
			bindAddress: "fd00::2",
			wantCode:    codes.InvalidArgument,
		},
		{
			name:        "IPv6-only node cannot use IPv4 storage",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: v6}},
			bindAddress: "10.0.0.2",
			wantCode:    codes.InvalidArgument,
		},
		{
			name:        "storage address must be numeric",
			addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: v4}},
			bindAddress: "storage.example.test",
			wantCode:    codes.FailedPrecondition,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := nodeWithStatus(nodeID, corev1.NodeStatus{Addresses: tc.addresses})
			srv := newMinimalControllerServer(t, node)
			pvs := &v1alpha1.PillarVolumeState{
				Spec: v1alpha1.PillarVolumeStateSpec{ProtocolType: ProtocolNFS},
				Status: v1alpha1.PillarVolumeStateStatus{
					ExportSpec: &v1alpha1.VolumeExportSpec{NFS: &v1alpha1.NFSExportSpec{}},
				},
			}
			got, _, err := srv.resolvePublishGrant(
				context.Background(), false, nodeID, ProtocolNFS, tc.bindAddress, pvs)
			checkResolveResult(t, got, err, tc.wantCode != codes.OK, tc.wantCode, tc.wantIP)
		})
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
		testIQN  = "iqn.2026-01.com.bhyoo.pillar-csi:node.table-test"
		testNode = "node-table-test"
	)

	csiNodeWith := func(annotations map[string]string) *storagev1.CSINode {
		return &storagev1.CSINode{
			Name:        testNode,
			Annotations: annotations,
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
			name:     "iscsi: only NVMe-oF annotation → FailedPrecondition",
			protocol: "iscsi",
			seedObjs: []ctrlclient.Object{csiNodeWith(map[string]string{AnnotationNVMeOFHostNQN: testNQN})},
			wantErr:  true,
			wantCode: codes.FailedPrecondition,
		},
		{
			name:       "iscsi: annotation present → IQN returned",
			protocol:   "iscsi",
			seedObjs:   []ctrlclient.Object{csiNodeWith(map[string]string{AnnotationISCSIInitiatorIQN: testIQN})},
			wantResult: testIQN,
		},
		// NFS uses a numeric Node InternalIP without any CSINode.
		{
			name:     "nfs: Node InternalIP without CSINode",
			protocol: ProtocolNFS,
			seedObjs: []ctrlclient.Object{nodeWithStatus(testNode, corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.13"},
			}})},
			wantResult: "10.0.0.13",
		},
		// Unsupported protocols remain passthrough for agent rejection.
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
