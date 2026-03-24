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

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// testDevicePath is the block-device path used in ReconcileState tests.
const testDevicePath = "/dev/zvol/tank/pvc-abc"

// nvmeofExportState builds an ExportDesiredState for NVMe-oF TCP on port 4420.
func nvmeofExportState(addr string, hosts ...string) *agentv1.ExportDesiredState {
	return &agentv1.ExportDesiredState{
		ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams:      nvmeofExportParams(addr, 4420),
		AllowedInitiators: hosts,
	}
}

// ReconcileState tests.

func TestReconcileState_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("Results len = %d, want 0 for empty volume list", len(resp.GetResults()))
	}
	if resp.GetReconciledAt() == nil {
		t.Error("ReconciledAt timestamp is nil")
	}
}

func TestReconcileState_NvmeofExportCreatesConfigfs(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("192.168.1.10"),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("Results len = %d, want 1", len(resp.GetResults()))
	}
	result := resp.GetResults()[0]
	if !result.GetSuccess() {
		t.Errorf("result.Success=false, ErrorMessage=%q", result.GetErrorMessage())
	}
	if result.GetVolumeId() != testVolumeID {
		t.Errorf("result.VolumeId = %q, want %q", result.GetVolumeId(), testVolumeID)
	}

	// The configfs subsystem directory must have been created.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created by ReconcileState: %v", statErr)
	}
}

func TestReconcileState_NonNvmeofSkipped(t *testing.T) {
	t.Parallel()
	// Non-NVMe-oF exports are silently skipped (Phase 1 only supports NVMe-oF TCP).
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
						ExportParams: &agentv1.ExportParams{},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("Results len = %d, want 1", len(resp.GetResults()))
	}
	// Non-NVMe-oF export is silently skipped → success.
	if !resp.GetResults()[0].GetSuccess() {
		t.Errorf("expected success for skipped non-NVMe-oF export, got error: %q",
			resp.GetResults()[0].GetErrorMessage())
	}
}

func TestReconcileState_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	req := &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1"),
				},
			},
		},
	}
	// First reconcile.
	resp1, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("first ReconcileState unexpected error: %v", err)
	}
	if !resp1.GetResults()[0].GetSuccess() {
		t.Fatalf("first reconcile failed: %q", resp1.GetResults()[0].GetErrorMessage())
	}

	// Second reconcile on the same state must also succeed (idempotent).
	resp2, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("second ReconcileState unexpected error: %v", err)
	}
	if !resp2.GetResults()[0].GetSuccess() {
		t.Fatalf("second reconcile failed (not idempotent): %q",
			resp2.GetResults()[0].GetErrorMessage())
	}
}

func TestReconcileState_WithAllowedInitiators(t *testing.T) {
	t.Parallel()
	hostNQN := testHostNQN
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1", hostNQN),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile failed: %q", resp.GetResults()[0].GetErrorMessage())
	}

	// The host directory must have been created by Apply().
	hostDir := filepath.Join(cfgRoot, "nvmet", "hosts", hostNQN)
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("host dir not created: %v", statErr)
	}
	// The allowed_hosts symlink must exist.
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", hostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Errorf("allowed_hosts symlink not created: %v", statErr)
	}
}

func TestReconcileState_MultipleVolumes(t *testing.T) {
	t.Parallel()
	const secondVolumeID = "tank/pvc-def"
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1"),
				},
			},
			{
				VolumeId:   secondVolumeID,
				DevicePath: "/dev/zvol/tank/pvc-def",
				Exports:    []*agentv1.ExportDesiredState{},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("Results len = %d, want 2", len(resp.GetResults()))
	}

	// Both volumes must succeed.
	for _, result := range resp.GetResults() {
		if !result.GetSuccess() {
			t.Errorf("volume %q reconcile failed: %q",
				result.GetVolumeId(), result.GetErrorMessage())
		}
	}
	// First result must be for the first volume.
	if resp.GetResults()[0].GetVolumeId() != testVolumeID {
		t.Errorf("Results[0].VolumeId = %q, want %q",
			resp.GetResults()[0].GetVolumeId(), testVolumeID)
	}
	if resp.GetResults()[1].GetVolumeId() != secondVolumeID {
		t.Errorf("Results[1].VolumeId = %q, want %q",
			resp.GetResults()[1].GetVolumeId(), secondVolumeID)
	}
}
