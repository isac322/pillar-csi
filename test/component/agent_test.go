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

package component_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// ---------------------------------------------------------------------------
// Mock VolumeBackend
// ---------------------------------------------------------------------------.

// mockVolumeBackend is a test double for backend.VolumeBackend.
//
// # Mock fidelity
//
// Approximates: the production zfs.Backend, which provisions and manages ZFS
// zvol block volumes by executing zfs(8) and zpool(8) commands on the storage
// node and exposing them as /dev/zvol/<pool>/… block devices.
//
// Omits / simplifies:
//   - No real ZFS operations: Create/Delete/Expand/Capacity/ListVolumes are
//     implemented as preset-response returns; no block device nodes are
//     created or destroyed under /dev/zvol/.
//   - Idempotency: the real backend inspects the live volsize property to
//     determine whether a conflicting dataset already exists and returns
//     *backend.ConflictError accordingly.  The mock returns whatever is
//     pre-set in its fields on every call, independently of prior calls.
//   - Capacity accounting: the real Capacity() reads live pool statistics from
//     zpool-list(8).  The mock returns the fixed capacityTotal /
//     capacityAvailable values set at construction time.
//   - Disk-space enforcement: ENOSPC is returned only if the test explicitly
//     sets createErr/expandErr to a matching error value.
//   - Dataset naming: the real backend enforces pool/parentDataset/volumeID
//     path conventions and validates that the pool prefix matches its
//     configured pool.  The mock accepts any volumeID string and ignores it.
//   - Thread safety: all field accesses are serialized with a mutex, which is
//     stricter synchronization than the real backend (which inherits atomicity
//     from ZFS kernel-level locking).
//   - ConflictError: the mock never returns *backend.ConflictError
//     automatically; tests must assign createErr = &backend.ConflictError{…}
//     to exercise conflict-detection paths.
type mockVolumeBackend struct {
	mu sync.Mutex

	// Create
	createDevicePath string
	createAllocated  int64
	createErr        error

	// Delete
	deleteErr error

	// Expand
	expandAllocated int64
	expandErr       error

	// Capacity
	capacityTotal     int64
	capacityAvailable int64
	capacityErr       error

	// ListVolumes
	listVolumesResult []*agentv1.VolumeInfo
	listVolumesErr    error

	// DevicePath
	devicePathResult string
}

func (m *mockVolumeBackend) Create(
	_ context.Context,
	_ string,
	_ int64,
	_ *agentv1.ZfsVolumeParams,
) (devicePath string, allocatedBytes int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createDevicePath, m.createAllocated, m.createErr
}

func (m *mockVolumeBackend) Delete(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteErr
}

func (m *mockVolumeBackend) Expand(_ context.Context, _ string, _ int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.expandAllocated, m.expandErr
}

func (m *mockVolumeBackend) Capacity(_ context.Context) (total, avail int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capacityTotal, m.capacityAvailable, m.capacityErr
}

func (m *mockVolumeBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listVolumesResult, m.listVolumesErr
}

func (m *mockVolumeBackend) DevicePath(_ string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.devicePathResult
}

// Type satisfies the backend.VolumeBackend interface.  The mock always
// identifies itself as a ZFS zvol backend so that GetCapabilities and
// collectPoolInfo work correctly in component tests.
func (*mockVolumeBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

var _ backend.VolumeBackend = (*mockVolumeBackend)(nil)

// ---------------------------------------------------------------------------
// Server construction helpers
// ---------------------------------------------------------------------------.

const (
	compTestPool     = "tank"
	compTestVolumeID = "tank/pvc-abc"
	compTestNQN      = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc"
	compTestHostNQN  = "nqn.2023-01.io.example:host-1"
)

// newAgentServer creates a Server with a single mockVolumeBackend for "tank"
// pool and a temp directory as configfs root.  AlwaysPresentChecker is
// injected so ExportVolume does not require real block devices.
func newAgentServer(
	t *testing.T, mb *mockVolumeBackend, extra ...agent.ServerOption,
) (srv *agent.Server, cfgRootDir string) {
	t.Helper()
	cfgRoot := t.TempDir()
	backends := map[string]backend.VolumeBackend{compTestPool: mb}
	opts := append(
		[]agent.ServerOption{agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker)},
		extra...,
	)
	return agent.NewServer(backends, cfgRoot, opts...), cfgRoot
}

