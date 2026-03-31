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

// Unit tests for ReadHostNQN / readHostNQNFrom.
//
// All tests use temporary files to avoid touching real system files.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestReadHostNQN

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadHostNQNFrom_HappyPath(t *testing.T) {
	const want = "nqn.2014-08.org.nvmexpress:uuid:test-host-nqn"

	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte(want+"\n"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	got, err := readHostNQNFrom(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHostNQNFrom_WhitespaceIsTrimmed(t *testing.T) {
	const want = "nqn.2014-08.org.nvmexpress:uuid:trimmed"

	f := filepath.Join(t.TempDir(), "hostnqn")
	// Write with leading/trailing whitespace including newlines and spaces.
	if err := os.WriteFile(f, []byte("  \n"+want+"\n  \n"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	got, err := readHostNQNFrom(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReadHostNQNFrom_MissingFile(t *testing.T) {
	_, err := readHostNQNFrom(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestReadHostNQNFrom_EmptyFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "hostnqn")
	if err := os.WriteFile(f, []byte("   \n  "), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	_, err := readHostNQNFrom(f)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}
