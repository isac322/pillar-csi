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

// Package csi — node_stage_state_test.go: pure unit tests for the nodeStageState
// discriminated union data structures and state migration helpers.
//
// These tests exercise the types defined in stage_state.go using only
// encoding/json and the functions in the same package.  They do NOT require a
// NodeServer, a real filesystem, or kernel NVMe modules, so they run fast and
// in parallel.
//
// Coverage goals:
//  1. JSON serialization: exact field names and nesting for every protocol.
//  2. JSON deserialization: parse from raw JSON strings.
//  3. Discriminated union isolation: only the relevant sub-struct is present.
//  4. Zero-value and empty-struct behavior.
//  5. isLegacyFormat, migrateFromLegacy edge cases.
//  6. ToProtocolState for all branches (NVMeoF, nil, unknown).
//  7. stageStateFromAttachResult invariants for all protocol arms.

import (
	"encoding/json"
	"strings"
	"testing"
)

// Protocol type string constants used in tests to avoid goconst lint warnings.
const (
	testProtocolISCSI = "iscsi"
	testProtocolNFS   = "nfs"
	testProtocolSMB   = "smb"
)

// ─────────────────────────────────────────────────────────────────────────────
// JSON field-name / nesting format tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageState_JSON_NVMeoF verifies that the serialized JSON for an
// NVMeoF state uses the expected keys ("protocol_type", "nvmeof",
// "subsys_nqn", "address", "port") and does NOT include keys for other
// protocol sub-structs.
func TestNodeStageState_JSON_NVMeoF(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: "nqn.2024-01.com.example:vol1",
			Address:   "192.168.1.10",
			Port:      "4420",
		},
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	raw := string(data)

	// Required keys.
	for _, want := range []string{`"protocol_type"`, `"nvmeof"`, `"subsys_nqn"`, `"address"`, `"port"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("JSON missing key %s: %s", want, raw)
		}
	}
	// Must NOT include other protocol sub-struct keys.
	for _, absent := range []string{`"iscsi"`, `"nfs"`, `"smb"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("JSON should not contain %s for NVMeoF state: %s", absent, raw)
		}
	}
}

// TestNodeStageState_JSON_ISCSI verifies that the serialized JSON for an iSCSI
// state contains the expected "iscsi" object with the correct field names.
func TestNodeStageState_JSON_ISCSI(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolISCSI,
		ISCSI: &ISCSIStageState{
			TargetIQN: "iqn.2024-01.com.example:vol1",
			Portal:    "192.168.1.30:3260",
			LUN:       5,
		},
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	raw := string(data)

	for _, want := range []string{`"iscsi"`, `"target_iqn"`, `"portal"`, `"lun"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("JSON missing key %s: %s", want, raw)
		}
	}
	for _, absent := range []string{`"nvmeof"`, `"nfs"`, `"smb"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("JSON should not contain %s for iSCSI state: %s", absent, raw)
		}
	}
}

// TestNodeStageState_JSON_NFS verifies that the serialized JSON for an NFS
// state contains the expected "nfs" object with "server" and "export_path".
func TestNodeStageState_JSON_NFS(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolNFS,
		NFS: &NFSStageState{
			Server:     "10.0.0.5",
			ExportPath: "/mnt/data/pvc-abc",
		},
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	raw := string(data)

	for _, want := range []string{`"nfs"`, `"server"`, `"export_path"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("JSON missing key %s: %s", want, raw)
		}
	}
	for _, absent := range []string{`"nvmeof"`, `"iscsi"`, `"smb"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("JSON should not contain %s for NFS state: %s", absent, raw)
		}
	}
}

