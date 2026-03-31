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

// Tests for the ControllerServer.CreateVolume idempotency behavior.
//
// These tests verify that:
//  1. A fresh CreateVolume call provisions the backend AND exports it.
//  2. A retry when the volume is already in StateCreated (phase=Ready in CRD)
//     returns the cached response without calling the agent at all.
//  3. A retry when the volume is in StateCreatePartial (backend created but
//     ExportVolume previously failed) skips agent.CreateVolume and calls only
//     agent.ExportVolume, preserving the existing zvol.
//
// All tests run without a real Kubernetes cluster or NVMe-oF kernel module.
// A controller-runtime fake client supplies Kubernetes API behavior, and a
// mock AgentServiceClient supplies agent RPC behavior.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestCreateVolume

import (
	"context"
	"errors"
	"io"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const testModeThin = "thin"

// ─────────────────────────────────────────────────────────────────────────────
// Mock AgentServiceClient
// ─────────────────────────────────────────────────────────────────────────────.

// mockAgentClient is a test double for agentv1.AgentServiceClient.
// It records calls to CreateVolume and ExportVolume, and returns pre-configured
// responses or errors.
type mockAgentClient struct {
	// Responses for CreateVolume (Step 1).
	createVolumeResp *agentv1.CreateVolumeResponse
	createVolumeErr  error

	// Responses for ExportVolume (Step 2).
	exportVolumeResp *agentv1.ExportVolumeResponse
	exportVolumeErr  error

	// Responses for UnexportVolume (DeleteVolume Step 1).
	unexportVolumeErr error

	// Responses for DeleteVolume (DeleteVolume Step 2).
	deleteVolumeErr error

	// Responses for GetCapacity.
	getCapacityResp *agentv1.GetCapacityResponse
	getCapacityErr  error

	// Responses for ExpandVolume.
	expandVolumeResp *agentv1.ExpandVolumeResponse
	expandVolumeErr  error

	// Responses for AllowInitiator / DenyInitiator.
	allowInitiatorErr  error
	denyInitiatorErr   error
	lastAllowInitiator *agentv1.AllowInitiatorRequest
	lastDenyInitiator  *agentv1.DenyInitiatorRequest

	// Call counters — verified by tests.
	createVolumeCalls   int
	exportVolumeCalls   int
	unexportVolumeCalls int
	deleteVolumeCalls   int
	getCapacityCalls    int
	expandVolumeCalls   int
	allowInitiatorCalls int
	denyInitiatorCalls  int

	// lastCreateVolumeReq captures the most recent CreateVolume request for
	// assertion on backend/export params in annotation integration tests.
	lastCreateVolumeReq *agentv1.CreateVolumeRequest
}

// Compile-time check that mockAgentClient implements the full interface.
var _ agentv1.AgentServiceClient = (*mockAgentClient)(nil)

func (m *mockAgentClient) CreateVolume(
	_ context.Context,
	req *agentv1.CreateVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.CreateVolumeResponse, error) {
	m.createVolumeCalls++
	m.lastCreateVolumeReq = req
	if m.createVolumeErr != nil {
		return nil, m.createVolumeErr
	}
	if m.createVolumeResp != nil {
		return m.createVolumeResp, nil
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/zvol/tank/pvc-test",
		CapacityBytes: 1073741824, // 1 GiB
	}, nil
}

func (m *mockAgentClient) ExportVolume(
	_ context.Context,
	_ *agentv1.ExportVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	m.exportVolumeCalls++
	if m.exportVolumeErr != nil {
		return nil, m.exportVolumeErr
	}
	if m.exportVolumeResp != nil {
		return m.exportVolumeResp, nil
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.example:pvc-test",
			Address:   "192.168.1.10",
			Port:      4420,
			VolumeRef: "pvc-test",
		},
	}, nil
}

