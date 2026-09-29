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

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ReconcileState applies the full desired state for the listed volumes.  It
// is called after an agent restart or node reboot to re-create configfs
// entries that are lost on reboot, and periodically.
//
// All exports of one protocol, across every listed volume, go to the protocol
// handler in one call, so the handler can prepare all of them before making
// any reachable (see nvmeof's port ordering contract).  A request without
// Complete is rejected while the export restore is pending; a Complete
// request ends it.
func (s *Server) ReconcileState(
	ctx context.Context,
	req *agentv1.ReconcileStateRequest,
) (*agentv1.ReconcileStateResponse, error) {
	if !req.GetComplete() {
		err := s.checkExportRestoreDone("ReconcileState")
		if err != nil {
			return nil, err
		}
	}

	vols := req.GetVolumes()
	failures := make([]error, len(vols))
	desiredByProtocol := make(map[agentv1.ProtocolType][]ExportDesiredState)
	// volumeByProtocol[p][j] is the index in vols of desiredByProtocol[p][j].
	volumeByProtocol := make(map[agentv1.ProtocolType][]int)
	handlers := make(map[agentv1.ProtocolType]AgentProtocolHandler)
	var protocolOrder []agentv1.ProtocolType

	for i, vol := range vols {
		failures[i] = s.resolveVolumeHandlers(vol, handlers, &protocolOrder)
	}
	for i, vol := range vols {
		if failures[i] != nil {
			continue
		}
		for _, export := range vol.GetExports() {
			protocolType := export.GetProtocolType()
			desiredByProtocol[protocolType] = append(desiredByProtocol[protocolType],
				exportDesiredState(vol, export))
			volumeByProtocol[protocolType] = append(volumeByProtocol[protocolType], i)
		}
	}

	for _, protocolType := range protocolOrder {
		desired := desiredByProtocol[protocolType]
		if len(desired) == 0 {
			continue
		}
		errs := handlers[protocolType].Reconcile(ctx, desired)
		for j, volume := range volumeByProtocol[protocolType] {
			if errs[j] != nil && failures[volume] == nil {
				failures[volume] = errs[j]
			}
		}
	}

	if req.GetComplete() {
		s.exportRestorePending.Store(false)
	}

	results := make([]*agentv1.ReconcileItemResult, 0, len(vols))
	for i, vol := range vols {
		results = append(results, reconcileResult(vol.GetVolumeId(), failures[i]))
	}
	return &agentv1.ReconcileStateResponse{
		Results:      results,
		ReconciledAt: timestamppb.Now(),
	}, nil
}

// resolveVolumeHandlers resolves the handler of every protocol vol exports,
// recording new protocols in first-seen order.  A volume with an unsupported
// protocol fails as a whole: none of its exports is applied.
func (s *Server) resolveVolumeHandlers(
	vol *agentv1.VolumeDesiredState,
	handlers map[agentv1.ProtocolType]AgentProtocolHandler,
	protocolOrder *[]agentv1.ProtocolType,
) error {
	for _, export := range vol.GetExports() {
		protocolType := export.GetProtocolType()
		if _, ok := handlers[protocolType]; ok {
			continue
		}
		handler, err := s.handlerForProtocol(protocolType)
		if err != nil {
			return err
		}
		handlers[protocolType] = handler
		*protocolOrder = append(*protocolOrder, protocolType)
	}
	return nil
}

func exportDesiredState(vol *agentv1.VolumeDesiredState, export *agentv1.ExportDesiredState) ExportDesiredState {
	return ExportDesiredState{
		VolumeID:          vol.GetVolumeId(),
		DevicePath:        vol.GetDevicePath(),
		ProtocolParams:    export.GetExportParams(),
		AllowedInitiators: export.GetAllowedInitiators(),
		ACLEnabled:        export.GetAclEnabled(),
		Fence:             vol.GetFence(),
		LocalAttach:       export.GetLocalAttach(),
	}
}

func reconcileResult(volumeID string, err error) *agentv1.ReconcileItemResult {
	if err == nil {
		return &agentv1.ReconcileItemResult{VolumeId: volumeID, Success: true}
	}
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
