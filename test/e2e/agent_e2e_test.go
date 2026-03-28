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

// Package e2e contains lightweight end-to-end tests for the pillar-csi agent
// gRPC server.  Unlike the Kubernetes-cluster-level e2e tests (guarded by the
// "e2e" build tag), these tests run on a real gRPC listener bound to
// localhost:0, with a mock ZFS backend (no real zfs commands) and a t.TempDir
// as the configfs root (no kernel configfs required).
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestAgent
package e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

// ----------------------------------------------------------------------------
// Mock backend
// ----------------------------------------------------------------------------.

// agentE2EMockBackend is a test double for backend.VolumeBackend.  It returns
// configurable values for each method and records calls for verification.
// The mock never invokes real ZFS commands.
type agentE2EMockBackend struct {
	// DevicePath to return from Create/DevicePath.
	devicePath string
	// Capacity values to return.
	totalBytes int64
	availBytes int64
	// Errors to return per-method (nil = success).
	createErr   error
	deleteErr   error
	expandErr   error
	capacityErr error
}

func (m *agentE2EMockBackend) Create(
	_ context.Context,
	_ string,
	capacityBytes int64,
	_ *agentv1.ZfsVolumeParams,
) (devicePath string, allocatedBytes int64, err error) {
	if m.createErr != nil {
		return "", 0, m.createErr
	}
	return m.devicePath, capacityBytes, nil
}

func (m *agentE2EMockBackend) Delete(_ context.Context, _ string) error {
	return m.deleteErr
}

func (m *agentE2EMockBackend) Expand(_ context.Context, _ string, requested int64) (int64, error) {
	if m.expandErr != nil {
		return 0, m.expandErr
	}
	return requested, nil
}

func (m *agentE2EMockBackend) Capacity(_ context.Context) (total, avail int64, err error) {
	if m.capacityErr != nil {
		return 0, 0, m.capacityErr
	}
	return m.totalBytes, m.availBytes, nil
}

func (*agentE2EMockBackend) ListVolumes(
	_ context.Context,
) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}

func (m *agentE2EMockBackend) DevicePath(_ string) string {
	return m.devicePath
}

// Type identifies the e2e mock as a ZFS zvol backend so that GetCapabilities
// returns the correct supported backend types without hardcoding.
func (*agentE2EMockBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

// Compile-time interface check.
var _ backend.VolumeBackend = (*agentE2EMockBackend)(nil)

// ----------------------------------------------------------------------------
// Test helpers
// ----------------------------------------------------------------------------.

// agentE2EEnv holds all the resources needed for a single e2e test case.
type agentE2EEnv struct {
	client     agentv1.AgentServiceClient
	cfgRoot    string
	grpcServer *grpc.Server
	conn       *grpc.ClientConn
}

// newAgentE2EEnv starts a real gRPC server on localhost:0 with the given mock
// backend and a t.TempDir() as the configfs root.  It registers a cleanup
// function that stops the server and closes the client connection.
//
// The configfs root is returned so tests can inspect or pre-populate the
// simulated configfs tree.
func newAgentE2EEnv(t *testing.T, mock *agentE2EMockBackend) *agentE2EEnv {
	t.Helper()

	cfgRoot := t.TempDir()

	backends := map[string]backend.VolumeBackend{
		"tank": mock,
	}
	agentSrv := agent.NewServer(backends, cfgRoot)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	grpcSrv := grpc.NewServer()
	agentSrv.Register(grpcSrv)

	go func() { _ = grpcSrv.Serve(lis) }() //nolint:errcheck // server errors are non-actionable in test setup

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		grpcSrv.GracefulStop()
		t.Fatalf("grpc.NewClient: %v", err)
	}

	env := &agentE2EEnv{
		client:     agentv1.NewAgentServiceClient(conn),
		cfgRoot:    cfgRoot,
		grpcServer: grpcSrv,
		conn:       conn,
	}

	t.Cleanup(func() {
		conn.Close() //nolint:errcheck,gosec // G104: conn.Close error is non-actionable in test cleanup
		grpcSrv.GracefulStop()
	})

	return env
}

// createFakeDevice creates an ordinary file inside t.TempDir() and returns its
// path.  This lets os.Stat-based device-presence checks succeed in
// ExportVolume without requiring real block devices.
func createFakeDevice(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("createFakeDevice: %v", err)
	}
	return path
}

// nvmeofTCPExportParams builds an NvmeofTcpExportParams-wrapped ExportParams.
func nvmeofTCPExportParams(
	addr string, port int32, //nolint:unparam // port is kept for API clarity
) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: addr,
				Port:        port,
			},
		},
	}
}