func (m *mockAgentClient) UnexportVolume(
	_ context.Context,
	_ *agentv1.UnexportVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.UnexportVolumeResponse, error) {
	m.unexportVolumeCalls++
	if m.unexportVolumeErr != nil {
		return nil, m.unexportVolumeErr
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

func (m *mockAgentClient) DeleteVolume(
	_ context.Context,
	_ *agentv1.DeleteVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.DeleteVolumeResponse, error) {
	m.deleteVolumeCalls++
	if m.deleteVolumeErr != nil {
		return nil, m.deleteVolumeErr
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

// Stubbed methods — not used by CreateVolume or DeleteVolume.
func (*mockAgentClient) GetCapabilities(
	_ context.Context,
	_ *agentv1.GetCapabilitiesRequest,
	_ ...grpc.CallOption,
) (*agentv1.GetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) GetCapacity(
	_ context.Context,
	_ *agentv1.GetCapacityRequest,
	_ ...grpc.CallOption,
) (*agentv1.GetCapacityResponse, error) {
	m.getCapacityCalls++
	if m.getCapacityErr != nil {
		return nil, m.getCapacityErr
	}
	if m.getCapacityResp != nil {
		return m.getCapacityResp, nil
	}
	return &agentv1.GetCapacityResponse{
		TotalBytes:     100 << 30, // 100 GiB
		AvailableBytes: 60 << 30,  // 60 GiB
		UsedBytes:      40 << 30,  // 40 GiB
	}, nil
}
func (*mockAgentClient) ListVolumes(
	_ context.Context,
	_ *agentv1.ListVolumesRequest,
	_ ...grpc.CallOption,
) (*agentv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (*mockAgentClient) ListExports(
	_ context.Context,
	_ *agentv1.ListExportsRequest,
	_ ...grpc.CallOption,
) (*agentv1.ListExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (*mockAgentClient) HealthCheck(
	_ context.Context,
	_ *agentv1.HealthCheckRequest,
	_ ...grpc.CallOption,
) (*agentv1.HealthCheckResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) ExpandVolume(
	_ context.Context,
	_ *agentv1.ExpandVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.ExpandVolumeResponse, error) {
	m.expandVolumeCalls++
	if m.expandVolumeErr != nil {
		return nil, m.expandVolumeErr
	}
	if m.expandVolumeResp != nil {
		return m.expandVolumeResp, nil
	}
	return &agentv1.ExpandVolumeResponse{
		CapacityBytes: 2147483648, // 2 GiB default
	}, nil
}
func (m *mockAgentClient) AllowInitiator(
	_ context.Context,
	req *agentv1.AllowInitiatorRequest,
	_ ...grpc.CallOption,
) (*agentv1.AllowInitiatorResponse, error) {
	m.allowInitiatorCalls++
	m.lastAllowInitiator = req
	if m.allowInitiatorErr != nil {
		return nil, m.allowInitiatorErr
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}
func (m *mockAgentClient) DenyInitiator(
	_ context.Context,
	req *agentv1.DenyInitiatorRequest,
	_ ...grpc.CallOption,
) (*agentv1.DenyInitiatorResponse, error) {
	m.denyInitiatorCalls++
	m.lastDenyInitiator = req
	if m.denyInitiatorErr != nil {
		return nil, m.denyInitiatorErr
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}
func (*mockAgentClient) SendVolume(
	_ context.Context,
	_ *agentv1.SendVolumeRequest,
	_ ...grpc.CallOption,
) (grpc.ServerStreamingClient[agentv1.SendVolumeChunk], error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (*mockAgentClient) ReceiveVolume(
	_ context.Context,
	_ ...grpc.CallOption,
) (grpc.ClientStreamingClient[agentv1.ReceiveVolumeChunk, agentv1.ReceiveVolumeResponse], error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (*mockAgentClient) ReconcileState(
	_ context.Context,
	_ *agentv1.ReconcileStateRequest,
	_ ...grpc.CallOption,
) (*agentv1.ReconcileStateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────.

// nopCloser satisfies io.Closer with a no-op.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// controllerTestEnv holds everything needed for a ControllerServer unit test.
type controllerTestEnv struct {
	srv    *ControllerServer
	agent  *mockAgentClient
	scheme *runtime.Scheme
}

// newControllerTestEnv builds a ControllerServer backed by:
//   - a controller-runtime fake k8s client seeded with one PillarTarget
//     that reports ResolvedAddress = "192.168.1.10:9500"
//   - a mockAgentClient injected via the AgentDialer
func newControllerTestEnv(t *testing.T) *controllerTestEnv {
	t.Helper()

	// Build the scheme with the v1alpha1 types and core/v1 PVC types registered.
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("storagev1.AddToScheme: %v", err)
	}

	// Seed the fake client with a ready PillarTarget.
	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "storage-node-1",
		},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{
				Address: "192.168.1.10",
				Port:    9500,
			},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolume{}, &v1alpha1.PillarTarget{}).
		Build()

	agent := &mockAgentClient{}

	dialer := func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, nopCloser{}, nil
	}

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	return &controllerTestEnv{
		srv:    srv,
		agent:  agent,
		scheme: scheme,
	}
}

// baseCreateVolumeRequest returns a minimal valid CreateVolumeRequest.
func baseCreateVolumeRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: "pvc-abc123",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824, // 1 GiB
		},
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":        "storage-node-1",
			"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
			"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
			"pillar-csi.bhyoo.com/pool":          "tank",
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestCreateVolume_FirstCall verifies the normal (no prior state) path:
// agent.CreateVolume and agent.ExportVolume are each called exactly once,
// and the returned VolumeId encodes the routing metadata.
func TestCreateVolume_FirstCall(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	resp, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}

	// Both agent RPCs must have been called exactly once.
	if got := env.agent.createVolumeCalls; got != 1 {
		t.Errorf("agent.CreateVolume call count = %d, want 1", got)
	}
	if got := env.agent.exportVolumeCalls; got != 1 {
		t.Errorf("agent.ExportVolume call count = %d, want 1", got)
	}

	// VolumeId must encode target / protocol / backend / agentVolID.
	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("response Volume is nil")
	}
	const wantID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123"
	if vol.GetVolumeId() != wantID {
		t.Errorf("VolumeId = %q, want %q", vol.GetVolumeId(), wantID)
	}

	// VolumeContext must carry connection parameters.
	vc := vol.GetVolumeContext()
	if vc[VolumeContextKeyTargetID] == "" {
		t.Errorf("VolumeContext[%q] is empty", VolumeContextKeyTargetID)
	}
	if vc[VolumeContextKeyAddress] == "" {
		t.Errorf("VolumeContext[%q] is empty", VolumeContextKeyAddress)
	}
}

// TestCreateVolume_IdempotentWhenAlreadyCreated verifies the CSI §5.1.1
// idempotency requirement: a second CreateVolume call for a volume that is
// already in the Ready phase (StateCreated) must return the cached response
// without calling the agent.
func TestCreateVolume_IdempotentWhenAlreadyCreated(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	// First call — full provisioning path.
	resp1, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("first CreateVolume error: %v", err)
	}
	calls1Create := env.agent.createVolumeCalls
	calls1Export := env.agent.exportVolumeCalls

	// Second call — must be a no-op (cached response).
	resp2, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("second CreateVolume error: %v", err)
	}

	// No additional agent calls should have been made.
	if env.agent.createVolumeCalls != calls1Create {
		t.Errorf("agent.CreateVolume called again on retry (total %d, after first %d)",
			env.agent.createVolumeCalls, calls1Create)
	}
	if env.agent.exportVolumeCalls != calls1Export {
		t.Errorf("agent.ExportVolume called again on retry (total %d, after first %d)",
			env.agent.exportVolumeCalls, calls1Export)
	}

	// Both responses must carry the same VolumeId.
	if got, want := resp2.GetVolume().GetVolumeId(), resp1.GetVolume().GetVolumeId(); got != want {
		t.Errorf("second response VolumeId = %q, want %q", got, want)
	}
}

// TestCreateVolume_SkipsBackendOnCreatePartialRetry is the core idempotency
// test for Sub-AC 4b.
//
// Scenario:
//  1. First call: agent.CreateVolume succeeds → agent.ExportVolume fails.
//     The controller persists StateCreatePartial (with devicePath) and returns
//     an error to the CO.
//  2. Second call (retry): the controller detects StateCreatePartial from the
//     persisted CRD, skips agent.CreateVolume entirely, and calls only
//     agent.ExportVolume.  This succeeds and the volume reaches StateCreated.
//
// The test verifies that agent.CreateVolume is called exactly once (during
// the first attempt), not twice.
func TestCreateVolume_SkipsBackendOnCreatePartialRetry(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	// ── First attempt: ExportVolume fails ────────────────────────────────────
	exportErr := status.Error(codes.Internal, "simulated export failure")
	env.agent.exportVolumeErr = exportErr

	_, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err == nil {
		t.Fatal("expected first CreateVolume to fail (ExportVolume error), got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("first CreateVolume error code = %v, want %v", st.Code(), codes.Internal)
	}

	// Verify the state machine advanced to CreatePartial.
	volumeID := "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123"
	if got := env.srv.sm.GetState(volumeID); got != StateCreatePartial {
		t.Errorf("state after first failed attempt = %v, want %v", got, StateCreatePartial)
	}

	// Record how many times CreateVolume was called in the first attempt.
	createCallsAfterFirst := env.agent.createVolumeCalls
	if createCallsAfterFirst != 1 {
		t.Errorf("agent.CreateVolume call count after first attempt = %d, want 1", createCallsAfterFirst)
	}

	// ── Second attempt (retry): ExportVolume now succeeds ────────────────────
	env.agent.exportVolumeErr = nil // clear the error

	resp, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("second CreateVolume (retry) unexpected error: %v", err)
	}

	// ── Key assertion: agent.CreateVolume must NOT have been called again ────
	if env.agent.createVolumeCalls != createCallsAfterFirst {
		t.Errorf("agent.CreateVolume called again on CreatePartial retry: "+
			"total calls = %d, after first attempt = %d (expected no new calls)",
			env.agent.createVolumeCalls, createCallsAfterFirst)
	}

	// ExportVolume must have been called once more (for the retry).
	if env.agent.exportVolumeCalls != 2 {
		t.Errorf("agent.ExportVolume total calls = %d, want 2 (one per attempt)", env.agent.exportVolumeCalls)
	}

	// The retry must return a valid volume.
	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("retry response Volume is nil")
	}
	const wantID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123"
	if vol.GetVolumeId() != wantID {
		t.Errorf("retry VolumeId = %q, want %q", vol.GetVolumeId(), wantID)
	}

	// State machine must now be in StateCreated.
	if got := env.srv.sm.GetState(volumeID); got != StateCreated {
		t.Errorf("state after successful retry = %v, want %v", got, StateCreated)
	}
}

// TestCreateVolume_CreatePartialRetry_DevicePathPreserved verifies that the
// device path stored in the PillarVolume CRD during a CreatePartial transition
// is the path passed to agent.ExportVolume on a retry, not a zero value.
//
// This ensures no silent data loss: the retry exports the same physical block
// device, not an empty device path that could cause the agent to reject the
// export.
func TestCreateVolume_CreatePartialRetry_DevicePathPreserved(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	const wantDevicePath = "/dev/zvol/tank/pvc-abc123"

	// Configure mock to return a specific device path.
	env.agent.createVolumeResp = &agentv1.CreateVolumeResponse{
		DevicePath:    wantDevicePath,
		CapacityBytes: 1073741824,
	}

	// First attempt: ExportVolume fails.
	origExport := env.agent.exportVolumeResp
	env.agent.exportVolumeErr = errors.New("export failed")

	//nolint:errcheck // first attempt is expected to fail; error is intentionally discarded
	_, _ = env.srv.CreateVolume(ctx, baseCreateVolumeRequest())

	// Verify PillarVolume CRD was created with the device path.
	pv := &v1alpha1.PillarVolume{}
	if err := env.srv.k8sClient.Get(ctx,
		ctrlKey("pvc-abc123"), pv); err != nil {
		t.Fatalf("get PillarVolume after first attempt: %v", err)
	}
	if pv.Status.BackendDevicePath != wantDevicePath {
		t.Errorf("PillarVolume.Status.BackendDevicePath = %q, want %q",
			pv.Status.BackendDevicePath, wantDevicePath)
	}
	if pv.Status.Phase != v1alpha1.PillarVolumePhaseCreatePartial {
		t.Errorf("PillarVolume.Status.Phase = %q, want CreatePartial", pv.Status.Phase)
	}

	// Second attempt: capture what device path ExportVolume receives.
	env.agent.exportVolumeErr = nil
	env.agent.exportVolumeResp = origExport

	var capturedDevicePath string
	origDialer := env.srv.dialAgent
	env.srv.dialAgent = func(ctx context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
		client, closer, err := origDialer(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		return &devicePathCapturingClient{
			AgentServiceClient: client,
			capturedDevicePath: &capturedDevicePath,
		}, closer, nil
	}

	if _, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest()); err != nil {
		t.Fatalf("second CreateVolume error: %v", err)
	}

	if capturedDevicePath != wantDevicePath {
		t.Errorf("ExportVolume received DevicePath = %q, want %q",
			capturedDevicePath, wantDevicePath)
	}

	// BackendDevicePath should be cleared from the CRD now that it's Ready.
	if err := env.srv.k8sClient.Get(ctx, ctrlKey("pvc-abc123"), pv); err != nil {
		t.Fatalf("get PillarVolume after retry: %v", err)
	}
	if pv.Status.BackendDevicePath != "" {
		t.Errorf("BackendDevicePath not cleared after reaching Ready: %q",
			pv.Status.BackendDevicePath)
	}
}

// TestCreateVolume_ValidationErrors checks that missing required parameters
// are rejected with InvalidArgument before any agent dial is attempted.
func TestCreateVolume_ValidationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  *csi.CreateVolumeRequest
		code codes.Code
	}{
		{
			name: "missing volume name",
			req: &csi.CreateVolumeRequest{
				VolumeCapabilities: []*csi.VolumeCapability{
					{AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					}},
				},
			},
			code: codes.InvalidArgument,
		},
		{
			name: "missing capabilities",
			req:  &csi.CreateVolumeRequest{Name: "pvc-test"},
			code: codes.InvalidArgument,
		},
		{
			name: "missing target parameter",
			req: &csi.CreateVolumeRequest{
				Name: "pvc-test",
				VolumeCapabilities: []*csi.VolumeCapability{
					{AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
					}},
				},
				Parameters: map[string]string{
					"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
					"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
				},
			},
			code: codes.InvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newControllerTestEnv(t)
			_, err := env.srv.CreateVolume(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			st, _ := status.FromError(err)
			if st.Code() != tc.code {
				t.Errorf("error code = %v, want %v", st.Code(), tc.code)
			}
			// No agent calls should have been made.
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Errorf("agent was contacted despite validation error")
			}
		})
	}
}