// TestNodeStageState_JSON_SMB verifies the serialized JSON for an SMB state.
func TestNodeStageState_JSON_SMB(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolSMB,
		SMB: &SMBStageState{
			Server: "10.0.0.6",
			Share:  "pvc-smb-01",
		},
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	raw := string(data)

	for _, want := range []string{`"smb"`, `"server"`, `"share"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("JSON missing key %s: %s", want, raw)
		}
	}
	for _, absent := range []string{`"nvmeof"`, `"iscsi"`, `"nfs"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("JSON should not contain %s for SMB state: %s", absent, raw)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON deserialization from raw strings
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageState_Unmarshal_NVMeoF verifies that a raw NVMeoF Phase 2 JSON
// string is correctly deserialized into a nodeStageState.
func TestNodeStageState_Unmarshal_NVMeoF(t *testing.T) {
	t.Parallel()

	// Split across two lines to satisfy the 120-character line limit.
	raw := `{"protocol_type":"nvmeof-tcp",` +
		`"nvmeof":{"subsys_nqn":"nqn.2024-01.com.example:vol1","address":"192.168.1.10","port":"4420"}}`

	var s nodeStageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if s.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q; want %q", s.ProtocolType, ProtocolNVMeoFTCP)
	}
	if s.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	if s.NVMeoF.SubsysNQN != "nqn.2024-01.com.example:vol1" {
		t.Errorf("SubsysNQN = %q", s.NVMeoF.SubsysNQN)
	}
	if s.NVMeoF.Address != "192.168.1.10" {
		t.Errorf("Address = %q", s.NVMeoF.Address)
	}
	if s.NVMeoF.Port != "4420" {
		t.Errorf("Port = %q", s.NVMeoF.Port)
	}
	// Other protocol sub-structs must be nil.
	if s.ISCSI != nil || s.NFS != nil || s.SMB != nil {
		t.Errorf("unexpected non-nil sub-structs: ISCSI=%v NFS=%v SMB=%v", s.ISCSI, s.NFS, s.SMB)
	}
}

// TestNodeStageState_Unmarshal_ISCSI verifies that an iSCSI Phase 2 JSON
// string is correctly deserialized.
func TestNodeStageState_Unmarshal_ISCSI(t *testing.T) {
	t.Parallel()

	// Split across two lines to satisfy the 120-character line limit.
	raw := `{"protocol_type":"iscsi",` +
		`"iscsi":{"target_iqn":"iqn.2024-01.com.example:vol2","portal":"10.0.0.3:3260","lun":7}}`

	var s nodeStageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if s.ProtocolType != testProtocolISCSI {
		t.Errorf("ProtocolType = %q; want %q", s.ProtocolType, testProtocolISCSI)
	}
	if s.ISCSI == nil {
		t.Fatal("ISCSI sub-struct is nil")
	}
	if s.ISCSI.TargetIQN != "iqn.2024-01.com.example:vol2" {
		t.Errorf("TargetIQN = %q", s.ISCSI.TargetIQN)
	}
	if s.ISCSI.Portal != "10.0.0.3:3260" {
		t.Errorf("Portal = %q", s.ISCSI.Portal)
	}
	if s.ISCSI.LUN != 7 {
		t.Errorf("LUN = %d; want 7", s.ISCSI.LUN)
	}
	if s.NVMeoF != nil || s.NFS != nil || s.SMB != nil {
		t.Errorf("unexpected non-nil sub-structs: NVMeoF=%v NFS=%v SMB=%v", s.NVMeoF, s.NFS, s.SMB)
	}
}

// TestNodeStageState_Unmarshal_NFS verifies that an NFS Phase 2 JSON is parsed.
func TestNodeStageState_Unmarshal_NFS(t *testing.T) {
	t.Parallel()

	const raw = `{"protocol_type":"nfs","nfs":{"server":"nfs-server.local","export_path":"/exports/pvc-abc"}}`

	var s nodeStageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if s.ProtocolType != testProtocolNFS {
		t.Errorf("ProtocolType = %q", s.ProtocolType)
	}
	if s.NFS == nil {
		t.Fatal("NFS sub-struct is nil")
	}
	if s.NFS.Server != "nfs-server.local" {
		t.Errorf("Server = %q", s.NFS.Server)
	}
	if s.NFS.ExportPath != "/exports/pvc-abc" {
		t.Errorf("ExportPath = %q", s.NFS.ExportPath)
	}
}

// TestNodeStageState_Unmarshal_SMB verifies that an SMB Phase 2 JSON is parsed.
func TestNodeStageState_Unmarshal_SMB(t *testing.T) {
	t.Parallel()

	const raw = `{"protocol_type":"smb","smb":{"server":"smb-server.local","share":"pvc-smb-01"}}`

	var s nodeStageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if s.ProtocolType != testProtocolSMB {
		t.Errorf("ProtocolType = %q", s.ProtocolType)
	}
	if s.SMB == nil {
		t.Fatal("SMB sub-struct is nil")
	}
	if s.SMB.Server != "smb-server.local" {
		t.Errorf("Server = %q", s.SMB.Server)
	}
	if s.SMB.Share != "pvc-smb-01" {
		t.Errorf("Share = %q", s.SMB.Share)
	}
}

// TestNodeStageState_Unmarshal_UnknownFieldsIgnored verifies that extra JSON
// fields added by future versions are silently ignored (forward compatibility).
func TestNodeStageState_Unmarshal_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()

	// Split to stay within 120-character line limit.
	raw := `{"protocol_type":"nvmeof-tcp",` +
		`"nvmeof":{"subsys_nqn":"nqn.x:vol","address":"1.2.3.4","port":"4420"},"future_field":"ignored"}`

	var s nodeStageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("json.Unmarshal should ignore unknown fields: %v", err)
	}
	if s.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q", s.ProtocolType)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Discriminated union isolation invariants
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageState_DiscriminatedUnion_OnlyNVMeoFSet verifies that when the
// NVMeoF sub-struct is set, the others remain nil after a JSON roundtrip.
func TestNodeStageState_DiscriminatedUnion_OnlyNVMeoFSet(t *testing.T) {
	t.Parallel()

	orig := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF:       &NVMeoFStageState{SubsysNQN: "nqn.x:vol", Address: "1.2.3.4", Port: "4420"},
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got nodeStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.NVMeoF == nil {
		t.Fatal("NVMeoF should be non-nil")
	}
	if got.ISCSI != nil {
		t.Errorf("ISCSI should be nil; got %+v", got.ISCSI)
	}
	if got.NFS != nil {
		t.Errorf("NFS should be nil; got %+v", got.NFS)
	}
	if got.SMB != nil {
		t.Errorf("SMB should be nil; got %+v", got.SMB)
	}
}

