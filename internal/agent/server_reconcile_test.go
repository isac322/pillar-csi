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

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
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

// nvmeofACLExportState builds an ACL-enforced ExportDesiredState on
// 10.0.0.1:4420 whose exact initiator set is hosts.
func nvmeofACLExportState(hosts ...string) *agentv1.ExportDesiredState {
	state := nvmeofExportState("10.0.0.1", hosts...)
	state.AclEnabled = true
	return state
}

// reconcileOne reconciles a single NVMe-oF volume and fails the test unless
// the agent reports success.
func reconcileOne(t *testing.T, srv *agent.Server, devicePath string, export *agentv1.ExportDesiredState) {
	t.Helper()
	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId:   testVolumeID,
			DevicePath: devicePath,
			Exports:    []*agentv1.ExportDesiredState{export},
			Fence:      testFence(t),
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 || !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile failed: %v", resp.GetResults())
	}
}

func readAllowAnyHost(t *testing.T, cfgRoot, nqn string) string {
	t.Helper()
	//nolint:gosec // G304: test reads a file under t.TempDir().
	raw, err := os.ReadFile(filepath.Join(cfgRoot, "nvmet", "subsystems", nqn, "attr_allow_any_host"))
	if err != nil {
		t.Fatalf("read attr_allow_any_host of %s: %v", nqn, err)
	}
	return strings.TrimSpace(string(raw))
}

func allowedHostLinked(cfgRoot, nqn, host string) bool {
	_, err := os.Lstat(filepath.Join(cfgRoot, "nvmet", "subsystems", nqn, "allowed_hosts", host))
	return err == nil
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
				Fence:      testFence(t),
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

func TestReconcileState_UnsupportedProtocolReported(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				Fence:      testFence(t),
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
	if resp.GetResults()[0].GetSuccess() {
		t.Fatal("expected unsupported protocol to be reported as a reconcile failure")
	}
	if !strings.Contains(
		resp.GetResults()[0].GetErrorMessage(),
		"protocol PROTOCOL_TYPE_ISCSI is not supported by this agent",
	) {
		t.Errorf("ErrorMessage = %q, want unsupported protocol detail", resp.GetResults()[0].GetErrorMessage())
	}
}

func TestReconcileState_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	req := &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   testVolumeID,
				Fence:      testFence(t),
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
				Fence:      testFence(t),
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofACLExportState(hostNQN),
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
				Fence:      testFence(t),
				DevicePath: testDevicePath,
				Exports: []*agentv1.ExportDesiredState{
					nvmeofExportState("10.0.0.1"),
				},
			},
			{
				VolumeId:   secondVolumeID,
				Fence:      testFence(t),
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

// TestReconcileState_RestoresLostTargetState: after the target loses its
// configfs state (reboot), reconcile re-creates the export with the ACL
// closed to everyone except the published initiator.
func TestReconcileState_RestoresLostTargetState(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testDevicePath, AclEnabled: true,
		Fence: testFence(t),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(cfgRoot, "nvmet")); err != nil {
		t.Fatalf("simulate target state loss: %v", err)
	}

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(testHostNQN))

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "0" {
		t.Errorf("attr_allow_any_host = %q, want 0", got)
	}
	if !allowedHostLinked(cfgRoot, testVolumeNQN, testHostNQN) {
		t.Errorf("allowed_hosts entry for %s not restored", testHostNQN)
	}
	ns := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1", "device_path")
	//nolint:gosec // G304: test reads a file under t.TempDir().
	if raw, readErr := os.ReadFile(ns); readErr != nil || strings.TrimSpace(string(raw)) != testDevicePath {
		t.Errorf("namespace device_path = %q (err %v), want %q", raw, readErr, testDevicePath)
	}
}

// TestReconcileState_EmptyACLStaysClosed: an ACL-enforced volume without any
// published node must admit nobody after reconcile (fail-closed).
func TestReconcileState_EmptyACLStaysClosed(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState())

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "0" {
		t.Errorf("attr_allow_any_host = %q, want 0 (empty ACL must not fail open)", got)
	}
}

// TestReconcileState_ACLDisabledStaysOpen: without ACL enforcement the target
// admits any initiator even when initiators are listed.
func TestReconcileState_ACLDisabledStaysOpen(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1", testHostNQN))

	if got := readAllowAnyHost(t, cfgRoot, testVolumeNQN); got != "1" {
		t.Errorf("attr_allow_any_host = %q, want 1", got)
	}
}

// TestReconcileState_RevokesHostsOutsideDesiredSet: the ACL converges to the
// exact desired set, while the shared host object and other subsystems'
// grants of the revoked host are untouched.
func TestReconcileState_RevokesHostsOutsideDesiredSet(t *testing.T) {
	t.Parallel()
	const otherHost = "nqn.2023-01.io.example:host-2"
	const foreignNQN = "nqn.2014-08.org.example:foreign"
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})
	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(testHostNQN, otherHost))

	foreignLink := filepath.Join(cfgRoot, "nvmet", "subsystems", foreignNQN, "allowed_hosts", testHostNQN)
	if err := os.MkdirAll(filepath.Dir(foreignLink), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cfgRoot, "nvmet", "hosts", testHostNQN), foreignLink); err != nil {
		t.Fatal(err)
	}

	reconcileOne(t, srv, testDevicePath, nvmeofACLExportState(otherHost))

	if allowedHostLinked(cfgRoot, testVolumeNQN, testHostNQN) {
		t.Errorf("host %s outside the desired set is still allowed", testHostNQN)
	}
	if !allowedHostLinked(cfgRoot, testVolumeNQN, otherHost) {
		t.Errorf("desired host %s lost its grant", otherHost)
	}
	if _, err := os.Lstat(foreignLink); err != nil {
		t.Errorf("foreign subsystem grant was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfgRoot, "nvmet", "hosts", testHostNQN)); err != nil {
		t.Errorf("shared host object removed: %v", err)
	}
}

// TestReconcileState_DerivesDevicePathFromBackend: an empty device_path is
// resolved through the backend that owns the volume.
func TestReconcileState_DerivesDevicePathFromBackend(t *testing.T) {
	t.Parallel()
	const backendPath = "/dev/zvol/tank/pvc-abc-from-backend"
	srv, cfgRoot := newExportTestServer(t, &mockBackend{devicePathResult: backendPath})

	reconcileOne(t, srv, "", nvmeofACLExportState())

	ns := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1", "device_path")
	raw, err := os.ReadFile(ns) //nolint:gosec // G304: test reads a file under t.TempDir().
	if err != nil || strings.TrimSpace(string(raw)) != backendPath {
		t.Errorf("namespace device_path = %q (err %v), want %q", raw, err, backendPath)
	}
}
