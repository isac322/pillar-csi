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

// Unit tests for NVMeoFTCPHandler — Attach, Detach, Rescan.
//
// All tests use a temporary directory as a fake sysfs root and a temporary
// file as a fake /dev/nvme-fabrics device, so no kernel NVMe modules or root
// privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNVMeoFTCPHandler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTestHandler constructs an NVMeoFTCPHandler with fake sysfs and fabricsDev
// paths using newNVMeoFTCPHandlerWithConnector.  Poll timeout is set to 100 ms
// and poll interval to 10 ms to keep unit tests fast without hanging.
func newTestHandler(sysfsRoot, fabricsDev string) *NVMeoFTCPHandler {
	h := newNVMeoFTCPHandlerWithConnector(&NVMeoFConnector{sysfsRoot: sysfsRoot, fabricsDev: fabricsDev})
	h.pollTimeout = 100 * time.Millisecond
	h.pollInterval = 10 * time.Millisecond
	return h
}

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolHandler compile-time check
// ─────────────────────────────────────────────────────────────────────────────

// TestNVMeoFTCPHandler_ImplementsProtocolHandler verifies that NVMeoFTCPHandler
// satisfies the ProtocolHandler interface.  This test exists purely to surface
// the compile-time assertion in documentation; the actual compile-time check is
// in nvmeof_tcp_handler.go.
func TestNVMeoFTCPHandler_ImplementsProtocolHandler(t *testing.T) {
	t.Helper()
	var _ ProtocolHandler = (*NVMeoFTCPHandler)(nil)
}

// TestNVMeoFProtocolState_ImplementsProtocolState verifies that
// NVMeoFProtocolState satisfies the ProtocolState interface.
func TestNVMeoFProtocolState_ImplementsProtocolState(t *testing.T) {
	t.Helper()
	var _ ProtocolState = (*NVMeoFProtocolState)(nil)
}

