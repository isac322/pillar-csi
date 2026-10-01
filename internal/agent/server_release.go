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

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ReleaseVolume ends a lifecycle's ownership of a volume ID without touching
// the backend resource: the controller retires an import lifecycle that was
// never durably adopted this way, because the zvol is pre-existing data the
// driver must never destroy, resize or reformat.
//
// When the lifecycle owns the fencing mark its export (if any) is removed
// first, under the lifecycle's token like UnexportVolume; the mark is then
// retired (see retireFence), so a delayed ImportVolume or any other mutating
// request of the released lifecycle is rejected while a new lifecycle can
// still import the volume.  The operation is idempotent.
func (s *Server) ReleaseVolume(
	ctx context.Context,
	req *agentv1.ReleaseVolumeRequest,
) (*agentv1.ReleaseVolumeResponse, error) {
	s.setVolumeSpanAttributes(ctx, req.GetVolumeId())
	volumeID, fence := req.GetVolumeId(), req.GetFence()
	unexported := false
	for {
		owned, err := s.retireFence(ctx, volumeID, fence, unexported)
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
