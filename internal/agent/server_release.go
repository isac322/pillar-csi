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

// ReleaseVolume ends a lifecycle's ownership of a volume ID without touching
// the backend resource: the controller retires an import lifecycle that was
// never durably adopted this way, and every PreserveOriginal LV adoption,
// because the resource is pre-existing data the driver must never destroy,
// resize or reformat.
//
// When the lifecycle owns the fencing mark its export (if any) is removed
// first, under the lifecycle's token like UnexportVolume; the mark is then
// retired (see retireFence), so a delayed ImportVolume or any other mutating
// request of the released lifecycle is rejected while a new lifecycle can
// still import the volume.  The operation is idempotent.
//
// For a mark that pins an adopted LV, the owner's retirement additionally
// requires, after the unexport and under the fencing lock, that the LV is
// still the pinned one; for a PreserveOriginal pin also that the agent
// observes no local consumer of it (see verifyPreservedIdle).  Any failure,
// including an unverifiable observation, refuses with the lifecycle still
// owning the volume ID, so the controller keeps the volume and retries.
func (s *Server) ReleaseVolume(
	ctx context.Context,
	req *agentv1.ReleaseVolumeRequest,
) (*agentv1.ReleaseVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	volumeID, fence := req.GetVolumeId(), req.GetFence()
	check := func(stored fencingMark) error {
		return s.verifyPinnedRelease(ctx, volumeID, stored)
	}
	unexported := false
	for {
		owned, err := s.retireFence(ctx, volumeID, fence, unexported, check)
		if err != nil {
			return nil, err
		}
		if !owned {
			return &agentv1.ReleaseVolumeResponse{}, nil
		}
		handler, err := s.handlerForProtocol(req.GetProtocolType())
		if err != nil {
			return nil, protocolRPCError(err)
		}
		err = handler.Unexport(ctx, volumeID, fence)
		if err != nil {
			return nil, protocolRPCError(err)
		}
		unexported = true
	}
}

// verifyPinnedRelease is the retireFence check of ReleaseVolume.  The caller
// holds volumeID's fencing lock and has removed the lifecycle's export.  An
// unpinned mark needs nothing; a pinned one must still resolve to its LV,
// and a PreserveOriginal one must also be idle.
func (s *Server) verifyPinnedRelease(ctx context.Context, volumeID string, stored fencingMark) error {
	if stored.LVMSource == nil {
		return nil
	}
	err := s.verifyPinnedSource(ctx, volumeID, stored)
	if err != nil {
		return err
	}
	if !stored.PreserveOriginal {
		return nil
	}
	return s.verifyPreservedIdle(ctx, volumeID, stored.LVMSource)
}

// verifyPreservedIdle observes the pinned LV through the backend's read-only
// LVInspector and fails closed unless the observation proves no local
// consumer: the observed identity equals the pin, the LV is active, the
// transient O_EXCL open succeeded, and no mount, holder or configured export
// (own or foreign — the own one was just removed) resolves to the device.
// An inspection error, an unknown claim or a backend without LVInspector is
// a refusal, never idle evidence.
//
// The filesystem signature fields (FSType/FSUUID/FSProbe) are deliberately
// not part of this proof: a preserved LV may be a raw or partitioned block
// device with no filesystem TYPE, and a signature — known, absent or an
// FSProbeUnknown probe — says nothing about who holds the device.  Release
// never formats or touches the data, so an unknown signature neither blocks
// nor substitutes for the identity, claim, consumer and export evidence.
func (s *Server) verifyPreservedIdle(ctx context.Context, volumeID string, pin *markLVMSource) error {
	refuse := func(format string, args ...any) error {
		return status.Errorf(codes.FailedPrecondition,
			"ReleaseVolume %q: preserved LV %s/%s is not proven idle: "+format,
			append([]any{volumeID, pin.VolumeGroup, pin.LogicalVolume}, args...)...)
	}
	b, err := s.backendForType(volumeID, agentv1.BackendType_BACKEND_TYPE_LVM)
	if err != nil {
		return refuse("no LVM backend serves the volume: %v", err)
	}
	inspector, ok := b.(backend.LVInspector)
	if !ok {
		return refuse("backend %s cannot observe LV consumers", b.Type())
	}
	obs, err := inspector.InspectLV(ctx, volumeID)
	if err != nil {
		if refused, isRefused := errors.AsType[*backend.ImportRefusedError](err); isRefused {
			return refuse("%v", refused)
		}
		return refuse("consumer probe failed: %v", err)
	}
	switch {
	case obs.Identity != pin.identity():
		return refuse("observed identity %+v differs from the pinned one", obs.Identity)
	case !obs.Active:
		return refuse("the LV is inactive, so its consumers cannot be observed")
	case obs.ExclusiveClaim != backend.ExclusiveClaimFree:
		return refuse("exclusive claim is %q", obs.ExclusiveClaim)
	case len(obs.Consumers) > 0:
		return refuse("local consumers %+v", obs.Consumers)
	case len(obs.Exports) > 0:
		return refuse("configured exports %+v", obs.Exports)
	}
	return nil
}
