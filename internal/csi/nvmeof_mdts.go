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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// ErrNVMeoFSubsystemNotConnected reports that LimitNVMeoFTransferSize found
// no subsystem with the requested NQN in sysfs.
var ErrNVMeoFSubsystemNotConnected = errors.New("NVMe-oF subsystem is not connected")

// errNoLiveNVMeController reports that no controller of a subsystem was
// live, so none could be asked for its MDTS.
var errNoLiveNVMeController = errors.New("no live controller")

// NVMeoFTransferLimiter is implemented by NVMe-oF protocol handlers whose
// connected subsystems can be capped with LimitNVMeoFTransferSize outside
// the handler's Attach (which caps the devices of a new stage):
// NodeStageVolume does so when the volume is already staged, and the
// background transfer limit reconciler (see
// StartNVMeoFTransferLimitReconciler) for every staged volume.
type NVMeoFTransferLimiter interface {
	// TransferLimitSysfs returns the sysfs root and the MDTS reader the
	// handler's Attach passes to LimitNVMeoFTransferSize.
	TransferLimitSysfs() (sysfsRoot string, readMDTS NVMeMDTSReader)
}

// NVMeoFDeviceLimitError is the failure to cap one namespace block device
// (e.g. "nvme0n1") in LimitNVMeoFTransferSize.
type NVMeoFDeviceLimitError struct {
	Device string
	Err    error
}

func (e *NVMeoFDeviceLimitError) Error() string { return e.Err.Error() }

func (e *NVMeoFDeviceLimitError) Unwrap() error { return e.Err }

// CheckNVMeoFTransferSizePageSize rejects a max data transfer size the
// worker cannot apply: the kernel refuses a queue/max_sectors_kb below
// PAGE_SIZE/1024 (queue_max_sectors_store), so on a worker with 16 KiB or
// 64 KiB pages a smaller limit would only fail later, after connecting.
// A size of 0 (no limit) is always accepted.  The value is never rounded
// up: the caller asked for a limit the node cannot enforce.
func CheckNVMeoFTransferSizePageSize(size int32, pageSize int) error {
	if size > 0 && int64(size) < int64(pageSize) {
		return fmt.Errorf("%s=%d bytes is smaller than this node's page size of %d bytes; "+
			"queue/max_sectors_kb cannot be set below one page, so use 0 or at least %d",
			paramNVMeOFMaxDataTransferSize, size, pageSize, pageSize)
	}
	return nil
}

// NVMeMDTSReader returns the MDTS field (byte 77 of the Identify Controller
// data) of the NVMe controller ctrl (e.g. "nvme0"): the maximum data
// transfer size as a power of two in units of the controller's minimum
// memory page size, or 0 when the controller advertises no limit.
type NVMeMDTSReader func(ctrl string) (uint8, error)

// ParseNVMeoFMaxDataTransferSize extracts the per-command transfer limit, in
// bytes, that CreateVolume copied into the VolumeContext.  An absent or
// empty key belongs to a volume provisioned before the key existed and
// resolves to v1alpha1.DefaultMaxDataTransferSize; 0 means no limit.  A
// value that is not a base-10 int32 accepted by
// v1alpha1.IsValidMaxDataTransferSize is an error, so a corrupted context
// never silently leaves the device without its limit.
func ParseNVMeoFMaxDataTransferSize(volCtx map[string]string) (int32, error) {
	raw := volCtx[paramNVMeOFMaxDataTransferSize]
	if raw == "" {
		return v1alpha1.DefaultMaxDataTransferSize, nil
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q: %w", paramNVMeOFMaxDataTransferSize, raw, err)
	}
	if !v1alpha1.IsValidMaxDataTransferSize(v) {
		return 0, fmt.Errorf("parse %s=%d: must be 0 or a power of two within [%d, %d]",
			paramNVMeOFMaxDataTransferSize, v,
			v1alpha1.MinMaxDataTransferSize, v1alpha1.MaxMaxDataTransferSize)
	}
	return int32(v), nil
}

