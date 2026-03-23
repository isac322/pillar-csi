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
	if err := os.Mkdir(dir, 0o755); err != nil {
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

// TestNvmetTargetPaths verifies that path-generation methods honour
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

// ─────────────────────────────────────────────────────────────────────────────
// test helpers
// ─────────────────────────────────────────────────────────────────────────────

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %q: got %q, want %q", path, got, want)
	}
}