// TestCreateVolume_AgentUnavailable verifies that a failed agent dial returns
// codes.Unavailable (not Internal or a panic).
func TestCreateVolume_AgentUnavailable(t *testing.T) {
	t.Parallel()
	// Build a scheme with our types.
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolume{}).
		Build()

	dialErr := status.Error(codes.Unavailable, "connection refused")
	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com",
		func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
			return nil, nil, dialErr
		})

	_, err := srv.CreateVolume(context.Background(), baseCreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %v, want Unavailable", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers for device-path capture
// ─────────────────────────────────────────────────────────────────────────────.

// devicePathCapturingClient wraps another AgentServiceClient and intercepts
// ExportVolume calls to record the DevicePath field.
type devicePathCapturingClient struct {
	agentv1.AgentServiceClient
	capturedDevicePath *string
}

func (c *devicePathCapturingClient) ExportVolume(
	ctx context.Context,
	in *agentv1.ExportVolumeRequest,
	opts ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	*c.capturedDevicePath = in.GetDevicePath()
	return c.AgentServiceClient.ExportVolume(ctx, in, opts...)
}

// ctrlKey returns a NamespacedName with an empty namespace (cluster-scoped
// resources like PillarVolume and PillarTarget use no namespace).
func ctrlKey(name string) types.NamespacedName {
	return types.NamespacedName{Name: name}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetCapacity tests
// ─────────────────────────────────────────────────────────────────────────────.

// baseGetCapacityRequest returns a minimal valid GetCapacityRequest.
func baseGetCapacityRequest() *csi.GetCapacityRequest {
	return &csi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":       "storage-node-1",
			"pillar-csi.bhyoo.com/pool":         "tank",
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	}
}

// TestGetCapacity_Success verifies the happy path: the controller dials the
// agent, calls GetCapacity, and returns AvailableCapacity from the response.
func TestGetCapacity_Success(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	env.agent.getCapacityResp = &agentv1.GetCapacityResponse{
		TotalBytes:     100 << 30,
		AvailableBytes: 60 << 30,
		UsedBytes:      40 << 30,
	}

	resp, err := env.srv.GetCapacity(ctx, baseGetCapacityRequest())
	if err != nil {
		t.Fatalf("GetCapacity unexpected error: %v", err)
	}

	const wantAvailable = int64(60 << 30)
	if resp.AvailableCapacity != wantAvailable {
		t.Errorf("AvailableCapacity = %d, want %d", resp.AvailableCapacity, wantAvailable)
	}
	if env.agent.getCapacityCalls != 1 {
		t.Errorf("getCapacityCalls = %d, want 1", env.agent.getCapacityCalls)
	}
}

// TestGetCapacity_MissingTargetParam verifies that omitting the required
// target parameter returns codes.InvalidArgument.
func TestGetCapacity_MissingTargetParam(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := &csi.GetCapacityRequest{
		Parameters: map[string]string{
			// target intentionally omitted
			"pillar-csi.bhyoo.com/pool":         "tank",
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	}

	_, err := env.srv.GetCapacity(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestGetCapacity_MissingPoolParam verifies that omitting the required
// pool parameter returns codes.InvalidArgument.
func TestGetCapacity_MissingPoolParam(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := &csi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target": "storage-node-1",
			// pool intentionally omitted
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	}

	_, err := env.srv.GetCapacity(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestGetCapacity_MissingBackendTypeParam verifies that omitting the required
// backend-type parameter returns codes.InvalidArgument.
func TestGetCapacity_MissingBackendTypeParam(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := &csi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target": "storage-node-1",
			"pillar-csi.bhyoo.com/pool":   "tank",
			// backend-type intentionally omitted
		},
	}

	_, err := env.srv.GetCapacity(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestGetCapacity_TargetNotFound verifies that referencing a non-existent
// PillarTarget returns codes.NotFound.
func TestGetCapacity_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := &csi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":       "nonexistent-target",
			"pillar-csi.bhyoo.com/pool":         "tank",
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	}

	_, err := env.srv.GetCapacity(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("error code = %v, want NotFound", st.Code())
	}
}

// TestGetCapacity_AgentError verifies that an error from the agent propagates
// to the caller with the original gRPC status code preserved.
func TestGetCapacity_AgentError(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	env.agent.getCapacityErr = status.Error(codes.NotFound, "pool not found")

	_, err := env.srv.GetCapacity(ctx, baseGetCapacityRequest())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("error code = %v, want NotFound", st.Code())
	}
}

// TestGetCapacity_TargetNoAddress verifies that a PillarTarget with an empty
// ResolvedAddress returns codes.Unavailable.
func TestGetCapacity_TargetNoAddress(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	// Create a PillarTarget with no ResolvedAddress.
	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: "", // empty — agent not ready
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarTarget{}).
		Build()

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com",
		func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
			t.Error("dialAgent should not be called when address is empty")
			return nil, nil, errors.New("should not dial")
		})

	_, err := srv.GetCapacity(context.Background(), baseGetCapacityRequest())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %v, want Unavailable", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PVC annotation override integration tests
