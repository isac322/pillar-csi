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

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ReconcileState applies the full desired state for all volumes managed by
// this agent.  It is called after an agent restart or node reboot to
// re-create configfs entries that are lost on reboot.
func (s *Server) ReconcileState(
	ctx context.Context,
	req *agentv1.ReconcileStateRequest,
) (*agentv1.ReconcileStateResponse, error) {
	vols := req.GetVolumes()
	results := make([]*agentv1.ReconcileItemResult, 0, len(vols))
	for _, vol := range vols {
		results = append(results, s.reconcileVolume(ctx, vol))
	}
	return &agentv1.ReconcileStateResponse{
		Results:      results,
		ReconciledAt: timestamppb.Now(),
	}, nil
}

// reconcileVolume applies all desired exports for one volume and returns a
// per-volume result.
func (s *Server) reconcileVolume(
	ctx context.Context,
	vol *agentv1.VolumeDesiredState,
) *agentv1.ReconcileItemResult {
	desiredByProtocol := make(map[agentv1.ProtocolType][]ExportDesiredState, len(vol.GetExports()))
	protocolOrder := make([]agentv1.ProtocolType, 0, len(vol.GetExports()))

	for _, export := range vol.GetExports() {
		protocolType := export.GetProtocolType()
		if _, ok := desiredByProtocol[protocolType]; !ok {
			protocolOrder = append(protocolOrder, protocolType)
		}
		desiredByProtocol[protocolType] = append(desiredByProtocol[protocolType], ExportDesiredState{
			VolumeID:          vol.GetVolumeId(),
			DevicePath:        vol.GetDevicePath(),
			ProtocolParams:    export.GetExportParams(),
			AllowedInitiators: export.GetAllowedInitiators(),
		})
	}

	for _, protocolType := range protocolOrder {
		handler, err := s.handlerForProtocol(protocolType)
		if err != nil {
			return reconcileFailure(vol.GetVolumeId(), err)
		}
		err = handler.Reconcile(ctx, desiredByProtocol[protocolType])
		if err != nil {
			return reconcileFailure(vol.GetVolumeId(), err)
		}
	}

	return &agentv1.ReconcileItemResult{
		VolumeId: vol.GetVolumeId(),
		Success:  true,
	}
}

func reconcileFailure(volumeID string, err error) *agentv1.ReconcileItemResult {
	msg := err.Error()
	if st, ok := status.FromError(err); ok {
		msg = st.Message()
	}

	return &agentv1.ReconcileItemResult{
		VolumeId:     volumeID,
		Success:      false,
		ErrorMessage: msg,
	}
}
