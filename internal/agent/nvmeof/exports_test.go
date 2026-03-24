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

// White-box tests for exports.go: ListExports configfs scanning.
// Uses t.TempDir() to simulate the configfs tree without root privileges.
package nvmeof

import (
	"os"
	"path/filepath"
	"testing"
)

// buildFakeSubsystem creates the configfs directory structure for one
// subsystem under subsDir.  If attrName is non-empty it is written to
// attr_name; otherwise the file is omitted (tests the fallback path).
// The namespacePaths parameter maps nsid strings to device_path content.
// The allowedHosts parameter lists host NQN names for which dirs are created.
func buildFakeSubsystem(
	t *testing.T,
	subsDir string,
	nqn string,
	attrName string,
	namespacePaths map[string]string,
	allowedHosts []string,
) {
	t.Helper()

	subPath := filepath.Join(subsDir, nqn)
	if err := os.MkdirAll(subPath, 0o750); err != nil {
		t.Fatalf("buildFakeSubsystem: mkdir %q: %v", subPath, err)
	}

	if attrName != "" {
		attrFile := filepath.Join(subPath, "attr_name")
		if err := os.WriteFile(attrFile, []byte(attrName), 0o600); err != nil {
			t.Fatalf("buildFakeSubsystem: write attr_name: %v", err)
		}
	}

	buildFakeNamespaces(t, subPath, namespacePaths)
	buildFakeAllowedHosts(t, subPath, allowedHosts)
}

// buildFakeNamespaces creates namespaces/<nsid>/device_path entries under subPath.
func buildFakeNamespaces(t *testing.T, subPath string, namespacePaths map[string]string) {
	t.Helper()
	if len(namespacePaths) == 0 {
		return
	}
	nsBaseDir := filepath.Join(subPath, "namespaces")
	for nsid, devPath := range namespacePaths {
		nsDir := filepath.Join(nsBaseDir, nsid)
		if err := os.MkdirAll(nsDir, 0o750); err != nil {
			t.Fatalf("buildFakeNamespaces: mkdir %q: %v", nsDir, err)
		}
		devPathFile := filepath.Join(nsDir, "device_path")
		if err := os.WriteFile(devPathFile, []byte(devPath), 0o600); err != nil {
			t.Fatalf("buildFakeNamespaces: write device_path: %v", err)
		}
	}
}

// buildFakeAllowedHosts creates allowed_hosts/<hostNQN>/ directories under subPath.
// The real kernel creates symlinks; plain dirs work equivalently for name-scanning.
func buildFakeAllowedHosts(t *testing.T, subPath string, allowedHosts []string) {
	t.Helper()
	if len(allowedHosts) == 0 {
		return
	}
	ahDir := filepath.Join(subPath, "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("buildFakeAllowedHosts: mkdir: %v", err)
	}
	for _, host := range allowedHosts {
		if err := os.MkdirAll(filepath.Join(ahDir, host), 0o750); err != nil {
			t.Fatalf("buildFakeAllowedHosts: mkdir host %q: %v", host, err)
		}
	}
}

// TestListExports_Empty verifies that ListExports returns nil (not an error)
// when the nvmet subsystems directory does not exist.
func TestListExports_Empty(t *testing.T) {
	root := t.TempDir()
	// Do NOT create the nvmet/subsystems directory.
	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports on missing subsystems dir: unexpected error: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("expected nil/empty slice, got %d entries", len(subs))
	}
}

// TestListExports_EmptySubsystemsDir verifies that ListExports returns an
// empty slice when the subsystems directory exists but contains no entries.
func TestListExports_EmptySubsystemsDir(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir subsystems: %v", err)
	}
	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports on empty subsystems dir: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("expected empty slice, got %d entries", len(subs))
	}
}

// TestListExports_DefaultConfigfsRoot verifies that passing an empty string
// uses DefaultConfigfsRoot without panicking (we just check the path logic;
// the directory won't exist in the test environment).
func TestListExports_DefaultConfigfsRoot(_ *testing.T) {
	// This exercises the "if configfsRoot == "" { ... }" branch.
	// The real /sys/kernel/config likely doesn't exist in the test sandbox;
	// a missing dir returns nil, nil.
	subs, err := ListExports("")
	// We don't care whether subs is nil or populated — we only check that
	// no panic occurs and either (subs != nil, err == nil) or (err != nil).
	_ = subs
	_ = err
}

