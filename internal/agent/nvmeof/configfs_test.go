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

// White-box tests: same package gives direct access to unexported helpers.
package nvmeof

import (
	"os"
	"path/filepath"
	"testing"
)

// Each test uses t.TempDir() as a stand-in for the configfs mount so the
// tests run without root privileges and without touching the real kernel tree.

// TestWriteFile verifies that writeFile creates the file with the expected
// content and that a second call overwrites it.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attr_allow_any_host")

	if err := writeFile(path, "0"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	assertFileContent(t, path, "0")

	if err := writeFile(path, "1"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	assertFileContent(t, path, "1")
}

// TestMkdirAll verifies that mkdirAll creates nested directories.
func TestMkdirAll(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "nvmet", "subsystems", "nqn.test:vol-001", "namespaces", "1")

	if err := mkdirAll(target); err != nil {
		t.Fatalf("mkdirAll: %v", err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after mkdirAll: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected directory, got mode=%s", fi.Mode())
	}

	// Calling again must be idempotent.
	if err := mkdirAll(target); err != nil {
		t.Fatalf("second mkdirAll: %v", err)
	}
}

// TestSymlink verifies creation, idempotent re-creation, and conflict detection.
func TestSymlink(t *testing.T) {
	root := t.TempDir()
	oldname := filepath.Join(root, "target")
	newname := filepath.Join(root, "link")

	// Create target file so the symlink destination exists.
	if err := os.WriteFile(oldname, nil, 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}

	// First call — should succeed.
	if err := symlink(oldname, newname); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if dest, err := os.Readlink(newname); err != nil || dest != oldname {
		t.Fatalf("expected symlink → %q, got %q (%v)", oldname, dest, err)
	}

	// Second call with same args — idempotent, no error.
	if err := symlink(oldname, newname); err != nil {
		t.Fatalf("idempotent symlink: %v", err)
	}

	// Third call pointing to a different target — must return an error.
	other := filepath.Join(root, "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := symlink(other, newname); err == nil {
		t.Fatal("expected error when symlink points to different target, got nil")
	}
}

// TestRemoveSymlink verifies removal and idempotency.
func TestRemoveSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")

	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if err := removeSymlink(link); err != nil {
		t.Fatalf("removeSymlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("expected link to be gone, lstat err=%v", err)
	}

	// Calling again on a missing path must be idempotent.
	if err := removeSymlink(link); err != nil {
		t.Fatalf("idempotent removeSymlink: %v", err)
	}
}

// TestRemoveSymlinkNonSymlink ensures removeSymlink refuses to remove a plain
// file to guard against accidentally removing configfs directories.
func TestRemoveSymlinkNonSymlink(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	if err := removeSymlink(plain); err == nil {
		t.Fatal("expected error removing non-symlink, got nil")
	}
}

// TestRemoveDir verifies directory removal and idempotency.
func TestRemoveDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "namespace")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := removeDir(dir); err != nil {
		t.Fatalf("removeDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected dir to be gone, stat err=%v", err)
	}

	// Idempotent: should not fail on missing path.
	if err := removeDir(dir); err != nil {
		t.Fatalf("idempotent removeDir: %v", err)
	}
}

// TestNvmetTargetPaths verifies that path-generation methods honor
// ConfigfsRoot and produce the expected configfs layout.
func TestNvmetTargetPaths(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.test:vol-001",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-test",
		BindAddress:  "192.168.1.1",
		Port:         DefaultPort,
	}

	want := func(suffix string) string { return filepath.Join(root, suffix) }

	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "nvmetRoot",
			got:  tgt.nvmetRoot(),
			want: want("nvmet"),
		},
		{
			name: "subsystemDir",
			got:  tgt.subsystemDir(),
			want: want("nvmet/subsystems/nqn.test:vol-001"),
		},
		{
			name: "namespaceDir",
			got:  tgt.namespaceDir(),
			want: want("nvmet/subsystems/nqn.test:vol-001/namespaces/1"),
		},
		{
			name: "hostDir",
			got:  tgt.hostDir("nqn.host:node-a"),
			want: want("nvmet/hosts/nqn.host:node-a"),
		},
		{
			name: "allowedHostLink",
			got:  tgt.allowedHostLink("nqn.host:node-a"),
			want: want("nvmet/subsystems/nqn.test:vol-001/allowed_hosts/nqn.host:node-a"),
		},
		{
			name: "portDir",
			got:  tgt.portDir(1),
			want: want("nvmet/ports/1"),
		},
		{
			name: "portSubsystemLink",
			got:  tgt.portSubsystemLink(1),
			want: want("nvmet/ports/1/subsystems/nqn.test:vol-001"),
		},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n  got  %q\n  want %q", c.name, c.got, c.want)
		}
	}
}

