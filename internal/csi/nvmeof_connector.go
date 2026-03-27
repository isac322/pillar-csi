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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NVMeoFConnector is the production Connector implementation that uses the
// nvme-cli tool to manage NVMe-oF TCP connections on the worker node.
//
// Connect calls "nvme connect -t tcp -a <addr> -s <port> -n <nqn>".
// Disconnect calls "nvme disconnect -n <nqn>".
// GetDevicePath scans /sys/class/nvme-subsystem/ to find the /dev/nvmeXnY
// block-device path associated with a given subsystem NQN.
//
// Both Connect and Disconnect are idempotent:
//   - Connect on an already-connected NQN is a no-op (success).
//   - Disconnect on an NQN that is not connected is a no-op (success).
//
// NVMeoFConnector has no exported fields and is constructed via
// NewNVMeoFConnector. The zero value is not valid.
type NVMeoFConnector struct {
	// sysfsRoot is the root of the sysfs virtual filesystem.  In production
	// this is "/sys"; tests may override it to a tmpdir to avoid kernel
	// interaction.
	sysfsRoot string

	// execCommand is the function used to run external processes.  In
	// production this is defaultExecCommand (os/exec); tests may inject a
	// fake to avoid calling real nvme-cli.
	execCommand execCommandFunc
}

// execCommandFunc is the signature for running an external command and
// returning its combined stdout+stderr output and any error.
type execCommandFunc func(name string, args ...string) ([]byte, error)

// defaultExecCommand runs an external command with os/exec and returns the
// combined stdout+stderr output.
func defaultExecCommand(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return out, fmt.Errorf("exec %s: %w", name, err)
	}
	return out, nil
}

// NewNVMeoFConnector constructs a production-ready NVMeoFConnector.
// It uses /sys as the sysfs root and os/exec to invoke nvme-cli.
func NewNVMeoFConnector() *NVMeoFConnector {
	return &NVMeoFConnector{
		sysfsRoot:   "/sys",
		execCommand: defaultExecCommand,
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
// scanning /sys/class/nvme-subsystem/) the method returns nil immediately
// without invoking nvme-cli again.
//
// On a new connection it runs:
//
//	nvme connect -t tcp -a <trAddr> -s <trSvcID> -n <subsysNQN>
func (c *NVMeoFConnector) Connect(_ context.Context, subsysNQN, trAddr, trSvcID string) error {
	// Idempotency check: return early if already connected.
	already, err := c.isConnected(subsysNQN)
	if err != nil {
		return fmt.Errorf("nvmeof Connect: check existing connection for %q: %w", subsysNQN, err)
	}
	if already {
		return nil
	}

	out, cmdErr := c.execCommand("nvme", "connect",
		"-t", "tcp",
		"-a", trAddr,
		"-s", trSvcID,
		"-n", subsysNQN,
	)
	if cmdErr != nil {
		return fmt.Errorf("nvme connect -t tcp -a %s -s %s -n %s: %w: %s",
			trAddr, trSvcID, subsysNQN, cmdErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Disconnect
// ─────────────────────────────────────────────────────────────────────────────

// Disconnect tears down the NVMe-oF connection to the given subsystem NQN.
//
// It is idempotent: if the subsystem NQN is not currently connected (detected
// by scanning /sys/class/nvme-subsystem/) the method returns nil immediately
// without invoking nvme-cli.
//
// On an existing connection it runs:
//
//	nvme disconnect -n <subsysNQN>
func (c *NVMeoFConnector) Disconnect(_ context.Context, subsysNQN string) error {
	// Idempotency check: return early if not connected.
	already, err := c.isConnected(subsysNQN)
	if err != nil {
		return fmt.Errorf("nvmeof Disconnect: check existing connection for %q: %w", subsysNQN, err)
	}
	if !already {
		return nil
	}

	out, cmdErr := c.execCommand("nvme", "disconnect", "-n", subsysNQN)
	if cmdErr != nil {
		return fmt.Errorf("nvme disconnect -n %s: %w: %s",
			subsysNQN, cmdErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetDevicePath
// ─────────────────────────────────────────────────────────────────────────────

// GetDevicePath returns the /dev/nvmeXnY block-device path for the given
// subsystem NQN after a successful Connect call.
//
// It scans /sys/class/nvme-subsystem/ looking for a subsystem whose
// "subsysnqn" file matches subsysNQN, then finds the first namespace entry
// (nvmeXnY) within that subsystem directory and constructs the /dev/ path.
//
// Returns ("", nil) when the device is not yet visible in sysfs; callers
// should poll until a non-empty path is returned or a deadline is exceeded.
func (c *NVMeoFConnector) GetDevicePath(_ context.Context, subsysNQN string) (string, error) {
	subsysDir := filepath.Join(c.sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			// sysfs directory doesn't exist yet — device not present.
			return "", nil
		}
		return "", fmt.Errorf("GetDevicePath: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		subsysPath := filepath.Join(subsysDir, entry.Name())

		// Read the subsysnqn file to check if this is our subsystem.
		nqnFile := filepath.Join(subsysPath, "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil {
			// Skip entries without a subsysnqn file.
			continue
		}
		if strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}

		// Found the matching subsystem.  Look for namespace block devices
		// (nvmeXnY pattern) within the subsystem directory.
		nsEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			return "", fmt.Errorf("GetDevicePath: read subsystem dir %s: %w", subsysPath, readErr)
		}

		for _, nsEntry := range nsEntries {
			name := nsEntry.Name()
			// NVMe namespace block devices follow the pattern nvmeXnY (e.g. nvme0n1).
			// They contain at least one 'n' and start with "nvme" but are not
			// just controllers (which look like nvmeX without the nY suffix).
			if strings.HasPrefix(name, "nvme") && strings.Contains(name, "n") {
				// Verify it looks like nvmeXnY by checking both 'e' and 'n' are present
				// after "nvm" — simple check: controller names are nvme0, nvme1, etc.
				// Namespace names are nvme0n1, nvme0n2, etc.
				// A namespace name has the form nvme<ctrl>n<ns> so it has 'n' after 'e'.
				suffix := strings.TrimPrefix(name, "nvme")
				if strings.ContainsRune(suffix, 'n') {
					return "/dev/" + name, nil
				}
			}
		}

		// Subsystem found but no namespace device visible yet.
		return "", nil
	}

	// No matching subsystem found — not connected or device not yet visible.
	return "", nil
}

// ─────────────────────────────────────────────────────────────────────────────
// isConnected helper
// ─────────────────────────────────────────────────────────────────────────────

// isConnected returns true when the given NQN already has an entry in
// /sys/class/nvme-subsystem/, indicating an active NVMe-oF connection.
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
