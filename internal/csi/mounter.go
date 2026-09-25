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
	"fmt"
	"os"

	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

// KubeMounter is the production Mounter implementation backed by
// k8s.io/utils/mount.SafeFormatAndMount.  It shells out to the host
// mount(8)/umount(8) binaries and uses blkid to detect existing
// filesystems before formatting, matching the behavior expected by all
// major Kubernetes CSI drivers.
//
// Use NewKubeMounter to construct a ready-to-use instance.
type KubeMounter struct {
	inner mount.SafeFormatAndMount
}

// NewKubeMounter returns a KubeMounter that delegates all privileged
// operations to the host kernel via k8s.io/utils/mount.
func NewKubeMounter() *KubeMounter {
	return &KubeMounter{
		inner: mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      utilexec.New(),
		},
	}
}

// FormatAndMount formats the block device at source with the given
// filesystem type (if it is not already formatted) and then mounts it at
// target.  The call is idempotent: if source is already formatted with
// fsType the format step is skipped.
func (m *KubeMounter) FormatAndMount(source, target, fsType string, options []string) error {
	err := m.inner.FormatAndMount(source, target, fsType, options)
	if err != nil {
		return fmt.Errorf("FormatAndMount %s → %s: %w", source, target, err)
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
	// IsLikelyNotMountPoint returns true when the path is NOT a mount point.
	notMnt, err := m.inner.IsLikelyNotMountPoint(target)
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
		return fmt.Errorf("IsLikelyNotMountPoint %s: %w", target, err)
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

// IsMounted returns true if target currently has an active mount.
//
// Unlike Unmount this probe stays strict: a corrupted mount (EIO on stat)
// is reported as an error rather than "mounted".  Reporting true would let
// NodeStageVolume/NodePublishVolume treat a dead filesystem as healthy;
// reporting false would let teardown paths skip the unmount and leak the
// mount.  Callers that only want to remove a mount must call the
// idempotent Unmount directly instead of gating on IsMounted.
func (m *KubeMounter) IsMounted(target string) (bool, error) {
	notMnt, err := m.inner.IsLikelyNotMountPoint(target)
	if err != nil {
		if isNotExistError(err) {
			return false, nil
		}
		return false, fmt.Errorf("IsLikelyNotMountPoint %s: %w", target, err)
	}
	return !notMnt, nil
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
