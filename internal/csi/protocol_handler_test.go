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

// Unit tests for the ProtocolHandler interface, AttachParams, AttachResult,
// ProtocolState types, stageStateFromAttachResult, and the mock/fake handler
// pattern.
//
// These tests verify the interface contract independently of any specific
// protocol implementation so that future protocol handlers (iSCSI, NFS, SMB)
// can be validated against the same set of expectations.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestProtocolHandler

import (
	"context"
	"errors"
	"testing"
)

// testNQN is the NVMe subsystem NQN used across all tests in this file.
const testNQN = "nqn.2024-01.com.example:vol1"

// testDevPath is the NVMe block device path used across all tests in this file.
const testDevPath = "/dev/nvme0n1"

// ─────────────────────────────────────────────────────────────────────────────
// Compile-time interface checks
// ─────────────────────────────────────────────────────────────────────────────

// TestProtocolHandler_InterfaceCompileCheck verifies that fakeProtocolHandler
// (defined in this file) satisfies the ProtocolHandler interface.
// The actual production compile-time check for NVMeoFTCPHandler lives in
// nvmeof_tcp_handler.go.
func TestProtocolHandler_InterfaceCompileCheck(t *testing.T) {
	t.Helper()
	var _ ProtocolHandler = (*fakeProtocolHandler)(nil)
}

