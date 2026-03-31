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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NVMeoFConnector is the production Connector implementation that uses the
// Linux /dev/nvme-fabrics kernel character device to manage NVMe-oF TCP
// connections.  It does NOT require nvme-cli — it speaks to the kernel
// NVMe-fabrics driver directly via the text-based write interface that has
// been available since Linux 4.15.
//
// Connect writes "transport=tcp,traddr=X,trsvcid=Y,nqn=Z" to /dev/nvme-fabrics.
// Disconnect writes "1" to each controller's delete_controller sysfs entry.
// GetDevicePath scans /sys/class/nvme-subsystem/ for the block device.
//
// Both Connect and Disconnect are idempotent:
//   - Connect on an already-connected NQN is a no-op (success).
//   - Disconnect on an NQN that is not connected is a no-op (success).
type NVMeoFConnector struct {
	// sysfsRoot is the root of the sysfs virtual filesystem.  In production
	// this is "/sys"; tests may override it to a tmpdir.
	sysfsRoot string

	// fabricsDev is the path to the NVMe-fabrics character device.
	// Production value: "/dev/nvme-fabrics".
	fabricsDev string
}

// NewNVMeoFConnector constructs a production-ready NVMeoFConnector.
func NewNVMeoFConnector() *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:  "/sys",
		fabricsDev: "/dev/nvme-fabrics",
	}
}

// Ensure NVMeoFConnector satisfies the Connector interface at compile time.
var _ Connector = (*NVMeoFConnector)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Connect
// ─────────────────────────────────────────────────────────────────────────────

// Connect establishes an NVMe-oF TCP connection to the given subsystem NQN
// at the given transport address (trAddr) and service ID (TCP port, trSvcID).
//
// It is idempotent: if the subsystem NQN is already connected (detected by
// scanning /sys/class/nvme-subsystem/) the method returns nil immediately.
//
// On a new connection it opens /dev/nvme-fabrics and writes:
//
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>
func (c *NVMeoFConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	already, err := c.isConnected(subsysNQN)
	if err != nil {
		return fmt.Errorf("nvmeof Connect: check existing connection for %q: %w", subsysNQN, err)
	}
	if already {
		return nil
	}

	f, err := os.OpenFile(c.fabricsDev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("nvmeof Connect: open %s: %w", c.fabricsDev, err)
	}
	defer f.Close() //nolint:errcheck

	opts := fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s", trAddr, trSvcID, subsysNQN)
	_, err = fmt.Fprintf(f, "%s\n", opts)
	if err != nil {
		return fmt.Errorf("nvmeof Connect: write to %s (nqn=%s): %w",
			c.fabricsDev, subsysNQN, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect
// ─────────────────────────────────────────────────────────────────────────────

// Disconnect tears down all NVMe-oF controllers associated with the given
// subsystem NQN by writing "1" to each controller's delete_controller sysfs
// entry.
//
// It is idempotent: if the NQN is not connected the method returns nil.
func (c *NVMeoFConnector) Disconnect(_ context.Context, subsysNQN string) error {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")
	var disconnectErr error

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no nvme-subsystem class — nothing to disconnect
		}
		return fmt.Errorf("nvmeof Disconnect: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		matches, readErr := SubsystemMatchesNQN(subsysDir, entry.Name(), subsysNQN)
		if readErr != nil {
			disconnectErr = errors.Join(disconnectErr, readErr)
			continue
		}
		if !matches {
			continue
		}
		disconnectErr = errors.Join(disconnectErr, DeleteSubsystemControllers(c.sysfsRoot, subsysDir, entry.Name()))
	}
	if disconnectErr != nil {
		return fmt.Errorf("nvmeof Disconnect: delete controllers: %w", disconnectErr)
	}
	return nil
}

// SubsystemMatchesNQN reads the subsysnqn sysfs file for the given subsystem
// entry and returns whether it matches the target NQN.
func SubsystemMatchesNQN(subsysDir, subsystemName, subsysNQN string) (bool, error) {
	nqnFile := filepath.Join(subsysDir, subsystemName, "subsysnqn")
	nqnBytes, err := os.ReadFile(nqnFile) //nolint:gosec // G304: sysfs path is built from connector-controlled roots.
	if err != nil {
		return false, fmt.Errorf("read subsystem NQN %s: %w", nqnFile, err)
	}
	return strings.TrimSpace(string(nqnBytes)) == subsysNQN, nil
}

// IsNVMeControllerEntry returns true for NVMe controller sysfs entries (nvmeX)
// and false for namespace entries (nvmeXnY).
func IsNVMeControllerEntry(name string) bool {
	if !strings.HasPrefix(name, "nvme") {
		return false
	}
	return !strings.ContainsRune(strings.TrimPrefix(name, "nvme"), 'n')
}

// DeleteSubsystemControllers writes "1" to each controller's delete_controller
// sysfs entry for the given subsystem. Errors are collected and returned.
func DeleteSubsystemControllers(sysfsRoot, subsysDir, subsystemName string) error {
	subsysPath := filepath.Join(subsysDir, subsystemName)
	ctrlEntries, err := os.ReadDir(subsysPath)
	if err != nil {
		return fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}

	var errs []error
	for _, ctrlEntry := range ctrlEntries {
		name := ctrlEntry.Name()
		if !IsNVMeControllerEntry(name) {
			continue
		}

		deletePath := filepath.Join(sysfsRoot, "class", "nvme", name, "delete_controller")
		writeErr := os.WriteFile(deletePath, []byte("1"), 0o600)
		if writeErr != nil {
			errs = append(errs, fmt.Errorf("write %s: %w", deletePath, writeErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete controllers for %s: %w", subsystemName, errors.Join(errs...))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDevicePath
// ─────────────────────────────────────────────────────────────────────────────

// GetDevicePath returns the /dev/nvmeXnY block-device path for the given
// subsystem NQN after a successful Connect call.
//
// Returns ("", nil) when the device is not yet visible in sysfs; callers
// should poll until a non-empty path is returned or a deadline is exceeded.
func (c *NVMeoFConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("GetDevicePath: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		subsysPath := filepath.Join(subsysDir, entry.Name())
		nqnFile := filepath.Join(subsysPath, "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil || strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}

		// Found matching subsystem — look for namespace block devices (nvmeXnY).
		nsEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			return "", fmt.Errorf("GetDevicePath: read subsystem dir %s: %w", subsysPath, readErr)
		}

		for _, nsEntry := range nsEntries {
			name := nsEntry.Name()
			if !strings.HasPrefix(name, "nvme") {
				continue
			}
			suffix := strings.TrimPrefix(name, "nvme")
			if strings.ContainsRune(suffix, 'n') {
				return "/dev/" + name, nil
			}
		}

		// Subsystem found but no namespace device visible yet.
		return "", nil
	}

	return "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// isConnected helper
// ─────────────────────────────────────────────────────────────────────────────

// isConnected returns true when the given NQN already has an entry in
// /sys/class/nvme-subsystem/.
func (c *NVMeoFConnector) isConnected(subsysNQN string) (bool, error) {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		nqnFile := filepath.Join(subsysDir, entry.Name(), "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil {
			continue
		}
		if strings.TrimSpace(string(nqnBytes)) == subsysNQN {
			return true, nil
		}
	}
	return false, nil
}
