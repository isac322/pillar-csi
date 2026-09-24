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

// Extended component tests for the CSI Controller Service (internal/csi.ControllerServer).
//
// This file covers sections 4.7 – 4.9 of TESTCASES.md:
//
//	4.7 Input Validation Edge Cases
//	4.8 PillarAgent State Errors
//	4.9 Partial Failure Recovery and Agent Response Handling
//
// Mock fidelity: same as csi_controller_test.go — csiMockAgent with function
// fields; no network I/O; fake k8s client for PillarAgent/PillarVolumeState CRDs.
package component_test

import (
	"context"
	"io"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
	"github.com/bhyoo/pillar-csi/internal/testutil/fakeuid"
)

// ─────────────────────────────────────────────────────────────────────────────
// Additional test environment helpers
// ─────────────────────────────────────────────────────────────────────────────.

// newCSIControllerTestEnvNoResolvedAddr creates a ControllerServer backed by a
// PillarAgent with an empty ResolvedAddress — simulating a target whose agent
// pod has not yet registered a reachable address.
func newCSIControllerTestEnvNoResolvedAddr(t *testing.T) *csiControllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status:     v1alpha1.PillarAgentStatus{ResolvedAddress: ""}, // no address
	}

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
		Build()

	agent := &csiMockAgent{}
	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, csiNopCloser{}, nil
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &csiControllerTestEnv{srv: srv, agent: agent, k8sClient: fakeClient}
}

// newCSIControllerTestEnvNoTarget creates a ControllerServer backed by a fake
// k8s client with no PillarAgent objects — simulating a decommissioned or
// misconfigured storage node.
func newCSIControllerTestEnvNoTarget(t *testing.T) *csiControllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
		Build()

	agent := &csiMockAgent{}
	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, csiNopCloser{}, nil
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &csiControllerTestEnv{srv: srv, agent: agent, k8sClient: fakeClient}
}

// baseControllerPublishRequest returns a valid ControllerPublishVolumeRequest.
func baseControllerPublishRequest() *csipb.ControllerPublishVolumeRequest {
	return &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "nqn.2026-01.com.bhyoo:host-test",
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
}

// requireGRPCCode asserts that err is a gRPC error with the given code.
func requireGRPCCode(t *testing.T, err error, wantCode codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected gRPC error with code %v, got nil", wantCode)
	}
	st, _ := status.FromError(err)
	if st.Code() != wantCode {
		t.Errorf("error code = %v, want %v (msg: %q)", st.Code(), wantCode, st.Message())
	}
}

// requireNonOKGRPC asserts that err is a non-nil gRPC error with a non-OK code.
func requireNonOKGRPC(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected non-OK gRPC error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("error code = OK, want non-OK")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 4.7 — Input Validation Edge Cases
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerPublishVolume_EmptyVolumeID verifies that an
// empty VolumeID on ControllerPublishVolume returns InvalidArgument before
// any agent call is made.
//
//	Setup:   VolumeID="" in request; valid NodeID and capability
//	Expect:  Returns gRPC InvalidArgument; no agent AllowInitiator call
func TestCSIController_ControllerPublishVolume_EmptyVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseControllerPublishRequest()
	req.VolumeId = ""

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	requireGRPCCode(t, err, codes.InvalidArgument)
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("agent.AllowInitiator called %d times, want 0", env.agent.allowInitiatorCalls)
	}
}

// TestCSIController_ControllerPublishVolume_EmptyNodeID verifies that an empty
// NodeID on ControllerPublishVolume returns InvalidArgument.
//
//	Setup:   Valid VolumeID; NodeID=""
//	Expect:  Returns gRPC InvalidArgument; no agent call
func TestCSIController_ControllerPublishVolume_EmptyNodeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseControllerPublishRequest()
	req.NodeId = ""

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	requireGRPCCode(t, err, codes.InvalidArgument)
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("agent.AllowInitiator called %d times, want 0", env.agent.allowInitiatorCalls)
	}
}

// TestCSIController_ControllerPublishVolume_NilVolumeCapability verifies that
// a nil volume_capability on ControllerPublishVolume returns InvalidArgument.
//
//	Setup:   Valid VolumeID and NodeID; VolumeCapability=nil
//	Expect:  Returns gRPC InvalidArgument
func TestCSIController_ControllerPublishVolume_NilVolumeCapability(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseControllerPublishRequest()
	req.VolumeCapability = nil

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestCSIController_ControllerPublishVolume_MalformedVolumeID verifies that a
// malformed volumeID (no slashes, wrong number of parts) returns NotFound.
//
// Per CSI spec §4.5: ControllerPublishVolume MUST return NotFound when the
// volume_id is unknown.  A volume_id that does not parse into this driver's
// "<target>/<protocol>/<backend>/<vol-id>" format cannot identify any
// volume the driver provisioned, so it is treated as unknown rather than
// bad input.
//
//	Setup:   VolumeID="badformat" (no slashes); valid NodeID and capability
//	Expect:  Returns gRPC NotFound
func TestCSIController_ControllerPublishVolume_MalformedVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseControllerPublishRequest()
	req.VolumeId = "badformat"

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	requireGRPCCode(t, err, codes.NotFound)
}

