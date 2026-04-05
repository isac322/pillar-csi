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

// Package unit_test — shared mock infrastructure for unit tests that require
// a mock agent and a fake Kubernetes client seeded with a PillarTarget.
//
// This file defines the unitMockAgent and unitControllerEnv types used by
// unit tests that exercise paths beyond the pure input-validation guards (e.g.,
// E13 volume clone, E14 capacity edge cases, E22 protocol error propagation).
//
// The infrastructure is intentionally lightweight:
//   - unitMockAgent: in-process function-dispatch double for AgentServiceClient
//   - unitControllerEnv: ControllerServer + mock agent + fake K8s client
//
// No network sockets, gRPC connections, or real storage operations are used.
package unit_test

import (
	"context"
	"io"
	"testing"

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
// unitNopCloser — trivial io.Closer for the AgentDialer.
// ─────────────────────────────────────────────────────────────────────────────

type unitNopCloser struct{}

func (unitNopCloser) Close() error { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// unitMockAgent — in-process test double for agentv1.AgentServiceClient.
// ─────────────────────────────────────────────────────────────────────────────

// unitMockAgent is a minimal mock of AgentServiceClient for unit tests.
// Each RPC method has an optional function field; when nil the method returns
// a sensible default success response.
type unitMockAgent struct {
	createVolumeFn func(*agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error)
	exportVolumeFn func(*agentv1.ExportVolumeRequest) (*agentv1.ExportVolumeResponse, error)
	expandVolumeFn func(*agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error)

	// call counters for assertion use.
	createVolumeCalls int
	exportVolumeCalls int
	expandVolumeCalls int
}

var _ agentv1.AgentServiceClient = (*unitMockAgent)(nil)

func (m *unitMockAgent) CreateVolume(
	_ context.Context, req *agentv1.CreateVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.CreateVolumeResponse, error) {
	m.createVolumeCalls++
	if m.createVolumeFn != nil {
		return m.createVolumeFn(req)
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    "/dev/zvol/tank/test-vol",
		CapacityBytes: req.GetCapacityBytes(),
	}, nil
}

func (*unitMockAgent) DeleteVolume(
	_ context.Context, _ *agentv1.DeleteVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.DeleteVolumeResponse, error) {
	return &agentv1.DeleteVolumeResponse{}, nil
}

func (m *unitMockAgent) ExportVolume(
	_ context.Context, req *agentv1.ExportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	m.exportVolumeCalls++
	if m.exportVolumeFn != nil {
		return m.exportVolumeFn(req)
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  "nqn.2026-01.com.bhyoo.pillar-csi:tank.test-vol",
			Address:   "192.168.1.10",
			Port:      4420,
			VolumeRef: req.GetVolumeId(),
		},
	}, nil
}

func (*unitMockAgent) UnexportVolume(
	_ context.Context, _ *agentv1.UnexportVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.UnexportVolumeResponse, error) {
	return &agentv1.UnexportVolumeResponse{}, nil
}

func (m *unitMockAgent) ExpandVolume(
	_ context.Context, req *agentv1.ExpandVolumeRequest, _ ...grpc.CallOption,
) (*agentv1.ExpandVolumeResponse, error) {
	m.expandVolumeCalls++
	if m.expandVolumeFn != nil {
		return m.expandVolumeFn(req)
	}
	return &agentv1.ExpandVolumeResponse{
		CapacityBytes: req.GetRequestedBytes(),
	}, nil
}

func (*unitMockAgent) AllowInitiator(
	_ context.Context, _ *agentv1.AllowInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.AllowInitiatorResponse, error) {
	return &agentv1.AllowInitiatorResponse{}, nil
}

func (*unitMockAgent) DenyInitiator(
	_ context.Context, _ *agentv1.DenyInitiatorRequest, _ ...grpc.CallOption,
) (*agentv1.DenyInitiatorResponse, error) {
	return &agentv1.DenyInitiatorResponse{}, nil
}

// Stubs for unused methods.
func (*unitMockAgent) GetCapabilities(
	_ context.Context, _ *agentv1.GetCapabilitiesRequest, _ ...grpc.CallOption,
) (*agentv1.GetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) GetCapacity(
	_ context.Context, _ *agentv1.GetCapacityRequest, _ ...grpc.CallOption,
) (*agentv1.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) ListVolumes(
	_ context.Context, _ *agentv1.ListVolumesRequest, _ ...grpc.CallOption,
) (*agentv1.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) ListExports(
	_ context.Context, _ *agentv1.ListExportsRequest, _ ...grpc.CallOption,
) (*agentv1.ListExportsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) HealthCheck(
	_ context.Context, _ *agentv1.HealthCheckRequest, _ ...grpc.CallOption,
) (*agentv1.HealthCheckResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) SendVolume(
	_ context.Context, _ *agentv1.SendVolumeRequest, _ ...grpc.CallOption,
) (grpc.ServerStreamingClient[agentv1.SendVolumeChunk], error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) ReceiveVolume(
	_ context.Context, _ ...grpc.CallOption,
) (grpc.ClientStreamingClient[agentv1.ReceiveVolumeChunk, agentv1.ReceiveVolumeResponse], error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}
func (*unitMockAgent) ReconcileState(
	_ context.Context, _ *agentv1.ReconcileStateRequest, _ ...grpc.CallOption,
) (*agentv1.ReconcileStateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not used in unit tests")
}

// ─────────────────────────────────────────────────────────────────────────────
// unitControllerEnv — ControllerServer + unitMockAgent + fake K8s client.
// ─────────────────────────────────────────────────────────────────────────────

// unitControllerEnv holds a ControllerServer and its mock agent, used by unit
// tests that need to exercise code paths beyond pure input-validation guards.
type unitControllerEnv struct {
	srv   *pillarcsi.ControllerServer
	agent *unitMockAgent
}

// newUnitControllerEnv creates a ControllerServer backed by:
//   - a fake K8s client seeded with a single PillarTarget named "storage-1"
//     whose ResolvedAddress is "192.168.1.10:9500"
//   - a unitMockAgent injected via AgentDialer (no real gRPC connection)
//
// The target name "storage-1" matches validParamsWithoutBinding()["target"].
func newUnitControllerEnv(t *testing.T) *unitControllerEnv {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	target := &v1alpha1.PillarTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-1"},
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

	agent := &unitMockAgent{}
	dialer := pillarcsi.AgentDialer(func(_ context.Context, _ string) (agentv1.AgentServiceClient, io.Closer, error) {
		return agent, unitNopCloser{}, nil
	})

	srv := pillarcsi.NewControllerServerWithDialer(fakeClient, "pillar-csi.bhyoo.com", dialer)
	return &unitControllerEnv{srv: srv, agent: agent}
}
