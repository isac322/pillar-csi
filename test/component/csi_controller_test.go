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

// Package component_test contains component-level tests for pillar-csi.
// These tests treat each major component as a black box, wiring mock
// dependencies and testing feature-level behavior including all exception paths.
//
// This file covers the CSI Controller Service (internal/csi.ControllerServer).
package component_test

import (
	"context"
	"io"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock AgentServiceClient for CSI controller tests
// ─────────────────────────────────────────────────────────────────────────────.

// csiMockAgent is a test double for agentv1.AgentServiceClient.
//
// # Mock fidelity
//
// Approximates: the real gRPC AgentServiceClient generated from agent.proto,
// which communicates over an mTLS-encrypted gRPC/HTTP-2 connection with a
// pillar-agent process running on a storage node.
//
// Omits / simplifies:
//   - No network transport: all RPC calls are synchronous, in-process
//     function-call dispatches.  There is no TCP socket, TLS handshake, or
//     gRPC framing overhead.
//   - No connection management: the real client caches connections, handles
//     transparent reconnections on transient failures, and propagates
//     keepalive timeout errors.  The mock is completely stateless.
//   - No mTLS: the real client presents a client certificate signed by a
//     cluster-internal CA and verifies the agent's server certificate.  The
//     mock skips all credential and certificate-chain logic.
//   - No serialization: protobuf encoding / decoding and per-field validation
//     performed by the generated gRPC stubs are not exercised.
//   - grpc.CallOption: deadline, per-RPC credentials, and other options
//     passed as variadic grpc.CallOption arguments are silently ignored.
//   - No real storage operations: the real agent performs ZFS and NVMe-oF
//     operations; the mock returns preset or default in-memory responses.
//   - Streaming RPCs (SendVolume, ReceiveVolume): stub methods return an
//     Unimplemented status error; no streaming-frame protocol is simulated.
//   - Call counters track invocation frequency but not concurrency safety;
//     tests that call the mock from multiple goroutines must add their own
//     synchronization.
//
// Each RPC method has a corresponding function field (e.g. createVolumeFn);
// when nil the method succeeds with a sensible default response.
type csiMockAgent struct {
	createVolumeFn   func(ctx context.Context, req *agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error)
	deleteVolumeFn   func(ctx context.Context, req *agentv1.DeleteVolumeRequest) (*agentv1.DeleteVolumeResponse, error)
	exportVolumeFn   func(ctx context.Context, req *agentv1.ExportVolumeRequest) (*agentv1.ExportVolumeResponse, error)
	unexportVolumeFn func(ctx context.Context, req *agentv1.UnexportVolumeRequest) (*agentv1.UnexportVolumeResponse, error)
	expandVolumeFn   func(ctx context.Context, req *agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error)
	allowInitiatorFn func(ctx context.Context, req *agentv1.AllowInitiatorRequest) (*agentv1.AllowInitiatorResponse, error)
	denyInitiatorFn  func(ctx context.Context, req *agentv1.DenyInitiatorRequest) (*agentv1.DenyInitiatorResponse, error)

	// call counters
	createVolumeCalls   int
	deleteVolumeCalls   int
	exportVolumeCalls   int
	unexportVolumeCalls int
	expandVolumeCalls   int
	allowInitiatorCalls int
	denyInitiatorCalls  int
}

// Verify csiMockAgent implements the full AgentServiceClient interface.
var _ agentv1.AgentServiceClient = (*csiMockAgent)(nil)

func (m *csiMockAgent) CreateVolume(
	ctx context.Context, req *agentv1.CreateVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.CreateVolumeResponse, error) {
	m.createVolumeCalls++
	if m.createVolumeFn != nil {
		return m.createVolumeFn(ctx, req)
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/zvol/tank/test-vol",
		CapacityBytes: req.GetCapacityBytes(),
	}, nil
}

func (m *csiMockAgent) DeleteVolume(
	ctx context.Context, req *agentv1.DeleteVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.DeleteVolumeResponse, error) {
	m.deleteVolumeCalls++
	if m.deleteVolumeFn != nil {
		return m.deleteVolumeFn(ctx, req)
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

func (m *csiMockAgent) ExportVolume(
	ctx context.Context, req *agentv1.ExportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	m.exportVolumeCalls++
	if m.exportVolumeFn != nil {
		return m.exportVolumeFn(ctx, req)
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.pillar-csi:test-vol",
			Address:   "192.168.1.10",
			Port:      4420,
			VolumeRef: req.GetVolumeId(),
		},
	}, nil
}

func (m *csiMockAgent) UnexportVolume(
	ctx context.Context, req *agentv1.UnexportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.UnexportVolumeResponse, error) {
	m.unexportVolumeCalls++
	if m.unexportVolumeFn != nil {
		return m.unexportVolumeFn(ctx, req)
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

func (m *csiMockAgent) ExpandVolume(
	ctx context.Context, req *agentv1.ExpandVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExpandVolumeResponse, error) {
	m.expandVolumeCalls++
	if m.expandVolumeFn != nil {
		return m.expandVolumeFn(ctx, req)
	}
	return &agentv1.ExpandVolumeResponse{
		CapacityBytes: req.GetRequestedBytes(),
	}, nil
}

func (m *csiMockAgent) AllowInitiator(
	ctx context.Context, req *agentv1.AllowInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.AllowInitiatorResponse, error) {
	m.allowInitiatorCalls++
	if m.allowInitiatorFn != nil {
		return m.allowInitiatorFn(ctx, req)
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}

func (m *csiMockAgent) DenyInitiator(
	ctx context.Context, req *agentv1.DenyInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.DenyInitiatorResponse, error) {
	m.denyInitiatorCalls++
	if m.denyInitiatorFn != nil {
		return m.denyInitiatorFn(ctx, req)
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}

// Stubs for methods not exercised by controller tests.
func (*csiMockAgent) GetCapabilities(
	_ context.Context, _ *agentv1.GetCapabilitiesRequest, _ ...grpc.CallOption,
) (*agentv1.GetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) GetCapacity(
	_ context.Context, _ *agentv1.GetCapacityRequest, _ ...grpc.CallOption,
) (*agentv1.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) ListVolumes(
	_ context.Context, _ *agentv1.ListVolumesRequest, _ ...grpc.CallOption,
) (*agentv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) ListExports(
	_ context.Context, _ *agentv1.ListExportsRequest, _ ...grpc.CallOption,
) (*agentv1.ListExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) HealthCheck(
	_ context.Context, _ *agentv1.HealthCheckRequest, _ ...grpc.CallOption,
) (*agentv1.HealthCheckResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) SendVolume(
	_ context.Context, _ *agentv1.SendVolumeRequest, _ ...grpc.CallOption,
) (grpc.ServerStreamingClient[agentv1.SendVolumeChunk], error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) ReceiveVolume(
	_ context.Context, _ ...grpc.CallOption,
) (grpc.ClientStreamingClient[agentv1.ReceiveVolumeChunk, agentv1.ReceiveVolumeResponse], error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}
func (*csiMockAgent) ReconcileState(
	_ context.Context, _ *agentv1.ReconcileStateRequest, _ ...grpc.CallOption,
) (*agentv1.ReconcileStateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in controller tests")
}

// ─────────────────────────────────────────────────────────────────────────────
// Test environment setup
// ─────────────────────────────────────────────────────────────────────────────.

// csiNopCloser is a test double for the io.Closer returned by an AgentDialer.
//
// # Mock fidelity
//
// Approximates: the real io.Closer paired with an AgentServiceClient by
// agentclient.Manager.Dial.  In production, Close() drains in-flight gRPC
// RPCs and tears down the underlying *grpc.ClientConn, releasing file
// descriptors, goroutines, and TLS session state.
//
// Omits: all connection teardown.  No goroutines are stopped, no sockets are
// closed, no gRPC draining protocol is exercised, and no TLS shutdown alert
// is sent.  Tests that need to verify orderly connection shutdown must use a
// real gRPC connection or a more sophisticated stub.
type csiNopCloser struct{}

func (csiNopCloser) Close() error { return nil }

// csiControllerTestEnv holds everything for a ControllerServer component test.
//
// The Kubernetes client embedded in csiControllerTestEnv is a
// controller-runtime fake client (fake.NewClientBuilder).
//
// # Fake Kubernetes client — mock fidelity
//
// Approximates: a real Kubernetes API server client (sigs.k8s.io/controller-
// runtime/pkg/client backed by a live kube-apiserver), which persists objects
// in etcd, enforces admission webhooks, applies defaulting, and notifies
// controllers via watch streams.
//
// Omits / simplifies:
//   - No etcd or API server process: object storage is entirely in-memory.
//   - No admission control: defaulting, validation webhooks, and mutating
//     admission plugins are not invoked; the test must supply fully-formed
//     objects.
//   - No watch/informer events: Get/List queries return stored objects
//     immediately; no controller cache is populated and no reconcile loop is
//     triggered.
//   - No optimistic concurrency: resource-version conflict detection
//     (HTTP 409 Conflict) is not enforced; concurrent updates overwrite each
//     other silently.
//   - Status sub-resource: WithStatusSubresource() opt-in is required for
//     Status().Update() to be segregated from the main object store; otherwise
//     status fields are ignored on Update calls.
type csiControllerTestEnv struct {
	srv   *pillarcsi.ControllerServer
	agent *csiMockAgent
}

// newCSIControllerTestEnv builds a ControllerServer backed by:
//   - a controller-runtime fake k8s client seeded with one PillarTarget
//     whose ResolvedAddress is "192.168.1.10:9500"
//   - a csiMockAgent injected via the AgentDialer
func newCSIControllerTestEnv(t *testing.T) *csiControllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

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

	agent := &csiMockAgent{}

	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, csiNopCloser{}, nil
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	return &csiControllerTestEnv{
		srv:   srv,
		agent: agent,
	}
}

// newCSIControllerTestEnvWithDialErr builds a ControllerServer whose AgentDialer
// always returns an error — simulating an unreachable agent.
func newCSIControllerTestEnvWithDialErr(t *testing.T, dialErr error) *csiControllerTestEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status:     v1alpha1.PillarTargetStatus{ResolvedAddress: "192.168.1.10:9500"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(target).
		WithStatusSubresource(&v1alpha1.PillarVolume{}, &v1alpha1.PillarTarget{}).
		Build()

	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return nil, nil, dialErr
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &csiControllerTestEnv{srv: srv, agent: nil}
}

// baseCSICreateVolumeRequest returns a valid CreateVolumeRequest for
// "storage-node-1" (the seeded PillarTarget) with a 1 GiB capacity.
func baseCSICreateVolumeRequest() *csipb.CreateVolumeRequest {
	return &csipb.CreateVolumeRequest{
		Name: "pvc-component-test",
		VolumeCapabilities: []*csipb.VolumeCapability{
			{
				AccessType: &csipb.VolumeCapability_Block{
					Block: &csipb.VolumeCapability_BlockVolume{},
				},
				AccessMode: &csipb.VolumeCapability_AccessMode{
					Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: 1 << 30, // 1 GiB
		},
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":        "storage-node-1",
			"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
			"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
			"pillar-csi.bhyoo.com/pool":          "tank",
		},
	}
}

// expectedVolumeID is the volume ID encoded by the controller for the base request.
const expectedCSIVolumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-component-test"

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_Success
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_Success verifies the happy-path CreateVolume
// flow: both agent.CreateVolume and agent.ExportVolume are called exactly once,
// the returned VolumeId encodes target/protocol/backend/agentVolID, and the
// VolumeContext contains the NVMe-oF connection parameters.
func TestCSIController_CreateVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	resp, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}

	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("response.Volume is nil")
	}

	// VolumeId must encode routing metadata.
	if got, want := vol.GetVolumeId(), expectedCSIVolumeID; got != want {
		t.Errorf("VolumeId = %q, want %q", got, want)
	}

	// VolumeContext must carry connection parameters for NodeStageVolume.
	vc := vol.GetVolumeContext()
	if vc[pillarcsi.VolumeContextKeyTargetNQN] == "" {
		t.Errorf("VolumeContext[%q] is empty", pillarcsi.VolumeContextKeyTargetNQN)
	}
	if vc[pillarcsi.VolumeContextKeyAddress] == "" {
		t.Errorf("VolumeContext[%q] is empty", pillarcsi.VolumeContextKeyAddress)
	}
	if vc[pillarcsi.VolumeContextKeyPort] == "" {
		t.Errorf("VolumeContext[%q] is empty", pillarcsi.VolumeContextKeyPort)
	}

	// Both agent RPCs must have been called exactly once.
	if env.agent.createVolumeCalls != 1 {
		t.Errorf("agent.CreateVolume calls = %d, want 1", env.agent.createVolumeCalls)
	}
	if env.agent.exportVolumeCalls != 1 {
		t.Errorf("agent.ExportVolume calls = %d, want 1", env.agent.exportVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_CapacityRange
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_CapacityRange verifies that the controller
// correctly propagates the CapacityRange.RequiredBytes to the agent and
// the agent's reported capacity is reflected in the response.
func TestCSIController_CreateVolume_CapacityRange(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const wantBytes = int64(10 << 30) // 10 GiB
	env.agent.createVolumeFn = func(
		_ context.Context, req *agentv1.CreateVolumeRequest,
	) (*agentv1.CreateVolumeResponse, error) {
		if req.GetCapacityBytes() != wantBytes {
			t.Errorf("agent CreateVolume: CapacityBytes = %d, want %d",
				req.GetCapacityBytes(), wantBytes)
		}
		return &agentv1.CreateVolumeResponse{
			DevicePath:    "/dev/zvol/tank/pvc-component-test",
			CapacityBytes: wantBytes,
		}, nil
	}

	req := baseCSICreateVolumeRequest()
	req.CapacityRange = &csipb.CapacityRange{RequiredBytes: wantBytes}

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d", got, wantBytes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_IdempotentRetry
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_IdempotentRetry verifies that a second
// CreateVolume call for the same volume returns the cached response without
// calling the agent again (CSI spec §5.1.1 idempotency requirement).
func TestCSIController_CreateVolume_IdempotentRetry(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// First call — provisions the volume.
	resp1, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err != nil {
		t.Fatalf("first CreateVolume error: %v", err)
	}
	callsCreate1 := env.agent.createVolumeCalls
	callsExport1 := env.agent.exportVolumeCalls

	// Second call — must return the cached result without extra agent calls.
	resp2, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err != nil {
		t.Fatalf("second CreateVolume error: %v", err)
	}

	if env.agent.createVolumeCalls != callsCreate1 {
		t.Errorf("agent.CreateVolume called again on retry: total %d, after first call %d",
			env.agent.createVolumeCalls, callsCreate1)
	}
	if env.agent.exportVolumeCalls != callsExport1 {
		t.Errorf("agent.ExportVolume called again on retry: total %d, after first call %d",
			env.agent.exportVolumeCalls, callsExport1)
	}

	if resp1.GetVolume().GetVolumeId() != resp2.GetVolume().GetVolumeId() {
		t.Errorf("VolumeId mismatch: first=%q, second=%q",
			resp1.GetVolume().GetVolumeId(), resp2.GetVolume().GetVolumeId())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_AgentError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_AgentError verifies that an agent-side
// CreateVolume error is propagated to the CO with the same gRPC status code.
func TestCSIController_CreateVolume_AgentError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.createVolumeFn = func(
		_ context.Context, _ *agentv1.CreateVolumeRequest,
	) (*agentv1.CreateVolumeResponse, error) {
		return nil, status.Error(codes.ResourceExhausted, "pool out of space")
	}

	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("error code = %v, want %v", st.Code(), codes.ResourceExhausted)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_AgentUnreachable verifies that a dial
// failure (agent unreachable) returns gRPC Unavailable.
func TestCSIController_CreateVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()
	dialErr := status.Error(codes.Unavailable, "connection refused")
	env := newCSIControllerTestEnvWithDialErr(t, dialErr)
	ctx := context.Background()

	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("error code = %v, want %v", st.Code(), codes.Unavailable)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_MissingName
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_MissingName verifies that an empty volume
// name is rejected with InvalidArgument before any agent call.
func TestCSIController_CreateVolume_MissingName(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseCSICreateVolumeRequest()
	req.Name = ""

	_, err := env.srv.CreateVolume(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Errorf("agent.CreateVolume was called %d times, expected 0", env.agent.createVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_TargetNotFound
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_TargetNotFound verifies that referencing a
// non-existent PillarTarget returns NotFound.
func TestCSIController_CreateVolume_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	req := baseCSICreateVolumeRequest()
	req.Parameters["pillar-csi.bhyoo.com/target"] = "does-not-exist"

	_, err := env.srv.CreateVolume(ctx, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("error code = %v, want %v", st.Code(), codes.NotFound)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_MissingParams
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_MissingParams verifies that missing required
// StorageClass parameters are rejected with InvalidArgument.
func TestCSIController_CreateVolume_MissingParams(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name        string
		removeParam string
	}{
		{"missing target", "pillar-csi.bhyoo.com/target"},
		{"missing backend-type", "pillar-csi.bhyoo.com/backend-type"},
		{"missing protocol-type", "pillar-csi.bhyoo.com/protocol-type"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newCSIControllerTestEnv(t)
			req := baseCSICreateVolumeRequest()
			delete(req.Parameters, tc.removeParam)

			_, err := env.srv.CreateVolume(ctx, req)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.removeParam)
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_DuplicateName
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_DuplicateName verifies that a CreateVolume
// call using the same volume name but requesting a LARGER capacity than the
// already-provisioned volume returns AlreadyExists (CSI spec §5.1.1).
//
// The controller must not call the agent again — it detects the incompatibility
// from its in-memory/persisted state and returns an error immediately.
func TestCSIController_CreateVolume_DuplicateName(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const smallBytes = int64(1 << 30)  // 1 GiB — original volume size
	const largeBytes = int64(20 << 30) // 20 GiB — incompatible larger request

	// First call — provisions the volume at 1 GiB.
	req1 := baseCSICreateVolumeRequest()
	req1.CapacityRange = &csipb.CapacityRange{RequiredBytes: smallBytes}
	if _, err := env.srv.CreateVolume(ctx, req1); err != nil {
		t.Fatalf("first CreateVolume (1 GiB): unexpected error: %v", err)
	}

	// Record the agent call count after the first provisioning.
	callsAfterFirst := env.agent.createVolumeCalls

	// Second call — same name, requests 20 GiB (larger than existing 1 GiB).
	req2 := baseCSICreateVolumeRequest()
	req2.CapacityRange = &csipb.CapacityRange{RequiredBytes: largeBytes}

	_, err := env.srv.CreateVolume(ctx, req2)
	if err == nil {
		t.Fatal("expected AlreadyExists error for incompatible duplicate, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.AlreadyExists {
		t.Errorf("error code = %v, want %v", st.Code(), codes.AlreadyExists)
	}

	// The agent must NOT have been called again — the controller detects the
	// incompatibility before touching the remote storage.
	if env.agent.createVolumeCalls != callsAfterFirst {
		t.Errorf("agent.CreateVolume called again on duplicate-name retry: total=%d, after first=%d",
			env.agent.createVolumeCalls, callsAfterFirst)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume_Success
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_DeleteVolume_Success verifies that DeleteVolume calls both
// agent.UnexportVolume and agent.DeleteVolume.
func TestCSIController_DeleteVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	// First create the volume so it can be deleted.
	if _, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest()); err != nil {
		t.Fatalf("setup CreateVolume: %v", err)
	}

	_, err := env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: expectedCSIVolumeID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume: unexpected error: %v", err)
	}
	if env.agent.unexportVolumeCalls != 1 {
		t.Errorf("agent.UnexportVolume calls = %d, want 1", env.agent.unexportVolumeCalls)
	}
	if env.agent.deleteVolumeCalls != 1 {
		t.Errorf("agent.DeleteVolume calls = %d, want 1", env.agent.deleteVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume_Idempotent
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_DeleteVolume_Idempotent verifies that if the agent returns
// NotFound for both Unexport and Delete, the controller still returns success
// (idempotent behavior per CSI spec §4.3.2).
func TestCSIController_DeleteVolume_Idempotent(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.unexportVolumeFn = func(
		_ context.Context, _ *agentv1.UnexportVolumeRequest,
	) (*agentv1.UnexportVolumeResponse, error) {
		return nil, status.Error(codes.NotFound, "not found")
	}
	env.agent.deleteVolumeFn = func(
		_ context.Context, _ *agentv1.DeleteVolumeRequest,
	) (*agentv1.DeleteVolumeResponse, error) {
		return nil, status.Error(codes.NotFound, "not found")
	}

	_, err := env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: expectedCSIVolumeID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume: expected success for NotFound, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_DeleteVolume_AgentError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_DeleteVolume_AgentError verifies that a non-NotFound error
// from agent.DeleteVolume is propagated to the CO.
func TestCSIController_DeleteVolume_AgentError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.deleteVolumeFn = func(
		_ context.Context, _ *agentv1.DeleteVolumeRequest,
	) (*agentv1.DeleteVolumeResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "device busy")
	}

	_, err := env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: expectedCSIVolumeID,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("error code = %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerPublishVolume_Success
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerPublishVolume_Success verifies that
// ControllerPublishVolume calls agent.AllowInitiator with the node's NQN and
// returns an empty PublishContext.
func TestCSIController_ControllerPublishVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const nodeNQN = "nqn.2014-08.org.nvmexpress:uuid:test-node-001"
	var capturedInitiatorID string
	env.agent.allowInitiatorFn = func(
		_ context.Context, req *agentv1.AllowInitiatorRequest,
	) (*agentv1.AllowInitiatorResponse, error) {
		capturedInitiatorID = req.GetInitiatorId()
		return &agentv1.AllowInitiatorResponse{}, nil
	}

	resp, err := env.srv.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeNQN,
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume: unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil")
	}
	if capturedInitiatorID != nodeNQN {
		t.Errorf("initiator_id = %q, want %q", capturedInitiatorID, nodeNQN)
	}
	if env.agent.allowInitiatorCalls != 1 {
		t.Errorf("agent.AllowInitiator calls = %d, want 1", env.agent.allowInitiatorCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerPublishVolume_AlreadyPublished
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerPublishVolume_AlreadyPublished verifies that a
// second ControllerPublishVolume for the same volume/node is idempotent:
// AllowInitiator is called again (the agent is idempotent) and returns success.
func TestCSIController_ControllerPublishVolume_AlreadyPublished(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	pubReq := &csipb.ControllerPublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "nqn.2014-08.org.nvmexpress:uuid:node-abc",
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	if _, err := env.srv.ControllerPublishVolume(ctx, pubReq); err != nil {
		t.Fatalf("first ControllerPublishVolume: %v", err)
	}
	if _, err := env.srv.ControllerPublishVolume(ctx, pubReq); err != nil {
		t.Fatalf("second ControllerPublishVolume (idempotent): %v", err)
	}
	if env.agent.allowInitiatorCalls != 2 {
		t.Errorf("agent.AllowInitiator calls = %d, want 2", env.agent.allowInitiatorCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerUnpublishVolume_Success
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerUnpublishVolume_Success verifies that
// ControllerUnpublishVolume calls agent.DenyInitiator.
func TestCSIController_ControllerUnpublishVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const nodeNQN = "nqn.2014-08.org.nvmexpress:uuid:test-node-001"
	var capturedInitiatorID string
	env.agent.denyInitiatorFn = func(
		_ context.Context, req *agentv1.DenyInitiatorRequest,
	) (*agentv1.DenyInitiatorResponse, error) {
		capturedInitiatorID = req.GetInitiatorId()
		return &agentv1.DenyInitiatorResponse{}, nil
	}

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   nodeNQN,
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: unexpected error: %v", err)
	}
	if capturedInitiatorID != nodeNQN {
		t.Errorf("initiator_id = %q, want %q", capturedInitiatorID, nodeNQN)
	}
	if env.agent.denyInitiatorCalls != 1 {
		t.Errorf("agent.DenyInitiator calls = %d, want 1", env.agent.denyInitiatorCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished verifies that
// if the agent returns NotFound for DenyInitiator, ControllerUnpublishVolume
// returns success (idempotent per CSI spec §4.3.4).
func TestCSIController_ControllerUnpublishVolume_AlreadyUnpublished(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.denyInitiatorFn = func(
		_ context.Context, _ *agentv1.DenyInitiatorRequest,
	) (*agentv1.DenyInitiatorResponse, error) {
		return nil, status.Error(codes.NotFound, "initiator not found")
	}

	_, err := env.srv.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: expectedCSIVolumeID,
		NodeId:   "nqn.2014-08.org.nvmexpress:uuid:node-abc",
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: expected success for NotFound, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ExpandVolume_Success
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ExpandVolume_Success verifies the happy-path expand:
// agent.ExpandVolume is called with the requested bytes, and the response
// has NodeExpansionRequired = true.
func TestCSIController_ExpandVolume_Success(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	const newBytes = int64(20 << 30) // 20 GiB
	env.agent.expandVolumeFn = func(
		_ context.Context, req *agentv1.ExpandVolumeRequest,
	) (*agentv1.ExpandVolumeResponse, error) {
		return &agentv1.ExpandVolumeResponse{CapacityBytes: req.GetRequestedBytes()}, nil
	}

	resp, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: newBytes},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != newBytes {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), newBytes)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired = false, want true")
	}
	if env.agent.expandVolumeCalls != 1 {
		t.Errorf("agent.ExpandVolume calls = %d, want 1", env.agent.expandVolumeCalls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ExpandVolume_AgentError
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ExpandVolume_AgentError verifies that an agent error
// (e.g., shrink rejected) is propagated with the correct gRPC code.
func TestCSIController_ExpandVolume_AgentError(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	env.agent.expandVolumeFn = func(
		_ context.Context, _ *agentv1.ExpandVolumeRequest,
	) (*agentv1.ExpandVolumeResponse, error) {
		return nil, status.Error(codes.InvalidArgument, "cannot shrink volume")
	}

	_, err := env.srv.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId:      expectedCSIVolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 512},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ValidateVolumeCapabilities_Supported
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ValidateVolumeCapabilities_Supported verifies that
// supported access modes are echoed back in the Confirmed field.
func TestCSIController_ValidateVolumeCapabilities_Supported(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	supported := []csipb.VolumeCapability_AccessMode_Mode{
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	}

	for _, mode := range supported {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			resp, err := env.srv.ValidateVolumeCapabilities(ctx, &csipb.ValidateVolumeCapabilitiesRequest{
				VolumeId: expectedCSIVolumeID,
				VolumeCapabilities: []*csipb.VolumeCapability{
					{
						AccessType: &csipb.VolumeCapability_Block{
							Block: &csipb.VolumeCapability_BlockVolume{},
						},
						AccessMode: &csipb.VolumeCapability_AccessMode{Mode: mode},
					},
				},
			})
			if err != nil {
				t.Fatalf("ValidateVolumeCapabilities: unexpected error: %v", err)
			}
			if resp.GetConfirmed() == nil {
				t.Errorf("mode %v: Confirmed is nil, want echo of capabilities", mode)
			}
			if resp.GetMessage() != "" {
				t.Errorf("mode %v: Message = %q, want empty (all supported)", mode, resp.GetMessage())
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_ValidateVolumeCapabilities_Unsupported
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_ValidateVolumeCapabilities_Unsupported verifies that
// unsupported access modes (e.g. MULTI_NODE_MULTI_WRITER) return a response
// with an empty Confirmed field and a non-empty Message.
func TestCSIController_ValidateVolumeCapabilities_Unsupported(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	unsupported := []csipb.VolumeCapability_AccessMode_Mode{
		csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
	}

	for _, mode := range unsupported {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			resp, err := env.srv.ValidateVolumeCapabilities(ctx, &csipb.ValidateVolumeCapabilitiesRequest{
				VolumeId: expectedCSIVolumeID,
				VolumeCapabilities: []*csipb.VolumeCapability{
					{
						AccessType: &csipb.VolumeCapability_Block{
							Block: &csipb.VolumeCapability_BlockVolume{},
						},
						AccessMode: &csipb.VolumeCapability_AccessMode{Mode: mode},
					},
				},
			})
			if err != nil {
				t.Fatalf("ValidateVolumeCapabilities: unexpected error: %v", err)
			}
			if resp.GetConfirmed() != nil {
				t.Errorf("mode %v: Confirmed is non-nil, want nil for unsupported mode", mode)
			}
			if resp.GetMessage() == "" {
				t.Errorf("mode %v: Message is empty, want explanation", mode)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_GetCapabilities
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_GetCapabilities verifies that ControllerGetCapabilities
// includes the expected RPC types.
func TestCSIController_GetCapabilities(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	resp, err := env.srv.ControllerGetCapabilities(ctx, &csipb.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}

	wantRPCTypes := map[csipb.ControllerServiceCapability_RPC_Type]bool{
		csipb.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME:     true,
		csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME: true,
		csipb.ControllerServiceCapability_RPC_EXPAND_VOLUME:            true,
	}

	for _, cap := range resp.GetCapabilities() {
		delete(wantRPCTypes, cap.GetRpc().GetType())
	}

	for missing := range wantRPCTypes {
		t.Errorf("capability %v missing from ControllerGetCapabilities response", missing)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestCSIController_CreateVolume_ACLToggle
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIController_CreateVolume_ACLDisabled verifies that the controller
// passes AclEnabled=false to agent.ExportVolume when the StorageClass
// parameter "pillar-csi.bhyoo.com/acl-enabled" is set to "false".
//
// This corresponds to PillarProtocol.spec.nvmeofTcp.acl = false, which tells
// the agent to set attr_allow_any_host=1 so any initiator may connect without
// an explicit AllowInitiator call.
func TestCSIController_CreateVolume_ACLDisabled(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	var capturedExportReq *agentv1.ExportVolumeRequest
	env.agent.exportVolumeFn = func(
		_ context.Context, req *agentv1.ExportVolumeRequest,
	) (*agentv1.ExportVolumeResponse, error) {
		capturedExportReq = req
		return &agentv1.ExportVolumeResponse{
			ExportInfo: &agentv1.ExportInfo{
				TargetId:  "nqn.2026-01.com.pillar-csi:acl-off-vol",
				Address:   "192.168.1.10",
				Port:      4420,
				VolumeRef: "1",
			},
		}, nil
	}

	req := baseCSICreateVolumeRequest()
	req.Parameters["pillar-csi.bhyoo.com/acl-enabled"] = "false"

	_, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}

	if capturedExportReq == nil {
		t.Fatal("agent.ExportVolume was not called")
	}
	if capturedExportReq.GetAclEnabled() {
		t.Errorf("ExportVolumeRequest.AclEnabled = true, want false when acl-enabled param is %q",
			"false")
	}
}

// TestCSIController_CreateVolume_ACLEnabled_Default verifies that the
// controller defaults to AclEnabled=true when the acl-enabled StorageClass
// parameter is absent (maintaining backward compatibility).
func TestCSIController_CreateVolume_ACLEnabled_Default(t *testing.T) {
	t.Parallel()
	env := newCSIControllerTestEnv(t)
	ctx := context.Background()

	var capturedExportReq *agentv1.ExportVolumeRequest
	env.agent.exportVolumeFn = func(
		_ context.Context, req *agentv1.ExportVolumeRequest,
	) (*agentv1.ExportVolumeResponse, error) {
		capturedExportReq = req
		return &agentv1.ExportVolumeResponse{
			ExportInfo: &agentv1.ExportInfo{
				TargetId:  "nqn.2026-01.com.pillar-csi:acl-default-vol",
				Address:   "192.168.1.10",
				Port:      4420,
				VolumeRef: "1",
			},
		}, nil
	}

	// Base request has no acl-enabled parameter — ACL should default to true.
	_, err := env.srv.CreateVolume(ctx, baseCSICreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume: unexpected error: %v", err)
	}

	if capturedExportReq == nil {
		t.Fatal("agent.ExportVolume was not called")
	}
	if !capturedExportReq.GetAclEnabled() {
		t.Errorf("ExportVolumeRequest.AclEnabled = false, want true (default) when acl-enabled param is absent")
	}
}
