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

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/telemetry"
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
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	b, err := s.backendForType(req.GetVolumeId(), req.GetBackendType())
	if err != nil {
		return nil, err
	}
	err = checkBackendType("CreateVolume", req.GetBackendType(), b.Type(), req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	var (
		devicePath string
		allocated  int64
	)
	volumeID := req.GetVolumeId()
	err = s.fencedChecked(ctx, volumeID, req.GetFence(), fenceGrant, refusePinnedCreate(volumeID), func() error {
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

// refusePinnedCreate refuses CreateVolume for a volume ID that pins an
// adopted LV, whatever its lifecycle state or policy: lvcreate's
// "already exists" idempotence would otherwise re-adopt the pre-existing LV
// as a newly provisioned managed volume.
func refusePinnedCreate(volumeID string) func(fencingMark) error {
	return func(stored fencingMark) error {
		if stored.LVMSource == nil {
			return nil
		}
		return status.Errorf(codes.FailedPrecondition,
			"CreateVolume %q: the volume ID is pinned to adopted LV %s/%s (lv_uuid %s); it is never provisioned",
			volumeID, stored.LVMSource.VolumeGroup, stored.LVMSource.LogicalVolume, stored.LVMSource.LogicalVolumeUUID)
	}
}

// refusePreserved refuses op on a volume ID pinned PreserveOriginal: the
// adopted LV's data is never destroyed or resized by the agent.
func refusePreserved(op, volumeID string) func(fencingMark) error {
	return func(stored fencingMark) error {
		if !stored.PreserveOriginal {
			return nil
		}
		return status.Errorf(codes.FailedPrecondition,
			"%s %q: the adopted LV is pinned PreserveOriginal; the agent never deletes or resizes it "+
				"(release the lifecycle with ReleaseVolume instead)", op, volumeID)
	}
}

// checkBackendType rejects a volume RPC whose backend_type is missing, names a
// backend this agent does not implement, or differs from the backend that owns
// the volume's pool/VG.  Op is the RPC name used in error messages.
func checkBackendType(op string, requested, configured agentv1.BackendType, volumeID string) error {
	switch requested {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		agentv1.BackendType_BACKEND_TYPE_LVM:
		if requested != configured {
			return status.Errorf(codes.InvalidArgument,
				"%s %q: backend_type %s does not match the pool's configured backend %s",
				op, volumeID, requested, configured)
		}
		return nil
	case agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED:
		return status.Errorf(codes.InvalidArgument, "%s %q: backend_type is required", op, volumeID)
	default:
		return status.Errorf(codes.Unimplemented,
			"%s %q: unsupported backend type %s", op, volumeID, requested)
	}
}

// createVolumeError maps a backend Create error onto a gRPC status.
func createVolumeError(err error) error {
	if err == nil {
		return nil
	}
	if conflictErr, ok := errors.AsType[*backend.ConflictError](err); ok {
		return status.Errorf(codes.AlreadyExists, "CreateVolume: %v", conflictErr)
	}
	if mismatch, ok := errors.AsType[*backend.LayoutMismatchError](err); ok {
		return status.Errorf(codes.FailedPrecondition, "CreateVolume: %v", mismatch)
	}
	capErr := insufficientCapacityStatus("CreateVolume", err)
	if capErr != nil {
		return capErr
	}
	// Preserve gRPC status codes returned by the backend (e.g. InvalidArgument
	// from name-validation wrappers). Plain Go errors are wrapped with Internal.
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "CreateVolume: %v", err)
}

// insufficientCapacityStatus maps a backend *InsufficientCapacityError onto
// ResourceExhausted, the code the CSI spec prescribes for CreateVolume and
// ControllerExpandVolume when the pool cannot hold the requested size.  It
// returns nil when err is not a capacity failure.
func insufficientCapacityStatus(op string, err error) error {
	capErr, ok := errors.AsType[*backend.InsufficientCapacityError](err)
	if !ok {
		return nil
	}
	return status.Errorf(codes.ResourceExhausted, "%s: %v", op, capErr)
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
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	b, err := s.backendForType(req.GetVolumeId(), req.GetBackendType())
	if err != nil {
		return nil, err
	}
	checkErr := checkBackendType("DeleteVolume", req.GetBackendType(), b.Type(), req.GetVolumeId())
	if checkErr != nil {
		return nil, checkErr
	}
	if b.Type() == agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET {
		if s.nfsManager == nil {
			return nil, status.Errorf(
				codes.Unavailable,
				"DeleteVolume: NFS protocol is unavailable on this agent for volume %q",
				req.GetVolumeId(),
			)
		}
		h, handlerErr := s.handlerForProtocol(agentv1.ProtocolType_PROTOCOL_TYPE_NFS)
		if handlerErr != nil {
			return nil, protocolRPCError(handlerErr)
		}
		unexportErr := h.Unexport(ctx, req.GetVolumeId(), req.GetFence())
		if unexportErr != nil {
			return nil, protocolRPCError(unexportErr)
		}
	}
	err = s.fencedChecked(ctx, req.GetVolumeId(), req.GetFence(), fenceDestroy,
		refusePreserved("DeleteVolume", req.GetVolumeId()), func() error {
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
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	b, err := s.backendForType(req.GetVolumeId(), req.GetBackendType())
	if err != nil {
		return nil, err
	}
	checkErr := checkBackendType("ExpandVolume", req.GetBackendType(), b.Type(), req.GetVolumeId())
	if checkErr != nil {
		return nil, checkErr
	}
	var allocated int64
	err = s.fencedChecked(ctx, req.GetVolumeId(), req.GetFence(), fenceGrant,
		refusePreserved("ExpandVolume", req.GetVolumeId()), func() error {
			var expandErr error
			allocated, expandErr = b.Expand(ctx, req.GetVolumeId(), req.GetRequestedBytes())
			if expandErr != nil {
				capErr := insufficientCapacityStatus("ExpandVolume", expandErr)
				if capErr != nil {
					return capErr
				}
				return status.Errorf(codes.Internal, "ExpandVolume: %v", expandErr)
			}
			if b.Type() == agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET {
				return nil
			}
			return s.revalidateNamespace(ctx, req.GetVolumeId())
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
func (s *Server) revalidateNamespace(ctx context.Context, volumeID string) error {
	nqn, nqnErr := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if nqnErr != nil {
		return status.Errorf(codes.Internal, "ExpandVolume: derive target NQN: %v", nqnErr)
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: nqn,
		NamespaceID:  1,
	}
	resizeErr := traceNvmet(ctx, telemetry.SpanAgentNVMetResizeNS, target, "", target.ResizeNamespace)
	if resizeErr != nil {
		return status.Errorf(codes.Internal,
			"ExpandVolume: revalidate NVMe namespace for volume %q (nqn=%q): %v",
			volumeID, nqn, resizeErr)
	}
	return nil
}