// TestCSIController_ControllerUnpublishVolume_EmptyVolumeID verifies that an
// empty VolumeID on ControllerUnpublishVolume returns InvalidArgument.
//
//	Setup:   VolumeID="" in request
//	Expect:  Returns gRPC InvalidArgument
func TestCSIController_ControllerUnpublishVolume_EmptyVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: "",
		NodeId:   "nqn.test:node",
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestCSIController_ControllerUnpublishVolume_EmptyNodeID verifies that an
// empty NodeID on ControllerUnpublishVolume unpublishes the volume from every
// node (CSI spec: "If the node_id is not specified, the SP MUST unpublish the
// volume from all nodes it is published to"): DenyInitiator is called for
// each recorded publication and the records are removed.
//
//	Setup:   Volume published to one node; unpublish with NodeID=""
//	Expect:  Returns empty ControllerUnpublishVolumeResponse; DenyInitiator
//	         called once with the recorded initiator; publishedNodes emptied
func TestCSIController_ControllerUnpublishVolume_EmptyNodeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const nodeID = "nqn.test:node-empty-unpublish"
	seedComponentPillarVolumeState(t, env, "pvc-component-test")
	seedCSINodeForNVMeOF(ctx, t, env.k8sClient, nodeID, nodeID)
	publishComponentTestVolume(ctx, t, env, nodeID)

	var deniedInitiators []string
	env.agent.denyInitiatorFn = func(
		_ context.Context, req *agentv1.DenyInitiatorRequest,
	) (*agentv1.DenyInitiatorResponse, error) {
		deniedInitiators = append(deniedInitiators, req.GetInitiatorId())
		return &agentv1.DenyInitiatorResponse{}, nil
	}

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "",
	})
	if err != nil {
		t.Fatalf("expected success for empty NodeID, got: %v", err)
	}
	if len(deniedInitiators) != 1 || deniedInitiators[0] != nodeID {
		t.Errorf("DenyInitiator initiators = %v, want [%q] (every recorded publication revoked)", deniedInitiators, nodeID)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.k8sClient.Get(ctx, types.NamespacedName{Name: "pvc-component-test"}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	if len(pvs.Status.PublishedNodes) != 0 {
		t.Errorf("status.publishedNodes = %+v, want empty", pvs.Status.PublishedNodes)
	}
}

// TestCSIController_ControllerUnpublishVolume_MalformedVolumeID verifies that
// a malformed volumeID on ControllerUnpublishVolume returns success — the
// controller treats unknown IDs as already-removed volumes.
//
//	Setup:   VolumeID="badformat"
//	Expect:  Returns empty response; no agent call
func TestCSIController_ControllerUnpublishVolume_MalformedVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: "badformat",
		NodeId:   "nqn.test:node",
	})
	if err != nil {
		t.Fatalf("expected success for malformed VolumeID, got: %v", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("agent.DenyInitiator called %d times, want 0", env.agent.denyInitiatorCalls)
	}
}