// subsystemNames extracts the Name field from each SubsystemStatus.
func subsystemNames(ss []*agentv1.SubsystemStatus) []string {
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, s.GetName())
	}
	return names
}

// ----------------------------------------------------------------------------
// TestAgent_GetCapabilities
// ----------------------------------------------------------------------------.

// TestAgent_GetCapabilities verifies that the agent reports ZFS_ZVOL as the
// supported backend and NVMe-oF TCP as the supported protocol, and that it
// returns a non-empty agent version.
func TestAgent_GetCapabilities(t *testing.T) {
	t.Parallel()

	mock := &agentE2EMockBackend{totalBytes: 10 << 30, availBytes: 8 << 30}
	env := newAgentE2EEnv(t, mock)

	resp, err := env.client.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}

	if resp.GetAgentVersion() == "" {
		t.Error("AgentVersion is empty")
	}

	// Phase 1: only ZFS_ZVOL backend.
	if len(resp.GetSupportedBackends()) == 0 {
		t.Fatal("SupportedBackends is empty")
	}
	foundZVOL := false
	for _, b := range resp.GetSupportedBackends() {
		if b == agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
			foundZVOL = true
		}
	}
	if !foundZVOL {
		t.Errorf("SupportedBackends %v does not contain ZFS_ZVOL", resp.GetSupportedBackends())
	}

	// Phase 1: only NVMe-oF TCP protocol.
	if len(resp.GetSupportedProtocols()) == 0 {
		t.Fatal("SupportedProtocols is empty")
	}
	foundNVMeOF := false
	for _, p := range resp.GetSupportedProtocols() {
		if p == agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
			foundNVMeOF = true
		}
	}
	if !foundNVMeOF {
		t.Errorf("SupportedProtocols %v does not contain NVMEOF_TCP", resp.GetSupportedProtocols())
	}

	// Discovered pools should include "tank".
	foundTank := false
	for _, pool := range resp.GetDiscoveredPools() {
		if pool.GetName() == "tank" {
			foundTank = true
			if pool.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
				t.Errorf("pool[tank].BackendType = %v, want ZFS_ZVOL", pool.GetBackendType())
			}
		}
	}
	if !foundTank {
		t.Errorf("DiscoveredPools %v does not include tank", resp.GetDiscoveredPools())
	}
}

// ----------------------------------------------------------------------------
// TestAgent_HealthCheck
// ----------------------------------------------------------------------------.

// TestAgent_HealthCheck verifies that HealthCheck returns a structurally valid
// response with named subsystem entries.  The test pre-creates the nvmet
// directory to make the configfs check pass; the ZFS module check is expected
// to report degraded in CI (no real ZFS kernel module).
func TestAgent_HealthCheck(t *testing.T) {
	t.Parallel()

	mock := &agentE2EMockBackend{totalBytes: 5 << 30, availBytes: 3 << 30}
	env := newAgentE2EEnv(t, mock)

	// Pre-create <cfgRoot>/nvmet so the configfs health check reports healthy.
	if err := os.MkdirAll(filepath.Join(env.cfgRoot, "nvmet"), 0o750); err != nil {
		t.Fatalf("pre-create nvmet dir: %v", err)
	}

	resp, err := env.client.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	if resp.GetAgentVersion() == "" {
		t.Error("AgentVersion is empty")
	}
	if resp.GetCheckedAt() == nil {
		t.Error("CheckedAt timestamp is nil")
	}

	// Must have subsystem entries: nvmet_configfs + pool/tank (≥ 2).
	subsystems := resp.GetSubsystems()
	if len(subsystems) < 2 { // nvmet_configfs + pool/tank
		t.Errorf("expected ≥ 2 subsystems, got %d: %v", len(subsystems), subsystemNames(subsystems))
	}

	// Verify required backend-agnostic subsystem names are present.
	names := subsystemNames(subsystems)
	wantNames := []string{"nvmet_configfs", "pool/tank"}
	for _, want := range wantNames {
		found := slices.Contains(names, want)
		if !found {
			t.Errorf("subsystem %q not found in %v", want, names)
		}
	}

	// nvmet_configfs should be healthy (we pre-created the dir).
	for _, s := range subsystems {
		if s.GetName() == "nvmet_configfs" && !s.GetHealthy() {
			t.Errorf("nvmet_configfs: Healthy=false, Message=%q", s.GetMessage())
		}
		// pool/tank should be healthy (mock Capacity succeeds).
		if s.GetName() == "pool/tank" && !s.GetHealthy() {
			t.Errorf("pool/tank: Healthy=false, Message=%q", s.GetMessage())
		}
		// Every subsystem must have a non-empty message.
		if s.GetMessage() == "" {
			t.Errorf("subsystem %q has empty message", s.GetName())
		}
	}
}

