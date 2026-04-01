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

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ExportVolume dispatches the export request to the protocol-specific handler.
// The operation is idempotent.
func (s *Server) ExportVolume(
	ctx context.Context,
	req *agentv1.ExportVolumeRequest,
) (*agentv1.ExportVolumeResponse, error) {
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
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
	}

	switch req.GetProtocolType() {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		if req.GetExportParams().GetNvmeofTcp() == nil {
			return ExportParams{}, status.Errorf(codes.InvalidArgument, "nvmeof_tcp export params required")
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		if req.GetExportParams().GetIscsi() == nil {
			return ExportParams{}, status.Errorf(codes.InvalidArgument, "iscsi export params required")
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_NFS:
		if req.GetExportParams().GetNfs() == nil {
			return ExportParams{}, status.Errorf(codes.InvalidArgument, "nfs export params required")
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_SMB:
		if req.GetExportParams().GetSmb() == nil {
			return ExportParams{}, status.Errorf(codes.InvalidArgument, "smb export params required")
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED:
		return ExportParams{}, status.Errorf(codes.InvalidArgument,
			"exportParamsForRequest: protocol_type is required")
	default:
		return ExportParams{}, status.Errorf(codes.InvalidArgument,
			"protocol %s has no export parameter mapping", req.GetProtocolType().String())
	}

	return params, nil
}

// UnexportVolume removes a protocol export entry. The operation is idempotent.
func (s *Server) UnexportVolume(
	ctx context.Context,
	req *agentv1.UnexportVolumeRequest,
) (*agentv1.UnexportVolumeResponse, error) {
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = handler.Unexport(ctx, req.GetVolumeId())
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
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = handler.AllowInitiator(ctx, req.GetVolumeId(), req.GetInitiatorId())
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
	handler, err := s.handlerForProtocol(req.GetProtocolType())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	err = handler.DenyInitiator(ctx, req.GetVolumeId(), req.GetInitiatorId())
	if err != nil {
		return nil, protocolRPCError(err)
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}

func protocolRPCError(err error) error {
	if st, ok := status.FromError(err); ok {
		return status.Errorf(st.Code(), "%s", st.Message())
	}
	return fmt.Errorf("protocol handler: %w", err)
}
