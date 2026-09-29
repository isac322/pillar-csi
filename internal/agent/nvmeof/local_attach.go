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
// drops every remote session, and it is re-enabled only after verifying that
// nothing on the storage node holds the backend device exclusively: a local
// stage keeps a kernel exclusive claim on the device (device-mapper linear
// target) until it is really unstaged, so an exclusive open failing with EBUSY
// proves the local attach is still in use.

// ErrDeviceHeld reports that the namespace was not enabled because its
// backend device is held exclusively on the storage node.
var ErrDeviceHeld = errors.New("backend device is held exclusively on the storage node")

// ErrNamespaceNotFound reports that the target has no namespace in configfs,
// i.e. the volume is not exported.
var ErrNamespaceNotFound = errors.New("namespace not found")

// DeviceHeldProbe reports whether the block device at path is held open
// exclusively by someone else.  A non-nil error means the answer is unknown,
// and callers must not enable an export on it.
type DeviceHeldProbe func(path string) (held bool, err error)

// DeviceHeldExclusively is the production DeviceHeldProbe.  It opens path with
// O_EXCL, which for a block device fails with EBUSY while another opener holds
// an exclusive claim (a mounted filesystem, a device-mapper target, ...):
//   - EBUSY  → (true, nil);
//   - ENOENT → (false, nil): a device that does not exist cannot be held;
//   - success → the descriptor is closed and (false, nil) returned;
//   - any other error → (false, err).
func DeviceHeldExclusively(path string) (bool, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_EXCL|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return true, nil
		}
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, fmt.Errorf("exclusive open %q: %w", path, err)
	}
	err = syscall.Close(fd)
	if err != nil {
		return false, fmt.Errorf("close exclusive probe of %q: %w", path, err)
	}
	return false, nil
}

// deviceHeldProbe returns the target's probe, defaulting to
// DeviceHeldExclusively.
func (t *NvmetTarget) deviceHeldProbe() DeviceHeldProbe {
	if t.DeviceHeldProbe != nil {
		return t.DeviceHeldProbe
	}
	return DeviceHeldExclusively
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
// Before enabling it probes the namespace's live device_path and refuses with
// ErrDeviceHeld while the device is held exclusively (a local attach on the
// storage node still uses it).  The node plugin establishes its exclusive
// claim only after the namespace reads disabled, so the probe is repeated
// after the enable write: a claim established between the first probe and the
// write is caught and enable=0 is restored (fail closed), guaranteeing the
// export never stays enabled under a local holder.  Kernel serialization of
// the claim and the enable makes at least one side observe the other.
// An empty device_path names no device that could be held, so the probes are
// skipped and the kernel decides on the enable write.
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
	if devicePath != "" {
		held, probeErr := t.deviceHeldProbe()(devicePath)
		if probeErr != nil {
			return fmt.Errorf("enable namespace %q ns=%d: probe exclusive holder of %s: %w",
				t.SubsystemNQN, t.NamespaceID, devicePath, probeErr)
		}
		if held {
			return fmt.Errorf("enable namespace %q ns=%d: backend device %s is still held on the storage node "+
				"(local attach in use): %w", t.SubsystemNQN, t.NamespaceID, devicePath, ErrDeviceHeld)
		}
	}
	err = writeFile(enablePath, "1")
	if err != nil {
		return fmt.Errorf("enable namespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	if devicePath == "" {
		return nil
	}
	held, probeErr := t.deviceHeldProbe()(devicePath)
	if probeErr == nil && !held {
		return nil
	}
	// Fail closed: the namespace was already enabled above, so restore
	// enable=0 before reporting refusal; a disable failure is joined in.
	cause := ErrDeviceHeld
	detail := "backend device is still held on the storage node (local attach in use)"
	if probeErr != nil {
		cause = probeErr
		detail = "probe exclusive holder after enabling"
	}
	return fmt.Errorf("enable namespace %q ns=%d: %s: %s: %w",
		t.SubsystemNQN, t.NamespaceID, devicePath, detail, errors.Join(cause, t.disableNamespace()))
}

// desiredEnable is the enable value Prepare establishes: "0" while the
// volume is locally attached, "1" otherwise.
func (t *NvmetTarget) desiredEnable() string {
	if t.LocalAttach {
		return "0"
	}
	return "1"
}