// TestNodeStageState_DiscriminatedUnion_OnlyISCSISet verifies iSCSI isolation.
func TestNodeStageState_DiscriminatedUnion_OnlyISCSISet(t *testing.T) {
	t.Parallel()

	orig := &nodeStageState{
		ProtocolType: testProtocolISCSI,
		ISCSI:        &ISCSIStageState{TargetIQN: "iqn.x:vol", Portal: "1.2.3.4:3260", LUN: 1},
	}

	data, _ := json.Marshal(orig) //nolint:errcheck // test-only
	var got nodeStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.ISCSI == nil {
		t.Fatal("ISCSI should be non-nil")
	}
	if got.NVMeoF != nil {
		t.Errorf("NVMeoF should be nil; got %+v", got.NVMeoF)
	}
	if got.NFS != nil {
		t.Errorf("NFS should be nil; got %+v", got.NFS)
	}
	if got.SMB != nil {
		t.Errorf("SMB should be nil; got %+v", got.SMB)
	}
}

// TestNodeStageState_DiscriminatedUnion_OnlyNFSSet verifies NFS isolation.
func TestNodeStageState_DiscriminatedUnion_OnlyNFSSet(t *testing.T) {
	t.Parallel()

	orig := &nodeStageState{
		ProtocolType: testProtocolNFS,
		NFS:          &NFSStageState{Server: "10.0.0.1", ExportPath: "/data/pvc"},
	}

	data, _ := json.Marshal(orig) //nolint:errcheck
	var got nodeStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.NFS == nil {
		t.Fatal("NFS should be non-nil")
	}
	if got.NVMeoF != nil || got.ISCSI != nil || got.SMB != nil {
		t.Errorf("unexpected non-nil sub-structs: NVMeoF=%v ISCSI=%v SMB=%v", got.NVMeoF, got.ISCSI, got.SMB)
	}
}

