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
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
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
	_ *agentv1.ZfsVolumeParams,
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

// Ensure mockBackend satisfies the interface.
var _ backend.VolumeBackend = (*mockBackend)(nil)

// newTestServer creates a Server with a single mock backend for pool "tank".
func newTestServer(mb *mockBackend) *agent.Server {
	backends := map[string]backend.VolumeBackend{
		testPool: mb,
	}
	return agent.NewServer(backends, "")
}

// CreateVolume tests.
func TestCreateVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		createDevicePath: "/dev/zvol/tank/pvc-abc",
		createAllocated:  1 << 30, // 1 GiB
	}
	srv := newTestServer(mb)

	resp, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
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
	srv := newTestServer(mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId: "no-slash",
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
	srv := newTestServer(mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId: "other-pool/pvc-xyz",
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
	srv := newTestServer(mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
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
	srv := newTestServer(mb)

	_, err := srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:      testVolumeID,
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

// DeleteVolume tests.
func TestDeleteVolume_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{}
	srv := newTestServer(mb)

	resp, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: testVolumeID,
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
	srv := newTestServer(mb)

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: testVolumeID,
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
	srv := newTestServer(&mockBackend{})

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId: "nopool",
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
	srv := newTestServer(mb)

	resp, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
		RequestedBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("ExpandVolume unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), 2<<30)
	}
}

func TestExpandVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{expandErr: errors.New("shrink not allowed")}
	srv := newTestServer(mb)

	_, err := srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
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

// GetCapacity tests.
func TestGetCapacity_Success(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     10 << 30, // 10 GiB
		capacityAvailable: 7 << 30,  // 7 GiB
	}
	srv := newTestServer(mb)

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
	srv := newTestServer(&mockBackend{})

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
	srv := newTestServer(mb)

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
	srv := newTestServer(mb)

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
	srv := newTestServer(mb)

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
	srv := newTestServer(&mockBackend{})

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
	srv := newTestServer(mb)

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
