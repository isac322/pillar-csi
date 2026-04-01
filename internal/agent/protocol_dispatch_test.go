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

package agent

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	dispatchTestVolumeID   = "tank/pvc-abc"
	dispatchTestDevicePath = "/dev/test/pvc-abc"
	dispatchInitiatorID    = "iqn.2026-01.io.example:node-1"
)

type recordingProtocolResolver struct {
	handlers map[agentv1.ProtocolType]AgentProtocolHandler
	calls    []agentv1.ProtocolType
}

func (r *recordingProtocolResolver) Resolve(
	protocol agentv1.ProtocolType,
) (AgentProtocolHandler, error) {
	r.calls = append(r.calls, protocol)

	handler, ok := r.handlers[protocol]
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"protocol %s is not supported by this agent", protocol.String())
	}
	return handler, nil
}

type initiatorDispatchCall struct {
	volumeID    string
	initiatorID string
}

type recordingProtocolHandler struct {
	exportResult *ExportResult
	exportErr    error
	unexportErr  error
	allowErr     error
	denyErr      error
	reconcileErr error

	exportCalls    []ExportParams
	unexportCalls  []string
	allowCalls     []initiatorDispatchCall
	denyCalls      []initiatorDispatchCall
	reconcileCalls [][]ExportDesiredState
}

func (h *recordingProtocolHandler) Export(
	_ context.Context,
	params ExportParams,
) (*ExportResult, error) {
	h.exportCalls = append(h.exportCalls, params)
	if h.exportErr != nil {
		return nil, h.exportErr
	}
	if h.exportResult == nil {
		return &ExportResult{}, nil
	}
	return h.exportResult, nil
}

func (h *recordingProtocolHandler) Unexport(_ context.Context, volumeID string) error {
	h.unexportCalls = append(h.unexportCalls, volumeID)
	return h.unexportErr
}

func (h *recordingProtocolHandler) AllowInitiator(
	_ context.Context,
	volumeID, initiatorID string,
) error {
	h.allowCalls = append(h.allowCalls, initiatorDispatchCall{
		volumeID:    volumeID,
		initiatorID: initiatorID,
	})
	return h.allowErr
}

func (h *recordingProtocolHandler) DenyInitiator(
	_ context.Context,
	volumeID, initiatorID string,
) error {
	h.denyCalls = append(h.denyCalls, initiatorDispatchCall{
		volumeID:    volumeID,
		initiatorID: initiatorID,
	})
	return h.denyErr
}

func (h *recordingProtocolHandler) Reconcile(
	_ context.Context,
	desired []ExportDesiredState,
) error {
	copied := make([]ExportDesiredState, 0, len(desired))
	for _, entry := range desired {
		entryCopy := entry
		entryCopy.AllowedInitiators = append([]string(nil), entry.AllowedInitiators...)
		copied = append(copied, entryCopy)
	}
	h.reconcileCalls = append(h.reconcileCalls, copied)
	return h.reconcileErr
}

var _ AgentProtocolHandler = (*recordingProtocolHandler)(nil)

func dispatchIscsiExportParams(addr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_Iscsi{
			Iscsi: &agentv1.IscsiExportParams{
				BindAddress: addr,
				Port:        port,
			},
		},
	}
}

func dispatchNvmeofExportParams(addr string) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: addr,
				Port:        4420,
			},
		},
	}
}

func dispatchNfsExportParams(version string) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_Nfs{
			Nfs: &agentv1.NfsExportParams{
				Version: version,
			},
		},
	}
}