// TestNodeStageState_DiscriminatedUnion_OnlySMBSet verifies SMB isolation.
func TestNodeStageState_DiscriminatedUnion_OnlySMBSet(t *testing.T) {
	t.Parallel()

	orig := &nodeStageState{
		ProtocolType: testProtocolSMB,
		SMB:          &SMBStageState{Server: "10.0.0.2", Share: "pvc-smb"},
	}

	data, _ := json.Marshal(orig) //nolint:errcheck
	var got nodeStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.SMB == nil {
		t.Fatal("SMB should be non-nil")
	}
	if got.NVMeoF != nil || got.ISCSI != nil || got.NFS != nil {
		t.Errorf("unexpected non-nil sub-structs: NVMeoF=%v ISCSI=%v NFS=%v", got.NVMeoF, got.ISCSI, got.NFS)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Zero-value and empty-struct behavior
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageState_ZeroValue verifies that a zero-value nodeStageState
// marshals and unmarshals without error, and has no sub-structs.
func TestNodeStageState_ZeroValue(t *testing.T) {
	t.Parallel()

	var s nodeStageState
	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("json.Marshal zero value: %v", err)
	}

	var got nodeStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal zero value: %v", err)
	}

	if got.ProtocolType != "" {
		t.Errorf("ProtocolType = %q; want empty", got.ProtocolType)
	}
	if got.NVMeoF != nil || got.ISCSI != nil || got.NFS != nil || got.SMB != nil {
		t.Error("all sub-structs should be nil for zero value")
	}
}

