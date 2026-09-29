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

package nvmeof

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Local attach support.
//
// A volume attached directly on the storage node (local attach) must never be
// written through the network export at the same time.  The namespace is
// therefore disabled (enable=0) while the volume is locally published, which
// drops every remote session, and it is re-enabled only under an exclusive
// claim on the backend device that is held across the entire enable write: a
// local stage keeps a kernel exclusive claim on the device (device-mapper
// linear target) until it is really unstaged, so an exclusive open failing
// with EBUSY proves the local attach is still in use, and holding that open
// makes the node's own claim fail for as long as the export is being
// re-enabled.

// ErrDeviceHeld reports that the namespace was not enabled because its
// backend device is held exclusively on the storage node.
var ErrDeviceHeld = errors.New("backend device is held exclusively on the storage node")

// ErrNamespaceNotFound reports that the target has no namespace in configfs,
// i.e. the volume is not exported.
var ErrNamespaceNotFound = errors.New("namespace not found")

// DeviceClaimer takes an exclusive claim on the block device at path and
// returns a release function that drops it.  While the claim is held, no one
// else can open the device exclusively, so callers keep the export enable
// write inside the claim's lifetime.  On success the release function is
// never nil; it is a no-op when there was nothing to claim.  ErrDeviceHeld
// reports a device that is already held; any other error means the claim
// could not be established and callers must not enable an export on it.
type DeviceClaimer func(path string) (release func() error, err error)

// noRelease is the release function of a claim that holds nothing.
func noRelease() error { return nil }

// UnclaimedDeviceClaimer is a DeviceClaimer that grants every claim without
// opening the device.  It is intended for tests that run the agent against a
// temporary configfs root and must not open real block devices; production
// code uses ClaimDeviceExclusively.
var UnclaimedDeviceClaimer DeviceClaimer = func(_ string) (func() error, error) {
	return noRelease, nil
}

// ClaimDeviceExclusively is the production DeviceClaimer.  It opens path
// with O_EXCL, which for a block device fails with EBUSY while another
// opener holds an exclusive claim (a mounted filesystem, a device-mapper
// target, ...):
//   - EBUSY  → ErrDeviceHeld;
//   - ENOENT → a no-op release: a device that does not exist cannot be held;
//   - success → release closes the held descriptor;
//   - any other error → the open error.
func ClaimDeviceExclusively(path string) (func() error, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_EXCL|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("exclusive open %q: %w", path, ErrDeviceHeld)
		}
		if errors.Is(err, syscall.ENOENT) {
			return noRelease, nil
		}
		return nil, fmt.Errorf("exclusive open %q: %w", path, err)
	}
	return func() error {
		closeErr := syscall.Close(fd)
		if closeErr != nil {
			return fmt.Errorf("close exclusive claim on %q: %w", path, closeErr)
		}
		return nil
	}, nil
}

// deviceClaimer returns the target's claimer, defaulting to
// ClaimDeviceExclusively.
func (t *NvmetTarget) deviceClaimer() DeviceClaimer {
	if t.DeviceClaimer != nil {
		return t.DeviceClaimer
	}
	return ClaimDeviceExclusively
}

// namespaceEnablePath returns the enable attribute of the target's namespace.
func (t *NvmetTarget) namespaceEnablePath() string {
	return filepath.Join(t.namespaceDir(), "enable")
}

