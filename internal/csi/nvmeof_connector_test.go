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

// Unit tests for NVMeoFConnector — Connect, Disconnect, GetDevicePath.
//
// All tests use a temporary directory as a fake sysfs root and a temporary
// file as a fake /dev/nvme-fabrics device, so no kernel NVMe modules or root
// privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNVMeoFConnector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// fakeSysfs creates a minimal /sys/class/nvme-subsystem tree inside dir for
// the given NQN. If addNamespace is true it also creates an nvme0n1 entry.
func fakeSysfs(t *testing.T, nqn string, addNamespace bool) string { //nolint:unparam
	t.Helper()
	root := t.TempDir()
	subsysDir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o750); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatalf("write subsysnqn: %v", err)
	}
	if addNamespace {
		nsDir := filepath.Join(subsysDir, "nvme0n1")
		if err := os.MkdirAll(nsDir, 0o750); err != nil {
			t.Fatalf("mkdirall ns: %v", err)
		}
	}
	return root
}

// fakeSysfsWithController creates a sysfs tree that contains a controller
// entry (nvme0) inside the subsystem directory and a corresponding
// class/nvme/nvme0/ directory so that Disconnect can write delete_controller.
// Returns the sysfsRoot and the path to the delete_controller file.
func fakeSysfsWithController(t *testing.T, nqn, ctrlName string) (sysfsRoot, deleteCtrlPath string) {
	t.Helper()
	sysfsRoot = t.TempDir()
	subsysDir := filepath.Join(sysfsRoot, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o750); err != nil {
		t.Fatalf("mkdirall subsys: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o600); err != nil {
		t.Fatalf("write subsysnqn: %v", err)
	}
	// Controller symlink-like entry inside the subsystem directory.
	ctrlDir := filepath.Join(subsysDir, ctrlName)
	if err := os.MkdirAll(ctrlDir, 0o750); err != nil {
		t.Fatalf("mkdirall ctrl: %v", err)
	}
	// class/nvme/<ctrlName>/ so Disconnect can create delete_controller there.
	nvmeClassDir := filepath.Join(sysfsRoot, "class", "nvme", ctrlName)
	if err := os.MkdirAll(nvmeClassDir, 0o750); err != nil {
		t.Fatalf("mkdirall nvme class: %v", err)
	}
	deleteCtrlPath = filepath.Join(nvmeClassDir, "delete_controller")
	return sysfsRoot, deleteCtrlPath
}

// fakeFabricsDev creates a temporary file that acts as /dev/nvme-fabrics.
// Returns the file path. The file is automatically cleaned up by t.TempDir.
func fakeFabricsDev(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "nvme-fabrics-*")
	if err != nil {
		t.Fatalf("create fake nvme-fabrics: %v", err)
	}
	f.Close() //nolint:errcheck,gosec
	return f.Name()
}

// newConnector constructs a NVMeoFConnector with injectable sysfs root and
// fabricsDev path — no exec command required.
func newConnector(sysfsRoot, fabricsDev string) *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:  sysfsRoot,
		fabricsDev: fabricsDev,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestDisconnect_NotConnected_IsNoOp verifies that Disconnect on an NQN that
// has no sysfs entry (not connected) returns nil without touching any file.
func TestDisconnect_NotConnected_IsNoOp(t *testing.T) {
	root := t.TempDir() // empty — no nvme-subsystem entries
	// Create the nvme-subsystem directory so ReadDir doesn't fail with ENOENT.
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}

	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestDisconnect_SysfsAbsent_IsNoOp verifies that Disconnect when the whole
// nvme-subsystem directory is missing (no NVMe support in kernel) returns nil
// without error — idempotent disconnect.
func TestDisconnect_SysfsAbsent_IsNoOp(t *testing.T) {
	root := t.TempDir() // no class/nvme-subsystem directory at all
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestDisconnect_Connected_DeletesController verifies that Disconnect when
// the NQN is present in sysfs writes "1" to the controller's delete_controller
// sysfs entry (kernel-native teardown, no nvme-cli).
func TestDisconnect_Connected_DeletesController(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root, deleteCtrlPath := fakeSysfsWithController(t, nqn, "nvme0")
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), nqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	got, err := os.ReadFile(deleteCtrlPath) //nolint:gosec
	if err != nil {
		t.Fatalf("delete_controller not written: %v", err)
	}
	if string(got) != "1" {
		t.Fatalf("expected \"1\" written to delete_controller, got %q", string(got))
	}
}