// TestNvmetTargetDefaultConfigfsRoot verifies that an empty ConfigfsRoot falls
// back to DefaultConfigfsRoot.
func TestNvmetTargetDefaultConfigfsRoot(t *testing.T) {
	tgt := &NvmetTarget{
		SubsystemNQN: "nqn.test:vol-001",
		NamespaceID:  1,
	}
	want := filepath.Join(DefaultConfigfsRoot, "nvmet")
	if got := tgt.nvmetRoot(); got != want {
		t.Errorf("nvmetRoot with empty ConfigfsRoot: got %q, want %q", got, want)
	}
}

// Subsystem and namespace tests.

// TestCreateSubsystem verifies that createSubsystem creates the subsystem
// directory and sets attr_allow_any_host to "1".
func TestCreateSubsystem(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-abc123",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc123",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
	}

	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}

	// Subsystem directory must exist.
	subDir := tgt.subsystemDir()
	fi, err := os.Stat(subDir)
	if err != nil {
		t.Fatalf("stat subsystemDir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("subsystemDir is not a directory, mode=%s", fi.Mode())
	}

	// attr_allow_any_host must be "1".
	assertFileContent(t, filepath.Join(subDir, "attr_allow_any_host"), "1")

	// Calling again must be idempotent (no error).
	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("idempotent createSubsystem: %v", err)
	}
}

// TestCreateNamespace verifies that createNamespace creates the namespace
// directory and writes device_path and enable=1.
func TestCreateNamespace(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-abc123",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-abc123",
		BindAddress:  "10.0.0.1",
		Port:         DefaultPort,
	}

	// createSubsystem must be called first to create the subsystem directory.
	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}

	if err := tgt.createNamespace(); err != nil {
		t.Fatalf("createNamespace: %v", err)
	}

	// Namespace directory must exist.
	nsDir := tgt.namespaceDir()
	fi, err := os.Stat(nsDir)
	if err != nil {
		t.Fatalf("stat namespaceDir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("namespaceDir is not a directory, mode=%s", fi.Mode())
	}

	// device_path must contain the device path.
	assertFileContent(t, filepath.Join(nsDir, "device_path"), tgt.DevicePath)

	// enable must be "1".
	assertFileContent(t, filepath.Join(nsDir, "enable"), "1")

	// Calling again must be idempotent (no error, values overwritten with same content).
	if err := tgt.createNamespace(); err != nil {
		t.Fatalf("idempotent createNamespace: %v", err)
	}
	assertFileContent(t, filepath.Join(nsDir, "device_path"), tgt.DevicePath)
	assertFileContent(t, filepath.Join(nsDir, "enable"), "1")
}

// TestCreateSubsystemAndNamespaceNonDefaultNsid verifies that non-1 namespace
// IDs are handled correctly (the directory name must reflect the actual nsid).
func TestCreateSubsystemAndNamespaceNonDefaultNsid(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-xyz",
		NamespaceID:  7,
		DevicePath:   "/dev/zvol/tank/pvc-xyz",
		BindAddress:  "10.0.0.2",
		Port:         DefaultPort,
	}

	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}
	if err := tgt.createNamespace(); err != nil {
		t.Fatalf("createNamespace: %v", err)
	}

	nsDir := tgt.namespaceDir()
	// The leaf directory name must be "7".
	if filepath.Base(nsDir) != "7" {
		t.Errorf("expected namespace dir leaf to be %q, got %q", "7", filepath.Base(nsDir))
	}
	assertFileContent(t, filepath.Join(nsDir, "device_path"), "/dev/zvol/tank/pvc-xyz")
	assertFileContent(t, filepath.Join(nsDir, "enable"), "1")
}

