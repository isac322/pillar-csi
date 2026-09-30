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
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
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

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/testutil/fakeuid"
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

	// Responses for ImportVolume (the import-zvol annotation path).
	importVolumeResp *agentv1.ImportVolumeResponse
	importVolumeErr  error

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

	// Responses for SetLocalAttach; every request is recorded in order.
	setLocalAttachErr   error
	setLocalAttachCalls []*agentv1.SetLocalAttachRequest

	// Call counters — verified by tests.
	createVolumeCalls   int
	importVolumeCalls   int
	exportVolumeCalls   int
	unexportVolumeCalls int
	deleteVolumeCalls   int
	getCapacityCalls    int
	expandVolumeCalls   int
	allowInitiatorCalls int
	denyInitiatorCalls  int

	// callOrder records the RPC method names in invocation order so tests can
	// assert the relative ordering of agent calls (e.g. an unfence before the
	// grant).
	callOrder []string

	// lastCreateVolumeReq captures the most recent CreateVolume request for
	// assertion on backend/export params in annotation integration tests.
	lastCreateVolumeReq *agentv1.CreateVolumeRequest
	// lastExportVolumeReq captures the most recent ExportVolume request.
	lastExportVolumeReq *agentv1.ExportVolumeRequest
	// lastImportVolumeReq captures the most recent ImportVolume request.
	lastImportVolumeReq *agentv1.ImportVolumeRequest
	// lastGetCapacityReq captures the most recent GetCapacity request.
	lastGetCapacityReq *agentv1.GetCapacityRequest
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

// ImportVolume returns the configured import response.  Tests exercising the
// import annotation set importVolumeResp / importVolumeErr and inspect
// lastImportVolumeReq.
func (m *mockAgentClient) ImportVolume(
	_ context.Context,
	req *agentv1.ImportVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.ImportVolumeResponse, error) {
	m.importVolumeCalls++
	m.lastImportVolumeReq = req
	if m.importVolumeErr != nil {
		return nil, m.importVolumeErr
	}
	if m.importVolumeResp != nil {
		return m.importVolumeResp, nil
	}
	return &agentv1.ImportVolumeResponse{
		DevicePath:    "/dev/zvol/" + req.GetVolumeId(),
		CapacityBytes: req.GetCapacityBytes(),
	}, nil
}

func (m *mockAgentClient) ExportVolume(
	_ context.Context,
	req *agentv1.ExportVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	m.exportVolumeCalls++
	m.lastExportVolumeReq = req
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
	req *agentv1.GetCapacityRequest,
	_ ...grpc.CallOption,
) (*agentv1.GetCapacityResponse, error) {
	m.getCapacityCalls++
	m.lastGetCapacityReq = req
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
	m.callOrder = append(m.callOrder, "AllowInitiator")
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
	m.callOrder = append(m.callOrder, "DenyInitiator")
	m.lastDenyInitiator = req
	if m.denyInitiatorErr != nil {
		return nil, m.denyInitiatorErr
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}
func (m *mockAgentClient) SetLocalAttach(
	_ context.Context,
	req *agentv1.SetLocalAttachRequest,
	_ ...grpc.CallOption,
) (*agentv1.SetLocalAttachResponse, error) {
	m.setLocalAttachCalls = append(m.setLocalAttachCalls, req)
	m.callOrder = append(m.callOrder, "SetLocalAttach")
	if m.setLocalAttachErr != nil {
		return nil, m.setLocalAttachErr
	}
	if !req.GetLocal() {
		return &agentv1.SetLocalAttachResponse{}, nil
	}
	return &agentv1.SetLocalAttachResponse{DevicePath: "/dev/zvol/" + req.GetVolumeId()}, nil
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
func (*mockAgentClient) Drain(
	_ context.Context,
	_ *agentv1.DrainRequest,
	_ ...grpc.CallOption,
) (*agentv1.DrainResponse, error) {
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

// Names of the configuration CRs every controller test environment seeds.
const (
	testStoreName    = "tank"        // ZFS store: pool "tank" on agent storage-node-1
	testLVMStoreName = "vg-store"    // LVM store: volume group "data-vg", thin pool "thin-pool-0"
	testProtocolName = "nvme"        // PillarProtocol with an nvmeofTcp member (acl unset: false)
	testACLProtocol  = "nvme-acl-on" // PillarProtocol with nvmeofTcp.acl true
)

// testConfigObjects returns fresh copies of the PillarStore / PillarProtocol
// CRs a hand-written StorageClass (store-ref / protocol-ref) resolves.
func testConfigObjects() []ctrlclient.Object {
	return []ctrlclient.Object{
		&v1alpha1.PillarStore{
			ObjectMeta: metav1.ObjectMeta{Name: testStoreName},
			Spec: v1alpha1.PillarStoreSpec{
				AgentRef: "storage-node-1",
				Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
					VolumeType: v1alpha1.ZFSVolumeTypeZvol,
					Pool:       "tank",
				}},
			},
		},
		&v1alpha1.PillarStore{
			ObjectMeta: metav1.ObjectMeta{Name: testLVMStoreName},
			Spec: v1alpha1.PillarStoreSpec{
				AgentRef: "storage-node-1",
				Backend: v1alpha1.BackendSpec{LVM: &v1alpha1.LVMBackendConfig{
					VolumeGroup: "data-vg",
					ThinPool:    "thin-pool-0",
				}},
			},
		},
		&v1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: testProtocolName},
			Spec: v1alpha1.PillarProtocolSpec{
				Protocol: v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4420}},
			},
		},
		&v1alpha1.PillarProtocol{
			ObjectMeta: metav1.ObjectMeta{Name: testACLProtocol},
			Spec: v1alpha1.PillarProtocolSpec{
				Protocol: v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4420, ACL: true}},
			},
		},
	}
}

// newControllerTestEnv builds a ControllerServer backed by:
//   - a controller-runtime fake k8s client seeded with one PillarAgent
//     that reports ResolvedAddress = "192.168.1.10:9500" and the
//     configuration CRs of testConfigObjects
//   - a mockAgentClient injected via the AgentDialer
//
// Extra objects (e.g. a PillarStorageClass binding) are seeded as well.
func newControllerTestEnv(t *testing.T, extra ...ctrlclient.Object) *controllerTestEnv {
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

	// Seed the fake client with a ready PillarAgent.
	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "storage-node-1",
		},
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{
				Address: "192.168.1.10",
				Port:    9500,
			},
		},
		Status: v1alpha1.PillarAgentStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	objs := append(testConfigObjects(), target)
	objs = append(objs, extra...)
	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
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

// seedPillarVolumeState creates a stub PillarVolumeState CRD with the given name so
// that lookups in code paths that gate on volume existence (e.g.
// ValidateVolumeCapabilities, ControllerPublishVolume) see the object.  The
// stub carries the metadata Name and the spec.agentVolumeID a provisioned
// volume carries ("<pool>/<name>", pool tank in these tests): the owner
// resolution used by every post-create RPC matches on it, so a seed without
// richer status fields should patch the object directly after seeding.  The
// seed carries the agent/backend/protocol fields a real lifecycle always
// records: backend volume IDs are only unique per agent, so ownership
// matching ignores a state that names a different agent.
func seedPillarVolumeState(t *testing.T, env *controllerTestEnv, name string) {
	t.Helper()
	pv := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PillarVolumeStateSpec{
			AgentVolumeID: "tank/" + name,
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
		},
	}
	if err := env.srv.k8sClient.Create(context.Background(), pv); err != nil {
		t.Fatalf("seed PillarVolumeState %q: %v", name, err)
	}
}

// baseCreateVolumeRequest returns a minimal valid CreateVolumeRequest from a
// hand-written StorageClass naming the seeded ZFS store and NVMe-oF protocol.
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
			paramStoreRef:    testStoreName,
			paramProtocolRef: testProtocolName,
		},
	}
}

// loadResolved returns spec.resolved of the named volume's PillarVolumeState.
func loadResolved(t *testing.T, env *controllerTestEnv, name string) *v1alpha1.ResolvedVolumeConfig {
	t.Helper()
	pvs, _, err := env.srv.loadPillarVolumeState(context.Background(), name)
	if err != nil {
		t.Fatalf("load PillarVolumeState %q: %v", name, err)
	}
	if pvs.Spec.Resolved == nil {
		t.Fatalf("PillarVolumeState %q: spec.resolved is nil", name)
	}
	return pvs.Spec.Resolved
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

// TestCreateVolume_FirstCallResolvedDefaults verifies that a first call
// provisions from the store's zfs member and the protocol's nvmeofTcp member
// with every default applied (ACL off, ext4) and persists that effective
// configuration in spec.resolved.
func TestCreateVolume_FirstCallResolvedDefaults(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	resp, err := env.srv.CreateVolume(context.Background(), baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}

	zfs := env.agent.lastCreateVolumeReq.GetBackendParams().GetZfs()
	if zfs == nil || zfs.GetPool() != "tank" {
		t.Errorf("BackendParams.Zfs = %+v, want pool tank", zfs)
	}
	exp := env.agent.lastExportVolumeReq
	if exp.GetExportParams().GetNvmeofTcp().GetPort() != 4420 {
		t.Errorf("ExportParams.NvmeofTcp.Port = %d, want 4420", exp.GetExportParams().GetNvmeofTcp().GetPort())
	}
	if exp.GetAclEnabled() {
		t.Error("ExportVolume AclEnabled = true, want false (acl defaults to false)")
	}

	// Nothing sets a filesystem: the node still learns the ext4 default.
	if got := resp.GetVolume().GetVolumeContext()[paramFSType]; got != "ext4" {
		t.Errorf("VolumeContext[%s] = %q, want the ext4 default", paramFSType, got)
	}

	// The effective configuration is persisted for retries and restore.
	resolved := loadResolved(t, env, "pvc-abc123")
	if resolved.Backend.ZFS == nil || resolved.Backend.ZFS.Pool != "tank" {
		t.Errorf("spec.resolved.backend = %+v, want zfs pool tank", resolved.Backend)
	}
	if resolved.Protocol.NVMeOFTCP == nil || resolved.Protocol.NVMeOFTCP.ACL {
		t.Errorf("spec.resolved.protocol = %+v, want nvmeofTcp with acl=false", resolved.Protocol)
	}
}

// TestCreateVolume_VolumeContextCarriesNVMeoFReconnectTuning verifies that
// the merged ctrl_loss_tmo / reconnect_delay parameters reach the node via the
// VolumeContext (explicit ctrl_loss_tmo=0 preserved), that an idempotent retry
// returns the same keys, and that unset parameters stay absent so kernel
// defaults apply.
func TestCreateVolume_VolumeContextCarriesNVMeoFReconnectTuning(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  ctrlLossTmo: 0\n  reconnectDelay: 5\n"

	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := env.srv.CreateVolume(ctx, req)
		if err != nil {
			t.Fatalf("attempt %d: CreateVolume: %v", attempt, err)
		}
		vc := resp.GetVolume().GetVolumeContext()
		if vc[paramNVMeOFCtrlLossTmo] != "0" || vc[paramNVMeOFReconnectDelay] != "5" {
			t.Fatalf("attempt %d: VolumeContext tuning = (%q, %q), want (\"0\", \"5\")",
				attempt, vc[paramNVMeOFCtrlLossTmo], vc[paramNVMeOFReconnectDelay])
		}
	}

	unsetEnv := newControllerTestEnv(t)
	resp, err := unsetEnv.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume without tuning: %v", err)
	}
	vc := resp.GetVolume().GetVolumeContext()
	for _, k := range []string{paramNVMeOFCtrlLossTmo, paramNVMeOFReconnectDelay} {
		if _, ok := vc[k]; ok {
			t.Errorf("VolumeContext must not carry %q when unset", k)
		}
	}
}

// TestCreateVolume_MalformedNVMeoFTuning_RejectedBeforeProvisioning verifies
// that a non-integer reconnect tuning value fails CreateVolume with
// InvalidArgument before any agent call, instead of provisioning a PV whose
// immutable VolumeContext could never be staged.
func TestCreateVolume_MalformedNVMeoFTuning_RejectedBeforeProvisioning(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  ctrlLossTmo: 10m\n"

	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume code = %v (err %v), want InvalidArgument", status.Code(err), err)
	}
	if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
		t.Fatalf("agent must not be called: create=%d export=%d",
			env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
	}
}

