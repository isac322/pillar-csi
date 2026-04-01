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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bhyoo/pillar-csi/internal/agent"
)

func newNVMeoFTCPHandler(t *testing.T) (handler *agent.NVMeoFTCPAgentHandler, cfgRoot string) {
	t.Helper()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})
	return agent.NewNVMeoFTCPAgentHandler(srv), cfgRoot
}

func TestNVMeoFTCPAgentHandler_Export(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	resp, err := handler.Export(context.Background(), agent.ExportParams{
		VolumeID:       testVolumeID,
		DevicePath:     testDevicePath,
		ProtocolParams: nvmeofExportParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("Export unexpected error: %v", err)
	}

	if resp.TargetID != testVolumeNQN {
		t.Errorf("TargetID = %q, want %q", resp.TargetID, testVolumeNQN)
	}
	if resp.Address != "192.168.1.10" {
		t.Errorf("Address = %q, want 192.168.1.10", resp.Address)
	}
	if resp.Port != 4420 {
		t.Errorf("Port = %d, want 4420", resp.Port)
	}
	if resp.VolumeRef != "1" {
		t.Errorf("VolumeRef = %q, want 1", resp.VolumeRef)
	}

	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
}

func TestNVMeoFTCPAgentHandler_ExportMissingParams(t *testing.T) {
	t.Parallel()
	handler, _ := newNVMeoFTCPHandler(t)

	_, err := handler.Export(context.Background(), agent.ExportParams{
		VolumeID: testVolumeID,
	})
	if err == nil {
		t.Fatal("expected error for missing params, got nil")
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestNVMeoFTCPAgentHandler_AllowAndDenyInitiator(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	_, err := handler.Export(context.Background(), agent.ExportParams{
		VolumeID:       testVolumeID,
		DevicePath:     testDevicePath,
		ProtocolParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err != nil {
		t.Fatalf("Export setup unexpected error: %v", err)
	}

	if err := handler.AllowInitiator(context.Background(), testVolumeID, testHostNQN); err != nil {
		t.Fatalf("AllowInitiator unexpected error: %v", err)
	}

	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", testHostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Fatalf("allowed_hosts symlink not created: %v", statErr)
	}

	if err := handler.DenyInitiator(context.Background(), testVolumeID, testHostNQN); err != nil {
		t.Fatalf("DenyInitiator unexpected error: %v", err)
	}
	if _, statErr := os.Lstat(linkPath); !os.IsNotExist(statErr) {
		t.Fatalf("allowed_hosts symlink still exists after deny: %v", statErr)
	}
}

func TestNVMeoFTCPAgentHandler_Unexport(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	_, err := handler.Export(context.Background(), agent.ExportParams{
		VolumeID:       testVolumeID,
		DevicePath:     testDevicePath,
		ProtocolParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err != nil {
		t.Fatalf("Export setup unexpected error: %v", err)
	}

	if err := handler.Unexport(context.Background(), testVolumeID); err != nil {
		t.Fatalf("Unexport unexpected error: %v", err)
	}

	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); !os.IsNotExist(statErr) {
		t.Fatalf("subsystem dir still exists after unexport: %v", statErr)
	}
}

func TestNVMeoFTCPAgentHandler_Reconcile(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	err := handler.Reconcile(context.Background(), []agent.ExportDesiredState{
		{
			VolumeID:          testVolumeID,
			DevicePath:        testDevicePath,
			ProtocolParams:    nvmeofExportParams("10.0.0.1", 4420),
			AllowedInitiators: []string{testHostNQN},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile unexpected error: %v", err)
	}

	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Fatalf("subsystem dir not created by reconcile: %v", statErr)
	}

	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "allowed_hosts", testHostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Fatalf("allowed_hosts symlink not created by reconcile: %v", statErr)
	}
}

var _ agent.AgentProtocolHandler = (*agent.NVMeoFTCPAgentHandler)(nil)
