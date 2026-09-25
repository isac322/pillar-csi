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

// Unit tests for KubeMounter.  The production mounter wraps
// k8s.io/utils/mount.SafeFormatAndMount; these tests back it with
// mount.FakeMounter so probe results (IsLikelyNotMountPoint) and umount
// outcomes can be scripted without touching the host filesystem.
//
// The matrix pins the teardown contract verified in the field during the
// NVMe controller-loss incident: an aborted XFS/EXT4 filesystem answers
// stat(2) with EIO, and unmount must proceed anyway instead of aborting on
// the probe — while every other probe failure still aborts the unmount.
//
// Run with:
//
//	go test ./internal/csi/ -v -run 'TestKubeMounter_'

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

// newFakeKubeMounter returns a KubeMounter backed by a mount.FakeMounter
// carrying the given in-memory mount table.
func newFakeKubeMounter(mountPoints []mount.MountPoint) (*KubeMounter, *mount.FakeMounter) {
	fake := mount.NewFakeMounter(mountPoints)
	if fake.MountCheckErrors == nil {
		fake.MountCheckErrors = map[string]error{}
	}
	km := &KubeMounter{
		inner: mount.SafeFormatAndMount{
			Interface: fake,
			Exec:      utilexec.New(),
		},
	}
	return km, fake
}

// statPathError builds the *os.PathError os.Stat returns for target, i.e.
// the exact shape mount.IsCorruptedMnt matches on.
func statPathError(target string, err error) error {
	return &os.PathError{Op: "stat", Path: target, Err: err}
}

// unmountLogCount returns how many unmount actions the fake recorded.
func unmountLogCount(f *mount.FakeMounter) int {
	count := 0
	for _, action := range f.GetLog() {
		if action.Action == mount.FakeActionUnmount {
			count++
		}
	}
	return count
}

// TestKubeMounter_Unmount_CorruptedMountEIO is the regression test for the
// kubelet teardown wedge: IsLikelyNotMountPoint fails with stat EIO (the
// signature of a filesystem in kernel shutdown after its block device was
// removed), yet the mount must still be unmounted.  Before the fix Unmount
// returned the EIO error without ever calling umount(8).
func TestKubeMounter_Unmount_CorruptedMountEIO(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "xfs"},
	})
	fake.MountCheckErrors[target] = statPathError(target, syscall.EIO)

	if err := km.Unmount(target); err != nil {
		t.Fatalf("Unmount on corrupted mount: %v", err)
	}
	if unmountLogCount(fake) != 1 {
		t.Errorf("umount attempts = %d, want 1", unmountLogCount(fake))
	}
	if len(fake.MountPoints) != 0 {
		t.Errorf("mount table after unmount = %+v, want empty", fake.MountPoints)
	}

	// A repeat call is an idempotent no-op success once the corrupted
	// mount is gone (the fake clears the probe error on successful unmount).
	if err := km.Unmount(target); err != nil {
		t.Fatalf("second Unmount after corrupted mount cleared: %v", err)
	}
}

// TestKubeMounter_Unmount_NormalMount verifies the happy path: a mounted
// target with a clean probe is unmounted exactly once.
func TestKubeMounter_Unmount_NormalMount(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "ext4"},
	})

	if err := km.Unmount(target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if unmountLogCount(fake) != 1 {
		t.Errorf("umount attempts = %d, want 1", unmountLogCount(fake))
	}
	if len(fake.MountPoints) != 0 {
		t.Errorf("mount table after unmount = %+v, want empty", fake.MountPoints)
	}
}

// TestKubeMounter_Unmount_NotMounted verifies idempotency on a path that
// exists but is not a mount point: success with no umount attempt.
func TestKubeMounter_Unmount_NotMounted(t *testing.T) {
	t.Parallel()

	km, fake := newFakeKubeMounter(nil)

	if err := km.Unmount(t.TempDir()); err != nil {
		t.Fatalf("Unmount on unmounted path: %v", err)
	}
	if unmountLogCount(fake) != 0 {
		t.Errorf("umount attempts = %d, want 0", unmountLogCount(fake))
	}
}

// TestKubeMounter_Unmount_MissingPath verifies idempotency on a target that
// no longer exists: ENOENT from the probe is a no-op success.
func TestKubeMounter_Unmount_MissingPath(t *testing.T) {
	t.Parallel()

	km, fake := newFakeKubeMounter(nil)

	if err := km.Unmount(t.TempDir() + "/gone"); err != nil {
		t.Fatalf("Unmount on missing path: %v", err)
	}
	if unmountLogCount(fake) != 0 {
		t.Errorf("umount attempts = %d, want 0", unmountLogCount(fake))
	}
}

