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

// Package e2e contains lightweight end-to-end tests for the pillar-csi CSI
// Controller and Node services, in addition to the existing agent gRPC tests.
//
// This file defines the shared test infrastructure used by both
// csi_controller_e2e_test.go and csi_node_e2e_test.go:
//
//   - mockAgentServer: a programmable gRPC server double that implements
//     the AgentService RPC set (CreateVolume, DeleteVolume, ExpandVolume,
//     ExportVolume, UnexportVolume, AllowInitiator, DenyInitiator) with
//     configurable responses, injected errors, and full call recording.
//
//   - csiControllerE2EEnv: wires the CSI ControllerServer against a real
//     gRPC listener backed by mockAgentServer and a fake Kubernetes client
//     pre-populated with a PillarTarget pointing at the mock listener.
//     No real Kubernetes cluster or network storage agent is required.
//
//   - mockCSIConnector: a test double for the csi.Connector interface that
//     records Connect / Disconnect / GetDevicePath calls without touching
//     the kernel NVMe-oF stack.
//
//   - mockCSIMounter: a test double for the csi.Mounter interface that
//     maintains an in-memory mount table and records all formatting/mounting
//     operations without calling mount(8) or mkfs(8).
//
//   - csiNodeE2EEnv: wires the CSI NodeServer with the above mock connector
//     and mounter, providing an isolated per-test staging state directory.
//
// Run lightweight CSI e2e tests (no build tag required):
//
//	go test ./test/e2e/ -v -run TestCSI
package e2e

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Call record types
// ─────────────────────────────────────────────────────────────────────────────

// agentCreateVolumeCall records the arguments of a CreateVolume invocation.
type agentCreateVolumeCall struct {
	VolumeID      string
	CapacityBytes int64
	BackendType   agentv1.BackendType
}

// agentDeleteVolumeCall records the arguments of a DeleteVolume invocation.
type agentDeleteVolumeCall struct {
	VolumeID    string
	BackendType agentv1.BackendType
}

// agentExpandVolumeCall records the arguments of an ExpandVolume invocation.
type agentExpandVolumeCall struct {
	VolumeID       string
	RequestedBytes int64
}

// agentExportVolumeCall records the arguments of an ExportVolume invocation.
type agentExportVolumeCall struct {
	VolumeID     string
	DevicePath   string
	ProtocolType agentv1.ProtocolType
}

// agentUnexportVolumeCall records the arguments of an UnexportVolume invocation.
type agentUnexportVolumeCall struct {
	VolumeID     string
	ProtocolType agentv1.ProtocolType
}

// agentAllowInitiatorCall records the arguments of an AllowInitiator invocation.
type agentAllowInitiatorCall struct {
	VolumeID     string
	ProtocolType agentv1.ProtocolType
	InitiatorID  string
}

// agentDenyInitiatorCall records the arguments of a DenyInitiator invocation.
type agentDenyInitiatorCall struct {
	VolumeID     string
	ProtocolType agentv1.ProtocolType
	InitiatorID  string
}

// ─────────────────────────────────────────────────────────────────────────────
// mockAgentServer
// ─────────────────────────────────────────────────────────────────────────────

// mockAgentServer is a programmable gRPC server double for the AgentService.
//
// Each RPC method can be configured with a custom error and/or response.
// All invocations are recorded in the corresponding *Calls slice so tests can
// assert on them.
//
// Methods not overridden here are handled by the embedded
// UnimplementedAgentServiceServer (returns codes.Unimplemented).
type mockAgentServer struct {
	agentv1.UnimplementedAgentServiceServer

	mu sync.Mutex

	// ── Configurable errors ──────────────────────────────────────────────────

	CreateVolumeErr   error
	DeleteVolumeErr   error
	ExpandVolumeErr   error
	ExportVolumeErr   error
	UnexportVolumeErr error
	AllowInitiatorErr error
	DenyInitiatorErr  error

	// ── Configurable responses ───────────────────────────────────────────────

	// CreateVolumeDevicePath is the DevicePath returned in CreateVolumeResponse.
	// Defaults to "/dev/test-device".
	CreateVolumeDevicePath string

	// CreateVolumeCapacityBytes is the CapacityBytes returned in
	// CreateVolumeResponse.  If zero the requested capacity is echoed back.
	CreateVolumeCapacityBytes int64

	// ExportVolumeInfo is the ExportInfo returned in ExportVolumeResponse.
	// If nil a default info struct is returned.
	ExportVolumeInfo *agentv1.ExportInfo

	// ExpandVolumeCapacityBytes is returned in ExpandVolumeResponse.
	// If zero the requested capacity is echoed back.
	ExpandVolumeCapacityBytes int64

	// ── Recorded calls ───────────────────────────────────────────────────────

	CreateVolumeCalls   []agentCreateVolumeCall
	DeleteVolumeCalls   []agentDeleteVolumeCall
	ExpandVolumeCalls   []agentExpandVolumeCall
	ExportVolumeCalls   []agentExportVolumeCall
	UnexportVolumeCalls []agentUnexportVolumeCall
	AllowInitiatorCalls []agentAllowInitiatorCall
	DenyInitiatorCalls  []agentDenyInitiatorCall
}

