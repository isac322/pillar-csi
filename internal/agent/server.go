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
// storage node.  It manages ZFS zvol and LVM volumes and exports them via
// NVMe-oF TCP (nvmet) or iSCSI (LIO) using direct configfs manipulation (no
// external CLI tools).
package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// agentVersion is the semver version string embedded in discovery responses.
const agentVersion = "0.5.3"

// defaultDrainStateDir is the production directory for the .drained marker.
const defaultDrainStateDir = "/var/lib/pillar-csi/agent"

// Server implements agentv1.AgentServiceServer.  It is bound to a set of
// named storage backends and a configfs root directory.
type Server struct {
	agentv1.UnimplementedAgentServiceServer

	// backends maps pool name → default backend.  backendVariants retains
	// distinct backend types when a ZFS pool hosts both zvols and datasets.
	backends        map[string]backend.VolumeBackend
	backendVariants map[string]map[agentv1.BackendType]backend.VolumeBackend

	// nfsManager is present only when a filesystem-capable backend is
	// configured and the NFS runtime could be constructed.
	nfsManager *nfs.Manager

	// configfsRoot is the root of the nvmet configfs tree.  Defaults to
	// DefaultConfigfsRoot when empty.
	configfsRoot string

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

	// deviceClaimer takes an exclusive claim on the backend device before a
	// disabled NVMe-oF namespace is enabled; the claim is held across the
	// enable write so a local attach on the storage node cannot race in.
	// When nil, nvmeof.ClaimDeviceExclusively is used.  Override via
	// WithDeviceClaimer or SetDeviceClaimer.
	deviceClaimer nvmeof.DeviceClaimer

	// mdtsUnsupportedOnce limits the warning that the kernel cannot
	// advertise a maximum data transfer size (no param_mdts, Linux < 7.1)
	// to once per agent process: it is a fact about the kernel, not about
	// any one export.  See reportMDTSUnsupported.
	mdtsUnsupportedOnce sync.Once

	// targetMu serializes protocol handler operations on the same
	// protocol-qualified target ID to prevent partial-state races when export
	// and unexport are called concurrently for the same volume
	// (e.g. during reconciliation).
	// Values are *sync.Mutex; LoadOrStore is used to create on first access.
	targetMu sync.Map

	// fencingMu serializes, per volume ID, every read-check-write and removal
	// of the durable fencing mark (see fencing.go).  Target locks are keyed by
	// protocol target, and backend DeleteVolume holds none, so the mark needs
	// its own per-volume lock.  Values are *sync.Mutex.
	fencingMu sync.Map

	// drained reports whether Drain has been called; further RPCs are rejected
	// with codes.Unavailable once true.
	drained atomic.Bool

	// exportRestorePending gates export-creating RPCs until a complete
	// ReconcileState has restored every export (see server_export_restore.go).
	// Set by WithExportRestoreGate; once cleared it is never set again.
	exportRestorePending atomic.Bool

	// drainGate sequences Drain against in-flight intercepted RPCs.  The
	// DrainGuardInterceptor takes an RLock for the entire handler invocation;
	// Drain takes the WLock so it blocks until every already-accepted RPC
	// finishes.  Without this lock a handler could pass the drained check,
	// then create a new targetMu entry AFTER Drain's per-target sweep had
	// already finished, leaving its mutating work running while Drain
	// reported clean shutdown.
	drainGate sync.RWMutex

	// drainStateDir is the directory where the .drained marker is written. When
	// empty, Drain falls back to os.TempDir()/pillar-csi-agent.
	drainStateDir string

	// protocolHandlerResolver optionally overrides handler selection for this
	// server instance. Production uses the built-in resolver; tests inject a
	// per-server resolver to verify dispatch without global state.
	protocolHandlerResolver func(agentv1.ProtocolType) (AgentProtocolHandler, error)

	// lioFS performs the LIO configfs operations of the iSCSI handler.  nil
	// selects lio.OSFS; tests inject an emulated kernel via WithLIOFS.
	lioFS lio.FS
}

