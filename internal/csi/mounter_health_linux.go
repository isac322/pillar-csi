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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// CheckMountHealth verifies that the filesystem mounted at target can still
// commit an in-memory metadata write.
//
// Plain stat(2) and friends are useless for this: a kernel-shutdown XFS still
// answers statfs(2) and, on older kernels, even stat(2) and readdir from
// cached dentries, while bind-mounting it also succeeds on newer kernels.
// The only cheap, non-destructive probe that reliably distinguishes a dead
// filesystem is a metadata write that must reach the journal: the kernel
// rejects it with EIO (XFS shutdown) or EROFS (ext4 remount-ro) before any
// data is committed.  Verified on kernel 7.0 against xfs_io -x shutdown:
// stat→EIO, statfs→OK, access(W_OK)→OK, O_TMPFILE→EIO; and ext4 remount-ro:
// O_TMPFILE→EROFS.
//
// O_TMPFILE is preferred because the inode is never linked — the probe
// leaves no directory entry at all.  Filesystems that do not support
// O_TMPFILE fall back to a create+unlink of a hidden file, which leaves a
// short-lived entry only.
//
// Returns:
//   - nil — the filesystem accepted the probe write.
//   - ErrMountUnhealthy — EIO or EROFS: the mount is present but dead.
//   - any other error — the probe itself failed (permissions, missing path);
//     nothing can be concluded and the caller must surface it.
func (*KubeMounter) CheckMountHealth(target string) error {
	return checkMountHealth(target, openTmpfileProbe, createProbeFile)
}

// checkMountHealth is CheckMountHealth with the probe syscalls injected:
// open is the O_TMPFILE open, create the named-file fallback.  Unit tests
// feed kernel-shutdown errnos through them without root or a dead mount.
func checkMountHealth(
	target string, open func(string) (int, error), create func(string) (string, error),
) error {
	err := probeMountWritable(target, open, create)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EIO), errors.Is(err, unix.EROFS):
		return fmt.Errorf("probe %q: %w: %w", target, ErrMountUnhealthy, err)
	default:
		return fmt.Errorf("probe %q: %w", target, err)
	}
}

// openTmpfileProbe creates an unnamed inode in dir with O_TMPFILE: a
// metadata write that leaves no directory entry.  The x/sys/unix package is
// required: the deprecated syscall package encodes O_TMPFILE without
// O_DIRECTORY on arm64, which open(2) rejects with EINVAL on every
// filesystem.
func openTmpfileProbe(dir string) (int, error) {
	return unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR, 0o600) //nolint:wrapcheck // caller classifies the errno
}

// createProbeFile is the named-file fallback probe for filesystems that
// reject O_TMPFILE: it creates an empty hidden file in dir and returns its
// name for removal.
func createProbeFile(dir string) (string, error) {
	f, err := os.CreateTemp(dir, ".pillar-health-*")
	if err != nil {
		return "", err //nolint:wrapcheck // checkMountHealth wraps and classifies the errno
	}
	_ = f.Close() //nolint:errcheck // empty probe file; nothing to flush
	return f.Name(), nil
}

// probeMountWritable issues the metadata-write probe checkMountHealth
// classifies: the O_TMPFILE open, falling back to create+unlink of a hidden
// file on filesystems that reject O_TMPFILE.  The raw error is returned
// unwrapped so the caller can classify the errno.
func probeMountWritable(
	target string, open func(string) (int, error), create func(string) (string, error),
) error {
	fd, err := open(target)
	switch {
	case err == nil:
		_ = unix.Close(fd) //nolint:errcheck // unnamed inode dies with the fd
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EISDIR):
		// O_TMPFILE unsupported on this filesystem — fall through to the
		// named-file probe below.
	default:
		return err
	}

	name, err := create(target)
	if err != nil {
		return err
	}
	rmErr := os.Remove(name)
	if rmErr != nil {
		// The probe write succeeded but left a stray file; report it rather
		// than leaving debris (AGENTS.md: no silent failures).
		return fmt.Errorf("remove health-check file: %w", rmErr)
	}
	return nil
}

// HasOtherMounts reports whether the filesystem mounted at target is also
// attached to the mount namespace through a second mount: pod bind mounts of
// the staging path (including subdirectory binds) share the target's device
// number in /proc/self/mountinfo.
//
// The answer decides whether a dead staged filesystem can be re-staged in
// place: with no other mounts, unmounting target detaches the last reference
// and a fresh mount replays the journal; with other mounts still pinning the
// superblock, unmount+mount silently re-attaches the same dead filesystem
// (verified on kernel 7.0 — the re-mount "succeeds" and every probe still
// returns EIO), so repair must wait until teardown removes the binds.
//
// An error is inconclusive: callers must not drop a mount on a failed check.
func (*KubeMounter) HasOtherMounts(target string) (bool, error) {
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		return false, err
	}
	return hasOtherMounts(mounts, target)
}

// hasOtherMounts is HasOtherMounts over an already-parsed mount table: any
// entry at another mount point with target's device number counts, whatever
// its root — a bind of a subdirectory of the staged filesystem pins the
// superblock just like a bind of its root.
func hasOtherMounts(entries []mountInfoEntry, target string) (bool, error) {
	me, ok := findMount(entries, target)
	if !ok {
		return false, fmt.Errorf("%q is not a mount point", filepath.Clean(target))
	}
	for _, other := range entries {
		if other.Major == me.Major && other.Minor == me.Minor && other.MountPoint != me.MountPoint {
			return true, nil
		}
	}
	return false, nil
}