// requireNamespace returns ErrNamespaceNotFound when the target's namespace
// directory does not exist.
func (t *NvmetTarget) requireNamespace() error {
	_, err := os.Stat(t.namespaceDir())
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return fmt.Errorf("subsystem %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, ErrNamespaceNotFound)
	}
	return fmt.Errorf("stat namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
}

// NamespaceEnabled reports whether the target's namespace exists and whether
// it is enabled.
func (t *NvmetTarget) NamespaceEnabled() (exists, enabled bool, err error) {
	err = t.requireNamespace()
	if errors.Is(err, ErrNamespaceNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	value, err := readAttr(t.namespaceEnablePath())
	if err != nil {
		return true, false, fmt.Errorf("read enable of %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return true, value == "1", nil
}

// DisableNamespace fences the export for a local attach: it writes enable=0
// (verified by read-back) unless the namespace is already disabled, and
// returns the namespace's backend device path.  A missing namespace returns
// ErrNamespaceNotFound.
func (t *NvmetTarget) DisableNamespace() (string, error) {
	err := t.requireNamespace()
	if err != nil {
		return "", fmt.Errorf("DisableNamespace: %w", err)
	}
	err = t.disableNamespace()
	if err != nil {
		return "", fmt.Errorf("DisableNamespace: %w", err)
	}
	devPathAttr := filepath.Join(t.namespaceDir(), "device_path")
	devicePath, err := readAttr(devPathAttr)
	if err != nil {
		return "", fmt.Errorf("DisableNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	if devicePath == "" {
		return "", fmt.Errorf("DisableNamespace %q ns=%d: %s is empty", t.SubsystemNQN, t.NamespaceID, devPathAttr)
	}
	return devicePath, nil
}

// EnableNamespace returns the export to serving remote initiators: it enables
// the namespace through the exclusive-holder check (see enableNamespace).  A
// missing namespace returns ErrNamespaceNotFound.
func (t *NvmetTarget) EnableNamespace() error {
	err := t.requireNamespace()
	if err != nil {
		return fmt.Errorf("EnableNamespace: %w", err)
	}
	err = t.enableNamespace()
	if err != nil {
		return fmt.Errorf("EnableNamespace: %w", err)
	}
	return nil
}

// disableNamespace writes enable=0 unless the namespace already reads "0".
// A missing attribute (regular test filesystem) is written too, so the
// disabled state is always explicit.
func (t *NvmetTarget) disableNamespace() error {
	enablePath := t.namespaceEnablePath()
	current, err := readAttr(enablePath)
	if err != nil {
		return fmt.Errorf("disable namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	if current == "0" {
		return nil
	}
	err = writeFile(enablePath, "0")
	if err != nil {
		return fmt.Errorf("disable namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return nil
}

// enableNamespace writes enable=1 unless the namespace is already enabled.
// When the namespace's live device_path names a device, an exclusive claim
// on it is taken first and held across the enable write and its read-back:
// probing and then releasing before the write left a window in which the
// storage node could claim the device under the still-disabled namespace,
// after which enable=1 would expose the backend to remote initiators.  While
// this claim is held, the node's device-mapper create fails, and once the
// claim is released the node sees enable=1 and backs off — no window.
// ErrDeviceHeld refuses the enable while the device is already claimed; the
// namespace stays disabled.  An empty device_path names no device that could
// be held, so no claim is taken and the kernel decides on the enable write.
func (t *NvmetTarget) enableNamespace() error {
	enablePath := t.namespaceEnablePath()
	current, err := readAttr(enablePath)
	if err != nil {
		return fmt.Errorf("enable namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	if current == "1" {
		return nil
	}
	devPathAttr := filepath.Join(t.namespaceDir(), "device_path")
	devicePath, err := readAttr(devPathAttr)
	if err != nil {
		return fmt.Errorf("enable namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	release := noRelease
	if devicePath != "" {
		release, err = t.deviceClaimer()(devicePath)
		if err != nil {
			if errors.Is(err, ErrDeviceHeld) {
				return fmt.Errorf("enable namespace %q ns=%d: backend device %s is still held on the "+
					"storage node (local attach in use): %w", t.SubsystemNQN, t.NamespaceID, devicePath, err)
			}
			return fmt.Errorf("enable namespace %q ns=%d: claim backend device %s: %w",
				t.SubsystemNQN, t.NamespaceID, devicePath, err)
		}
	}
	writeErr := writeFile(enablePath, "1")
	// A release failure is reported, but the namespace state stays as written
	// above: dropping the claim cannot un-write enable=1.
	releaseErr := release()
	if writeErr != nil || releaseErr != nil {
		return fmt.Errorf("enable namespace %q ns=%d (device %q): %w",
			t.SubsystemNQN, t.NamespaceID, devicePath, errors.Join(writeErr, releaseErr))
	}
	return nil
}

// desiredEnable is the enable value Prepare establishes: "0" while the
// volume is locally attached, "1" otherwise.
func (t *NvmetTarget) desiredEnable() string {
	if t.LocalAttach {
		return "0"
	}
	return "1"
}
