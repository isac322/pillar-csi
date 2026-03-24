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
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ReconcileState applies the full desired state for all volumes managed by
// this agent.  It is called after an agent restart or node reboot to
// re-create configfs entries that are lost on reboot.
func (s *Server) ReconcileState(
	_ context.Context,
	req *agentv1.ReconcileStateRequest,
) (*agentv1.ReconcileStateResponse, error) {
	vols := req.GetVolumes()
	results := make([]*agentv1.ReconcileItemResult, 0, len(vols))
	for _, vol := range vols {
		results = append(results, s.reconcileVolume(vol))
	}
	return &agentv1.ReconcileStateResponse{
		Results:      results,
		ReconciledAt: timestamppb.Now(),
	}, nil
}

// reconcileVolume applies all desired exports for one volume and returns a
// per-volume result.
func (s *Server) reconcileVolume(vol *agentv1.VolumeDesiredState) *agentv1.ReconcileItemResult {
	for _, export := range vol.GetExports() {
		err := s.applyExport(vol.GetVolumeId(), vol.GetDevicePath(), export)
		if err != nil {
			return &agentv1.ReconcileItemResult{
				VolumeId:     vol.GetVolumeId(),
				Success:      false,
				ErrorMessage: err.Error(),
			}
		}
	}
	return &agentv1.ReconcileItemResult{
		VolumeId: vol.GetVolumeId(),
		Success:  true,
	}
}

// applyExport applies a single desired export state for NVMe-oF TCP.
// Non-NVMe-oF exports are silently skipped (Phase 1 only supports NVMe-oF TCP).
func (s *Server) applyExport(
	volumeID, devicePath string,
	export *agentv1.ExportDesiredState,
) error {
	if export.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		return nil
	}
	params := export.GetExportParams().GetNvmeofTcp()
	if params == nil {
		return nil
	}
	t := &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: volumeNQN(volumeID),
		NamespaceID:  1,
		DevicePath:   devicePath,
		BindAddress:  params.GetBindAddress(),
		Port:         params.GetPort(),
		AllowedHosts: export.GetAllowedInitiators(),
	}
	applyErr := t.Apply()
	if applyErr != nil {
		return fmt.Errorf("applyExport %q: %w", volumeID, applyErr)
	}
	return nil
}