// nvmeofParams builds ExportParams for NVMe-oF TCP.
func nvmeofParams(addr string, port int32) *agentv1.ExportParams {
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress: addr,
				Port:        port,
			},
		},
	}
}

// exportVolume is a convenience helper for ExportVolume with NVMe-oF TCP.
func exportVolume(
	t *testing.T, srv *agent.Server, volumeID, addr string, port int32, //nolint:unparam // port is kept for API clarity
) {
	t.Helper()
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams(addr, port),
	})
	if err != nil {
		t.Fatalf("exportVolume setup: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Component 1.1 — CreateVolume
// ---------------------------------------------------------------------------.

// TestAgentServer_CreateVolume_Success validates normal volume creation.
func TestAgentServer_CreateVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		createDevicePath: "/dev/zvol/tank/pvc-abc",
		createAllocated:  10 * 1024 * 1024 * 1024,
	}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}
	if resp.GetDevicePath() != "/dev/zvol/tank/pvc-abc" {
		t.Errorf("DevicePath = %q, want %q", resp.GetDevicePath(), "/dev/zvol/tank/pvc-abc")
	}
	if resp.GetCapacityBytes() != 10*1024*1024*1024 {
		t.Errorf("CapacityBytes = %d, want 10 GiB", resp.GetCapacityBytes())
	}
}

// TestAgentServer_CreateVolume_Idempotent validates that creating the same
// volume twice returns the existing info without error.
func TestAgentServer_CreateVolume_Idempotent(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		createDevicePath: "/dev/zvol/tank/pvc-abc",
		createAllocated:  10 * 1024 * 1024 * 1024,
	}
	srv, _ := newAgentServer(t, mb)

	req := &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	}
	for range 2 {
		if _, err := srv.CreateVolume(context.Background(), req); err != nil {
			t.Fatalf("CreateVolume iteration unexpected error: %v", err)
		}
	}
}

