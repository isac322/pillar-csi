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

// Package-internal test helpers that expose Server fields for white-box
// testing.  This file is compiled only when running `go test`; it is NOT
// part of the production binary.
//
// All helpers operate on a specific *Server instance rather than on
// package-level globals, so tests that call t.Parallel() are safe to use them
// without data races.
package agent

import (
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// SetServerSysModuleZFSPath overrides the path used by checkZFSModule to
// verify that the ZFS kernel module is loaded.  In production the path
// defaults to /sys/module/zfs; tests supply a t.TempDir()-based path so the
// check can be made to return either healthy or unhealthy without requiring
// ZFS to be installed in the CI environment.
//
// The original value is restored automatically via t.Cleanup so that parallel
// tests cannot interfere with each other.
func SetServerSysModuleZFSPath(t *testing.T, s *Server, path string) {
	t.Helper()
	orig := s.sysModuleZFSPath
	s.sysModuleZFSPath = path
	t.Cleanup(func() { s.sysModuleZFSPath = orig })
}

// SetDeviceChecker overrides the DeviceChecker used by ExportVolume to probe
// whether the zvol block device is present.  In production the server uses
// nvmeof.OsStatDeviceChecker (os.Stat-based); tests substitute a mock so that
// ExportVolume does not require real block devices in the test environment.
//
// Pass nvmeof.AlwaysPresentChecker to make every device appear immediately
// present.  Pass nil to restore the production default.
//
// The original checker is restored automatically via t.Cleanup so that
// parallel tests cannot interfere with each other.
func SetDeviceChecker(t *testing.T, s *Server, checker nvmeof.DeviceChecker) {
	t.Helper()
	orig := s.deviceChecker
	s.deviceChecker = checker
	t.Cleanup(func() { s.deviceChecker = orig })
}

// SetDevicePollParams overrides the poll interval and timeout used by
// ExportVolume when waiting for the zvol block device to appear.  In
// production the server defaults to nvmeof.DefaultDevicePollInterval (500 ms)
// and nvmeof.DefaultDevicePollTimeout (5 s).  Tests that need to exercise the
// timeout path should pass small values (e.g. 10 ms / 50 ms) so that the test
// completes quickly.
//
// Both values are restored automatically via t.Cleanup so that parallel tests
// cannot interfere with each other.
func SetDevicePollParams(t *testing.T, s *Server, interval, timeout time.Duration) {
	t.Helper()
	origInterval := s.devicePollInterval
	origTimeout := s.devicePollTimeout
	s.devicePollInterval = interval
	s.devicePollTimeout = timeout
	t.Cleanup(func() {
		s.devicePollInterval = origInterval
		s.devicePollTimeout = origTimeout
	})
}
