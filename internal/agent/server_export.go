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
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ExportVolume dispatches the export request to the protocol-specific handler.
// The operation is idempotent.
func (s *Server) ExportVolume(
	ctx context.Context,
	req *agentv1.ExportVolumeRequest,
) (*agentv1.ExportVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = s.checkExportRestoreDone("ExportVolume")
	if err != nil {
		return nil, err
	}
	params, err := exportParamsForRequest(req)
	if err != nil {
		return nil, protocolRPCError(err)
	}

	result, err := handler.Export(ctx, params)
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  result.TargetID,
			Address:   result.Address,
			Port:      result.Port,
			VolumeRef: result.VolumeRef,
		},
	}, nil
}

func exportParamsForRequest(req *agentv1.ExportVolumeRequest) (ExportParams, error) {
	params := ExportParams{
		VolumeID:       req.GetVolumeId(),
		DevicePath:     req.GetDevicePath(),
		ProtocolParams: req.GetExportParams(),
		ACLEnabled:     req.GetAclEnabled(),
		Fence:          req.GetFence(),
	}

	switch req.GetProtocolType() {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		if req.GetExportParams().GetNvmeofTcp() == nil {
			return ExportParams{}, status.Errorf(codes.InvalidArgument, "nvmeof_tcp export params required")
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED:
		return ExportParams{}, status.Errorf(codes.InvalidArgument,
			"exportParamsForRequest: protocol_type is required")
	default:
		return ExportParams{}, status.Errorf(codes.Unimplemented,
			"exportParamsForRequest: unsupported protocol type %s", req.GetProtocolType().String())
	}

	return params, nil
}

// UnexportVolume removes a protocol export entry. The operation is idempotent.
func (s *Server) UnexportVolume(
	ctx context.Context,
	req *agentv1.UnexportVolumeRequest,
) (*agentv1.UnexportVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = handler.Unexport(ctx, req.GetVolumeId(), req.GetFence())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

// AllowInitiator grants protocol-specific access to the given initiator.
// The operation is idempotent.
func (s *Server) AllowInitiator(
	ctx context.Context,
	req *agentv1.AllowInitiatorRequest,
) (*agentv1.AllowInitiatorResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = s.checkExportRestoreDone("AllowInitiator")
	if err != nil {
		return nil, err
	}
	err = handler.AllowInitiator(ctx, req.GetVolumeId(), req.GetInitiatorId(), req.GetFence())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}

// DenyInitiator revokes protocol-specific access for the given initiator.
// The operation is idempotent.
func (s *Server) DenyInitiator(
	ctx context.Context,
	req *agentv1.DenyInitiatorRequest,
) (*agentv1.DenyInitiatorResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = handler.DenyInitiator(ctx, req.GetVolumeId(), req.GetInitiatorId(), req.GetFence())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}

// SetLocalAttach switches a volume's export between serving remote
// initiators (local=false) and being fenced for a direct attach on the
// storage node (local=true, returning the backend device path).  It is gated
// by the export restore like AllowInitiator and fenced (grant-class) in the
// protocol handler.  The operation is idempotent in both directions.
func (s *Server) SetLocalAttach(
	ctx context.Context,
	req *agentv1.SetLocalAttachRequest,
) (*agentv1.SetLocalAttachResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	_, err := poolFromVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = s.checkExportRestoreDone("SetLocalAttach")
	if err != nil {
		return nil, err
	}
	devicePath, err := handler.SetLocalAttach(ctx, req.GetVolumeId(), req.GetLocal(), req.GetFence())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.SetLocalAttachResponse{DevicePath: devicePath}, nil
}

func protocolRPCError(err error) error {
	if st, ok := status.FromError(err); ok {
		return status.Errorf(st.Code(), "%s", st.Message())
	}
	return fmt.Errorf("protocol handler: %w", err)
}