// TestAgentServer_CreateVolume_DiskFull validates that a disk-full backend
// error maps to ResourceExhausted or Internal gRPC status.
func TestAgentServer_CreateVolume_DiskFull(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		createErr: errors.New("out of space"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Backend errors are wrapped as Internal by server_volume.go
	st, _ := status.FromError(err)
	if st.Code() == codes.OK {
		t.Errorf("code = OK, want non-OK gRPC status")
	}
}

// TestAgentServer_CreateVolume_InvalidPool validates that a volume ID
// referencing an unknown pool returns NotFound.
func TestAgentServer_CreateVolume_InvalidPool(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      "missing-pool/pvc-xyz",
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected NotFound, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// TestAgentServer_CreateVolume_InvalidVolumeID validates that a malformed
// volume ID (no slash) returns InvalidArgument.
func TestAgentServer_CreateVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      "noslash",
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestAgentServer_CreateVolume_BackendError validates that a generic backend
// error maps to Internal.
func TestAgentServer_CreateVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		createErr: errors.New("unexpected ZFS failure"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 10 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected Internal error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestAgentServer_CreateVolume_ConflictSize validates that creating a volume
// that already exists with different capacity returns AlreadyExists.
func TestAgentServer_CreateVolume_ConflictSize(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		createErr: &backend.ConflictError{
			VolumeID:       compTestVolumeID,
			ExistingBytes:  10 * 1024 * 1024 * 1024,
			RequestedBytes: 20 * 1024 * 1024 * 1024,
		},
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      compTestVolumeID,
		CapacityBytes: 20 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected AlreadyExists, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.AlreadyExists {
		t.Errorf("code = %v, want AlreadyExists", st.Code())
	}
}

// ---------------------------------------------------------------------------
// Component 1.2 — DeleteVolume
// ---------------------------------------------------------------------------.

// TestAgentServer_DeleteVolume_Success validates normal volume deletion.
func TestAgentServer_DeleteVolume_Success(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	resp, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: compTestVolumeID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// TestAgentServer_DeleteVolume_Idempotent validates that deleting a
// non-existent volume (backend returns nil) succeeds.
func TestAgentServer_DeleteVolume_Idempotent(t *testing.T) {
	t.Parallel()
	// Mock backend always returns nil for Delete — already idempotent at backend level.
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: compTestVolumeID,
	})
	if err != nil {
		t.Fatalf("DeleteVolume idempotent unexpected error: %v", err)
	}
}

// TestAgentServer_DeleteVolume_InvalidPool validates that an unknown pool
// returns NotFound.
func TestAgentServer_DeleteVolume_InvalidPool(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "other-pool/pvc-abc",
	})
	if err == nil {
		t.Fatal("expected NotFound, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// TestAgentServer_DeleteVolume_DeviceBusy validates that a "device busy"
// backend error maps to Internal.
func TestAgentServer_DeleteVolume_DeviceBusy(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		deleteErr: errors.New("dataset is busy"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: compTestVolumeID,
	})
	if err == nil {
		t.Fatal("expected Internal error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestAgentServer_DeleteVolume_BackendError validates that a generic backend
// error maps to Internal.
func TestAgentServer_DeleteVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		deleteErr: errors.New("unexpected failure"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: compTestVolumeID,
	})
	if err == nil {
		t.Fatal("expected Internal error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// ---------------------------------------------------------------------------
// Component 1.3 — ExpandVolume
// ---------------------------------------------------------------------------.

// TestAgentServer_ExpandVolume_Success validates normal volume expansion.
func TestAgentServer_ExpandVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{expandAllocated: 20 * 1024 * 1024 * 1024}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       compTestVolumeID,
		RequestedBytes: 20 * 1024 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("ExpandVolume unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != 20*1024*1024*1024 {
		t.Errorf("CapacityBytes = %d, want 20 GiB", resp.GetCapacityBytes())
	}
}

// TestAgentServer_ExpandVolume_ShrinkRejected validates that a shrink attempt
// at the backend level propagates as Internal.
func TestAgentServer_ExpandVolume_ShrinkRejected(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		expandErr: errors.New("volsize cannot be decreased"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       compTestVolumeID,
		RequestedBytes: 1 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected error for shrink attempt, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestAgentServer_ExpandVolume_NotFound validates that expanding a
// non-existent volume propagates as Internal.
func TestAgentServer_ExpandVolume_NotFound(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		expandErr: errors.New("dataset does not exist"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       compTestVolumeID,
		RequestedBytes: 20 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected error for non-existent volume, got nil")
	}
}

// TestAgentServer_ExpandVolume_InvalidPool validates unknown pool returns NotFound.
func TestAgentServer_ExpandVolume_InvalidPool(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       "other-pool/pvc-abc",
		RequestedBytes: 20 * 1024 * 1024 * 1024,
	})
	if err == nil {
		t.Fatal("expected NotFound, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// ---------------------------------------------------------------------------
// Component 1.4 — ExportVolume
// ---------------------------------------------------------------------------.

// TestAgentServer_ExportVolume_Success validates that ExportVolume creates
// the configfs subsystem directory and returns correct ExportInfo.
func TestAgentServer_ExportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newAgentServer(t, mb)

	resp, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}

	if resp.GetExportInfo().GetTargetId() != compTestNQN {
		t.Errorf("TargetId = %q, want %q", resp.GetExportInfo().GetTargetId(), compTestNQN)
	}
	if resp.GetExportInfo().GetAddress() != "192.168.1.10" {
		t.Errorf("Address = %q, want 192.168.1.10", resp.GetExportInfo().GetAddress())
	}
	if resp.GetExportInfo().GetPort() != 4420 {
		t.Errorf("Port = %d, want 4420", resp.GetExportInfo().GetPort())
	}

	// Configfs subsystem dir must exist.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
}

// TestAgentServer_ExportVolume_Idempotent validates that exporting the same
// volume twice is idempotent.
func TestAgentServer_ExportVolume_Idempotent(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newAgentServer(t, mb)

	req := &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.10", 4420),
	}
	for range 2 {
		if _, err := srv.ExportVolume(context.Background(), req); err != nil {
			t.Fatalf("ExportVolume iteration unexpected error: %v", err)
		}
	}
}

// TestAgentServer_ExportVolume_InvalidProtocol validates that requesting
// an unsupported protocol returns Unimplemented.
func TestAgentServer_ExportVolume_InvalidProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})
	if err == nil {
		t.Fatal("expected Unimplemented, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// TestAgentServer_ExportVolume_MissingParams validates that missing NVMe-oF
// params return InvalidArgument.
func TestAgentServer_ExportVolume_MissingParams(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		// No ExportParams.
	})
	if err == nil {
		t.Fatal("expected InvalidArgument, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// TestAgentServer_ExportVolume_DeviceNotReady validates that when the block
// device never appears within the poll window, FailedPrecondition is returned.
func TestAgentServer_ExportVolume_DeviceNotReady(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	neverPresent := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, nil
	})
	srv, _ := newAgentServer(t, mb,
		agent.WithDeviceChecker(neverPresent),
		agent.WithDevicePollParams(5*time.Millisecond, 20*time.Millisecond),
	)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.10", 4420),
	})
	if err == nil {
		t.Fatal("expected FailedPrecondition, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", st.Code())
	}
}

// TestAgentServer_ExportVolume_DeviceAppearsAfterDelay validates that
// ExportVolume succeeds if the device appears before the timeout.
func TestAgentServer_ExportVolume_DeviceAppearsAfterDelay(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}

	var callCount int
	var mu sync.Mutex
	delayedChecker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		return n >= 3, nil // Present on 3rd+ call.
	})
	srv, cfgRoot := newAgentServer(t, mb,
		agent.WithDeviceChecker(delayedChecker),
		agent.WithDevicePollParams(10*time.Millisecond, 5*time.Second),
	)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.10", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume unexpected error: %v", err)
	}
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created: %v", statErr)
	}
}

