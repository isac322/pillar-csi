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
// All tests use a temporary directory as a fake sysfs root and an injected
// execCommand stub, so no kernel NVMe modules or root privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNVMeoFConnector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// fakeSysfs creates a minimal /sys/class/nvme-subsystem tree inside dir for
// the given NQN. If addNamespace is true it also creates an nvme0n1 entry.
func fakeSysfs(t *testing.T, nqn string, addNamespace bool) string {
	t.Helper()
	root := t.TempDir()
	subsysDir := filepath.Join(root, "class", "nvme-subsystem", "nvme-subsys0")
	if err := os.MkdirAll(subsysDir, 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subsysDir, "subsysnqn"), []byte(nqn+"\n"), 0o644); err != nil {
		t.Fatalf("write subsysnqn: %v", err)
	}
	if addNamespace {
		nsDir := filepath.Join(subsysDir, "nvme0n1")
		if err := os.MkdirAll(nsDir, 0o755); err != nil {
			t.Fatalf("mkdirall ns: %v", err)
		}
	}
	return root
}

// stubExec records all invocations and returns pre-programmed (output, err).
type stubExec struct {
	calls  [][]string // each element is [name, arg0, arg1, ...]
	output []byte
	err    error
}

func (s *stubExec) fn(name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	s.calls = append(s.calls, call)
	return s.output, s.err
}

// newConnector constructs a NVMeoFConnector with injectable sysfs root and
// exec stub.
func newConnector(sysfsRoot string, stub *stubExec) *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:   sysfsRoot,
		execCommand: stub.fn,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestDisconnect_NotConnected_IsNoOp verifies that Disconnect on an NQN that
// has no sysfs entry (not connected) returns nil without invoking nvme-cli.
func TestDisconnect_NotConnected_IsNoOp(t *testing.T) {
	root := t.TempDir() // empty — no nvme-subsystem entries
	// Create the nvme-subsystem directory so ReadDir doesn't fail with ENOENT.
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &stubExec{}
	c := newConnector(root, stub)

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected 0 exec calls, got %d: %v", len(stub.calls), stub.calls)
	}
}

// TestDisconnect_SysfsAbsent_IsNoOp verifies that Disconnect when the whole
// nvme-subsystem directory is missing (no NVMe support in kernel) returns nil
// without error — idempotent disconnect.
func TestDisconnect_SysfsAbsent_IsNoOp(t *testing.T) {
	root := t.TempDir() // no class/nvme-subsystem directory at all
	stub := &stubExec{}
	c := newConnector(root, stub)

	if err := c.Disconnect(context.Background(), "nqn.2024-01.com.example:vol1"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected 0 exec calls, got %d", len(stub.calls))
	}
}

// TestDisconnect_Connected_RunsNvmeDisconnect verifies that Disconnect when
// the NQN is present in sysfs invokes "nvme disconnect -n <nqn>".
func TestDisconnect_Connected_RunsNvmeDisconnect(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	stub := &stubExec{output: []byte("NVMe disconnect")}
	c := newConnector(root, stub)

	if err := c.Disconnect(context.Background(), nqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	want := []string{"nvme", "disconnect", "-n", nqn}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("exec args mismatch: want %v, got %v", want, got)
		}
	}
}

// TestDisconnect_Connected_CmdError propagates nvme-cli exit errors.
func TestDisconnect_Connected_CmdError(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	cmdErr := errors.New("exit status 1")
	stub := &stubExec{output: []byte("failed to disconnect"), err: cmdErr}
	c := newConnector(root, stub)

	err := c.Disconnect(context.Background(), nqn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, cmdErr) {
		t.Fatalf("expected error to wrap cmdErr; got: %v", err)
	}
}

// TestDisconnect_DifferentNQN_IsNoOp ensures we only disconnect when the
// sysfs NQN exactly matches — a different NQN in sysfs must not trigger disconnect.
func TestDisconnect_DifferentNQN_IsNoOp(t *testing.T) {
	const sysnqn = "nqn.2024-01.com.example:vol1"
	const reqnqn = "nqn.2024-01.com.example:vol2"
	root := fakeSysfs(t, sysnqn, true)
	stub := &stubExec{}
	c := newConnector(root, stub)

	if err := c.Disconnect(context.Background(), reqnqn); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected 0 exec calls, got %d", len(stub.calls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Connect tests
// ─────────────────────────────────────────────────────────────────────────────

// TestConnect_NotConnected_RunsNvmeConnect verifies that Connect on a new NQN
// invokes "nvme connect -t tcp -a <addr> -s <port> -n <nqn>".
func TestConnect_NotConnected_RunsNvmeConnect(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := &stubExec{output: []byte("connected")}
	c := newConnector(root, stub)

	const nqn = "nqn.2024-01.com.example:vol1"
	if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(stub.calls))
	}
	want := []string{"nvme", "connect", "-t", "tcp", "-a", "192.168.1.10", "-s", "4420", "-n", nqn}
	for i, w := range want {
		if i >= len(stub.calls[0]) || stub.calls[0][i] != w {
			t.Fatalf("exec args mismatch: want %v, got %v", want, stub.calls[0])
		}
	}
}

// TestConnect_AlreadyConnected_IsNoOp verifies that Connect on an NQN already
// present in sysfs returns nil without invoking nvme-cli.
func TestConnect_AlreadyConnected_IsNoOp(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	stub := &stubExec{}
	c := newConnector(root, stub)

	if err := c.Connect(context.Background(), nqn, "192.168.1.10", "4420"); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("expected 0 exec calls, got %d", len(stub.calls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDevicePath tests
// ─────────────────────────────────────────────────────────────────────────────

// TestGetDevicePath_Found returns the /dev/nvmeXnY path when sysfs has it.
func TestGetDevicePath_Found(t *testing.T) {
	const nqn = "nqn.2024-01.com.example:vol1"
	root := fakeSysfs(t, nqn, true)
	stub := &stubExec{}
	c := newConnector(root, stub)

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
	stub := &stubExec{}
	c := newConnector(root, stub)

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
	if err := os.MkdirAll(filepath.Join(root, "class", "nvme-subsystem"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := &stubExec{}
	c := newConnector(root, stub)

	path, err := c.GetDevicePath(context.Background(), "nqn.2024-01.com.example:vol1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}