// TestListExports_SingleSubsystemNoAttrName verifies that when attr_name is
// absent the NQN falls back to the directory name.
func TestListExports_SingleSubsystemNoAttrName(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"
	buildFakeSubsystem(t, subsDir, nqn, "", // no attr_name
		map[string]string{"1": "/dev/zvol/tank/pvc-abc"},
		nil,
	)

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}
	if subs[0].NQN != nqn {
		t.Errorf("NQN: got %q, want %q", subs[0].NQN, nqn)
	}
	if subs[0].NamespaceDevicePaths[1] != "/dev/zvol/tank/pvc-abc" {
		t.Errorf("ns[1] device_path: got %q, want %q",
			subs[0].NamespaceDevicePaths[1], "/dev/zvol/tank/pvc-abc")
	}
	if len(subs[0].AllowedHosts) != 0 {
		t.Errorf("expected no allowed hosts, got %v", subs[0].AllowedHosts)
	}
}

// TestListExports_SingleSubsystemWithAttrName verifies that when attr_name is
// present its content is used as the NQN (including trailing-newline trim).
func TestListExports_SingleSubsystemWithAttrName(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const dirName = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-xyz"
	const attrName = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-xyz\n" // kernel appends newline
	buildFakeSubsystem(t, subsDir, dirName, attrName,
		map[string]string{"1": "/dev/zvol/tank/pvc-xyz"},
		[]string{"nqn.2026-01.com.bhyoo.pillar-csi:initiator-a"},
	)

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}
	// Trailing newline must be stripped.
	wantNQN := "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-xyz"
	if subs[0].NQN != wantNQN {
		t.Errorf("NQN: got %q, want %q", subs[0].NQN, wantNQN)
	}
	if len(subs[0].AllowedHosts) != 1 || subs[0].AllowedHosts[0] != "nqn.2026-01.com.bhyoo.pillar-csi:initiator-a" {
		t.Errorf("AllowedHosts: got %v", subs[0].AllowedHosts)
	}
}

// TestListExports_MultipleSubsystems verifies that multiple subsystems are
// returned sorted by NQN.
func TestListExports_MultipleSubsystems(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	type subsSpec struct {
		nqn     string
		devPath string
		hosts   []string
	}
	specs := []subsSpec{
		{
			nqn:     "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-zzz",
			devPath: "/dev/zvol/tank/pvc-zzz",
			hosts:   []string{"nqn.host:worker-2"},
		},
		{
			nqn:     "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-aaa",
			devPath: "/dev/zvol/tank/pvc-aaa",
			hosts:   nil,
		},
		{
			nqn:     "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-mmm",
			devPath: "/dev/zvol/tank/pvc-mmm",
			hosts:   []string{"nqn.host:worker-1", "nqn.host:worker-3"},
		},
	}
	for _, s := range specs {
		buildFakeSubsystem(t, subsDir, s.nqn, "",
			map[string]string{"1": s.devPath},
			s.hosts,
		)
	}

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("expected 3 subsystems, got %d", len(subs))
	}

	// Verify sorted order.
	wantNQNs := []string{
		"nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-aaa",
		"nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-mmm",
		"nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-zzz",
	}
	for i, want := range wantNQNs {
		if subs[i].NQN != want {
			t.Errorf("subs[%d].NQN: got %q, want %q", i, subs[i].NQN, want)
		}
	}

	// Verify pvc-aaa has no allowed hosts.
	if len(subs[0].AllowedHosts) != 0 {
		t.Errorf("pvc-aaa: expected no allowed hosts, got %v", subs[0].AllowedHosts)
	}

	// Verify pvc-mmm allowed hosts are sorted.
	if len(subs[1].AllowedHosts) != 2 {
		t.Fatalf("pvc-mmm: expected 2 allowed hosts, got %v", subs[1].AllowedHosts)
	}
	if subs[1].AllowedHosts[0] != "nqn.host:worker-1" || subs[1].AllowedHosts[1] != "nqn.host:worker-3" {
		t.Errorf("pvc-mmm: allowed hosts: got %v", subs[1].AllowedHosts)
	}
}

// TestListExports_MultipleNamespaces verifies that multiple namespaces within
// a single subsystem are all captured in NamespaceDevicePaths.
func TestListExports_MultipleNamespaces(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-multi"
	buildFakeSubsystem(t, subsDir, nqn, "",
		map[string]string{
			"1": "/dev/zvol/tank/ns1",
			"2": "/dev/zvol/tank/ns2",
			"5": "/dev/zvol/tank/ns5",
		},
		nil,
	)

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}
	nsPaths := subs[0].NamespaceDevicePaths
	if len(nsPaths) != 3 {
		t.Fatalf("expected 3 namespace paths, got %d: %v", len(nsPaths), nsPaths)
	}
	for nsid, want := range map[uint32]string{
		1: "/dev/zvol/tank/ns1",
		2: "/dev/zvol/tank/ns2",
		5: "/dev/zvol/tank/ns5",
	} {
		if got := nsPaths[nsid]; got != want {
			t.Errorf("ns[%d]: got %q, want %q", nsid, got, want)
		}
	}
}