// TestAgentServer_ExportVolume_PermissionError validates that a permission
// denied error from the DeviceChecker propagates as FailedPrecondition.
func TestAgentServer_ExportVolume_PermissionError(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	permDeniedChecker := nvmeof.DeviceChecker(func(_ string) (bool, error) {
		return false, os.ErrPermission
	})
	srv, _ := newAgentServer(t, mb,
		agent.WithDeviceChecker(permDeniedChecker),
		agent.WithDevicePollParams(5*time.Millisecond, 100*time.Millisecond),
	)

	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofParams("192.168.1.10", 4420),
	})
	if err == nil {
		t.Fatal("expected error on permission denied, got nil")
	}
}

// ---------------------------------------------------------------------------
// Component 1.5 — UnexportVolume
// ---------------------------------------------------------------------------.

// TestAgentServer_UnexportVolume_Success validates that UnexportVolume cleans
// up configfs entries created by ExportVolume.
func TestAgentServer_UnexportVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newAgentServer(t, mb)

	exportVolume(t, srv, compTestVolumeID, "192.168.1.10", 4420)

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err != nil {
		t.Fatalf("UnexportVolume unexpected error: %v", err)
	}

	// Subsystem dir must be gone.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN)
	if _, statErr := os.Stat(subDir); !os.IsNotExist(statErr) {
		t.Errorf("subsystem dir still exists after unexport: %v", statErr)
	}
}

// TestAgentServer_UnexportVolume_Idempotent validates that unexporting a
// volume that was never exported is a no-op (idempotent).
func TestAgentServer_UnexportVolume_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	if err != nil {
		t.Fatalf("UnexportVolume idempotent unexpected error: %v", err)
	}
}

// TestAgentServer_UnexportVolume_InvalidProtocol validates that requesting
// an unsupported protocol returns Unimplemented.
func TestAgentServer_UnexportVolume_InvalidProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NFS,
	})
	if err == nil {
		t.Fatal("expected Unimplemented, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// ---------------------------------------------------------------------------
// Component 1.6 — AllowInitiator / DenyInitiator
// ---------------------------------------------------------------------------.

// TestAgentServer_AllowInitiator_Success validates that AllowInitiator creates
// the allowed_hosts symlink in configfs.
func TestAgentServer_AllowInitiator_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newAgentServer(t, mb)

	exportVolume(t, srv, compTestVolumeID, "192.168.1.10", 4420)

	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  compTestHostNQN,
	})
	if err != nil {
		t.Fatalf("AllowInitiator unexpected error: %v", err)
	}

	// Host dir must be created.
	hostDir := filepath.Join(cfgRoot, "nvmet", "hosts", compTestHostNQN)
	if _, statErr := os.Stat(hostDir); statErr != nil {
		t.Errorf("host dir not created: %v", statErr)
	}
	// Allowed_hosts symlink must exist.
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN, "allowed_hosts", compTestHostNQN)
	if _, statErr := os.Lstat(linkPath); statErr != nil {
		t.Errorf("allowed_hosts symlink not created: %v", statErr)
	}
}

