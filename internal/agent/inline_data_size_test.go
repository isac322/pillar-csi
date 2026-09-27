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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// nvmeofExportParamsInline builds NVMe-oF export params on 10.0.0.1:4420
// with the given in-capsule data size (0: none requested).
func nvmeofExportParamsInline(size int32) *agentv1.ExportParams {
	params := nvmeofExportParams("10.0.0.1", 4420)
	params.GetNvmeofTcp().InCapsuleDataSize = size
	return params
}

func inlineFence(t *testing.T, volumeID string) *agentv1.FencingToken {
	t.Helper()
	return &agentv1.FencingToken{VolumeUid: t.Name() + "/" + volumeID, Generation: 1}
}

// portInlineDataSize returns the param_inline_data_size of the only port.
func portInlineDataSize(t *testing.T, cfgRoot string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cfgRoot, "nvmet", "ports", "*", "param_inline_data_size"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("param_inline_data_size files = %v (err %v), want exactly one", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read %s: %v", paths[0], err)
	}
	return strings.TrimSpace(string(raw))
}

// TestNVMeoFTCPAgentHandler_ExportInlineDataSizeConflict verifies that
// ExportVolume applies the requested in-capsule data size to the port and
// rejects, with FailedPrecondition, a second volume on the same port that
// requires a different value, instead of exporting it with the port's value.
func TestNVMeoFTCPAgentHandler_ExportInlineDataSizeConflict(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)
	ctx := context.Background()

	const first, second = testPool + "/pvc-inline-a", testPool + "/pvc-inline-b"
	_, err := handler.Export(ctx, agent.ExportParams{
		VolumeID: first, Fence: inlineFence(t, first), DevicePath: "/dev/zvol/" + first,
		ProtocolParams: nvmeofExportParamsInline(8192),
	})
	if err != nil {
		t.Fatalf("Export %s: %v", first, err)
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size = %q, want 8192", got)
	}

	_, err = handler.Export(ctx, agent.ExportParams{
		VolumeID: second, Fence: inlineFence(t, second), DevicePath: "/dev/zvol/" + second,
		ProtocolParams: nvmeofExportParamsInline(4096),
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "param_inline_data_size") {
		t.Fatalf("conflicting Export err = %v, want FailedPrecondition naming param_inline_data_size", err)
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size after conflict = %q, want 8192", got)
	}
}

// TestNVMeoFTCPAgentHandler_ExportRejectsInlineDataSizeBelowConnectData
// verifies that a size too small for the fabrics Connect data is rejected
// as InvalidArgument before anything is configured.
func TestNVMeoFTCPAgentHandler_ExportRejectsInlineDataSizeBelowConnectData(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	_, err := handler.Export(context.Background(), agent.ExportParams{
		VolumeID: testVolumeID, Fence: testFence(t), DevicePath: testDevicePath,
		ProtocolParams: nvmeofExportParamsInline(512),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Export err = %v, want InvalidArgument", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)); !os.IsNotExist(statErr) {
		t.Fatalf("subsystem created for a rejected export: %v", statErr)
	}
}

// TestNVMeoFTCPAgentHandler_ReconcileLinksInRequestOrder verifies that a
// restore batch sharing one port links in request order: the first export
// fixes the port's in-capsule data size, a later export accepting any value
// joins it, and a later export requiring another value is reported as a
// conflict rather than exported with the port's value.
func TestNVMeoFTCPAgentHandler_ReconcileLinksInRequestOrder(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)

	const required, unset, conflicting = testPool + "/pvc-required", testPool + "/pvc-unset", testPool + "/pvc-conflict"
	export := func(volumeID string, size int32) agent.ExportDesiredState {
		return agent.ExportDesiredState{
			VolumeID: volumeID, Fence: inlineFence(t, volumeID), DevicePath: "/dev/zvol/" + volumeID,
			ProtocolParams: nvmeofExportParamsInline(size),
		}
	}
	errs := handler.Reconcile(context.Background(), []agent.ExportDesiredState{
		export(required, 8192), export(unset, 0), export(conflicting, 4096),
	})
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Reconcile errors = %v, want the first two exports linked", errs)
	}
	if !errors.Is(errs[2], nvmeof.ErrPortInlineDataSizeConflict) {
		t.Fatalf("conflicting export error = %v, want ErrPortInlineDataSizeConflict", errs[2])
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size = %q, want 8192", got)
	}
}