// Compile-time interface check.
var _ agentv1.AgentServiceServer = (*mockAgentServer)(nil)

// newMockAgentServer returns a *mockAgentServer with sensible defaults.
//
// The default ExportVolumeInfo contains:
//
//	TargetId  = "nqn.2026-01.com.bhyoo.pillar-csi:test-volume"
//	Address   = "127.0.0.1"
//	Port      = 4420
//	VolumeRef = "test-volume"
func newMockAgentServer() *mockAgentServer {
	return &mockAgentServer{
		CreateVolumeDevicePath: "/dev/test-device",
		ExportVolumeInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:test-volume",
			Address:   "127.0.0.1",
			Port:      4420,
			VolumeRef: "test-volume",
		},
	}
}

// CreateVolume implements AgentServiceServer.
func (m *mockAgentServer) CreateVolume(
	_ context.Context,
	req *agentv1.CreateVolumeRequest,
) (*agentv1.CreateVolumeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.CreateVolumeCalls = append(m.CreateVolumeCalls, agentCreateVolumeCall{
		VolumeID:      req.GetVolumeId(),
		CapacityBytes: req.GetCapacityBytes(),
		BackendType:   req.GetBackendType(),
	})

	if m.CreateVolumeErr != nil {
		return nil, m.CreateVolumeErr
	}

	capacity := m.CreateVolumeCapacityBytes
	if capacity == 0 {
		capacity = req.GetCapacityBytes()
	}
	devPath := m.CreateVolumeDevicePath
	if devPath == "" {
		devPath = "/dev/test-device"
	}

	return &agentv1.CreateVolumeResponse{
		DevicePath:    devPath,
		CapacityBytes: capacity,
	}, nil
}