// ─────────────────────────────────────────────────────────────────────────────.

// newControllerTestEnvWithPVC builds a ControllerServer test environment where
// a PVC in the given namespace carries the supplied annotations.  The
// StorageClass parameters in the returned request include the
// csi.storage.k8s.io/pvc-name and csi.storage.k8s.io/pvc-namespace keys so
// that CreateVolume can look up the PVC and apply annotation overrides.
func newControllerTestEnvWithPVC(
	t *testing.T,
	pvcNamespace, pvcName string,
	annotations map[string]string,
) (*controllerTestEnv, *csi.CreateVolumeRequest) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   pvcNamespace,
			Annotations: annotations,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target, pvc).
		WithStatusSubresource(&v1alpha1.PillarVolume{}, &v1alpha1.PillarTarget{}).
		Build()

	agent := &mockAgentClient{}
	dialer := func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, nopCloser{}, nil
	}
	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	// Build a CreateVolumeRequest that includes the pvc-name / pvc-namespace
	// metadata injected by external-provisioner --extra-create-metadata.
	req := baseCreateVolumeRequest()
	req.Parameters["csi.storage.k8s.io/pvc-name"] = pvcName
	req.Parameters["csi.storage.k8s.io/pvc-namespace"] = pvcNamespace

	return &controllerTestEnv{srv: srv, agent: agent, scheme: scheme}, req
}