// TestKubeMounter_Unmount_UnknownProbeErrorFails verifies that a probe
// failure which is neither ENOENT nor a corrupted-mount signature aborts
// the unmount: the error propagates and umount(8) is never invoked.  A
// failed probe must never be treated as "not mounted".
func TestKubeMounter_Unmount_UnknownProbeErrorFails(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "xfs"},
	})
	fake.MountCheckErrors[target] = statPathError(target, syscall.EFAULT)

	if err := km.Unmount(target); err == nil {
		t.Fatal("Unmount with unknown probe error: expected error, got nil")
	}
	if unmountLogCount(fake) != 0 {
		t.Errorf("umount attempts = %d, want 0", unmountLogCount(fake))
	}
	if len(fake.MountPoints) != 1 {
		t.Errorf("mount table after failed unmount = %+v, want unchanged", fake.MountPoints)
	}
}

// TestKubeMounter_Unmount_CorruptedProbePreservesUnmountFailure verifies the
// no-silent-failure contract on the corrupted path: when umount(8) itself
// fails, that failure is returned — the corrupted probe error is never
// reported as success.
func TestKubeMounter_Unmount_CorruptedProbePreservesUnmountFailure(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "xfs"},
	})
	fake.MountCheckErrors[target] = statPathError(target, syscall.EIO)
	attempts := 0
	fake.UnmountFunc = func(string) error {
		attempts++
		return fmt.Errorf("umount: %w", syscall.EBUSY)
	}

	err := km.Unmount(target)
	if err == nil {
		t.Fatal("Unmount with failing umount on corrupted mount: expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("umount attempts = %d, want 1", attempts)
	}
	if len(fake.MountPoints) != 1 {
		t.Errorf("mount table after failed unmount = %+v, want unchanged", fake.MountPoints)
	}
}

// TestKubeMounter_Unmount_EACCESProbeAttemptsUnmount pins the EACCES branch
// of mount.IsCorruptedMnt semantics: a permission-denied stat is a known
// signature of dead FUSE servers, so upstream escalates it to a real
// umount(8) attempt instead of failing on the probe.  The permission error
// is never swallowed — when the escalation itself is denied, that umount
// error is what the caller sees.
func TestKubeMounter_Unmount_EACCESProbeAttemptsUnmount(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "fuse"},
	})
	fake.MountCheckErrors[target] = statPathError(target, syscall.EACCES)
	attempts := 0
	fake.UnmountFunc = func(string) error {
		attempts++
		return fmt.Errorf("umount: %w", syscall.EPERM)
	}

	err := km.Unmount(target)
	if err == nil {
		t.Fatal("Unmount with EACCES probe and denied umount: expected error, got nil")
	}
	if attempts != 1 {
		t.Errorf("umount attempts = %d, want 1 (EACCES probe must escalate to umount)", attempts)
	}
}

// TestKubeMounter_Unmount_WrappedCorruptedProbeFails pins a subtle
// dependency: mount.IsCorruptedMnt type-switches on the error it is given
// and does NOT unwrap it.  Because Unmount checks the raw
// IsLikelyNotMountPoint result, a wrapped EIO stays a plain probe failure
// and aborts the unmount.  This guards against a well-meaning errors.As
// "improvement" silently unmounting targets whose probe error merely wraps
// a corrupted-mount errno.
func TestKubeMounter_Unmount_WrappedCorruptedProbeFails(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "xfs"},
	})
	fake.MountCheckErrors[target] = fmt.Errorf("probing %s: %w",
		target, statPathError(target, syscall.EIO))

	if err := km.Unmount(target); err == nil {
		t.Fatal("Unmount with wrapped EIO probe: expected error, got nil")
	}
	if unmountLogCount(fake) != 0 {
		t.Errorf("umount attempts = %d, want 0", unmountLogCount(fake))
	}
}

// TestKubeMounter_IsMounted_CorruptedProbeStaysStrict verifies that
// IsMounted does not adopt the corrupted-mount fast path: a stat EIO must
// surface as an error so NodeStageVolume/NodePublishVolume cannot mistake
// an aborted filesystem for a healthy staged mount (false-healthy) or for
// no mount at all (leak).
func TestKubeMounter_IsMounted_CorruptedProbeStaysStrict(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	km, fake := newFakeKubeMounter([]mount.MountPoint{
		{Device: "/dev/nvme0n1", Path: target, Type: "xfs"},
	})
	fake.MountCheckErrors[target] = statPathError(target, syscall.EIO)

	mounted, err := km.IsMounted(target)
	if err == nil {
		t.Fatal("IsMounted on corrupted mount: expected error, got nil")
	}
	if mounted {
		t.Error("IsMounted on corrupted mount = true, want false with error")
	}
}