// DeleteVolume implements AgentServiceServer.
func (m *mockAgentServer) DeleteVolume(
	_ context.Context,
	req *agentv1.DeleteVolumeRequest,
) (*agentv1.DeleteVolumeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.DeleteVolumeCalls = append(m.DeleteVolumeCalls, agentDeleteVolumeCall{
		VolumeID:    req.GetVolumeId(),
		BackendType: req.GetBackendType(),
	})

	if m.DeleteVolumeErr != nil {
		return nil, m.DeleteVolumeErr
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

// ExpandVolume implements AgentServiceServer.
func (m *mockAgentServer) ExpandVolume(
	_ context.Context,
	req *agentv1.ExpandVolumeRequest,
) (*agentv1.ExpandVolumeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ExpandVolumeCalls = append(m.ExpandVolumeCalls, agentExpandVolumeCall{
		VolumeID:       req.GetVolumeId(),
		RequestedBytes: req.GetRequestedBytes(),
	})

	if m.ExpandVolumeErr != nil {
		return nil, m.ExpandVolumeErr
	}

	capacity := m.ExpandVolumeCapacityBytes
	if capacity == 0 {
		capacity = req.GetRequestedBytes()
	}

	return &agentv1.ExpandVolumeResponse{
		CapacityBytes: capacity,
	}, nil
}

// ExportVolume implements AgentServiceServer.
func (m *mockAgentServer) ExportVolume(
	_ context.Context,
	req *agentv1.ExportVolumeRequest,
) (*agentv1.ExportVolumeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.ExportVolumeCalls = append(m.ExportVolumeCalls, agentExportVolumeCall{
		VolumeID:     req.GetVolumeId(),
		DevicePath:   req.GetDevicePath(),
		ProtocolType: req.GetProtocolType(),
	})

	if m.ExportVolumeErr != nil {
		return nil, m.ExportVolumeErr
	}

	info := m.ExportVolumeInfo
	if info == nil {
		info = &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:test-volume",
			Address:   "127.0.0.1",
			Port:      4420,
			VolumeRef: req.GetVolumeId(),
		}
	}

	return &agentv1.ExportVolumeResponse{
		ExportInfo: info,
	}, nil
}

// UnexportVolume implements AgentServiceServer.
func (m *mockAgentServer) UnexportVolume(
	_ context.Context,
	req *agentv1.UnexportVolumeRequest,
) (*agentv1.UnexportVolumeResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.UnexportVolumeCalls = append(m.UnexportVolumeCalls, agentUnexportVolumeCall{
		VolumeID:     req.GetVolumeId(),
		ProtocolType: req.GetProtocolType(),
	})

	if m.UnexportVolumeErr != nil {
		return nil, m.UnexportVolumeErr
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

// AllowInitiator implements AgentServiceServer.
func (m *mockAgentServer) AllowInitiator(
	_ context.Context,
	req *agentv1.AllowInitiatorRequest,
) (*agentv1.AllowInitiatorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.AllowInitiatorCalls = append(m.AllowInitiatorCalls, agentAllowInitiatorCall{
		VolumeID:     req.GetVolumeId(),
		ProtocolType: req.GetProtocolType(),
		InitiatorID:  req.GetInitiatorId(),
	})

	if m.AllowInitiatorErr != nil {
		return nil, m.AllowInitiatorErr
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}

// DenyInitiator implements AgentServiceServer.
func (m *mockAgentServer) DenyInitiator(
	_ context.Context,
	req *agentv1.DenyInitiatorRequest,
) (*agentv1.DenyInitiatorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.DenyInitiatorCalls = append(m.DenyInitiatorCalls, agentDenyInitiatorCall{
		VolumeID:     req.GetVolumeId(),
		ProtocolType: req.GetProtocolType(),
		InitiatorID:  req.GetInitiatorId(),
	})

	if m.DenyInitiatorErr != nil {
		return nil, m.DenyInitiatorErr
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// csiControllerE2EEnv
// ─────────────────────────────────────────────────────────────────────────────

// csiControllerE2EEnv is a self-contained test environment for the CSI
// ControllerServer.
//
// It comprises:
//   - a mockAgentServer listening on a real TCP port (localhost:0)
//   - a fake Kubernetes client pre-populated with a PillarTarget whose
//     Status.ResolvedAddress points at the mock listener
//   - a ControllerServer wired to the fake client and a dialer that
//     reaches the mock listener
//
// No real Kubernetes cluster, NVMe-oF kernel modules, or external processes
// are required.
type csiControllerE2EEnv struct {
	// Controller is the CSI ControllerServer under test.
	Controller *csisrv.ControllerServer

	// AgentMock holds the programmable mock that the ControllerServer dials.
	AgentMock *mockAgentServer

	// TargetName is the name of the pre-created PillarTarget in the fake
	// Kubernetes client.  Tests use this when constructing CSI requests that
	// need to name a target (e.g. in StorageClass parameters).
	TargetName string

	// AgentAddr is the "host:port" of the mock gRPC listener.
	AgentAddr string

	// K8sClient is the fake Kubernetes client backing the ControllerServer.
	// Tests may use it to read or verify PillarVolume CRD objects that the
	// controller creates during CreateVolume / DeleteVolume.
	K8sClient client.Client

	grpcSrv *grpc.Server
}

// newCSIControllerE2EEnv creates a csiControllerE2EEnv for the duration of a
// single test.  Cleanup (gRPC server stop) is registered via t.Cleanup.
//
// targetName is the Kubernetes name assigned to the PillarTarget that the
// ControllerServer will look up; pass any non-empty string, e.g. "storage-1".
func newCSIControllerE2EEnv(t *testing.T, targetName string) *csiControllerE2EEnv {
	t.Helper()

	// 1. Build the mock agent gRPC server.
	mockAgent := newMockAgentServer()
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, mockAgent)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("csiControllerE2EEnv: net.Listen: %v", err)
	}
	agentAddr := lis.Addr().String()

	go func() { _ = grpcSrv.Serve(lis) }()

	t.Cleanup(func() {
		grpcSrv.GracefulStop()
	})

	// 2. Build a fake Kubernetes client with the PillarTarget pre-loaded.
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("csiControllerE2EEnv: AddToScheme: %v", err)
	}

	pillarTarget := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name: targetName,
		},
		Spec: v1alpha1.PillarTargetSpec{
			External: &v1alpha1.ExternalSpec{
				Address: "127.0.0.1",
				Port:    4500, // any port — real address is in Status
			},
		},
		Status: v1alpha1.PillarTargetStatus{
			ResolvedAddress: agentAddr,
		},
	}

	// Without WithStatusSubresource for PillarTarget, WithObjects stores the
	// full object (including status) as-is, so Get() returns the populated
	// Status.  PillarVolume needs WithStatusSubresource so that
	// Status().Update() works correctly (the controller persists partial-failure
	// state via the status subresource).
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pillarTarget).
		WithStatusSubresource(&v1alpha1.PillarVolume{}).
		Build()

	// 3. Build the AgentDialer closure that connects to our mock listener.
	dialer := func(_ context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
		conn, dialErr := grpc.NewClient(
			addr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if dialErr != nil {
			return nil, nil, dialErr
		}
		return agentv1.NewAgentServiceClient(conn), conn, nil
	}

	// 5. Construct the ControllerServer.
	srv := csisrv.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)

	return &csiControllerE2EEnv{
		Controller: srv,
		AgentMock:  mockAgent,
		TargetName: targetName,
		AgentAddr:  agentAddr,
		K8sClient:  fakeClient,
		grpcSrv:    grpcSrv,
	}
}

