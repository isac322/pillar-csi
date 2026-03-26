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

// Extended component tests for the NVMe-oF configfs target (internal/agent/nvmeof/).
//
// This file covers sections 3.11 – 3.15 of TESTCASES.md:
//
//	3.11 Port ID Determinism and Multi-target Port Reuse
//	3.12 Remove Lifecycle Edge Cases
//	3.13 AllowHost / DenyHost Edge Cases
//	3.14 ListExports Advanced Scanning
//	3.15 Apply Field Verification
//
// Mock fidelity: same as nvmeof_test.go — NvmetTarget.ConfigfsRoot is set to
// t.TempDir() (a real tmpfs directory).  No kernel nvmet module; all
// configfs operations are plain filesystem operations.
package component_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// extTestBindAddr is the NVMe-oF bind address used across extended tests.
const extTestBindAddr = "10.0.0.1"

// ─────────────────────────────────────────────────────────────────────────────
// Section 3.11 — Port ID Determinism and Multi-target Port Reuse
// ─────────────────────────────────────────────────────────────────────────────.

// TestNvmeof_Apply_PortID_Deterministic verifies that two independent Apply
// calls with the same BindAddress+Port produce port dirs with identical names,
// demonstrating that the port ID is computed deterministically.
//
//	Setup:   Apply target1 (addr="10.0.0.1", port=4420) to tmpdir1;
//	         apply target2 (same addr+port, different NQN) to tmpdir2
//	Expect:  Both tmpdirs contain a port dir with identical names
func TestNvmeof_Apply_PortID_Deterministic(t *testing.T) {
	t.Parallel()

	const addr = extTestBindAddr
	const port = int32(4420)

	tmpdir1 := t.TempDir()
	tmpdir2 := t.TempDir()

	tgt1 := nvmeTarget(tmpdir1, "nqn.test:pvc-det-1", "/dev/zvol/tank/det-1", addr, port)
	tgt2 := nvmeTarget(tmpdir2, "nqn.test:pvc-det-2", "/dev/zvol/tank/det-2", addr, port)

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1.Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2.Apply: %v", err)
	}

	entries1 := findPortDirs(t, tmpdir1)
	entries2 := findPortDirs(t, tmpdir2)

	if len(entries1) != 1 || len(entries2) != 1 {
		t.Fatalf("expected 1 port dir each; got %d and %d", len(entries1), len(entries2))
	}

	name1 := entries1[0].Name()
	name2 := entries2[0].Name()
	if name1 != name2 {
		t.Errorf("port dir name mismatch: tmpdir1=%q, tmpdir2=%q (not deterministic)", name1, name2)
	}
}

// TestNvmeof_Apply_PortID_DifferentForDifferentAddresses verifies that two
// NvmetTargets with the same port number but different bind addresses produce
// separate port directories.
//
//	Setup:   Apply two NvmetTargets sharing the same tmpdir with same port
//	         but different BindAddress values
//	Expect:  Two distinct port dirs; each contains the correct addr_traddr
func TestNvmeof_Apply_PortID_DifferentForDifferentAddresses(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	const port = int32(4420)
	addr1 := extTestBindAddr
	addr2 := "10.0.0.2"

	tgt1 := nvmeTarget(tmpdir, "nqn.test:pvc-addr1", "/dev/zvol/tank/addr1", addr1, port)
	tgt2 := nvmeTarget(tmpdir, "nqn.test:pvc-addr2", "/dev/zvol/tank/addr2", addr2, port)

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1.Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2.Apply: %v", err)
	}

	entries := findPortDirs(t, tmpdir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 port dirs for 2 different addresses, got %d", len(entries))
	}

	// Verify each port dir has the correct addr_traddr.
	portsDir := nvmetPortsDir(tmpdir)
	foundAddrs := map[string]bool{}
	for _, e := range entries {
		content := readFile(t, filepath.Join(portsDir, e.Name(), "addr_traddr"))
		foundAddrs[content] = true
	}
	if !foundAddrs[addr1] {
		t.Errorf("addr_traddr %q not found in any port dir", addr1)
	}
	if !foundAddrs[addr2] {
		t.Errorf("addr_traddr %q not found in any port dir", addr2)
	}
}

