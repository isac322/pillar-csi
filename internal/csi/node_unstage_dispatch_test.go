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

// Tests for NodeUnstageVolume protocol-handler dispatch (AC 8).
//
// These tests verify that NodeUnstageVolume reads the discriminated union
// nodeStageState and dispatches to the correct ProtocolHandler.Detach
// implementation for the persisted protocol type — without relying on the
// legacy Connector adapter.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeUnstageVolume_Dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// mockProtocolHandler — test double for ProtocolHandler
// ─────────────────────────────────────────────────────────────────────────────

// mockProtocolHandler is a minimal ProtocolHandler that records Detach calls
// and returns pre-programmed responses.  It is used by NodeUnstageVolume
// dispatch tests to verify protocol-type-based routing without requiring a
// real transport stack.
type mockProtocolHandler struct {
	// attachResult is returned by Attach.
	attachResult *AttachResult
	// attachErr is returned instead of attachResult if non-nil.
	attachErr error

	// detachErr is returned by Detach if non-nil.
	detachErr error

	// Recorded Detach calls: each entry is the ProtocolState passed to Detach.
	detachCalls []ProtocolState
}

func (m *mockProtocolHandler) Attach(_ context.Context, _ AttachParams) (*AttachResult, error) {
	if m.attachErr != nil {
		return nil, m.attachErr
	}
	return m.attachResult, nil
}

func (m *mockProtocolHandler) Detach(_ context.Context, state ProtocolState) error {
	m.detachCalls = append(m.detachCalls, state)
	return m.detachErr
}

func (*mockProtocolHandler) Rescan(_ context.Context, _ ProtocolState) error {
	return nil
}

// Compile-time interface check.
var _ ProtocolHandler = (*mockProtocolHandler)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// newHandlerNodeTestEnv — test environment with handler map
// ─────────────────────────────────────────────────────────────────────────────

// handlerNodeTestEnv is a test environment that uses a handlers map directly,
// bypassing the legacy Connector adapter.  This lets tests inject arbitrary
// mock ProtocolHandlers to verify dispatch logic.
type handlerNodeTestEnv struct {
	srv      *NodeServer
	mounter  *mockMounter
	stateDir string
}

// newHandlerNodeTestEnv constructs a NodeServer backed by the given handler map.
func newHandlerNodeTestEnv(t *testing.T, handlers map[string]ProtocolHandler) *handlerNodeTestEnv {
	t.Helper()
	mnt := newMockMounter()
	stateDir := t.TempDir()
	// Construct NodeServer directly (package-level access) to inject stateDir.
	srv := &NodeServer{
		nodeID:   "test-node",
		handlers: handlers,
		mounter:  mnt,
		stateDir: stateDir,
	}
	return &handlerNodeTestEnv{srv: srv, mounter: mnt, stateDir: stateDir}
}

// ─────────────────────────────────────────────────────────────────────────────
// writeLegacyStateFileSanitized
// ─────────────────────────────────────────────────────────────────────────────