// TestCreateVolume_NVMeoFQueueAndInCapsuleSize verifies that the merged
// max-queue-size reaches the node through the VolumeContext (it becomes the
// fabrics queue_size), that the in-capsule data size reaches the agent in
// ExportVolume and the durable exportSpec used by export restore, and that
// both stay unset when not configured so the kernel defaults apply.
func TestCreateVolume_NVMeoFQueueAndInCapsuleSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := newControllerTestEnv(t)
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  maxQueueSize: 64\n  inCapsuleDataSize: 8192\n"
	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vc := resp.GetVolume().GetVolumeContext()
	if vc[paramNVMeOFMaxQueueSize] != "64" {
		t.Errorf("VolumeContext[%s] = %q, want \"64\"", paramNVMeOFMaxQueueSize, vc[paramNVMeOFMaxQueueSize])
	}
	if got := env.agent.lastExportVolumeReq.GetExportParams().GetNvmeofTcp().GetInCapsuleDataSize(); got != 8192 {
		t.Errorf("ExportVolume in_capsule_data_size = %d, want 8192", got)
	}
	pvs, _, err := env.srv.loadPillarVolumeState(ctx, req.GetName())
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if spec := pvs.Status.ExportSpec; spec == nil || spec.InCapsuleDataSize == nil || *spec.InCapsuleDataSize != 8192 {
		t.Errorf("status.exportSpec = %+v, want inCapsuleDataSize 8192", spec)
	}

	unsetEnv := newControllerTestEnv(t)
	resp, err = unsetEnv.srv.CreateVolume(ctx, baseCreateVolumeRequest())
	if err != nil {
		t.Fatalf("CreateVolume without tuning: %v", err)
	}
	if _, ok := resp.GetVolume().GetVolumeContext()[paramNVMeOFMaxQueueSize]; ok {
		t.Errorf("VolumeContext must not carry %s when unset", paramNVMeOFMaxQueueSize)
	}
	if got := unsetEnv.agent.lastExportVolumeReq.GetExportParams().GetNvmeofTcp().GetInCapsuleDataSize(); got != 0 {
		t.Errorf("ExportVolume in_capsule_data_size = %d, want unset (0)", got)
	}
	pvs, _, err = unsetEnv.srv.loadPillarVolumeState(ctx, baseCreateVolumeRequest().GetName())
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if spec := pvs.Status.ExportSpec; spec == nil || spec.InCapsuleDataSize != nil {
		t.Errorf("status.exportSpec = %+v, want no inCapsuleDataSize", spec)
	}
}

// TestCreateVolume_PartialRetryRecordsCorrectedInCapsuleSize verifies that a
// CreateVolume retry after a failed export (e.g. an in-capsule data size
// conflicting with the shared port) records the export settings it retries
// with, so export restore re-creates the export the volume actually has.
func TestCreateVolume_PartialRetryRecordsCorrectedInCapsuleSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newControllerTestEnv(t)

	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  inCapsuleDataSize: 4096\n"
	env.agent.exportVolumeErr = status.Error(codes.FailedPrecondition, "port in-capsule data size conflict")
	if _, err := env.srv.CreateVolume(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("first CreateVolume err = %v, want FailedPrecondition", err)
	}

	env.agent.exportVolumeErr = nil
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  inCapsuleDataSize: 8192\n"
	if _, err := env.srv.CreateVolume(ctx, req); err != nil {
		t.Fatalf("retried CreateVolume: %v", err)
	}
	if env.agent.createVolumeCalls != 1 {
		t.Fatalf("backend created %d times, want once (retry only re-exports)", env.agent.createVolumeCalls)
	}
	pvs, _, err := env.srv.loadPillarVolumeState(ctx, req.GetName())
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if spec := pvs.Status.ExportSpec; spec == nil || spec.InCapsuleDataSize == nil || *spec.InCapsuleDataSize != 8192 {
		t.Errorf("status.exportSpec = %+v, want the retried inCapsuleDataSize 8192", spec)
	}
}

// TestCreateVolume_PartialRetryKeepsResolvedBackend verifies that once the
// first attempt persisted spec.resolved (CreatePartial), a retry re-resolves
// only the protocol axis from the live CRs: the backend and agentRef come
// from spec.resolved and the PillarStore is not re-loaded at all.
func TestCreateVolume_PartialRetryKeepsResolvedBackend(t *testing.T) {
	t.Parallel()
	for _, mutate := range []struct {
		name  string
		patch func(t *testing.T, env *controllerTestEnv)
	}{
		{
			name: "store mutated to an invalid config",
			patch: func(t *testing.T, env *controllerTestEnv) {
				t.Helper()
				store := &v1alpha1.PillarStore{}
				if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(testLVMStoreName), store); err != nil {
					t.Fatalf("get store: %v", err)
				}
				// thin without a thinPool would fail resolution if the
				// store were re-loaded.
				store.Spec.Backend.LVM.ThinPool = ""
				store.Spec.Backend.LVM.ProvisioningMode = v1alpha1.LVMProvisioningModeThin
				if err := env.srv.k8sClient.Update(context.Background(), store); err != nil {
					t.Fatalf("update store: %v", err)
				}
			},
		},
		{
			name: "store deleted",
			patch: func(t *testing.T, env *controllerTestEnv) {
				t.Helper()
				store := &v1alpha1.PillarStore{ObjectMeta: metav1.ObjectMeta{Name: testLVMStoreName}}
				if err := env.srv.k8sClient.Delete(context.Background(), store); err != nil {
					t.Fatalf("delete store: %v", err)
				}
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			ctx := context.Background()
			req := baseCreateVolumeRequest()
			req.Parameters[paramStoreRef] = testLVMStoreName

			env.agent.exportVolumeErr = status.Error(codes.Unavailable, "agent restarting")
			if _, err := env.srv.CreateVolume(ctx, req); status.Code(err) != codes.Unavailable {
				t.Fatalf("first CreateVolume err = %v, want Unavailable", err)
			}

			// The protocol axis is re-resolved (new values win) while the
			// backend axis must stay the first attempt's linear mode even
			// though the store can no longer be loaded as resolved.
			mutate.patch(t, env)
			patchNVMeoFProtocol(t, env, func(nvme *v1alpha1.NVMeOFTCPConfig) {
				nvme.MaxQueueSize = testInt32(64)
				nvme.InCapsuleDataSize = testInt32(8192)
			})
			env.agent.exportVolumeErr = nil
			assertPartialRetryReusesBackend(t, env, req)
		})
	}
}

// patchNVMeoFProtocol applies mutate to the seeded PillarProtocol's
// nvmeofTcp member.
func patchNVMeoFProtocol(t *testing.T, env *controllerTestEnv, mutate func(*v1alpha1.NVMeOFTCPConfig)) {
	t.Helper()
	proto := &v1alpha1.PillarProtocol{}
	if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(testProtocolName), proto); err != nil {
		t.Fatalf("get protocol: %v", err)
	}
	mutate(proto.Spec.Protocol.NVMeOFTCP)
	if err := env.srv.k8sClient.Update(context.Background(), proto); err != nil {
		t.Fatalf("update protocol: %v", err)
	}
}

// assertPartialRetryReusesBackend verifies the retry of req: it must succeed
// without a second backend CreateVolume, keep spec.resolved.backend at the
// first attempt's linear LVM mode, send the protocol's new export value to
// the agent, and keep the node-side VolumeContext from spec.resolved.
func assertPartialRetryReusesBackend(t *testing.T, env *controllerTestEnv, req *csi.CreateVolumeRequest) {
	t.Helper()
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("partial retry: %v", err)
	}
	if env.agent.createVolumeCalls != 1 {
		t.Fatalf("backend created %d times, want once (retry only re-exports)", env.agent.createVolumeCalls)
	}
	resolved := loadResolved(t, env, req.GetName())
	if resolved.Backend.LVM == nil || string(resolved.Backend.LVM.ProvisioningMode) != "linear" {
		t.Errorf("spec.resolved.backend.lvm = %+v, want the first attempt's linear mode", resolved.Backend.LVM)
	}
	// Node-side connect options stay the first attempt's (no maxQueueSize).
	if got, ok := resp.GetVolume().GetVolumeContext()[paramNVMeOFMaxQueueSize]; ok {
		t.Errorf("VolumeContext[%s] = %q, want absent (fixed by spec.resolved)", paramNVMeOFMaxQueueSize, got)
	}
	exp := env.agent.lastExportVolumeReq.GetExportParams().GetNvmeofTcp()
	if exp.GetInCapsuleDataSize() != 8192 {
		t.Errorf("ExportParams.InCapsuleDataSize = %d, want the retried 8192", exp.GetInCapsuleDataSize())
	}
}

// TestCreateVolume_InvalidNVMeoFQueueOrInCapsuleSize_RejectedBeforeProvisioning
// verifies that a queue size the kernel would reject and a malformed or
// negative in-capsule data size fail CreateVolume with InvalidArgument
// before any agent call.
func TestCreateVolume_InvalidNVMeoFQueueOrInCapsuleSize_RejectedBeforeProvisioning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ key, value string }{
		{"maxQueueSize", "8"},
		{"maxQueueSize", "2048"},
		{"inCapsuleDataSize", "-1"},
		{"inCapsuleDataSize", "0"},
		{"inCapsuleDataSize", "1023"},
		{"inCapsuleDataSize", "16K"},
	} {
		env := newControllerTestEnv(t)
		req := baseCreateVolumeRequest()
		req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  " + tc.key + ": " + tc.value + "\n"
		_, err := env.srv.CreateVolume(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%s: CreateVolume err = %v, want InvalidArgument naming the key", tc.key, tc.value, err)
		}
		if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
			t.Errorf("%s=%s: agent must not be called: create=%d export=%d", tc.key, tc.value,
				env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
		}
	}
}

func TestValidateVolumeCapabilities_BlockProtocolRejectsRWX(t *testing.T) {
	t.Parallel()

	srv := &ControllerServer{}
	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123",
		VolumeContext: map[string]string{
			vcProtocolType: string(v1alpha1.ProtocolIDNVMeOFTCP),
		},
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Block{
					Block: &csi.VolumeCapability_BlockVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
	}

	resp, err := srv.ValidateVolumeCapabilities(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities unexpected error: %v", err)
	}
	if resp.GetConfirmed() != nil {
		t.Fatal("ValidateVolumeCapabilities unexpectedly confirmed block protocol RWX")
	}
	if resp.GetMessage() == "" {
		t.Fatal("ValidateVolumeCapabilities rejection message is empty")
	}
}

func TestCreateVolume_BlockProtocolRejectsRWX(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	ctx := context.Background()
	req := baseCreateVolumeRequest()
	req.VolumeCapabilities[0].GetAccessMode().Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER

	_, err := env.srv.CreateVolume(ctx, req)
	if err == nil {
		t.Fatal("expected InvalidArgument for block protocol RWX, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want %v", st.Code(), codes.InvalidArgument)
	}
	if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
		t.Errorf("agent was contacted despite block protocol RWX validation failure")
	}
}

