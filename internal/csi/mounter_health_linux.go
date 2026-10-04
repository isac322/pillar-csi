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
	fd, err := open(target)
	switch {
	case err == nil:
		_ = unix.Close(fd) //nolint:errcheck // unnamed inode dies with the fd
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EISDIR):
		// O_TMPFILE unsupported on this filesystem — fall through to the
		// named-file probe below.
	default:
		return classifyProbeError(target, err)
	}

	name, err := create(target)
	if err != nil {
		return classifyProbeError(target, err)
	}
	rmErr := os.Remove(name)
	if rmErr != nil {
		// The probe write succeeded but left a stray file; report it rather
		// than leaving debris (AGENTS.md: no silent failures).  The write
		// itself was accepted, so this is never a dead-filesystem verdict.
		return fmt.Errorf("probe %q: remove health-check file: %w", target, rmErr)
	}
	return nil
}

// classifyProbeError wraps a failed probe write: EIO and EROFS are the
// kernel-shutdown signature (ErrMountUnhealthy); anything else is an
// inconclusive probe failure.
func classifyProbeError(target string, err error) error {
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.EROFS) {
		return fmt.Errorf("probe %q: %w: %w", target, ErrMountUnhealthy, err)
	}
	return fmt.Errorf("probe %q: %w", target, err)
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
	return hasOtherMounts(mounts, mountTablePath(target))
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

// MountSource returns the mountinfo source field of the mount at target —
// the device path (or pseudo source) the filesystem was mounted from.
// Stacked mounts resolve to the topmost entry, matching findMount.  An
// error is inconclusive: callers must not treat it as "no device".
func (*KubeMounter) MountSource(target string) (string, error) {
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		return "", err
	}
	me, ok := findMount(mounts, mountTablePath(target))
	if !ok {
		return "", fmt.Errorf("%q is not a mount point", filepath.Clean(target))
	}
	return me.Source, nil
}

// MountEntryExists reports whether target has a mount table entry in this
// mount namespace, consulting /proc/self/mountinfo only.  It never touches
// the mounted filesystem — unlike a stat-based check such as
// IsLikelyNotMountPoint — so a kernel-shutdown mount whose stat fails with
// EIO still answers true (issue #175): the mount table entry is what
// decides whether the dead mount can be probed and repaired.  Stacked
// mounts count once, matching findMount.
func (*KubeMounter) MountEntryExists(target string) (bool, error) {
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		return false, err
	}
	_, ok := findMount(mounts, mountTablePath(target))
	return ok, nil
}

// mountTablePath spells target the way /proc/self/mountinfo does: the
// kernel records mount points with every symlink resolved, so a kubelet
// root reached through a symlink (/var/lib/kubelet -> /data/kubelet) would
// otherwise never match its own mounts — a stat-based check follows
// symlinks, so the mount-table lookups must too.  Only the parent directory
// is resolved: the mount point itself may be a kernel-shutdown filesystem
// whose stat answers EIO, and kubelet never makes it a symlink.  When the
// parent cannot be resolved (it is itself a dead mount, or missing) the
// cleaned literal path is the best available spelling.
func mountTablePath(target string) string {
	clean := filepath.Clean(target)
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return clean
	}
	return filepath.Join(parent, filepath.Base(clean))
}
