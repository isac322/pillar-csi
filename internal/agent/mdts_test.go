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

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
)

// nvmeofExportParamsMDTS builds NVMe-oF export params on 10.0.0.1:4420 that
// require the given maximum data transfer size.
func nvmeofExportParamsMDTS(size int32) *agentv1.ExportParams {
	params := nvmeofExportParams("10.0.0.1", 4420)
	params.GetNvmeofTcp().MaxDataTransferSize = &size
	return params
}

// emulateMDTSKernel prepares the configfs port of 10.0.0.1:4420 as a Linux
// >= 7.1 kernel creates it, with param_mdts "0".  The port ID is learned
// from a probe export that is removed again.
func emulateMDTSKernel(t *testing.T, handler *agent.NVMeoFTCPAgentHandler, cfgRoot string) {
	t.Helper()
	ctx := context.Background()
	const probe = testPool + "/pvc-mdts-probe"
	_, err := handler.Export(ctx, agent.ExportParams{
		VolumeID: probe, Fence: inlineFence(t, probe), DevicePath: "/dev/zvol/" + probe,
		ProtocolParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err != nil {
		t.Fatalf("Export probe: %v", err)
	}
	ports, err := filepath.Glob(filepath.Join(cfgRoot, "nvmet", "ports", "*"))
	if err != nil || len(ports) != 1 {
		t.Fatalf("ports = %v (err %v), want exactly one", ports, err)
	}
	if err := handler.Unexport(ctx, probe, inlineFence(t, probe)); err != nil {
		t.Fatalf("Unexport probe: %v", err)
	}
	if err := os.MkdirAll(ports[0], 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ports[0], "param_mdts"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// portMDTS returns the param_mdts of the only port.
func portMDTS(t *testing.T, cfgRoot string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cfgRoot, "nvmet", "ports", "*", "param_mdts"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("param_mdts files = %v (err %v), want exactly one", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read %s: %v", paths[0], err)
	}
	return strings.TrimSpace(string(raw))
}

// TestNVMeoFTCPAgentHandler_ExportMDTSConflict verifies that ExportVolume
// applies the requested maximum data transfer size to the port, rejects
// with FailedPrecondition a second volume on the same port that allows only
// a smaller limit, instead of exporting it with the port's larger limit,
// and accepts one that allows a larger limit.
func TestNVMeoFTCPAgentHandler_ExportMDTSConflict(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)
	emulateMDTSKernel(t, handler, cfgRoot)
	ctx := context.Background()
	export := func(volumeID string, size int32) error {
		_, err := handler.Export(ctx, agent.ExportParams{
			VolumeID: volumeID, Fence: inlineFence(t, volumeID), DevicePath: "/dev/zvol/" + volumeID,
			ProtocolParams: nvmeofExportParamsMDTS(size),
		})
		return err
	}

	const first, smaller, larger = testPool + "/pvc-mdts-a", testPool + "/pvc-mdts-b", testPool + "/pvc-mdts-c"
	if err := export(first, 4<<20); err != nil {
		t.Fatalf("Export %s: %v", first, err)
	}
	if got := portMDTS(t, cfgRoot); got != "10" {
		t.Fatalf("param_mdts = %q, want 10", got)
	}

	err := export(smaller, 2<<20)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "param_mdts") {
		t.Fatalf("conflicting Export err = %v, want FailedPrecondition naming param_mdts", err)
	}
	if err := export(larger, 8<<20); err != nil {
		t.Fatalf("Export %s allowing a larger limit: %v", larger, err)
	}
	if got := portMDTS(t, cfgRoot); got != "10" {
		t.Fatalf("param_mdts after conflict = %q, want 10", got)
	}
}

// TestNVMeoFTCPAgentHandler_ReconcileRestoresMixedMDTS verifies that exports
// a port advertising no limit accepted with different limits are restored
// after a reboot cleared configfs, whatever their order: the port is set to
// the smallest limit before the first link, which every export allows.
func TestNVMeoFTCPAgentHandler_ReconcileRestoresMixedMDTS(t *testing.T) {
	t.Parallel()
	handler, cfgRoot := newNVMeoFTCPHandler(t)
	emulateMDTSKernel(t, handler, cfgRoot)
	ctx := context.Background()

	const unlimited, four, eight = testPool + "/pvc-mdts-old", testPool + "/pvc-mdts-4m", testPool + "/pvc-mdts-8m"
	desired := func(volumeID string, params *agentv1.ExportParams) agent.ExportDesiredState {
		return agent.ExportDesiredState{
			VolumeID: volumeID, Fence: inlineFence(t, volumeID), DevicePath: "/dev/zvol/" + volumeID,
			ProtocolParams: params,
		}
	}
	exportFour, exportEight := desired(four, nvmeofExportParamsMDTS(4<<20)), desired(eight, nvmeofExportParamsMDTS(8<<20))
	for _, export := range []agent.ExportDesiredState{
		desired(unlimited, nvmeofExportParamsMDTS(0)), exportFour, exportEight,
	} {
		if errs := handler.Reconcile(ctx, []agent.ExportDesiredState{export}); errs[0] != nil {
			t.Fatalf("export %s: %v", export.VolumeID, errs[0])
		}
	}
	if got := portMDTS(t, cfgRoot); got != "0" {
		t.Fatalf("param_mdts before reboot = %q, want 0", got)
	}

	for _, order := range [][]agent.ExportDesiredState{{exportFour, exportEight}, {exportEight, exportFour}} {
		if err := os.RemoveAll(filepath.Join(cfgRoot, "nvmet")); err != nil {
			t.Fatal(err)
		}
		emulateMDTSKernel(t, handler, cfgRoot)
		errs := handler.Reconcile(ctx, order)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("restore %s after %s: %v", order[i].VolumeID, order[0].VolumeID, err)
			}
		}
		if got := portMDTS(t, cfgRoot); got != "10" {
			t.Fatalf("param_mdts after restoring %s first = %q, want 10", order[0].VolumeID, got)
		}
	}
}

// TestNVMeoFTCPAgentHandler_ExportRejectsInvalidMDTS verifies that a size
// param_mdts cannot express is rejected as InvalidArgument before anything
// is configured.
func TestNVMeoFTCPAgentHandler_ExportRejectsInvalidMDTS(t *testing.T) {
	t.Parallel()
	for _, size := range []int32{4096, 5000} {
		handler, cfgRoot := newNVMeoFTCPHandler(t)
		_, err := handler.Export(context.Background(), agent.ExportParams{
			VolumeID: testVolumeID, Fence: testFence(t), DevicePath: testDevicePath,
			ProtocolParams: nvmeofExportParamsMDTS(size),
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Export with max_data_transfer_size %d err = %v, want InvalidArgument", size, err)
		}
		subsystem := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
		if _, statErr := os.Stat(subsystem); !os.IsNotExist(statErr) {
			t.Fatalf("subsystem created for a rejected export: %v", statErr)
		}
	}
}