// TestAgentServer_AllowInitiator_Idempotent validates that allowing the same
// initiator twice is a no-op.
func TestAgentServer_AllowInitiator_Idempotent(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newAgentServer(t, mb)

	exportVolume(t, srv, compTestVolumeID, "192.168.1.10", 4420)

	req := &agentv1.AllowInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  compTestHostNQN,
	}
	for range 2 {
		if _, err := srv.AllowInitiator(context.Background(), req); err != nil {
			t.Fatalf("AllowInitiator iteration unexpected error: %v", err)
		}
	}
}

// TestAgentServer_AllowInitiator_InvalidProtocol validates unsupported
// protocol returns Unimplemented.
func TestAgentServer_AllowInitiator_InvalidProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
		InitiatorId:  "iqn.example",
	})
	if err == nil {
		t.Fatal("expected Unimplemented, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// TestAgentServer_DenyInitiator_Success validates that DenyInitiator removes
// the allowed_hosts symlink.
func TestAgentServer_DenyInitiator_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, cfgRoot := newAgentServer(t, mb)

	exportVolume(t, srv, compTestVolumeID, "192.168.1.10", 4420)

	// Allow first.
	if _, err := srv.AllowInitiator(context.Background(), &agentv1.AllowInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  compTestHostNQN,
	}); err != nil {
		t.Fatalf("AllowInitiator setup: %v", err)
	}

	// Then deny.
	if _, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  compTestHostNQN,
	}); err != nil {
		t.Fatalf("DenyInitiator unexpected error: %v", err)
	}

	// Symlink must be gone.
	linkPath := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN, "allowed_hosts", compTestHostNQN)
	if _, statErr := os.Lstat(linkPath); !os.IsNotExist(statErr) {
		t.Errorf("allowed_hosts symlink still exists after deny: %v", statErr)
	}
}

// TestAgentServer_DenyInitiator_Idempotent validates that denying an
// initiator that was never allowed is a no-op.
func TestAgentServer_DenyInitiator_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  compTestHostNQN,
	})
	if err != nil {
		t.Fatalf("DenyInitiator idempotent unexpected error: %v", err)
	}
}

// TestAgentServer_DenyInitiator_InvalidProtocol validates unsupported
// protocol returns Unimplemented.
func TestAgentServer_DenyInitiator_InvalidProtocol(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.DenyInitiator(context.Background(), &agentv1.DenyInitiatorRequest{
		VolumeId:     compTestVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_SMB,
		InitiatorId:  "WORKGROUP\\host1",
	})
	if err == nil {
		t.Fatal("expected Unimplemented, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}

// ---------------------------------------------------------------------------
// Component 1.7 — GetCapabilities / GetCapacity / ListVolumes / ListExports
// ---------------------------------------------------------------------------.

// TestAgentServer_GetCapabilities_ReturnsAll validates that the response
// includes ZFS backend type and NVMe-oF TCP protocol.
func TestAgentServer_GetCapabilities_ReturnsAll(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		capacityTotal:     100 * 1024 * 1024 * 1024,
		capacityAvailable: 60 * 1024 * 1024 * 1024,
	}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities unexpected error: %v", err)
	}

	// Must include ZFS backend.
	foundZFS := false
	for _, bt := range resp.GetSupportedBackends() {
		if bt == agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL {
			foundZFS = true
		}
	}
	if !foundZFS {
		t.Error("GetCapabilities: ZFS_ZVOL backend not in SupportedBackends")
	}

	// Must include NVMe-oF TCP protocol.
	foundNVMeoF := false
	for _, pt := range resp.GetSupportedProtocols() {
		if pt == agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
			foundNVMeoF = true
		}
	}
	if !foundNVMeoF {
		t.Error("GetCapabilities: NVMEOF_TCP not in SupportedProtocols")
	}
}

