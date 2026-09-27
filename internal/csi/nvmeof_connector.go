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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// NVMeoFConnectOptions carries optional kernel fabrics tuning for a connect.
// A nil field is omitted from the connect string so the kernel default
// applies (ctrl_loss_tmo=600, reconnect_delay=10, queue_size=128 on Linux).
// Explicit values, including 0 and -1 for the timeouts, are passed through
// verbatim.
type NVMeoFConnectOptions struct {
	// CtrlLossTmo maps to the ctrl_loss_tmo fabrics option (seconds).
	CtrlLossTmo *int32
	// ReconnectDelay maps to the reconnect_delay fabrics option (seconds).
	ReconnectDelay *int32
	// QueueSize maps to the queue_size fabrics option: the I/O queue depth
	// of every queue of the controller.
	QueueSize *int32
}

// Linux accepts a fabrics queue_size only within [NVMF_MIN_QUEUE_SIZE,
// NVMF_MAX_QUEUE_SIZE] (drivers/nvme/host/fabrics.h) and fails the whole
// connect with EINVAL otherwise.
const (
	minNVMeoFQueueSize = 16
	maxNVMeoFQueueSize = 1024
)

// ParseNVMeoFConnectOptions extracts the NVMe-oF fabrics tuning parameters
// that CreateVolume copied into the VolumeContext.  Absent or empty keys leave
// the option unset; a present value that is not a base-10 int32 is an error
// so a misconfigured timeout is never silently replaced by the kernel default.
// A queue size outside the kernel's accepted range is an error too, because
// the kernel would reject the connect.
func ParseNVMeoFConnectOptions(volCtx map[string]string) (NVMeoFConnectOptions, error) {
	var opts NVMeoFConnectOptions
	for _, f := range []struct {
		key string
		dst **int32
	}{
		{paramNVMeOFCtrlLossTmo, &opts.CtrlLossTmo},
		{paramNVMeOFReconnectDelay, &opts.ReconnectDelay},
		{paramNVMeOFMaxQueueSize, &opts.QueueSize},
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
	if q := opts.QueueSize; q != nil && (*q < minNVMeoFQueueSize || *q > maxNVMeoFQueueSize) {
		return NVMeoFConnectOptions{}, fmt.Errorf("parse %s=%d: queue size must be within [%d, %d]",
			paramNVMeOFMaxQueueSize, *q, minNVMeoFQueueSize, maxNVMeoFQueueSize)
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
	if o.QueueSize != nil {
		connectOpts += ",queue_size=" + strconv.Itoa(int(*o.QueueSize))
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
// Disconnect writes "1" to each controller's delete_controller sysfs entry and
// waits (bounded) until each controller has left sysfs.
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

	// removalWait bounds how long Disconnect waits for each deleted
	// controller to leave sysfs.  The zero value selects
	// DefaultControllerRemovalWait.
	removalWait ControllerRemovalWait
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
//	transport=tcp,traddr=<trAddr>,trsvcid=<trSvcID>,nqn=<subsysNQN>[,ctrl_loss_tmo=N][,reconnect_delay=N][,queue_size=N]
//
// connectOpts only affect a new connection; an existing controller keeps the
// options it was created with.
//
// Before a new connection it waits (bounded) for any controller of the same
// NQN that the kernel is still deleting (see WaitForDyingControllers).
func (c *NVMeoFConnector) Connect(
	ctx context.Context,
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
	err = WaitForDyingControllers(ctx, c.sysfsRoot, subsysNQN, c.removalWait)
	if err != nil {
		return fmt.Errorf("nvmeof Connect: %w", err)
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
// subsystem NQN and returns only after each one has left sysfs (see
// DisconnectSubsystem).
//
// It is idempotent: if the NQN is not connected the method returns nil.
func (c *NVMeoFConnector) Disconnect(ctx context.Context, subsysNQN string) error {
	return DisconnectSubsystem(ctx, c.sysfsRoot, subsysNQN, c.removalWait)
}

// DisconnectSubsystem deletes every controller of each
// /sys/class/nvme-subsystem entry whose subsysnqn matches subsysNQN and waits
// until each controller has left sysfs (see DeleteSubsystemControllers).
// A missing nvme-subsystem class means nothing is connected and returns nil.
func DisconnectSubsystem(ctx context.Context, sysfsRoot, subsysNQN string, wait ControllerRemovalWait) error {
	err := forEachMatchingSubsystem(sysfsRoot, subsysNQN, func(subsysDir, name string) error {
		return DeleteSubsystemControllers(ctx, sysfsRoot, subsysDir, name, wait)
	})
	if err != nil {
		return fmt.Errorf("nvmeof disconnect %q: %w", subsysNQN, err)
	}
	return nil
}

// WaitForDyingControllers waits (bounded) until every controller of a
// subsystem matching subsysNQN whose state is "dead", "deleting" or
// "deleting (no IO)" has left sysfs.  Call it before a fresh connect: a
// deletion the kernel started on its own (ctrl_loss_tmo expiry, a DNR
// status) finishes asynchronously, and until it does a new connection can
// pick up the dying controller's namespace.
func WaitForDyingControllers(ctx context.Context, sysfsRoot, subsysNQN string, wait ControllerRemovalWait) error {
	err := forEachMatchingSubsystem(sysfsRoot, subsysNQN, func(subsysDir, name string) error {
		return waitSubsystemDyingControllers(ctx, filepath.Join(subsysDir, name), wait)
	})
	if err != nil {
		return fmt.Errorf("wait for dying controllers of %q: %w", subsysNQN, err)
	}
	return nil
}

// waitSubsystemDyingControllers waits for each dying controller linked from
// subsysPath to leave sysfs.
func waitSubsystemDyingControllers(ctx context.Context, subsysPath string, wait ControllerRemovalWait) error {
	ctrlEntries, err := os.ReadDir(subsysPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // subsystem destroyed with its last controller
		}
		return fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}
	var errs []error
	for _, ctrlEntry := range ctrlEntries {
		if !IsNVMeControllerEntry(ctrlEntry.Name()) {
			continue
		}
		ctrlPath := filepath.Join(subsysPath, ctrlEntry.Name())
		statePath := filepath.Join(ctrlPath, "state")
		state, readErr := os.ReadFile(statePath) //nolint:gosec // G304: sysfs path under connector-controlled root.
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // controller already gone (dangling link) or no state attribute
			}
			errs = append(errs, fmt.Errorf("read controller state %s: %w", statePath, readErr))
			continue
		}
		if !isDyingControllerState(strings.TrimSpace(string(state))) {
			continue
		}
		waitErr := waitControllerRemoved(ctx, ctrlPath, wait)
		if waitErr != nil {
			errs = append(errs, waitErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("subsystem %s: %w", subsysPath, errors.Join(errs...))
	}
	return nil
}

// forEachMatchingSubsystem calls fn for each /sys/class/nvme-subsystem entry
// whose subsysnqn equals subsysNQN and joins the errors.  An entry that
// vanishes while being read belongs to a subsystem the kernel just destroyed
// (another volume's teardown), so it cannot be ours and is skipped.
func forEachMatchingSubsystem(sysfsRoot, subsysNQN string, fn func(subsysDir, name string) error) error {
	subsysDir := filepath.Join(sysfsRoot, "class", "nvme-subsystem")
	entries, err := os.ReadDir(subsysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no nvme-subsystem class — nothing connected
		}
		return fmt.Errorf("read %s: %w", subsysDir, err)
	}

	var errs []error
	for _, entry := range entries {
		matches, readErr := SubsystemMatchesNQN(subsysDir, entry.Name(), subsysNQN)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) || errors.Is(readErr, syscall.ENODEV) {
				continue
			}
			errs = append(errs, readErr)
			continue
		}
		if !matches {
			continue
		}
		fnErr := fn(subsysDir, entry.Name())
		if fnErr != nil {
			errs = append(errs, fnErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scan %s: %w", subsysDir, errors.Join(errs...))
	}
	return nil
}

// isDyingControllerState reports whether a controller sysfs state means the
// kernel is tearing the controller down and it will never serve I/O again.
func isDyingControllerState(state string) bool {
	switch state {
	case "dead", "deleting", "deleting (no IO)":
		return true
	default:
		return false
	}
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
		if isDyingControllerState(strings.TrimSpace(string(state))) {
			continue
		}
		return true, nil
	}
	return false, nil
}

// ErrControllerNotRemoved reports that a controller was still present in
// sysfs when the bounded removal wait expired.
var ErrControllerNotRemoved = errors.New("nvme controller still present after delete_controller")

// ControllerRemovalWait bounds the wait for a deleted controller to leave
// sysfs.  Zero fields select the DefaultControllerRemovalWait values.
type ControllerRemovalWait struct {
	Timeout      time.Duration
	PollInterval time.Duration
}

// DefaultControllerRemovalWait keeps the wait well inside kubelet's CSI call
// timeout so a stuck teardown surfaces as this error, not a gRPC deadline.
var DefaultControllerRemovalWait = ControllerRemovalWait{
	Timeout:      10 * time.Second,
	PollInterval: 50 * time.Millisecond,
}

func (w ControllerRemovalWait) withDefaults() ControllerRemovalWait {
	if w.Timeout <= 0 {
		w.Timeout = DefaultControllerRemovalWait.Timeout
	}
	if w.PollInterval <= 0 {
		w.PollInterval = DefaultControllerRemovalWait.PollInterval
	}
	return w
}

// DeleteSubsystemControllers writes "1" to each controller's delete_controller
// sysfs entry for the given subsystem and waits until the controller has left
// the subsystem.  Errors are collected and returned.
//
// The write alone does not prove the controller is gone.  The kernel's
// nvme_sysfs_delete runs the teardown synchronously only for the caller that
// moves the controller to DELETING; when a kernel path (ctrl_loss_tmo expiry,
// a DNR status) already started the deletion, the write returns success at
// once while nvme_delete_wq is still tearing the controller down.  A re-stage
// in that window would connect next to a dying controller and could pick up
// its namespace, so every delete is followed by a read-back wait for
// <subsystem>/<ctrl> to stop resolving.
func DeleteSubsystemControllers(
	ctx context.Context,
	sysfsRoot, subsysDir, subsystemName string,
	wait ControllerRemovalWait,
) error {
	subsysPath := filepath.Join(subsysDir, subsystemName)
	ctrlEntries, err := os.ReadDir(subsysPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // subsystem destroyed with its last controller
		}
		return fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}

	var errs []error
	for _, ctrlEntry := range ctrlEntries {
		name := ctrlEntry.Name()
		if !IsNVMeControllerEntry(name) {
			continue
		}
		deleteErr := deleteControllerAndWait(ctx, sysfsRoot, subsysPath, name, wait)
		if deleteErr != nil {
			errs = append(errs, deleteErr)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete controllers for %s: %w", subsystemName, errors.Join(errs...))
	}
	return nil
}

// deleteControllerAndWait requests deletion of controller name and waits
// until subsysPath/name no longer resolves.  ENOENT from the write means the
// controller (or its attribute) is already gone, which is still confirmed by
// the wait; any other write error is returned without waiting because no
// deletion was started.
func deleteControllerAndWait(
	ctx context.Context,
	sysfsRoot, subsysPath, name string,
	wait ControllerRemovalWait,
) error {
	deletePath := filepath.Join(sysfsRoot, "class", "nvme", name, "delete_controller")
	writeErr := writeSysfsAttr(deletePath, "1")
	if writeErr != nil && !errors.Is(writeErr, fs.ErrNotExist) {
		return writeErr
	}
	return waitControllerRemoved(ctx, filepath.Join(subsysPath, name), wait)
}

// writeSysfsAttr writes value to an existing sysfs attribute.  It never
// creates the file: on sysfs a vanished attribute opened with O_CREAT fails
// with EACCES, while opening it without O_CREAT reports ENOENT.
func writeSysfsAttr(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: sysfs path under connector-controlled root.
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, writeErr := f.WriteString(value)
	closeErr := f.Close()
	joined := errors.Join(writeErr, closeErr)
	if joined != nil {
		return fmt.Errorf("write %s: %w", path, joined)
	}
	return nil
}

// waitControllerRemoved polls until ctrlPath (the subsystem's link to the
// controller) no longer resolves: the kernel removes the controller device
// in nvme_uninit_ctrl, after its namespaces, so a dangling or removed link
// means the controller and its namespaces are gone.
func waitControllerRemoved(ctx context.Context, ctrlPath string, wait ControllerRemovalWait) error {
	w := wait.withDefaults()
	deadline := time.NewTimer(w.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	for {
		_, statErr := os.Stat(ctrlPath)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return fmt.Errorf("verify removal of controller %s: %w", ctrlPath, statErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for removal of controller %s: %w", ctrlPath, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("controller %s (state %q) after %s: %w",
				ctrlPath, readControllerState(ctrlPath), w.Timeout, ErrControllerNotRemoved)
		case <-ticker.C:
		}
	}
}

// readControllerState returns the controller's sysfs state for diagnostics,
// or a description of why it could not be read.
func readControllerState(ctrlPath string) string {
	statePath := filepath.Join(ctrlPath, "state")
	state, err := os.ReadFile(statePath) //nolint:gosec // G304: sysfs path under connector-controlled root.
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(state))
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
