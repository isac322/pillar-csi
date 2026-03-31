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

package csi

// Package csi — stage_state_migration_test.go: tests for legacy nodeStageState migration.
//
// These tests verify that readStageState detects the old Phase 1 format
// ({"subsys_nqn":"nqn.…"}) and converts it in-place to the Phase 2
// discriminated union format (RFC §5.5.2).
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestLegacyStageState

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// isLegacyFormat unit tests
// ─────────────────────────────────────────────────────────────────────────────

func TestIsLegacyFormat_OldFormatDetected(t *testing.T) {
	t.Parallel()
	raw := &legacyNodeStageState{SubsysNQN: "nqn.2024-01.com.example:vol1"}
	if !isLegacyFormat(raw) {
		t.Error("expected isLegacyFormat to return true for Phase 1 state (no protocol_type, has subsys_nqn)")
	}
}

func TestIsLegacyFormat_NewFormatNotLegacy(t *testing.T) {
	t.Parallel()
	raw := &legacyNodeStageState{
		SubsysNQN:    "nqn.2024-01.com.example:vol1",
		ProtocolType: protocolNVMeoFTCP,
	}
	if isLegacyFormat(raw) {
		t.Error("expected isLegacyFormat to return false when protocol_type is present")
	}
}

func TestIsLegacyFormat_EmptySubsysNQN(t *testing.T) {
	t.Parallel()
	raw := &legacyNodeStageState{SubsysNQN: ""}
	if isLegacyFormat(raw) {
		t.Error("expected isLegacyFormat to return false when subsys_nqn is empty")
	}
}

func TestIsLegacyFormat_Nil(t *testing.T) {
	t.Parallel()
	if isLegacyFormat(nil) {
		t.Error("expected isLegacyFormat to return false for nil input")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// migrateFromLegacy unit tests
// ─────────────────────────────────────────────────────────────────────────────

func TestMigrateFromLegacy_SetsProtocolType(t *testing.T) {
	t.Parallel()
	raw := &legacyNodeStageState{SubsysNQN: "nqn.2024-01.com.example:vol1"}
	got := migrateFromLegacy(raw)
	if got.ProtocolType != protocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q; want %q", got.ProtocolType, protocolNVMeoFTCP)
	}
}

func TestMigrateFromLegacy_PopulatesNVMeoFSubsysNQN(t *testing.T) {
	t.Parallel()
	const nqn = "nqn.2024-01.com.example:storage:vol42"
	raw := &legacyNodeStageState{SubsysNQN: nqn}
	got := migrateFromLegacy(raw)
	if got.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil after migration")
	}
	if got.NVMeoF.SubsysNQN != nqn {
		t.Errorf("NVMeoF.SubsysNQN = %q; want %q", got.NVMeoF.SubsysNQN, nqn)
	}
}

func TestMigrateFromLegacy_OtherProtocolSubStructsNil(t *testing.T) {
	t.Parallel()
	raw := &legacyNodeStageState{SubsysNQN: "nqn.2024-01.com.example:vol1"}
	got := migrateFromLegacy(raw)
	if got.ISCSI != nil {
		t.Error("ISCSI sub-struct should be nil after NVMeoF migration")
	}
	if got.NFS != nil {
		t.Error("NFS sub-struct should be nil after NVMeoF migration")
	}
	if got.SMB != nil {
		t.Error("SMB sub-struct should be nil after NVMeoF migration")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// readStageState integration tests — legacy migration
// ─────────────────────────────────────────────────────────────────────────────

// writeLegacyStateFile writes a Phase 1 state file (bare {"subsys_nqn":...})
// to the given directory for the given volumeID.
func writeLegacyStateFile(t *testing.T, stateDir, volumeID, subsysNQN string) {
	t.Helper()
	type legacyJSON struct {
		SubsysNQN string `json:"subsys_nqn"`
	}
	data, err := json.Marshal(legacyJSON{SubsysNQN: subsysNQN})
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	path := filepath.Join(stateDir, volumeID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy state file: %v", err)
	}
}

// TestReadStageState_LegacyFormatMigratedInMemory verifies that readStageState
// returns a properly migrated in-memory state when it reads a Phase 1 file.
func TestReadStageState_LegacyFormatMigratedInMemory(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	const volumeID = "vol-legacy-001"
	const nqn = "nqn.2024-01.com.example:storage:vol-legacy-001"

	writeLegacyStateFile(t, stateDir, volumeID, nqn)

	srv := NewNodeServerWithStateDir("test-node", &mockConnector{}, newMockMounter(), stateDir)
	state, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState returned error: %v", err)
	}
	if state == nil {
		t.Fatal("readStageState returned nil state for legacy file")
	}
	if state.ProtocolType != protocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q; want %q", state.ProtocolType, protocolNVMeoFTCP)
	}
	if state.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil after migration")
	}
	if state.NVMeoF.SubsysNQN != nqn {
		t.Errorf("NVMeoF.SubsysNQN = %q; want %q", state.NVMeoF.SubsysNQN, nqn)
	}
}