// StablePortID tests.

func TestStablePortID(t *testing.T) {
	// Same input → same output (deterministic).
	id1 := stablePortID("192.168.1.10", 4420)
	id2 := stablePortID("192.168.1.10", 4420)
	if id1 != id2 {
		t.Errorf("same input produced different IDs: %d vs %d", id1, id2)
	}
	// Result must be in [1, 65535].
	if id1 < 1 || id1 > 65535 {
		t.Errorf("port ID %d out of range [1, 65535]", id1)
	}
	// Different address → (very likely) different ID.
	id3 := stablePortID("10.0.0.1", 4420)
	if id3 == id1 {
		t.Log("warning: hash collision (unlikely but possible)")
	}
}

// CreatePort tests.

func TestCreatePort(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.test:vol-001",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-test",
		BindAddress:  "192.168.1.10",
		Port:         4420,
	}

	portID, err := tgt.createPort()
	if err != nil {
		t.Fatalf("createPort: %v", err)
	}
	if portID < 1 || portID > 65535 {
		t.Fatalf("portID %d out of range", portID)
	}

	pDir := tgt.portDir(portID)
	assertFileContent(t, filepath.Join(pDir, "addr_trtype"), "tcp")
	assertFileContent(t, filepath.Join(pDir, "addr_adrfam"), "ipv4")
	assertFileContent(t, filepath.Join(pDir, "addr_traddr"), "192.168.1.10")
	assertFileContent(t, filepath.Join(pDir, "addr_trsvcid"), "4420")

	// Idempotent.
	portID2, err := tgt.createPort()
	if err != nil {
		t.Fatalf("idempotent createPort: %v", err)
	}
	if portID2 != portID {
		t.Errorf("idempotent portID changed: %d vs %d", portID, portID2)
	}
}

// Apply / Remove tests.

func TestApplyAndRemove(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-lifecycle",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-lifecycle",
		BindAddress:  "10.0.0.5",
		Port:         4420,
	}

	// Apply should create the full configfs tree.
	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Verify subsystem.
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "1")

	// Verify namespace.
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "device_path"), tgt.DevicePath)
	assertFileContent(t, filepath.Join(tgt.namespaceDir(), "enable"), "1")

	// Verify port.
	portID := stablePortID(tgt.BindAddress, tgt.Port)
	pDir := tgt.portDir(portID)
	assertFileContent(t, filepath.Join(pDir, "addr_traddr"), "10.0.0.5")

	// Verify subsystem linked to port.
	linkPath := tgt.portSubsystemLink(portID)
	dest, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink port-subsystem: %v", err)
	}
	if dest != tgt.subsystemDir() {
		t.Errorf("port-subsystem link: got %q, want %q", dest, tgt.subsystemDir())
	}

	// Apply again — idempotent.
	if err := tgt.Apply(); err != nil {
		t.Fatalf("idempotent Apply: %v", err)
	}

	// Remove should clean up.
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Subsystem directory should be gone.
	if _, err := os.Stat(tgt.subsystemDir()); !os.IsNotExist(err) {
		t.Errorf("subsystem dir still exists after Remove")
	}

	// Remove again — idempotent.
	if err := tgt.Remove(); err != nil {
		t.Fatalf("idempotent Remove: %v", err)
	}
}

func TestApplyWithACL(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-acl",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-acl",
		BindAddress:  "10.0.0.6",
		Port:         4420,
		AllowedHosts: []string{"nqn.host:node-a", "nqn.host:node-b"},
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply with ACL: %v", err)
	}

	// attr_allow_any_host should be "0" (ACL enabled).
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "0")

	// Allowed hosts symlinks should exist.
	for _, host := range tgt.AllowedHosts {
		linkPath := tgt.allowedHostLink(host)
		dest, err := os.Readlink(linkPath)
		if err != nil {
			t.Fatalf("readlink allowed_host %q: %v", host, err)
		}
		if dest != tgt.hostDir(host) {
			t.Errorf("allowed_host link for %q: got %q, want %q", host, dest, tgt.hostDir(host))
		}
	}

	// Remove should clean up ACL symlinks too.
	if err := tgt.Remove(); err != nil {
		t.Fatalf("Remove with ACL: %v", err)
	}
}

