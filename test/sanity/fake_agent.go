//go:build csi_sanity
// +build csi_sanity

package sanity

// fake_agent.go — minimal in-process AgentService backing the controller while
// the csi-sanity suite drives the pillar-csi gRPC API.  All RPCs return
// success-shaped responses that mirror what a real LVM / ZFS agent would
// produce; nothing here touches a real backend.

import (
	"context"
	"sync"

	"google.golang.org/grpc"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// fakeAgent implements agentv1.AgentServiceServer.  It is deliberately
// permissive: every RPC short-circuits to a fixed, success-shaped reply so
// that csi-sanity can exercise the full CSI surface without provisioning a
// backend.
type fakeAgent struct {
	agentv1.UnimplementedAgentServiceServer

	mu sync.Mutex
	// sizes tracks declared volume sizes so ExpandVolume can echo back the
	// requested capacity.
	sizes map[string]int64
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{sizes: map[string]int64{}}
}

// registerFakeAgent attaches a fakeAgent to the given gRPC server and returns
// the agent so tests may introspect call sites if needed.
func registerFakeAgent(srv *grpc.Server) *fakeAgent {
	a := newFakeAgent()
	agentv1.RegisterAgentServiceServer(srv, a)
	return a
}

func (a *fakeAgent) CreateVolume(
	_ context.Context,
	req *agentv1.CreateVolumeRequest,
) (*agentv1.CreateVolumeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sizes[req.GetVolumeId()] = req.GetCapacityBytes()
	return &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/fake/" + req.GetVolumeId(),
		CapacityBytes: req.GetCapacityBytes(),
	}, nil
}

func (a *fakeAgent) DeleteVolume(
	_ context.Context,
	req *agentv1.DeleteVolumeRequest,
) (*agentv1.DeleteVolumeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sizes, req.GetVolumeId())
	return &agentv1.DeleteVolumeResponse{}, nil
}

func (a *fakeAgent) ExportVolume(
	_ context.Context,
	req *agentv1.ExportVolumeRequest,
) (*agentv1.ExportVolumeResponse, error) {
	// Return a deterministic NVMe-oF subsystem so the controller can hand a
	// stable VolumeContext to the node service.
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:" + req.GetVolumeId(),
			Address:   "127.0.0.1",
			Port:      4420,
			VolumeRef: req.GetVolumeId(),
		},
	}, nil
}

func (a *fakeAgent) UnexportVolume(
	_ context.Context,
	_ *agentv1.UnexportVolumeRequest,
) (*agentv1.UnexportVolumeResponse, error) {
	return &agentv1.UnexportVolumeResponse{}, nil
}

func (a *fakeAgent) AllowInitiator(
	_ context.Context,
	_ *agentv1.AllowInitiatorRequest,
) (*agentv1.AllowInitiatorResponse, error) {
	return &agentv1.AllowInitiatorResponse{}, nil
}

func (a *fakeAgent) DenyInitiator(
	_ context.Context,
	_ *agentv1.DenyInitiatorRequest,
) (*agentv1.DenyInitiatorResponse, error) {
	return &agentv1.DenyInitiatorResponse{}, nil
}

func (a *fakeAgent) ExpandVolume(
	_ context.Context,
	req *agentv1.ExpandVolumeRequest,
) (*agentv1.ExpandVolumeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sizes[req.GetVolumeId()] = req.GetRequestedBytes()
	return &agentv1.ExpandVolumeResponse{CapacityBytes: req.GetRequestedBytes()}, nil
}

func (a *fakeAgent) GetCapacity(
	_ context.Context,
	_ *agentv1.GetCapacityRequest,
) (*agentv1.GetCapacityResponse, error) {
	return &agentv1.GetCapacityResponse{
		TotalBytes:     1 << 40,
		AvailableBytes: 1 << 40,
		UsedBytes:      0,
	}, nil
}

func (a *fakeAgent) HealthCheck(
	_ context.Context,
	_ *agentv1.HealthCheckRequest,
) (*agentv1.HealthCheckResponse, error) {
	return &agentv1.HealthCheckResponse{Healthy: true, AgentVersion: "sanity-fake-0.0.0"}, nil
}
