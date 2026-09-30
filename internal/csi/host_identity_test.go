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

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestReadOrGenerateHostNQN_HappyPath(t *testing.T) {
	const want = "nqn.2014-08.org.nvmexpress:uuid:test-host-nqn"

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	got, err := readOrGenerateHostNQN(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadOrGenerateHostNQN_WhitespaceIsTrimmed(t *testing.T) {
	const want = "nqn.2014-08.org.nvmexpress:uuid:trimmed"

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("  \n"+want+"\n  \n"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	got, err := readOrGenerateHostNQN(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadOrGenerateHostNQN_MissingFileGenerates(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "nvme", "hostnqn")

	got, err := readOrGenerateHostNQN(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, hostNQNUUIDPrefix) {
		t.Errorf("generated NQN %q does not start with %q", got, hostNQNUUIDPrefix)
	}

	persisted, readErr := os.ReadFile(f) //nolint:gosec // f is a TempDir-scoped path
	if readErr != nil {
		t.Fatalf("generated file not persisted: %v", readErr)
	}
	if strings.TrimSpace(string(persisted)) != got {
		t.Errorf("persisted %q does not match returned %q", strings.TrimSpace(string(persisted)), got)
	}

	got2, err2 := readOrGenerateHostNQN(f)
	if err2 != nil {
		t.Fatalf("second read failed: %v", err2)
	}
	if got2 != got {
		t.Errorf("second read returned different value: %q vs %q", got2, got)
	}
}

func TestReadOrGenerateHostNQN_EmptyFileRegenerates(t *testing.T) {
	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("   \n  "), 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	got, err := readOrGenerateHostNQN(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, hostNQNUUIDPrefix) {
		t.Errorf("regenerated NQN %q does not start with %q", got, hostNQNUUIDPrefix)
	}
}

// TestReadInitiatorIQN_ParsesOpenISCSIFile verifies comments, blank lines and
// surrounding whitespace are ignored and the first InitiatorName wins, and
// that an existing file is never rewritten.
func TestReadInitiatorIQN_ParsesOpenISCSIFile(t *testing.T) {
	const want = "iqn.1993-08.org.debian:01:a1b2c3d4e5f6"
	content := "## DO NOT EDIT OR REMOVE THIS FILE!\n" +
		"# InitiatorName=iqn.2000-01.com.example:commented-out\n" +
		"\n" +
		"   InitiatorName=" + want + "  \r\n" +
		"InitiatorName=iqn.2000-01.com.example:second\n"
	f := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ReadInitiatorIQN(f)
	if err != nil {
		t.Fatalf("ReadInitiatorIQN: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if after := readTestFile(t, f); after != content {
		t.Errorf("existing file was modified:\n%s", after)
	}
}

// readTestFile returns the content of a test-owned file.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: test reads a file under t.TempDir().
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestReadInitiatorIQN_GeneratesAndPersists covers an absent file (parent
// directory created) and a file without an InitiatorName line: both get a
// generated pillar-csi IQN, persisted with mode 0644 and returned unchanged
// on the next read.
func TestReadInitiatorIQN_GeneratesAndPersists(t *testing.T) {
	for name, seed := range map[string]*string{
		"absent file":   nil,
		"comments only": new("# no name yet\n\nInitiatorName=\n"),
	} {
		t.Run(name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "iscsi", "initiatorname.iscsi")
			if seed != nil {
				seedTestFile(t, f, *seed)
			}
			assertGeneratedInitiatorIQN(t, f)
		})
	}
}

// seedTestFile writes content to path, creating its parent directory.
func seedTestFile(t *testing.T, path, content string) {
	t.Helper()
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// assertGeneratedInitiatorIQN checks that ReadInitiatorIQN generates a
// pillar-csi IQN at f, persists it with mode 0644 and returns it again on
// the next read.
func assertGeneratedInitiatorIQN(t *testing.T, f string) {
	t.Helper()
	generated := regexp.MustCompile(`^iqn\.2026-01\.com\.bhyoo\.pillar-csi:node\.[0-9a-f]{32}$`)
	got, err := ReadInitiatorIQN(f)
	if err != nil {
		t.Fatalf("ReadInitiatorIQN: %v", err)
	}
	if !generated.MatchString(got) {
		t.Fatalf("generated IQN %q does not match %s", got, generated)
	}
	if data := readTestFile(t, f); data != "InitiatorName="+got+"\n" {
		t.Errorf("persisted content = %q", data)
	}
	fi, err := os.Stat(f)
	if err != nil {
		t.Fatalf("stat persisted file: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", fi.Mode().Perm())
	}
	again, err := ReadInitiatorIQN(f)
	if err != nil || again != got {
		t.Errorf("second read = %q, %v; want %q (stable identity)", again, err, got)
	}
}

// TestReadInitiatorIQN_RejectsInvalidName verifies a malformed existing name
// is an error naming the file, and the file is left untouched.
func TestReadInitiatorIQN_RejectsInvalidName(t *testing.T) {
	for _, bad := range []string{"not-an-iqn", "iqn.2020.example:x", "iqn.2020-01.example:has space", "eui.123"} {
		f := filepath.Join(t.TempDir(), "initiatorname.iscsi")
		content := "InitiatorName=" + bad + "\n"
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadInitiatorIQN(f)
		if err == nil || !strings.Contains(err.Error(), f) {
			t.Errorf("%q: got %v, want error naming %s", bad, err, f)
		}
		if data := readTestFile(t, f); data != content {
			t.Errorf("%q: file rewritten to %q", bad, data)
		}
	}
	for _, good := range []string{
		"eui.02004567A425678D", "naa.52004567BA64678D", "iqn.2001-04.com.example:storage:disk2.sys1.xyz",
	} {
		if !validISCSIName(good) {
			t.Errorf("%q: valid iSCSI name rejected", good)
		}
	}
}