// TestCreateVolume_AccessModeRevalidatedOnReadyRetry verifies that access
// modes are validated even when the volume is already Ready: a same-name
// retry must not take the completed fast path before the mode check.
func TestCreateVolume_AccessModeRevalidatedOnReadyRetry(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	if _, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest()); err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	req := baseCreateVolumeRequest()
	req.VolumeCapabilities[0].GetAccessMode().Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	_, err := env.srv.CreateVolume(ctx, req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("retried CreateVolume err = %v, want InvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "access mode") {
		t.Errorf("error %q does not name the access mode", err)
	}
	if env.agent.exportVolumeCalls != 1 {
		t.Errorf("exportVolumeCalls = %d, want 1 (the retry must not reach the agent)", env.agent.exportVolumeCalls)
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
// device path stored in the PillarVolumeState CRD during a CreatePartial transition
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

	// Verify PillarVolumeState CRD was created with the device path.
	pv := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(ctx,
		ctrlKey("pvc-abc123"), pv); err != nil {
		t.Fatalf("get PillarVolumeState after first attempt: %v", err)
	}
	if pv.Status.BackendDevicePath != wantDevicePath {
		t.Errorf("PillarVolumeState.Status.BackendDevicePath = %q, want %q",
			pv.Status.BackendDevicePath, wantDevicePath)
	}
	if pv.Status.Phase != v1alpha1.PillarVolumeStatePhaseCreatePartial {
		t.Errorf("PillarVolumeState.Status.Phase = %q, want CreatePartial", pv.Status.Phase)
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
		t.Fatalf("get PillarVolumeState after retry: %v", err)
	}
	if pv.Status.BackendDevicePath != "" {
		t.Errorf("BackendDevicePath not cleared after reaching Ready: %q",
			pv.Status.BackendDevicePath)
	}
}

// TestCreateVolume_ValidationErrors checks that malformed requests and
// StorageClass parameters are rejected before any agent dial is attempted:
// InvalidArgument for request/parameter errors, FailedPrecondition when a
// referenced configuration CR does not exist.
func TestCreateVolume_ValidationErrors(t *testing.T) {
	t.Parallel()
	withParams := func(mutate func(map[string]string)) *csi.CreateVolumeRequest {
		req := baseCreateVolumeRequest()
		mutate(req.Parameters)
		return req
	}
	tests := []struct {
		name     string
		req      *csi.CreateVolumeRequest
		code     codes.Code
		fragment string // required substring of the error message ("" = any)
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
			name: "no identity parameters",
			req:  withParams(func(p map[string]string) { clear(p) }),
			code: codes.InvalidArgument,
		},
		{
			name: "store-ref without protocol-ref",
			req:  withParams(func(p map[string]string) { delete(p, paramProtocolRef) }),
			code: codes.InvalidArgument,
		},
		{
			name: "protocol-ref without store-ref",
			req:  withParams(func(p map[string]string) { delete(p, paramStoreRef) }),
			code: codes.InvalidArgument,
		},
		{
			name: "binding and store refs both set",
			req:  withParams(func(p map[string]string) { p[paramBinding] = "some-binding" }),
			code: codes.InvalidArgument,
		},
		{
			name:     "unknown pillar-csi parameter",
			req:      withParams(func(p map[string]string) { p["pillar-csi.bhyoo.com/filesytem"] = "fsType: xfs" }),
			code:     codes.InvalidArgument,
			fragment: "pillar-csi.bhyoo.com/filesytem",
		},
		{
			name: "structural field in backend document",
			req: withParams(func(p map[string]string) {
				p[paramBackendDoc] = "zfs:\n  pool: other-pool\n"
			}),
			code:     codes.InvalidArgument,
			fragment: "zfs.pool is structural",
		},
		{
			name: "backend document member does not match the store",
			req: withParams(func(p map[string]string) {
				p[paramBackendDoc] = "lvm:\n  provisioningMode: thin\n"
			}),
			code: codes.InvalidArgument,
		},
		{
			name: "structural field in protocol document",
			req: withParams(func(p map[string]string) {
				p[paramProtocolDoc] = "nvmeofTcp:\n  acl: true\n"
			}),
			code:     codes.InvalidArgument,
			fragment: "nvmeofTcp.acl is structural",
		},
		{
			name: "filesystem document disagrees with csi fstype",
			req: withParams(func(p map[string]string) {
				p[paramFSTypeSC] = "ext4"
				p[paramFilesystemDoc] = "fsType: xfs\n"
			}),
			code: codes.InvalidArgument,
		},
		{
			name:     "unknown store",
			req:      withParams(func(p map[string]string) { p[paramStoreRef] = "no-such-store" }),
			code:     codes.FailedPrecondition,
			fragment: "no-such-store",
		},
		{
			name:     "unknown protocol",
			req:      withParams(func(p map[string]string) { p[paramProtocolRef] = "no-such-protocol" }),
			code:     codes.FailedPrecondition,
			fragment: "no-such-protocol",
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
				t.Errorf("error code = %v (%v), want %v", st.Code(), err, tc.code)
			}
			if tc.fragment != "" && !strings.Contains(st.Message(), tc.fragment) {
				t.Errorf("error %q does not mention %q", st.Message(), tc.fragment)
			}
			// No agent calls should have been made.
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Errorf("agent was contacted despite validation error")
			}
		})
	}
}

// TestCreateVolume_LegacyFlatParametersRejected verifies that the removed
// flat StorageClass vocabulary is rejected by name instead of silently
// ignored, so an old StorageClass cannot provision with unintended defaults.
func TestCreateVolume_LegacyFlatParametersRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"pillar-csi.bhyoo.com/backend-type",
		"pillar-csi.bhyoo.com/protocol-type",
		"pillar-csi.bhyoo.com/store",
		"pillar-csi.bhyoo.com/agent",
		"pillar-csi.bhyoo.com/zfs-parent-dataset",
		"pillar-csi.bhyoo.com/zfs-prop.compression",
		"pillar-csi.bhyoo.com/lvm-vg",
		"pillar-csi.bhyoo.com/lvm-thin-pool",
		"pillar-csi.bhyoo.com/lvm-mode",
		"pillar-csi.bhyoo.com/nvmeof-port",
		"pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo",
		"pillar-csi.bhyoo.com/nvmeof-in-capsule-data-size",
		"pillar-csi.bhyoo.com/acl-enabled",
		"pillar-csi.bhyoo.com/iscsi-port",
		"pillar-csi.bhyoo.com/nfs-version",
		"pillar-csi.bhyoo.com/fs-type",
		"pillar-csi.bhyoo.com/mkfs-options",
		"pillar-csi.bhyoo.com/param.lvm-mode",
	} {
		env := newControllerTestEnv(t)
		req := baseCreateVolumeRequest()
		req.Parameters[key] = "x"
		_, err := env.srv.CreateVolume(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: CreateVolume err = %v, want InvalidArgument naming the key", key, err)
		}
		if env.agent.createVolumeCalls != 0 {
			t.Errorf("%s: agent contacted despite rejected parameter", key)
		}
	}
}

// TestCreateVolume_RejectsVolumeContentSource verifies the CSI spec §5.1.1
// contract: a plugin that does not advertise CREATE_DELETE_SNAPSHOT or
// CLONE_VOLUME must reject any CreateVolume request carrying a
// volume_content_source with codes.InvalidArgument instead of silently
// provisioning an empty volume.  The agent must never be contacted.
func TestCreateVolume_RejectsVolumeContentSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source *csi.VolumeContentSource
	}{
		{
			name: "snapshot source",
			source: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Snapshot{
					Snapshot: &csi.VolumeContentSource_SnapshotSource{
						SnapshotId: "snap-1",
					},
				},
			},
		},
		{
			name: "clone source",
			source: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Volume{
					Volume: &csi.VolumeContentSource_VolumeSource{
						VolumeId: "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-src",
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newControllerTestEnv(t)
			req := baseCreateVolumeRequest()
			req.VolumeContentSource = tc.source

			_, err := env.srv.CreateVolume(context.Background(), req)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.InvalidArgument {
				t.Errorf("error code = %v, want InvalidArgument", st.Code())
			}
			// The rejection must happen before any agent RPC.
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Errorf("agent was contacted despite unsupported content source")
			}
		})
	}
}

// TestCreateVolume_ContentSourceRetryNotServedFromCache verifies that a retry
// carrying a volume_content_source for a name that already exists as an
// ordinary volume is rejected rather than served from the StateCreated
// idempotency cache (CSI spec §5.1.1: the existing volume is incompatible
// with the requested source).
func TestCreateVolume_ContentSourceRetryNotServedFromCache(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	// First create an ordinary volume so the name lands in StateCreated.
	if _, err := env.srv.CreateVolume(ctx, baseCreateVolumeRequest()); err != nil {
		t.Fatalf("initial CreateVolume: %v", err)
	}

	req := baseCreateVolumeRequest()
	req.VolumeContentSource = &csi.VolumeContentSource{
		Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "snap-1"},
		},
	}
	_, err := env.srv.CreateVolume(ctx, req)
	if err == nil {
		t.Fatal("expected error for content-source retry, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
	// Only the initial ordinary create may have reached the agent.
	if env.agent.createVolumeCalls != 1 {
		t.Errorf("agent.CreateVolume call count = %d, want 1", env.agent.createVolumeCalls)
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

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarAgentStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(append(testConfigObjects(), target)...).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}).
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
// resources like PillarVolumeState and PillarAgent use no namespace).
func ctrlKey(name string) types.NamespacedName {
	return types.NamespacedName{Name: name}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetCapacity tests
// ─────────────────────────────────────────────────────────────────────────────.

// baseGetCapacityRequest returns a minimal valid GetCapacityRequest from a
// hand-written StorageClass naming the seeded ZFS store.
func baseGetCapacityRequest() *csi.GetCapacityRequest {
	return &csi.GetCapacityRequest{
		Parameters: map[string]string{
			paramStoreRef:    testStoreName,
			paramProtocolRef: testProtocolName,
		},
	}
}

// TestGetCapacity_Success verifies the happy path: the controller resolves
// the store, dials its agent, asks for the store's pool and backend, and
// returns AvailableCapacity from the response.
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
	got := env.agent.lastGetCapacityReq
	if got.GetPoolName() != "tank" || got.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
		t.Errorf("agent GetCapacity request = %+v, want pool tank / ZFS_ZVOL", got)
	}
}

// TestGetCapacity_LVMStoreUsesVolumeGroup verifies that the pool asked for
// is the store's physical volume group, not the PillarStore name.
func TestGetCapacity_LVMStoreUsesVolumeGroup(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := baseGetCapacityRequest()
	req.Parameters[paramStoreRef] = testLVMStoreName

	if _, err := env.srv.GetCapacity(context.Background(), req); err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	got := env.agent.lastGetCapacityReq
	if got.GetPoolName() != "data-vg" || got.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM {
		t.Errorf("agent GetCapacity request = %+v, want pool data-vg / LVM", got)
	}
}

// TestGetCapacity_BindingIdentity verifies that a generated StorageClass
// (storage-class parameter only) resolves the store through the binding.
func TestGetCapacity_BindingIdentity(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding"},
		Spec:       v1alpha1.PillarStorageClassSpec{StoreRef: testLVMStoreName, ProtocolRef: testProtocolName},
	})
	req := &csi.GetCapacityRequest{Parameters: map[string]string{paramBinding: "lvm-binding"}}
	if _, err := env.srv.GetCapacity(context.Background(), req); err != nil {
		t.Fatalf("GetCapacity: %v", err)
	}
	if got := env.agent.lastGetCapacityReq.GetPoolName(); got != "data-vg" {
		t.Errorf("agent GetCapacity pool = %q, want data-vg", got)
	}
}

// TestGetCapacity_NoIdentityParams verifies that a request naming no store
// returns AvailableCapacity=0 with no error.  Per CSI spec §4.1.2 the
// parameters field is informational; the driver must not fail GetCapacity
// when it cannot resolve a pool from the supplied parameters, and instead
// reports zero so the CO knows no pool was selectable.
func TestGetCapacity_NoIdentityParams(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	for _, params := range []map[string]string{nil, {"csi.storage.k8s.io/fstype": "ext4"}} {
		resp, err := env.srv.GetCapacity(ctx, &csi.GetCapacityRequest{Parameters: params})
		if err != nil {
			t.Fatalf("params %v: expected no error, got %v", params, err)
		}
		if resp.GetAvailableCapacity() != 0 {
			t.Errorf("params %v: AvailableCapacity = %d, want 0", params, resp.GetAvailableCapacity())
		}
	}
	if env.agent.getCapacityCalls != 0 {
		t.Errorf("agent contacted %d times without a store", env.agent.getCapacityCalls)
	}
}

