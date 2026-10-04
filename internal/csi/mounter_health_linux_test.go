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

// Unit tests for KubeMounter.CheckMountHealth and KubeMounter.HasOtherMounts.
//
// CheckMountHealth issues a real O_TMPFILE syscall against t.TempDir(), so
// it runs on the host filesystem without root.  The dead-filesystem
// signature itself (XFS forced shutdown → EIO, ext4 remount-ro → EROFS) was
// reproduced with xfs_io -x shutdown on a live VM; the mapping from those
// errnos to ErrMountUnhealthy is pinned indirectly by the mock-based node
// tests.
//
// Run with:
//
//	go test ./internal/csi/ -v -run 'TestKubeMounter_(CheckMountHealth|HasOtherMounts)'

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKubeMounter_CheckMountHealth_WritableDir verifies that the health
// probe reports a writable filesystem directory as healthy and leaves no
// files behind (O_TMPFILE primary path, CreateTemp fallback alike).
func TestKubeMounter_CheckMountHealth_WritableDir(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km := NewKubeMounter()

	if err := km.CheckMountHealth(target); err != nil {
		t.Fatalf("CheckMountHealth on writable dir: %v", err)
	}

	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pillar-health-") {
			t.Errorf("health probe left file %q behind", e.Name())
		}
	}
}

// TestKubeMounter_CheckMountHealth_MissingDir verifies that a nonexistent
// target surfaces a plain probe error, not ErrMountUnhealthy: nothing about
// the filesystem's health can be concluded.
func TestKubeMounter_CheckMountHealth_MissingDir(t *testing.T) {
	t.Parallel()

	km := NewKubeMounter()
	err := km.CheckMountHealth(filepath.Join(t.TempDir(), "gone"))
	if err == nil {
		t.Fatal("CheckMountHealth on missing dir: expected error, got nil")
	}
	if errors.Is(err, ErrMountUnhealthy) {
		t.Error("missing dir must not be classified as a dead filesystem")
	}
}

// TestKubeMounter_CheckMountHealth_UnwritableDir verifies that a directory
// the probe cannot write (EACCES) is an inconclusive probe error — only
// EIO/EROFS map to ErrMountUnhealthy.
func TestKubeMounter_CheckMountHealth_UnwritableDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks are bypassed")
	}

	target := t.TempDir()
	if err := os.Chmod(target, 0o500); err != nil { //nolint:gosec // G302: a deliberately unwritable test fixture
		t.Fatalf("chmod: %v", err)
	}
	km := NewKubeMounter()

	err := km.CheckMountHealth(target)
	if err == nil {
		t.Fatal("CheckMountHealth on unwritable dir: expected error, got nil")
	}
	if errors.Is(err, ErrMountUnhealthy) {
		t.Error("EACCES probe failure must not be classified as a dead filesystem")
	}
}

// TestKubeMounter_HasOtherMounts_NotMountPoint verifies that probing a
// path that is not a mount point is an error, never a silent false: a
// failed check must not let callers drop a mount.
func TestKubeMounter_HasOtherMounts_NotMountPoint(t *testing.T) {
	t.Parallel()

	km := NewKubeMounter()
	others, err := km.HasOtherMounts(t.TempDir())
	if err == nil {
		t.Fatal("HasOtherMounts on a non-mount path: expected error, got nil")
	}
	if others {
		t.Error("non-mount path reported as having other mounts")
	}
}