// defaultCreateVolumeParams returns a minimal set of StorageClass-style
// parameters for a CreateVolume CSI request targeting this environment's
// PillarTarget.  Tests may override individual keys after calling this helper.
//
//	target      = env.TargetName
//	backend-type = "zfs-zvol"
//	protocol-type = "nvmeof-tcp"
//	zfs-pool    = "tank"
func (e *csiControllerE2EEnv) defaultCreateVolumeParams() map[string]string {
	return map[string]string{
		"pillar-csi.bhyoo.com/target":        e.TargetName,
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
		"pillar-csi.bhyoo.com/zfs-pool":      "tank",
	}
}

// defaultVolumeCapabilities returns a single SINGLE_NODE_WRITER + Mount
// capability, sufficient for most tests.
func defaultVolumeCapabilities() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// mockCSIConnector
// ─────────────────────────────────────────────────────────────────────────────

// connectCall records the arguments of a single Connector.Connect invocation.
type connectCall struct {
	SubsysNQN string
	TrAddr    string
	TrSvcID   string
}

// mockCSIConnector is a test double for the csi.Connector interface.
//
// It records every Connect / Disconnect / GetDevicePath call without touching
// the NVMe-oF kernel stack.  Configure DevicePath to control what
// GetDevicePath returns; leave it empty to simulate a device that has not yet
// appeared (useful for testing timeout behaviour).
type mockCSIConnector struct {
	mu sync.Mutex

	// ConnectErr is returned by Connect, or nil for success.
	ConnectErr error
	// DisconnectErr is returned by Disconnect, or nil for success.
	DisconnectErr error
	// DevicePath is returned by GetDevicePath once the device is "ready".
	// GetDevicePathErr takes precedence if set.
	DevicePath string
	// GetDevicePathErr, when non-nil, is returned by GetDevicePath instead of DevicePath.
	GetDevicePathErr error

	// Recorded calls.
	ConnectCalls    []connectCall
	DisconnectCalls []string // subsystem NQNs
	GetDeviceCalls  []string // subsystem NQNs
}

// Compile-time interface check.
var _ csisrv.Connector = (*mockCSIConnector)(nil)

// Connect implements Connector.
func (m *mockCSIConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ConnectCalls = append(m.ConnectCalls, connectCall{
		SubsysNQN: subsysNQN,
		TrAddr:    trAddr,
		TrSvcID:   trSvcID,
	})
	return m.ConnectErr
}

// Disconnect implements Connector.
func (m *mockCSIConnector) Disconnect(_ context.Context, subsysNQN string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.DisconnectCalls = append(m.DisconnectCalls, subsysNQN)
	return m.DisconnectErr
}

