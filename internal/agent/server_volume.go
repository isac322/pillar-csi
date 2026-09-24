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
// returned without error.  It is fenced (a grant-class operation), so a
// delayed create from a retired or superseded lifecycle cannot re-create a
// backend volume.
func (s *Server) CreateVolume(
	ctx context.Context,
	req *agentv1.CreateVolumeRequest,
) (*agentv1.CreateVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	var (
		devicePath string
		allocated  int64
	)
	err = s.fenced(req.GetVolumeId(), req.GetFence(), fenceGrant, func() error {
		var createErr error
		devicePath, allocated, createErr = b.Create(
			ctx,
			req.GetVolumeId(),
			req.GetCapacityBytes(),
			req.GetBackendParams(),
		)
		return createVolumeError(createErr)
	})
	if err != nil {
		return nil, err
	}
	return &agentv1.CreateVolumeResponse{
		DevicePath:    devicePath,
		CapacityBytes: allocated,
	}, nil
}

// createVolumeError maps a backend Create error onto a gRPC status.
func createVolumeError(err error) error {
	if err == nil {
		return nil
	}
	if conflictErr, ok := errors.AsType[*backend.ConflictError](err); ok {
		return status.Errorf(codes.AlreadyExists, "CreateVolume: %v", conflictErr)
	}
	// Preserve gRPC status codes returned by the backend (e.g. InvalidArgument
	// from name-validation wrappers). Plain Go errors are wrapped with Internal.
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "CreateVolume: %v", err)
}

// DeleteVolume destroys the backend storage resource for the given volume.
// The operation is idempotent: if the volume does not exist it returns success.
//
// The request is fenced as the lifecycle's terminal operation: a delayed
// delete from a former controller cannot destroy a volume a newer operation or
// lifecycle owns, and after the deletion succeeded the lifecycle is recorded
// as ended, so any later grant-class request for it is rejected.  The mark is
// never removed.
func (s *Server) DeleteVolume(
	ctx context.Context,
	req *agentv1.DeleteVolumeRequest,
) (*agentv1.DeleteVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	err = s.fenced(req.GetVolumeId(), req.GetFence(), fenceDestroy, func() error {
		deleteErr := b.Delete(ctx, req.GetVolumeId())
		if deleteErr != nil {
			return status.Errorf(codes.Internal, "DeleteVolume: %v", deleteErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &agentv1.DeleteVolumeResponse{}, nil
}

// ExpandVolume grows the backend storage resource to at least the requested
// size and returns the actual allocated size.  It is fenced (grant-class), so
// it cannot act on a volume whose lifecycle ended or was superseded.
func (s *Server) ExpandVolume(
	ctx context.Context,
	req *agentv1.ExpandVolumeRequest,
) (*agentv1.ExpandVolumeResponse, error) {
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	var allocated int64
	err = s.fenced(req.GetVolumeId(), req.GetFence(), fenceGrant, func() error {
		var expandErr error
		allocated, expandErr = b.Expand(ctx, req.GetVolumeId(), req.GetRequestedBytes())
		if expandErr != nil {
			return status.Errorf(codes.Internal, "ExpandVolume: %v", expandErr)
		}
		return s.revalidateNamespace(req.GetVolumeId())
	})
	if err != nil {
		return nil, err
	}
	return &agentv1.ExpandVolumeResponse{CapacityBytes: allocated}, nil
}

// revalidateNamespace asks the enabled NVMe-oF namespace of volumeID to
// revalidate its backing size after a backend resize.  The operation is a
// no-op when the volume has no active NVMe export.  When an export exists,
// failure must be returned: otherwise ControllerExpandVolume reports success
// while connected nodes keep seeing the old capacity and retry
// NodeExpandVolume indefinitely.
func (s *Server) revalidateNamespace(volumeID string) error {
	nqn, nqnErr := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if nqnErr != nil {
		return status.Errorf(codes.Internal, "ExpandVolume: derive target NQN: %v", nqnErr)
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: nqn,
		NamespaceID:  1,
	}
	resizeErr := target.ResizeNamespace()
	if resizeErr != nil {
		return status.Errorf(codes.Internal,
			"ExpandVolume: revalidate NVMe namespace for volume %q (nqn=%q): %v",
			volumeID, nqn, resizeErr)
	}
	return nil
}
