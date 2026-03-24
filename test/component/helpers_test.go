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

// Package component_test contains component-level tests for pillar-csi.
//
// This file (helpers_test.go) provides shared test infrastructure that is
// used across multiple component test files in this package.
//
// # Shared helpers provided here
//
//   - makeReadOnly / makeFileReadOnly — flip Unix permission bits to trigger
//     EACCES errors in configfs write paths, with automatic cleanup
//
// # Mock types defined in individual test files
//
// All mock implementations (mockVolumeBackend, csiMockAgent, csiMockConnector,
// csiMockMounter, seqExec, etc.) are defined in the test file that uses them
// most heavily.  Because all *_test.go files in this directory share the same
// package (component_test), those types are visible to every other test file
// without any additional imports.
//
// Shared constructor helpers (newAgentServer, nvmeofParams, exportVolume, …)
// follow the same convention.
package component_test

import (
	"os"
	"testing"
)

// makeReadOnly sets the directory at path to read-only mode (0555) and
// registers a t.Cleanup function to restore it to 0755.  Restoring the mode
// is necessary so that t.TempDir() can recursively remove the directory tree
// at the end of the test.
//
// The test is automatically skipped when running as root because the Linux
// kernel ignores DAC (Discretionary Access Control) checks for the root user,
// making permission-denied scenarios impossible to reproduce.
func makeReadOnly(t *testing.T, path string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("requires non-root: root bypasses Unix DAC permission checks")
	}
	if err := os.Chmod(path, 0o555); err != nil {
		t.Fatalf("makeReadOnly %q: %v", path, err)
	}
	t.Cleanup(func() {
		// Restore write permission so that t.TempDir() can clean up the tree.
		_ = os.Chmod(path, 0o755)
	})
}

// makeFileReadOnly sets the regular file at path to read-only mode (0444) and
// registers a t.Cleanup function to restore it to 0644.
//
// The test is automatically skipped when running as root for the same reason
// as makeReadOnly.
func makeFileReadOnly(t *testing.T, path string) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("requires non-root: root bypasses Unix DAC permission checks")
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("makeFileReadOnly %q: %v", path, err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(path, 0o644)
	})
}