// GetDevicePath implements Connector.
func (m *mockCSIConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GetDeviceCalls = append(m.GetDeviceCalls, subsysNQN)
	if m.GetDevicePathErr != nil {
		return "", m.GetDevicePathErr
	}
	return m.DevicePath, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// mockCSIMounter
// ─────────────────────────────────────────────────────────────────────────────

// formatAndMountCall records arguments of a single FormatAndMount invocation.
type formatAndMountCall struct {
	Source  string
	Target  string
	FsType  string
	Options []string
}

// mountCall records arguments of a single Mount invocation.
type mountCall struct {
	Source  string
	Target  string
	FsType  string
	Options []string
}

// mockCSIMounter is a test double for the csi.Mounter interface.
//
// It maintains an in-memory mount table (mountedPaths) that drives
// IsMounted without performing any real system calls.  All formatting and
// mounting operations update the table so tests can assert on mount state
// without root privileges.
type mockCSIMounter struct {
	mu sync.Mutex

	// mountedPaths is the set of target paths currently considered "mounted".
	mountedPaths map[string]bool

	// Configurable errors (nil = success).
	FormatAndMountErr error
	MountErr          error
	UnmountErr        error
	IsMountedErr      error

	// Recorded calls.
	FormatAndMountCalls []formatAndMountCall
	MountCalls          []mountCall
	UnmountCalls        []string // target paths
	IsMountedCalls      []string // target paths
}

// Compile-time interface check.
var _ csisrv.Mounter = (*mockCSIMounter)(nil)

// newMockCSIMounter returns a mockCSIMounter with an initialised mount table.
func newMockCSIMounter() *mockCSIMounter {
	return &mockCSIMounter{
		mountedPaths: make(map[string]bool),
	}
}

// FormatAndMount implements Mounter.
func (m *mockCSIMounter) FormatAndMount(source, target, fsType string, options []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.FormatAndMountCalls = append(m.FormatAndMountCalls, formatAndMountCall{
		Source:  source,
		Target:  target,
		FsType:  fsType,
		Options: options,
	})
	if m.FormatAndMountErr != nil {
		return m.FormatAndMountErr
	}
	m.mountedPaths[target] = true
	return nil
}

// Mount implements Mounter.
func (m *mockCSIMounter) Mount(source, target, fsType string, options []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MountCalls = append(m.MountCalls, mountCall{
		Source:  source,
		Target:  target,
		FsType:  fsType,
		Options: options,
	})
	if m.MountErr != nil {
		return m.MountErr
	}
	m.mountedPaths[target] = true
	return nil
}

// Unmount implements Mounter.
func (m *mockCSIMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UnmountCalls = append(m.UnmountCalls, target)
	if m.UnmountErr != nil {
		return m.UnmountErr
	}
	delete(m.mountedPaths, target)
	return nil
}

// IsMounted implements Mounter.
func (m *mockCSIMounter) IsMounted(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IsMountedCalls = append(m.IsMountedCalls, target)
	if m.IsMountedErr != nil {
		return false, m.IsMountedErr
	}
	return m.mountedPaths[target], nil
}

// ─────────────────────────────────────────────────────────────────────────────
// csiNodeE2EEnv
// ─────────────────────────────────────────────────────────────────────────────

// csiNodeE2EEnv is a self-contained test environment for the CSI NodeServer.
//
// It provides:
//   - a NodeServer with injectable mock Connector and Mounter
//   - an isolated per-test staging state directory (t.TempDir)
//   - convenience methods for building CSI Node requests
//
// No real NVMe-oF kernel modules, block devices, or root privileges are needed.
type csiNodeE2EEnv struct {
	// Node is the CSI NodeServer under test.
	Node *csisrv.NodeServer

	// Connector is the mock NVMe-oF connector.  Tests configure its fields
	// before calling Node methods.
	Connector *mockCSIConnector

	// Mounter is the mock filesystem mounter.  Tests configure its fields
	// before calling Node methods.
	Mounter *mockCSIMounter

	// StateDir is the per-test temporary staging state directory.
	StateDir string

	// NodeID is the node identifier registered in the NodeServer.
	NodeID string
}

// newCSINodeE2EEnv creates a csiNodeE2EEnv for the duration of a single test.
//
// nodeID is the Kubernetes node name embedded in NodeGetInfo responses and
// used by the CO to route ControllerPublishVolume calls.  Pass any non-empty
// string, e.g. "worker-1".
func newCSINodeE2EEnv(t *testing.T, nodeID string) *csiNodeE2EEnv {
	t.Helper()

	stateDir := t.TempDir()
	connector := &mockCSIConnector{}
	mounter := newMockCSIMounter()

	node := csisrv.NewNodeServerWithStateDir(nodeID, connector, mounter, stateDir)

	return &csiNodeE2EEnv{
		Node:      node,
		Connector: connector,
		Mounter:   mounter,
		StateDir:  stateDir,
		NodeID:    nodeID,
	}
}

// defaultStageVolumeContext returns a VolumeContext map containing the minimum
// set of keys expected by NodeStageVolume.
//
//	target_id = subsysNQN
//	address   = "127.0.0.1"
//	port      = "4420"
func defaultStageVolumeContext(subsysNQN string) map[string]string {
	return map[string]string{
		csisrv.VolumeContextKeyTargetNQN: subsysNQN,
		csisrv.VolumeContextKeyAddress:   "127.0.0.1",
		csisrv.VolumeContextKeyPort:      "4420",
	}
}

// mountVolumeCapability returns a VolumeCapability with MOUNT access type and
// the given filesystem type and access mode.
func mountVolumeCapability(
	fsType string,
	mode csi.VolumeCapability_AccessMode_Mode,
) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: fsType},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// blockVolumeCapability returns a VolumeCapability with BLOCK access type and
// the given access mode.
func blockVolumeCapability(
	mode csi.VolumeCapability_AccessMode_Mode,
) *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// csiOrderedLifecycleEnv
// ─────────────────────────────────────────────────────────────────────────────

