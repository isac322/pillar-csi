package e2e

// inprocess_fake_agent.go — fakeAgentServer for in-process E2E TCs.
//
// fakeAgentServer implements agentv1.AgentServiceServer with configurable
// per-operation responses and call counters. Unlike the old localMockAgentClient
// it is registered with a real gRPC server and communicates via real
// gRPC transport (bufconn), so serialization/deserialization is exercised.

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// fakeAgentServer implements agentv1.AgentServiceServer with configurable
// per-operation responses and call counters.
type fakeAgentServer struct {
	agentv1.UnimplementedAgentServiceServer
	mu sync.Mutex

	// Configurable responses (nil = use default success response)
	createVolumeResp  *agentv1.CreateVolumeResponse
	createVolumeErr   error
	exportVolumeResp  *agentv1.ExportVolumeResponse
	exportVolumeErr   error
	unexportVolumeErr error
	deleteVolumeErr   error
	allowInitiatorErr error
	denyInitiatorErr  error
	expandVolumeResp  *agentv1.ExpandVolumeResponse
	expandVolumeErr   error
	getCapacityResp   *agentv1.GetCapacityResponse
	getCapacityErr    error

	// Call counters (read-safe under mu)
	createVolumeCalls   int
	exportVolumeCalls   int
	unexportVolumeCalls int
	deleteVolumeCalls   int
	allowInitiatorCalls int
	denyInitiatorCalls  int
	expandVolumeCalls   int
	getCapacityCalls    int

	// Recorded requests for detailed assertions
	createVolumeReqs   []*agentv1.CreateVolumeRequest
	exportVolumeReqs   []*agentv1.ExportVolumeRequest
	allowInitiatorReqs []*agentv1.AllowInitiatorRequest
	denyInitiatorReqs  []*agentv1.DenyInitiatorRequest
}

var _ agentv1.AgentServiceServer = (*fakeAgentServer)(nil)

func newFakeAgentServer() *fakeAgentServer {
	return &fakeAgentServer{
		createVolumeResp: &agentv1.CreateVolumeResponse{
			DevicePath:    "/dev/zvol/tank/pvc-fake",
			CapacityBytes: 1 << 30,
		},
		exportVolumeResp: &agentv1.ExportVolumeResponse{
			ExportInfo: &agentv1.ExportInfo{
				TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-fake",
				Address:   "127.0.0.1",
				Port:      4420,
				VolumeRef: "tank/pvc-fake",
			},
		},
		expandVolumeResp: &agentv1.ExpandVolumeResponse{
			CapacityBytes: 2 << 30,
		},
		getCapacityResp: &agentv1.GetCapacityResponse{
			TotalBytes:     100 << 30,
			AvailableBytes: 60 << 30,
			UsedBytes:      40 << 30,
		},
	}
}

func (s *fakeAgentServer) CreateVolume(_ context.Context, req *agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createVolumeCalls++
	s.createVolumeReqs = append(s.createVolumeReqs, req)
	if s.createVolumeErr != nil {
		return nil, s.createVolumeErr
	}
	if s.createVolumeResp != nil {
		return s.createVolumeResp, nil
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/zvol/tank/" + req.GetVolumeId(),
		CapacityBytes: req.GetCapacityBytes(),
	}, nil
}