// TestNVMeoFProtocolState_ProtocolType verifies that ProtocolType returns
// the expected "nvmeof-tcp" string.
func TestNVMeoFProtocolState_ProtocolType(t *testing.T) {
	s := &NVMeoFProtocolState{SubsysNQN: "nqn.2024-01.com.example:vol1"}
	if got := s.ProtocolType(); got != protocolNVMeoFTCP {
		t.Errorf("expected %q, got %q", protocolNVMeoFTCP, got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Attach tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNVMeoFTCPHandler_Attach_Success verifies that Attach:
//  1. Writes the correct connect string to the fabricsDev.
//  2. Polls GetDevicePath and returns DevicePath when the device appears.
//  3. Populates AttachResult.State with the correct NVMeoFProtocolState fields.
func TestNVMeoFTCPHandler_Attach_Success(t *testing.T) {
	const (
		nqn  = "nqn.2024-01.com.example:vol1"
		addr = "192.168.1.10"
		port = "4420"
	)

	// Prepare sysfs with subsystem + namespace entries (device already visible).
	// NQN already exists in sysfs so Connect is a no-op (idempotent).
	sysfsRoot := fakeSysfs(t, nqn, true /* addNamespace */)
	fabricsDev := fakeFabricsDev(t)

	h := newTestHandler(sysfsRoot, fabricsDev)

	result, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		ConnectionID: nqn,
		Address:      addr,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	if result == nil {
		t.Fatal("Attach returned nil result")
	}
	if result.DevicePath != "/dev/nvme0n1" {
		t.Errorf("expected DevicePath=/dev/nvme0n1, got %q", result.DevicePath)
	}
	if result.MountSource != "" {
		t.Errorf("expected empty MountSource for block protocol, got %q", result.MountSource)
	}

	// Verify State is correctly populated.
	nvmeState, ok := result.State.(*NVMeoFProtocolState)
	if !ok {
		t.Fatalf("expected *NVMeoFProtocolState, got %T", result.State)
	}
	if nvmeState.SubsysNQN != nqn {
		t.Errorf("SubsysNQN: want %q, got %q", nqn, nvmeState.SubsysNQN)
	}
	if nvmeState.Address != addr {
		t.Errorf("Address: want %q, got %q", addr, nvmeState.Address)
	}
	if nvmeState.Port != port {
		t.Errorf("Port: want %q, got %q", port, nvmeState.Port)
	}

	// Verify fabricsDev was NOT written (NQN was already connected in sysfs).
	content, readErr := os.ReadFile(fabricsDev) //nolint:gosec
	if readErr != nil {
		t.Fatalf("read fabricsDev: %v", readErr)
	}
	if len(content) != 0 {
		t.Errorf("fabricsDev should be empty (idempotent connect), got %q", string(content))
	}
}

// TestNVMeoFTCPHandler_Attach_NewConnect verifies that Attach writes the
// correct connect string when the subsystem is not yet connected.
func TestNVMeoFTCPHandler_Attach_NewConnect(t *testing.T) {
	const (
		nqn  = "nqn.2024-01.com.example:vol1"
		addr = "192.168.1.10"
		port = "4420"
	)

	// Use an empty sysfs — Connect will write to fabricsDev (NQN not yet connected).
	// We can't poll GetDevicePath successfully in empty sysfs, so we allow the
	// handler to time out after a very short window and verify the write happened.
	emptyRoot := t.TempDir()
	fabricsDev := fakeFabricsDev(t)
	if err := os.MkdirAll(filepath.Join(emptyRoot, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}

	h := &NVMeoFTCPHandler{
		connector:    &NVMeoFConnector{sysfsRoot: emptyRoot, fabricsDev: fabricsDev},
		pollTimeout:  20 * time.Millisecond, // short so test doesn't hang
		pollInterval: 5 * time.Millisecond,
	}

	_, attachErr := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		ConnectionID: nqn,
		Address:      addr,
		Port:         port,
	})
	// We expect a timeout error since the device never appears in empty sysfs.
	if attachErr == nil {
		t.Fatal("expected error (device never appears), got nil")
	}
	if !strings.Contains(attachErr.Error(), "did not appear") {
		t.Errorf("expected timeout error, got: %v", attachErr)
	}

	// Verify that Connect was called (fabricsDev was written).
	content, readErr := os.ReadFile(fabricsDev) //nolint:gosec
	if readErr != nil {
		t.Fatalf("read fabricsDev: %v", readErr)
	}
	written := strings.TrimRight(string(content), "\n")
	want := "transport=tcp,traddr=" + addr + ",trsvcid=" + port + ",nqn=" + nqn
	if written != want {
		t.Errorf("fabricsDev content mismatch:\n  want: %q\n  got:  %q", want, written)
	}
}

// TestNVMeoFTCPHandler_Attach_MissingConnectionID verifies that Attach
// returns an error when ConnectionID (SubsysNQN) is empty.
func TestNVMeoFTCPHandler_Attach_MissingConnectionID(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	_, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		Address:      "192.168.1.10",
		Port:         "4420",
	})
	if err == nil {
		t.Fatal("expected error for empty ConnectionID, got nil")
	}
}

// TestNVMeoFTCPHandler_Attach_MissingAddress verifies that Attach returns an
// error when Address is empty.
func TestNVMeoFTCPHandler_Attach_MissingAddress(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	_, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		ConnectionID: "nqn.2024-01.com.example:vol1",
		Port:         "4420",
	})
	if err == nil {
		t.Fatal("expected error for empty Address, got nil")
	}
}