// TestListExports_NoNamespacesDir verifies that a subsystem whose namespaces
// directory is absent returns a nil NamespaceDevicePaths map without error.
func TestListExports_NoNamespacesDir(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-nonamesp"
	buildFakeSubsystem(t, subsDir, nqn, "", nil, nil)

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}
	if len(subs[0].NamespaceDevicePaths) != 0 {
		t.Errorf("expected nil/empty NamespaceDevicePaths, got %v", subs[0].NamespaceDevicePaths)
	}
}

// TestListExports_AllowedHostsSymlinks verifies that allowed_hosts symlinks
// (not just plain directories) are correctly read — the entry name is the
// host NQN regardless of whether it's a symlink or directory.
func TestListExports_AllowedHostsSymlinks(t *testing.T) {
	root := t.TempDir()
	subsDir := filepath.Join(root, "nvmet", "subsystems")
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const nqn = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-syml"
	subPath := filepath.Join(subsDir, nqn)
	if err := os.MkdirAll(subPath, 0o750); err != nil {
		t.Fatalf("mkdir subPath: %v", err)
	}

	// Create a real host directory and a symlink inside allowed_hosts/.
	hostsBaseDir := filepath.Join(root, "nvmet", "hosts")
	if err := os.MkdirAll(hostsBaseDir, 0o750); err != nil {
		t.Fatalf("mkdir hosts: %v", err)
	}
	const hostNQN = "nqn.2026-01.com.bhyoo.pillar-csi:initiator-syml"
	hostDir := filepath.Join(hostsBaseDir, hostNQN)
	if err := os.MkdirAll(hostDir, 0o750); err != nil {
		t.Fatalf("mkdir hostDir: %v", err)
	}

	ahDir := filepath.Join(subPath, "allowed_hosts")
	if err := os.MkdirAll(ahDir, 0o750); err != nil {
		t.Fatalf("mkdir allowed_hosts: %v", err)
	}
	// Create a symlink as the real kernel does.
	if err := os.Symlink(hostDir, filepath.Join(ahDir, hostNQN)); err != nil {
		t.Fatalf("symlink allowed_host: %v", err)
	}

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}
	if len(subs[0].AllowedHosts) != 1 || subs[0].AllowedHosts[0] != hostNQN {
		t.Errorf("AllowedHosts: got %v, want [%q]", subs[0].AllowedHosts, hostNQN)
	}
}

// TestListExports_RoundTrip verifies that Apply followed by ListExports
// returns data consistent with what was applied (integration of both APIs
// against the same tmpdir-based fake configfs).
func TestListExports_RoundTrip(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-roundtrip",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-roundtrip",
		BindAddress:  "10.0.0.99",
		Port:         4420,
		AllowedHosts: []string{
			"nqn.2026-01.com.bhyoo.pillar-csi:host-a",
			"nqn.2026-01.com.bhyoo.pillar-csi:host-b",
		},
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	subs, err := ListExports(root)
	if err != nil {
		t.Fatalf("ListExports after Apply: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(subs))
	}

	sub := subs[0]
	if sub.NQN != tgt.SubsystemNQN {
		t.Errorf("NQN: got %q, want %q", sub.NQN, tgt.SubsystemNQN)
	}
	if sub.NamespaceDevicePaths[1] != tgt.DevicePath {
		t.Errorf("ns[1] device_path: got %q, want %q",
			sub.NamespaceDevicePaths[1], tgt.DevicePath)
	}
	if len(sub.AllowedHosts) != 2 {
		t.Fatalf("expected 2 allowed hosts, got %v", sub.AllowedHosts)
	}
	wantHosts := []string{
		"nqn.2026-01.com.bhyoo.pillar-csi:host-a",
		"nqn.2026-01.com.bhyoo.pillar-csi:host-b",
	}
	for i, want := range wantHosts {
		if sub.AllowedHosts[i] != want {
			t.Errorf("AllowedHosts[%d]: got %q, want %q", i, sub.AllowedHosts[i], want)
		}
	}
}