// TestNvmeof_Apply_ReusesSamePortDir verifies that two different subsystems
// sharing the same bind addr:port reuse one port directory and each receive
// their own subsystem symlink inside it.
//
//	Setup:   Apply two NvmetTargets with identical BindAddress+Port but
//	         different NQNs into one shared tmpdir
//	Expect:  Exactly one port dir; two subsystem symlinks inside
//	         ports/<id>/subsystems/
func TestNvmeof_Apply_ReusesSamePortDir(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()

	nqn1 := "nqn.test:pvc-shared-port-1"
	nqn2 := "nqn.test:pvc-shared-port-2"
	addr := extTestBindAddr
	port := int32(4420)

	tgt1 := nvmeTarget(tmpdir, nqn1, "/dev/zvol/tank/sp1", addr, port)
	tgt2 := nvmeTarget(tmpdir, nqn2, "/dev/zvol/tank/sp2", addr, port)

	if err := tgt1.Apply(); err != nil {
		t.Fatalf("tgt1.Apply: %v", err)
	}
	if err := tgt2.Apply(); err != nil {
		t.Fatalf("tgt2.Apply: %v", err)
	}

	// Exactly one port dir.
	portDir := requireSinglePort(t, tmpdir)

	// Both subsystem symlinks exist inside the shared port dir.
	link1 := filepath.Join(portDir, "subsystems", nqn1)
	link2 := filepath.Join(portDir, "subsystems", nqn2)
	if _, err := os.Readlink(link1); err != nil {
		t.Errorf("subsystem symlink for nqn1 missing: %v", err)
	}
	if _, err := os.Readlink(link2); err != nil {
		t.Errorf("subsystem symlink for nqn2 missing: %v", err)
	}
}