// newCSILifecycleEnvWithSM creates a csiLifecycleEnv in which the CSI
// ControllerServer and CSI NodeServer share a single VolumeStateMachine.
//
// With a shared SM both services enforce per-operation ordering constraints:
//   - NodeStageVolume returns FailedPrecondition unless ControllerPublishVolume
//     has already been called for the same volumeID.
//   - NodePublishVolume returns FailedPrecondition unless NodeStageVolume
//     has already been called.
//   - NodeUnstageVolume returns FailedPrecondition if NodeUnpublishVolume
//     has not yet been called (i.e. the volume is still in NodePublished state).
//
// Use this variant for negative ordering tests.  For positive lifecycle tests
// and unit tests that do not require cross-component ordering validation, use
// newCSILifecycleEnv instead (NodeServer has no SM → no ordering guards).
func newCSILifecycleEnvWithSM(t *testing.T, targetName, nodeID string) *csiLifecycleEnv {
	t.Helper()

	// Build the controller environment (includes mock agent gRPC listener).
	ctrl := newCSIControllerE2EEnv(t, targetName)

	// Configure the mock agent's ExportInfo so the NQN/address/port from
	// CreateVolume flow through to NodeStageVolume correctly.
	ctrl.AgentMock.ExportVolumeInfo = &agentv1.ExportInfo{
		TargetId:  lifecycleTestNQN,
		Address:   lifecycleTestAddress,
		Port:      lifecycleTestPort,
		VolumeRef: lifecycleTestVolumeRef,
	}
	ctrl.AgentMock.CreateVolumeDevicePath = lifecycleTestDevicePath

	// Retrieve the shared VolumeStateMachine from the controller.
	// Both controller and node will consult this same SM instance.
	sharedSM := ctrl.Controller.GetStateMachine()

	// Build the node environment with the shared SM so NodeServer operations
	// validate lifecycle state against the same machine the controller updates.
	stateDir := t.TempDir()
	connector := &mockCSIConnector{DevicePath: lifecycleTestDevicePath}
	mounter := newMockCSIMounter()
	node := csisrv.NewNodeServerWithStateMachine(nodeID, connector, mounter, stateDir, sharedSM)

	return &csiLifecycleEnv{
		Controller: ctrl.Controller,
		Node:       node,
		AgentMock:  ctrl.AgentMock,
		Connector:  connector,
		Mounter:    mounter,
		TargetName: targetName,
		NodeID:     nodeID,
		StateDir:   stateDir,
	}
}
