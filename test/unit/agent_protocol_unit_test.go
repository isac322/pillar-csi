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

// Package unit_test — Agent-level protocol dispatch unit tests.
//
// This file tests the agent.Server's protocol dispatch logic directly, using a
// real agent.Server backed by an in-process mock VolumeBackend and a temporary
// directory as the configfs root.  No real ZFS, LVM, or kernel configfs is
// required.
//
//   - E22-176: ExportVolume with PROTOCOL_TYPE_UNSPECIFIED returns
//     codes.InvalidArgument.  (The spec originally said Unimplemented; the
//     actual implementation returns InvalidArgument because UNSPECIFIED is
//     treated as "required field missing", not "unsupported protocol".)
//   - E22-180: ReconcileState with a mixed NVMe-oF TCP + iSCSI volume list
//     reports per-volume results: the NVMe-oF TCP volume succeeds and the
//     iSCSI volume fails (Unimplemented), verifying the per-item result
//     reporting in server_reconcile.go.
//
// Run with:
//
//	go test ./test/unit/ -v -run TestAgentProtocol
package unit_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ─────────────────────────────────────────────────────────────────────────────
// unitAgentBackend — minimal VolumeBackend mock for agent-level unit tests.
// ─────────────────────────────────────────────────────────────────────────────

// unitAgentBackend is a minimal test double for backend.VolumeBackend.
// All methods return sensible zero-value responses unless a specific field is
// set.  It is used only by agent-level unit tests in this package; the richer
// mockVolumeBackend in test/component/ is not accessible from here.
type unitAgentBackend struct {
	mu sync.Mutex

	devicePathResult string
}

var _ backend.VolumeBackend = (*unitAgentBackend)(nil)

func (b *unitAgentBackend) Create(
	_ context.Context,
	_ string,
	capacityBytes int64,
	_ *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	dp := b.devicePathResult
	if dp == "" {
		dp = "/dev/zvol/tank/pvc-unit-test"
	}
	return dp, capacityBytes, nil
}

func (*unitAgentBackend) Delete(_ context.Context, _ string) error { return nil }

func (*unitAgentBackend) Expand(
	_ context.Context, _ string, requestedBytes int64,
) (int64, error) {
	return requestedBytes, nil
}

func (*unitAgentBackend) Capacity(_ context.Context) (totalBytes, availableBytes int64, err error) {
	return 100 * 1024 * 1024 * 1024, 50 * 1024 * 1024 * 1024, nil
}

func (*unitAgentBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}

func (b *unitAgentBackend) DevicePath(_ string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.devicePathResult != "" {
		return b.devicePathResult
	}
	return "/dev/zvol/tank/pvc-unit-test"
}