// ----------------------------------------------------------------------------
// TestAgent_RoundTrip
// ----------------------------------------------------------------------------.

// TestAgent_RoundTrip exercises the full Phase 1 volume lifecycle over a real
// gRPC connection:
//
//  1. CreateVolume   – allocates backend storage
//  2. ExportVolume   – creates NVMe-oF TCP configfs entries
//  3. AllowInitiator – adds an initiator ACL entry
//  4. DenyInitiator  – removes the initiator ACL entry
//  5. UnexportVolume – tears down configfs entries
//  6. DeleteVolume   – destroys backend storage
//
// A real file in t.TempDir() stands in for the zvol block device so that
// the device-presence polling in ExportVolume passes without root or ZFS.
func TestAgent_RoundTrip(t *testing.T) {
	t.Parallel()

	const (
		volumeID = "tank/pvc-roundtrip"
		bindAddr = "127.0.0.1"
		hostNQN  = "nqn.2026-01.io.example:initiator-1"
	)

	// Create a fake device file so WaitForDevice (os.Stat) succeeds.
	fakeDevPath := createFakeDevice(t, "zvol-roundtrip")

	mock := &agentE2EMockBackend{
		devicePath: fakeDevPath,
		totalBytes: 10 << 30,
		availBytes: 8 << 30,
	}
	env := newAgentE2EEnv(t, mock)
	ctx := context.Background()

	// 1. CreateVolume
	createResp, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if createResp.GetCapacityBytes() != 1<<30 {
		t.Errorf("CreateVolume CapacityBytes = %d, want %d", createResp.GetCapacityBytes(), int64(1<<30))
	}

	// 2. ExportVolume — provide the device path explicitly so WaitForDevice
	//    finds the existing temp file rather than the production zvol path.
	exportResp, err := env.client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     volumeID,
		DevicePath:   fakeDevPath,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams(bindAddr, 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	exportInfo := exportResp.GetExportInfo()
	if exportInfo == nil {
		t.Fatal("ExportVolume: ExportInfo is nil")
	}
	// TargetId should be the NQN derived from the volume ID.
	wantNQN := "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-roundtrip"
	if exportInfo.GetTargetId() != wantNQN {
		t.Errorf("ExportInfo.TargetId = %q, want %q", exportInfo.GetTargetId(), wantNQN)
	}
	if exportInfo.GetPort() != 4420 {
		t.Errorf("ExportInfo.Port = %d, want 4420", exportInfo.GetPort())
	}

	// Configfs subsystem directory must exist.
	subDir := filepath.Join(env.cfgRoot, "nvmet", "subsystems", wantNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Fatalf("subsystem dir missing after ExportVolume: %v", statErr)
	}

	// 3. AllowInitiator
	_, err = env.client.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	})
	if err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}
	// allowed_hosts symlink must exist.
	linkPath := filepath.Join(env.cfgRoot, "nvmet", "subsystems", wantNQN, "allowed_hosts", hostNQN)
	if _, lstatErr := os.Lstat(linkPath); lstatErr != nil {
		t.Errorf("allowed_hosts symlink missing after AllowInitiator: %v", lstatErr)
	}

	// 4. DenyInitiator
	_, err = env.client.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	})
	if err != nil {
		t.Fatalf("DenyInitiator: %v", err)
	}
	// allowed_hosts symlink must be gone.
	if _, lstatErr := os.Lstat(linkPath); !os.IsNotExist(lstatErr) {
		t.Errorf("allowed_hosts symlink still exists after DenyInitiator: stat=%v", lstatErr)
	}

	// 5. UnexportVolume
	_, err = env.client.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err != nil {
		t.Fatalf("UnexportVolume: %v", err)
	}
	// Subsystem directory must be gone.
	if _, statErr := os.Stat(subDir); !os.IsNotExist(statErr) {
		t.Errorf("subsystem dir still exists after UnexportVolume: stat=%v", statErr)
	}

	// 6. DeleteVolume
	_, err = env.client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
}

// ----------------------------------------------------------------------------
// TestAgent_ReconcileStateRestoresExports
// ----------------------------------------------------------------------------.

