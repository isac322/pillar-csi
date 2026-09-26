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
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// Issue #90: hosts reconnecting after a storage-node reboot compare the
// namespace identifiers with the ones they cached and drop the namespace on
// any difference.  These tests pin that every re-export presents the same
// identity.

// newIdentityTestServer starts an agent over cfgRoot and stateDir, standing in
// for one agent process on a storage node whose configfs tree and state
// directory are the given paths.
func newIdentityTestServer(t *testing.T, cfgRoot, stateDir string) *agent.Server {
	t.Helper()
	backends := map[string]backend.VolumeBackend{testPool: &mockBackend{}}
	s := agent.NewServer(backends, cfgRoot, agent.WithDrainStateDir(stateDir))
	agent.SetDeviceChecker(t, s, nvmeof.AlwaysPresentChecker)
	return s
}

func readExportedIdentity(t *testing.T, cfgRoot string) nvmeof.Identity {
	t.Helper()
	sub := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	read := func(path string) string {
		//nolint:gosec // G304: test reads a file under t.TempDir().
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return strings.TrimSpace(string(raw))
	}
	return nvmeof.Identity{
		UUID:   read(filepath.Join(sub, "namespaces", "1", "device_uuid")),
		NGUID:  read(filepath.Join(sub, "namespaces", "1", "device_nguid")),
		Serial: read(filepath.Join(sub, "attr_serial")),
	}
}

func simulateReboot(t *testing.T, cfgRoot string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(cfgRoot, "nvmet")); err != nil {
		t.Fatalf("simulate storage-node reboot: %v", err)
	}
}

// TestNVMeIdentity_StableAcrossRebootAndStateLoss: the identity written by
// ExportVolume is reproduced by ReconcileState after the configfs tree is
// lost, even when the agent's own state directory is lost as well.
func TestNVMeIdentity_StableAcrossRebootAndStateLoss(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	srv := newIdentityTestServer(t, cfgRoot, t.TempDir())
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420), DevicePath: testDevicePath,
		Fence: testFence(t),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	exported := readExportedIdentity(t, cfgRoot)
	if exported != nvmeof.DeriveIdentity(testVolumeNQN, 1) {
		t.Fatalf("exported identity %+v is not the derived identity", exported)
	}

	// Reboot with the agent state intact, then reboot onto a fresh state
	// directory (state loss): both re-exports must match the original.
	for _, stateDir := range []string{"", t.TempDir()} {
		simulateReboot(t, cfgRoot)
		if stateDir != "" {
			srv = newIdentityTestServer(t, cfgRoot, stateDir)
		}
		reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))
		if got := readExportedIdentity(t, cfgRoot); got != exported {
			t.Fatalf("re-exported identity %+v, want %+v", got, exported)
		}
	}
}

// legacyLiveIdentity is what a 0.2.0 export looks like: kernel-random uuid
// and serial, zero nguid.
var legacyLiveIdentity = nvmeof.Identity{
	UUID:   "d1f0c7e4-3b0a-4f8e-9c55-2a9b6e7d1c03",
	NGUID:  "00000000-0000-0000-0000-000000000000",
	Serial: "6c1a5e0b2f9d4a31",
}

// writeLegacyExport lays out the configfs objects of a live 0.2.0 export.
func writeLegacyExport(t *testing.T, cfgRoot string) {
	t.Helper()
	sub := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)
	ns := filepath.Join(sub, "namespaces", "1")
	if err := os.MkdirAll(ns, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		filepath.Join(sub, "attr_allow_any_host"): "1\n",
		filepath.Join(sub, "attr_serial"):         legacyLiveIdentity.Serial + "    \n",
		filepath.Join(ns, "device_path"):          testDevicePath + "\n",
		filepath.Join(ns, "device_uuid"):          legacyLiveIdentity.UUID + "\n",
		filepath.Join(ns, "device_nguid"):         legacyLiveIdentity.NGUID + "\n",
		filepath.Join(ns, "enable"):               "1\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// TestNVMeIdentity_LegacyExportKeptAcrossReboot: after upgrading the agent
// while a 0.2.0 export is live, reconcile keeps the live identity (connected
// hosts cached it) and reproduces it after the next storage-node reboot.
func TestNVMeIdentity_LegacyExportKeptAcrossReboot(t *testing.T) {
	t.Parallel()
	cfgRoot, stateDir := t.TempDir(), t.TempDir()
	writeLegacyExport(t, cfgRoot)
	srv := newIdentityTestServer(t, cfgRoot, stateDir)

	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))
	if got := readExportedIdentity(t, cfgRoot); got != legacyLiveIdentity {
		t.Fatalf("live identity changed by reconcile: %+v, want %+v", got, legacyLiveIdentity)
	}

	simulateReboot(t, cfgRoot)
	// A restarted agent process on the rebooted node.
	srv = newIdentityTestServer(t, cfgRoot, stateDir)
	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))
	if got := readExportedIdentity(t, cfgRoot); got != legacyLiveIdentity {
		t.Fatalf("re-exported identity after reboot %+v, want the legacy %+v", got, legacyLiveIdentity)
	}
}

// TestNVMeIdentity_UnexportDropsKeptIdentity: once the export is removed no
// host holds the legacy identity, so the next export uses the derived one.
func TestNVMeIdentity_UnexportDropsKeptIdentity(t *testing.T) {
	t.Parallel()
	cfgRoot := t.TempDir()
	writeLegacyExport(t, cfgRoot)
	srv := newIdentityTestServer(t, cfgRoot, t.TempDir())
	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId: testVolumeID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Fence: testFence(t),
	})
	if err != nil {
		t.Fatalf("UnexportVolume: %v", err)
	}
	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))
	if got := readExportedIdentity(t, cfgRoot); got != nvmeof.DeriveIdentity(testVolumeNQN, 1) {
		t.Fatalf("identity after unexport and re-export %+v, want the derived identity", got)
	}
}

// TestNVMeIdentity_CorruptRecordFailsReconcile: a kept identity that cannot be
// read must fail the re-export rather than guess, because a guessed identity
// would drop the namespace from connected hosts.
func TestNVMeIdentity_CorruptRecordFailsReconcile(t *testing.T) {
	t.Parallel()
	cfgRoot, stateDir := t.TempDir(), t.TempDir()
	writeLegacyExport(t, cfgRoot)
	srv := newIdentityTestServer(t, cfgRoot, stateDir)
	reconcileOne(t, srv, testDevicePath, nvmeofExportState("10.0.0.1"))

	records, err := filepath.Glob(filepath.Join(stateDir, "nvmet-identity", "*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("identity records = %v (err %v), want exactly one", records, err)
	}
	if err = os.WriteFile(records[0], []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}
	simulateReboot(t, cfgRoot)

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId: testVolumeID, DevicePath: testDevicePath,
			Exports: []*agentv1.ExportDesiredState{nvmeofExportState("10.0.0.1")},
			Fence:   testFence(t),
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}
	if len(resp.GetResults()) != 1 || resp.GetResults()[0].GetSuccess() {
		t.Fatalf("reconcile with a corrupt identity record succeeded: %v", resp.GetResults())
	}
	if _, statErr := os.Stat(filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN)); !os.IsNotExist(statErr) {
		t.Errorf("subsystem created despite unreadable identity (stat err %v)", statErr)
	}
}
