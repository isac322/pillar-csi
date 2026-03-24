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

// Component error/exception path tests for the NVMe-oF configfs target
// (internal/agent/nvmeof/).
//
// This file is the dedicated home for NVMe-oF configfs error-path coverage.
// Each test targets a specific failure scenario:
//
//   - Partial failure mid-apply: filesystem-level blockers at specific steps
//     of the Apply pipeline (namespace directory, port directory).
//   - Permission denied on configfs writes: read-only directories prevent
//     directory creation and symlink creation (AllowHost).
//   - Device never appears (timeout): WaitForDevice with pre-cancelled context
//     or a very short timeout.
//
// Black-box setup: each test uses t.TempDir() as ConfigfsRoot.  No root
// privileges, no kernel configfs, no real NVMe devices are required.
// Tests that rely on Unix DAC permission bits auto-skip when running as root.
package component_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ---------------------------------------------------------------------------
// Partial failure mid-apply
// ---------------------------------------------------------------------------

// TestNvmeof_Error_PartialApply_NamespaceBlockedByFile verifies that Apply
// returns an error and leaves the subsystem in a recoverable state when the
// namespace directory path is occupied by a regular file.
//
// This tests a different failure point from TestNvmeof_Apply_PartialFailureMidApply
// (which blocks the ports/ directory); here the blocker is placed at the
// namespaces/ sub-path of the subsystem, causing step 2 (createNamespace) to
// fail while step 1 (createSubsystem) has already succeeded.
//
//	Setup:   Place a regular file at <subsystem>/namespaces to block mkdirAll.
//	Expect:  Apply returns error; subsystem dir still exists (partial state);
//	         removing the blocker and calling Apply again succeeds.
func TestNvmeof_Error_PartialApply_NamespaceBlockedByFile(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-ns-blocked"
	tgt := defaultTarget(tmpdir, nqn)

	// Pre-create the subsystem dir so that createSubsystem completes
	// successfully in step 1, and place a regular FILE at the namespaces/
	// sub-path so that mkdirAll(<subsystem>/namespaces/1) fails in step 2.
	subDir := nvmetSubsystemDir(tmpdir, nqn)
	if err := os.MkdirAll(subDir, 0o750); err != nil {
		t.Fatalf("MkdirAll subsystem: %v", err)
	}
	nsBlocker := filepath.Join(subDir, "namespaces")
	if err := os.WriteFile(nsBlocker, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("WriteFile namespaces blocker: %v", err)
	}

	// First Apply must fail: namespaces/ is a file, not a directory.
	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected Apply to fail when namespaces path is a file, got nil")
	}
	t.Logf("Apply correctly returned error: %v", err)

	// Subsystem directory (created in step 1) should still be present.
	requireDirExists(t, subDir)

	// Remove the blocker and confirm a second Apply succeeds (idempotent recovery).
	if err := os.Remove(nsBlocker); err != nil {
		t.Fatalf("remove namespaces blocker: %v", err)
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply after removing blocker: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Permission denied on configfs writes
// ---------------------------------------------------------------------------

// TestNvmeof_Error_Apply_PortsDirPermissionDenied verifies that Apply returns
// a descriptive error when the nvmet/ports/ directory is read-only and a new
// port subdirectory cannot be created (step 3: createPort).
//
// This is distinct from XC1 (subsystems/ read-only) in exceptions_test.go:
// here steps 1 (createSubsystem) and 2 (createNamespace) succeed because the
// subsystem tree is writable; only the ports tree is locked.
//
//	Setup:   Pre-create nvmet/ports/ as read-only (mode 0555).
//	Expect:  Apply returns non-nil error; no panic.
func TestNvmeof_Error_Apply_PortsDirPermissionDenied(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()

	// Pre-create nvmet/ports/ as read-only so that createPort cannot create
	// a subdirectory inside it.
	portsDir := filepath.Join(tmpdir, "nvmet", "ports")
	if err := os.MkdirAll(portsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll nvmet/ports: %v", err)
	}
	makeReadOnly(t, portsDir) // auto-skips as root; restores 0755 on cleanup

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: "nqn.2026-01.com.bhyoo:pvc-ports-permdeny",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-ports-permdeny",
		BindAddress:  "10.20.30.40",
		Port:         4420,
	}

	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected error when ports/ dir is read-only, got nil")
	}
	t.Logf("Apply correctly returned error on read-only ports dir: %v", err)
}

