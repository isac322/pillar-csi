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
