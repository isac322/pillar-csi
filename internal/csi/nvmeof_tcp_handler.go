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

// NVMeoFTCPHandler implements ProtocolHandler for the NVMe-oF TCP protocol.
//
// It wraps the existing NVMeoFConnector to provide the protocol-agnostic
// ProtocolHandler interface defined in protocol_handler.go (RFC §5.4.2).
//
// The three layers map as follows:
//   - Attach  → connector.Connect + device-path poll (transport setup, Layer 1)
//   - Detach  → connector.Disconnect (transport teardown, Layer 1)
//   - Rescan  → sysfs rescan_controller write (kernel-level rescan after resize)
//
// Callers should use NewNVMeoFTCPHandler to construct a production instance.
// For tests, use newNVMeoFTCPHandlerWithConnector to inject a fake connector.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NVMeoFProtocolState is the concrete ProtocolState produced by
// NVMeoFTCPHandler.Attach and consumed by NVMeoFTCPHandler.Detach/Rescan.
//
// It stores just enough information for teardown and rescan to reconstruct
// the NVMe-oF session identity without the original VolumeContext.
type NVMeoFProtocolState struct {
	// SubsysNQN is the NVMe Qualified Name of the connected subsystem.
	SubsysNQN string
	// Address is the IP address of the NVMe-oF TCP target.
	Address string
	// Port is the TCP port of the NVMe-oF TCP target.
	Port string
}

// ProtocolType satisfies the ProtocolState interface.
func (*NVMeoFProtocolState) ProtocolType() string { return protocolNVMeoFTCP }

// ─────────────────────────────────────────────────────────────────────────────
// NVMeoFTCPHandler
// ─────────────────────────────────────────────────────────────────────────────

// NVMeoFTCPHandler implements ProtocolHandler for NVMe-oF TCP.
//
// It holds a reference to an NVMeoFConnector that performs the actual kernel
// interactions (writes to /dev/nvme-fabrics, reads from /sys/class/nvme-subsystem/).
// This separation allows unit tests to inject a fake NVMeoFConnector without
// requiring kernel NVMe modules or root privileges.
type NVMeoFTCPHandler struct {
	// connector is the low-level NVMe-oF kernel interface.
	connector *NVMeoFConnector

	// pollTimeout is the maximum time Attach waits for the block device to
	// appear in sysfs after a successful Connect call.
	pollTimeout time.Duration

	// pollInterval is the sleep between successive GetDevicePath polls.
	pollInterval time.Duration
}

// Ensure NVMeoFTCPHandler satisfies the ProtocolHandler interface at compile time.
var _ ProtocolHandler = (*NVMeoFTCPHandler)(nil)

// NewNVMeoFTCPHandler constructs a production-ready NVMeoFTCPHandler.
// It uses the default poll timeout (30 s) and poll interval (500 ms) from
// the node package constants.
func NewNVMeoFTCPHandler() *NVMeoFTCPHandler {
	return &NVMeoFTCPHandler{
		connector:    NewNVMeoFConnector(),
		pollTimeout:  deviceWaitTimeout,
		pollInterval: devicePollInterval,
	}
}