// LimitNVMeoFTransferSize caps the block-layer request size of the NVMe-oF
// namespace devices of subsysNQN to size bytes when no controller of the
// subsystem advertises a maximum data transfer size (MDTS).
//
// A Linux nvmet target before 7.1 (no port param_mdts) reports MDTS 0, and a
// recent host then builds commands of up to 32 MiB.  The nvmet_tcp driver
// allocates the scatterlist of a command with one kmalloc, which fails on a
// fragmented target and fails the write.  When any controller reports a
// non-zero MDTS the kernel already splits requests to it, so nothing is
// written.  A size of 0 means the volume asked for no limit.
//
// The MDTS is read with an Identify Controller admin command (readMDTS)
// rather than derived from sysfs: queue/max_hw_sectors_kb is the minimum of
// the MDTS and transport/driver limits (nvme_set_ctrl_limits, the multipath
// head's stacked max_hw_sectors), so it cannot tell "MDTS 0" from "MDTS equal
// to the driver cap".  Only "live" controllers are queried (one without a
// state attribute counts as live, as in SubsystemHasActiveController); a
// connecting or dying controller cannot answer admin commands.  At least one
// live controller is required.
//
// The cap is written to queue/max_sectors_kb of the multipath head namespace
// devices (nvmeXnY) and of every per-controller path or namespace device
// (nvmeXcYnZ, or nvmeXnY without multipath).
func LimitNVMeoFTransferSize(sysfsRoot, subsysNQN string, size int32, readMDTS NVMeMDTSReader) error {
	if size == 0 {
		return nil
	}
	subsysPaths, err := nvmeofSubsystemPaths(sysfsRoot, subsysNQN)
	if err != nil {
		return err
	}
	ctrls, err := liveControllersOf(subsysPaths)
	if err != nil {
		return fmt.Errorf("read MDTS of subsystem %q: %w", subsysNQN, err)
	}
	limited, err := controllersAdvertiseMDTS(ctrls, readMDTS)
	if err != nil {
		return fmt.Errorf("read MDTS of subsystem %q: %w", subsysNQN, err)
	}
	if limited {
		return nil
	}
	return capNVMeoFDevices(sysfsRoot, subsysNQN, subsysPaths, nvmeofSizeKB(size))
}

// nvmeofSizeKB converts a transfer limit in bytes to KiB, the unit of
// queue/max_sectors_kb.
func nvmeofSizeKB(size int32) int64 {
	return int64(size) / 1024 //nolint:mnd // bytes to KiB
}