func TestExportVolume_DispatchesToResolvedHandler(t *testing.T) {
	t.Parallel()

	nvmeHandler := &recordingProtocolHandler{}
	iscsiHandler := &recordingProtocolHandler{
		exportResult: &ExportResult{
			TargetID:  "iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc",
			Address:   "10.0.0.2",
			Port:      3260,
			VolumeRef: "0",
		},
	}
	resolver := &recordingProtocolResolver{
		handlers: map[agentv1.ProtocolType]AgentProtocolHandler{
			agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP: nvmeHandler,
			agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:      iscsiHandler,
		},
	}

	srv := NewServer(nil, "")
	srv.protocolHandlerResolver = resolver.Resolve

	req := &agentv1.ExportVolumeRequest{
		VolumeId:     dispatchTestVolumeID,
		DevicePath:   dispatchTestDevicePath,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		ExportParams: dispatchIscsiExportParams("10.0.0.2", 3260),
		AclEnabled:   true,
	}

	resp, err := srv.ExportVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolver.calls, []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	}) {
		t.Fatalf("resolver calls = %v, want [PROTOCOL_TYPE_ISCSI]", resolver.calls)
	}
	if len(nvmeHandler.exportCalls) != 0 {
		t.Fatalf("NVMe handler export calls = %d, want 0", len(nvmeHandler.exportCalls))
	}
	if len(iscsiHandler.exportCalls) != 1 {
		t.Fatalf("iSCSI handler export calls = %d, want 1", len(iscsiHandler.exportCalls))
	}

	gotParams := iscsiHandler.exportCalls[0]
	if gotParams.VolumeID != req.GetVolumeId() {
		t.Errorf("VolumeID = %q, want %q", gotParams.VolumeID, req.GetVolumeId())
	}
	if gotParams.DevicePath != req.GetDevicePath() {
		t.Errorf("DevicePath = %q, want %q", gotParams.DevicePath, req.GetDevicePath())
	}
	if gotParams.ACLEnabled != req.GetAclEnabled() {
		t.Errorf("ACLEnabled = %v, want %v", gotParams.ACLEnabled, req.GetAclEnabled())
	}
	if !proto.Equal(gotParams.ProtocolParams, req.GetExportParams()) {
		t.Errorf("ProtocolParams = %v, want %v", gotParams.ProtocolParams, req.GetExportParams())
	}

	if got := resp.GetExportInfo().GetTargetId(); got != iscsiHandler.exportResult.TargetID {
		t.Errorf("TargetId = %q, want %q", got, iscsiHandler.exportResult.TargetID)
	}
	if got := resp.GetExportInfo().GetAddress(); got != iscsiHandler.exportResult.Address {
		t.Errorf("Address = %q, want %q", got, iscsiHandler.exportResult.Address)
	}
	if got := resp.GetExportInfo().GetPort(); got != iscsiHandler.exportResult.Port {
		t.Errorf("Port = %d, want %d", got, iscsiHandler.exportResult.Port)
	}
	if got := resp.GetExportInfo().GetVolumeRef(); got != iscsiHandler.exportResult.VolumeRef {
		t.Errorf("VolumeRef = %q, want %q", got, iscsiHandler.exportResult.VolumeRef)
	}
}

func TestExportLifecycleRPCs_DispatchToResolvedHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		protocol agentv1.ProtocolType
		call     func(context.Context, *Server) error
		assert   func(*testing.T, *recordingProtocolHandler)
	}{
		{
			name:     "UnexportVolume",
			protocol: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
			call: func(ctx context.Context, srv *Server) error {
				_, err := srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     dispatchTestVolumeID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
				})
				return err
			},
			assert: func(t *testing.T, handler *recordingProtocolHandler) {
				t.Helper()
				if !reflect.DeepEqual(handler.unexportCalls, []string{dispatchTestVolumeID}) {
					t.Fatalf("unexport calls = %v, want [%q]", handler.unexportCalls, dispatchTestVolumeID)
				}
			},
		},
		{
			name:     "AllowInitiator",
			protocol: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
			call: func(ctx context.Context, srv *Server) error {
				_, err := srv.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
					VolumeId:     dispatchTestVolumeID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
					InitiatorId:  dispatchInitiatorID,
				})
				return err
			},
			assert: func(t *testing.T, handler *recordingProtocolHandler) {
				t.Helper()
				want := []initiatorDispatchCall{{
					volumeID:    dispatchTestVolumeID,
					initiatorID: dispatchInitiatorID,
				}}
				if !reflect.DeepEqual(handler.allowCalls, want) {
					t.Fatalf("allow calls = %v, want %v", handler.allowCalls, want)
				}
			},
		},
		{
			name:     "DenyInitiator",
			protocol: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
			call: func(ctx context.Context, srv *Server) error {
				_, err := srv.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
					VolumeId:     dispatchTestVolumeID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
					InitiatorId:  dispatchInitiatorID,
				})
				return err
			},
			assert: func(t *testing.T, handler *recordingProtocolHandler) {
				t.Helper()
				want := []initiatorDispatchCall{{
					volumeID:    dispatchTestVolumeID,
					initiatorID: dispatchInitiatorID,
				}}
				if !reflect.DeepEqual(handler.denyCalls, want) {
					t.Fatalf("deny calls = %v, want %v", handler.denyCalls, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handler := &recordingProtocolHandler{}
			resolver := &recordingProtocolResolver{
				handlers: map[agentv1.ProtocolType]AgentProtocolHandler{
					tt.protocol: handler,
				},
			}

			srv := NewServer(nil, "")
			srv.protocolHandlerResolver = resolver.Resolve

			if err := tt.call(context.Background(), srv); err != nil {
				t.Fatalf("%s unexpected error: %v", tt.name, err)
			}
			if !reflect.DeepEqual(resolver.calls, []agentv1.ProtocolType{tt.protocol}) {
				t.Fatalf("resolver calls = %v, want [%s]", resolver.calls, tt.protocol.String())
			}

			tt.assert(t, handler)
		})
	}
}

