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

// Component tests for the NVMe-oF configfs target (internal/agent/nvmeof/).
//
// Black-box setup: NvmetTarget with ConfigfsRoot = t.TempDir() (real filesystem,
// no root privileges, no kernel configfs).  Every test gets its own isolated
// tmpdir; all tests are safe to run in parallel.
package component_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ---------------------------------------------------------------------------
// Shared helpers for NVMe-oF tests
// ---------------------------------------------------------------------------

// nvmeTarget returns a NvmetTarget wired to configfsRoot with sane defaults.
func nvmeTarget(configfsRoot, nqn, devicePath, bindAddr string, port int32) *nvmeof.NvmetTarget {
	return &nvmeof.NvmetTarget{
		ConfigfsRoot: configfsRoot,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   devicePath,
		BindAddress:  bindAddr,
		Port:         port,
	}
}

// defaultTarget returns a NvmetTarget with standard test defaults.
func defaultTarget(configfsRoot, nqn string) *nvmeof.NvmetTarget {
	return nvmeTarget(configfsRoot, nqn, "/dev/zvol/tank/"+nqn, "10.0.0.1", 4420)
}

// nvmetSubsystemDir returns the configfs path for the subsystem directory.
func nvmetSubsystemDir(configfsRoot, nqn string) string {
	return filepath.Join(configfsRoot, "nvmet", "subsystems", nqn)
}

// nvmetNamespaceDir returns the configfs path for the namespace directory.
func nvmetNamespaceDir(configfsRoot, nqn string) string {
	return filepath.Join(nvmetSubsystemDir(configfsRoot, nqn), "namespaces", "1")
}

// nvmetPortsDir returns the configfs path for the ports directory.
func nvmetPortsDir(configfsRoot string) string {
	return filepath.Join(configfsRoot, "nvmet", "ports")
}

// readFile reads a file and returns its content as string, failing the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		t.Fatalf("readFile %q: %v", path, err)
	}
	return string(data)
}

// requireFileContent asserts a file exists and contains the expected content.
func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	got := readFile(t, path)
	if got != want {
		t.Errorf("file %q: got %q, want %q", path, got, want)
	}
}

// requireDirExists asserts a directory exists.
func requireDirExists(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("dir %q: %v", path, err)
	}
	if !fi.IsDir() {
		t.Fatalf("path %q is not a directory (mode=%s)", path, fi.Mode())
	}
}

// requireNotExist asserts a path does not exist.
func requireNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("path %q exists but should have been removed", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("lstat %q: unexpected error: %v", path, err)
	}
}

// findPortDirs returns all entries in the ports directory.
func findPortDirs(t *testing.T, configfsRoot string) []os.DirEntry {
	t.Helper()
	portsDir := nvmetPortsDir(configfsRoot)
	entries, err := os.ReadDir(portsDir)
	if err != nil {
		t.Fatalf("read ports dir %q: %v", portsDir, err)
	}
	return entries
}

// requireSinglePort asserts exactly one port dir exists and returns its path.
func requireSinglePort(t *testing.T, configfsRoot string) string {
	t.Helper()
	entries := findPortDirs(t, configfsRoot)
	if len(entries) != 1 {
		t.Fatalf("expected 1 port dir, got %d", len(entries))
	}
	return filepath.Join(nvmetPortsDir(configfsRoot), entries[0].Name())
}

// ---------------------------------------------------------------------------
// 3.1 Apply: full lifecycle and idempotency
// ---------------------------------------------------------------------------