func (s *fakeAgentServer) DeleteVolume(_ context.Context, _ *agentv1.DeleteVolumeRequest) (*agentv1.DeleteVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteVolumeCalls++
	if s.deleteVolumeErr != nil {
		return nil, s.deleteVolumeErr
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

func (s *fakeAgentServer) ExportVolume(_ context.Context, req *agentv1.ExportVolumeRequest) (*agentv1.ExportVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exportVolumeCalls++
	s.exportVolumeReqs = append(s.exportVolumeReqs, req)
	if s.exportVolumeErr != nil {
		return nil, s.exportVolumeErr
	}
	if s.exportVolumeResp != nil {
		return s.exportVolumeResp, nil
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:tank.default",
			Address:   "127.0.0.1",
			Port:      4420,
			VolumeRef: req.GetVolumeId(),
		},
	}, nil
}

func (s *fakeAgentServer) UnexportVolume(_ context.Context, _ *agentv1.UnexportVolumeRequest) (*agentv1.UnexportVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unexportVolumeCalls++
	if s.unexportVolumeErr != nil {
		return nil, s.unexportVolumeErr
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

func (s *fakeAgentServer) AllowInitiator(_ context.Context, req *agentv1.AllowInitiatorRequest) (*agentv1.AllowInitiatorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowInitiatorCalls++
	s.allowInitiatorReqs = append(s.allowInitiatorReqs, req)
	if s.allowInitiatorErr != nil {
		return nil, s.allowInitiatorErr
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}

func (s *fakeAgentServer) DenyInitiator(_ context.Context, req *agentv1.DenyInitiatorRequest) (*agentv1.DenyInitiatorResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denyInitiatorCalls++
	s.denyInitiatorReqs = append(s.denyInitiatorReqs, req)
	if s.denyInitiatorErr != nil {
		return nil, s.denyInitiatorErr
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}

func (s *fakeAgentServer) ExpandVolume(_ context.Context, _ *agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expandVolumeCalls++
	if s.expandVolumeErr != nil {
		return nil, s.expandVolumeErr
	}
	if s.expandVolumeResp != nil {
		return s.expandVolumeResp, nil
	}
	return &agentv1.ExpandVolumeResponse{CapacityBytes: 2 << 30}, nil
}

func (s *fakeAgentServer) GetCapacity(_ context.Context, _ *agentv1.GetCapacityRequest) (*agentv1.GetCapacityResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCapacityCalls++
	if s.getCapacityErr != nil {
		return nil, s.getCapacityErr
	}
	if s.getCapacityResp != nil {
		return s.getCapacityResp, nil
	}
	return &agentv1.GetCapacityResponse{
		TotalBytes:     100 << 30,
		AvailableBytes: 60 << 30,
	}, nil
}

func (s *fakeAgentServer) HealthCheck(_ context.Context, _ *agentv1.HealthCheckRequest) (*agentv1.HealthCheckResponse, error) {
	return &agentv1.HealthCheckResponse{Healthy: true, AgentVersion: "fake-0.1.0"}, nil
}

func (*fakeAgentServer) GetCapabilities(_ context.Context, _ *agentv1.GetCapabilitiesRequest) (*agentv1.GetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fake agent: GetCapabilities")
}

func (*fakeAgentServer) ListVolumes(_ context.Context, _ *agentv1.ListVolumesRequest) (*agentv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fake agent: ListVolumes")
}

func (*fakeAgentServer) ListExports(_ context.Context, _ *agentv1.ListExportsRequest) (*agentv1.ListExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fake agent: ListExports")
}

func (*fakeAgentServer) ReconcileState(_ context.Context, _ *agentv1.ReconcileStateRequest) (*agentv1.ReconcileStateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "fake agent: ReconcileState")
}

func (*fakeAgentServer) SendVolume(_ *agentv1.SendVolumeRequest, _ grpc.ServerStreamingServer[agentv1.SendVolumeChunk]) error {
	return status.Error(codes.Unimplemented, "fake agent: SendVolume")
}

func (*fakeAgentServer) ReceiveVolume(_ grpc.ClientStreamingServer[agentv1.ReceiveVolumeChunk, agentv1.ReceiveVolumeResponse]) error {
	return status.Error(codes.Unimplemented, "fake agent: ReceiveVolume")
}

// resetCounts zeroes all call counters (but not responses/errors).
func (s *fakeAgentServer) resetCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createVolumeCalls = 0
	s.exportVolumeCalls = 0
	s.unexportVolumeCalls = 0
	s.deleteVolumeCalls = 0
	s.allowInitiatorCalls = 0
	s.denyInitiatorCalls = 0
	s.expandVolumeCalls = 0
	s.getCapacityCalls = 0
	s.createVolumeReqs = nil
	s.exportVolumeReqs = nil
	s.allowInitiatorReqs = nil
	s.denyInitiatorReqs = nil
}

// counts returns a snapshot of all call counters.
func (s *fakeAgentServer) counts() fakeAgentCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fakeAgentCounts{
		CreateVolume:   s.createVolumeCalls,
		ExportVolume:   s.exportVolumeCalls,
		UnexportVolume: s.unexportVolumeCalls,
		DeleteVolume:   s.deleteVolumeCalls,
		AllowInitiator: s.allowInitiatorCalls,
		DenyInitiator:  s.denyInitiatorCalls,
		ExpandVolume:   s.expandVolumeCalls,
		GetCapacity:    s.getCapacityCalls,
	}
}

type fakeAgentCounts struct {
	CreateVolume   int
	ExportVolume   int
	UnexportVolume int
	DeleteVolume   int
	AllowInitiator int
	DenyInitiator  int
	ExpandVolume   int
	GetCapacity    int
}
