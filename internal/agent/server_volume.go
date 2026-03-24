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
		req.GetBackendParams().GetZfs(),
	)
	if err != nil {
		var conflictErr *backend.ConflictError
		if errors.As(err, &conflictErr) {
			return nil, status.Errorf(codes.AlreadyExists, "CreateVolume: %v", conflictErr)
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
	return &agentv1.ExpandVolumeResponse{CapacityBytes: allocated}, nil
}