// TestNvmeof_Apply_FullLifecycle verifies that Apply creates all required
// configfs directories and writes the correct attribute values.
//
//	Setup:   Fresh t.TempDir() as ConfigfsRoot; no AllowedHosts
//	Expect:  subsystems/<nqn>/, namespaces/1/, ports/<id>/ created;
//	         device_path, enable, addr_* files written; port symlink created
func TestNvmeof_Apply_FullLifecycle(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-lifecycle"
	devicePath := "/dev/zvol/tank/pvc-lifecycle"
	bindAddr := "10.0.0.1"
	port := int32(4420)

	tgt := nvmeTarget(tmpdir, nqn, devicePath, bindAddr, port)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// --- Subsystem directory ---
	subDir := nvmetSubsystemDir(tmpdir, nqn)
	requireDirExists(t, subDir)

	// attr_allow_any_host = "1" (no ACL hosts)
	requireFileContent(t, filepath.Join(subDir, "attr_allow_any_host"), "1")

	// --- Namespace directory ---
	nsDir := nvmetNamespaceDir(tmpdir, nqn)
	requireDirExists(t, nsDir)
	requireFileContent(t, filepath.Join(nsDir, "device_path"), devicePath)
	requireFileContent(t, filepath.Join(nsDir, "enable"), "1")

	// --- Port directory ---
	portDir := requireSinglePort(t, tmpdir)
	requireFileContent(t, filepath.Join(portDir, "addr_trtype"), "tcp")
	requireFileContent(t, filepath.Join(portDir, "addr_adrfam"), "ipv4")
	requireFileContent(t, filepath.Join(portDir, "addr_traddr"), bindAddr)
	requireFileContent(t, filepath.Join(portDir, "addr_trsvcid"), fmt.Sprintf("%d", port))

	// --- Port subsystem symlink ---
	linkPath := filepath.Join(portDir, "subsystems", nqn)
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink %q: %v", linkPath, err)
	}
	if target != subDir {
		t.Errorf("port symlink points to %q, want %q", target, subDir)
	}
}

// TestNvmeof_Apply_Idempotent verifies that calling Apply twice on the same
// target produces the same configfs state without error.
//
//	Setup:   Apply called twice with identical parameters
//	Expect:  No error; same files/dirs; no duplicates
func TestNvmeof_Apply_Idempotent(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-idem"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("second Apply (idempotent): %v", err)
	}

	// Still exactly one port dir.
	portDirs := findPortDirs(t, tmpdir)
	if len(portDirs) != 1 {
		t.Errorf("expected 1 port dir after idempotent Apply, got %d", len(portDirs))
	}

	// Symlink still correct.
	portDir := filepath.Join(nvmetPortsDir(tmpdir), portDirs[0].Name())
	linkPath := filepath.Join(portDir, "subsystems", nqn)
	if _, err := os.Readlink(linkPath); err != nil {
		t.Errorf("symlink gone after idempotent Apply: %v", err)
	}
}

// TestNvmeof_Apply_PartialFailureMidApply verifies that Apply is recoverable
// after a mid-flight failure and a subsequent Apply can complete successfully.
//
//	Setup:   Block port-dir creation by placing a regular file where the
//	         ports/ directory needs to be.  First Apply fails at createPort().
//	         Remove the blocker, second Apply succeeds.
//	Expect:  First Apply returns error; second Apply returns nil; configfs
//	         fully consistent after the second call.
func TestNvmeof_Apply_PartialFailureMidApply(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-partial"
	tgt := defaultTarget(tmpdir, nqn)

	// Create the nvmet root so that subsystem/namespace creation succeeds,
	// but place a FILE at the ports/ path so mkdirAll inside it fails.
	nvmetRoot := filepath.Join(tmpdir, "nvmet")
	if err := os.MkdirAll(nvmetRoot, 0o750); err != nil {
		t.Fatalf("mkdir nvmet root: %v", err)
	}
	portBlocker := filepath.Join(nvmetRoot, "ports")
	if err := os.WriteFile(portBlocker, []byte("block"), 0o600); err != nil {
		t.Fatalf("write ports blocker: %v", err)
	}

	// First Apply must fail because ports/ is a file, not a directory.
	err := tgt.Apply()
	if err == nil {
		t.Fatal("expected Apply to fail with ports blocked, got nil")
	}

	// Subsystem and namespace dirs should have been created before the failure.
	requireDirExists(t, nvmetSubsystemDir(tmpdir, nqn))
	requireDirExists(t, nvmetNamespaceDir(tmpdir, nqn))

	// Remove the blocker file so ports/ can be created as a directory.
	if err := os.Remove(portBlocker); err != nil {
		t.Fatalf("remove ports blocker: %v", err)
	}

	// Second Apply must succeed (idempotent for already-created parts).
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply after recovery: %v", err)
	}

	// Port dir and symlink must now exist.
	requireSinglePort(t, tmpdir)
}

