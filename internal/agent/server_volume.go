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
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// CreateVolume creates the backend storage resource (ZFS zvol) for the given
// volume.  The operation is idempotent: if the zvol already exists it is
// returned without error.
func (s *Server) CreateVolume(
	ctx context.Context,
	req *agentv1.CreateVolumeRequest,
) (*agentv1.CreateVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	devicePath, allocated, err := b.Create(
		ctx,
		req.GetVolumeId(),
		req.GetCapacityBytes(),
		req.GetBackendParams(),
	)
	if err != nil {
		if conflictErr, ok := errors.AsType[*backend.ConflictError](err); ok {
			return nil, status.Errorf(codes.AlreadyExists, "CreateVolume: %v", conflictErr)
		}
		// Preserve gRPC status codes returned by the backend (e.g. InvalidArgument
		// from name-validation wrappers). Plain Go errors are wrapped with Internal.
		if _, ok := status.FromError(err); ok {
			return nil, err //nolint:wrapcheck // intentional: preserve backend status code
		}
		return nil, status.Errorf(codes.Internal, "CreateVolume: %v", err)
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    devicePath,
		CapacityBytes: allocated,
	}, nil
}

// DeleteVolume destroys the backend storage resource for the given volume.
// The operation is idempotent: if the volume does not exist it returns success.
func (s *Server) DeleteVolume(
	ctx context.Context,
	req *agentv1.DeleteVolumeRequest,
) (*agentv1.DeleteVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	err = b.Delete(ctx, req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "DeleteVolume: %v", err)
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

// ExpandVolume grows the backend storage resource to at least the requested
// size and returns the actual allocated size.
func (s *Server) ExpandVolume(
	ctx context.Context,
	req *agentv1.ExpandVolumeRequest,
) (*agentv1.ExpandVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	allocated, err := b.Expand(ctx, req.GetVolumeId(), req.GetRequestedBytes())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ExpandVolume: %v", err)
	}

	// After the backend volume is resized, ask the enabled NVMe-oF namespace
	// to revalidate its backing size. The operation is a no-op when this volume
	// has no active NVMe export. When an export exists, failure must be returned:
	// otherwise ControllerExpandVolume reports success while connected nodes
	// keep seeing the old capacity and retry NodeExpandVolume indefinitely.
	nqn, nqnErr := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, req.GetVolumeId())
	if nqnErr != nil {
		return nil, status.Errorf(codes.Internal, "ExpandVolume: derive target NQN: %v", nqnErr)
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: nqn,
		NamespaceID:  1,
	}
	resizeErr := target.ResizeNamespace()
	if resizeErr != nil {
		return nil, status.Errorf(
			codes.Internal,
			"ExpandVolume: revalidate NVMe namespace for volume %q (nqn=%q): %v",
			req.GetVolumeId(),
			nqn,
			resizeErr,
		)
	}

	return &agentv1.ExpandVolumeResponse{CapacityBytes: allocated}, nil
}
