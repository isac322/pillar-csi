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
// The operation is idempotent (a repeated import returns the same device path)
// and fenced (a grant-class operation): the fence mark the first successful
// import records binds the backend resource to this lifecycle, so a concurrent
// or delayed import/create for a different lifecycle is rejected.  A failed
// import records no mark and the resource is untouched, so the request can
// simply be retried once the refusal cause is fixed.
func (s *Server) ImportVolume(
	ctx context.Context,
	req *agentv1.ImportVolumeRequest,
) (*agentv1.ImportVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return nil, err
	}
	err = checkBackendType("ImportVolume", req.GetBackendType(), b.Type(), req.GetVolumeId())
	if err != nil {
		return nil, err
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
	err = s.fenced(ctx, req.GetVolumeId(), req.GetFence(), fenceGrant, func() error {
		var importErr error
		devicePath, sizeBytes, importErr = importer.Import(ctx, req.GetVolumeId(), req.GetCapacityBytes())
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