// TestCreateVolume_PVCAnnotationOverride_ZFSProperty verifies the end-to-end
// PVC annotation override flow for a ZFS property (compression).
//
// Expected behavior:
//  1. The PVC carries "pillar-csi.bhyoo.com/backend-override" with zfs.properties.compression=zstd.
//  2. CreateVolume merges this annotation into the parameter map.
//  3. buildBackendParams populates ZfsVolumeParams.Properties["compression"] = "zstd".
//  4. The agent.CreateVolume request contains that ZFS property.
func TestCreateVolume_PVCAnnotationOverride_ZFSProperty(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		AnnotationBackendOverride: `
zfs:
  properties:
    compression: zstd
    volblocksize: "16K"
`,
	}

	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-ann-test", annotations)
	ctx := context.Background()

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}
	if resp.GetVolume() == nil {
		t.Fatal("response Volume is nil")
	}

	// The agent must have been called.
	if env.agent.createVolumeCalls != 1 {
		t.Fatalf("agent.CreateVolume call count = %d, want 1", env.agent.createVolumeCalls)
	}

	// The CreateVolume request must carry ZFS properties derived from the PVC
	// annotation.
	req2 := env.agent.lastCreateVolumeReq
	if req2 == nil {
		t.Fatal("lastCreateVolumeReq is nil — mock did not capture the request")
	}
	zfsParams := req2.GetBackendParams().GetZfs()
	if zfsParams == nil {
		t.Fatal("BackendParams.Zfs is nil")
	}
	wantProps := map[string]string{
		"compression":  "zstd",
		"volblocksize": "16K",
	}
	for k, wantV := range wantProps {
		gotV, ok := zfsParams.GetProperties()[k]
		if !ok {
			t.Errorf("ZfsVolumeParams.Properties[%q] not present (got map %v)", k, zfsParams.GetProperties())
			continue
		}
		if gotV != wantV {
			t.Errorf("ZfsVolumeParams.Properties[%q] = %q, want %q", k, gotV, wantV)
		}
	}
}

