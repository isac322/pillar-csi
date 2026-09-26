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
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// Export restore regression tests (issue #92): after a storage-node reboot
// the agent must not make a shared port listen before every export on it is
// fully configured, or hosts reconnecting to a not-yet-linked subsystem get a
// do-not-retry rejection and delete their controllers.

const restoreHostNQN = "nqn.2023-01.io.example:restore-host"

// restoreVolume is one volume of a complete ReconcileState in these tests.
type restoreVolume struct {
	id     string
	device string
	port   int32
	acl    bool
}

func (v restoreVolume) nqn() string {
	return "nqn.2026-01.com.bhyoo.pillar-csi:" + strings.ReplaceAll(v.id, "/", ".")
}

func (v restoreVolume) desired(t *testing.T) *agentv1.VolumeDesiredState {
	t.Helper()
	export := &agentv1.ExportDesiredState{
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", v.port),
		AclEnabled:   v.acl,
	}
	if v.acl {
		export.AllowedInitiators = []string{restoreHostNQN}
	}
	return &agentv1.VolumeDesiredState{
		VolumeId:   v.id,
		DevicePath: v.device,
		Exports:    []*agentv1.ExportDesiredState{export},
		Fence:      &agentv1.FencingToken{VolumeUid: t.Name() + "/" + v.id, Generation: 1},
	}
}

func newGatedServer(t *testing.T, cfgRoot, stateDir string) *agent.Server {
	t.Helper()
	backends := map[string]backend.VolumeBackend{testPool: &mockBackend{}}
	srv := agent.NewServer(backends, cfgRoot, agent.WithDrainStateDir(stateDir), agent.WithExportRestoreGate())
	agent.SetDeviceChecker(t, srv, nvmeof.AlwaysPresentChecker)
	return srv
}

// portLinks returns every ports/<id>/subsystems/<nqn> entry, i.e. every
// subsystem a host could reach.
func portLinks(t *testing.T, cfgRoot string) []string {
	t.Helper()
	links, err := filepath.Glob(filepath.Join(cfgRoot, "nvmet", "ports", "*", "subsystems", "*"))
	if err != nil {
		t.Fatalf("glob port links: %v", err)
	}
	return links
}

func restorePending(t *testing.T, srv *agent.Server) bool {
	t.Helper()
	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	return resp.GetExportRestorePending()
}

func completeReconcile(
	t *testing.T,
	srv *agent.Server,
	vols ...restoreVolume,
) *agentv1.ReconcileStateResponse {
	t.Helper()
	req := &agentv1.ReconcileStateRequest{Complete: true}
	for _, v := range vols {
		req.Volumes = append(req.Volumes, v.desired(t))
	}
	resp, err := srv.ReconcileState(context.Background(), req)
	if err != nil {
		t.Fatalf("complete ReconcileState: %v", err)
	}
	return resp
}

// assertReachableAndReady checks the state a host sees through a port link:
// namespace enabled with the pinned identity, and the ACL in place.
func assertReachableAndReady(t *testing.T, cfgRoot string, v restoreVolume) {
	t.Helper()
	nvmet := filepath.Join(cfgRoot, "nvmet")
	links, err := filepath.Glob(filepath.Join(nvmet, "ports", "*", "subsystems", v.nqn()))
	if err != nil || len(links) != 1 {
		t.Fatalf("%s: port links = %v (err %v), want exactly one", v.id, links, err)
	}
	nsDir := filepath.Join(nvmet, "subsystems", v.nqn(), "namespaces", "1")
	id := nvmeof.DeriveIdentity(v.nqn(), 1)
	for file, want := range map[string]string{
		"enable":       "1",
		"device_path":  v.device,
		"device_uuid":  id.UUID,
		"device_nguid": id.NGUID,
	} {
		assertTrimmedFile(t, filepath.Join(nsDir, file), want)
	}
	wantAllowAny := "1"
	if v.acl {
		wantAllowAny = "0"
		if !allowedHostLinked(cfgRoot, v.nqn(), restoreHostNQN) {
			t.Fatalf("%s: allowed host %s missing", v.id, restoreHostNQN)
		}
	}
	if got := readAllowAnyHost(t, cfgRoot, v.nqn()); got != wantAllowAny {
		t.Fatalf("%s: attr_allow_any_host = %q, want %q", v.id, got, wantAllowAny)
	}
}

func assertTrimmedFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file under t.TempDir().
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// TestExportRestore_NoPortListensBeforeEveryExportIsPrepared restores exports
// sharing port 4420 plus one on another port, with ACL on and off.  The device
// check runs in each export's prepare step, so it observes the configfs tree
// while the batch is being prepared: no subsystem may be linked to any port
// until the last export was prepared.
func TestExportRestore_NoPortListensBeforeEveryExportIsPrepared(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	srv := newGatedServer(t, cfgRoot, t.TempDir())

	vols := []restoreVolume{
		{id: testPool + "/pvc-a", device: "/dev/zvol/tank/pvc-a", port: 4420, acl: true},
		{id: testPool + "/pvc-b", device: "/dev/zvol/tank/pvc-b", port: 4420, acl: false},
		{id: testPool + "/pvc-c", device: "/dev/zvol/tank/pvc-c", port: 4420, acl: true},
		{id: testPool + "/pvc-d", device: "/dev/zvol/tank/pvc-d", port: 4421, acl: true},
	}
	var checked []string
	agent.SetDeviceChecker(t, srv, func(path string) (bool, error) {
		if links := portLinks(t, cfgRoot); len(links) != 0 {
			t.Errorf("preparing %s while subsystems are already reachable: %v", path, links)
		}
		checked = append(checked, path)
		return true, nil
	})

	resp := completeReconcile(t, srv, vols...)

	for _, r := range resp.GetResults() {
		if !r.GetSuccess() {
			t.Fatalf("volume %s failed: %s", r.GetVolumeId(), r.GetErrorMessage())
		}
	}
	if len(checked) != len(vols) {
		t.Fatalf("device checks = %v, want one per volume", checked)
	}
	for _, v := range vols {
		assertReachableAndReady(t, cfgRoot, v)
	}
	if restorePending(t, srv) {
		t.Fatal("export restore still pending after a complete ReconcileState")
	}
}