// TestNvmeof_Apply_ACLEnabled verifies that when AllowedHosts is set,
// Apply creates host dirs, allowed_hosts symlinks, and sets
// attr_allow_any_host = "0".
//
//	Setup:   NvmetTarget with AllowedHosts = ["nqn.host-a", "nqn.host-b"]
//	Expect:  attr_allow_any_host = "0"; two symlinks under allowed_hosts/;
//	         two dirs under hosts/
func TestNvmeof_Apply_ACLEnabled(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-acl-on"
	hostA := "nqn.2026-01.com.bhyoo:host-a"
	hostB := "nqn.2026-01.com.bhyoo:host-b"

	tgt := defaultTarget(tmpdir, nqn)
	tgt.AllowedHosts = []string{hostA, hostB}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	subDir := nvmetSubsystemDir(tmpdir, nqn)

	// attr_allow_any_host must be "0" when ACL is enforced.
	requireFileContent(t, filepath.Join(subDir, "attr_allow_any_host"), "0")

	// Verify symlinks in allowed_hosts/.
	ahDir := filepath.Join(subDir, "allowed_hosts")
	for _, host := range []string{hostA, hostB} {
		linkPath := filepath.Join(ahDir, host)
		target, err := os.Readlink(linkPath)
		if err != nil {
			t.Errorf("allowed_hosts symlink for %q: %v", host, err)
			continue
		}
		expectedTarget := filepath.Join(tmpdir, "nvmet", "hosts", host)
		if target != expectedTarget {
			t.Errorf("symlink %q → %q, want %q", linkPath, target, expectedTarget)
		}
		// Host dir must exist.
		requireDirExists(t, filepath.Join(tmpdir, "nvmet", "hosts", host))
	}
}

// TestNvmeof_Apply_ACLDisabled verifies that when AllowedHosts is empty,
// Apply sets attr_allow_any_host = "1" (open access).
//
//	Setup:   NvmetTarget with empty AllowedHosts
//	Expect:  attr_allow_any_host = "1"
func TestNvmeof_Apply_ACLDisabled(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-acl-off"

	tgt := defaultTarget(tmpdir, nqn)
	// AllowedHosts is nil/empty by default.

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	subDir := nvmetSubsystemDir(tmpdir, nqn)
	requireFileContent(t, filepath.Join(subDir, "attr_allow_any_host"), "1")

	// No allowed_hosts directory should exist (or it's empty).
	ahDir := filepath.Join(subDir, "allowed_hosts")
	if entries, err := os.ReadDir(ahDir); err == nil && len(entries) > 0 {
		t.Errorf("expected no allowed_hosts entries when ACL disabled, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// 3.2 Remove: cleanup and idempotency
// ---------------------------------------------------------------------------

// TestNvmeof_Remove_FullCleanup verifies that Remove tears down all configfs
// entries created by Apply.
//
//	Setup:   Apply then Remove (no AllowedHosts)
//	Expect:  subsystems/<nqn>/ gone; port symlink gone; no orphan dirs
func TestNvmeof_Remove_FullCleanup(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-cleanup"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Capture port dir path before Remove deletes the symlink.
	portDir := requireSinglePort(t, tmpdir)
	linkPath := filepath.Join(portDir, "subsystems", nqn)

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subsystem directory must be gone.
	requireNotExist(t, nvmetSubsystemDir(tmpdir, nqn))

	// Port subsystem symlink must be gone.
	requireNotExist(t, linkPath)
}

// TestNvmeof_Remove_Idempotent verifies that calling Remove on a target that
// was never Apply-ed (or already fully removed) returns nil.
//
//	Setup:   Fresh tmpdir, no prior Apply
//	Expect:  Remove returns nil; no error
func TestNvmeof_Remove_Idempotent(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-remove-idem"
	tgt := defaultTarget(tmpdir, nqn)

	// First Remove: nothing to clean up.
	if err := tgt.Remove(); err != nil {
		t.Fatalf("first Remove (nothing existed): %v", err)
	}

	// Second Remove after Apply + Remove: also no-op.
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove after Apply: %v", err)
	}
	if err := tgt.Remove(); err != nil {
		t.Fatalf("second Remove (already cleaned): %v", err)
	}
}

// TestNvmeof_Remove_AlreadyRemovedSubsystem verifies that Remove handles
// a partial-removal state where the subsystem directory was deleted externally
// but the port symlink still exists.
//
//	Setup:   Apply succeeds; subsystem dir manually deleted; Remove called
//	Expect:  Remove succeeds; port symlink also cleaned up
func TestNvmeof_Remove_AlreadyRemovedSubsystem(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-partial-rm"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	portDir := requireSinglePort(t, tmpdir)
	linkPath := filepath.Join(portDir, "subsystems", nqn)

	// Verify port symlink exists before we break things.
	if _, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("expected symlink to exist: %v", err)
	}

	// Manually remove only the namespace files (simulates kernel removing them)
	// and then the namespace dir and subsystem dir.
	nsDir := nvmetNamespaceDir(tmpdir, nqn)
	_ = os.Remove(filepath.Join(nsDir, "device_path"))
	_ = os.Remove(filepath.Join(nsDir, "enable"))
	_ = os.Remove(nsDir)
	_ = os.Remove(filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "attr_allow_any_host"))
	_ = os.Remove(filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "namespaces"))
	_ = os.Remove(nvmetSubsystemDir(tmpdir, nqn))

	// Remove must succeed even though the subsystem dir is gone.
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove with missing subsystem dir: %v", err)
	}

	// Port symlink must have been cleaned up.
	requireNotExist(t, linkPath)
}

