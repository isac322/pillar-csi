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

package agent_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const (
	testPool     = "tank"
	testVolumeID = "tank/pvc-abc"
)

// mockBackend is a test double for the VolumeBackend interface.
// Each method records whether it was called and returns the configured outputs.
type mockBackend struct {
	// Create
	createDevicePath string
	createAllocated  int64
	createErr        error
	createCalledWith []createArgs
	// Delete
	deleteErr        error
	deleteCalledWith []string
	// Expand
	expandAllocated int64
	expandErr       error
	// Capacity
	capacityTotal     int64
	capacityAvailable int64
	capacityErr       error
	// Layout
	layout backend.Layout
	// ListVolumes
	listVolumesResult []*agentv1.VolumeInfo
	listVolumesErr    error
	// DevicePath
	devicePathResult string
}

type createArgs struct {
	volumeID      string
	capacityBytes int64
}

func (m *mockBackend) Create(
	_ context.Context,
	volumeID string,
	capacityBytes int64,
	_ *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	m.createCalledWith = append(m.createCalledWith, createArgs{volumeID, capacityBytes})
	return m.createDevicePath, m.createAllocated, m.createErr
}

func (m *mockBackend) Delete(_ context.Context, volumeID string) error {
	m.deleteCalledWith = append(m.deleteCalledWith, volumeID)
	return m.deleteErr
}

func (m *mockBackend) Expand(_ context.Context, _ string, _ int64) (allocatedBytes int64, err error) {
	return m.expandAllocated, m.expandErr
}

func (m *mockBackend) Capacity(_ context.Context) (totalBytes, availableBytes int64, err error) {
	return m.capacityTotal, m.capacityAvailable, m.capacityErr
}

func (m *mockBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	return m.listVolumesResult, m.listVolumesErr
}

func (m *mockBackend) DevicePath(_ string) string {
	return m.devicePathResult
}

// Type returns BACKEND_TYPE_ZFS_ZVOL so that the mock satisfies the
// VolumeBackend interface and makes GetCapabilities / collectPoolInfo tests
// pass without hardcoding a backend type in production code.
func (*mockBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

func (m *mockBackend) Layout() backend.Layout { return m.layout }

// Ensure mockBackend satisfies the interface.
var _ backend.VolumeBackend = (*mockBackend)(nil)

// newTestServer creates a Server with a single mock backend for pool "tank".
// Fencing marks are kept in a per-test state directory.
func newTestServer(t *testing.T, mb *mockBackend) *agent.Server {
	t.Helper()
	backends := map[string]backend.VolumeBackend{
		testPool: mb,
	}
	return agent.NewServer(backends, "", agent.WithDrainStateDir(t.TempDir()))
}

// testFence returns the fencing token of the test's single volume lifecycle.
// The agent rejects mutations without a token.  The UID is the test name, so
// each test's lifecycle is unique and every op in the test reuses it.
func testFence(t *testing.T) *agentv1.FencingToken {
	t.Helper()
	return &agentv1.FencingToken{VolumeUid: t.Name(), Generation: 1}
}

// CreateVolume tests.
func TestCreateVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		createDevicePath: "/dev/zvol/tank/pvc-abc",
		createAllocated:  1 << 30, // 1 GiB
	}
	srv := newTestServer(t, mb)

	resp, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:         testFence(t),
		CapacityBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}
	if resp.GetDevicePath() != "/dev/zvol/tank/pvc-abc" {
		t.Errorf("DevicePath = %q, want %q", resp.GetDevicePath(), "/dev/zvol/tank/pvc-abc")
	}
	if resp.GetCapacityBytes() != 1<<30 {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), 1<<30)
	}
	if len(mb.createCalledWith) != 1 || mb.createCalledWith[0].volumeID != testVolumeID {
		t.Errorf("backend.Create called with %v, want volumeID %q", mb.createCalledWith, testVolumeID)
	}
}

func TestCreateVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{}
	srv := newTestServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:    "no-slash",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       testFence(t),
	})
	if err == nil {
		t.Fatal("expected error for invalid volumeID, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestCreateVolume_UnknownPool(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{}
	srv := newTestServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:    "other-pool/pvc-xyz",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       testFence(t),
	})
	if err == nil {
		t.Fatal("expected error for unknown pool, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestCreateVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{createErr: errors.New("disk full")}
	srv := newTestServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:         testFence(t),
		CapacityBytes: 1 << 30,
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

func TestCreateVolume_ConflictSize(t *testing.T) {
	t.Parallel()
	// Simulate a volume that already exists with 2 GiB but caller requests 1 GiB.
	mb := &mockBackend{
		createErr: &backend.ConflictError{
			VolumeID:       testVolumeID,
			ExistingBytes:  2 << 30,
			RequestedBytes: 1 << 30,
		},
	}
	srv := newTestServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:         testFence(t),
		CapacityBytes: 1 << 30,
	})
	if err == nil {
		t.Fatal("expected AlreadyExists error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.AlreadyExists {
		t.Errorf("code = %v, want AlreadyExists", st.Code())
	}
	if !strings.Contains(st.Message(), "already exists") {
		t.Errorf("message %q does not mention 'already exists'", st.Message())
	}
}

// TestCreateVolume_LayoutMismatch verifies that a backend refusing a create
// because the declared layout differs from its configuration (issue #113)
// surfaces as FailedPrecondition, not Internal: retrying cannot succeed until
// an operator aligns the PillarStore with the agent config file.
func TestCreateVolume_LayoutMismatch(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		createErr: fmt.Errorf("zfs: create %q: %w", testVolumeID, &backend.LayoutMismatchError{
			VolumeID: testVolumeID, Setting: "ZFS parent dataset", Requested: "k8s", Configured: "",
		}),
	}
	srv := newTestServer(t, mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:         testFence(t),
		CapacityBytes: 1 << 30,
	})
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v (err %v), want FailedPrecondition", st.Code(), err)
	}
	if !strings.Contains(st.Message(), `requested ZFS parent dataset "k8s"`) {
		t.Errorf("message %q lost the mismatch detail", st.Message())
	}
}

// TestCreateVolume_BackendTypeRejected verifies that CreateVolume refuses a
// backend_type that is missing, names a backend kind the agent does not
// implement, or differs from the backend owning the volume's pool, and that
// the backend is never asked to create anything in those cases.
func TestCreateVolume_BackendTypeRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		backendType agentv1.BackendType
		want        codes.Code
	}{
		{"unspecified", agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED, codes.InvalidArgument},
		{"zfs dataset", agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, codes.Unimplemented},
		{"directory", agentv1.BackendType_BACKEND_TYPE_DIRECTORY, codes.Unimplemented},
		{"lvm on a zfs pool", agentv1.BackendType_BACKEND_TYPE_LVM, codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mb := &mockBackend{createDevicePath: "/dev/zvol/tank/pvc-abc", createAllocated: 1 << 30}
			srv := newTestServer(t, mb)

			_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
				VolumeId:      testVolumeID,
				BackendType:   tc.backendType,
				Fence:         testFence(t),
				CapacityBytes: 1 << 30,
			})
			if code := status.Code(err); code != tc.want {
				t.Fatalf("code = %v (err %v), want %v", code, err, tc.want)
			}
			if len(mb.createCalledWith) != 0 {
				t.Errorf("backend.Create called %d times, want 0", len(mb.createCalledWith))
			}
		})
	}
}

// DeleteVolume tests.
func TestDeleteVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{}
	srv := newTestServer(t, mb)

	resp, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: testVolumeID,
		Fence:    testFence(t),
	})
	if err != nil {
		t.Fatalf("DeleteVolume unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if len(mb.deleteCalledWith) != 1 || mb.deleteCalledWith[0] != testVolumeID {
		t.Errorf("backend.Delete called with %v, want [%s]", mb.deleteCalledWith, testVolumeID)
	}
}

func TestDeleteVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{deleteErr: errors.New("device busy")}
	srv := newTestServer(t, mb)

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: testVolumeID,
		Fence:    testFence(t),
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

