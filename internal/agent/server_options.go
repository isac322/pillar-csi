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

package agent

import (
	"time"

	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ServerOption is a functional option that configures a Server at construction
// time.  Options are applied in order by NewServer after the base Server is
// initialized.
type ServerOption func(*Server)

// WithDeviceChecker overrides the DeviceChecker used by ExportVolume to probe
// whether the zvol block device is present before writing configfs state.
//
// In production the server defaults to nvmeof.OsStatDeviceChecker (os.Stat).
// Pass nvmeof.AlwaysPresentChecker in tests to skip the device-presence check
// without requiring real block devices in the test environment.
func WithDeviceChecker(c nvmeof.DeviceChecker) ServerOption {
	return func(s *Server) { s.deviceChecker = c }
}

// WithDevicePollParams overrides the poll interval and timeout used by
// ExportVolume when waiting for the zvol block device to appear.
//
// In production the server defaults to nvmeof.DefaultDevicePollInterval (500 ms)
// and nvmeof.DefaultDevicePollTimeout (5 s).  Pass small values in tests to
// exercise timeout paths without blocking for seconds.
func WithDevicePollParams(interval, timeout time.Duration) ServerOption {
	return func(s *Server) {
		s.devicePollInterval = interval
		s.devicePollTimeout = timeout
	}
}

// WithSysModuleZFSPath overrides the filesystem path used by HealthCheck to
// verify that the ZFS kernel module is loaded.
//
// In production this defaults to /sys/module/zfs.  Tests supply a tmpdir-based
// path so the health check can be exercised without requiring ZFS to be
// installed on the test host.
func WithSysModuleZFSPath(path string) ServerOption {
	return func(s *Server) { s.sysModuleZFSPath = path }
}