// TestGetCapacity_UnknownConfigCR verifies that a store or binding that does
// not exist returns codes.NotFound without contacting any agent.
func TestGetCapacity_UnknownConfigCR(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]map[string]string{
		"store":   {paramStoreRef: "no-such-store", paramProtocolRef: testProtocolName},
		"binding": {paramBinding: "no-such-binding"},
	} {
		env := newControllerTestEnv(t)
		_, err := env.srv.GetCapacity(context.Background(), &csi.GetCapacityRequest{Parameters: params})
		if status.Code(err) != codes.NotFound {
			t.Errorf("%s: GetCapacity err = %v, want NotFound", name, err)
		}
		if env.agent.getCapacityCalls != 0 {
			t.Errorf("%s: agent contacted for an unknown CR", name)
		}
	}
}

// TestGetCapacity_TargetNotFound verifies that a store whose agentRef names a
// non-existent PillarAgent returns codes.NotFound.
func TestGetCapacity_TargetNotFound(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, &v1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-store"},
		Spec: v1alpha1.PillarStoreSpec{
			AgentRef: "nonexistent-target",
			Backend:  v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{Pool: "tank"}},
		},
	})
	ctx := context.Background()

	req := baseGetCapacityRequest()
	req.Parameters[paramStoreRef] = "orphan-store"

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

// TestAgentResourceExhausted_Propagated verifies that an agent's
// ResourceExhausted (pool out of space) reaches the CO unchanged from both
// CreateVolume and ControllerExpandVolume, so the provisioner and resizer
// report insufficient capacity rather than an internal error (issue #99).
func TestAgentResourceExhausted_Propagated(t *testing.T) {
	t.Parallel()
	agentErr := status.Error(codes.ResourceExhausted, "pool tank is out of space")
	tests := []struct {
		name string
		call func(t *testing.T, env *controllerTestEnv) error
	}{
		{
			name: "CreateVolume",
			call: func(_ *testing.T, env *controllerTestEnv) error {
				env.agent.createVolumeErr = agentErr
				_, err := env.srv.CreateVolume(context.Background(), baseCreateVolumeRequest())
				return err
			},
		},
		{
			name: "ControllerExpandVolume",
			call: func(t *testing.T, env *controllerTestEnv) error {
				env.agent.expandVolumeErr = agentErr
				volumeID := expandableVolumeID(t, env, "tank/pvc-full")
				_, err := env.srv.ControllerExpandVolume(context.Background(),
					expandRequest(volumeID, 1<<40))
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(t, newControllerTestEnv(t))
			if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted {
				t.Fatalf("code = %v, want ResourceExhausted (err: %v)", st.Code(), err)
			}
			if !strings.Contains(err.Error(), "out of space") {
				t.Errorf("error %q lost the agent detail", err)
			}
		})
	}
}

// TestGetCapacity_TargetNoAddress verifies that a PillarAgent with an empty
// ResolvedAddress returns codes.Unavailable.
func TestGetCapacity_TargetNoAddress(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	// Create a PillarAgent with no ResolvedAddress.
	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarAgentStatus{
			ResolvedAddress: "", // empty — agent not ready
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(append(testConfigObjects(), target)...).
		WithStatusSubresource(&v1alpha1.PillarAgent{}).
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
// csi.storage.k8s.io/pvc/name and csi.storage.k8s.io/pvc/namespace keys
// (external-provisioner --extra-create-metadata) so that CreateVolume can
// look up the PVC and apply annotation overrides.
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

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Spec: v1alpha1.PillarAgentSpec{
			External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500},
		},
		Status: v1alpha1.PillarAgentStatus{
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
		WithInterceptorFuncs(fakeuid.Interceptor()).
		WithScheme(scheme).
		WithObjects(append(testConfigObjects(), target, pvc)...).
		WithStatusSubresource(&v1alpha1.PillarVolumeState{}, &v1alpha1.PillarAgent{}).
		Build()

	agent := &mockAgentClient{}
	dialer := func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, nopCloser{}, nil
	}
	srv := NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	// Build a CreateVolumeRequest that includes the claim metadata injected
	// by external-provisioner --extra-create-metadata.
	req := baseCreateVolumeRequest()
	req.Parameters[paramPVCNameMeta] = pvcName
	req.Parameters[paramPVCNamespaceMeta] = pvcNamespace

	return &controllerTestEnv{srv: srv, agent: agent, scheme: scheme}, req
}

// TestCreateVolume_PVCOverrideWinsForNVMeoFReconnectTuning verifies the
// precedence (PVC protocol document over the StorageClass protocol document)
// carries through to the VolumeContext the node connects with.
func TestCreateVolume_PVCOverrideWinsForNVMeoFReconnectTuning(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "tenant-a", "pvc-tuned", map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  ctrlLossTmo: 900\n",
	})
	req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  ctrlLossTmo: 1800\n  reconnectDelay: 5\n"

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vc := resp.GetVolume().GetVolumeContext()
	if got := vc[paramNVMeOFCtrlLossTmo]; got != "900" {
		t.Errorf("ctrl-loss-tmo = %q, want PVC override \"900\"", got)
	}
	if got := vc[paramNVMeOFReconnectDelay]; got != "5" {
		t.Errorf("reconnect-delay = %q, want StorageClass value \"5\"", got)
	}
}

// TestCreateVolume_PVCAnnotationOverride_ZFSProperty verifies the end-to-end
// PVC annotation override flow for a ZFS property (compression).
//
// Expected behavior:
//  1. The PVC carries "pillar-csi.bhyoo.com/backend" with zfs.properties.compression=zstd.
//  2. CreateVolume merges this document onto the store's backend.
//  3. The agent.CreateVolume request contains that ZFS property.
func TestCreateVolume_PVCAnnotationOverride_ZFSProperty(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		v1alpha1.AnnotationBackendDoc: `
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

// TestCreateVolume_PVCRemovedAnnotationsRejected verifies that the removed PVC
// annotation vocabulary (flat "param.<key>", the *-override documents) and
// unknown pillar-csi.bhyoo.com/ keys fail provisioning with InvalidArgument
// naming the annotation instead of being silently ignored.
func TestCreateVolume_PVCRemovedAnnotationsRejected(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"pillar-csi.bhyoo.com/param.zfs-prop.compression",
		"pillar-csi.bhyoo.com/backend-override",
		"pillar-csi.bhyoo.com/protocol-override",
		"pillar-csi.bhyoo.com/fs-override",
		"pillar-csi.bhyoo.com/fs-type",
	} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			env, req := newControllerTestEnvWithPVC(t, "default", "pvc-removed", map[string]string{
				key: "zfs:\n  properties:\n    compression: lz4\n",
			})
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), key) {
				t.Fatalf("CreateVolume err = %v, want InvalidArgument naming %q", err, key)
			}
			if env.agent.createVolumeCalls != 0 {
				t.Errorf("agent.CreateVolume called despite rejected annotation")
			}
		})
	}
}

// TestCreateVolume_PVCAnnotationOverride_BlockedField verifies that a PVC
// annotation that attempts to override a structural ZFS field (pool) causes
// CreateVolume to return codes.InvalidArgument (not codes.Internal).
func TestCreateVolume_PVCAnnotationOverride_BlockedField(t *testing.T) {
	t.Parallel()

	annotations := map[string]string{
		v1alpha1.AnnotationBackendDoc: `
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
	want := v1alpha1.AnnotationBackendDoc + ": zfs.pool is structural and cannot be set per volume"
	if !strings.Contains(st.Message(), want) {
		t.Errorf("error %q does not contain %q", st.Message(), want)
	}
	// Agent must NOT have been called.
	if env.agent.createVolumeCalls != 0 {
		t.Errorf("agent.CreateVolume called despite annotation validation failure")
	}
}

// TestCreateVolume_PVCAnnotationOverride_NoPVCMetadata verifies that when the
// pvc/name / pvc/namespace parameters are absent (StorageClass provisioned
// without external-provisioner --extra-create-metadata) the call succeeds
// without annotation overrides.
func TestCreateVolume_PVCAnnotationOverride_NoPVCMetadata(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	req := baseCreateVolumeRequest()
	// Deliberately omit pvc/name and pvc/namespace.

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

// A claim that external-provisioner named but that cannot be read must fail
// provisioning (retryable) instead of provisioning without its overrides.
func TestCreateVolume_PVCLookupFailure_NotSilentlySkipped(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-unreadable", map[string]string{
		v1alpha1.AnnotationBackendDoc: "zfs:\n  properties:\n    compression: zstd\n",
	})
	funcs := fakeuid.Interceptor()
	funcs.Get = func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey,
		obj ctrlclient.Object, opts ...ctrlclient.GetOption,
	) error {
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			return errors.New("apiserver unavailable")
		}
		return c.Get(ctx, key, obj, opts...)
	}
	env.srv.apiReader = fake.NewClientBuilder().WithScheme(env.scheme).WithInterceptorFuncs(funcs).Build()

	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "default/pvc-unreadable") {
		t.Fatalf("CreateVolume error = %v, want Internal naming the claim", err)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Errorf("agent.CreateVolume called without the claim's overrides")
	}

	req.Parameters[paramPVCNameMeta] = "pvc-deleted"
	env.srv.apiReader = env.srv.k8sClient
	_, err = env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateVolume for a missing claim: error = %v, want FailedPrecondition", err)
	}
}

// A StorageClass that names a PillarStorageClass (or whose binding names a
// PillarStore / PillarProtocol) that no longer exists must not provision a
// volume without the store, protocol and binding settings.
func TestCreateVolume_MissingBindingOrStore_FailedPrecondition(t *testing.T) {
	t.Parallel()
	orphanStore := &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-store-binding"},
		Spec:       v1alpha1.PillarStorageClassSpec{StoreRef: "deleted-store", ProtocolRef: testProtocolName},
	}
	orphanProtocol := &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-protocol-binding"},
		Spec:       v1alpha1.PillarStorageClassSpec{StoreRef: testStoreName, ProtocolRef: "deleted-protocol"},
	}
	for name, bindingName := range map[string]string{
		"binding":  "deleted-binding",
		"store":    orphanStore.Name,
		"protocol": orphanProtocol.Name,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t, orphanStore.DeepCopy(), orphanProtocol.DeepCopy())
			req := baseCreateVolumeRequest()
			req.Parameters = map[string]string{paramBinding: bindingName}
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("CreateVolume error = %v, want FailedPrecondition", err)
			}
			if env.agent.createVolumeCalls != 0 {
				t.Errorf("agent.CreateVolume called without the store and binding settings")
			}
		})
	}
}

// Once a volume is durably Ready the retry must keep serving it even when the
// after the response was lost); re-requiring the merge inputs would strand a
// completed backend behind an error.  The retried response must also replay
// the overrides the claim set: it may still become the PV's VolumeContext.
func TestCreateVolume_CompletedRetry_ClaimDeleted(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-gone-after", map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  ctrlLossTmo: 900\n",
	})
	ctx := context.Background()

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramNVMeOFCtrlLossTmo]; got != "900" {
		t.Fatalf("first VolumeContext ctrl-loss-tmo = %q, want 900", got)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-gone-after", Namespace: "default"},
	}
	if delErr := env.srv.k8sClient.Delete(ctx, pvc); delErr != nil {
		t.Fatalf("delete claim: %v", delErr)
	}

	resp2, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("retry after claim deletion: %v", err)
	}
	if !maps.Equal(resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext()) {
		t.Errorf("retry VolumeContext %v != first %v",
			resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext())
	}
	if resp2.GetVolume().GetVolumeId() != resp.GetVolume().GetVolumeId() {
		t.Errorf("retry VolumeId %q != first %q", resp2.GetVolume().GetVolumeId(), resp.GetVolume().GetVolumeId())
	}
	if env.agent.createVolumeCalls != 1 || env.agent.exportVolumeCalls != 1 {
		t.Errorf("retry re-provisioned: create=%d export=%d, want 1/1",
			env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
	}
}

