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
	"strconv"
	"strings"
)

// NVMeoFConnectOptions carries optional kernel fabrics tuning for a connect.
// A nil field is omitted from the connect string so the kernel default
// applies (ctrl_loss_tmo=600, reconnect_delay=10 on Linux).  Explicit values,
// including 0 and -1, are passed through verbatim.
type NVMeoFConnectOptions struct {
	// CtrlLossTmo maps to the ctrl_loss_tmo fabrics option (seconds).
	CtrlLossTmo *int32
	// ReconnectDelay maps to the reconnect_delay fabrics option (seconds).
	ReconnectDelay *int32
}

// ParseNVMeoFConnectOptions extracts the NVMe-oF fabrics tuning parameters
// that CreateVolume copied into the VolumeContext.  Absent or empty keys leave
// the option unset; a present value that is not a base-10 int32 is an error
// so a misconfigured timeout is never silently replaced by the kernel default.
func ParseNVMeoFConnectOptions(volCtx map[string]string) (NVMeoFConnectOptions, error) {
	var opts NVMeoFConnectOptions
	for _, f := range []struct {
		key string
		dst **int32
	}{
		{paramNVMeOFCtrlLossTmo, &opts.CtrlLossTmo},
		{paramNVMeOFReconnectDelay, &opts.ReconnectDelay},
	} {
		raw := volCtx[f.key]
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return NVMeoFConnectOptions{}, fmt.Errorf("parse %s=%q: %w", f.key, raw, err)
		}
		v32 := int32(v)
		*f.dst = &v32
	}
	return opts, nil
}

// AppendTo appends the set options to a fabrics connect string.
func (o NVMeoFConnectOptions) AppendTo(connectOpts string) string {
	if o.CtrlLossTmo != nil {
		connectOpts += ",ctrl_loss_tmo=" + strconv.Itoa(int(*o.CtrlLossTmo))
	}
	if o.ReconnectDelay != nil {
		connectOpts += ",reconnect_delay=" + strconv.Itoa(int(*o.ReconnectDelay))
	}
	return connectOpts
}

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
	// Production value: NvmeFabricsDevice.
	fabricsDev string
}

// NewNVMeoFConnector constructs a production-ready NVMeoFConnector.
func NewNVMeoFConnector() *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:  "/sys",
		fabricsDev: NvmeFabricsDevice,
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
// It is idempotent: if the subsystem NQN already has a live or reconnecting
// controller (see SubsystemHasActiveController) the method returns nil
// immediately.
//
// On a new connection it opens /dev/nvme-fabrics and writes:
//
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>[,ctrl_loss_tmo=N][,reconnect_delay=N]
//
// connectOpts only affect a new connection; an existing controller keeps the
// options it was created with.
func (c *NVMeoFConnector) Connect(
	_ context.Context,
	subsysNQN, trAddr, trSvcID string,
	connectOpts NVMeoFConnectOptions,
) error {
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

	opts := connectOpts.AppendTo(
		fmt.Sprintf("transport=tcp,traddr=%s,trsvcid=%s,nqn=%s", trAddr, trSvcID, subsysNQN))
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

// SubsystemHasActiveController reports whether the subsystem directory
// subsysPath (/sys/class/nvme-subsystem/<name>) holds at least one NVMe
// controller that still owns or is re-establishing the session.
//
// After ctrl_loss_tmo expires the kernel removes every controller, but the
// subsystem entry (with its subsysnqn) can linger; that subsystem must count
// as disconnected so a fresh fabrics connect is issued.  Controllers in
// "live", "connecting", "resetting", or "new" state count as active so a
// reconnecting session is never duplicated.  Controllers reporting "dead",
// "deleting", or "deleting (no IO)" are ignored.  A controller that vanishes
// between the directory scan and the state read (its sysfs link now dangles)
// is ignored too.  A controller that still exists but has no state attribute
// (absent on some kernels) counts as active.
func SubsystemHasActiveController(subsysPath string) (bool, error) {
	entries, err := os.ReadDir(subsysPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !IsNVMeControllerEntry(name) {
			continue
		}
		ctrlPath := filepath.Join(subsysPath, name)
		statePath := filepath.Join(ctrlPath, "state")
		state, readErr := os.ReadFile(statePath) //nolint:gosec // G304: sysfs path under connector-controlled root.
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return false, fmt.Errorf("read controller state %s: %w", statePath, readErr)
			}
			_, statErr := os.Stat(ctrlPath)
			if os.IsNotExist(statErr) {
				continue // removed while scanning (ctrl_loss_tmo teardown)
			}
			if statErr != nil {
				return false, fmt.Errorf("stat controller %s: %w", ctrlPath, statErr)
			}
			return true, nil
		}
		switch strings.TrimSpace(string(state)) {
		case "dead", "deleting", "deleting (no IO)":
			continue
		default:
			return true, nil
		}
	}
	return false, nil
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

// isConnected returns true when a subsystem with the given NQN exists in
// /sys/class/nvme-subsystem/ and has an active controller.
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
		if strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}
		active, activeErr := SubsystemHasActiveController(filepath.Join(subsysDir, entry.Name()))
		if activeErr != nil {
			return false, activeErr
		}
		if active {
			return true, nil
		}
	}
	return false, nil
}