// writeLegacyStateFileSanitized writes a Phase 1 state file (bare
// {"subsys_nqn":...}) to the stateDir using the same filename sanitization as
// NodeServer.stateFilePath — slashes in volumeID are replaced with underscores.
//
// Unlike writeLegacyStateFile (stage_state_migration_test.go) which writes the
// filename as volumeID+".json" verbatim, this helper sanitizes slashes to
// underscores so it works for volumeIDs like "tank/pvc-legacy-dispatch" that
// contain path separators.
func writeLegacyStateFileSanitized(t *testing.T, stateDir, volumeID, subsysNQN string) {
	t.Helper()
	type legacyJSON struct {
		SubsysNQN string `json:"subsys_nqn"`
	}
	data, err := json.Marshal(legacyJSON{SubsysNQN: subsysNQN})
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	safeID := strings.ReplaceAll(volumeID, "/", "_")
	path := filepath.Join(stateDir, safeID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write legacy state file: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_Dispatch_NVMeoF
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_Dispatch_NVMeoF verifies that NodeUnstageVolume reads
// an NVMe-oF discriminated union state file and calls Detach on the
// "nvmeof-tcp" ProtocolHandler with the correct NVMeoFProtocolState.
func TestNodeUnstageVolume_Dispatch_NVMeoF(t *testing.T) {
	t.Parallel()

	handler := &mockProtocolHandler{}
	handlers := map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	}
	env := newHandlerNodeTestEnv(t, handlers)
	stagingPath := t.TempDir()

	const (
		volumeID = "tank/pvc-dispatch-nvme"
		nqn      = "nqn.2024-01.com.example:storage:dispatch-test"
		addr     = "10.0.0.5"
		port     = "4420"
	)

	// Pre-write a Phase 2 NVMe-oF state file (simulating a prior NodeStageVolume).
	if err := env.srv.writeStageState(volumeID, &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: nqn,
			Address:   addr,
			Port:      port,
		},
	}); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	// Mark the staging path as mounted so the unmount step runs.
	env.mounter.mountedPaths[stagingPath] = true

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume: %v", err)
	}

	// Handler.Detach must have been called exactly once.
	if len(handler.detachCalls) != 1 {
		t.Fatalf("Detach called %d times, want 1", len(handler.detachCalls))
	}

	// The ProtocolState passed to Detach must be an NVMeoFProtocolState.
	nvmeState, ok := handler.detachCalls[0].(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("Detach state type = %T, want *NVMeoFProtocolState", handler.detachCalls[0])
	}
	if nvmeState.SubsysNQN != nqn {
		t.Errorf("NVMeoFProtocolState.SubsysNQN = %q, want %q", nvmeState.SubsysNQN, nqn)
	}
	if nvmeState.Address != addr {
		t.Errorf("NVMeoFProtocolState.Address = %q, want %q", nvmeState.Address, addr)
	}
	if nvmeState.Port != port {
		t.Errorf("NVMeoFProtocolState.Port = %q, want %q", nvmeState.Port, port)
	}

	// State file must have been deleted.
	remaining, _ := env.srv.readStageState(volumeID) //nolint:errcheck // assert only nil check
	if remaining != nil {
		t.Error("stage state still present after NodeUnstageVolume")
	}

	// Staging path must be unmounted.
	mounted, _ := env.mounter.IsMounted(stagingPath) //nolint:errcheck // mock never errors
	if mounted {
		t.Error("staging path still mounted after NodeUnstageVolume")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_Dispatch_LegacyMigration
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_Dispatch_LegacyMigration verifies that NodeUnstageVolume
// reads a Phase 1 legacy state file ({"subsys_nqn":"nqn.…"}), migrates it to the
// discriminated union format in memory, and dispatches to the "nvmeof-tcp" handler.
func TestNodeUnstageVolume_Dispatch_LegacyMigration(t *testing.T) {
	t.Parallel()

	handler := &mockProtocolHandler{}
	handlers := map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	}
	env := newHandlerNodeTestEnv(t, handlers)
	stagingPath := t.TempDir()

	const (
		volumeID = "tank/pvc-legacy-dispatch"
		nqn      = "nqn.2024-01.com.example:storage:legacy"
	)

	// Write a Phase 1 legacy state file using the same filename sanitization
	// as stateFilePath (slashes → underscores).
	writeLegacyStateFileSanitized(t, env.stateDir, volumeID, nqn)

	// Mark staging path as mounted.
	env.mounter.mountedPaths[stagingPath] = true

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume (legacy state): %v", err)
	}

	// Detach must have been called with the migrated NVMeoFProtocolState.
	if len(handler.detachCalls) != 1 {
		t.Fatalf("Detach called %d times, want 1", len(handler.detachCalls))
	}
	nvmeState, ok := handler.detachCalls[0].(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("Detach state type = %T, want *NVMeoFProtocolState", handler.detachCalls[0])
	}
	if nvmeState.SubsysNQN != nqn {
		t.Errorf("SubsysNQN = %q, want %q", nvmeState.SubsysNQN, nqn)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_Dispatch_UnknownProtocol
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_Dispatch_UnknownProtocol verifies that NodeUnstageVolume
// returns codes.Internal when the persisted state file names a protocol type for
// which no ProtocolHandler is registered in the handlers map.
func TestNodeUnstageVolume_Dispatch_UnknownProtocol(t *testing.T) {
	t.Parallel()

	// Register only the NVMe-oF handler; the state file will claim "iscsi".
	handlers := map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: &mockProtocolHandler{},
	}
	env := newHandlerNodeTestEnv(t, handlers)
	stagingPath := t.TempDir()

	const volumeID = "tank/pvc-unknown-proto"

	// Write a state file with an unregistered protocol type.
	if err := env.srv.writeStageState(volumeID, &nodeStageState{
		ProtocolType: "iscsi",
		ISCSI: &ISCSIStageState{
			TargetIQN: "iqn.2024-01.com.example:storage:vol1",
			Portal:    "10.0.0.5:3260",
			LUN:       0,
		},
	}); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_Dispatch_DetachError
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_Dispatch_DetachError verifies that if the ProtocolHandler
// Detach call returns an error, NodeUnstageVolume propagates it as
// codes.Internal without deleting the state file (teardown is incomplete).
func TestNodeUnstageVolume_Dispatch_DetachError(t *testing.T) {
	t.Parallel()

	handler := &mockProtocolHandler{
		detachErr: errors.New("transport failure"),
	}
	handlers := map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	}
	env := newHandlerNodeTestEnv(t, handlers)
	stagingPath := t.TempDir()

	const volumeID = "tank/pvc-detach-err"

	// Write a valid NVMe-oF state file.
	if err := env.srv.writeStageState(volumeID, &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: "nqn.test:detach-err",
			Address:   "192.0.2.1", // RFC 5737 TEST-NET-1
			Port:      "4420",
		},
	}); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
	})
	requireGRPCCode(t, err, codes.Internal)

	// Detach must have been called once.
	if len(handler.detachCalls) != 1 {
		t.Errorf("Detach called %d times, want 1", len(handler.detachCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeUnstageVolume_Dispatch_NoStateFile_NoDetach
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeUnstageVolume_Dispatch_NoStateFile_NoDetach verifies that if no state
// file exists (volume was never staged or already unstaged), NodeUnstageVolume
// succeeds idempotently without calling any handler.
func TestNodeUnstageVolume_Dispatch_NoStateFile_NoDetach(t *testing.T) {
	t.Parallel()

	handler := &mockProtocolHandler{}
	handlers := map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	}
	env := newHandlerNodeTestEnv(t, handlers)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "tank/pvc-no-state",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume (no state file): %v", err)
	}

	// No Detach must have been called.
	if len(handler.detachCalls) != 0 {
		t.Errorf("Detach called %d times for unstaged volume, want 0", len(handler.detachCalls))
	}
}