// TestReadStageState_LegacyFormatMigratedOnDisk verifies that readStageState
// rewrites the state file to the Phase 2 format during migration, so subsequent
// reads no longer trigger migration.
func TestReadStageState_LegacyFormatMigratedOnDisk(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	const volumeID = "vol-legacy-002"
	const nqn = "nqn.2024-01.com.example:storage:vol-legacy-002"

	writeLegacyStateFile(t, stateDir, volumeID, nqn)

	srv := NewNodeServerWithStateDir("test-node", &mockConnector{}, newMockMounter(), stateDir)

	// First read: triggers migration.
	if _, err := srv.readStageState(volumeID); err != nil {
		t.Fatalf("first readStageState: %v", err)
	}

	// Verify the file on disk is now in Phase 2 format.
	stateFilePath := filepath.Join(stateDir, volumeID+".json")
	rawData, err := os.ReadFile(stateFilePath) //nolint:gosec // G304: test-only path under t.TempDir()
	if err != nil {
		t.Fatalf("read state file after migration: %v", err)
	}

	var onDisk nodeStageState
	if err := json.Unmarshal(rawData, &onDisk); err != nil {
		t.Fatalf("unmarshal migrated state file: %v", err)
	}
	if onDisk.ProtocolType != protocolNVMeoFTCP {
		t.Errorf("on-disk ProtocolType = %q; want %q", onDisk.ProtocolType, protocolNVMeoFTCP)
	}
	if onDisk.NVMeoF == nil || onDisk.NVMeoF.SubsysNQN != nqn {
		t.Errorf("on-disk NVMeoF.SubsysNQN = %q; want %q",
			func() string {
				if onDisk.NVMeoF == nil {
					return "<nil>"
				}
				return onDisk.NVMeoF.SubsysNQN
			}(), nqn)
	}

	// Second read: should use new format without migration.
	state2, err2 := srv.readStageState(volumeID)
	if err2 != nil {
		t.Fatalf("second readStageState after migration: %v", err2)
	}
	if state2 == nil || state2.ProtocolType != protocolNVMeoFTCP {
		t.Error("second read after migration should use new format directly")
	}
}

// TestReadStageState_NewFormatUnchanged verifies that readStageState does not
// modify a state file that is already in the Phase 2 discriminated union format.
func TestReadStageState_NewFormatUnchanged(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	const volumeID = "vol-new-001"
	const nqn = "nqn.2024-01.com.example:storage:vol-new-001"
	const addr = "192.168.1.42"
	const port = "4420"

	// Write a Phase 2 state file.
	newState := &nodeStageState{
		ProtocolType: protocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: nqn,
			Address:   addr,
			Port:      port,
		},
	}
	srv := NewNodeServerWithStateDir("test-node", &mockConnector{}, newMockMounter(), stateDir)
	if err := srv.writeStageState(volumeID, newState); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	// Capture the file's modification time before reading.
	stateFilePath := filepath.Join(stateDir, volumeID+".json")
	fi1, err := os.Stat(stateFilePath)
	if err != nil {
		t.Fatalf("stat before read: %v", err)
	}

	state, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if state == nil || state.ProtocolType != protocolNVMeoFTCP {
		t.Errorf("expected nvmeof-tcp state, got %+v", state)
	}
	if state.NVMeoF == nil || state.NVMeoF.Address != addr || state.NVMeoF.Port != port {
		t.Errorf("Address/Port not preserved: got %+v", state.NVMeoF)
	}

	// The file should NOT have been rewritten (no migration occurred).
	fi2, err := os.Stat(stateFilePath)
	if err != nil {
		t.Fatalf("stat after read: %v", err)
	}
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Error("readStageState should not rewrite a Phase 2 state file")
	}
}