// newNVMeoFTCPHandlerWithConnector constructs an NVMeoFTCPHandler with an
// injected NVMeoFConnector.  Use this in unit tests to supply a fake sysfs
// root and fabricsDev path without touching the real kernel interface.
func newNVMeoFTCPHandlerWithConnector(c *NVMeoFConnector) *NVMeoFTCPHandler {
	return &NVMeoFTCPHandler{
		connector:    c,
		pollTimeout:  deviceWaitTimeout,
		pollInterval: devicePollInterval,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Attach
// ─────────────────────────────────────────────────────────────────────────────

// Attach establishes the NVMe-oF TCP connection and waits for the block device
// to appear in sysfs.
//
// It performs two steps:
//  1. connector.Connect — writes the connect string to /dev/nvme-fabrics.
//     Idempotent: a no-op if the subsystem NQN is already connected.
//  2. Poll connector.GetDevicePath until the /dev/nvmeXnY path appears or
//     the poll timeout (30 s) is exceeded.
//
// On success it returns an AttachResult with DevicePath set to the block device
// path and State populated with NVMeoFProtocolState so that Detach/Rescan can
// identify the session later.
//
// AttachParams fields used by this handler:
//   - ConnectionID — the subsystem NQN (e.g. "nqn.2024-01.com.example:vol1")
//   - Address      — the NVMe-oF TCP target IP address
//   - Port         — the NVMe-oF TCP target port (e.g. "4420")
func (h *NVMeoFTCPHandler) Attach(ctx context.Context, params AttachParams) (*AttachResult, error) {
	subsysNQN := params.ConnectionID
	trAddr := params.Address
	trSvcID := params.Port

	if subsysNQN == "" {
		return nil, fmt.Errorf("nvmeof-tcp Attach: ConnectionID (subsystem NQN) is required")
	}
	if trAddr == "" {
		return nil, fmt.Errorf("nvmeof-tcp Attach: Address is required")
	}
	if trSvcID == "" {
		return nil, fmt.Errorf("nvmeof-tcp Attach: Port is required")
	}

	// Step 1: Connect to the NVMe-oF TCP target.
	err := h.connector.Connect(ctx, subsysNQN, trAddr, trSvcID)
	if err != nil {
		return nil, fmt.Errorf("nvmeof-tcp Attach: connect to %q at %s:%s: %w",
			subsysNQN, trAddr, trSvcID, err)
	}

	// Step 2: Poll for the block device path.
	// After Connect, the kernel enqueues a uevent that eventually creates the
	// /dev/nvmeXnY node; we poll until it appears or the deadline is exceeded.
	pollCtx, pollCancel := context.WithTimeout(ctx, h.pollTimeout)
	defer pollCancel()

	var devicePath string
	for {
		devPath, err := h.connector.GetDevicePath(pollCtx, subsysNQN)
		if err != nil {
			return nil, fmt.Errorf("nvmeof-tcp Attach: get device path for %q: %w", subsysNQN, err)
		}
		if devPath != "" {
			devicePath = devPath
			break
		}
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("nvmeof-tcp Attach: block device for NQN %q did not appear within %s",
				subsysNQN, h.pollTimeout)
		case <-time.After(h.pollInterval):
			// next iteration
		}
	}

	return &AttachResult{
		DevicePath: devicePath,
		State: &NVMeoFProtocolState{
			SubsysNQN: subsysNQN,
			Address:   trAddr,
			Port:      trSvcID,
		},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Detach
// ─────────────────────────────────────────────────────────────────────────────

// Detach tears down the NVMe-oF TCP connection identified by state.
//
// State must be a *NVMeoFProtocolState (or a value containing SubsysNQN).
// It calls connector.Disconnect, which is idempotent: disconnecting an NQN
// that is not currently connected is a no-op (returns nil).
func (h *NVMeoFTCPHandler) Detach(ctx context.Context, state ProtocolState) error {
	nvmeState, ok := state.(*NVMeoFProtocolState)
	if !ok || nvmeState == nil {
		return fmt.Errorf("nvmeof-tcp Detach: unexpected state type %T (want *NVMeoFProtocolState)", state)
	}
	if nvmeState.SubsysNQN == "" {
		return fmt.Errorf("nvmeof-tcp Detach: state has empty SubsysNQN")
	}

	disconnErr := h.connector.Disconnect(ctx, nvmeState.SubsysNQN)
	if disconnErr != nil {
		return fmt.Errorf("nvmeof-tcp Detach: disconnect %q: %w", nvmeState.SubsysNQN, disconnErr)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Rescan
// ─────────────────────────────────────────────────────────────────────────────

// Rescan triggers an online resize rescan for the NVMe-oF namespace associated
// with the given state.
//
// Implementation: it scans /sys/class/nvme-subsystem/ for the subsystem NQN,
// identifies namespace block devices (nvmeXnY entries), and writes "1" to
// each namespace's rescan_controller sysfs attribute.
//
// This is the kernel-native way to trigger an NVMe namespace resize after a
// ControllerExpandVolume call — equivalent to:
//
//	echo 1 > /sys/class/nvme/<ctrlName>/rescan_controller
//
// For file protocols the rescan is a no-op because the server-side resize is
// transparent to the client.  NVMeoFTCPHandler always performs the active
// rescan.
func (h *NVMeoFTCPHandler) Rescan(_ context.Context, state ProtocolState) error {
	nvmeState, ok := state.(*NVMeoFProtocolState)
	if !ok || nvmeState == nil {
		return fmt.Errorf("nvmeof-tcp Rescan: unexpected state type %T (want *NVMeoFProtocolState)", state)
	}
	if nvmeState.SubsysNQN == "" {
		return fmt.Errorf("nvmeof-tcp Rescan: state has empty SubsysNQN")
	}

	return h.rescanByNQN(nvmeState.SubsysNQN)
}

// rescanByNQN writes "1" to the rescan_controller sysfs attribute for all
// NVMe controllers associated with the given subsystem NQN.
func (h *NVMeoFTCPHandler) rescanByNQN(subsysNQN string) error {
	sysfsRoot := h.connector.sysfsRoot
	subsysDir := filepath.Join(sysfsRoot, "class", "nvme-subsystem")

	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no nvme-subsystem class — nothing to rescan
		}
		return fmt.Errorf("nvmeof-tcp Rescan: read %s: %w", subsysDir, err)
	}

	for _, entry := range entries {
		subsysPath := filepath.Join(subsysDir, entry.Name())
		nqnFile := filepath.Join(subsysPath, "subsysnqn")
		nqnBytes, readErr := os.ReadFile(nqnFile) //nolint:gosec
		if readErr != nil || strings.TrimSpace(string(nqnBytes)) != subsysNQN {
			continue
		}

		// Found matching subsystem — rescan each controller (nvmeX entries,
		// not namespace entries nvmeXnY).
		ctrlEntries, readErr := os.ReadDir(subsysPath)
		if readErr != nil {
			continue
		}
		for _, ctrlEntry := range ctrlEntries {
			name := ctrlEntry.Name()
			if !strings.HasPrefix(name, "nvme") {
				continue
			}
			// Skip namespace entries (nvmeXnY); only rescan controllers (nvmeX).
			suffix := strings.TrimPrefix(name, "nvme")
			if strings.ContainsRune(suffix, 'n') {
				continue
			}
			rescanPath := filepath.Join(sysfsRoot, "class", "nvme", name, "rescan_controller")
			// Best-effort: ignore individual write errors.
			_ = os.WriteFile(rescanPath, []byte("1"), 0o600) //nolint:errcheck
		}
		return nil
	}
	return nil
}