// Same guarantee when the referenced PillarStorageClass (binding) — and thus
// its store — is deleted after the volume was created.
func TestCreateVolume_CompletedRetry_BindingDeleted(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()

	pool := &v1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "gone-pool"},
		Spec: v1alpha1.PillarStoreSpec{
			AgentRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{
				ZFS: &v1alpha1.ZFSBackendConfig{
					Pool:       "tank",
					Properties: map[string]string{"compression": "lz4"},
				},
			},
		},
	}
	binding := &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gone-binding"},
		Spec:       v1alpha1.PillarStorageClassSpec{StoreRef: pool.Name, ProtocolRef: testProtocolName},
	}
	for _, obj := range []ctrlclient.Object{pool, binding} {
		if err := env.srv.k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %s: %v", obj.GetName(), err)
		}
	}

	req := baseCreateVolumeRequest()
	req.Parameters = map[string]string{paramBinding: binding.Name}
	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}

	if delErr := env.srv.k8sClient.Delete(ctx, binding); delErr != nil {
		t.Fatalf("delete binding: %v", delErr)
	}
	if delErr := env.srv.k8sClient.Delete(ctx, pool); delErr != nil {
		t.Fatalf("delete store: %v", delErr)
	}

	resp2, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("retry after binding deletion: %v", err)
	}
	if !maps.Equal(resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext()) {
		t.Errorf("retry VolumeContext %v != first %v",
			resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext())
	}

	if resp2.GetVolume().GetVolumeId() != resp.GetVolume().GetVolumeId() {
		t.Errorf("retry VolumeId %q != first %q", resp2.GetVolume().GetVolumeId(), resp.GetVolume().GetVolumeId())
	}
	if env.agent.createVolumeCalls != 1 || env.agent.exportVolumeCalls != 1 {
		t.Errorf("retry re-provisioned: create=%d export=%d, want 1/1",
			env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
	}
}

// A CreatePartial retry reports the connect parameters frozen at the first
// attempt (spec.resolved), and the completed retry that follows reports the
// same — the VolumeContext never changes between responses for one volume
// even when the claim's annotation changed or the claim is gone.
func TestCreateVolume_PartialThenCompletedRetry_StableVolumeContext(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-tuned-900", map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  ctrlLossTmo: 900\n",
	})
	ctx := context.Background()

	// First attempt: backend is created, export fails → CreatePartial.
	env.agent.exportVolumeErr = status.Error(codes.Internal, "simulated export failure")
	if _, err := env.srv.CreateVolume(ctx, req); err == nil {
		t.Fatal("first CreateVolume: expected export failure")
	}

	// The claim's annotation changes while the volume is unfinished; the
	// retry must still report the value the volume was created with.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := env.srv.k8sClient.Get(ctx,
		types.NamespacedName{Name: "pvc-tuned-900", Namespace: "default"}, pvc); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	pvc.Annotations[v1alpha1.AnnotationProtocolDoc] = "nvmeofTcp:\n  ctrlLossTmo: 600\n"
	if err := env.srv.k8sClient.Update(ctx, pvc); err != nil {
		t.Fatalf("update claim: %v", err)
	}

	env.agent.exportVolumeErr = nil
	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("partial retry: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramNVMeOFCtrlLossTmo]; got != "900" {
		t.Fatalf("partial-retry ctrl-loss-tmo = %q, want the first-attempt value 900", got)
	}

	// The claim is deleted; the completed retry must report identically.
	if delErr := env.srv.k8sClient.Delete(ctx, pvc); delErr != nil {
		t.Fatalf("delete claim: %v", delErr)
	}
	resp2, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("completed retry: %v", err)
	}
	if !maps.Equal(resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext()) {
		t.Errorf("completed-retry VolumeContext %v != partial-retry %v",
			resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext())
	}
	if env.agent.createVolumeCalls != 1 || env.agent.exportVolumeCalls != 2 {
		t.Errorf("agent calls = create %d / export %d, want 1/2",
			env.agent.createVolumeCalls, env.agent.exportVolumeCalls)
	}
}

// An all-default first-attempt merge is still a recorded snapshot: a connect
// override added to the claim before a partial retry must NOT leak into the
// response.
func TestCreateVolume_PartialRetry_EmptySnapshotBlocksLaterOverride(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-defaults", nil)
	ctx := context.Background()

	env.agent.exportVolumeErr = status.Error(codes.Internal, "simulated export failure")
	if _, err := env.srv.CreateVolume(ctx, req); err == nil {
		t.Fatal("first CreateVolume: expected export failure")
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := env.srv.k8sClient.Get(ctx,
		types.NamespacedName{Name: "pvc-defaults", Namespace: "default"}, pvc); err != nil {
		t.Fatalf("get claim: %v", err)
	}
	pvc.Annotations = map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  ctrlLossTmo: 600\n",
	}
	if err := env.srv.k8sClient.Update(ctx, pvc); err != nil {
		t.Fatalf("update claim: %v", err)
	}

	env.agent.exportVolumeErr = nil
	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("partial retry: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramNVMeOFCtrlLossTmo]; got != "" {
		t.Errorf("partial-retry ctrl-loss-tmo = %q, want absent (snapshot had no overrides)", got)
	}
}

// A connect key the create-time resolution left absent must not be
// resurrected by a later change to the live PillarProtocol on a
// completed-volume retry: spec.resolved wins over the CRs.
func TestCreateVolume_CompletedRetry_AbsentKeyNotResurrected(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-no-delay", map[string]string{
		v1alpha1.AnnotationProtocolDoc: "nvmeofTcp:\n  ctrlLossTmo: 900\n",
	})
	ctx := context.Background()

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[paramNVMeOFCtrlLossTmo]; got != "900" {
		t.Fatalf("first ctrl-loss-tmo = %q, want 900", got)
	}
	if got, ok := resp.GetVolume().GetVolumeContext()[paramNVMeOFReconnectDelay]; ok {
		t.Fatalf("first reconnect-delay = %q, want absent (not configured anywhere)", got)
	}

	// The protocol gains a reconnectDelay and the claim is deleted.
	proto := &v1alpha1.PillarProtocol{}
	if err = env.srv.k8sClient.Get(ctx, ctrlKey(testProtocolName), proto); err != nil {
		t.Fatalf("get protocol: %v", err)
	}
	proto.Spec.Protocol.NVMeOFTCP.ReconnectDelay = testInt32(5)
	if err = env.srv.k8sClient.Update(ctx, proto); err != nil {
		t.Fatalf("update protocol: %v", err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-no-delay", Namespace: "default"},
	}
	if delErr := env.srv.k8sClient.Delete(ctx, pvc); delErr != nil {
		t.Fatalf("delete claim: %v", delErr)
	}

	resp2, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("retry after claim deletion: %v", err)
	}
	if !maps.Equal(resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext()) {
		t.Errorf("retry VolumeContext %v != first %v",
			resp2.GetVolume().GetVolumeContext(), resp.GetVolume().GetVolumeContext())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Effective configuration resolution (store → binding → SC docs → PVC docs)
// ─────────────────────────────────────────────────────────────────────────────

// createLVMVolume provisions req on the seeded LVM store through a
// hand-written StorageClass carrying scBackendDoc (may be empty) and returns
// the LVM params the agent received.
func createLVMVolume(
	t *testing.T, env *controllerTestEnv, req *csi.CreateVolumeRequest, scBackendDoc string,
) *agentv1.LvmVolumeParams {
	t.Helper()
	req.Parameters[paramStoreRef] = testLVMStoreName
	if scBackendDoc != "" {
		req.Parameters[paramBackendDoc] = scBackendDoc
	}
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if want := "storage-node-1/nvmeof-tcp/lvm-lv/data-vg/" + req.GetName(); resp.GetVolume().GetVolumeId() != want {
		t.Errorf("VolumeId = %q, want %q", resp.GetVolume().GetVolumeId(), want)
	}
	if got := env.agent.lastCreateVolumeReq.GetBackendType(); got != agentv1.BackendType_BACKEND_TYPE_LVM {
		t.Errorf("agent BackendType = %v, want LVM", got)
	}
	lvm := env.agent.lastCreateVolumeReq.GetBackendParams().GetLvm()
	if lvm == nil {
		t.Fatal("BackendParams.Lvm is nil")
	}
	if lvm.GetVolumeGroup() != "data-vg" {
		t.Errorf("VolumeGroup = %q, want data-vg", lvm.GetVolumeGroup())
	}
	return lvm
}

// setLVMStoreMode sets the seeded LVM store's provisioningMode.
func setLVMStoreMode(t *testing.T, env *controllerTestEnv, mode v1alpha1.LVMProvisioningMode) {
	t.Helper()
	store := &v1alpha1.PillarStore{}
	if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(testLVMStoreName), store); err != nil {
		t.Fatalf("get store: %v", err)
	}
	store.Spec.Backend.LVM.ProvisioningMode = mode
	if err := env.srv.k8sClient.Update(context.Background(), store); err != nil {
		t.Fatalf("update store: %v", err)
	}
}

// TestCreateVolume_LVMProvisioningModePrecedence verifies the provisioning
// mode resolution PVC > StorageClass document > store > default "linear",
// and that the resolved mode is always sent to the agent and persisted.
func TestCreateVolume_LVMProvisioningModePrecedence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		storeMode v1alpha1.LVMProvisioningMode
		scDoc     string
		pvcDoc    string
		want      string
	}{
		{name: "default is linear", want: "linear"},
		{name: "store thin", storeMode: v1alpha1.LVMProvisioningModeThin, want: testModeThin},
		{
			name: "StorageClass document beats store", storeMode: v1alpha1.LVMProvisioningModeThin,
			scDoc: "lvm:\n  provisioningMode: linear\n", want: "linear",
		},
		{
			name: "PVC document beats StorageClass document", storeMode: v1alpha1.LVMProvisioningModeLinear,
			scDoc: "lvm:\n  provisioningMode: linear\n", pvcDoc: "lvm:\n  provisioningMode: thin\n", want: testModeThin,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ann map[string]string
			if tc.pvcDoc != "" {
				ann = map[string]string{v1alpha1.AnnotationBackendDoc: tc.pvcDoc}
			}
			env, req := newControllerTestEnvWithPVC(t, "default", "pvc-lvm", ann)
			if tc.storeMode != "" {
				setLVMStoreMode(t, env, tc.storeMode)
			}
			lvm := createLVMVolume(t, env, req, tc.scDoc)
			if lvm.GetProvisionMode() != tc.want {
				t.Errorf("ProvisionMode = %q, want %q", lvm.GetProvisionMode(), tc.want)
			}
			resolved := loadResolved(t, env, req.GetName())
			if resolved.Backend.LVM == nil || string(resolved.Backend.LVM.ProvisioningMode) != tc.want {
				t.Errorf("spec.resolved.backend.lvm = %+v, want provisioningMode %q", resolved.Backend.LVM, tc.want)
			}
		})
	}
}

// TestCreateVolume_LVMBindingPrecedence verifies PVC > binding > store for the
// LVM provisioning mode on the generated StorageClass path.
func TestCreateVolume_LVMBindingPrecedence(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pvcDoc string
		want   string
	}{
		"binding beats store": {want: testModeThin},
		"PVC beats binding":   {pvcDoc: "lvm:\n  provisioningMode: linear\n", want: "linear"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var ann map[string]string
			if tc.pvcDoc != "" {
				ann = map[string]string{v1alpha1.AnnotationBackendDoc: tc.pvcDoc}
			}
			env, req := newControllerTestEnvWithPVC(t, "default", "pvc-lvm", ann)
			binding := &v1alpha1.PillarStorageClass{
				ObjectMeta: metav1.ObjectMeta{Name: "lvm-binding"},
				Spec: v1alpha1.PillarStorageClassSpec{
					StoreRef:    testLVMStoreName,
					ProtocolRef: testProtocolName,
					Overrides: &v1alpha1.StorageClassOverrides{
						Backend: &v1alpha1.BackendOverrides{
							LVM: &v1alpha1.LVMBackendOverrides{ProvisioningMode: v1alpha1.LVMProvisioningModeThin},
						},
					},
				},
			}
			if err := env.srv.k8sClient.Create(context.Background(), binding); err != nil {
				t.Fatalf("create binding: %v", err)
			}
			setLVMStoreMode(t, env, v1alpha1.LVMProvisioningModeLinear)

			req.Parameters = map[string]string{
				paramBinding:          binding.Name,
				paramPVCNameMeta:      "pvc-lvm",
				paramPVCNamespaceMeta: "default",
			}
			if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			lvm := env.agent.lastCreateVolumeReq.GetBackendParams().GetLvm()
			if lvm.GetVolumeGroup() != "data-vg" || lvm.GetProvisionMode() != tc.want {
				t.Errorf("Lvm params = %+v, want data-vg / %s", lvm, tc.want)
			}
		})
	}
}

