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
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
)

// nvmeofExportParamsInline builds NVMe-oF export params on 10.0.0.1:4420
// with the given in-capsule data size (nil: none requested).
func nvmeofExportParamsInline(size *int32) *agentv1.ExportParams {
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
	size := func(v int32) *int32 { return &v }

	const first, second = testPool + "/pvc-inline-a", testPool + "/pvc-inline-b"
	_, err := handler.Export(ctx, agent.ExportParams{
		VolumeID: first, Fence: inlineFence(t, first), DevicePath: "/dev/zvol/" + first,
		ProtocolParams: nvmeofExportParamsInline(size(8192)),
	})
	if err != nil {
		t.Fatalf("Export %s: %v", first, err)
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size = %q, want 8192", got)
	}

	_, err = handler.Export(ctx, agent.ExportParams{
		VolumeID: second, Fence: inlineFence(t, second), DevicePath: "/dev/zvol/" + second,
		ProtocolParams: nvmeofExportParamsInline(size(4096)),
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "param_inline_data_size") {
		t.Fatalf("conflicting Export err = %v, want FailedPrecondition naming param_inline_data_size", err)
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size after conflict = %q, want 8192", got)
	}
}

// TestNVMeoFTCPAgentHandler_ReconcileLinksRequiredInlineDataSizeFirst
// verifies that a restore batch sharing one port succeeds when an export
// without an in-capsule data size is listed before one that requires a
// value: the export requiring the value must enable the port, since the
// kernel freezes the attribute once the port has a linked subsystem.
func TestNVMeoFTCPAgentHandler_ReconcileLinksRequiredInlineDataSizeFirst(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)
	size := int32(8192)

	const unset, required = testPool + "/pvc-unset", testPool + "/pvc-required"
	errs := handler.Reconcile(context.Background(), []agent.ExportDesiredState{
		{
			VolumeID: unset, Fence: inlineFence(t, unset), DevicePath: "/dev/zvol/" + unset,
			ProtocolParams: nvmeofExportParamsInline(nil),
		},
		{
			VolumeID: required, Fence: inlineFence(t, required), DevicePath: "/dev/zvol/" + required,
			ProtocolParams: nvmeofExportParamsInline(&size),
		},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("Reconcile export %d: %v", i, err)
		}
	}
	if got := portInlineDataSize(t, cfgRoot); got != "8192" {
		t.Fatalf("param_inline_data_size = %q, want 8192", got)
	}
}