// nvmeofSubsystemPaths returns the sysfs directories of the connected
// subsystems with NQN subsysNQN, or an error wrapping
// ErrNVMeoFSubsystemNotConnected when there is none.
func nvmeofSubsystemPaths(sysfsRoot, subsysNQN string) ([]string, error) {
	var subsysPaths []string
	err := forEachMatchingSubsystem(sysfsRoot, subsysNQN, func(subsysDir, name string) error {
		subsysPaths = append(subsysPaths, filepath.Join(subsysDir, name))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(subsysPaths) == 0 {
		return nil, fmt.Errorf("limit transfer size of subsystem %q: %w", subsysNQN, ErrNVMeoFSubsystemNotConnected)
	}
	return subsysPaths, nil
}

// capNVMeoFDevices caps every namespace device of the subsystems to wantKB
// (see capQueueMaxSectorsKB).  Each device that cannot be capped is joined
// into the error as an NVMeoFDeviceLimitError; the others are still capped.
func capNVMeoFDevices(sysfsRoot, subsysNQN string, subsysPaths []string, wantKB int64) error {
	devices, err := nvmeNamespaceDevices(sysfsRoot, subsysPaths)
	if err != nil {
		return fmt.Errorf("list namespace devices of subsystem %q: %w", subsysNQN, err)
	}
	var errs []error
	for _, dev := range devices {
		capErr := capQueueMaxSectorsKB(sysfsRoot, dev, wantKB)
		if capErr != nil {
			errs = append(errs, &NVMeoFDeviceLimitError{Device: dev, Err: capErr})
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("limit transfer size of subsystem %q: %w", subsysNQN, errors.Join(errs...))
	}
	return nil
}

// liveControllersOf returns the live controllers of the subsystems (see
// liveSubsystemControllers), sorted.
func liveControllersOf(subsysPaths []string) ([]string, error) {
	var live []string
	for _, subsysPath := range subsysPaths {
		ctrls, err := liveSubsystemControllers(subsysPath)
		if err != nil {
			return nil, err
		}
		live = append(live, ctrls...)
	}
	slices.Sort(live)
	return live, nil
}

// controllersAdvertiseMDTS reports whether any of the live controllers
// ctrls reports a non-zero MDTS.  It fails when ctrls is empty.
func controllersAdvertiseMDTS(ctrls []string, readMDTS NVMeMDTSReader) (bool, error) {
	if len(ctrls) == 0 {
		return false, errNoLiveNVMeController
	}
	for _, ctrl := range ctrls {
		mdts, err := readMDTS(ctrl)
		if err != nil {
			return false, fmt.Errorf("controller %s: %w", ctrl, err)
		}
		if mdts != 0 {
			return true, nil
		}
	}
	return false, nil
}

// liveSubsystemControllers returns the controllers linked from subsysPath
// whose sysfs state is "live" or absent.  A controller that vanishes while
// being read is skipped.
func liveSubsystemControllers(subsysPath string) ([]string, error) {
	entries, err := os.ReadDir(subsysPath)
	if err != nil {
		return nil, fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}
	var live []string
	for _, entry := range entries {
		name := entry.Name()
		if !IsNVMeControllerEntry(name) {
			continue
		}
		statePath := filepath.Join(subsysPath, name, "state")
		state, readErr := os.ReadFile(statePath) //nolint:gosec // G304: sysfs path under connector-controlled root
		switch {
		case readErr == nil:
			if strings.TrimSpace(string(state)) == "live" {
				live = append(live, name)
			}
		case !errors.Is(readErr, fs.ErrNotExist):
			return nil, fmt.Errorf("read controller state %s: %w", statePath, readErr)
		default:
			_, statErr := os.Stat(filepath.Join(subsysPath, name))
			if statErr == nil {
				live = append(live, name) // no state attribute on this kernel
			}
		}
	}
	return live, nil
}

// nvmeNamespaceDevices returns the block devices of the subsystems: the
// namespace entries of each subsystem directory (multipath heads) and the
// namespace entries of each controller's /sys/class/nvme/<ctrl> directory
// (multipath path devices, or the namespaces themselves without multipath).
// Controllers the kernel is tearing down are skipped.
func nvmeNamespaceDevices(sysfsRoot string, subsysPaths []string) ([]string, error) {
	seen := map[string]bool{}
	var devices []string
	add := func(dir string) error {
		names, err := namespaceEntries(dir)
		for _, name := range names {
			if !seen[name] {
				seen[name] = true
				devices = append(devices, name)
			}
		}
		return err
	}
	for _, subsysPath := range subsysPaths {
		err := add(subsysPath)
		if err != nil {
			return nil, err
		}
		ctrls, err := activeSubsystemControllers(subsysPath)
		if err != nil {
			return nil, err
		}
		for _, ctrl := range ctrls {
			err = add(filepath.Join(sysfsRoot, "class", "nvme", ctrl))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	return devices, nil
}

// activeSubsystemControllers returns the controllers linked from subsysPath
// that are not being torn down.
func activeSubsystemControllers(subsysPath string) ([]string, error) {
	entries, err := os.ReadDir(subsysPath)
	if err != nil {
		return nil, fmt.Errorf("read subsystem dir %s: %w", subsysPath, err)
	}
	var ctrls []string
	for _, entry := range entries {
		name := entry.Name()
		if !IsNVMeControllerEntry(name) {
			continue
		}
		state, readErr := os.ReadFile(filepath.Join(subsysPath, name, "state")) //nolint:gosec // G304: sysfs path
		if readErr == nil && isDyingControllerState(strings.TrimSpace(string(state))) {
			continue
		}
		ctrls = append(ctrls, name)
	}
	return ctrls, nil
}

// namespaceEntries returns the nvme namespace device entries (nvmeXnY,
// nvmeXcYnZ) of dir.
func namespaceEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "nvme") && !IsNVMeControllerEntry(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

// capQueueMaxSectorsKB sets <sysfsRoot>/block/<dev>/queue/max_sectors_kb to
// wantKB, or to max_hw_sectors_kb when that is smaller: the kernel rejects a
// max_sectors_kb above max_hw_sectors_kb with EINVAL, and the hardware limit
// is already the tighter one.  The value is read back because a sysfs write
// can succeed without taking effect.
func capQueueMaxSectorsKB(sysfsRoot, dev string, wantKB int64) error {
	queueDir := filepath.Join(sysfsRoot, "block", dev, "queue")
	hwPath := filepath.Join(queueDir, "max_hw_sectors_kb")
	hwRaw, err := os.ReadFile(hwPath) //nolint:gosec // G304: sysfs path under connector-controlled root
	switch {
	case err == nil:
		hwKB, parseErr := strconv.ParseInt(strings.TrimSpace(string(hwRaw)), 10, 64)
		if parseErr != nil {
			return fmt.Errorf("parse %s of %s: %w", hwPath, dev, parseErr)
		}
		wantKB = min(wantKB, hwKB)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read %s of %s: %w", hwPath, dev, err)
	}

	path := filepath.Join(queueDir, "max_sectors_kb")
	want := strconv.FormatInt(wantKB, 10)
	cur, err := os.ReadFile(path) //nolint:gosec // G304: sysfs path under connector-controlled root
	if err != nil {
		return fmt.Errorf("read %s of %s: %w", path, dev, err)
	}
	if strings.TrimSpace(string(cur)) == want {
		return nil
	}
	err = writeSysfsAttr(path, want)
	if err != nil {
		return fmt.Errorf("limit %s: %w", dev, err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: sysfs path under connector-controlled root
	if err != nil {
		return fmt.Errorf("read back %s of %s: %w", path, dev, err)
	}
	if strings.TrimSpace(string(got)) != want {
		return fmt.Errorf("limit %s: %s reads %q after writing %q",
			dev, path, strings.TrimSpace(string(got)), want)
	}
	return nil
}

// nvmeofStageLimit returns the transfer limit of an NVMe-oF stage record:
// the persisted value, or v1alpha1.DefaultMaxDataTransferSize for a record
// written before NodeStageVolume persisted it.
func nvmeofStageLimit(s *NVMeoFStageState) int32 {
	if s.MaxDataTransferSize != nil {
		return *s.MaxDataTransferSize
	}
	return v1alpha1.DefaultMaxDataTransferSize
}

// limitStagedNVMeoF caps the connected subsystem of an already-staged
// NVMe-oF volume to size (see LimitNVMeoFTransferSize) and records size in
// state, which the caller persists.  Local attaches, other protocols and
// handlers that cannot cap a connected subsystem are left alone.
func (*NodeServer) limitStagedNVMeoF(
	handler ProtocolHandler, state *nodeStageState, targetID string, size int32,
) error {
	if state.ProtocolType != ProtocolNVMeoFTCP || state.isLocalAttach() || state.NVMeoF == nil {
		return nil
	}
	limiter, ok := handler.(NVMeoFTransferLimiter)
	if !ok {
		return nil
	}
	nqn := state.NVMeoF.SubsysNQN
	if nqn == "" {
		nqn = targetID
	}
	sysfsRoot, readMDTS := limiter.TransferLimitSysfs()
	err := LimitNVMeoFTransferSize(sysfsRoot, nqn, size, readMDTS)
	if err != nil {
		return fmt.Errorf("limit transfer size for %q: %w", nqn, err)
	}
	state.NVMeoF.MaxDataTransferSize = &size
	return nil
}

// nvmeofDeviceLimitErrors returns the per-device failures joined into err
// by LimitNVMeoFTransferSize.
func nvmeofDeviceLimitErrors(err error) []*NVMeoFDeviceLimitError {
	switch u := err.(type) { //nolint:errorlint // walks the error tree itself
	case *NVMeoFDeviceLimitError:
		return []*NVMeoFDeviceLimitError{u}
	case interface{ Unwrap() []error }:
		var out []*NVMeoFDeviceLimitError
		for _, child := range u.Unwrap() {
			out = append(out, nvmeofDeviceLimitErrors(child)...)
		}
		return out
	case interface{ Unwrap() error }:
		return nvmeofDeviceLimitErrors(u.Unwrap())
	}
	return nil
}