// TestCreateVolume_PVCAnnotationOverride_FlatParam verifies that a flat
// "pillar-csi.bhyoo.com/param.<key>" annotation is also merged into the
// parameter map and reaches the agent as a ZFS property.
func TestCreateVolume_PVCAnnotationOverride_FlatParam(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		// Flat override: sets zfs-prop.compression directly.
		"pillar-csi.bhyoo.com/param." + paramZFSPropPrefix + "compression": "lz4",
	}

	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-flat-test", annotations)
	ctx := context.Background()

	_, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}
	if env.agent.createVolumeCalls != 1 {
		t.Fatalf("agent.CreateVolume call count = %d, want 1", env.agent.createVolumeCalls)
	}

	zfsParams := env.agent.lastCreateVolumeReq.GetBackendParams().GetZfs()
	if zfsParams == nil {
		t.Fatal("BackendParams.Zfs is nil")
	}
	if got := zfsParams.GetProperties()["compression"]; got != "lz4" {
		t.Errorf("ZfsVolumeParams.Properties[\"compression\"] = %q, want \"lz4\"", got)
	}
}

// TestCreateVolume_PVCAnnotationOverride_BlockedField verifies that a PVC
// annotation that attempts to override a structural ZFS field (pool) causes
// CreateVolume to return codes.InvalidArgument (not codes.Internal).
func TestCreateVolume_PVCAnnotationOverride_BlockedField(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		AnnotationBackendOverride: `
zfs:
  pool: evil-pool
`,
	}

	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-blocked-test", annotations)
	ctx := context.Background()

	_, err := env.srv.CreateVolume(ctx, req)
	if err == nil {
		t.Fatal("expected error for blocked structural field, got nil")
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument (got message: %s)", st.Code(), st.Message())
	}
	// Agent must NOT have been called.
	if env.agent.createVolumeCalls != 0 {
		t.Errorf("agent.CreateVolume called despite annotation validation failure")
	}
}