func TestReconcileState_DispatchesDesiredStateByProtocol(t *testing.T) {
	t.Parallel()

	nvmeHandler := &recordingProtocolHandler{}
	nfsHandler := &recordingProtocolHandler{}
	resolver := &recordingProtocolResolver{
		handlers: map[agentv1.ProtocolType]AgentProtocolHandler{
			agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP: nvmeHandler,
			agentv1.ProtocolType_PROTOCOL_TYPE_NFS:        nfsHandler,
		},
	}

	srv := NewServer(nil, "")
	srv.protocolHandlerResolver = resolver.Resolve

	req := &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   dispatchTestVolumeID,
				DevicePath: dispatchTestDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams:      dispatchNvmeofExportParams("10.0.0.1"),
						AllowedInitiators: []string{"nqn.2026-01.io.example:host-a"},
					},
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
						ExportParams:      dispatchNfsExportParams("4.2"),
						AllowedInitiators: []string{"10.0.0.0/24"},
					},
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams:      dispatchNvmeofExportParams("10.0.0.2"),
						AllowedInitiators: []string{"nqn.2026-01.io.example:host-b"},
					},
				},
			},
		},
	}

	resp, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}

	if !reflect.DeepEqual(resolver.calls, []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
	}) {
		t.Fatalf("resolver calls = %v, want [PROTOCOL_TYPE_NVMEOF_TCP PROTOCOL_TYPE_NFS]", resolver.calls)
	}
	if len(resp.GetResults()) != 1 || !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile result = %+v, want single successful result", resp.GetResults())
	}
	if len(nvmeHandler.reconcileCalls) != 1 {
		t.Fatalf("NVMe reconcile calls = %d, want 1", len(nvmeHandler.reconcileCalls))
	}
	if len(nfsHandler.reconcileCalls) != 1 {
		t.Fatalf("NFS reconcile calls = %d, want 1", len(nfsHandler.reconcileCalls))
	}

	nvmeDesired := nvmeHandler.reconcileCalls[0]
	assertDesiredStateCount(t, "NVMe", nvmeDesired, 2)
	assertDesiredState(t, "nvme desired[0]", nvmeDesired[0], dispatchTestVolumeID,
		dispatchTestDevicePath, dispatchNvmeofExportParams("10.0.0.1"),
		[]string{"nqn.2026-01.io.example:host-a"})
	assertDesiredState(t, "nvme desired[1]", nvmeDesired[1], dispatchTestVolumeID,
		dispatchTestDevicePath, dispatchNvmeofExportParams("10.0.0.2"),
		[]string{"nqn.2026-01.io.example:host-b"})

	nfsDesired := nfsHandler.reconcileCalls[0]
	assertDesiredStateCount(t, "NFS", nfsDesired, 1)
	assertDesiredState(t, "nfs desired[0]", nfsDesired[0], dispatchTestVolumeID,
		dispatchTestDevicePath, dispatchNfsExportParams("4.2"),
		[]string{"10.0.0.0/24"})
}

func TestAllowInitiator_UnsupportedProtocolDoesNotInvokeHandler(t *testing.T) {
	t.Parallel()

	supportedHandler := &recordingProtocolHandler{}
	resolver := &recordingProtocolResolver{
		handlers: map[agentv1.ProtocolType]AgentProtocolHandler{
			agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP: supportedHandler,
		},
	}

	srv := NewServer(nil, "")
	srv.protocolHandlerResolver = resolver.Resolve

	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     dispatchTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
		InitiatorId:  dispatchInitiatorID,
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol, got nil")
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", st.Code())
	}
	if st.Message() != "protocol PROTOCOL_TYPE_SMB is not supported by this agent" {
		t.Fatalf("message = %q, want unsupported protocol detail", st.Message())
	}
	if !reflect.DeepEqual(resolver.calls, []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
	}) {
		t.Fatalf("resolver calls = %v, want [PROTOCOL_TYPE_SMB]", resolver.calls)
	}
	if len(supportedHandler.allowCalls) != 0 {
		t.Fatalf("supported handler allow calls = %d, want 0", len(supportedHandler.allowCalls))
	}
}

func assertDesiredStateCount(t *testing.T, label string, desired []ExportDesiredState, want int) {
	t.Helper()
	if len(desired) != want {
		t.Fatalf("%s desired exports len = %d, want %d", label, len(desired), want)
	}
}

func assertDesiredState(
	t *testing.T,
	label string,
	got ExportDesiredState,
	wantVolumeID, wantDevicePath string,
	wantParams *agentv1.ExportParams,
	wantInitiators []string,
) {
	t.Helper()

	if got.VolumeID != wantVolumeID {
		t.Errorf("%s VolumeID = %q, want %q", label, got.VolumeID, wantVolumeID)
	}
	if got.DevicePath != wantDevicePath {
		t.Errorf("%s DevicePath = %q, want %q", label, got.DevicePath, wantDevicePath)
	}
	if !proto.Equal(got.ProtocolParams, wantParams) {
		t.Errorf("%s ProtocolParams = %v, want %v", label, got.ProtocolParams, wantParams)
	}
	if !reflect.DeepEqual(got.AllowedInitiators, wantInitiators) {
		t.Errorf("%s AllowedInitiators = %v, want %v", label, got.AllowedInitiators, wantInitiators)
	}
}