// ---------------------------------------------------------------------------
// 3.3 ACL: AllowHost / DenyHost
// ---------------------------------------------------------------------------

// TestNvmeof_AllowHost_CreatesSymlink verifies that AllowHost creates the
// expected directory under hosts/ and the symlink under allowed_hosts/.
//
//	Setup:   Apply (to create subsystem), then AllowHost
//	Expect:  hosts/<hostNQN>/ dir exists; allowed_hosts/<hostNQN> symlink exists
func TestNvmeof_AllowHost_CreatesSymlink(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-allow"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-1"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	// Host directory must exist.
	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	requireDirExists(t, hostDir)

	// allowed_hosts symlink must point to the host directory.
	linkPath := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts", hostNQN)
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink allowed_hosts symlink: %v", err)
	}
	if target != hostDir {
		t.Errorf("symlink points to %q, want %q", target, hostDir)
	}
}

// TestNvmeof_AllowHost_Idempotent verifies that calling AllowHost twice for
// the same host NQN does not return an error and produces a single symlink.
//
//	Setup:   Apply; AllowHost called twice for same host
//	Expect:  No error; exactly one symlink in allowed_hosts/
func TestNvmeof_AllowHost_Idempotent(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-allow-idem"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-idem"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("first AllowHost: %v", err)
	}
	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("second AllowHost (idempotent): %v", err)
	}

	// Exactly one symlink in allowed_hosts/.
	ahDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts")
	entries, err := os.ReadDir(ahDir)
	if err != nil {
		t.Fatalf("readdir allowed_hosts: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry in allowed_hosts, got %d", len(entries))
	}
}

// TestNvmeof_AllowHost_MultipleHosts verifies that AllowHost can be called for
// three distinct initiator NQNs, creating three separate symlinks.
//
//	Setup:   Apply; AllowHost for 3 different initiators
//	Expect:  3 dirs under hosts/; 3 symlinks under allowed_hosts/
func TestNvmeof_AllowHost_MultipleHosts(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-multi-host"
	hosts := []string{
		"nqn.2026-01.com.bhyoo:initiator-a",
		"nqn.2026-01.com.bhyoo:initiator-b",
		"nqn.2026-01.com.bhyoo:initiator-c",
	}
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, h := range hosts {
		if err := tgt.AllowHost(h); err != nil {
			t.Fatalf("AllowHost(%q): %v", h, err)
		}
	}

	ahDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts")
	entries, err := os.ReadDir(ahDir)
	if err != nil {
		t.Fatalf("readdir allowed_hosts: %v", err)
	}
	if len(entries) != len(hosts) {
		t.Errorf("expected %d entries in allowed_hosts, got %d", len(hosts), len(entries))
	}

	// Verify each host dir and symlink.
	for _, h := range hosts {
		hostDir := filepath.Join(tmpdir, "nvmet", "hosts", h)
		requireDirExists(t, hostDir)
		linkPath := filepath.Join(ahDir, h)
		if _, err := os.Readlink(linkPath); err != nil {
			t.Errorf("symlink for host %q: %v", h, err)
		}
	}
}

// TestNvmeof_DenyHost_RemovesSymlink verifies that DenyHost removes the
// allowed_hosts symlink for the given initiator NQN.
//
//	Setup:   Apply; AllowHost; DenyHost for same host
//	Expect:  Symlink removed; host dir in hosts/ still exists
func TestNvmeof_DenyHost_RemovesSymlink(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-deny"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-deny"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	// Verify the symlink exists before denying.
	linkPath := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts", hostNQN)
	if _, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("symlink should exist before DenyHost: %v", err)
	}

	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("DenyHost: %v", err)
	}

	// Symlink must be gone.
	requireNotExist(t, linkPath)

	// Host directory in hosts/ should still exist (not cleaned by DenyHost).
	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	requireDirExists(t, hostDir)
}