// TestCreateVolume_PVCAnnotationOverride_NoPVCMetadata verifies that when the
// pvc-name / pvc-namespace parameters are absent (StorageClass provisioned
// without external-provisioner --extra-create-metadata) the call succeeds
// without annotation overrides.
func TestCreateVolume_PVCAnnotationOverride_NoPVCMetadata(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := baseCreateVolumeRequest()
	// Deliberately omit pvc-name and pvc-namespace.

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}
	if resp.GetVolume() == nil {
		t.Fatal("response Volume is nil")
	}
	// Agent must still have been called normally.
	if env.agent.createVolumeCalls != 1 {
		t.Errorf("agent.CreateVolume call count = %d, want 1", env.agent.createVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LVM mode parameter-parsing unit tests
// ─────────────────────────────────────────────────────────────────────────────

// TestBuildBackendParams_LVM_WithThinMode verifies that buildBackendParams
// correctly propagates the "thin" provisioning mode from the merged parameter
// map into LvmVolumeParams.ProvisionMode.
func TestBuildBackendParams_LVM_WithThinMode(t *testing.T) {
	t.Parallel()

	params := map[string]string{
		paramLVMVG:   "data-vg",
		paramLVMMode: testModeThin,
	}
	got := buildBackendParams(params, agentv1.BackendType_BACKEND_TYPE_LVM)
	if got == nil {
		t.Fatal("buildBackendParams returned nil")
	}
	lvm := got.GetLvm()
	if lvm == nil {
		t.Fatal("BackendParams.Lvm is nil")
	}
	if lvm.GetVolumeGroup() != "data-vg" {
		t.Errorf("VolumeGroup = %q, want %q", lvm.GetVolumeGroup(), "data-vg")
	}
	if lvm.GetProvisionMode() != testModeThin {
		t.Errorf("ProvisionMode = %q, want %q", lvm.GetProvisionMode(), testModeThin)
	}
}

// TestBuildBackendParams_LVM_WithLinearMode verifies that "linear" mode is
// forwarded correctly.
func TestBuildBackendParams_LVM_WithLinearMode(t *testing.T) {
	t.Parallel()

	params := map[string]string{
		paramLVMVG:   "fast-vg",
		paramLVMMode: "linear",
	}
	got := buildBackendParams(params, agentv1.BackendType_BACKEND_TYPE_LVM)
	lvm := got.GetLvm()
	if lvm == nil {
		t.Fatal("BackendParams.Lvm is nil")
	}
	if lvm.GetProvisionMode() != "linear" {
		t.Errorf("ProvisionMode = %q, want %q", lvm.GetProvisionMode(), "linear")
	}
}

// TestBuildBackendParams_LVM_AbsentMode verifies that when paramLVMMode is
// absent from the parameter map, ProvisionMode is the empty string (letting the
// agent backend use its compiled-in default).
func TestBuildBackendParams_LVM_AbsentMode(t *testing.T) {
	t.Parallel()

	params := map[string]string{
		paramLVMVG: "data-vg",
		// paramLVMMode intentionally absent
	}
	got := buildBackendParams(params, agentv1.BackendType_BACKEND_TYPE_LVM)
	lvm := got.GetLvm()
	if lvm == nil {
		t.Fatal("BackendParams.Lvm is nil")
	}
	if lvm.GetProvisionMode() != "" {
		t.Errorf("ProvisionMode = %q, want empty string", lvm.GetProvisionMode())
	}
}

// TestMergeParamsFromCRDs_LVM_PoolDefault verifies that the PillarPool-level
// LVM provisioning mode (Layer 1) is propagated into the merged parameter map
// as paramLVMMode when no binding-level override is present.
func TestMergeParamsFromCRDs_LVM_PoolDefault(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	pool := &v1alpha1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-pool"},
		Spec: v1alpha1.PillarPoolSpec{
			TargetRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{
				Type: v1alpha1.BackendTypeLVMLV,
				LVM: &v1alpha1.LVMBackendConfig{
					VolumeGroup:      "data-vg",
					ThinPool:         "thin-pool-0",
					ProvisioningMode: v1alpha1.LVMProvisioningModeThin,
				},
			},
		},
	}
	binding := &v1alpha1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding"},
		Spec: v1alpha1.PillarBindingSpec{
			PoolRef:     "lvm-pool",
			ProtocolRef: "nvmeof-tcp",
			// No LVM overrides — pool default should surface.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool, binding).
		Build()

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)

	scParams := map[string]string{
		paramBinding: "lvm-binding",
	}
	merged, err := srv.mergeParamsFromCRDs(context.Background(), scParams)
	if err != nil {
		t.Fatalf("mergeParamsFromCRDs unexpected error: %v", err)
	}

	if got := merged[paramLVMMode]; got != testModeThin {
		t.Errorf("merged[paramLVMMode] = %q, want %q", got, testModeThin)
	}
}

// TestMergeParamsFromCRDs_LVM_BindingOverride verifies that the PillarBinding-
// level LVM provisioning mode override (Layer 3) wins over the pool-level
// default (Layer 1).
func TestMergeParamsFromCRDs_LVM_BindingOverride(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	// Pool default is "linear" …
	pool := &v1alpha1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-pool2"},
		Spec: v1alpha1.PillarPoolSpec{
			TargetRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{
				Type: v1alpha1.BackendTypeLVMLV,
				LVM: &v1alpha1.LVMBackendConfig{
					VolumeGroup:      "data-vg",
					ThinPool:         "thin-pool-0",
					ProvisioningMode: v1alpha1.LVMProvisioningModeLinear,
				},
			},
		},
	}
	// … but binding overrides to "thin".
	binding := &v1alpha1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding2"},
		Spec: v1alpha1.PillarBindingSpec{
			PoolRef:     "lvm-pool2",
			ProtocolRef: "nvmeof-tcp",
			Overrides: &v1alpha1.BindingOverrides{
				Backend: &v1alpha1.BackendOverrides{
					LVM: &v1alpha1.LVMOverrides{
						ProvisioningMode: v1alpha1.LVMProvisioningModeThin,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool, binding).
		Build()

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)

	scParams := map[string]string{
		paramBinding: "lvm-binding2",
	}
	merged, err := srv.mergeParamsFromCRDs(context.Background(), scParams)
	if err != nil {
		t.Fatalf("mergeParamsFromCRDs unexpected error: %v", err)
	}

	// The binding override ("thin") must beat the pool default ("linear").
	if got := merged[paramLVMMode]; got != testModeThin {
		t.Errorf("merged[paramLVMMode] = %q, want %q (binding override should win)", got, testModeThin)
	}
}

// TestMergeParamsFromCRDs_LVM_SCOverridePool verifies that an explicit lvm-mode
// value already present in the StorageClass parameters (Layer 2) takes priority
// over the PillarPool-level default (Layer 1).  The StorageClass value must be
// preserved unchanged after mergeParamsFromCRDs returns.
func TestMergeParamsFromCRDs_LVM_SCOverridePool(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	// Pool wants "thin" provisioning …
	pool := &v1alpha1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-pool-sc"},
		Spec: v1alpha1.PillarPoolSpec{
			TargetRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{
				Type: v1alpha1.BackendTypeLVMLV,
				LVM: &v1alpha1.LVMBackendConfig{
					VolumeGroup:      "data-vg",
					ThinPool:         "thin-pool-0",
					ProvisioningMode: v1alpha1.LVMProvisioningModeThin,
				},
			},
		},
	}
	binding := &v1alpha1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding-sc"},
		Spec: v1alpha1.PillarBindingSpec{
			PoolRef:     "lvm-pool-sc",
			ProtocolRef: "nvmeof-tcp",
			// No LVM overrides — pool default should remain below SC value.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool, binding).
		Build()

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)

	// StorageClass explicitly opts into "linear" even though the pool defaults to "thin".
	scParams := map[string]string{
		paramBinding: "lvm-binding-sc",
		paramLVMMode: "linear", // SC override
	}
	merged, err := srv.mergeParamsFromCRDs(context.Background(), scParams)
	if err != nil {
		t.Fatalf("mergeParamsFromCRDs unexpected error: %v", err)
	}

	// SC value must win over pool default.
	if got := merged[paramLVMMode]; got != "linear" {
		t.Errorf("merged[paramLVMMode] = %q, want %q (SC override should beat pool default)", got, "linear")
	}
}

// TestMergeParamsFromCRDs_LVM_NoModeConfigured verifies that paramLVMMode is
// absent from the merged map when neither the pool nor the binding specifies a
// provisioning mode.  This lets the agent use its compiled-in default.
func TestMergeParamsFromCRDs_LVM_NoModeConfigured(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}

	pool := &v1alpha1.PillarPool{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-pool3"},
		Spec: v1alpha1.PillarPoolSpec{
			TargetRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{
				Type: v1alpha1.BackendTypeLVMLV,
				LVM: &v1alpha1.LVMBackendConfig{
					VolumeGroup: "data-vg",
					// ProvisioningMode deliberately omitted.
				},
			},
		},
	}
	binding := &v1alpha1.PillarBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding3"},
		Spec: v1alpha1.PillarBindingSpec{
			PoolRef:     "lvm-pool3",
			ProtocolRef: "nvmeof-tcp",
			// No overrides.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool, binding).
		Build()

	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", nil)

	scParams := map[string]string{
		paramBinding: "lvm-binding3",
	}
	merged, err := srv.mergeParamsFromCRDs(context.Background(), scParams)
	if err != nil {
		t.Fatalf("mergeParamsFromCRDs unexpected error: %v", err)
	}

	// paramLVMMode must not be set — absent key means "use agent default".
	if got, present := merged[paramLVMMode]; present {
		t.Errorf("merged[paramLVMMode] = %q, want key absent", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerPublishVolume — CSINode annotation lookup and FailedPrecondition
// ─────────────────────────────────────────────────────────────────────────────

// newPublishTestEnv builds a ControllerServer wired to a fake k8s client that
// has a PillarTarget but no CSINode by default.  Callers can seed CSINode
// objects as needed for each test case.
func newPublishTestEnv(t *testing.T, objs ...ctrlclient.Object) *controllerTestEnv {
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

// basePublishRequest returns a minimal valid ControllerPublishVolumeRequest for
// the nvmeof-tcp protocol targeting "worker-node-1".
func basePublishRequest() *csi.ControllerPublishVolumeRequest {
	return &csi.ControllerPublishVolumeRequest{
		VolumeId: "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123",
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
}

// TestControllerPublishVolume_FailedPrecondition_CSINodeNotFound verifies that
// ControllerPublishVolume returns FailedPrecondition when the CSINode object
// does not exist yet (node plugin has not registered).
func TestControllerPublishVolume_FailedPrecondition_CSINodeNotFound(t *testing.T) {
	t.Parallel()

	// No CSINode objects seeded → CSINode lookup will return NotFound.
	env := newPublishTestEnv(t)
	ctx := context.Background()

	_, err := env.srv.ControllerPublishVolume(ctx, basePublishRequest())
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

// TestControllerPublishVolume_FailedPrecondition_AnnotationMissing verifies
// that ControllerPublishVolume returns FailedPrecondition when the CSINode
// exists but the nvmeof-host-nqn annotation is absent.
//
// This is the "annotation write race" scenario described in RFC Section 5.2:
// the node plugin has not yet written its identity after a fresh node bootstrap.
func TestControllerPublishVolume_FailedPrecondition_AnnotationMissing(t *testing.T) {
	t.Parallel()

	// CSINode exists but has no annotations.
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// Annotations deliberately omitted.
		},
	}
	env := newPublishTestEnv(t, csiNode)
	ctx := context.Background()

	_, err := env.srv.ControllerPublishVolume(ctx, basePublishRequest())
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

// TestControllerPublishVolume_SuccessWithAnnotation verifies that
// ControllerPublishVolume resolves the NQN from the CSINode annotation and
// passes it as initiator_id to AllowInitiator when the annotation is present.
func TestControllerPublishVolume_SuccessWithAnnotation(t *testing.T) {
	t.Parallel()

	const hostNQN = "nqn.2014-08.org.nvmexpress:uuid:worker-node-1-nqn"

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			Annotations: map[string]string{
				AnnotationNVMeOFHostNQN: hostNQN,
			},
		},
	}
	env := newPublishTestEnv(t, csiNode)
	ctx := context.Background()

	_, err := env.srv.ControllerPublishVolume(ctx, basePublishRequest())
	if err != nil {
		t.Fatalf("ControllerPublishVolume unexpected error: %v", err)
	}

	// AllowInitiator must have been called exactly once with the resolved NQN.
	if env.agent.allowInitiatorCalls != 1 {
		t.Errorf("AllowInitiator call count = %d, want 1", env.agent.allowInitiatorCalls)
	}
	if env.agent.lastAllowInitiator == nil {
		t.Fatal("lastAllowInitiator is nil")
	}
	if got := env.agent.lastAllowInitiator.InitiatorId; got != hostNQN {
		t.Errorf("AllowInitiator.InitiatorId = %q, want %q", got, hostNQN)
	}
}

// TestControllerPublishVolume_FailedPrecondition_ISCSIAnnotationMissing verifies
// that ControllerPublishVolume returns FailedPrecondition for the iSCSI protocol
// when the iscsi-initiator-iqn annotation is absent.
func TestControllerPublishVolume_FailedPrecondition_ISCSIAnnotationMissing(t *testing.T) {
	t.Parallel()

	// CSINode exists but has no iSCSI IQN annotation.
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-node-1",
			// No iscsi-initiator-iqn annotation.
		},
	}
	env := newPublishTestEnv(t, csiNode)
	ctx := context.Background()

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

	_, err := env.srv.ControllerPublishVolume(ctx, req)
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