// TestNvmeof_Error_AllowHost_HostsDirPermissionDenied verifies that AllowHost
// returns a descriptive error when the nvmet/hosts/ directory is read-only and
// a new host directory cannot be created.
//
// AllowHost step 1 creates <nvmetRoot>/hosts/<hostNQN>/.  If the parent
// hosts/ directory is read-only, this mkdir fails immediately.
//
//	Setup:   Apply the target (creates nvmet/hosts/), then make hosts/ read-only.
//	Expect:  AllowHost returns non-nil error; no panic.
func TestNvmeof_Error_AllowHost_HostsDirPermissionDenied(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-hosts-ro"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-blocked"
	tgt := defaultTarget(tmpdir, nqn)

	// Apply successfully to establish the subsystem in configfs.
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Ensure the hosts/ directory exists (Apply may or may not create it
	// without ACL hosts), then make it read-only.
	hostsDir := filepath.Join(tmpdir, "nvmet", "hosts")
	if err := os.MkdirAll(hostsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll nvmet/hosts: %v", err)
	}
	makeReadOnly(t, hostsDir) // auto-skips as root; restores on cleanup

	// AllowHost must fail because it cannot create a subdirectory in hostsDir.
	err := tgt.AllowHost(hostNQN)
	if err == nil {
		t.Fatal("expected error when nvmet/hosts/ is read-only, got nil")
	}
	t.Logf("AllowHost correctly returned error on read-only hosts dir: %v", err)
}

// TestNvmeof_Error_AllowHost_AllowedHostsDirPermissionDenied verifies that
// AllowHost returns an error when the subsystem's allowed_hosts/ directory
// is read-only and the symlink cannot be created inside it.
//
// AllowHost step 2 creates the symlink at
// <subsystem>/allowed_hosts/<hostNQN>.  If allowed_hosts/ is read-only,
// os.Symlink fails with EACCES.
//
//	Setup:   Apply; manually create allowed_hosts/ and make it read-only.
//	Expect:  AllowHost returns non-nil error; no panic.
func TestNvmeof_Error_AllowHost_AllowedHostsDirPermissionDenied(t *testing.T) {
	t.Parallel()

	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-ah-ro"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-ah-ro"
	tgt := defaultTarget(tmpdir, nqn)

	// Apply to create the subsystem tree.
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Pre-create the allowed_hosts/ directory and make it read-only.
	// AllowHost step 2 calls mkdirAll(allowed_hosts) — a no-op because the
	// dir already exists — then tries to create a symlink inside it, which
	// will fail due to the directory being read-only.
	ahDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("MkdirAll allowed_hosts: %v", err)
	}
	makeReadOnly(t, ahDir) // auto-skips as root; restores on cleanup

	err := tgt.AllowHost(hostNQN)
	if err == nil {
		t.Fatal("expected error when allowed_hosts/ is read-only, got nil")
	}
	t.Logf("AllowHost correctly returned error on read-only allowed_hosts: %v", err)
}

// TestNvmeof_Error_Apply_EnableFileReadOnly verifies that Apply returns an
// error when the namespace's enable pseudo-file is pre-created as read-only.
//
// createNamespace writes "1" to the enable file to activate the namespace.
// If the file already exists but is unwritable (simulating a kernel-level
// restriction on repeated enable writes), Apply must surface the error.
//
//	Setup:   Pre-create namespace dir and enable file with mode 0444.
//	Expect:  Apply returns non-nil error; no panic.
func TestNvmeof_Error_Apply_EnableFileReadOnly(t *testing.T) {
	t.Parallel()

	const nqn = "nqn.2026-01.com.bhyoo:pvc-enable-ro"
	tmpdir := t.TempDir()

	// Pre-create the namespace directory and the enable pseudo-file.
	nsDir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn, "namespaces", "1")
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll namespace dir: %v", err)
	}
	enableFile := filepath.Join(nsDir, "enable")
	if err := os.WriteFile(enableFile, []byte("0"), 0o644); err != nil {
		t.Fatalf("WriteFile enable: %v", err)
	}
	makeFileReadOnly(t, enableFile) // auto-skips as root; restores 0644 on cleanup

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-enable-ro",
		BindAddress:  "10.0.0.1",
		Port:         4420,
	}

	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected error when namespace enable file is read-only, got nil")
	}
	t.Logf("Apply correctly returned error on read-only enable file: %v", err)
}

// ---------------------------------------------------------------------------
// Device never appears (timeout)
// ---------------------------------------------------------------------------

// TestNvmeof_Error_WaitForDevice_PreCancelledContext verifies that
// WaitForDevice returns an error immediately when the caller's context is
// already cancelled before the function is invoked.
//
// This is distinct from TestNvmeof_DevicePoll_ContextCancelled (which cancels
// the context after the function starts): here the context is cancelled before
// WaitForDevice is even called, so the first select should observe ctx.Done()
// immediately after the first checker invocation.
//
//	Setup:   Checker returns (false, nil); context pre-cancelled.
//	Expect:  Returns non-nil error without entering a long wait.
func TestNvmeof_Error_WaitForDevice_PreCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling WaitForDevice

	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil // device never present
	})

	start := time.Now()
	err := nvmeof.WaitForDevice(ctx, "/dev/fake/pre-cancel", 5*time.Millisecond, 5*time.Second, checker)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from pre-cancelled context, got nil")
	}

	// Should return almost immediately — not wait the 5 s internal timeout.
	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("elapsed %v, want < %v (should exit on pre-cancelled context)", elapsed, maxElapsed)
	}
	t.Logf("WaitForDevice with pre-cancelled context returned in %v: %v", elapsed, err)
}

// TestNvmeof_Error_WaitForDevice_ShortTimeoutNeverAppears verifies that
// WaitForDevice returns a timeout error when the device never becomes present
// within a very tight timeout.
//
// This test uses a shorter timeout than TestNvmeof_DevicePoll_NeverAppears to
// validate the timeout path under different timing constraints.
//
//	Setup:   Checker always returns (false, nil); timeout = 15 ms.
//	Expect:  Returns non-nil error; error message mentions "timed out".
func TestNvmeof_Error_WaitForDevice_ShortTimeoutNeverAppears(t *testing.T) {
	t.Parallel()

	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil // device never appears
	})

	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/zvol/tank/pvc-short-timeout",
		2*time.Millisecond,  // poll interval
		15*time.Millisecond, // very short timeout
		checker,
	)
	if err == nil {
		t.Fatal("expected timeout error when device never appears, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// The error message must contain "timed out" even if wrapped.
		errStr := err.Error()
		if !strings.Contains(errStr, "timed out") && !strings.Contains(errStr, "deadline") {
			t.Errorf("expected 'timed out' or 'deadline' in error, got: %v", err)
		}
	}
}
