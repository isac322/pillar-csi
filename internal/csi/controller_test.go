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

// Tests for the ControllerServer.CreateVolume idempotency behaviour.
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
// A controller-runtime fake client supplies Kubernetes API behaviour, and a
// mock AgentServiceClient supplies agent RPC behaviour.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock AgentServiceClient
// ─────────────────────────────────────────────────────────────────────────────

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

	// Call counters — verified by tests.
	createVolumeCalls   int
	exportVolumeCalls   int
	unexportVolumeCalls int
	deleteVolumeCalls   int
	getCapacityCalls    int
}

// Compile-time check that mockAgentClient implements the full interface.
var _ agentv1.AgentServiceClient = (*mockAgentClient)(nil)

func (m *mockAgentClient) CreateVolume(
	_ context.Context,
	_ *agentv1.CreateVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.CreateVolumeResponse, error) {
	m.createVolumeCalls++
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
func (m *mockAgentClient) GetCapabilities(_ context.Context, _ *agentv1.GetCapabilitiesRequest, _ ...grpc.CallOption) (*agentv1.GetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) GetCapacity(_ context.Context, _ *agentv1.GetCapacityRequest, _ ...grpc.CallOption) (*agentv1.GetCapacityResponse, error) {
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
func (m *mockAgentClient) ListVolumes(_ context.Context, _ *agentv1.ListVolumesRequest, _ ...grpc.CallOption) (*agentv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) ListExports(_ context.Context, _ *agentv1.ListExportsRequest, _ ...grpc.CallOption) (*agentv1.ListExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) HealthCheck(_ context.Context, _ *agentv1.HealthCheckRequest, _ ...grpc.CallOption) (*agentv1.HealthCheckResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) ExpandVolume(_ context.Context, _ *agentv1.ExpandVolumeRequest, _ ...grpc.CallOption) (*agentv1.ExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) AllowInitiator(_ context.Context, _ *agentv1.AllowInitiatorRequest, _ ...grpc.CallOption) (*agentv1.AllowInitiatorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) DenyInitiator(_ context.Context, _ *agentv1.DenyInitiatorRequest, _ ...grpc.CallOption) (*agentv1.DenyInitiatorResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) SendVolume(_ context.Context, _ *agentv1.SendVolumeRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentv1.SendVolumeChunk], error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) ReceiveVolume(_ context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[agentv1.ReceiveVolumeChunk, agentv1.ReceiveVolumeResponse], error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}
func (m *mockAgentClient) ReconcileState(_ context.Context, _ *agentv1.ReconcileStateRequest, _ ...grpc.CallOption) (*agentv1.ReconcileStateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in mock")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

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

	// Build the scheme with the v1alpha1 types registered.
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
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
			"pillar-csi.bhyoo.com/zfs-pool":      "tank",
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

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
	if vc[VolumeContextKeyTargetNQN] == "" {
		t.Errorf("VolumeContext[%q] is empty", VolumeContextKeyTargetNQN)
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
			AgentServiceClient:  client,
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
// ─────────────────────────────────────────────────────────────────────────────

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
// ─────────────────────────────────────────────────────────────────────────────

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
			"pillar-csi.bhyoo.com/pool":  "tank",
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