// TestCSIController_ExpandVolume_NilCapacityRange verifies that a nil
// capacity_range on ControllerExpandVolume returns InvalidArgument.
//
//	Setup:   Valid VolumeID; CapacityRange=nil
//	Expect:  Returns gRPC InvalidArgument
func TestCSIController_ExpandVolume_NilCapacityRange(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: nil,
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestCSIController_ExpandVolume_NegativeRequiredBytes verifies that a
// negative required_bytes on ControllerExpandVolume returns InvalidArgument.
//
//	Setup:   CapacityRange.RequiredBytes=-1
//	Expect:  Returns gRPC InvalidArgument
func TestCSIController_ExpandVolume_NegativeRequiredBytes(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: -1},
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// TestCSIController_ExpandVolume_MalformedVolumeID verifies that a malformed
// volumeID on ControllerExpandVolume returns InvalidArgument.
//
//	Setup:   VolumeID="bad-format-no-slashes"; valid capacity range
//	Expect:  Returns gRPC InvalidArgument
func TestCSIController_ExpandVolume_MalformedVolumeID(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      "bad-format-no-slashes",
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	requireGRPCCode(t, err, codes.InvalidArgument)
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 4.8 — PillarAgent State Errors
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_TargetNoResolvedAddress verifies that a
// PillarAgent with an empty ResolvedAddress causes CreateVolume to return
// Unavailable (the agent pod has not yet registered its address).
//
//	Setup:   PillarAgent seeded with empty Status.ResolvedAddress
//	Expect:  Returns gRPC Unavailable
func TestCSIController_CreateVolume_TargetNoResolvedAddress(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoResolvedAddr(t)
	ctx := context.Background()

	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	requireGRPCCode(t, err, codes.Unavailable)
}

// TestCSIController_DeleteVolume_TargetNotFound verifies that if the
// PillarAgent object cannot be found during DeleteVolume of a provisioned
// volume, the controller fails closed: a missing PillarAgent object does not
// prove the storage node's backend and target are gone, so the durable record
// is kept (marked deleting) and no agent is contacted.
//
//	Setup:   PillarVolumeState exists; VolumeID encodes a target name not
//	         present in the k8s store
//	Expect:  Returns gRPC FailedPrecondition; record kept and marked deleting
func TestCSIController_DeleteVolume_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoTarget(t)
	ctx := context.Background()
	seedComponentPillarVolumeState(t, env, "pvc-test")

	// VolumeID encodes "nonexistent-node" which has no PillarAgent.
	volumeID := "nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test"
	_, err := env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: volumeID})
	requireGRPCCode(t, err, codes.FailedPrecondition)
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Errorf("agent calls unexport=%d delete=%d, want none",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	pvs := &v1alpha1.PillarVolumeState{}
	if getErr := env.k8sClient.Get(ctx, types.NamespacedName{Name: "pvc-test"}, pvs); getErr != nil {
		t.Fatalf("PillarVolumeState: %v, want kept", getErr)
	}
	if !pvs.Status.Deleting {
		t.Error("PillarVolumeState not marked deleting")
	}
}

// TestCSIController_DeleteVolume_TargetNoResolvedAddress verifies that a
// PillarAgent with an empty ResolvedAddress causes DeleteVolume of a
// provisioned volume to return Unavailable (transient state; CO should retry).
//
//	Setup:   PillarVolumeState exists; PillarAgent with empty ResolvedAddress
//	Expect:  Returns gRPC Unavailable
func TestCSIController_DeleteVolume_TargetNoResolvedAddress(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoResolvedAddr(t)
	ctx := context.Background()
	seedComponentPillarVolumeState(t, env, "pvc-component-test")

	_, err := env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: expectedCSIVolumeID,
	})
	requireGRPCCode(t, err, codes.Unavailable)
}

// TestCSIController_ControllerPublishVolume_TargetNotFound verifies that a
// missing PillarAgent on ControllerPublishVolume returns NotFound.
//
//	Setup:   PillarVolumeState exists; VolumeID encodes a target name not in
//	         the k8s store
//	Expect:  Returns gRPC NotFound
func TestCSIController_ControllerPublishVolume_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoTarget(t)
	ctx := context.Background()
	// Seed the volume so the PillarAgent lookup (not the volume lookup) fails.
	seedComponentPillarVolumeState(t, env, "pvc-test")

	req := baseControllerPublishRequest()
	req.VolumeId = "nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test"

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	requireGRPCCode(t, err, codes.NotFound)
}

// TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress verifies
// that a PillarAgent with empty ResolvedAddress on ControllerPublishVolume
// returns Unavailable.
//
//	Setup:   PillarVolumeState exists; PillarAgent with empty ResolvedAddress;
//	         valid VolumeID and NodeID
//	Expect:  Returns gRPC Unavailable
func TestCSIController_ControllerPublishVolume_TargetNoResolvedAddress(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoResolvedAddr(t)
	ctx := context.Background()
	// Seed the volume so the publish passes the existence check and reaches
	// the PillarAgent address resolution.
	seedComponentPillarVolumeState(t, env, "pvc-component-test")
	_, err := env.srv.ControllerPublishVolume(ctx, baseControllerPublishRequest())
	requireGRPCCode(t, err, codes.Unavailable)
}

// TestCSIController_ExpandVolume_TargetNotFound verifies that a missing
// PillarAgent on ControllerExpandVolume returns NotFound.
//
//	Setup:   VolumeID encoding non-existent target
//	Expect:  Returns gRPC NotFound
func TestCSIController_ExpandVolume_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoTarget(t)
	ctx := context.Background()

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      "nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	requireGRPCCode(t, err, codes.NotFound)
}