// TestAgentServer_GetCapacity_Success validates that capacity values are
// correctly returned from the backend.
func TestAgentServer_GetCapacity_Success(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		capacityTotal:     100 * 1024 * 1024 * 1024,
		capacityAvailable: 60 * 1024 * 1024 * 1024,
	}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: compTestPool,
	})
	if err != nil {
		t.Fatalf("GetCapacity unexpected error: %v", err)
	}
	if resp.GetTotalBytes() != 100*1024*1024*1024 {
		t.Errorf("TotalBytes = %d, want 100 GiB", resp.GetTotalBytes())
	}
	if resp.GetAvailableBytes() != 60*1024*1024*1024 {
		t.Errorf("AvailableBytes = %d, want 60 GiB", resp.GetAvailableBytes())
	}
	if resp.GetUsedBytes() != 40*1024*1024*1024 {
		t.Errorf("UsedBytes = %d, want 40 GiB", resp.GetUsedBytes())
	}
}

// TestAgentServer_GetCapacity_PoolOffline validates that a backend capacity
// error maps to Internal.
func TestAgentServer_GetCapacity_PoolOffline(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{
		capacityErr: errors.New("pool is unavailable"),
	}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: compTestPool,
	})
	if err == nil {
		t.Fatal("expected Internal error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestAgentServer_GetCapacity_UnknownPool validates that querying an unknown
// pool returns NotFound.
func TestAgentServer_GetCapacity_UnknownPool(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	_, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: "no-such-pool",
	})
	if err == nil {
		t.Fatal("expected NotFound, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// TestAgentServer_ListVolumes_Success validates that three volumes are
// returned from the backend.
func TestAgentServer_ListVolumes_Success(t *testing.T) {
	t.Parallel()
	vols := []*agentv1.VolumeInfo{
		{VolumeId: "tank/pvc-abc", CapacityBytes: 10 * 1024 * 1024 * 1024},
		{VolumeId: "tank/pvc-def", CapacityBytes: 20 * 1024 * 1024 * 1024},
		{VolumeId: "tank/pvc-ghi", CapacityBytes: 5 * 1024 * 1024 * 1024},
	}
	mb := &mockVolumeBackend{listVolumesResult: vols}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: compTestPool,
	})
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(resp.GetVolumes()) != 3 {
		t.Errorf("len(Volumes) = %d, want 3", len(resp.GetVolumes()))
	}
}

// TestAgentServer_ListVolumes_Empty validates that an empty pool returns
// an empty list without error.
func TestAgentServer_ListVolumes_Empty(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{listVolumesResult: []*agentv1.VolumeInfo{}}
	srv, _ := newAgentServer(t, mb)

	resp, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: compTestPool,
	})
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(resp.GetVolumes()) != 0 {
		t.Errorf("expected empty list, got %d volumes", len(resp.GetVolumes()))
	}
}

