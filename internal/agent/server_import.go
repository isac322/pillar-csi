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
)

// ImportVolume adopts an already-existing backend storage resource — created
// by another provisioning stack such as democratic-csi or openebs
// zfs-localpv — into the given volume lifecycle.  Unlike CreateVolume it
// never creates anything: the backend only verifies that the resolved
// volumeID names an existing, idle resource inside its configured layout and
// reports its device path and size.
//
// The operation is idempotent (a repeated import returns the same device
// path) and fenced (a grant-class operation), but the ownership mark is
// persisted only after the backend's import checks pass — see fencedImport.
// A refused import therefore records nothing: the volume ID stays unclaimed,
// the pre-existing resource is untouched, and retrying after the refusal
// cause is fixed can still adopt it.  Once the mark is written it binds the
// resource to this lifecycle, so a concurrent or delayed import/create for a
// different lifecycle is rejected.
//
// BACKEND_TYPE_LVM imports require expected_lvm_source and go through the
// backend's LVImporter only (never the name-only VolumeImporter); the
// adopted identity and preserve_original are pinned into the mark, and a
// later import naming another source or downgrading the policy is refused
// before the backend runs.  Every other backend rejects
// expected_lvm_source.
func (s *Server) ImportVolume(
	ctx context.Context,
	req *agentv1.ImportVolumeRequest,
) (*agentv1.ImportVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	if req.GetFilesystemAdoption() != nil {
		// Filesystem adoptions never resolve through an LV; refuse the LVM
		// identity here, before the filesystem path claims any ownership.
		if req.GetExpectedLvmSource() != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"ImportVolume %q: expected_lvm_source is only valid for BACKEND_TYPE_LVM, not %s",
				req.GetVolumeId(), req.GetBackendType())
		}
		return s.importFilesystem(ctx, req)
	}
	b, err := s.backendForType(req.GetVolumeId(), req.GetBackendType())
	if err != nil {
		return nil, err
	}
	err = checkBackendType("ImportVolume", req.GetBackendType(), b.Type(), req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	if b.Type() == agentv1.BackendType_BACKEND_TYPE_LVM {
		return s.importLV(ctx, req, b)
	}
	if req.GetExpectedLvmSource() != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"ImportVolume %q: expected_lvm_source is only valid for BACKEND_TYPE_LVM, not %s",
			req.GetVolumeId(), b.Type())
	}
	importer, ok := b.(backend.VolumeImporter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"ImportVolume %q: backend %s cannot adopt existing volumes", req.GetVolumeId(), b.Type())
	}
	var (
		devicePath string
		sizeBytes  int64
	)
	err = s.fencedImport(ctx, req.GetVolumeId(), req.GetFence(), nil, false, func() error {
		var importErr error
		devicePath, sizeBytes, importErr = importer.Import(
			ctx, req.GetVolumeId(), req.GetCapacityBytes(), req.GetExpectedDataset())
		return importVolumeError(importErr)
	})
	if err != nil {
		return nil, err
	}
	return &agentv1.ImportVolumeResponse{
		DevicePath:    devicePath,
		CapacityBytes: sizeBytes,
	}, nil
}

// importLV adopts an existing LV for an LVM ImportVolume request.  The
// identity is mandatory and complete.  An absent preserve_original field
// defaults to PreserveOriginal; only an explicit false opts in to Managed.
// The pin is checked before ImportLV runs and written only after ImportLV
// succeeded.
func (s *Server) importLV(
	ctx context.Context,
	req *agentv1.ImportVolumeRequest,
	b backend.VolumeBackend,
) (*agentv1.ImportVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	expected := req.GetExpectedLvmSource()
	if expected == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"ImportVolume %q: expected_lvm_source is required for BACKEND_TYPE_LVM", volumeID)
	}
	if expected.GetVolumeGroup() == "" || expected.GetLogicalVolume() == "" ||
		expected.GetVolumeGroupUuid() == "" || expected.GetLogicalVolumeUuid() == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"ImportVolume %q: expected_lvm_source needs volume_group, logical_volume, "+
				"volume_group_uuid and logical_volume_uuid", volumeID)
	}
	if req.GetExpectedDataset() != "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"ImportVolume %q: expected_dataset is not valid for BACKEND_TYPE_LVM", volumeID)
	}
	importer, ok := b.(backend.LVImporter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"ImportVolume %q: backend %s cannot adopt existing LVs", volumeID, b.Type())
	}
	src := markLVMSourceFromProto(expected)
	var (
		devicePath string
		sizeBytes  int64
	)
	preserve := req.PreserveOriginal == nil || req.GetPreserveOriginal()
	err := s.fencedImport(ctx, volumeID, req.GetFence(), src, preserve, func() error {
		var importErr error
		devicePath, sizeBytes, importErr = importer.ImportLV(ctx, volumeID, req.GetCapacityBytes(), src.identity())
		return importVolumeError(importErr)
	})
	if err != nil {
		return nil, err
	}
	return &agentv1.ImportVolumeResponse{
		DevicePath:    devicePath,
		CapacityBytes: sizeBytes,
	}, nil
}

// importVolumeError maps a backend Import error onto a gRPC status.
func importVolumeError(err error) error {
	if err == nil {
		return nil
	}
	refused, isRefused := errors.AsType[*backend.ImportRefusedError](err)
	if isRefused {
		return status.Errorf(codes.FailedPrecondition, "ImportVolume: %v", refused)
	}
	mismatch, isMismatch := errors.AsType[*backend.LayoutMismatchError](err)
	if isMismatch {
		return status.Errorf(codes.FailedPrecondition, "ImportVolume: %v", mismatch)
	}
	capErr := insufficientCapacityStatus("ImportVolume", err)
	if capErr != nil {
		return capErr
	}
	// Preserve gRPC status codes returned by the backend.  Plain Go errors are
	// wrapped with Internal.
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "ImportVolume: %v", err)
}