// TestExportRestore_GateRejectsExportCreatingRPCs verifies that while the
// restore is pending nothing can make a port listen on behalf of one volume,
// that revoke-only RPCs still work, and that an empty complete request (an
// agent without exports) opens the gate.
func TestExportRestore_GateRejectsExportCreatingRPCs(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	srv := newGatedServer(t, cfgRoot, t.TempDir())
	ctx := context.Background()

	if !restorePending(t, srv) {
		t.Fatal("gated agent must start with the export restore pending")
	}
	exportReq := &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		DevicePath:   testDevicePath,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
		Fence:        testFence(t),
	}
	_, exportErr := srv.ExportVolume(ctx, exportReq)
	_, allowErr := srv.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  testHostNQN,
		Fence:        testFence(t),
	})
	_, reconcileErr := srv.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId:   testVolumeID,
			DevicePath: testDevicePath,
			Exports:    []*agentv1.ExportDesiredState{nvmeofExportState("10.0.0.1")},
			Fence:      testFence(t),
		}},
	})
	for name, err := range map[string]error{
		"ExportVolume": exportErr, "AllowInitiator": allowErr, "ReconcileState": reconcileErr,
	} {
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("%s while restore pending: err = %v, want Unavailable", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfgRoot, "nvmet"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read nvmet dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("gated RPCs touched configfs: %v", entries)
	}

	_, err = srv.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Fence:        testFence(t),
	})
	if err != nil {
		t.Fatalf("UnexportVolume while restore pending: %v", err)
	}

	completeReconcile(t, srv)
	if restorePending(t, srv) {
		t.Fatal("empty complete ReconcileState did not end the restore")
	}
	_, err = srv.ExportVolume(ctx, exportReq)
	if err != nil {
		t.Fatalf("ExportVolume after restore: %v", err)
	}
}

// TestExportRestore_FailedExportDoesNotBlockOthers verifies that an export
// whose device never appears is reported and left unlinked, while the other
// exports are restored and the gate opens.
func TestExportRestore_FailedExportDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	srv := newGatedServer(t, cfgRoot, t.TempDir())
	missing := restoreVolume{id: testPool + "/pvc-gone", device: "/dev/zvol/tank/pvc-gone", port: 4420, acl: true}
	ready := restoreVolume{id: testPool + "/pvc-ok", device: "/dev/zvol/tank/pvc-ok", port: 4420, acl: true}
	agent.SetDevicePollParams(t, srv, time.Millisecond, 20*time.Millisecond)
	agent.SetDeviceChecker(t, srv, func(path string) (bool, error) {
		return path != missing.device, nil
	})

	resp := completeReconcile(t, srv, missing, ready)

	results := resp.GetResults()
	if len(results) != 2 || results[0].GetSuccess() || !results[1].GetSuccess() {
		t.Fatalf("results = %v, want [failure success]", results)
	}
	links := portLinks(t, cfgRoot)
	if len(links) != 1 || filepath.Base(links[0]) != ready.nqn() {
		t.Fatalf("port links = %v, want only %s", links, ready.nqn())
	}
	assertReachableAndReady(t, cfgRoot, ready)
	if restorePending(t, srv) {
		t.Fatal("export restore still pending after a complete ReconcileState")
	}
}

// TestExportRestore_RestartWithoutRebootKeepsLiveExports covers an agent
// restart while configfs is intact: the restore must re-apply the live export
// without unlinking it or disturbing its enabled namespace.
func TestExportRestore_RestartWithoutRebootKeepsLiveExports(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	stateDir := t.TempDir()
	vol := restoreVolume{id: testPool + "/pvc-live", device: "/dev/zvol/tank/pvc-live", port: 4420, acl: true}

	first := newGatedServer(t, cfgRoot, stateDir)
	completeReconcile(t, first, vol)
	before := portLinks(t, cfgRoot)
	if len(before) != 1 {
		t.Fatalf("port links after first restore = %v, want one", before)
	}
	linkInfo, err := os.Lstat(before[0])
	if err != nil {
		t.Fatalf("lstat port link: %v", err)
	}

	restarted := newGatedServer(t, cfgRoot, stateDir)
	agent.SetDeviceChecker(t, restarted, func(string) (bool, error) {
		if len(portLinks(t, cfgRoot)) != 1 {
			t.Error("live export was unlinked during the restore")
		}
		return true, nil
	})
	resp := completeReconcile(t, restarted, vol)
	if !resp.GetResults()[0].GetSuccess() {
		t.Fatalf("restore after restart failed: %s", resp.GetResults()[0].GetErrorMessage())
	}

	after, err := os.Lstat(before[0])
	if err != nil {
		t.Fatalf("live port link gone after restart: %v", err)
	}
	if !os.SameFile(linkInfo, after) {
		t.Fatal("live port link was recreated instead of kept")
	}
	assertReachableAndReady(t, cfgRoot, vol)
}