func TestDeleteVolume_InvalidVolumeID(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &mockBackend{})

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "nopool",
		Fence:    testFence(t),
	})
	if err == nil {
		t.Fatal("expected error for invalid volumeID")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

// ExpandVolume tests.
func TestExpandVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{expandAllocated: 2 << 30}
	srv := newTestServer(t, mb)

	resp, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
		Fence:          testFence(t),
		RequestedBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("ExpandVolume unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), 2<<30)
	}
}
func TestExpandVolume_ActiveNVMeNamespaceRevalidationFailure(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{expandAllocated: 2 << 30}
	root := t.TempDir()
	namespaceDir := filepath.Join(
		root,
		"nvmet",
		"subsystems",
		"nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc",
		"namespaces",
		"1",
	)
	if err := os.MkdirAll(namespaceDir, 0o750); err != nil {
		t.Fatalf("create active namespace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(namespaceDir, "enable"), []byte("1"), 0o600); err != nil {
		t.Fatalf("seed enabled namespace: %v", err)
	}
	if err := os.Mkdir(filepath.Join(namespaceDir, "revalidate_size"), 0o750); err != nil {
		t.Fatalf("create invalid revalidate_size attribute: %v", err)
	}
	srv := agent.NewServer(
		map[string]backend.VolumeBackend{testPool: mb}, root, agent.WithDrainStateDir(t.TempDir()),
	)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
		Fence:          testFence(t),
		RequestedBytes: 2 << 30,
	})
	if err == nil {
		t.Fatal("expected namespace revalidation error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
	if !strings.Contains(st.Message(), "revalidate NVMe namespace") {
		t.Errorf("error = %q, want revalidation context", st.Message())
	}
}

func TestExpandVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{expandErr: errors.New("shrink not allowed")}
	srv := newTestServer(t, mb)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
		Fence:          testFence(t),
		RequestedBytes: 512,
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// TestVolumeRPCs_InsufficientCapacity verifies that a backend
// *InsufficientCapacityError is reported as ResourceExhausted by both
// CreateVolume and ExpandVolume, as the CSI spec requires for insufficient
// capacity, and that the backend detail survives in the message (issue #99).
func TestVolumeRPCs_InsufficientCapacity(t *testing.T) {
	t.Parallel()
	capErr := func() error {
		return &backend.InsufficientCapacityError{
			VolumeID:       testVolumeID,
			RequestedBytes: 5 << 30,
			Err:            errors.New("zfs create -V 5368709120 tank/test-vol: out of space"),
		}
	}
	cases := map[string]func(t *testing.T) error{
		"CreateVolume": func(t *testing.T) error {
			srv := newTestServer(t, &mockBackend{createErr: capErr()})
			_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
				VolumeId:      testVolumeID,
				BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				Fence:         testFence(t),
				CapacityBytes: 5 << 30,
			})
			return err
		},
		"ExpandVolume": func(t *testing.T) error {
			srv := newTestServer(t, &mockBackend{expandErr: capErr()})
			_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
				VolumeId:       testVolumeID,
				Fence:          testFence(t),
				RequestedBytes: 5 << 30,
			})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := call(t)
			st, _ := status.FromError(err)
			if st.Code() != codes.ResourceExhausted {
				t.Fatalf("code = %v, want ResourceExhausted (err: %v)", st.Code(), err)
			}
			if !strings.Contains(st.Message(), "out of space") {
				t.Errorf("message %q lost the backend detail", st.Message())
			}
		})
	}
}

// GetCapacity tests.
func TestGetCapacity_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     10 << 30, // 10 GiB
		capacityAvailable: 7 << 30,  // 7 GiB
	}
	srv := newTestServer(t, mb)

	resp, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: testPool,
	})
	if err != nil {
		t.Fatalf("GetCapacity unexpected error: %v", err)
	}
	if resp.GetTotalBytes() != 10<<30 {
		t.Errorf("TotalBytes = %d, want %d", resp.GetTotalBytes(), 10<<30)
	}
	if resp.GetAvailableBytes() != 7<<30 {
		t.Errorf("AvailableBytes = %d, want %d", resp.GetAvailableBytes(), 7<<30)
	}
	if resp.GetUsedBytes() != 3<<30 {
		t.Errorf("UsedBytes = %d, want %d", resp.GetUsedBytes(), 3<<30)
	}
}

func TestGetCapacity_UnknownPool(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &mockBackend{})

	_, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for unknown pool, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestGetCapacity_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{capacityErr: errors.New("pool offline")}
	srv := newTestServer(t, mb)

	_, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: testPool,
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

// ListVolumes tests.
func TestListVolumes_Success(t *testing.T) {
	t.Parallel()
	vols := []*agentv1.VolumeInfo{
		{VolumeId: "tank/pvc-abc", CapacityBytes: 1 << 30, DevicePath: "/dev/zvol/tank/pvc-abc"},
		{VolumeId: "tank/pvc-def", CapacityBytes: 2 << 30, DevicePath: "/dev/zvol/tank/pvc-def"},
	}
	mb := &mockBackend{listVolumesResult: vols}
	srv := newTestServer(t, mb)

	resp, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: testPool,
	})
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(resp.GetVolumes()) != 2 {
		t.Errorf("len(Volumes) = %d, want 2", len(resp.GetVolumes()))
	}
	if resp.GetVolumes()[0].GetVolumeId() != testVolumeID {
		t.Errorf("Volumes[0].VolumeId = %q, want %q",
			resp.GetVolumes()[0].GetVolumeId(), testVolumeID)
	}
}

func TestListVolumes_Empty(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{listVolumesResult: []*agentv1.VolumeInfo{}}
	srv := newTestServer(t, mb)

	resp, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: testPool,
	})
	if err != nil {
		t.Fatalf("ListVolumes unexpected error: %v", err)
	}
	if len(resp.GetVolumes()) != 0 {
		t.Errorf("expected empty list, got %d volumes", len(resp.GetVolumes()))
	}
}

func TestListVolumes_UnknownPool(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &mockBackend{})

	_, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: "no-such-pool",
	})
	if err == nil {
		t.Fatal("expected error for unknown pool, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestListVolumes_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{listVolumesErr: errors.New("zfs gone")}
	srv := newTestServer(t, mb)

	_, err := srv.ListVolumes(context.Background(), &agentv1.ListVolumesRequest{
		PoolName: testPool,
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}