// TestNVMeoFStageState_AllFieldsRoundtrip verifies that all three fields of
// NVMeoFStageState survive a JSON marshal/unmarshal roundtrip.
func TestNVMeoFStageState_AllFieldsRoundtrip(t *testing.T) {
	t.Parallel()

	const (
		subsysNQN = "nqn.2025-03.com.example:storage-pool:pvc-1a2b3c"
		address   = "fd00::1"
		port      = "4420"
	)

	orig := NVMeoFStageState{SubsysNQN: subsysNQN, Address: address, Port: port}
	data, err := json.Marshal(&orig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got NVMeoFStageState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.SubsysNQN != subsysNQN {
		t.Errorf("SubsysNQN = %q; want %q", got.SubsysNQN, subsysNQN)
	}
	if got.Address != address {
		t.Errorf("Address = %q; want %q", got.Address, address)
	}
	if got.Port != port {
		t.Errorf("Port = %q; want %q", got.Port, port)
	}
}

// TestISCSIStageState_LUNIntField verifies that the LUN integer field of
// ISCSIStageState is correctly marshaled and unmarshaled, including
// the boundary case where LUN == 0 (which is a valid LUN, not a zero-value
// omission).
func TestISCSIStageState_LUNIntField(t *testing.T) {
	t.Parallel()

	for _, lun := range []int{0, 1, 255, 1023} {
		t.Run("lun", func(t *testing.T) {
			t.Parallel()
			orig := ISCSIStageState{TargetIQN: "iqn.x:vol", Portal: "1.2.3.4:3260", LUN: lun}
			data, err := json.Marshal(&orig)
			if err != nil {
				t.Fatalf("json.Marshal LUN=%d: %v", lun, err)
			}
			var got ISCSIStageState
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal LUN=%d: %v", lun, err)
			}
			if got.LUN != lun {
				t.Errorf("LUN roundtrip: got %d; want %d", got.LUN, lun)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// isLegacyFormat — edge cases not covered by stage_state_migration_test.go
// ─────────────────────────────────────────────────────────────────────────────

// TestIsLegacyFormat_WhitespaceOnlySubsysNQN verifies that a state with only
// whitespace in SubsysNQN is treated as having a non-empty NQN and is considered
// legacy (the check is a non-empty string comparison, not a trimmed check).
// This documents the current behavior explicitly.
func TestIsLegacyFormat_WhitespaceOnlySubsysNQN(t *testing.T) {
	t.Parallel()

	raw := &legacyNodeStageState{SubsysNQN: "   ", ProtocolType: ""}
	// A whitespace-only NQN is technically non-empty, so isLegacyFormat returns true.
	// The migrated state will have an odd NQN but that is a data-quality issue,
	// not a correctness issue with the migration logic.
	if !isLegacyFormat(raw) {
		t.Error("expected isLegacyFormat to return true for whitespace-only SubsysNQN (non-empty string)")
	}
}

// TestIsLegacyFormat_BothFieldsEmpty verifies the boundary where both SubsysNQN
// and ProtocolType are empty — this is NOT a legacy file (it looks like a newly
// created empty struct).
func TestIsLegacyFormat_BothFieldsEmpty(t *testing.T) {
	t.Parallel()

	raw := &legacyNodeStageState{}
	if isLegacyFormat(raw) {
		t.Error("expected false for empty legacyNodeStageState (no NQN, no ProtocolType)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// migrateFromLegacy — comprehensive field-level checks
// ─────────────────────────────────────────────────────────────────────────────

// TestMigrateFromLegacy_AddressAndPortLeftEmpty verifies that Address and Port
// are empty strings after migration (they are not present in Phase 1 files).
func TestMigrateFromLegacy_AddressAndPortLeftEmpty(t *testing.T) {
	t.Parallel()

	raw := &legacyNodeStageState{SubsysNQN: "nqn.x:vol"}
	got := migrateFromLegacy(raw)

	if got.NVMeoF == nil {
		t.Fatal("NVMeoF is nil")
	}
	if got.NVMeoF.Address != "" {
		t.Errorf("Address = %q; want empty (not available in Phase 1)", got.NVMeoF.Address)
	}
	if got.NVMeoF.Port != "" {
		t.Errorf("Port = %q; want empty (not available in Phase 1)", got.NVMeoF.Port)
	}
}

// TestMigrateFromLegacy_ResultSerializesCorrectly verifies that the output of
// migrateFromLegacy can be serialized to a valid Phase 2 JSON.
func TestMigrateFromLegacy_ResultSerializesCorrectly(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.2024-01.com.example:pvc-migrate"
	raw := &legacyNodeStageState{SubsysNQN: nqn}
	migrated := migrateFromLegacy(raw)

	data, err := json.Marshal(migrated)
	if err != nil {
		t.Fatalf("json.Marshal migrated state: %v", err)
	}
	jsonStr := string(data)

	if !strings.Contains(jsonStr, `"protocol_type":"nvmeof-tcp"`) {
		t.Errorf("migrated JSON does not contain protocol_type: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"nvmeof"`) {
		t.Errorf("migrated JSON does not contain nvmeof key: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, nqn) {
		t.Errorf("migrated JSON does not contain original NQN: %s", jsonStr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ToProtocolState — all branches
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageState_ToProtocolState_ISCSIReturnsNil verifies that ToProtocolState
// returns nil for an iSCSI state (handler not yet implemented — documented stub
// behavior per stage_state.go comment).
func TestNodeStageState_ToProtocolState_ISCSIReturnsNil(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolISCSI,
		ISCSI: &ISCSIStageState{
			TargetIQN: "iqn.x:vol",
			Portal:    "1.2.3.4:3260",
			LUN:       0,
		},
	}

	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error for unimplemented iSCSI handler")
	}
}

// TestNodeStageState_ToProtocolState_NFSReturnsError verifies that ToProtocolState
// returns an error for an NFS state (not yet implemented).
func TestNodeStageState_ToProtocolState_NFSReturnsError(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolNFS,
		NFS:          &NFSStageState{Server: "1.2.3.4", ExportPath: "/export"},
	}

	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error for unimplemented NFS handler")
	}
}

// TestNodeStageState_ToProtocolState_SMBReturnsError verifies that ToProtocolState
// returns an error for an SMB state (not yet implemented).
func TestNodeStageState_ToProtocolState_SMBReturnsError(t *testing.T) {
	t.Parallel()

	s := &nodeStageState{
		ProtocolType: testProtocolSMB,
		SMB:          &SMBStageState{Server: "1.2.3.4", Share: "share"},
	}

	if _, err := s.ToProtocolState(); err == nil {
		t.Error("expected error for unimplemented SMB handler")
	}
}

// TestNodeStageState_ToProtocolState_NVMeoFFieldsPreserved verifies that all
// three fields of NVMeoFStageState survive the conversion to NVMeoFProtocolState.
func TestNodeStageState_ToProtocolState_NVMeoFFieldsPreserved(t *testing.T) {
	t.Parallel()

	const (
		subsysNQN = "nqn.2025-03.com.example:vol-proto"
		address   = "fd00::cafe"
		port      = "4420"
	)

	s := &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF:       &NVMeoFStageState{SubsysNQN: subsysNQN, Address: address, Port: port},
	}

	ps, err := s.ToProtocolState()
	if err != nil {
		t.Fatalf("ToProtocolState error: %v", err)
	}

	nvme, ok := ps.(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("got %T; want *NVMeoFProtocolState", ps)
	}

	if nvme.SubsysNQN != subsysNQN {
		t.Errorf("SubsysNQN = %q; want %q", nvme.SubsysNQN, subsysNQN)
	}
	if nvme.Address != address {
		t.Errorf("Address = %q; want %q", nvme.Address, address)
	}
	if nvme.Port != port {
		t.Errorf("Port = %q; want %q", nvme.Port, port)
	}
	// ProtocolType() must match the package constant.
	if nvme.ProtocolType() != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType() = %q; want %q", nvme.ProtocolType(), ProtocolNVMeoFTCP)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// stageStateFromAttachResult — additional invariant tests
// ─────────────────────────────────────────────────────────────────────────────

// TestStageStateFromAttachResult_ISCSIProtocol verifies that stageStateFromAttachResult
// returns only ProtocolType="iscsi" with no typed sub-struct (future implementation).
func TestStageStateFromAttachResult_ISCSIProtocol(t *testing.T) {
	t.Parallel()

	s := stageStateFromAttachResult(testProtocolISCSI, "iqn.x:vol", "1.2.3.4:3260", "", nil)
	if s == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if s.ProtocolType != testProtocolISCSI {
		t.Errorf("ProtocolType = %q; want %q", s.ProtocolType, testProtocolISCSI)
	}
	// No typed sub-struct until iSCSI handler is implemented.
	if s.NVMeoF != nil {
		t.Error("NVMeoF should be nil for iSCSI protocol")
	}
	if s.ISCSI != nil {
		t.Error("ISCSI sub-struct should be nil (not yet populated by stageStateFromAttachResult)")
	}
}

// TestStageStateFromAttachResult_NFSProtocol verifies that stageStateFromAttachResult
// returns only ProtocolType="nfs" with no typed sub-struct.
func TestStageStateFromAttachResult_NFSProtocol(t *testing.T) {
	t.Parallel()

	s := stageStateFromAttachResult(testProtocolNFS, "10.0.0.1:/export", "", "", nil)
	if s == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if s.ProtocolType != testProtocolNFS {
		t.Errorf("ProtocolType = %q; want %q", s.ProtocolType, testProtocolNFS)
	}
	if s.NVMeoF != nil || s.NFS != nil || s.ISCSI != nil || s.SMB != nil {
		t.Error("all sub-structs should be nil for NFS protocol (not yet populated)")
	}
}

// TestStageStateFromAttachResult_SMBProtocol verifies that stageStateFromAttachResult
// returns only ProtocolType="smb" with no typed sub-struct.
func TestStageStateFromAttachResult_SMBProtocol(t *testing.T) {
	t.Parallel()

	s := stageStateFromAttachResult(testProtocolSMB, "smb-srv/share", "", "", nil)
	if s == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if s.ProtocolType != testProtocolSMB {
		t.Errorf("ProtocolType = %q; want %q", s.ProtocolType, testProtocolSMB)
	}
	if s.NVMeoF != nil || s.NFS != nil || s.ISCSI != nil || s.SMB != nil {
		t.Error("all sub-structs should be nil for SMB protocol (not yet populated)")
	}
}

// TestStageStateFromAttachResult_NVMeoFEmptyAttachResultState verifies that
// when AttachResult.State is nil (e.g. from a minimal fake handler), the function
// falls back to VolumeContext fields.
func TestStageStateFromAttachResult_NVMeoFEmptyAttachResultState(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.x:vol-fallback"
	const addr = "172.16.0.1"
	const port = "4420"

	result := &AttachResult{
		DevicePath: "/dev/nvme0n1",
		State:      nil, // no state in AttachResult
	}

	s := stageStateFromAttachResult(ProtocolNVMeoFTCP, nqn, addr, port, result)
	if s == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if s.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	if s.NVMeoF.SubsysNQN != nqn {
		t.Errorf("SubsysNQN = %q; want %q (fallback from VolumeContext)", s.NVMeoF.SubsysNQN, nqn)
	}
	if s.NVMeoF.Address != addr {
		t.Errorf("Address = %q; want %q (fallback from VolumeContext)", s.NVMeoF.Address, addr)
	}
	if s.NVMeoF.Port != port {
		t.Errorf("Port = %q; want %q (fallback from VolumeContext)", s.NVMeoF.Port, port)
	}
}

// TestStageStateFromAttachResult_NVMeoFResultStateTakesPrecedence verifies that
// when AttachResult.State is a non-nil *NVMeoFProtocolState with different values
// than the VolumeContext fields, the AttachResult.State values win.
func TestStageStateFromAttachResult_NVMeoFResultStateTakesPrecedence(t *testing.T) {
	t.Parallel()

	const resultNQN = "nqn.x:vol-from-result"
	const resultAddr = "10.100.0.1"
	const resultPort = "4421"

	result := &AttachResult{
		DevicePath: "/dev/nvme0n1",
		State: &NVMeoFProtocolState{
			SubsysNQN: resultNQN,
			Address:   resultAddr,
			Port:      resultPort,
		},
	}

	s := stageStateFromAttachResult(ProtocolNVMeoFTCP,
		"nqn.x:vol-from-volctx", "volctx-addr", "volctx-port",
		result,
	)
	if s == nil || s.NVMeoF == nil {
		t.Fatal("state or NVMeoF sub-struct is nil")
	}

	if s.NVMeoF.SubsysNQN != resultNQN {
		t.Errorf("SubsysNQN = %q; want %q (from AttachResult.State)", s.NVMeoF.SubsysNQN, resultNQN)
	}
	if s.NVMeoF.Address != resultAddr {
		t.Errorf("Address = %q; want %q (from AttachResult.State)", s.NVMeoF.Address, resultAddr)
	}
	if s.NVMeoF.Port != resultPort {
		t.Errorf("Port = %q; want %q (from AttachResult.State)", s.NVMeoF.Port, resultPort)
	}
}

// TestStageStateFromAttachResult_NVMeoFEmptyProtocolType verifies that passing
// an empty protocol type produces a state with empty ProtocolType and no
// NVMeoF sub-struct (the function must not assume NVMeoF when type is "").
func TestStageStateFromAttachResult_NVMeoFEmptyProtocolType(t *testing.T) {
	t.Parallel()

	s := stageStateFromAttachResult("", "nqn.x:vol", "1.2.3.4", "4420", nil)
	if s == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if s.ProtocolType != "" {
		t.Errorf("ProtocolType = %q; want empty", s.ProtocolType)
	}
	if s.NVMeoF != nil {
		t.Errorf("NVMeoF should be nil for empty protocol type; got %+v", s.NVMeoF)
	}
}
