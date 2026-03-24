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
	"context"
	"fmt"
	"os"
	"time"
)

// DefaultDevicePollInterval is the default probe cadence used by
// WaitForDevice when no custom interval is passed.  500 ms balances
// responsiveness with avoiding a tight busy-loop while waiting for udev to
// settle a zvol after "zfs create -V".
const DefaultDevicePollInterval = 500 * time.Millisecond

// DefaultDevicePollTimeout is the default upper bound used by WaitForDevice.
// 5 s is sufficient for local ZFS zvol enumeration; reduce via the timeout
// parameter when you need a tighter bound in tests.
const DefaultDevicePollTimeout = 5 * time.Second

// DeviceChecker probes a filesystem path and reports whether the device
// exists.  It returns (true, nil) when the device is present, (false, nil)
// when the device is absent and polling should continue, or (false, non-nil)
// when a permanent error (e.g. permission denied) makes further retries
// pointless.
//
// The production implementation is OsStatDeviceChecker.  Tests can inject a
// mock that simulates device presence without requiring real block devices.
type DeviceChecker func(path string) (exists bool, permanentErr error)

// OsStatDeviceChecker is the production DeviceChecker.  It uses os.Stat to
// follow symlinks (matching what the kernel nvmet configfs layer expects) and
// returns:
//   - (true, nil)  – path is accessible (device present).
//   - (false, nil) – path does not exist (ENOENT/ENOTDIR); keep polling.
//   - (false, err) – any other stat error (e.g. EACCES); stop immediately.
var OsStatDeviceChecker DeviceChecker = func(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	// Unexpected error (e.g. permission denied): signal permanent failure.
	return false, fmt.Errorf("wait for device %q: %w", path, err)
}

// AlwaysPresentChecker is a DeviceChecker that unconditionally reports every
// path as present.  It is intended for use in tests that exercise logic after
// the device-presence check (e.g. configfs writes) without requiring real
// block devices in the test environment.
var AlwaysPresentChecker DeviceChecker = func(_ string) (bool, error) {
	return true, nil
}

// WaitForDevice polls path at the given interval until checker reports the
// path as present or the total wait exceeds timeout.
//
// Use case: after "zfs create -V …" the kernel schedules a uevent that causes
// udevd to create the /dev/zvol/… symlink.  That process is asynchronous — the
// zvol block device may not be present by the time control returns from the
// zfs(8) command.  WaitForDevice bridges that gap before the agent hands the
// device path to the nvmet configfs layer.
//
// Checker is a DeviceChecker that probes the path.  When checker is nil,
// OsStatDeviceChecker is used (production default).  The function returns nil
// as soon as checker reports (true, nil).  When checker returns a non-nil
// permanentErr, WaitForDevice returns that error immediately without further
// retries.
//
// Parameters:
//
//	ctx      – parent context; cancellation causes an immediate return.
//	path     – filesystem path to probe (e.g. "/dev/zvol/tank/pvc-abc").
//	interval – how long to sleep between consecutive stat calls.
//	timeout  – how long to wait in total before giving up.
//	checker  – DeviceChecker to use; nil defaults to OsStatDeviceChecker.
//
// The effective deadline is min(ctx.Deadline(), now+timeout).
func WaitForDevice(ctx context.Context, path string, interval, timeout time.Duration, checker DeviceChecker) error {
	if checker == nil {
		checker = OsStatDeviceChecker
	}

	// Derive a child context that expires at now+timeout, while still
	// honoring any shorter deadline already set on the parent ctx.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		exists, permanentErr := checker(path)
		if permanentErr != nil {
			return permanentErr
		}
		if exists {
			// Device is present — success.
			return nil
		}

		// Device not yet present; wait for the next probe interval or
		// until the context expires, whichever comes first.
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for device %q: timed out after %s: %w", path, timeout, ctx.Err())
		case <-time.After(interval):
			// Continue to the next probe iteration.
		}
	}
}