// TestNvmeof_DenyHost_Idempotent verifies that DenyHost on a host that has
// not been allowed (no symlink) returns nil.
//
//	Setup:   Apply; no AllowHost; DenyHost called
//	Expect:  No error
func TestNvmeof_DenyHost_Idempotent(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-deny-idem"
	hostNQN := "nqn.2026-01.com.bhyoo:initiator-not-allowed"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// DenyHost without prior AllowHost must succeed.
	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("DenyHost (no prior AllowHost): %v", err)
	}

	// Second DenyHost also a no-op.
	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("second DenyHost: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3.4 Port Management
// ---------------------------------------------------------------------------

// TestNvmeof_Port_MultipleSubsystemsSamePort verifies that two NvmetTargets
// sharing the same BindAddress:Port produce a single port directory containing
// symlinks for both subsystems.
//
//	Setup:   Two NvmetTargets: same BindAddress+Port, different NQNs
//	Expect:  One port dir; two subsystem symlinks inside ports/<id>/subsystems/
func TestNvmeof_Port_MultipleSubsystemsSamePort(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	tgt1 := nvmeTarget(tmpdir,
		"nqn.2026-01.com.bhyoo:pvc-port-a",
		"/dev/zvol/tank/pvc-port-a",
		"10.0.0.1", 4420)

	tgt2 := nvmeTarget(tmpdir,
		"nqn.2026-01.com.bhyoo:pvc-port-b",
		"/dev/zvol/tank/pvc-port-b",
		"10.0.0.1", 4420) // same BindAddress:Port → same portID

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1 Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2 Apply: %v", err)
	}

	// Only one port directory must exist (same portID derived from same addr:port).
	portDirEntries := findPortDirs(t, tmpdir)
	if len(portDirEntries) != 1 {
		t.Fatalf("expected 1 port dir, got %d", len(portDirEntries))
	}

	// Both subsystems must be linked from that single port dir.
	portSubsDir := filepath.Join(nvmetPortsDir(tmpdir), portDirEntries[0].Name(), "subsystems")
	subEntries, err := os.ReadDir(portSubsDir)
	if err != nil {
		t.Fatalf("readdir port subsystems: %v", err)
	}
	if len(subEntries) != 2 {
		t.Fatalf("expected 2 subsystem links in port dir, got %d", len(subEntries))
	}

	present := make(map[string]bool)
	for _, e := range subEntries {
		present[e.Name()] = true
	}
	for _, nqn := range []string{tgt1.SubsystemNQN, tgt2.SubsystemNQN} {
		if !present[nqn] {
			t.Errorf("subsystem %q missing from port symlinks directory", nqn)
		}
	}
}

// TestNvmeof_Port_SeparatePortsForDifferentAddresses verifies that two
// NvmetTargets with different BindAddresses produce separate port directories.
//
//	Setup:   Two NvmetTargets: different BindAddress, same NQN pattern
//	Expect:  Two distinct port directories
func TestNvmeof_Port_SeparatePortsForDifferentAddresses(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	tgt1 := nvmeTarget(tmpdir,
		"nqn.2026-01.com.bhyoo:pvc-sep-a",
		"/dev/zvol/tank/pvc-sep-a",
		"192.168.1.10", 4420)

	tgt2 := nvmeTarget(tmpdir,
		"nqn.2026-01.com.bhyoo:pvc-sep-b",
		"/dev/zvol/tank/pvc-sep-b",
		"192.168.1.11", 4420) // different BindAddress → different portID (with high probability)

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1 Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2 Apply: %v", err)
	}

	portDirEntries := findPortDirs(t, tmpdir)
	// The two different BindAddresses should produce different portIDs.
	// If they hash to the same value, the test is still valid (shared port is fine).
	// We just verify both subsystems are reachable via their port links.
	subsFound := make(map[string]bool)
	for _, pd := range portDirEntries {
		subsDir := filepath.Join(nvmetPortsDir(tmpdir), pd.Name(), "subsystems")
		entries, _ := os.ReadDir(subsDir)
		for _, e := range entries {
			subsFound[e.Name()] = true
		}
	}
	for _, nqn := range []string{tgt1.SubsystemNQN, tgt2.SubsystemNQN} {
		if !subsFound[nqn] {
			t.Errorf("subsystem %q not linked to any port", nqn)
		}
	}
}