// TestNVMeoFTCPHandler_Attach_MissingPort verifies that Attach returns an
// error when Port is empty.
func TestNVMeoFTCPHandler_Attach_MissingPort(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	_, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		ConnectionID: "nqn.2024-01.com.example:vol1",
		Address:      "192.168.1.10",
	})
	if err == nil {
		t.Fatal("expected error for empty Port, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Detach tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNVMeoFTCPHandler_Detach_DeletesController verifies that Detach calls
// connector.Disconnect and writes "1" to the controller's delete_controller
// sysfs entry.
func TestNVMeoFTCPHandler_Detach_DeletesController(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root, deleteCtrlPath := fakeSysfsWithController(t, nqn, "nvme0")

	h := newTestHandler(root, "")
	state := &NVMeoFProtocolState{SubsysNQN: nqn, Address: "192.168.1.10", Port: "4420"}

	if err := h.Detach(context.Background(), state); err != nil {
		t.Fatalf("Detach error: %v", err)
	}

	got, err := os.ReadFile(deleteCtrlPath) //nolint:gosec
	if err != nil {
		t.Fatalf("delete_controller not written: %v", err)
	}
	if string(got) != "1" {
		t.Fatalf("expected \"1\" written to delete_controller, got %q", string(got))
	}
}

// TestNVMeoFTCPHandler_Detach_NotConnected_IsNoOp verifies that Detach is
// idempotent: disconnecting an NQN that is not connected returns nil.
func TestNVMeoFTCPHandler_Detach_NotConnected_IsNoOp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(root, "")
	state := &NVMeoFProtocolState{SubsysNQN: "nqn.2024-01.com.example:vol1"}

	if err := h.Detach(context.Background(), state); err != nil {
		t.Fatalf("expected nil for no-op Detach, got: %v", err)
	}
}

// TestNVMeoFTCPHandler_Detach_WrongStateType verifies that passing a
// non-NVMeoFProtocolState to Detach returns an error.
func TestNVMeoFTCPHandler_Detach_WrongStateType(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	if err := h.Detach(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil state, got nil")
	}
}

// TestNVMeoFTCPHandler_Detach_EmptySubsysNQN verifies that passing a state
// with an empty SubsysNQN returns an error.
func TestNVMeoFTCPHandler_Detach_EmptySubsysNQN(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	state := &NVMeoFProtocolState{SubsysNQN: ""}
	if err := h.Detach(context.Background(), state); err == nil {
		t.Fatal("expected error for empty SubsysNQN, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Rescan tests
// ─────────────────────────────────────────────────────────────────────────────

// TestNVMeoFTCPHandler_Rescan_WritesRescanController verifies that Rescan
// writes "1" to the rescan_controller sysfs attribute for matching controllers.
func TestNVMeoFTCPHandler_Rescan_WritesRescanController(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"

	// Build a sysfs tree with a subsystem matching the NQN and a controller nvme0.
	root := t.TempDir()
	subsysDir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Add controller entry nvme0 inside subsystem directory.
	ctrlDir := filepath.Join(subsysDir, "nvme0")
	if err := os.MkdirAll(ctrlDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Create class/nvme/nvme0/ directory so rescan_controller can be written there.
	nvmeClassDir := filepath.Join(root, "class", "nvme", "nvme0")
	if err := os.MkdirAll(nvmeClassDir, 0o750); err != nil {
		t.Fatal(err)
	}
	rescanPath := filepath.Join(nvmeClassDir, "rescan_controller")

	h := newTestHandler(root, "")
	state := &NVMeoFProtocolState{SubsysNQN: nqn}

	if err := h.Rescan(context.Background(), state); err != nil {
		t.Fatalf("Rescan error: %v", err)
	}

	got, err := os.ReadFile(rescanPath) //nolint:gosec
	if err != nil {
		t.Fatalf("rescan_controller not written: %v", err)
	}
	if string(got) != "1" {
		t.Fatalf("expected \"1\" written to rescan_controller, got %q", string(got))
	}
}

// TestNVMeoFTCPHandler_Rescan_SkipsNamespaceEntries verifies that namespace
// entries (nvmeXnY pattern) are NOT treated as controllers during rescan.
func TestNVMeoFTCPHandler_Rescan_SkipsNamespaceEntries(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true /* addNamespace nvme0n1 */)

	// Ensure class/nvme/nvme0n1/ exists to detect accidental writes.
	nsClassDir := filepath.Join(root, "class", "nvme", "nvme0n1")
	if err := os.MkdirAll(nsClassDir, 0o750); err != nil {
		t.Fatal(err)
	}

	h := newTestHandler(root, "")
	state := &NVMeoFProtocolState{SubsysNQN: nqn}

	if err := h.Rescan(context.Background(), state); err != nil {
		t.Fatalf("Rescan error: %v", err)
	}

	// rescan_controller must NOT be written for namespace entries.
	rescanPath := filepath.Join(nsClassDir, "rescan_controller")
	if _, err := os.Stat(rescanPath); err == nil {
		t.Fatal("rescan_controller must not be written for a namespace (nvme0n1) entry")
	}
}

// TestNVMeoFTCPHandler_Rescan_SysfsAbsent_IsNoOp verifies that Rescan returns
// nil when the nvme-subsystem directory does not exist (kernel has no NVMe support).
func TestNVMeoFTCPHandler_Rescan_SysfsAbsent_IsNoOp(t *testing.T) {
	root := t.TempDir() // no class/nvme-subsystem directory
	h := newTestHandler(root, "")
	state := &NVMeoFProtocolState{SubsysNQN: "nqn.2024-01.com.example:vol1"}

	if err := h.Rescan(context.Background(), state); err != nil {
		t.Fatalf("expected nil for absent sysfs, got: %v", err)
	}
}

// TestNVMeoFTCPHandler_Rescan_WrongStateType verifies that passing a
// non-NVMeoFProtocolState to Rescan returns an error.
func TestNVMeoFTCPHandler_Rescan_WrongStateType(t *testing.T) {
	h := newTestHandler(t.TempDir(), "")
	if err := h.Rescan(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil state, got nil")
	}
}

// TestNVMeoFTCPHandler_Attach_Detach_RoundTrip verifies the full attach→detach
// lifecycle: Attach returns a State that Detach can use to disconnect.
func TestNVMeoFTCPHandler_Attach_Detach_RoundTrip(t *testing.T) {
	const (
		// Use a distinct NQN and controller name to ensure fakeSysfsWithController
		// parameters receive varied values across callers (fixes unparam lint).
		nqn  = "nqn.2024-01.com.example:roundtrip-vol"
		addr = "192.168.1.10"
		port = "4420"
	)

	// Use sysfs tree with subsystem + namespace already visible (idempotent connect)
	// and a controller entry so Detach can write delete_controller.
	// Use "nvme1" (not "nvme0") to vary the ctrlName argument across tests.
	root, deleteCtrlPath := fakeSysfsWithController(t, nqn, "nvme1")

	// Also add namespace entry so GetDevicePath returns the device.
	nsDir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0", "nvme0n1")
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		t.Fatal(err)
	}

	fabricsDev := fakeFabricsDev(t)
	h := newTestHandler(root, fabricsDev)

	// Attach
	result, err := h.Attach(context.Background(), AttachParams{
		ProtocolType: protocolNVMeoFTCP,
		ConnectionID: nqn,
		Address:      addr,
		Port:         port,
	})
	if err != nil {
		t.Fatalf("Attach error: %v", err)
	}
	if result.DevicePath == "" {
		t.Fatal("expected non-empty DevicePath after Attach")
	}

	// Detach using the State returned by Attach.
	detachErr := h.Detach(context.Background(), result.State)
	if detachErr != nil {
		t.Fatalf("Detach error: %v", detachErr)
	}

	// Verify delete_controller was written.
	got, err := os.ReadFile(deleteCtrlPath) //nolint:gosec
	if err != nil {
		t.Fatalf("delete_controller not written after Detach: %v", err)
	}
	if string(got) != "1" {
		t.Fatalf("expected \"1\" written to delete_controller, got %q", string(got))
	}
}
