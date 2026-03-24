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

// Package agent implements the AgentService gRPC server that runs on each
// storage node.  It manages ZFS zvol volumes and exports them via NVMe-oF TCP
// using direct configfs manipulation (no external CLI tools).
package agent

import (
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// agentVersion is the semver version string embedded in discovery responses.
const agentVersion = "0.1.0"

// errOnlyNvmeofTCP is returned by Phase-1 handlers for unsupported protocols.
const errOnlyNvmeofTCP = "only NVMe-oF TCP is supported"

// nqnPrefix is the fixed NQN prefix used for all NVMe subsystem names.
const nqnPrefix = "nqn.2026-01.com.bhyoo.pillar-csi:"

// Server implements agentv1.AgentServiceServer.  It is bound to a set of
// named storage backends (one per ZFS pool) and a configfs root directory.
type Server struct {
	agentv1.UnimplementedAgentServiceServer

	// backends maps pool name → VolumeBackend for that pool.
	backends map[string]backend.VolumeBackend

	// configfsRoot is the root of the nvmet configfs tree.  Defaults to
	// DefaultConfigfsRoot when empty.
	configfsRoot string

	// sysModuleZFSPath is the path used by checkZFSModule to verify that the
	// ZFS kernel module is loaded.  It defaults to /sys/module/zfs in
	// production; tests may override it to a temporary directory via
	// SetServerSysModuleZFSPath (defined in export_test.go).
	sysModuleZFSPath string

	// devicePollInterval is the cadence passed to nvmeof.WaitForDevice in
	// ExportVolume.  When zero, nvmeof.DefaultDevicePollInterval is used.
	// Override in tests via SetDevicePollParams (export_test.go).
	devicePollInterval time.Duration

	// devicePollTimeout is the upper-bound wait duration passed to
	// nvmeof.WaitForDevice in ExportVolume.  When zero,
	// nvmeof.DefaultDevicePollTimeout is used.
	// Override in tests via SetDevicePollParams (export_test.go).
	devicePollTimeout time.Duration

	// deviceChecker is the DeviceChecker used by ExportVolume to probe whether
	// the zvol block device is present before configfs writes.  When nil,
	// nvmeof.OsStatDeviceChecker (os.Stat-based) is used as the production
	// default.  Override in tests via SetDeviceChecker (export_test.go).
	deviceChecker nvmeof.DeviceChecker
}

// Ensure Server satisfies the interface at compile time.
var _ agentv1.AgentServiceServer = (*Server)(nil)

// NewServer constructs an AgentService Server bound to the given backends and
// configfs root directory.  An empty configfsRoot falls back to
// /sys/kernel/config at runtime.
func NewServer(backends map[string]backend.VolumeBackend, configfsRoot string) *Server {
	return &Server{
		backends:     backends,
		configfsRoot: configfsRoot,
	}
}

// Register wires s into the provided gRPC server.
func (s *Server) Register(g *grpc.Server) {
	agentv1.RegisterAgentServiceServer(g, s)
}

// volumeNQN derives the NVMe Qualified Name for a volume ID.
//
// Format: nqn.2026-01.com.bhyoo.pillar-csi:<pool>.<name>
// Example: "tank/pvc-abc" → "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc".
func volumeNQN(volumeID string) string {
	return nqnPrefix + strings.ReplaceAll(volumeID, "/", ".")
}

// poolFromVolumeID extracts the pool name from a volumeID of form
// "<pool>/<name>".  Returns an InvalidArgument gRPC status error on bad input.
func poolFromVolumeID(volumeID string) (string, error) {
	idx := strings.IndexByte(volumeID, '/')
	if idx <= 0 {
		return "", status.Errorf(codes.InvalidArgument,
			"volumeID %q: expected \"<pool>/<name>\" format", volumeID)
	}
	return volumeID[:idx], nil
}

// backendFor looks up the VolumeBackend for the pool inferred from volumeID.
// Returns a NotFound gRPC status error if no backend is registered.
func (s *Server) backendFor(volumeID string) (backend.VolumeBackend, error) {
	pool, err := poolFromVolumeID(volumeID)
	if err != nil {
		return nil, err
	}
	b, ok := s.backends[pool]
	if !ok {
		return nil, status.Errorf(codes.NotFound,
			"no backend registered for pool %q", pool)
	}
	return b, nil
}