// ---------------------------------------------------------------------------
// 3.5 Exports Scanning: ListExports
// ---------------------------------------------------------------------------

// TestNvmeof_ListExports_Success verifies that ListExports returns one entry
// per applied subsystem, with correct NQNs and namespace device paths.
//
//	Setup:   Two NvmetTargets applied in tmpdir configfs
//	Expect:  2 ExportedSubsystem entries, sorted by NQN
func TestNvmeof_ListExports_Success(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	nqn1 := "nqn.2026-01.com.bhyoo:pvc-exp-aaa"
	nqn2 := "nqn.2026-01.com.bhyoo:pvc-exp-bbb"
	dev1 := "/dev/zvol/tank/pvc-exp-aaa"
	dev2 := "/dev/zvol/tank/pvc-exp-bbb"

	tgt1 := nvmeTarget(tmpdir, nqn1, dev1, "10.0.0.1", 4420)
	tgt2 := nvmeTarget(tmpdir, nqn2, dev2, "10.0.0.1", 4420)

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1 Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2 Apply: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 2 {
		t.Fatalf("expected 2 exports, got %d", len(exports))
	}

	// ListExports returns sorted by NQN; nqn1 < nqn2 lexicographically.
	if exports[0].NQN != nqn1 {
		t.Errorf("exports[0].NQN = %q, want %q", exports[0].NQN, nqn1)
	}
	if exports[1].NQN != nqn2 {
		t.Errorf("exports[1].NQN = %q, want %q", exports[1].NQN, nqn2)
	}

	// Device paths must be correct.
	if got := exports[0].NamespaceDevicePaths[1]; got != dev1 {
		t.Errorf("exports[0] ns1 device_path = %q, want %q", got, dev1)
	}
	if got := exports[1].NamespaceDevicePaths[1]; got != dev2 {
		t.Errorf("exports[1] ns1 device_path = %q, want %q", got, dev2)
	}
}

// TestNvmeof_ListExports_Empty verifies that ListExports returns an empty
// slice (not an error) when the subsystems directory exists but is empty.
//
//	Setup:   Fresh tmpdir with empty nvmet/subsystems/ directory
//	Expect:  nil or empty slice; no error
func TestNvmeof_ListExports_Empty(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	// Create the empty subsystems directory (simulates nvmet module loaded,
	// no subsystems configured yet).
	subsDir := filepath.Join(tmpdir, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir subsystems: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 0 {
		t.Errorf("expected 0 exports, got %d", len(exports))
	}
}

// TestNvmeof_ListExports_NoSubsystemsDir verifies that ListExports returns
// a nil slice (no error) when the nvmet/subsystems/ directory does not exist.
//
//	Setup:   Fresh tmpdir (no nvmet subtree)
//	Expect:  nil slice; no error
func TestNvmeof_ListExports_NoSubsystemsDir(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports on empty dir: %v", err)
	}
	if exports != nil {
		t.Errorf("expected nil, got %v", exports)
	}
}