// Ensure Server satisfies the interface at compile time.
var _ agentv1.AgentServiceServer = (*Server)(nil)

type targetLockKey struct {
	protocolType agentv1.ProtocolType
	targetID     string
}

// NewServer constructs an AgentService Server bound to the given backends and
// configfs root directory.  An empty configfsRoot falls back to
// /sys/kernel/config at runtime.
//
// Zero or more ServerOption values may be passed to override defaults; options
// are applied in order after the base Server is initialized.  This signature is
// backward-compatible with callers that pass no options.
func NewServer(backends map[string]backend.VolumeBackend, configfsRoot string, opts ...ServerOption) *Server {
	s := &Server{
		backends:      backends,
		configfsRoot:  configfsRoot,
		drainStateDir: defaultDrainStateDir,
	}
	for _, o := range opts {
		o(s)
	}
	if s.backendVariants == nil {
		s.backendVariants = make(map[string]map[agentv1.BackendType]backend.VolumeBackend, len(backends))
		for pool, b := range backends {
			if b != nil {
				s.backendVariants[pool] = map[agentv1.BackendType]backend.VolumeBackend{b.Type(): b}
			}
		}
	}
	return s
}

// Register wires s into the provided gRPC server.
func (s *Server) Register(g *grpc.Server) {
	agentv1.RegisterAgentServiceServer(g, s)
}

// lockTarget acquires the per-target mutex and returns an unlock function.
// It serializes concurrent protocol mutations for the same protocol-qualified
// target so that target state is always internally consistent.  The time
// spent waiting is recorded on the RPC span in ctx.
func (s *Server) lockTarget(ctx context.Context, protocolType agentv1.ProtocolType, targetID string) func() {
	unlock, wait := s.acquireTarget(protocolType, targetID)
	recordLockWait(ctx, wait)
	return unlock
}

// acquireTarget locks the per-target mutex and returns its unlock and how
// long the lock took to acquire.
func (s *Server) acquireTarget(protocolType agentv1.ProtocolType, targetID string) (func(), time.Duration) {
	key := targetLockKey{
		protocolType: protocolType,
		targetID:     targetID,
	}
	v, _ := s.targetMu.LoadOrStore(key, &sync.Mutex{})
	mu, ok := v.(*sync.Mutex)
	if !ok {
		// Should never happen: only *sync.Mutex values are stored in targetMu.
		mu = &sync.Mutex{}
	}
	start := time.Now()
	mu.Lock()
	return mu.Unlock, time.Since(start)
}

// recordLockWait sets pillar_csi.lock.target.wait_duration (seconds) on the
// span in ctx.
func recordLockWait(ctx context.Context, wait time.Duration) {
	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(telemetry.KeyLockTargetWaitDuration.Float64(wait.Seconds()))
	}
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
	if variants := s.backendVariants[pool]; len(variants) > 1 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %q requires an explicit backend path when pool %q has multiple backend types",
			volumeID, pool)
	}
	b, ok := s.backends[pool]
	if !ok {
		return nil, status.Errorf(codes.NotFound,
			"no backend registered for pool %q", pool)
	}
	return b, nil
}

// backendForType resolves a volume to the backend selected by its explicit
// backend type.  This is required when one ZFS pool contains both zvol and
// dataset resources.
func (s *Server) backendForType(volumeID string, requested agentv1.BackendType) (backend.VolumeBackend, error) {
	pool, err := poolFromVolumeID(volumeID)
	if err != nil {
		return nil, err
	}
	if variants := s.backendVariants[pool]; variants != nil {
		if b, ok := variants[requested]; ok {
			return b, nil
		}
	}
	b, ok := s.backends[pool]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "no backend registered for pool %q", pool)
	}
	return b, nil
}