// TestProtocolState_InterfaceCompileCheck verifies that fakeProtocolState
// satisfies the ProtocolState interface.
func TestProtocolState_InterfaceCompileCheck(t *testing.T) {
	t.Helper()
	var _ ProtocolState = (*fakeProtocolState)(nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// Fake ProtocolState
// ─────────────────────────────────────────────────────────────────────────────

// fakeProtocolState is a minimal ProtocolState for testing interface callers.
type fakeProtocolState struct {
	protocol string
}

// ProtocolType satisfies the ProtocolState interface.
func (f *fakeProtocolState) ProtocolType() string { return f.protocol }

// ─────────────────────────────────────────────────────────────────────────────
// Fake ProtocolHandler
// ─────────────────────────────────────────────────────────────────────────────

// fakeProtocolHandler is a test double for ProtocolHandler.
// It records calls and returns pre-programmed responses.
type fakeProtocolHandler struct {
	protocol string // "block" or "file" for driving the response path

	// AttachResult to return (nil triggers attachErr).
	attachResult *AttachResult
	attachErr    error

	// Errors to return for Detach/Rescan.
	detachErr error
	rescanErr error

	// Recorded calls.
	attachCalls []AttachParams
	detachCalls []ProtocolState
	rescanCalls []ProtocolState
}

func (f *fakeProtocolHandler) Attach(_ context.Context, p AttachParams) (*AttachResult, error) {
	f.attachCalls = append(f.attachCalls, p)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return f.attachResult, nil
}

func (f *fakeProtocolHandler) Detach(_ context.Context, s ProtocolState) error {
	f.detachCalls = append(f.detachCalls, s)
	return f.detachErr
}

func (f *fakeProtocolHandler) Rescan(_ context.Context, s ProtocolState) error {
	f.rescanCalls = append(f.rescanCalls, s)
	return f.rescanErr
}

// ─────────────────────────────────────────────────────────────────────────────
// AttachParams field tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAttachParams_ZeroValue verifies that a zero-value AttachParams is valid
// to construct (all fields are strings or maps with zero values).
func TestAttachParams_ZeroValue(t *testing.T) {
	t.Parallel()
	var p AttachParams
	if p.ProtocolType != "" || p.ConnectionID != "" || p.Address != "" || p.Port != "" {
		t.Error("zero-value AttachParams should have empty string fields")
	}
	if p.Extra != nil {
		t.Error("zero-value AttachParams.Extra should be nil")
	}
}

// TestAttachParams_Fields verifies that all documented fields are accessible
// and writable.
func TestAttachParams_Fields(t *testing.T) {
	t.Parallel()
	p := AttachParams{
		ProtocolType: "nvmeof-tcp",
		ConnectionID: testNQN,
		Address:      "192.168.1.10",
		Port:         "4420",
		VolumeRef:    "1",
		Extra:        map[string]string{"key": "value"},
	}
	if p.ProtocolType != "nvmeof-tcp" {
		t.Errorf("ProtocolType = %q", p.ProtocolType)
	}
	if p.ConnectionID != testNQN {
		t.Errorf("ConnectionID = %q", p.ConnectionID)
	}
	if p.Address != "192.168.1.10" {
		t.Errorf("Address = %q", p.Address)
	}
	if p.Port != "4420" {
		t.Errorf("Port = %q", p.Port)
	}
	if p.VolumeRef != "1" {
		t.Errorf("VolumeRef = %q", p.VolumeRef)
	}
	if p.Extra["key"] != "value" {
		t.Errorf("Extra[\"key\"] = %q", p.Extra["key"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// AttachResult invariant tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAttachResult_BlockProtocol verifies the AttachResult invariant for block
// protocols: DevicePath is set and MountSource is empty.
func TestAttachResult_BlockProtocol(t *testing.T) {
	t.Parallel()
	result := &AttachResult{
		DevicePath: testDevPath,
		State:      &fakeProtocolState{protocol: "nvmeof-tcp"},
	}
	if result.DevicePath == "" {
		t.Error("block protocol AttachResult must have non-empty DevicePath")
	}
	if result.MountSource != "" {
		t.Errorf("block protocol AttachResult must have empty MountSource, got %q", result.MountSource)
	}
}

// TestAttachResult_FileProtocol verifies the AttachResult invariant for file
// protocols: MountSource is set and DevicePath is empty.
func TestAttachResult_FileProtocol(t *testing.T) {
	t.Parallel()
	result := &AttachResult{
		MountSource: "192.168.1.10:/export/pvc-abc123",
		State:       &fakeProtocolState{protocol: "nfs"},
	}
	if result.MountSource == "" {
		t.Error("file protocol AttachResult must have non-empty MountSource")
	}
	if result.DevicePath != "" {
		t.Errorf("file protocol AttachResult must have empty DevicePath, got %q", result.DevicePath)
	}
}

// TestAttachResult_StateRoundTrip verifies that the State field in AttachResult
// preserves the ProtocolType through a type assertion.
func TestAttachResult_StateRoundTrip(t *testing.T) {
	t.Parallel()
	const proto = "nvmeof-tcp"
	result := &AttachResult{
		DevicePath: testDevPath,
		State:      &fakeProtocolState{protocol: proto},
	}
	if result.State == nil {
		t.Fatal("AttachResult.State must not be nil")
	}
	if result.State.ProtocolType() != proto {
		t.Errorf("State.ProtocolType() = %q; want %q", result.State.ProtocolType(), proto)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolState interface tests
// ─────────────────────────────────────────────────────────────────────────────

// TestProtocolState_ProtocolTypeMethod verifies that a concrete implementation
// of ProtocolState returns the expected protocol identifier.
func TestProtocolState_ProtocolTypeMethod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		protocol string
	}{
		{"nvmeof-tcp"},
		{"iscsi"},
		{"nfs"},
		{"smb"},
		{""},
	}
	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			t.Parallel()
			s := &fakeProtocolState{protocol: tt.protocol}
			if got := s.ProtocolType(); got != tt.protocol {
				t.Errorf("ProtocolType() = %q; want %q", got, tt.protocol)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fakeProtocolHandler behavior tests
// ─────────────────────────────────────────────────────────────────────────────

// TestFakeProtocolHandler_Attach_Success verifies that fakeProtocolHandler
// returns the programmed AttachResult and records the call.
func TestFakeProtocolHandler_Attach_Success(t *testing.T) {
	t.Parallel()
	expected := &AttachResult{
		DevicePath: "/dev/sda",
		State:      &fakeProtocolState{protocol: "nvmeof-tcp"},
	}
	h := &fakeProtocolHandler{attachResult: expected}
	params := AttachParams{
		ProtocolType: "nvmeof-tcp",
		ConnectionID: testNQN,
		Address:      "192.168.1.10",
		Port:         "4420",
	}

	result, err := h.Attach(context.Background(), params)
	if err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if result != expected {
		t.Fatalf("Attach returned unexpected result: %+v", result)
	}
	if len(h.attachCalls) != 1 {
		t.Fatalf("expected 1 Attach call, got %d", len(h.attachCalls))
	}
	if h.attachCalls[0].ConnectionID != testNQN {
		t.Errorf("recorded ConnectionID = %q", h.attachCalls[0].ConnectionID)
	}
}

// TestFakeProtocolHandler_Attach_Error verifies that fakeProtocolHandler
// propagates the programmed error.
func TestFakeProtocolHandler_Attach_Error(t *testing.T) {
	t.Parallel()
	sentinelErr := errors.New("attach failed")
	h := &fakeProtocolHandler{attachErr: sentinelErr}

	_, err := h.Attach(context.Background(), AttachParams{})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr, got %v", err)
	}
}

// TestFakeProtocolHandler_Detach_Success verifies that Detach records the call
// and returns nil.
func TestFakeProtocolHandler_Detach_Success(t *testing.T) {
	t.Parallel()
	h := &fakeProtocolHandler{}
	state := &fakeProtocolState{protocol: "nvmeof-tcp"}

	if err := h.Detach(context.Background(), state); err != nil {
		t.Fatalf("Detach returned error: %v", err)
	}
	if len(h.detachCalls) != 1 {
		t.Fatalf("expected 1 Detach call, got %d", len(h.detachCalls))
	}
	if h.detachCalls[0] != state {
		t.Error("Detach was called with unexpected state")
	}
}

// TestFakeProtocolHandler_Detach_Error verifies that Detach propagates the
// programmed error.
func TestFakeProtocolHandler_Detach_Error(t *testing.T) {
	t.Parallel()
	sentinelErr := errors.New("detach failed")
	h := &fakeProtocolHandler{detachErr: sentinelErr}

	err := h.Detach(context.Background(), &fakeProtocolState{})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr, got %v", err)
	}
}

// TestFakeProtocolHandler_Rescan_Success verifies that Rescan records the call
// and returns nil.
func TestFakeProtocolHandler_Rescan_Success(t *testing.T) {
	t.Parallel()
	h := &fakeProtocolHandler{}
	state := &fakeProtocolState{protocol: "nvmeof-tcp"}

	if err := h.Rescan(context.Background(), state); err != nil {
		t.Fatalf("Rescan returned error: %v", err)
	}
	if len(h.rescanCalls) != 1 {
		t.Fatalf("expected 1 Rescan call, got %d", len(h.rescanCalls))
	}
}

// TestFakeProtocolHandler_Rescan_Error verifies that Rescan propagates the
// programmed error.
func TestFakeProtocolHandler_Rescan_Error(t *testing.T) {
	t.Parallel()
	sentinelErr := errors.New("rescan failed")
	h := &fakeProtocolHandler{rescanErr: sentinelErr}

	err := h.Rescan(context.Background(), &fakeProtocolState{})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinelErr, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// stageStateFromAttachResult tests
// ─────────────────────────────────────────────────────────────────────────────

// TestStageStateFromAttachResult_NVMeoF_UsesProtocolStateFields verifies that
// when the AttachResult carries a *NVMeoFProtocolState, stageStateFromAttachResult
// prefers those values over the explicitly passed targetID/address/port.
func TestStageStateFromAttachResult_NVMeoF_UsesProtocolStateFields(t *testing.T) {
	t.Parallel()
	const addr = "192.168.1.10"
	const port = "4420"

	result := &AttachResult{
		DevicePath: testDevPath,
		State: &NVMeoFProtocolState{
			SubsysNQN: testNQN,
			Address:   addr,
			Port:      port,
		},
	}

	state := stageStateFromAttachResult(ProtocolNVMeoFTCP, "other-nqn", "other-addr", "other-port", result)
	if state == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if state.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q; want %q", state.ProtocolType, ProtocolNVMeoFTCP)
	}
	if state.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	// Preferred from NVMeoFProtocolState (not the passed "other-*" values).
	if state.NVMeoF.SubsysNQN != testNQN {
		t.Errorf("SubsysNQN = %q; want %q", state.NVMeoF.SubsysNQN, testNQN)
	}
	if state.NVMeoF.Address != addr {
		t.Errorf("Address = %q; want %q", state.NVMeoF.Address, addr)
	}
	if state.NVMeoF.Port != port {
		t.Errorf("Port = %q; want %q", state.NVMeoF.Port, port)
	}
}

// TestStageStateFromAttachResult_NVMeoF_FallsBackToVolumeContextFields verifies
// that when the AttachResult does NOT carry a *NVMeoFProtocolState (e.g. the
// state is a fakeProtocolState), the function falls back to the explicitly
// passed targetID/address/port values.
func TestStageStateFromAttachResult_NVMeoF_FallsBackToVolumeContextFields(t *testing.T) {
	t.Parallel()
	const targetID = "nqn.2024-01.com.example:vol2"
	const address = "10.0.0.5"
	const port = "4421"

	// AttachResult with a non-NVMeoF state (forces fallback path).
	result := &AttachResult{
		DevicePath: "/dev/nvme1n1",
		State:      &fakeProtocolState{protocol: "nvmeof-tcp"},
	}

	state := stageStateFromAttachResult(ProtocolNVMeoFTCP, targetID, address, port, result)
	if state == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if state.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil")
	}
	if state.NVMeoF.SubsysNQN != targetID {
		t.Errorf("SubsysNQN = %q; want %q", state.NVMeoF.SubsysNQN, targetID)
	}
	if state.NVMeoF.Address != address {
		t.Errorf("Address = %q; want %q", state.NVMeoF.Address, address)
	}
	if state.NVMeoF.Port != port {
		t.Errorf("Port = %q; want %q", state.NVMeoF.Port, port)
	}
}

// TestStageStateFromAttachResult_NVMeoF_NilAttachResult verifies that
// stageStateFromAttachResult handles a nil AttachResult gracefully.
func TestStageStateFromAttachResult_NVMeoF_NilAttachResult(t *testing.T) {
	t.Parallel()
	state := stageStateFromAttachResult(ProtocolNVMeoFTCP, "nqn.x:vol", "1.2.3.4", "4420", nil)
	if state == nil {
		t.Fatal("stageStateFromAttachResult returned nil for nil AttachResult")
	}
	if state.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("ProtocolType = %q; want %q", state.ProtocolType, ProtocolNVMeoFTCP)
	}
	if state.NVMeoF == nil {
		t.Fatal("NVMeoF sub-struct is nil for nil AttachResult")
	}
	if state.NVMeoF.SubsysNQN != "nqn.x:vol" {
		t.Errorf("SubsysNQN = %q; want %q", state.NVMeoF.SubsysNQN, "nqn.x:vol")
	}
}

// TestStageStateFromAttachResult_OtherProtocol_OnlyProtocolTypeSet verifies
// that for protocol types other than "nvmeof-tcp", only the ProtocolType field
// is set and no typed sub-struct is populated.
func TestStageStateFromAttachResult_OtherProtocol_OnlyProtocolTypeSet(t *testing.T) {
	t.Parallel()
	state := stageStateFromAttachResult("nfs", "server:/export", "", "", nil)
	if state == nil {
		t.Fatal("stageStateFromAttachResult returned nil")
	}
	if state.ProtocolType != "nfs" {
		t.Errorf("ProtocolType = %q; want \"nfs\"", state.ProtocolType)
	}
	if state.NVMeoF != nil {
		t.Error("NVMeoF sub-struct should be nil for non-NVMe-oF protocol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Handler map pattern tests
// ─────────────────────────────────────────────────────────────────────────────

// TestHandlerMap_LookupByProtocolType verifies that a map[string]ProtocolHandler
// correctly dispatches to the expected handler based on the protocol type string.
func TestHandlerMap_LookupByProtocolType(t *testing.T) {
	t.Parallel()
	nvmeHandler := &fakeProtocolHandler{protocol: "nvmeof-tcp"}
	nfsHandler := &fakeProtocolHandler{protocol: "nfs"}

	handlers := map[string]ProtocolHandler{
		"nvmeof-tcp": nvmeHandler,
		"nfs":        nfsHandler,
	}

	for proto, expected := range handlers {
		got, ok := handlers[proto]
		if !ok {
			t.Errorf("handler for %q not found in map", proto)
			continue
		}
		if got != expected {
			t.Errorf("handler for %q: got %p, want %p", proto, got, expected)
		}
	}
}

// TestHandlerMap_MissingProtocol verifies that looking up a protocol that has
// no registered handler returns nil (ok == false), which callers must handle.
func TestHandlerMap_MissingProtocol(t *testing.T) {
	t.Parallel()
	handlers := map[string]ProtocolHandler{
		"nvmeof-tcp": &fakeProtocolHandler{},
	}

	_, ok := handlers["iscsi"] // not registered
	if ok {
		t.Error("expected handler lookup to fail for unregistered protocol")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Attach→Detach lifecycle using fakeProtocolHandler
// ─────────────────────────────────────────────────────────────────────────────

// TestProtocolHandler_AttachDetachLifecycle_Block verifies the full block
// protocol lifecycle using a fake handler: Attach returns a DevicePath, and
// the State from the result is passed to Detach.
func TestProtocolHandler_AttachDetachLifecycle_Block(t *testing.T) {
	t.Parallel()
	state := &fakeProtocolState{protocol: "nvmeof-tcp"}
	h := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: testDevPath,
			State:      state,
		},
	}

	// Attach
	result, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: "nvmeof-tcp",
		ConnectionID: testNQN,
		Address:      "192.168.1.10",
		Port:         "4420",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if result.DevicePath != testDevPath {
		t.Errorf("DevicePath = %q; want /dev/nvme0n1", result.DevicePath)
	}
	if result.MountSource != "" {
		t.Errorf("MountSource = %q; want empty for block protocol", result.MountSource)
	}

	// Detach using the State returned by Attach.
	if err := h.Detach(context.Background(), result.State); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if len(h.detachCalls) != 1 || h.detachCalls[0] != state {
		t.Error("Detach was not called with the State returned by Attach")
	}
}

// TestProtocolHandler_AttachDetachLifecycle_File verifies the full file protocol
// lifecycle: Attach returns a MountSource, DevicePath is empty.
func TestProtocolHandler_AttachDetachLifecycle_File(t *testing.T) {
	t.Parallel()
	state := &fakeProtocolState{protocol: "nfs"}
	h := &fakeProtocolHandler{
		attachResult: &AttachResult{
			MountSource: "192.168.1.20:/export/pvc-abc123",
			State:       state,
		},
	}

	result, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: "nfs",
		ConnectionID: "192.168.1.20",
		VolumeRef:    "/export/pvc-abc123",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if result.DevicePath != "" {
		t.Errorf("DevicePath = %q; want empty for file protocol", result.DevicePath)
	}
	if result.MountSource != "192.168.1.20:/export/pvc-abc123" {
		t.Errorf("MountSource = %q", result.MountSource)
	}

	// Detach (e.g. NFS unmount — no-op at ProtocolHandler level).
	if err := h.Detach(context.Background(), result.State); err != nil {
		t.Fatalf("Detach: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Rescan after expansion — protocol-level contract
// ─────────────────────────────────────────────────────────────────────────────

// TestProtocolHandler_Rescan_BlockAfterExpansion verifies that Rescan is called
// with the ProtocolState from a previous Attach (simulating online resize).
func TestProtocolHandler_Rescan_BlockAfterExpansion(t *testing.T) {
	t.Parallel()
	state := &fakeProtocolState{protocol: "nvmeof-tcp"}
	h := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: testDevPath,
			State:      state,
		},
	}

	// Simulate: Attach, then ControllerExpandVolume (external), then Rescan.
	result, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: "nvmeof-tcp",
		ConnectionID: testNQN,
		Address:      "10.0.0.1",
		Port:         "4420",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if err := h.Rescan(context.Background(), result.State); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if len(h.rescanCalls) != 1 {
		t.Errorf("expected 1 Rescan call, got %d", len(h.rescanCalls))
	}
	if h.rescanCalls[0] != state {
		t.Error("Rescan was not called with the State returned by Attach")
	}
}

// TestProtocolHandler_Rescan_FileProtocol_IsNoOp verifies that file protocol
// handlers report Rescan as a no-op (returns nil) because server-side resize
// is transparent to the client mount.
func TestProtocolHandler_Rescan_FileProtocol_IsNoOp(t *testing.T) {
	t.Parallel()
	// fakeProtocolHandler.rescanErr defaults to nil — represents a no-op Rescan.
	h := &fakeProtocolHandler{protocol: "nfs"}

	state := &fakeProtocolState{protocol: "nfs"}
	if err := h.Rescan(context.Background(), state); err != nil {
		t.Fatalf("Rescan for file protocol must be a no-op (nil), got: %v", err)
	}
}