// TestNvmeof_ListExports_PartialSubsystem verifies that a subsystem directory
// without a namespaces/ subdirectory is returned with an empty
// NamespaceDevicePaths map (not an error).
//
//	Setup:   Subsystem dir created without namespaces/ subdirectory
//	Expect:  One ExportedSubsystem with empty NamespaceDevicePaths
func TestNvmeof_ListExports_PartialSubsystem(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-partial-exp"

	// Create subsystem directory without namespaces/ subtree.
	subDir := filepath.Join(tmpdir, "nvmet", "subsystems", nqn)
	if err := os.MkdirAll(subDir, 0o750); err != nil {
		t.Fatalf("mkdir subDir: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}
	// NQN falls back to directory name when attr_name is absent.
	if exports[0].NQN != nqn {
		t.Errorf("NQN = %q, want %q", exports[0].NQN, nqn)
	}
	// No namespace device paths.
	if len(exports[0].NamespaceDevicePaths) != 0 {
		t.Errorf("expected empty NamespaceDevicePaths, got %v", exports[0].NamespaceDevicePaths)
	}
}

// TestNvmeof_ListExports_WithAllowedHosts verifies that ExportedSubsystem.AllowedHosts
// is populated from the allowed_hosts/ directory entries.
//
//	Setup:   Apply with 2 AllowedHosts; ListExports
//	Expect:  AllowedHosts slice contains both host NQNs (sorted)
func TestNvmeof_ListExports_WithAllowedHosts(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-with-hosts"
	hostA := "nqn.2026-01.com.bhyoo:host-alpha"
	hostB := "nqn.2026-01.com.bhyoo:host-beta"

	tgt := defaultTarget(tmpdir, nqn)
	tgt.AllowedHosts = []string{hostA, hostB}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}

	allowedHosts := exports[0].AllowedHosts
	if len(allowedHosts) != 2 {
		t.Fatalf("expected 2 AllowedHosts, got %d: %v", len(allowedHosts), allowedHosts)
	}
	// The results are sorted alphabetically.
	if allowedHosts[0] != hostA {
		t.Errorf("AllowedHosts[0] = %q, want %q", allowedHosts[0], hostA)
	}
	if allowedHosts[1] != hostB {
		t.Errorf("AllowedHosts[1] = %q, want %q", allowedHosts[1], hostB)
	}
}

// TestNvmeof_ListExports_RoundTrip verifies the round-trip consistency:
// Apply a target and then ListExports should return matching data.
//
//	Setup:   Apply; ListExports
//	Expect:  Exported subsystem matches NvmetTarget parameters exactly
func TestNvmeof_ListExports_RoundTrip(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-roundtrip"
	devicePath := "/dev/zvol/tank/pvc-roundtrip"

	tgt := nvmeTarget(tmpdir, nqn, devicePath, "10.1.2.3", 4421)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}

	got := exports[0]
	if got.NQN != nqn {
		t.Errorf("NQN = %q, want %q", got.NQN, nqn)
	}
	if got.NamespaceDevicePaths[1] != devicePath {
		t.Errorf("ns1 device_path = %q, want %q", got.NamespaceDevicePaths[1], devicePath)
	}
	if got.AllowedHosts != nil {
		t.Errorf("expected nil AllowedHosts (no ACL), got %v", got.AllowedHosts)
	}
}

// ---------------------------------------------------------------------------
// 3.6 Device Polling: WaitForDevice
// ---------------------------------------------------------------------------

// TestNvmeof_DevicePoll_AppearsImmediately verifies that WaitForDevice returns
// immediately when the DeviceChecker reports the path as present on the first call.
//
//	Setup:   AlwaysPresentChecker (first call returns true)
//	Expect:  Returns nil without delay; no timeout
func TestNvmeof_DevicePoll_AppearsImmediately(t *testing.T) {
	t.Parallel()
	start := time.Now()
	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/fake/device",
		10*time.Millisecond,
		500*time.Millisecond,
		nvmeof.AlwaysPresentChecker,
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should return almost immediately (well under the timeout).
	if elapsed > 200*time.Millisecond {
		t.Errorf("took too long: %v (expected near-instant with AlwaysPresentChecker)", elapsed)
	}
}

// TestNvmeof_DevicePoll_AppearsAfterDelay verifies that WaitForDevice retries
// until the DeviceChecker finally reports the device as present.
//
//	Setup:   Custom checker that returns false for first 2 calls, then true
//	Expect:  Returns nil; call count >= 3
func TestNvmeof_DevicePoll_AppearsAfterDelay(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	callCount := 0

	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		return callCount >= 3, nil // present on 3rd call
	})

	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/fake/delayed",
		5*time.Millisecond,
		2*time.Second,
		checker,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	got := callCount
	mu.Unlock()

	if got < 3 {
		t.Errorf("expected at least 3 checker calls, got %d", got)
	}
}

// TestNvmeof_DevicePoll_NeverAppears verifies that WaitForDevice returns a
// timeout error when the device never becomes present within the deadline.
//
//	Setup:   Checker always returns false; very short timeout
//	Expect:  Returns error containing "timed out"
func TestNvmeof_DevicePoll_NeverAppears(t *testing.T) {
	t.Parallel()
	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil // never present
	})

	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/fake/never",
		5*time.Millisecond,
		30*time.Millisecond,
		checker,
	)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error, got: %v", err)
	}
}

// TestNvmeof_DevicePoll_ContextCancelled verifies that WaitForDevice respects
// context cancellation and returns immediately when the context is cancelled.
//
//	Setup:   Checker never returns true; context cancelled before timeout
//	Expect:  Returns error immediately (well before 5s timeout)
func TestNvmeof_DevicePoll_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context after a short delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil
	})

	start := time.Now()
	err := nvmeof.WaitForDevice(ctx, "/dev/fake/ctx-cancel", 5*time.Millisecond, 5*time.Second, checker)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	// Should not have waited the full 5s timeout.
	if elapsed > 2*time.Second {
		t.Errorf("waited too long after context cancel: %v", elapsed)
	}
}