// TestAgent_ReconcileStateRestoresExports simulates an agent restart.
//
// A fresh server (representing the agent after reboot) receives a
// ReconcileState call describing one volume with an NVMe-oF TCP export.
// The test verifies that:
//   - ReconcileState succeeds
//   - The configfs subsystem directory is (re-)created
//   - An allowed initiator ACL entry is applied
func TestAgent_ReconcileStateRestoresExports(t *testing.T) {
	t.Parallel()

	const (
		volumeID = "tank/pvc-restarted"
		devPath  = "/dev/zvol/tank/pvc-restarted" // any path — ReconcileState does not stat it
		bindAddr = "10.0.0.1"
		hostNQN  = "nqn.2026-01.io.example:initiator-restored"
	)
	wantNQN := "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-restarted"

	// Simulate post-restart state: fresh server, same configfs root, no
	// in-memory state.  The configfsRoot is a temp dir (no kernel configfs).
	mock := &agentE2EMockBackend{}
	env := newAgentE2EEnv(t, mock)
	ctx := context.Background()

	resp, err := env.client.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   volumeID,
				DevicePath: devPath,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams:      nvmeofTCPExportParams(bindAddr, 4420),
						AllowedInitiators: []string{hostNQN},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}

	// Response must contain one result for the volume.
	results := resp.GetResults()
	if len(results) != 1 {
		t.Fatalf("ReconcileState: len(Results) = %d, want 1", len(results))
	}
	result := results[0]
	if !result.GetSuccess() {
		t.Errorf("ReconcileState result.Success = false, ErrorMessage = %q", result.GetErrorMessage())
	}
	if result.GetVolumeId() != volumeID {
		t.Errorf("ReconcileState result.VolumeId = %q, want %q", result.GetVolumeId(), volumeID)
	}

	// Timestamp must be set.
	if resp.GetReconciledAt() == nil {
		t.Error("ReconcileState ReconciledAt is nil")
	}

	// Subsystem directory must exist.
	subDir := filepath.Join(env.cfgRoot, "nvmet", "subsystems", wantNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir missing after ReconcileState: %v", statErr)
	}

	// Allowed host directory must exist.
	hostDir := filepath.Join(env.cfgRoot, "nvmet", "hosts", hostNQN)
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("host dir missing after ReconcileState: %v", statErr)
	}

	// allowed_hosts symlink must exist.
	linkPath := filepath.Join(env.cfgRoot, "nvmet", "subsystems", wantNQN, "allowed_hosts", hostNQN)
	if _, lstatErr := os.Lstat(linkPath); lstatErr != nil {
		t.Errorf("allowed_hosts symlink missing after ReconcileState: %v", lstatErr)
	}

	// Calling ReconcileState again with the same desired state must succeed
	// (idempotent — useful for subsequent controller reconcile loops).
	resp2, err := env.client.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   volumeID,
				DevicePath: devPath,
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType:      agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams:      nvmeofTCPExportParams(bindAddr, 4420),
						AllowedInitiators: []string{hostNQN},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState (idempotent): %v", err)
	}
	if !resp2.GetResults()[0].GetSuccess() {
		t.Errorf("ReconcileState (idempotent) failed: %q", resp2.GetResults()[0].GetErrorMessage())
	}
}

// ----------------------------------------------------------------------------
// TestAgent_ErrorHandling
// ----------------------------------------------------------------------------.

// TestAgent_ErrorHandling verifies that the agent returns correct gRPC status
// codes for common error conditions.
func TestAgent_ErrorHandling(t *testing.T) {
	t.Parallel()

	mock := &agentE2EMockBackend{}
	env := newAgentE2EEnv(t, mock)
	ctx := context.Background()

	t.Run("invalid_volumeID_no_slash", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
			VolumeId:      "noslash",
			CapacityBytes: 1 << 30,
		})
		requireCode(t, err, codes.InvalidArgument)
	})

	t.Run("invalid_volumeID_empty", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
			VolumeId: "",
		})
		requireCode(t, err, codes.InvalidArgument)
	})

	t.Run("unknown_pool", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
			VolumeId:      "no-such-pool/pvc-x",
			CapacityBytes: 1 << 30,
		})
		requireCode(t, err, codes.NotFound)
	})

	t.Run("unknown_pool_expand", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
			VolumeId:       "ghost-pool/pvc-y",
			RequestedBytes: 2 << 30,
		})
		requireCode(t, err, codes.NotFound)
	})

	t.Run("unsupported_protocol_export", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
			VolumeId:     "tank/pvc-err",
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		})
		requireCode(t, err, codes.Unimplemented)
	})

	t.Run("unsupported_protocol_unexport", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
			VolumeId:     "tank/pvc-err",
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
		})
		requireCode(t, err, codes.Unimplemented)
	})

	t.Run("unsupported_protocol_allow", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
			VolumeId:     "tank/pvc-err",
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
			InitiatorId:  "WORKGROUP\\host",
		})
		requireCode(t, err, codes.Unimplemented)
	})

	t.Run("unsupported_protocol_deny", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
			VolumeId:     "tank/pvc-err",
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
			InitiatorId:  "iqn.2026-01.example:host",
		})
		requireCode(t, err, codes.Unimplemented)
	})

	t.Run("unknown_pool_capacity", func(t *testing.T) {
		t.Parallel()
		_, err := env.client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
			PoolName: "nonexistent",
		})
		requireCode(t, err, codes.NotFound)
	})

	t.Run("missing_nvmeof_params", func(t *testing.T) {
		t.Parallel()
		// ExportVolume with correct protocol but no NvmeofTcp params.
		_, err := env.client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
			VolumeId:     "tank/pvc-err",
			ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			// ExportParams is nil.
		})
		requireCode(t, err, codes.InvalidArgument)
	})
}

