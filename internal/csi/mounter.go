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
	"io"
	"os"
	"syscall"

	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

// xfsCompatProfile is the mkfs.xfs configuration file that pins the on-disk
// feature set of new XFS volumes.  The xfsprogs package ships one
// lts_<version>.conf per upstream LTS kernel with the features that kernel
// can mount, and keeps the files current: a feature that becomes a mkfs
// default later is added to the older profiles as disabled.  The node image
// gets them from xfsprogs-extra.
//
// Linux 5.15 is the oldest node kernel pillar-csi supports.  A volume moves
// between nodes, so its filesystem must be mountable by every supported
// kernel, not only by the kernel of the node that formats it; the mkfs.xfs
// defaults follow the newest kernel instead (xfsprogs 7.0 turns on the
// exchange-range and parent-pointer features, which need Linux 6.10 and 6.12).
const xfsCompatProfile = "/usr/share/xfsprogs/mkfs/lts_5.15.conf"

// ErrMountUnhealthy reports that a mounted filesystem can no longer serve
// I/O: the kernel has shut the filesystem down (XFS forced shutdown after a
// log I/O error, reported as EIO) or has aborted it read-only (ext4
// errors=remount-ro, reported as EROFS) while its mount table entry still
// looks perfectly mounted.
//
// Callers must test this sentinel with errors.Is before treating a
// CheckMountHealth error as "dead mount, recoverable by re-staging"; any
// other probe failure is inconclusive and must surface as an ordinary
// error.
var ErrMountUnhealthy = errors.New("mounted filesystem is dead")

// KubeMounter is the production Mounter implementation backed by
// k8s.io/utils/mount.SafeFormatAndMount.  It shells out to the host
// mount(8)/umount(8) binaries and uses blkid to detect existing
// filesystems before formatting, matching the behavior expected by all
// major Kubernetes CSI drivers.
//
// Use NewKubeMounter to construct a ready-to-use instance.
type KubeMounter struct {
	inner mount.SafeFormatAndMount
	// xfsProfile is the mkfs.xfs configuration file whose options new XFS
	// filesystems get by default (xfsCompatProfile).
	xfsProfile string
	// checkReadable verifies that a block device can be opened and read
	// before blkid's verdict on it is trusted (checkDeviceReadable).
	checkReadable func(source string) error
}

// NewKubeMounter returns a KubeMounter that delegates all privileged
// operations to k8s.io/utils/mount. The node image supplies mount.nfs;
// mount.New("") resolves the bundled mount utility and helper inside the
// pillar-node container, so hosts do not need nfs-utils installed.
func NewKubeMounter() *KubeMounter {
	return &KubeMounter{
		inner: mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      utilexec.New(),
		},
		xfsProfile:    xfsCompatProfile,
		checkReadable: checkDeviceReadable,
	}
}

// readProbeBytes is how much of a device checkDeviceReadable reads: the
// region holding the partition table and the ext4 and xfs superblocks.
const readProbeBytes = 64 << 10

// checkDeviceReadable opens the block device at source, requires a non-zero
// capacity and reads its first readProbeBytes.
func checkDeviceReadable(source string) error {
	f, err := os.Open(source) //nolint:gosec // G304: the staged block device path.
	if err != nil {
		return fmt.Errorf("device is not readable: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only descriptor; nothing to flush
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("device is not readable: read capacity: %w", err)
	}
	if size == 0 {
		return errors.New("device is not readable: it reports zero capacity")
	}
	buf := make([]byte, min(size, readProbeBytes))
	_, err = f.ReadAt(buf, 0)
	if err != nil {
		return fmt.Errorf("device is not readable: read the first %d bytes: %w", len(buf), err)
	}
	return nil
}

// Mount performs a plain mount of source at target with the given type and
// options.  Callers typically use this for bind mounts where the source
// block device is already formatted.
func (m *KubeMounter) Mount(source, target, fsType string, options []string) error {
	err := m.inner.Mount(source, target, fsType, options)
	if err != nil {
		return fmt.Errorf("mount %s → %s: %w", source, target, err)
	}
	return nil
}

// Unmount unmounts the filesystem mounted at target.  The call is
// idempotent: if target is not currently mounted (or no longer exists)
// the function returns nil.
//
// A probe error that identifies a corrupted mount point — e.g. EIO from
// stat(2) on a filesystem that entered kernel shutdown after its block
// device disappeared — is treated as "still mounted" rather than as a
// fatal failure, matching k8s.io/utils/mount.CleanupMountPoint.  The
// unmount is attempted directly; a genuine umount(8) failure (permission
// denied, target busy, transport gone) is still reported to the caller.
func (m *KubeMounter) Unmount(target string) error {
	// The accurate probe includes same-device bind mounts, which the cheap
	// device/inode heuristic cannot detect.
	notMnt, err := mount.IsNotMountPoint(m.inner.Interface, target)
	if err != nil {
		if isNotExistError(err) {
			// Path does not exist — nothing to unmount.
			return nil
		}
		if mount.IsCorruptedMnt(err) {
			// The mount table may be unreadable because the filesystem is
			// corrupted (aborted XFS/EXT4 after NVMe device loss, dead FUSE
			// server).  The mount object itself is still attached and must
			// be torn down; attempt the unmount instead of failing on the
			// probe.  IsCorruptedMnt matches EACCES too, which is
			// proportionate — the probe error is never swallowed, it merely
			// escalates to a real umount whose own failure is returned.
			unmountErr := m.inner.Unmount(target)
			if unmountErr != nil {
				return fmt.Errorf("unmount corrupted mountpoint %s: %w", target, unmountErr)
			}
			return nil
		}
		return fmt.Errorf("IsNotMountPoint %s: %w", target, err)
	}
	if notMnt {
		// Already unmounted (or never mounted).
		return nil
	}
	unmountErr := m.inner.Unmount(target)
	if unmountErr != nil {
		return fmt.Errorf("unmount %s: %w", target, unmountErr)
	}
	return nil
}

// CheckMountReadable stats target: nil when the kernel answers, an error
// wrapping ErrMountUnhealthy when the mounted filesystem is dead (EIO from
// a kernel-shutdown XFS or ext4, ESTALE/ENOTCONN from a lost NFS or FUSE
// server), and any other stat error as an inconclusive probe failure.  It
// never writes, so it is safe on mounts that are read-only by request.
func (*KubeMounter) CheckMountReadable(target string) error {
	_, err := os.Stat(target)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE) || errors.Is(err, syscall.ENOTCONN) {
		return fmt.Errorf("stat %s: %w: %w", target, ErrMountUnhealthy, err)
	}
	return fmt.Errorf("stat %s: %w", target, err)
}

// Compile-time check that KubeMounter satisfies the Mounter interface.
var _ Mounter = (*KubeMounter)(nil)

// isNotExistError returns true when err represents a "no such file or
// directory" condition from the OS.
func isNotExistError(err error) bool {
	if err == nil {
		return false
	}
	// os.IsNotExist handles both syscall.ENOENT and the wrapped form
	// returned by the mount helper.
	return os.IsNotExist(err)
}