// TestNvmeof_Apply_NamespaceIDNonDefault verifies that a non-standard NamespaceID
// produces the correct namespace directory path.
//
//	Setup:   NvmetTarget{NamespaceID: 5, …}; Apply
//	Expect:  namespaces/5/ created with device_path and enable files;
//	         no namespaces/1/
func TestNvmeof_Apply_NamespaceIDNonDefault(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-nsid5"

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  5,
		DevicePath:   "/dev/zvol/tank/nsid5",
		BindAddress:  extTestBindAddr,
		Port:         4420,
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	subDir := nvmetSubsystemDir(tmpdir, nqn)
	ns5Dir := filepath.Join(subDir, "namespaces", "5")
	requireDirExists(t, ns5Dir)
	requireFileContent(t, filepath.Join(ns5Dir, "device_path"), "/dev/zvol/tank/nsid5")
	requireFileContent(t, filepath.Join(ns5Dir, "enable"), "1")

	// NamespaceID=1 must NOT be created.
	ns1Dir := filepath.Join(subDir, "namespaces", "1")
	requireNotExist(t, ns1Dir)
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 3.12 — Remove Lifecycle Edge Cases
// ─────────────────────────────────────────────────────────────────────────────.

// TestNvmeof_Remove_LeavesPortDirIntact verifies that Remove deletes the
// subsystem's port symlink but does NOT remove the port directory itself
// (other subsystems may still reference that port).
//
//	Setup:   Apply; Remove; inspect nvmet/ports/
//	Expect:  subsystem dir gone; ports/<id>/ still present
func TestNvmeof_Remove_LeavesPortDirIntact(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-port-intact"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	portDir := requireSinglePort(t, tmpdir)

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subsystem dir must be gone.
	requireNotExist(t, nvmetSubsystemDir(tmpdir, nqn))

	// Port dir must still exist (not cleaned by Remove).
	requireDirExists(t, portDir)
}

// TestNvmeof_Remove_LeavesHostsDirIntact verifies that Remove does not clean
// the global hosts/ directory, even when AllowedHosts were configured.
//
//	Setup:   Apply with AllowedHosts=["host-nqn"]; Remove
//	Expect:  nvmet/hosts/ still present; hosts/host-nqn/ still exists
func TestNvmeof_Remove_LeavesHostsDirIntact(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-hosts-intact"
	hostNQN := "nqn.test:host-intact"

	// Use AllowedHosts in the target struct so Remove knows to clean the symlinks.
	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/hosts-intact",
		BindAddress:  extTestBindAddr,
		Port:         4420,
		AllowedHosts: []string{hostNQN},
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	requireDirExists(t, hostDir)

	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The global hosts/ dir must still exist after Remove.
	requireDirExists(t, filepath.Join(tmpdir, "nvmet", "hosts"))
	// The specific host subdir must also still exist (Remove does not clean hosts/).
	requireDirExists(t, hostDir)
}

// TestNvmeof_Remove_PortLinkAlreadyGone verifies that Remove succeeds
// (idempotent) when the port subsystem symlink has already been deleted.
//
//	Setup:   Apply; manually delete port symlink; Remove
//	Expect:  Returns nil; subsystem dir cleaned up; no error
func TestNvmeof_Remove_PortLinkAlreadyGone(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-link-gone"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Manually remove the port subsystem symlink.
	portDir := requireSinglePort(t, tmpdir)
	linkPath := filepath.Join(portDir, "subsystems", nqn)
	if err := os.Remove(linkPath); err != nil {
		t.Fatalf("manual remove port symlink: %v", err)
	}

	// Remove must still succeed.
	if err := tgt.Remove(); err != nil {
		t.Errorf("Remove: expected nil when port link already gone, got: %v", err)
	}

	// Subsystem dir must be cleaned.
	requireNotExist(t, nvmetSubsystemDir(tmpdir, nqn))
}

// TestNvmeof_Remove_SuccessAfterApplyWithNoHosts verifies that a complete
// Apply→Remove lifecycle with no AllowedHosts leaves no stale entries.
//
//	Setup:   Apply with empty AllowedHosts; Remove
//	Expect:  All created dirs removed; no dangling entries in
//	         subsystems/ or namespaces/
func TestNvmeof_Remove_SuccessAfterApplyWithNoHosts(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-no-hosts"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subsystem dir must be gone.
	requireNotExist(t, nvmetSubsystemDir(tmpdir, nqn))
	// Namespace dir must be gone.
	requireNotExist(t, filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "namespaces", "1"))
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 3.13 — AllowHost / DenyHost Edge Cases
// ─────────────────────────────────────────────────────────────────────────────.

// TestNvmeof_AllowHost_CreatesHostDir verifies that AllowHost creates the
// global hosts/<hostNQN>/ directory if it does not already exist.
//
//	Setup:   Apply; AllowHost("host-nqn")
//	Expect:  hosts/host-nqn/ dir exists under nvmet/
func TestNvmeof_AllowHost_CreatesHostDir(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-create-hostdir"
	hostNQN := "nqn.test:host-new"

	tgt := defaultTarget(tmpdir, nqn)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// host dir must not exist before AllowHost.
	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	requireNotExist(t, hostDir)

	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	// hosts/<nqn>/ must now exist.
	requireDirExists(t, hostDir)
}

// TestNvmeof_AllowHost_HostDirPreExists verifies that AllowHost is idempotent
// when the hosts/<nqn>/ directory already exists.
//
//	Setup:   Pre-create nvmet/hosts/host-nqn/ manually; AllowHost("host-nqn")
//	Expect:  Returns nil; allowed_hosts/<nqn> symlink created; no error
func TestNvmeof_AllowHost_HostDirPreExists(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-pre-hostdir"
	hostNQN := "nqn.test:host-pre"

	// Pre-create the hosts/<nqn>/ directory.
	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		t.Fatalf("pre-create host dir: %v", err)
	}

	tgt := defaultTarget(tmpdir, nqn)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost with pre-existing host dir: %v", err)
	}

	// Symlink must be created correctly.
	linkPath := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts", hostNQN)
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink %q: %v", linkPath, err)
	}
	if target != hostDir {
		t.Errorf("symlink target = %q, want %q", target, hostDir)
	}
}