// TestCSIController_ExpandVolume_TargetNoResolvedAddress verifies that a
// PillarAgent with empty ResolvedAddress on ControllerExpandVolume returns
// Unavailable.
//
//	Setup:   PillarAgent with empty ResolvedAddress; valid VolumeID
//	Expect:  Returns gRPC Unavailable
func TestCSIController_ExpandVolume_TargetNoResolvedAddress(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnvNoResolvedAddr(t)
	ctx := context.Background()

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	requireGRPCCode(t, err, codes.Unavailable)
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 4.9 — Partial Failure Recovery and Agent Response Handling
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_ExportFails_RecordsCreatePartial verifies
// that when the backend CreateVolume succeeds but ExportVolume fails, the
// controller returns an error AND persists the PillarVolumeState CRD in
// CreatePartial phase for subsequent retry recovery.
//
//	Setup:   Mock agent: CreateVolume→OK; ExportVolume→gRPC Internal
//	Expect:  Returns gRPC Internal; PillarVolumeState CRD found in k8s with
//	         CreatePartial phase
func TestCSIController_CreateVolume_ExportFails_RecordsCreatePartial(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status:     v1alpha1.PillarAgentStatus{ResolvedAddress: "192.168.1.10:9500"},
	}
	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
		Build()

	agent := &csiMockAgent{
		exportVolumeFn: func(_ context.Context, _ *agentv1.ExportVolumeRequest) (*agentv1.ExportVolumeResponse, error) {
			return nil, status.Error(codes.Internal, "export failed: simulated error")
		},
	}
	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, csiNopCloser{}, nil
	})
	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	_, err := srv.CreateVolume(context.Background(), baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error when ExportVolume fails, got nil")
	}
	requireNonOKGRPC(t, err)

	// Verify the PillarVolumeState CRD was persisted in the k8s store.
	pv := &v1alpha1.PillarVolumeState{}
	if getErr := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "pvc-component-test"}, pv); getErr != nil {
		t.Fatalf("PillarVolumeState not found in k8s store: %v (expected CreatePartial CRD to be persisted)", getErr)
	}
	// The CRD must reflect the CreatePartial phase.
	if pv.Status.Phase != v1alpha1.PillarVolumeStatePhaseCreatePartial {
		t.Errorf("PillarVolumeState.Status.Phase = %q, want %q",
			pv.Status.Phase, v1alpha1.PillarVolumeStatePhaseCreatePartial)
	}
}

// TestCSIController_ExpandVolume_AgentReturnsZeroBytes verifies that when the
// agent's ExpandVolume RPC returns CapacityBytes=0, the controller falls back
// to using the requested bytes in the response.
//
//	Setup:   Mock agent: ExpandVolume→{CapacityBytes:0}; request required_bytes=20 GiB
//	Expect:  Returns ControllerExpandVolumeResponse.CapacityBytes=20 GiB
func TestCSIController_ExpandVolume_AgentReturnsZeroBytes(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()
	seedComponentPillarVolumeState(t, env, "pvc-component-test")

	const wantBytes = int64(20 << 30) // 20 GiB

	env.agent.expandVolumeFn = func(
		_ context.Context, _ *agentv1.ExpandVolumeRequest,
	) (*agentv1.ExpandVolumeResponse, error) {
		// Return zero CapacityBytes to trigger the fallback.
		return &agentv1.ExpandVolumeResponse{CapacityBytes: 0}, nil
	}

	resp, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: wantBytes},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: unexpected error: %v", err)
	}
	if got := resp.GetCapacityBytes(); got != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d (fallback to requested_bytes)", got, wantBytes)
	}
}

// TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound verifies that
// when DenyInitiator returns an error other than NotFound, the error is
// propagated to the caller (not silently swallowed) and the publication
// record is kept so the CO retry revokes it again.
//
//	Setup:   Volume published to the node; Mock agent: DenyInitiator→gRPC Internal
//	Expect:  Returns gRPC Internal; publication record still present
func TestCSIErrors_ControllerUnpublish_DenyInitiatorNonNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const nodeID = "nqn.test:node-deny-fail"
	// Unpublish only revokes recorded publications: seed the volume and CSINode,
	// then publish so DenyInitiator is reached.
	seedComponentPillarVolumeState(t, env, "pvc-component-test")
	seedCSINodeForNVMeOF(ctx, t, env.k8sClient, nodeID, nodeID)
	publishComponentTestVolume(ctx, t, env, nodeID)

	env.agent.denyInitiatorFn = func(
		_ context.Context, _ *agentv1.DenyInitiatorRequest,
	) (*agentv1.DenyInitiatorResponse, error) {
		return nil, status.Error(codes.Internal, "deny initiator failed: internal error")
	}

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeID,
	})
	requireGRPCCode(t, err, codes.Internal)
	if env.agent.denyInitiatorCalls != 1 {
		t.Errorf("agent.DenyInitiator calls = %d, want 1", env.agent.denyInitiatorCalls)
	}

	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.k8sClient.Get(ctx, types.NamespacedName{Name: "pvc-component-test"}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	if len(pvs.Status.PublishedNodes) != 1 || pvs.Status.PublishedNodes[0].NodeID != nodeID {
		t.Errorf("status.publishedNodes = %+v, want record for %q kept after failed revoke",
			pvs.Status.PublishedNodes, nodeID)
	}
}