// TestCreateVolume_LVMThinPoolSent verifies that the store's thin pool
// reaches the agent as LvmVolumeParams.thin_pool, including an empty value
// when the store declares none, so the agent can refuse a create in a
// different thin pool (issue #113).
func TestCreateVolume_LVMThinPoolSent(t *testing.T) {
	t.Parallel()
	for name, thinPool := range map[string]string{"declared": "thin-pool-0", "none": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			store := &v1alpha1.PillarStore{}
			if err := env.srv.k8sClient.Get(context.Background(), ctrlKey(testLVMStoreName), store); err != nil {
				t.Fatalf("get store: %v", err)
			}
			store.Spec.Backend.LVM.ThinPool = thinPool
			if err := env.srv.k8sClient.Update(context.Background(), store); err != nil {
				t.Fatalf("update store: %v", err)
			}
			lvm := createLVMVolume(t, env, baseCreateVolumeRequest(), "")
			if lvm.ThinPool == nil || lvm.GetThinPool() != thinPool {
				t.Errorf("ThinPool = %v, want declared %q", lvm.ThinPool, thinPool)
			}
		})
	}
}

// zfsPrecedenceStore is a ZFS store whose properties every override layer
// partially replaces.
func zfsPrecedenceStore() *v1alpha1.PillarStore {
	return &v1alpha1.PillarStore{
		ObjectMeta: metav1.ObjectMeta{Name: "zfs-layers"},
		Spec: v1alpha1.PillarStoreSpec{
			AgentRef: "storage-node-1",
			Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
				VolumeType:    v1alpha1.ZFSVolumeTypeZvol,
				Pool:          "hot-data",
				ParentDataset: "k8s",
				Properties:    map[string]string{"atime": "store", "compression": "store", "volblocksize": "store"},
			}},
		},
	}
}

// assertZFSLayers checks the agent received the store's placement and the
// key-wise merged properties.
func assertZFSLayers(t *testing.T, env *controllerTestEnv, resp *csi.CreateVolumeResponse, want map[string]string) {
	t.Helper()
	// The volume ID carries the physical pool, not the PillarStore name,
	// and not the parent dataset (the agent applies its own).
	const wantID = "storage-node-1/nvmeof-tcp/zfs-zvol/hot-data/pvc-zfs"
	if got := resp.GetVolume().GetVolumeId(); got != wantID {
		t.Errorf("VolumeId = %q, want %q", got, wantID)
	}
	zfs := env.agent.lastCreateVolumeReq.GetBackendParams().GetZfs()
	if zfs.GetPool() != "hot-data" || zfs.GetParentDataset() != "k8s" {
		t.Errorf("Zfs placement = pool %q parent %q, want hot-data / k8s", zfs.GetPool(), zfs.GetParentDataset())
	}
	if !maps.Equal(zfs.GetProperties(), want) {
		t.Errorf("Zfs properties = %v, want %v", zfs.GetProperties(), want)
	}
}

// TestCreateVolume_ZFSPropertiesPrecedence_Binding verifies PVC > binding >
// store for ZFS properties on the generated StorageClass path.
func TestCreateVolume_ZFSPropertiesPrecedence_Binding(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-zfs", map[string]string{
		v1alpha1.AnnotationBackendDoc: "zfs:\n  properties:\n    volblocksize: pvc\n",
	})
	ctx := context.Background()
	binding := &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "zfs-binding"},
		Spec: v1alpha1.PillarStorageClassSpec{
			StoreRef:    "zfs-layers",
			ProtocolRef: testProtocolName,
			Overrides: &v1alpha1.StorageClassOverrides{Backend: &v1alpha1.BackendOverrides{
				ZFS: &v1alpha1.ZFSBackendOverrides{Properties: map[string]string{
					"compression": "binding", "volblocksize": "binding",
				}},
			}},
		},
	}
	for _, obj := range []ctrlclient.Object{zfsPrecedenceStore(), binding} {
		if err := env.srv.k8sClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %s: %v", obj.GetName(), err)
		}
	}
	req.Name = "pvc-zfs"
	req.Parameters = map[string]string{
		paramBinding:          binding.Name,
		paramPVCNameMeta:      "pvc-zfs",
		paramPVCNamespaceMeta: "default",
	}

	resp, err := env.srv.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	assertZFSLayers(t, env, resp, map[string]string{"atime": "store", "compression": "binding", "volblocksize": "pvc"})
}

// TestCreateVolume_ZFSPropertiesPrecedence_HandWritten verifies PVC >
// StorageClass document > store for ZFS properties on a hand-written
// StorageClass.
func TestCreateVolume_ZFSPropertiesPrecedence_HandWritten(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-zfs", map[string]string{
		v1alpha1.AnnotationBackendDoc: "zfs:\n  properties:\n    volblocksize: pvc\n",
	})
	if err := env.srv.k8sClient.Create(context.Background(), zfsPrecedenceStore()); err != nil {
		t.Fatalf("create store: %v", err)
	}
	req.Name = "pvc-zfs"
	req.Parameters[paramStoreRef] = "zfs-layers"
	req.Parameters[paramBackendDoc] = "zfs:\n  properties:\n    compression: sc\n    volblocksize: sc\n"

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	assertZFSLayers(t, env, resp, map[string]string{"atime": "store", "compression": "sc", "volblocksize": "pvc"})
}

// TestCreateVolume_PVCBackendMemberMustMatchStore verifies that a PVC backend
// document naming the other backend is rejected rather than ignored.
func TestCreateVolume_PVCBackendMemberMustMatchStore(t *testing.T) {
	t.Parallel()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-mismatch", map[string]string{
		v1alpha1.AnnotationBackendDoc: "lvm:\n  provisioningMode: thin\n",
	})
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume err = %v, want InvalidArgument (lvm document on a zfs store)", err)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Error("agent contacted despite mismatched backend document")
	}
}

// TestCreateVolume_ProtocolACLAndPort verifies that the protocol's acl and
// port reach ExportVolume and the durable exportSpec.
func TestCreateVolume_ProtocolACLAndPort(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, &v1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: "nvme-acl"},
		Spec: v1alpha1.PillarProtocolSpec{Protocol: v1alpha1.ProtocolSpec{
			NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4421, ACL: true},
		}},
	})
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolRef] = "nvme-acl"
	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	exp := env.agent.lastExportVolumeReq
	if !exp.GetAclEnabled() || exp.GetExportParams().GetNvmeofTcp().GetPort() != 4421 {
		t.Errorf("ExportVolume acl=%v port=%d, want true / 4421",
			exp.GetAclEnabled(), exp.GetExportParams().GetNvmeofTcp().GetPort())
	}
	pvs, _, err := env.srv.loadPillarVolumeState(context.Background(), req.GetName())
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if spec := pvs.Status.ExportSpec; spec == nil || !spec.ACLEnabled || spec.Port != 4421 {
		t.Errorf("status.exportSpec = %+v, want aclEnabled / port 4421", spec)
	}
}

// filesystemBinding is a PillarStorageClass carrying the filesystem axis and
// a protocol override.
func filesystemBinding() *v1alpha1.PillarStorageClass {
	mkfs := []string{"-K"}
	mount := []string{"noatime"}
	return &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "xfs-binding"},
		Spec: v1alpha1.PillarStorageClassSpec{
			StoreRef:    testStoreName,
			ProtocolRef: testProtocolName,
			Filesystem:  &v1alpha1.FilesystemConfig{FSType: "xfs", MkfsOptions: &mkfs, MountOptions: &mount},
			Overrides: &v1alpha1.StorageClassOverrides{Protocol: &v1alpha1.ProtocolOverrides{
				NVMeOFTCP: &v1alpha1.NVMeOFTCPOverrides{MaxQueueSize: testInt32(128)},
			}},
		},
	}
}

// createWithFilesystemBinding provisions a Mount volume through a generated
// StorageClass (binding + csi fstype) for a claim carrying ann.
func createWithFilesystemBinding(
	t *testing.T, ann map[string]string,
) (*controllerTestEnv, *csi.CreateVolumeResponse, error) {
	t.Helper()
	env, req := newControllerTestEnvWithPVC(t, "default", "pvc-fs", ann)
	if err := env.srv.k8sClient.Create(context.Background(), filesystemBinding()); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	req.Parameters = map[string]string{
		paramBinding:          "xfs-binding",
		paramFSTypeSC:         "xfs",
		paramPVCNameMeta:      "pvc-fs",
		paramPVCNamespaceMeta: "default",
	}
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
	resp, err := env.srv.CreateVolume(context.Background(), req)
	return env, resp, err
}

// TestCreateVolume_BindingFilesystemAndProtocolOverride verifies that the
// binding's filesystem axis and protocol override reach the node through the
// VolumeContext, including the binding's mountOptions (resolved, so the node
// applies them even though the kubelet passes the StorageClass's own list).
func TestCreateVolume_BindingFilesystemAndProtocolOverride(t *testing.T) {
	t.Parallel()
	env, resp, err := createWithFilesystemBinding(t, nil)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vc := resp.GetVolume().GetVolumeContext()
	if vc[paramFSType] != "xfs" {
		t.Errorf("VolumeContext[%s] = %q, want xfs", paramFSType, vc[paramFSType])
	}
	var mkfs []string
	err = json.Unmarshal([]byte(vc[paramMkfsOptions]), &mkfs)
	if err != nil || !slices.Equal(mkfs, []string{"-K"}) {
		t.Errorf("VolumeContext[%s] = %q, want [\"-K\"]", paramMkfsOptions, vc[paramMkfsOptions])
	}
	if vc[paramNVMeOFMaxQueueSize] != "128" {
		t.Errorf("VolumeContext[%s] = %q, want 128 (binding override)", paramNVMeOFMaxQueueSize, vc[paramNVMeOFMaxQueueSize])
	}
	var mount []string
	err = json.Unmarshal([]byte(vc[paramMountOptions]), &mount)
	if err != nil || !slices.Equal(mount, []string{"noatime"}) {
		t.Errorf("VolumeContext[%s] = %q, want [\"noatime\"]", paramMountOptions, vc[paramMountOptions])
	}
	resolved := loadResolved(t, env, "pvc-abc123")
	if resolved.Filesystem == nil || resolved.Filesystem.FSType != "xfs" {
		t.Errorf("spec.resolved.filesystem = %+v, want fsType xfs", resolved.Filesystem)
	}
}