// TestNvmeof_DenyHost_LeavesHostDirIntact verifies that DenyHost removes the
// allowed_hosts symlink but leaves the hosts/<nqn>/ directory intact
// (other subsystems may still reference it).
//
//	Setup:   Apply; AllowHost; DenyHost same host
//	Expect:  allowed_hosts/<nqn> symlink gone; hosts/host-nqn/ dir still present
func TestNvmeof_DenyHost_LeavesHostDirIntact(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-deny-hosts-intact"
	hostNQN := "nqn.test:host-deny"

	tgt := defaultTarget(tmpdir, nqn)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	hostDir := filepath.Join(tmpdir, "nvmet", "hosts", hostNQN)
	requireDirExists(t, hostDir)

	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("DenyHost: %v", err)
	}

	// Symlink must be gone.
	linkPath := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts", hostNQN)
	requireNotExist(t, linkPath)

	// hosts/<nqn>/ dir must still exist.
	requireDirExists(t, hostDir)
}

// TestNvmeof_AllowHost_AllowedHostsDirCreated verifies that AllowHost creates
// the allowed_hosts/ directory inside the subsystem if it is absent.
//
//	Setup:   Apply; call AllowHost (allowed_hosts/ not pre-created)
//	Expect:  allowed_hosts/ dir exists under subsystem; symlink inside it
func TestNvmeof_AllowHost_AllowedHostsDirCreated(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-ah-dir-created"
	hostNQN := "nqn.test:host-ah"

	tgt := defaultTarget(tmpdir, nqn)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify allowed_hosts/ does not exist yet (Apply without AllowedHosts).
	ahDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "allowed_hosts")
	// allowed_hosts/ may or may not pre-exist; AllowHost must create it if absent.
	// Remove it if it was created by Apply to simulate the no-pre-existing state.
	_ = os.Remove(ahDir) //nolint:errcheck // best-effort; ignore error if it doesn't exist

	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	requireDirExists(t, ahDir)
	linkPath := filepath.Join(ahDir, hostNQN)
	if _, err := os.Lstat(linkPath); err != nil {
		t.Errorf("symlink %q not created: %v", linkPath, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 3.14 — ListExports Advanced Scanning
// ─────────────────────────────────────────────────────────────────────────────.

// TestNvmeof_ListExports_MultipleNamespaces verifies that a subsystem with
// two namespace directories reports both device paths in the scan result.
//
//	Setup:   Manually create subsystem dir with namespaces/1 and namespaces/2,
//	         each containing a device_path file
//	Expect:  ExportedSubsystem.NamespaceDevicePaths has 2 entries
func TestNvmeof_ListExports_MultipleNamespaces(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-multi-ns"

	subsDir := filepath.Join(tmpdir, "nvmet", "subsystems")
	subDir := filepath.Join(subsDir, nqn)

	for _, nsid := range []string{"1", "2"} {
		nsDir := filepath.Join(subDir, "namespaces", nsid)
		if err := os.MkdirAll(nsDir, 0o750); err != nil {
			t.Fatalf("mkdir namespace %s: %v", nsid, err)
		}
		devPath := fmt.Sprintf("/dev/zvol/tank/pvc-multi-ns-ns%s", nsid)
		if err := os.WriteFile(filepath.Join(nsDir, "device_path"), []byte(devPath), 0o600); err != nil {
			t.Fatalf("write device_path ns%s: %v", nsid, err)
		}
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}

	nsPaths := exports[0].NamespaceDevicePaths
	if len(nsPaths) != 2 {
		t.Errorf("NamespaceDevicePaths count = %d, want 2; entries: %v", len(nsPaths), nsPaths)
	}
	if nsPaths[1] != "/dev/zvol/tank/pvc-multi-ns-ns1" {
		t.Errorf("nsid=1 path = %q, want %q", nsPaths[1], "/dev/zvol/tank/pvc-multi-ns-ns1")
	}
	if nsPaths[2] != "/dev/zvol/tank/pvc-multi-ns-ns2" {
		t.Errorf("nsid=2 path = %q, want %q", nsPaths[2], "/dev/zvol/tank/pvc-multi-ns-ns2")
	}
}

// TestNvmeof_ListExports_DevicePathFileEmpty verifies that a namespace with an
// empty device_path file is handled without panic, returning an empty string.
//
//	Setup:   Apply NvmetTarget with DevicePath=""; call ListExports
//	Expect:  ListExports returns entry with blank DevicePath; no panic
func TestNvmeof_ListExports_DevicePathFileEmpty(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-empty-devpath"

	tgt := &nvmeof.NvmetTarget{
		ConfigfsRoot: tmpdir,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   "", // empty on purpose
		BindAddress:  extTestBindAddr,
		Port:         4420,
	}
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) == 0 {
		t.Fatal("expected at least 1 export, got 0")
	}
	// No panic; device path may be empty string or absent.
	nsPaths := exports[0].NamespaceDevicePaths
	if path, ok := nsPaths[1]; ok && path != "" {
		t.Errorf("nsid=1 device path = %q, want empty string", path)
	}
}

// TestNvmeof_ListExports_NQNFromDirName verifies that ListExports returns the
// exact NQN string taken from the subsystem directory name when no attr_name
// pseudo-file is present.
//
//	Setup:   Apply with NQN "nqn.test:pvc-unique"; call ListExports
//	Expect:  ExportedSubsystem.NQN == "nqn.test:pvc-unique"
func TestNvmeof_ListExports_NQNFromDirName(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-unique"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	exports, err := nvmeof.ListExports(tmpdir)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(exports) == 0 {
		t.Fatal("expected 1 export, got 0")
	}
	if got := exports[0].NQN; got != nqn {
		t.Errorf("NQN = %q, want %q", got, nqn)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Section 3.15 — Apply Field Verification
// ─────────────────────────────────────────────────────────────────────────────.

// TestNvmeof_Apply_DevicePathVerified verifies that the device_path file
// content exactly matches NvmetTarget.DevicePath.
//
//	Setup:   Apply with DevicePath="/dev/zvol/pool/pvc-abc"
//	Expect:  File at namespaces/1/device_path contains "/dev/zvol/pool/pvc-abc"
func TestNvmeof_Apply_DevicePathVerified(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-devpath-verified"
	wantDevPath := "/dev/zvol/pool/pvc-abc"

	tgt := nvmeTarget(tmpdir, nqn, wantDevPath, extTestBindAddr, 4420)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	nsDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "namespaces", "1")
	requireFileContent(t, filepath.Join(nsDir, "device_path"), wantDevPath)
}

// TestNvmeof_Apply_EnableFileContainsOne verifies that the enable file
// contains exactly "1" after a successful Apply.
//
//	Setup:   Apply NvmetTarget
//	Expect:  namespaces/1/enable file contains exactly "1"
func TestNvmeof_Apply_EnableFileContainsOne(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-enable-one"
	tgt := defaultTarget(tmpdir, nqn)

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	nsDir := filepath.Join(nvmetSubsystemDir(tmpdir, nqn), "namespaces", "1")
	requireFileContent(t, filepath.Join(nsDir, "enable"), "1")
}

// TestNvmeof_Apply_AddrTrsvcidMatchesPort verifies that the addr_trsvcid file
// content matches NvmetTarget.Port as a decimal string.
//
//	Setup:   Apply with Port=9500
//	Expect:  ports/<id>/addr_trsvcid file contains "9500"
func TestNvmeof_Apply_AddrTrsvcidMatchesPort(t *testing.T) {
	t.Parallel()
	tmpdir := t.TempDir()
	nqn := "nqn.test:pvc-port-9500"
	wantPort := int32(9500)

	tgt := nvmeTarget(tmpdir, nqn, "/dev/zvol/tank/pvc-port-9500", extTestBindAddr, wantPort)
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	portDir := requireSinglePort(t, tmpdir)
	requireFileContent(t, filepath.Join(portDir, "addr_trsvcid"), fmt.Sprintf("%d", wantPort))
}