// requireCode fails t if err does not carry the expected gRPC status code.
func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != want {
		t.Errorf("gRPC code = %v, want %v (message: %q)", st.Code(), want, st.Message())
	}
}

// ----------------------------------------------------------------------------
// TestAgent_AllPhase1RPCs
// ----------------------------------------------------------------------------.

// TestAgent_AllPhase1RPCs verifies that every Phase 1 RPC is reachable and
// returns a non-nil response (no "unimplemented" default handler).  It does
// not deeply validate each response — the other tests cover that.
func TestAgent_AllPhase1RPCs(t *testing.T) {
	t.Parallel()

	fakeDevPath := createFakeDevice(t, "all-rpcs-zvol")
	mock := &agentE2EMockBackend{
		devicePath: fakeDevPath,
		totalBytes: 10 << 30,
		availBytes: 6 << 30,
	}
	env := newAgentE2EEnv(t, mock)
	ctx := context.Background()

	const volumeID = "tank/pvc-all-rpcs"

	// GetCapabilities
	if _, err := env.client.GetCapabilities(ctx, &agentv1.GetCapabilitiesRequest{}); err != nil {
		t.Errorf("GetCapabilities: %v", err)
	}

	// GetCapacity
	if _, err := env.client.GetCapacity(ctx, &agentv1.GetCapacityRequest{PoolName: "tank"}); err != nil {
		t.Errorf("GetCapacity: %v", err)
	}

	// HealthCheck
	if _, err := env.client.HealthCheck(ctx, &agentv1.HealthCheckRequest{}); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}

	// CreateVolume
	if _, err := env.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: 1 << 30,
	}); err != nil {
		t.Errorf("CreateVolume: %v", err)
	}

	// ExpandVolume
	if _, err := env.client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       volumeID,
		RequestedBytes: 2 << 30,
	}); err != nil {
		t.Errorf("ExpandVolume: %v", err)
	}

	// ListVolumes
	if _, err := env.client.ListVolumes(ctx, &agentv1.ListVolumesRequest{PoolName: "tank"}); err != nil {
		t.Errorf("ListVolumes: %v", err)
	}

	// ExportVolume (device path provided explicitly)
	if _, err := env.client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     volumeID,
		DevicePath:   fakeDevPath,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	}); err != nil {
		t.Errorf("ExportVolume: %v", err)
	}

	// ListExports
	if _, err := env.client.ListExports(ctx, &agentv1.ListExportsRequest{}); err != nil {
		t.Errorf("ListExports: %v", err)
	}

	// AllowInitiator
	const hostNQN = "nqn.2026-01.io.example:all-rpcs-host"
	if _, err := env.client.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	}); err != nil {
		t.Errorf("AllowInitiator: %v", err)
	}

	// DenyInitiator
	if _, err := env.client.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  hostNQN,
	}); err != nil {
		t.Errorf("DenyInitiator: %v", err)
	}

	// UnexportVolume
	if _, err := env.client.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	}); err != nil {
		t.Errorf("UnexportVolume: %v", err)
	}

	// ReconcileState
	if _, err := env.client.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{},
	}); err != nil {
		t.Errorf("ReconcileState: %v", err)
	}

	// DeleteVolume
	if _, err := env.client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: volumeID,
	}); err != nil {
		t.Errorf("DeleteVolume: %v", err)
	}
}