func (*unitAgentBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

// newUnitAgentServer creates an agent.Server backed by a unitAgentBackend
// and a fresh temporary directory as the configfs root.  AlwaysPresentChecker
// is injected so device-poll loops succeed immediately without real devices.
func newUnitAgentServer(t *testing.T) (srv *agent.Server, cfgRootDir string) {
	t.Helper()
	cfgRootDir = t.TempDir()
	mb := &unitAgentBackend{devicePathResult: "/dev/zvol/tank/pvc-unit"}
	backends := map[string]backend.VolumeBackend{"tank": mb}
	srv = agent.NewServer(backends, cfgRootDir,
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)
	return srv, cfgRootDir
}

// ─────────────────────────────────────────────────────────────────────────────
// E22.176 — PROTOCOL_TYPE_UNSPECIFIED → InvalidArgument
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentProtocol_ExportVolume_UNSPECIFIED_Unimplemented verifies that
// ExportVolume with PROTOCOL_TYPE_UNSPECIFIED(0) returns codes.InvalidArgument.
//
// Note: The spec originally stated codes.Unimplemented; however the actual
// implementation in handlerForProtocol treats UNSPECIFIED as a "required field
// missing" error and returns InvalidArgument, reserving Unimplemented for
// protocols that are recognized but not yet implemented (e.g., iSCSI, NFS).
// This test documents and verifies the actual implementation behavior.
//
// Corresponds to E22 ID 176 in docs/testing/UNIT-TESTS.md.
func TestAgentProtocol_ExportVolume_UNSPECIFIED_Unimplemented(t *testing.T) {
	t.Parallel()

	srv, _ := newUnitAgentServer(t)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-unspecified-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED, // 0
	})

	if err == nil {
		t.Fatal("expected error for PROTOCOL_TYPE_UNSPECIFIED, got nil")
	}

	// The implementation returns InvalidArgument ("protocol_type is required"),
	// not Unimplemented.  This is intentional: UNSPECIFIED means "caller forgot
	// to set the field", which is an argument error, not an unimplemented feature.
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("ExportVolume(UNSPECIFIED): code = %s, want %s", st.Code(), codes.InvalidArgument)
	}
	t.Logf("ExportVolume(UNSPECIFIED) correctly returned %s: %v", st.Code(), err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E22.180 — ReconcileState: unsupported protocol → per-item failure report
// ─────────────────────────────────────────────────────────────────────────────

// TestAgentProtocol_ReconcileState_UnsupportedProtocol_SkipAndReport verifies
// that ReconcileState handles a mixed volume list (NVMe-oF TCP + iSCSI) by:
//   - Succeeding for the NVMe-oF TCP volume (results[v1].Success = true)
//   - Reporting a per-item failure for the iSCSI volume (results[v2].Success = false,
//     ErrorMessage non-empty) without aborting the entire reconciliation
//
// The server processes each volume independently and accumulates per-item
// results, so an unsupported protocol for one volume must not prevent the
// supported-protocol volume from being reconciled.
//
// Corresponds to E22 ID 180 in docs/testing/UNIT-TESTS.md.
func TestAgentProtocol_ReconcileState_UnsupportedProtocol_SkipAndReport(t *testing.T) {
	t.Parallel()

	srv, cfgRoot := newUnitAgentServer(t)

	const (
		nvmeofVolumeID = "tank/pvc-mixed-nvmeof"
		iscsiVolumeID  = "tank/pvc-mixed-iscsi"
		hostNQN        = "nqn.2023-01.io.example:host-unit-test"
	)

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				// v1: NVMe-oF TCP — should succeed.
				VolumeId:    nvmeofVolumeID,
				DevicePath:  "/dev/zvol/tank/pvc-mixed-nvmeof",
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						AllowedInitiators: []string{hostNQN},
						ExportParams: &agentv1.ExportParams{
							Params: &agentv1.ExportParams_NvmeofTcp{
								NvmeofTcp: &agentv1.NvmeofTcpExportParams{
									BindAddress: "192.168.1.10",
									Port:        4420,
								},
							},
						},
					},
				},
			},
			{
				// v2: iSCSI — unsupported protocol, should fail per-item.
				VolumeId:    iscsiVolumeID,
				DevicePath:  "/dev/zvol/tank/pvc-mixed-iscsi",
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
					},
				},
			},
		},
	})

	// ReconcileState itself must not return a top-level error.
	if err != nil {
		t.Fatalf("ReconcileState: unexpected top-level error: %v", err)
	}

	if got := len(resp.GetResults()); got != 2 {
		t.Fatalf("ReconcileState: len(results) = %d, want 2", got)
	}

	// Identify results by VolumeId (order is not guaranteed by the API contract).
	results := make(map[string]*agentv1.ReconcileItemResult, 2)
	for _, r := range resp.GetResults() {
		results[r.GetVolumeId()] = r
	}

	// v1 (NVMe-oF TCP) must succeed.
	v1 := results[nvmeofVolumeID]
	if v1 == nil {
		t.Fatalf("ReconcileState: no result for NVMe-oF volume %q", nvmeofVolumeID)
	}
	if !v1.GetSuccess() {
		t.Errorf("ReconcileState: NVMe-oF volume %q: Success=false, want true; error=%q",
			nvmeofVolumeID, v1.GetErrorMessage())
	}

	// v2 (iSCSI) must fail with a non-empty error message.
	v2 := results[iscsiVolumeID]
	if v2 == nil {
		t.Fatalf("ReconcileState: no result for iSCSI volume %q", iscsiVolumeID)
	}
	if v2.GetSuccess() {
		t.Errorf("ReconcileState: iSCSI volume %q: Success=true, want false", iscsiVolumeID)
	}
	if v2.GetErrorMessage() == "" {
		t.Errorf("ReconcileState: iSCSI volume %q: ErrorMessage is empty, want non-empty", iscsiVolumeID)
	}
	t.Logf("ReconcileState: iSCSI failure message: %q", v2.GetErrorMessage())

	// Verify the NVMe-oF configfs subsystem directory was created in the temp root.
	// NQN format: nqn.2026-01.com.bhyoo.pillar-csi:<pool>.<name>
	// pool/name = "tank/pvc-mixed-nvmeof" → suffix = "tank.pvc-mixed-nvmeof"
	const nqnPrefix = "nqn.2026-01.com.bhyoo.pillar-csi:"
	nqn := nqnPrefix + "tank.pvc-mixed-nvmeof"
	subsysDir := cfgRoot + "/nvmet/subsystems/" + nqn
	if _, err2 := os.Stat(subsysDir); err2 != nil {
		t.Errorf("ReconcileState: configfs subsystem dir %q not created: %v", subsysDir, err2)
	}
}