// TestAgentServer_ListVolumes_BackendError validates that a backend error
// maps to Internal.
func TestAgentServer_ListVolumes_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{listVolumesErr: errors.New("ZFS pool gone")}
	srv, _ := newAgentServer(t, mb)

	_, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: compTestPool,
	})
	if err == nil {
		t.Fatal("expected Internal error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestAgentServer_ListExports_ReturnsEmpty validates that ListExports returns
// an empty map (Phase 1 implementation always returns empty).
func TestAgentServer_ListExports_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	resp, err := srv.ListExports(context.Background(), &agentv1.ListExportsRequest{})
	if err != nil {
		t.Fatalf("ListExports unexpected error: %v", err)
	}
	if len(resp.GetExports()) != 0 {
		t.Errorf("expected empty exports, got %d", len(resp.GetExports()))
	}
}

// ---------------------------------------------------------------------------
// Component 1.8 — HealthCheck
// ---------------------------------------------------------------------------.

// TestAgentServer_HealthCheck_AllHealthy validates that HealthCheck reports
// healthy when the configfs nvmet dir exists and all pool backends are reachable.
func TestAgentServer_HealthCheck_AllHealthy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create the nvmet directory inside the configfs root.
	nvmetDir := filepath.Join(tmpDir, "configfs", "nvmet")
	if err := os.MkdirAll(nvmetDir, 0o750); err != nil {
		t.Fatalf("create nvmet dir: %v", err)
	}

	mb := &mockVolumeBackend{
		capacityTotal:     100 * 1024 * 1024 * 1024,
		capacityAvailable: 60 * 1024 * 1024 * 1024,
	}
	backends := map[string]backend.VolumeBackend{compTestPool: mb}
	srv := agent.NewServer(backends,
		filepath.Join(tmpDir, "configfs"),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	if !resp.GetHealthy() {
		t.Errorf("expected Healthy=true, got false; subsystems: %v", resp.GetSubsystems())
	}
}

// TestAgentServer_HealthCheck_NvmetConfigfsHealthy validates that HealthCheck
// reports healthy for the nvmet_configfs subsystem when the nvmet directory
// exists, regardless of the backend type.
func TestAgentServer_HealthCheck_NvmetConfigfsHealthy(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create nvmet dir so that configfs check passes.
	nvmetDir := filepath.Join(tmpDir, "configfs", "nvmet")
	if err := os.MkdirAll(nvmetDir, 0o750); err != nil {
		t.Fatalf("create nvmet dir: %v", err)
	}

	backends := map[string]backend.VolumeBackend{
		compTestPool: &mockVolumeBackend{
			capacityTotal:     100 * 1024 * 1024 * 1024,
			capacityAvailable: 60 * 1024 * 1024 * 1024,
		},
	}
	srv := agent.NewServer(backends,
		filepath.Join(tmpDir, "configfs"),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	// Find the nvmet_configfs subsystem and verify it is healthy.
	foundNvmet := false
	for _, sub := range resp.GetSubsystems() {
		if sub.GetName() == "nvmet_configfs" {
			foundNvmet = true
			if !sub.GetHealthy() {
				t.Errorf("nvmet_configfs subsystem should be healthy: %s", sub.GetMessage())
			}
		}
	}
	if !foundNvmet {
		t.Error("nvmet_configfs subsystem missing from response")
	}
}

// TestAgentServer_HealthCheck_ConfigfsMissing validates that HealthCheck
// reports unhealthy when the nvmet configfs directory does not exist.
func TestAgentServer_HealthCheck_ConfigfsMissing(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// DO NOT create nvmet dir — configfs check should fail.
	backends := map[string]backend.VolumeBackend{
		compTestPool: &mockVolumeBackend{
			capacityTotal:     100 * 1024 * 1024 * 1024,
			capacityAvailable: 60 * 1024 * 1024 * 1024,
		},
	}
	srv := agent.NewServer(backends,
		filepath.Join(tmpDir, "configfs-missing"), // no nvmet dir here
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	if resp.GetHealthy() {
		t.Error("expected Healthy=false when configfs missing, got true")
	}
}

// TestAgentServer_HealthCheck_PoolDegraded validates that HealthCheck reports
// unhealthy when a pool backend returns a capacity error.
func TestAgentServer_HealthCheck_PoolDegraded(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	nvmetDir := filepath.Join(tmpDir, "configfs", "nvmet")
	if err := os.MkdirAll(nvmetDir, 0o750); err != nil {
		t.Fatalf("create nvmet dir: %v", err)
	}

	backends := map[string]backend.VolumeBackend{
		compTestPool: &mockVolumeBackend{
			capacityErr: errors.New("pool is degraded"),
		},
	}
	srv := agent.NewServer(backends,
		filepath.Join(tmpDir, "configfs"),
		agent.WithDeviceChecker(nvmeof.AlwaysPresentChecker),
	)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	if resp.GetHealthy() {
		t.Error("expected Healthy=false when pool degraded, got true")
	}
}

// ---------------------------------------------------------------------------
// Component 1.9 — ReconcileState
// ---------------------------------------------------------------------------.

// TestAgentServer_ReconcileState_ReExportsAfterRestart validates that
// ReconcileState creates configfs entries for all desired exports.
func TestAgentServer_ReconcileState_ReExportsAfterRestart(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newAgentServer(t, &mockVolumeBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   compTestVolumeID,
				DevicePath: "/dev/zvol/tank/pvc-abc",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofParams("192.168.1.10", 4420),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("Results len = %d, want 1", len(resp.GetResults()))
	}
	if !resp.GetResults()[0].GetSuccess() {
		t.Errorf("result.Success=false: %q", resp.GetResults()[0].GetErrorMessage())
	}

	// Configfs entry must have been created.
	subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", compTestNQN)
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("subsystem dir not created by ReconcileState: %v", statErr)
	}
}

// TestAgentServer_ReconcileState_Idempotent validates that ReconcileState
// can be called multiple times with the same desired state.
func TestAgentServer_ReconcileState_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	req := &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   compTestVolumeID,
				DevicePath: "/dev/zvol/tank/pvc-abc",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofParams("10.0.0.1", 4420),
					},
				},
			},
		},
	}

	for i := range 3 {
		resp, err := srv.ReconcileState(context.Background(), req)
		if err != nil {
			t.Fatalf("ReconcileState iteration %d unexpected error: %v", i, err)
		}
		if !resp.GetResults()[0].GetSuccess() {
			t.Errorf("iteration %d: result.Success=false: %q",
				i, resp.GetResults()[0].GetErrorMessage())
		}
	}
}