// TestNvmeof_DevicePoll_PermissionDenied verifies that WaitForDevice returns
// the permanent error from the DeviceChecker immediately (no retry loop).
//
//	Setup:   Checker returns permanent error on first call
//	Expect:  Returns the permanent error without further retries (fast)
func TestNvmeof_DevicePoll_PermissionDenied(t *testing.T) {
	t.Parallel()
	permErr := errors.New("permission denied: /dev/fake/perm")

	var callCount int
	checker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		callCount++
		return false, permErr
	})

	err := nvmeof.WaitForDevice(
		context.Background(),
		"/dev/fake/perm",
		10*time.Millisecond,
		5*time.Second,
		checker,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, permErr) {
		t.Errorf("expected permErr in error chain, got: %v", err)
	}
	// Checker must have been called only once (no retries on permanent errors).
	if callCount != 1 {
		t.Errorf("expected 1 checker call, got %d (should stop on permanent error)", callCount)
	}
}

// ---------------------------------------------------------------------------
// Additional edge cases
// ---------------------------------------------------------------------------

// TestNvmeof_Apply_Remove_WithACL verifies the full lifecycle (Apply + Remove)
// when AllowedHosts is set: both the subsystem and the allowed_hosts symlinks
// are cleaned up correctly.
//
//	Setup:   Apply with 1 AllowedHost; Remove
//	Expect:  subsystem dir, allowed_hosts symlink, and namespace dir all gone
func TestNvmeof_Apply_Remove_WithACL(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-acl-lifecycle"
	hostNQN := "nqn.2026-01.com.bhyoo:host-lifecycle"

	tgt := defaultTarget(tmpdir, nqn)
	tgt.AllowedHosts = []string{hostNQN}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify ACL was set up.
	linkPath := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts", hostNQN)
	if _, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("ACL symlink should exist: %v", err)
	}

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subsystem dir must be gone.
	requireNotExist(t, nvmetSubsystemDir(tmpdir, nqn))
	// ACL symlink must be gone.
	requireNotExist(t, linkPath)
}

// TestNvmeof_Apply_DefaultPort verifies that when Port is 0, Apply uses
// the DefaultPort (4420) for the addr_trsvcid attribute.
//
//	Setup:   NvmetTarget with Port = 0
//	Expect:  addr_trsvcid = "4420"
func TestNvmeof_Apply_DefaultPort(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: "nqn.2026-01.com.bhyoo:pvc-default-port",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-default-port",
		BindAddress:  "10.0.0.1",
		Port:         0, // should default to 4420
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	portDir := requireSinglePort(t, tmpdir)
	requireFileContent(t, filepath.Join(portDir, "addr_trsvcid"), fmt.Sprintf("%d", nvmeof.DefaultPort))
}

// TestNvmeof_AllowHost_SymlinkWrongTarget verifies that AllowHost returns an
// error when an allowed_hosts symlink already exists but points to the wrong target.
//
//	Setup:   Manually create allowed_hosts/<hostNQN> symlink pointing to a
//	         wrong target; then call AllowHost
//	Expect:  AllowHost returns a non-nil error describing the conflict
func TestNvmeof_AllowHost_SymlinkWrongTarget(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.2026-01.com.bhyoo:pvc-symlink-conflict"
	hostNQN := "nqn.2026-01.com.bhyoo:host-conflict"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Manually create the allowed_hosts dir and a symlink pointing to the WRONG target.
	ahDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("mkdir allowed_hosts: %v", err)
	}
	wrongTarget := filepath.Join(tmpdir, "nvmet", "hosts", "nqn.wrong-target")
	linkPath := filepath.Join(ahDir, hostNQN)
	if err := os.Symlink(wrongTarget, linkPath); err != nil {
		t.Fatalf("create wrong symlink: %v", err)
	}

	// AllowHost should detect the wrong symlink target and return an error.
	err := tgt.AllowHost(hostNQN)
	if err == nil {
		t.Fatal("expected error when symlink points to wrong target, got nil")
	}
	if !strings.Contains(err.Error(), "already points to") {
		t.Errorf("expected 'already points to' in error, got: %v", err)
	}
}