// TestCreateVolume_FilesystemListSemantics verifies list semantics from the
// binding to the PVC layer: an omitted (or null) list inherits the binding's,
// an explicit [] clears it (mkfs options disappear; mount options are sent as
// "[]" so the node drops the StorageClass flags), a set list replaces it.
func TestCreateVolume_FilesystemListSemantics(t *testing.T) {
	t.Parallel()
	const absent = "<absent>"
	for name, tc := range map[string]struct {
		pvcDoc    string
		wantMkfs  string
		wantMount string
	}{
		"PVC omits both lists": {wantMkfs: `["-K"]`, wantMount: `["noatime"]`},
		"PVC null lists inherit": {
			pvcDoc: "mkfsOptions: null\nmountOptions: null\n", wantMkfs: `["-K"]`, wantMount: `["noatime"]`,
		},
		"PVC clears mkfsOptions":  {pvcDoc: "mkfsOptions: []\n", wantMkfs: absent, wantMount: `["noatime"]`},
		"PVC clears mountOptions": {pvcDoc: "mountOptions: []\n", wantMkfs: `["-K"]`, wantMount: `[]`},
		"PVC replaces mountOptions": {
			pvcDoc: "mountOptions: [nodiscard]\n", wantMkfs: `["-K"]`, wantMount: `["nodiscard"]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var ann map[string]string
			if tc.pvcDoc != "" {
				ann = map[string]string{v1alpha1.AnnotationFilesystemDoc: tc.pvcDoc}
			}
			_, resp, err := createWithFilesystemBinding(t, ann)
			if err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			vc := resp.GetVolume().GetVolumeContext()
			for key, want := range map[string]string{paramMkfsOptions: tc.wantMkfs, paramMountOptions: tc.wantMount} {
				got, ok := vc[key]
				if !ok {
					got = absent
				}
				if got != want {
					t.Errorf("VolumeContext[%s] = %s, want %s", key, got, want)
				}
			}
		})
	}
}

// TestCreateVolume_NVMeoFTuningPrecedence verifies PVC > binding > protocol
// (generated StorageClass) and PVC > StorageClass document > protocol
// (hand-written StorageClass) for every per-volume NVMe-oF tunable: the
// connect settings reach the VolumeContext, inCapsuleDataSize reaches
// ExportVolume.
func TestCreateVolume_NVMeoFTuningPrecedence(t *testing.T) {
	t.Parallel()
	protocol := &v1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: "nvme-tuned"},
		Spec: v1alpha1.PillarProtocolSpec{Protocol: v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{
			Port:              4420,
			MaxQueueSize:      testInt32(32),
			InCapsuleDataSize: testInt32(4096),
			CtrlLossTmo:       testInt32(600),
			ReconnectDelay:    testInt32(10),
		}}},
	}
	// Middle layer sets three fields, the PVC one; each layer leaves the
	// rest to the layer below.
	const middleDoc = "nvmeofTcp:\n  maxQueueSize: 64\n  inCapsuleDataSize: 8192\n  ctrlLossTmo: 1200\n"
	const pvcDoc = "nvmeofTcp:\n  ctrlLossTmo: 1800\n"
	check := func(t *testing.T, env *controllerTestEnv, resp *csi.CreateVolumeResponse) {
		t.Helper()
		vc := resp.GetVolume().GetVolumeContext()
		for key, want := range map[string]string{
			paramNVMeOFMaxQueueSize:   "64",   // middle layer
			paramNVMeOFCtrlLossTmo:    "1800", // PVC
			paramNVMeOFReconnectDelay: "10",   // protocol
		} {
			if vc[key] != want {
				t.Errorf("VolumeContext[%s] = %q, want %q", key, vc[key], want)
			}
		}
		if got := env.agent.lastExportVolumeReq.GetExportParams().GetNvmeofTcp().GetInCapsuleDataSize(); got != 8192 {
			t.Errorf("ExportVolume in_capsule_data_size = %d, want 8192 (middle layer)", got)
		}
	}

	t.Run("binding", func(t *testing.T) {
		t.Parallel()
		env, req := newControllerTestEnvWithPVC(t, "default", "pvc-nvme", map[string]string{
			v1alpha1.AnnotationProtocolDoc: pvcDoc,
		})
		binding := &v1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "nvme-binding"},
			Spec: v1alpha1.PillarStorageClassSpec{
				StoreRef:    testStoreName,
				ProtocolRef: protocol.Name,
				Overrides: &v1alpha1.StorageClassOverrides{Protocol: &v1alpha1.ProtocolOverrides{
					NVMeOFTCP: &v1alpha1.NVMeOFTCPOverrides{
						MaxQueueSize: testInt32(64), InCapsuleDataSize: testInt32(8192), CtrlLossTmo: testInt32(1200),
					},
				}},
			},
		}
		for _, obj := range []ctrlclient.Object{protocol.DeepCopy(), binding} {
			if err := env.srv.k8sClient.Create(context.Background(), obj); err != nil {
				t.Fatalf("create %s: %v", obj.GetName(), err)
			}
		}
		req.Parameters = map[string]string{
			paramBinding:          binding.Name,
			paramPVCNameMeta:      "pvc-nvme",
			paramPVCNamespaceMeta: "default",
		}
		resp, err := env.srv.CreateVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		check(t, env, resp)
	})

	t.Run("hand-written", func(t *testing.T) {
		t.Parallel()
		env, req := newControllerTestEnvWithPVC(t, "default", "pvc-nvme", map[string]string{
			v1alpha1.AnnotationProtocolDoc: pvcDoc,
		})
		if err := env.srv.k8sClient.Create(context.Background(), protocol.DeepCopy()); err != nil {
			t.Fatalf("create protocol: %v", err)
		}
		req.Parameters[paramProtocolRef] = protocol.Name
		req.Parameters[paramProtocolDoc] = middleDoc
		resp, err := env.srv.CreateVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		check(t, env, resp)
	})
}

// TestCreateVolume_DocumentShapeSameOnPVCAndStorageClass verifies that the
// identical three documents are accepted both as PVC annotations and as
// hand-written StorageClass parameters and resolve to the same volume.
func TestCreateVolume_DocumentShapeSameOnPVCAndStorageClass(t *testing.T) {
	t.Parallel()
	docs := map[string]string{
		v1alpha1.AnnotationBackendDoc:    "zfs:\n  properties:\n    volblocksize: 16K\n",
		v1alpha1.AnnotationProtocolDoc:   "nvmeofTcp:\n  maxQueueSize: 64\n",
		v1alpha1.AnnotationFilesystemDoc: "fsType: xfs\nmkfsOptions: [\"-K\"]\nmountOptions: [noatime]\n",
	}
	create := func(t *testing.T, onPVC bool) (*controllerTestEnv, *csi.CreateVolumeResponse) {
		t.Helper()
		var ann map[string]string
		if onPVC {
			ann = docs
		}
		env, req := newControllerTestEnvWithPVC(t, "default", "pvc-docs", ann)
		if !onPVC {
			maps.Copy(req.Parameters, docs)
		}
		req = mountCreateVolumeRequest(req, "xfs")
		resp, err := env.srv.CreateVolume(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateVolume (onPVC=%v): %v", onPVC, err)
		}
		return env, resp
	}
	pvcEnv, pvcResp := create(t, true)
	scEnv, scResp := create(t, false)
	if !maps.Equal(pvcResp.GetVolume().GetVolumeContext(), scResp.GetVolume().GetVolumeContext()) {
		t.Errorf("VolumeContext differs: PVC %v, StorageClass %v",
			pvcResp.GetVolume().GetVolumeContext(), scResp.GetVolume().GetVolumeContext())
	}
	pvcProps := pvcEnv.agent.lastCreateVolumeReq.GetBackendParams().GetZfs().GetProperties()
	scProps := scEnv.agent.lastCreateVolumeReq.GetBackendParams().GetZfs().GetProperties()
	if pvcProps["volblocksize"] != "16K" || !maps.Equal(pvcProps, scProps) {
		t.Errorf("zfs properties: PVC %v, StorageClass %v, want volblocksize=16K on both", pvcProps, scProps)
	}
}

// TestCreateVolume_PVCFsTypeRevalidatesInheritedMkfsOptions verifies that the
// mkfs allowlist is checked against the effective fsType: a claim switching
// to ext4 while inheriting the class's xfs-only "-K" is rejected before
// provisioning.
func TestCreateVolume_PVCFsTypeRevalidatesInheritedMkfsOptions(t *testing.T) {
	t.Parallel()
	env, _, err := createWithFilesystemBinding(t, map[string]string{
		v1alpha1.AnnotationFilesystemDoc: "fsType: ext4\n",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume err = %v, want InvalidArgument (-K is not an ext4 option)", err)
	}
	if env.agent.createVolumeCalls != 0 {
		t.Error("agent contacted despite invalid mkfs options")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerPublishVolume — CSINode annotation lookup and FailedPrecondition
// ─────────────────────────────────────────────────────────────────────────────

// newPublishTestEnv builds a ControllerServer wired to a fake k8s client that
// has a PillarAgent and the configuration CRs of testConfigObjects (so
// baseCreateVolumeRequest resolves) but no CSINode and no PillarVolumeState
// by default.  Callers seed CSINode objects and the volume's
// PillarVolumeState (see volumeStateFor) as needed for each test case.
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

	target := &v1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-node-1"},
		Status: v1alpha1.PillarAgentStatus{
			ResolvedAddress: "192.168.1.10:9500",
		},
	}

	allObjs := append(append(testConfigObjects(), target), objs...)
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

// volumeStateFor returns the PillarVolumeState CreateVolume would have left
// for volumeID (phase Ready), carrying the given publication records.
func volumeStateFor(volumeID string, pubs ...v1alpha1.VolumePublication) *v1alpha1.PillarVolumeState {
	return &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: pillarVolumeStateNameFromVolumeID(volumeID)},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      volumeID,
			AgentVolumeID: "tank/" + pillarVolumeStateNameFromVolumeID(volumeID),
			AgentRef:      "storage-node-1",
			BackendType:   "zfs-zvol",
			ProtocolType:  strings.SplitN(volumeID, "/", volumeIDParts)[1],
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase:          v1alpha1.PillarVolumeStatePhaseReady,
			PublishedNodes: pubs,
		},
	}
}

// TestControllerPublishVolume_FailedPrecondition_CSINodeNotFound verifies that
// ControllerPublishVolume returns FailedPrecondition when the CSINode object
// does not exist yet (node plugin has not registered).
func TestControllerPublishVolume_NotFound_CSINodeNotFound(t *testing.T) {
	t.Parallel()

	// No CSINode objects seeded → CSINode lookup will return NotFound.
	// CSI spec §4.5 requires NotFound for an unknown node_id in
	// ControllerPublishVolume specifically — see the resolveInitiatorID
	// FailedPrecondition→NotFound translation in controller.go.  Other
	// callers of resolveInitiatorID (e.g. Unpublish) retain the
	// FailedPrecondition signal for retry-friendly behavior.
	env := newPublishTestEnv(t, volumeStateFor(basePublishRequest().GetVolumeId()))
	ctx := context.Background()

	_, err := env.srv.ControllerPublishVolume(ctx, basePublishRequest())
	if err == nil {
		t.Fatal("expected NotFound error, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("error code = %v, want %v", st.Code(), codes.NotFound)
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
	env := newPublishTestEnv(t, csiNode, volumeStateFor(basePublishRequest().GetVolumeId()))
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

// TestControllerPublishUnpublish_ACLOffSkipsInitiatorRPCs verifies that a
// volume exported with ACL off (attr_allow_any_host=1, where the kernel
// rejects allowed_hosts links with EINVAL) is published and unpublished
// without AllowInitiator / DenyInitiator, while the publication record is
// still kept and released (exclusivity and fencing stay ordered).
func TestControllerPublishUnpublish_ACLOffSkipsInitiatorRPCs(t *testing.T) {
	t.Parallel()

	req := basePublishRequest()
	pvs := volumeStateFor(req.GetVolumeId())
	pvs.Status.ExportSpec = &v1alpha1.VolumeExportSpec{BindAddress: "192.168.1.10", Port: 4420, ACLEnabled: false}
	csiNode := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{
		Name:        req.GetNodeId(),
		Annotations: map[string]string{AnnotationNVMeOFHostNQN: "nqn.2014-08.org.nvmexpress:uuid:w1"},
	}}
	env := newPublishTestEnv(t, csiNode, pvs)
	ctx := context.Background()

	_, err := env.srv.ControllerPublishVolume(ctx, req)
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0 for an allow-any-host export", env.agent.allowInitiatorCalls)
	}
	got, _, err := env.srv.loadPillarVolumeState(ctx, pvs.Name)
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if len(got.Status.PublishedNodes) != 1 || got.Status.PublishedNodes[0].NodeID != req.GetNodeId() {
		t.Fatalf("publishedNodes = %+v, want the published node recorded", got.Status.PublishedNodes)
	}

	_, err = env.srv.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: req.GetVolumeId(), NodeId: req.GetNodeId(),
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator calls = %d, want 0 for an allow-any-host export", env.agent.denyInitiatorCalls)
	}
	got, _, err = env.srv.loadPillarVolumeState(ctx, pvs.Name)
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if len(got.Status.PublishedNodes) != 0 {
		t.Errorf("publishedNodes = %+v, want released", got.Status.PublishedNodes)
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
	env := newPublishTestEnv(t, csiNode, volumeStateFor(basePublishRequest().GetVolumeId()))
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

func TestValidateVolumeCapabilities_AllowsFilesystemVolumeMode(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	seedPillarVolumeState(t, env, "pvc-abc123")
	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123",
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		},
	}

	resp, err := env.srv.ValidateVolumeCapabilities(context.Background(), req)
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities unexpected error: %v", err)
	}
	if resp.GetConfirmed() == nil {
		t.Fatal("Confirmed is nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerPublishVolume — publish exclusivity (CSI access modes)
// ─────────────────────────────────────────────────────────────────────────────

const (
	exclNode1 = "worker-node-1"
	exclNode2 = "worker-node-2"
)

// exclNQN returns the NVMe-oF host NQN the fixture CSINode reports for node.
func exclNQN(node string) string { return "nqn.2014-08.org.nvmexpress:uuid:" + node }

// exclCSINodes returns CSINode objects for both test nodes with distinct NQNs.
func exclCSINodes() []ctrlclient.Object {
	objs := make([]ctrlclient.Object, 0, 2)
	for _, node := range []string{exclNode1, exclNode2} {
		objs = append(objs, &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{
			Name:        node,
			Annotations: map[string]string{AnnotationNVMeOFHostNQN: exclNQN(node)},
		}})
	}
	return objs
}

// exclPublishReq builds a publish request for volumeID on node.
func exclPublishReq(
	volumeID, node string,
	mode csi.VolumeCapability_AccessMode_Mode,
	readonly bool,
) *csi.ControllerPublishVolumeRequest {
	req := basePublishRequest()
	req.VolumeId = volumeID
	req.NodeId = node
	req.Readonly = readonly
	req.VolumeCapability.AccessMode.Mode = mode
	return req
}

// exclPub builds the publication record ControllerPublishVolume writes for
// an NVMe-oF node.
func exclPub(
	node, initiator string,
	mode csi.VolumeCapability_AccessMode_Mode,
	readonly bool,
) v1alpha1.VolumePublication {
	return v1alpha1.VolumePublication{
		NodeID:      node,
		InitiatorID: initiator,
		AccessMode:  mode.String(),
		Readonly:    readonly,
	}
}

// exclCountWinners counts successful publishes and reports any failure that
// is not the expected FailedPrecondition rejection.
func exclCountWinners(t *testing.T, errs []error) int {
	t.Helper()
	wins := 0
	for _, err := range errs {
		switch status.Code(err) {
		case codes.OK:
			wins++
		case codes.FailedPrecondition:
		default:
			t.Errorf("unexpected loser error: %v", err)
		}
	}
	return wins
}

// exclPublishedNodes reads the durable publication records of volumeID.
func exclPublishedNodes(t *testing.T, c ctrlclient.Client, volumeID string) []v1alpha1.VolumePublication {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: pillarVolumeStateNameFromVolumeID(volumeID)}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	return pvs.Status.PublishedNodes
}

// TestControllerPublishVolume_Exclusivity verifies the CSI ControllerPublishVolume
// compatibility contract against existing publication records: another node
// holding the volume incompatibly yields FailedPrecondition, the same node
// with a different capability yields AlreadyExists, compatible multi-node
// modes coexist, and a rejected publish never reaches AllowInitiator.
func TestControllerPublishVolume_Exclusivity(t *testing.T) {
	t.Parallel()

	blockID := basePublishRequest().GetVolumeId()
	const (
		snw  = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
		snmw = csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER
		snsw = csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER
		mnro = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
		mnsw = csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER
		mnmw = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	)

	testCases := []struct {
		name        string
		volumeID    string
		existing    []v1alpha1.VolumePublication
		req         *csi.ControllerPublishVolumeRequest
		wantCode    codes.Code
		wantRecords int
	}{
		{"first publish records node", blockID, nil,
			exclPublishReq(blockID, exclNode1, snw, false), codes.OK, 1},
		{"SINGLE_NODE_WRITER second node rejected", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snw, false)},
			exclPublishReq(blockID, exclNode2, snw, false), codes.FailedPrecondition, 1},
		{"SINGLE_NODE_MULTI_WRITER second node rejected", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snmw, false)},
			exclPublishReq(blockID, exclNode2, snmw, false), codes.FailedPrecondition, 1},
		{"SINGLE_NODE_SINGLE_WRITER second node rejected", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snsw, false)},
			exclPublishReq(blockID, exclNode2, snsw, false), codes.FailedPrecondition, 1},
		{"reader-only request against single-node writer rejected", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snw, false)},
			exclPublishReq(blockID, exclNode2, mnro, true), codes.FailedPrecondition, 1},
		{"same node identical retry is idempotent", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snw, false)},
			exclPublishReq(blockID, exclNode1, snw, false), codes.OK, 1},
		{"same node different access mode", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snw, false)},
			exclPublishReq(blockID, exclNode1, snsw, false), codes.AlreadyExists, 1},
		{"same node different readonly", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), snw, false)},
			exclPublishReq(blockID, exclNode1, snw, true), codes.AlreadyExists, 1},
		{"same node reports a different initiator", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, "nqn.old-host", snw, false)},
			exclPublishReq(blockID, exclNode1, snw, false), codes.FailedPrecondition, 1},
		{"MULTI_NODE_READER_ONLY shares across nodes", blockID,
			[]v1alpha1.VolumePublication{exclPub(exclNode1, exclNQN(exclNode1), mnro, true)},
			exclPublishReq(blockID, exclNode2, mnro, true), codes.OK, 2},
		{"block protocol rejects MULTI_NODE_MULTI_WRITER", blockID, nil,
			exclPublishReq(blockID, exclNode1, mnmw, false), codes.InvalidArgument, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			objs := append(exclCSINodes(), volumeStateFor(tc.volumeID, tc.existing...))
			env := newPublishTestEnv(t, objs...)

			_, err := env.srv.ControllerPublishVolume(context.Background(), tc.req)
			if got := status.Code(err); got != tc.wantCode {
				t.Fatalf("ControllerPublishVolume code = %v (err=%v), want %v", got, err, tc.wantCode)
			}
			wantAllow := 0
			if tc.wantCode == codes.OK {
				wantAllow = 1
			}
			if env.agent.allowInitiatorCalls != wantAllow {
				t.Errorf("AllowInitiator calls = %d, want %d", env.agent.allowInitiatorCalls, wantAllow)
			}
			if got := exclPublishedNodes(t, env.srv.k8sClient, tc.volumeID); len(got) != tc.wantRecords {
				t.Errorf("publishedNodes = %+v, want %d records", got, tc.wantRecords)
			}
		})
	}
}

// TestControllerPublishVolume_UnknownVolume_NotFound verifies that publishing
// a volume without a PillarVolumeState returns NotFound and grants nothing.
func TestControllerPublishVolume_UnknownVolume_NotFound(t *testing.T) {
	t.Parallel()

	env := newPublishTestEnv(t, exclCSINodes()...)
	_, err := env.srv.ControllerPublishVolume(context.Background(), basePublishRequest())
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v (err=%v), want NotFound", status.Code(err), err)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0", env.agent.allowInitiatorCalls)
	}
}

// TestControllerPublishVolume_AllowFailureKeepsReservation verifies the
// fail-closed ordering: the publication is recorded before AllowInitiator, so
// a failed grant still blocks a second node while a retry on the same node
// succeeds once the agent recovers.
func TestControllerPublishVolume_AllowFailureKeepsReservation(t *testing.T) {
	t.Parallel()

	volumeID := basePublishRequest().GetVolumeId()
	env := newPublishTestEnv(t, append(exclCSINodes(), volumeStateFor(volumeID))...)
	ctx := context.Background()
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

	env.agent.allowInitiatorErr = status.Error(codes.Internal, "configfs write failed")
	if _, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode1, mode, false)); err == nil {
		t.Fatal("expected AllowInitiator failure to propagate")
	}
	if got := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(got) != 1 || got[0].NodeID != exclNode1 {
		t.Fatalf("publishedNodes after failed grant = %+v, want reservation for %s", got, exclNode1)
	}

	env.agent.allowInitiatorErr = nil
	_, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode2, mode, false))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second node code = %v (err=%v), want FailedPrecondition", status.Code(err), err)
	}
	if _, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode1, mode, false)); err != nil {
		t.Fatalf("same-node retry after agent recovery: %v", err)
	}
}

// TestControllerPublishVolume_ConcurrentNodes_ExactlyOneWins verifies that two
// concurrent publishes of a SINGLE_NODE_WRITER volume to different nodes
// grant exactly one node, both within one controller (per-volume lock) and
// across two controller instances sharing the API server (resourceVersion
// compare-and-swap).
func TestControllerPublishVolume_ConcurrentNodes_ExactlyOneWins(t *testing.T) {
	t.Parallel()

	for _, twoControllers := range []bool{false, true} {
		name := "single controller"
		if twoControllers {
			name = "two controllers"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			volumeID := basePublishRequest().GetVolumeId()
			env := newPublishTestEnv(t, append(exclCSINodes(), volumeStateFor(volumeID))...)
			servers := []*ControllerServer{env.srv, env.srv}
			if twoControllers {
				second := &mockAgentClient{}
				servers[1] = NewControllerServerWithDialer(env.srv.k8sClient, "pillar-csi.bhyoo.com",
					func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
						return second, nopCloser{}, nil
					})
			}

			mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
			nodes := []string{exclNode1, exclNode2}
			errs := make([]error, len(nodes))
			var wg sync.WaitGroup
			for i, node := range nodes {
				wg.Go(func() {
					_, errs[i] = servers[i].ControllerPublishVolume(context.Background(),
						exclPublishReq(volumeID, node, mode, false))
				})
			}
			wg.Wait()

			wins := exclCountWinners(t, errs)
			if wins != 1 {
				t.Fatalf("successful publishes = %d (errs=%v), want exactly 1", wins, errs)
			}
			if got := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(got) != 1 {
				t.Errorf("publishedNodes = %+v, want exactly one record", got)
			}
		})
	}
}

// TestControllerPublishVolume_ExclusivitySurvivesRestart verifies that the
// publication record, not in-memory state, enforces exclusivity: a fresh
// controller that reloads state from PillarVolumeStates rejects a second node
// and restores the ControllerPublished state.
func TestControllerPublishVolume_ExclusivitySurvivesRestart(t *testing.T) {
	t.Parallel()

	volumeID := basePublishRequest().GetVolumeId()
	env := newPublishTestEnv(t, append(exclCSINodes(), volumeStateFor(volumeID))...)
	ctx := context.Background()
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	if _, err := env.srv.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode1, mode, false)); err != nil {
		t.Fatalf("publish node 1: %v", err)
	}

	restarted := NewControllerServerWithDialer(env.srv.k8sClient, "pillar-csi.bhyoo.com", env.srv.dialAgent)
	if err := restarted.LoadStateFromPillarVolumeStates(ctx); err != nil {
		t.Fatalf("LoadStateFromPillarVolumeStates: %v", err)
	}
	if got := restarted.GetStateMachine().GetState(volumeID); got != StateControllerPublished {
		t.Errorf("restored state = %v, want ControllerPublished", got)
	}
	_, err := restarted.ControllerPublishVolume(ctx, exclPublishReq(volumeID, exclNode2, mode, false))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("publish node 2 after restart code = %v (err=%v), want FailedPrecondition", status.Code(err), err)
	}
}

// TestDeleteVolume_PublishedVolume_FailedPrecondition verifies that a volume
// with a publication record is not deleted and no agent teardown runs, and
// that deletion proceeds once the node is unpublished.
func TestDeleteVolume_PublishedVolume_FailedPrecondition(t *testing.T) {
	t.Parallel()

	volumeID := basePublishRequest().GetVolumeId()
	mode := csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	env := newPublishTestEnv(t, append(exclCSINodes(),
		volumeStateFor(volumeID, exclPub(exclNode1, exclNQN(exclNode1), mode, false)))...)
	ctx := context.Background()

	_, err := env.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume code = %v (err=%v), want FailedPrecondition", status.Code(err), err)
	}
	if env.agent.unexportVolumeCalls != 0 || env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("agent teardown ran for a published volume: unexport=%d delete=%d",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}

	if _, err := env.srv.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: exclNode1,
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if _, err := env.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume after unpublish: %v", err)
	}
	if env.agent.deleteVolumeCalls != 1 {
		t.Errorf("agent DeleteVolume calls = %d, want 1", env.agent.deleteVolumeCalls)
	}
}