// AclEnabled tests.

// TestCreateSubsystem_AclEnabled_True verifies that createSubsystem writes
// attr_allow_any_host = "0" when AclEnabled is true (ACL enforced from the
// moment the subsystem is created, before any AllowHost call is made).
func TestCreateSubsystem_AclEnabled_True(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-acl-on",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-acl-on",
		BindAddress:  "10.0.0.8",
		Port:         DefaultPort,
		ACLEnabled:   true, // ACL enforced
	}

	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}

	// attr_allow_any_host must be "0" — no initiator can connect until
	// AllowHost is called.
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "0")
}

// TestCreateSubsystem_AclEnabled_False verifies that createSubsystem writes
// attr_allow_any_host = "1" when AclEnabled is false (open access; any
// initiator may connect without an explicit ACL entry).
func TestCreateSubsystem_AclEnabled_False(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-acl-off",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-acl-off",
		BindAddress:  "10.0.0.9",
		Port:         DefaultPort,
		ACLEnabled:   false, // allow any host
	}

	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}

	// attr_allow_any_host must be "1" — any initiator may connect.
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "1")
}

// TestApply_AclEnabled_NoAllowedHosts verifies that Apply with AclEnabled=true
// and an empty AllowedHosts slice still sets attr_allow_any_host = "0".
// This is the normal ExportVolume flow: ACL is enabled but no initiators are
// added yet (they are added later via AllowInitiator / AllowHost).
func TestApply_AclEnabled_NoAllowedHosts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-acl-nodelay",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-acl-nodelay",
		BindAddress:  "10.0.0.10",
		Port:         DefaultPort,
		ACLEnabled:   true,
		AllowedHosts: nil, // no hosts yet
	}

	if err := tgt.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// attr_allow_any_host should be "0" even though no hosts were added yet.
	assertFileContent(t, filepath.Join(tgt.subsystemDir(), "attr_allow_any_host"), "0")
}

// AllowHost / DenyHost tests.

func TestAllowAndDenyHost(t *testing.T) {
	root := t.TempDir()
	tgt := &NvmetTarget{
		ConfigfsRoot: root,
		SubsystemNQN: "nqn.2026-01.io.pillar-csi:pvc-host",
		NamespaceID:  1,
		DevicePath:   "/dev/zvol/tank/pvc-host",
		BindAddress:  "10.0.0.7",
		Port:         4420,
	}

	// Create subsystem first (AllowHost needs the subsystem dir).
	if err := tgt.createSubsystem(); err != nil {
		t.Fatalf("createSubsystem: %v", err)
	}

	hostNQN := "nqn.host:worker-1"

	// AllowHost
	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("AllowHost: %v", err)
	}

	// Host dir should exist.
	if _, err := os.Stat(tgt.hostDir(hostNQN)); err != nil {
		t.Fatalf("host dir missing after AllowHost: %v", err)
	}

	// Symlink should exist.
	linkPath := tgt.allowedHostLink(hostNQN)
	if _, err := os.Readlink(linkPath); err != nil {
		t.Fatalf("allowed_host symlink missing: %v", err)
	}

	// Idempotent AllowHost.
	if err := tgt.AllowHost(hostNQN); err != nil {
		t.Fatalf("idempotent AllowHost: %v", err)
	}

	// DenyHost
	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("DenyHost: %v", err)
	}

	// Symlink should be gone.
	if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
		t.Errorf("allowed_host symlink still exists after DenyHost")
	}

	// Host dir should still exist (shared across subsystems).
	if _, err := os.Stat(tgt.hostDir(hostNQN)); err != nil {
		t.Errorf("host dir removed by DenyHost — should be preserved")
	}

	// Idempotent DenyHost.
	if err := tgt.DenyHost(hostNQN); err != nil {
		t.Fatalf("idempotent DenyHost: %v", err)
	}
}

// Test helpers.

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: test helper reads from t.TempDir() paths only.
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %q: got %q, want %q", path, got, want)
	}
}