// TestReadStageState_NoFileReturnsNil verifies that readStageState returns
// (nil, nil) when no state file exists.
func TestReadStageState_NoFileReturnsNil(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	srv := NewNodeServerWithStateDir("test-node", &mockConnector{}, newMockMounter(), stateDir)
	state, err := srv.readStageState("vol-nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if state != nil {
		t.Errorf("expected nil state, got %+v", state)
	}
}

// TestReadStageState_LegacyMigratedToProtocolState verifies that the migrated
// state can be converted to a ProtocolState suitable for ProtocolHandler.Detach.
func TestReadStageState_LegacyMigratedToProtocolState(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	const volumeID = "vol-legacy-proto"
	const nqn = "nqn.2024-01.com.example:storage:proto-test"

	writeLegacyStateFile(t, stateDir, volumeID, nqn)

	srv := NewNodeServerWithStateDir("test-node", &mockConnector{}, newMockMounter(), stateDir)
	state, err := srv.readStageState(volumeID)
	if err != nil {
		t.Fatalf("readStageState: %v", err)
	}
	if state == nil {
		t.Fatal("readStageState returned nil")
	}

	protoState, protoErr := state.ToProtocolState()
	if protoErr != nil {
		t.Fatalf("ToProtocolState error: %v", protoErr)
	}

	nvmeState, ok := protoState.(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("ToProtocolState returned %T; want *NVMeoFProtocolState", protoState)
	}
	if nvmeState.SubsysNQN != nqn {
		t.Errorf("NVMeoFProtocolState.SubsysNQN = %q; want %q", nvmeState.SubsysNQN, nqn)
	}
	if nvmeState.ProtocolType() != protocolNVMeoFTCP {
		t.Errorf("ProtocolType() = %q; want %q", nvmeState.ProtocolType(), "nvmeof-tcp")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ToProtocolState unit tests
// ─────────────────────────────────────────────────────────────────────────────

func TestToProtocolState_NVMeoF(t *testing.T) {
	t.Parallel()
	s := &nodeStageState{
		ProtocolType: protocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: "nqn.2024-01.com.example:vol1",
			Address:   "10.0.0.1",
			Port:      "4420",
		},
	}
	ps, psErr := s.ToProtocolState()
	if psErr != nil {
		t.Fatalf("ToProtocolState error: %v", psErr)
	}
	nvme, ok := ps.(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("got %T; want *NVMeoFProtocolState", ps)
	}
	if nvme.SubsysNQN != "nqn.2024-01.com.example:vol1" {
		t.Errorf("SubsysNQN = %q", nvme.SubsysNQN)
	}
	if nvme.Address != "10.0.0.1" {
		t.Errorf("Address = %q", nvme.Address)
	}
	if nvme.Port != "4420" {
		t.Errorf("Port = %q", nvme.Port)
	}
}

func TestToProtocolState_NVMeoF_NilSubStruct(t *testing.T) {
	t.Parallel()
	s := &nodeStageState{ProtocolType: protocolNVMeoFTCP, NVMeoF: nil}
	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error when NVMeoF sub-struct is nil")
	}
}

func TestToProtocolState_Nil(t *testing.T) {
	t.Parallel()
	var s *nodeStageState
	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error for nil receiver")
	}
}

func TestToProtocolState_UnknownProtocol(t *testing.T) {
	t.Parallel()
	s := &nodeStageState{ProtocolType: "fc"} // fiber channel — not implemented
	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error for unknown protocol")
	}
}