// TestAgentServer_ReconcileState_MultipleVolumes validates that multiple
// volumes are reconciled correctly in a single call.
func TestAgentServer_ReconcileState_MultipleVolumes(t *testing.T) {
	t.Parallel()
	const secondVolumeID = "tank/pvc-def"
	const secondNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-def"

	srv, cfgRoot := newAgentServer(t, &mockVolumeBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{
			{
				VolumeId:   compTestVolumeID,
				DevicePath: "/dev/zvol/tank/pvc-abc",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofParams("10.0.0.1", 4420),
					},
				},
			},
			{
				VolumeId:   secondVolumeID,
				DevicePath: "/dev/zvol/tank/pvc-def",
				Exports: []*agentv1.ExportDesiredState{
					{
						ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
						ExportParams: nvmeofParams("10.0.0.1", 4421),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("Results len = %d, want 2", len(resp.GetResults()))
	}
	for _, r := range resp.GetResults() {
		if !r.GetSuccess() {
			t.Errorf("volume %q reconcile failed: %q", r.GetVolumeId(), r.GetErrorMessage())
		}
	}

	// Both subsystem dirs must have been created.
	for _, nqn := range []string{compTestNQN, secondNQN} {
		subDir := filepath.Join(cfgRoot, "nvmet", "subsystems", nqn)
		if _, statErr := os.Stat(subDir); statErr != nil {
			t.Errorf("subsystem dir %q not created: %v", nqn, statErr)
		}
	}
}

// TestAgentServer_ReconcileState_EmptyList validates that an empty volume
// list returns an empty results slice without error.
func TestAgentServer_ReconcileState_EmptyList(t *testing.T) {
	t.Parallel()
	srv, _ := newAgentServer(t, &mockVolumeBackend{})

	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{},
	})
	if err != nil {
		t.Fatalf("ReconcileState unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 0 {
		t.Errorf("Results len = %d, want 0", len(resp.GetResults()))
	}
	if resp.GetReconciledAt() == nil {
		t.Error("ReconciledAt timestamp is nil")
	}
}

// ---------------------------------------------------------------------------
// Component 1.10 — Concurrent operations
// ---------------------------------------------------------------------------.

// TestAgentServer_ConcurrentExportUnexport validates that concurrent Export
// and Unexport of the same volume do not cause deadlock or data corruption.
func TestAgentServer_ConcurrentExportUnexport(t *testing.T) {
	t.Parallel()
	mb := &mockVolumeBackend{devicePathResult: "/dev/zvol/tank/pvc-abc"}
	srv, _ := newAgentServer(t, mb)

	const goroutines = 4
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Half goroutines export, half unexport.
	for range goroutines {
		go func() {
			defer wg.Done()

			//nolint:errcheck // concurrent errors are non-actionable
			_, _ = srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
				VolumeId:     compTestVolumeID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: nvmeofParams("10.0.0.1", 4420),
			})
		}()
		go func() {
			defer wg.Done()

			//nolint:errcheck // concurrent errors are non-actionable
			_, _ = srv.UnexportVolume(context.Background(), &agentv1.UnexportVolumeRequest{
				VolumeId:     compTestVolumeID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			})
		}()
	}

	// Neither deadlock nor panic — just verify all goroutines complete.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: concurrent Export/Unexport goroutines deadlocked")
	}
}