// TestDisconnect_Connected_SkipsNamespaceEntries verifies that namespace
// entries (nvmeXnY pattern) inside the subsystem directory are NOT treated
// as controllers — no delete_controller write should occur for them.
func TestDisconnect_Connected_SkipsNamespaceEntries(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	// Use fakeSysfs which adds nvme0n1 (namespace), not a controller.
	root := fakeSysfs(t, nqn, true /* addNamespace */)
	// Ensure class/nvme/ exists — if a namespace were incorrectly treated as
	// a controller the code would attempt to write there.
	nvmeClassDir := filepath.Join(root, "class", "nvme", "nvme0n1")
	if err := os.MkdirAll(nvmeClassDir, 0o750); err != nil {
		t.Fatal(err)
	}
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), nqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// delete_controller must NOT have been created for the namespace entry.
	deleteCtrlPath := filepath.Join(nvmeClassDir, "delete_controller")
	if _, err := os.Stat(deleteCtrlPath); err == nil {
		t.Fatal("delete_controller must not be written for a namespace (nvme0n1) entry")
	}
}

// TestDisconnect_DifferentNQN_IsNoOp ensures we only disconnect when the
// sysfs NQN exactly matches — a different NQN in sysfs must not trigger
// any delete_controller write.
func TestDisconnect_DifferentNQN_IsNoOp(t *testing.T) {
	const sysnqn = "nqn.2024-01.com.example:vol1"
	const reqnqn = "nqn.2024-01.com.example:vol2"
	root, deleteCtrlPath := fakeSysfsWithController(t, sysnqn, "nvme0")
	c := newConnector(root, "")

	if err := c.Disconnect(context.Background(), reqnqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// delete_controller must NOT have been written.
	if _, err := os.Stat(deleteCtrlPath); err == nil {
		data, _ := os.ReadFile(deleteCtrlPath) //nolint:errcheck,gosec
		t.Fatalf("delete_controller must not be written for a different NQN; got %q", data)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Connect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestConnect_NotConnected_WritesFabricsDevice verifies that Connect on a new
// NQN opens the fabrics device and writes the correct connect string:
//
//	transport=tcp,traddr=<addr>,trsvcid=<port>,nqn=<nqn>
func TestConnect_NotConnected_WritesFabricsDevice(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	const (
		nqn  = "nqn.2024-01.com.example:vol1"
		addr = "192.168.1.10"
		port = "4420"
	)
	if err := c.Connect(context.Background(), nqn, addr, port); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	content, err := os.ReadFile(fabricsDev) //nolint:gosec
	if err != nil {
		t.Fatalf("read fabricsDev: %v", err)
	}
	written := strings.TrimRight(string(content), "\n")
	want := "transport=tcp,traddr=" + addr + ",trsvcid=" + port + ",nqn=" + nqn
	if written != want {
		t.Fatalf("fabricsDev content mismatch:\n  want: %q\n  got:  %q", want, written)
	}
}

// TestConnect_AlreadyConnected_IsNoOp verifies that Connect on an NQN already
// present in sysfs returns nil without writing to the fabrics device.
func TestConnect_AlreadyConnected_IsNoOp(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	fabricsDev := fakeFabricsDev(t)
	c := newConnector(root, fabricsDev)

	if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// fabricsDev must remain empty (no write occurred).
	content, err := os.ReadFile(fabricsDev) //nolint:gosec
	if err != nil {
		t.Fatalf("read fabricsDev: %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("expected no write to fabricsDev for already-connected NQN, got %q", string(content))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDevicePath tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetDevicePath_Found returns the /dev/nvmeXnY path when sysfs has it.
func TestGetDevicePath_Found(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), nqn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/dev/nvme0n1" {
		t.Fatalf("expected /dev/nvme0n1, got %q", path)
	}
}

// TestGetDevicePath_NoNamespace returns ("", nil) when subsystem exists but
// no namespace block device entry is present yet.
func TestGetDevicePath_NoNamespace(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, false) // no namespace dir
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), nqn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}

// TestGetDevicePath_NotConnected returns ("", nil) when there is no subsystem
// entry for the requested NQN.
func TestGetDevicePath_NotConnected(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := newConnector(root, "")

	path, err := c.GetDevicePath(context.Background(), "nqn.2024-01.com.example:vol1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}
