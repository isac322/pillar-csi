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

var errNoFilesystemIdentity = errors.New("no existing filesystem identity")

// mockBackend is a test double for the VolumeBackend interface.
// Each method records whether it was called and returns the configured outputs.
type mockBackend struct {
	// Backend identity/state
	backendType            agentv1.BackendType
	backingResourcePresent bool
	// Create
	createDevicePath string
	createAllocated  int64
	createErr        error
	createCalledWith []createArgs
	// Delete
	deleteErr        error
	deleteCalledWith []string
	// Expand
	expandAllocated  int64
	expandErr        error
	expandCalledWith []string
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
	if m.deleteErr == nil {
		m.backingResourcePresent = false
	}
	return m.deleteErr
}

func (m *mockBackend) Expand(_ context.Context, volumeID string, _ int64) (allocatedBytes int64, err error) {
	m.expandCalledWith = append(m.expandCalledWith, volumeID)
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

// Type returns the configured backend type, defaulting to BACKEND_TYPE_ZFS_ZVOL
// for the existing volume tests.
func (m *mockBackend) Type() agentv1.BackendType {
	if m.backendType != agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED {
		return m.backendType
	}
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

func (m *mockBackend) Layout() backend.Layout { return m.layout }

// ExistingFilesystemIdentity models native metadata for the filesystem-kind
// test resource; absent resources and real block volumes have no filesystem.
func (m *mockBackend) ExistingFilesystemIdentity(
	_ context.Context,
	volumeID string,
) (*agentv1.FilesystemAdoption, error) {
	if m.Type() != agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET || !m.backingResourcePresent {
		return nil, errNoFilesystemIdentity
	}
	pool, leaf, _ := strings.Cut(volumeID, "/")
	source := volumeID
	if m.layout.ParentDataset != "" {
		source = pool + "/" + strings.Trim(m.layout.ParentDataset, "/") + "/" + leaf
	}
	return &agentv1.FilesystemAdoption{
		Kind: "zfs-dataset", CanonicalSource: source,
		ResourceId: "test-native/" + volumeID, FilesystemType: "zfs",
	}, nil
}

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
		{"zfs dataset on a zvol pool", agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, codes.InvalidArgument},
		{"directory on a zvol pool", agentv1.BackendType_BACKEND_TYPE_DIRECTORY, codes.InvalidArgument},
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
		VolumeId:    testVolumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       testFence(t),
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

func TestDeleteVolume_DatasetRequiresNFSManager(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		backendType:            agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		backingResourcePresent: true,
	}
	srv := newTestServer(t, mb)
	req := &agentv1.DeleteVolumeRequest{
		VolumeId:    testVolumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		Fence:       testFence(t),
	}

	_, err := srv.DeleteVolume(context.Background(), req)
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("DeleteVolume error = %v, want Unavailable", err)
	}
	st, _ := status.FromError(err)
	if !strings.Contains(st.Message(), testVolumeID) {
		t.Errorf("DeleteVolume error %q does not name volume %q", st.Message(), testVolumeID)
	}
	if !mb.backingResourcePresent {
		t.Fatal("DeleteVolume destroyed dataset backing resource without an NFS manager")
	}

	_, err = srv.CreateVolume(context.Background(), &agentv1.CreateVolumeRequest{
		VolumeId:    testVolumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET,
		Fence:       testFence(t),
	})
	if err != nil {
		t.Fatalf("CreateVolume after unavailable DeleteVolume = %v, want lifecycle still open", err)
	}
}

func TestDeleteVolume_BackendError(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{deleteErr: errors.New("device busy")}
	srv := newTestServer(t, mb)

	_, err := srv.DeleteVolume(context.Background(), &agentv1.DeleteVolumeRequest{
		VolumeId:    testVolumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       testFence(t),
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
		VolumeId:    "nopool",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:       testFence(t),
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
		BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
				BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    testPool,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    "nonexistent",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    testPool,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    testPool,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    testPool,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    "no-such-pool",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
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
		PoolName:    testPool,
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
	})
	if err == nil {
		t.Fatal("expected error from backend, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v, want Internal", st.Code())
	}
}

func lvDeleteRequest(fence *agentv1.FencingToken) *agentv1.DeleteVolumeRequest {
	return &agentv1.DeleteVolumeRequest{
		VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_LVM, Fence: fence,
	}
}

func lvExpandRequest(fence *agentv1.FencingToken) *agentv1.ExpandVolumeRequest {
	return &agentv1.ExpandVolumeRequest{
		VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
		RequestedBytes: 2 << 30, Fence: fence,
	}
}

func lvCreateRequest(fence *agentv1.FencingToken) *agentv1.CreateVolumeRequest {
	return &agentv1.CreateVolumeRequest{
		VolumeId: testVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
		CapacityBytes: 1 << 30, Fence: fence,
	}
}

// requireLVUntouched asserts the backend never created, deleted or resized
// the LV: it is still present and no mutation was recorded.
func requireLVUntouched(t *testing.T, mb *mockBackend, what string) {
	t.Helper()
	if !mb.backingResourcePresent || len(mb.createCalledWith) != 0 ||
		len(mb.deleteCalledWith) != 0 || len(mb.expandCalledWith) != 0 {
		t.Fatalf("%s mutated the LV: present=%t create=%v delete=%v expand=%v", what,
			mb.backingResourcePresent, mb.createCalledWith, mb.deleteCalledWith, mb.expandCalledWith)
	}
}

// A PreserveOriginal LV is never destroyed or resized by the agent, whatever
// the token: the request is refused before the backend runs and the mark
// stays byte-identical, so the lifecycle keeps owning the untouched LV.
func TestPreservedMark_RefusesDestructiveOps(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, _ := newLVTestServer(t, b)
	ctx := context.Background()
	seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), true))

	ops := []struct {
		name string
		call func() error
	}{
		{"DeleteVolume(A,6)", func() error {
			_, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 6)))
			return err
		}},
		{"ExpandVolume(A,6)", func() error {
			_, err := srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 6)))
			return err
		}},
		{"stale DeleteVolume(A,4)", func() error {
			_, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 4)))
			return err
		}},
		{"CreateVolume(A,6)", func() error {
			_, err := srv.CreateVolume(ctx, lvCreateRequest(token2("lifecycle-a", 6)))
			return err
		}},
	}
	for _, op := range ops {
		requireCode(t, op.name, op.call(), codes.FailedPrecondition)
		requireLVUntouched(t, b.mockBackend, op.name)
		requireMarkUnchanged(t, stateDir, seeded, op.name)
	}
}

// A pinned source is never re-provisioned as a managed volume: once any
// lifecycle adopted the LV, CreateVolume for the volume ID is refused even
// after that lifecycle ended, so lvcreate idempotence cannot re-adopt it.
func TestPinnedSource_CreateVolumeRefusedAfterEnd(t *testing.T) {
	t.Parallel()
	for _, preserve := range []bool{true, false} {
		t.Run(map[bool]string{true: "PreserveOriginal", false: "Managed"}[preserve], func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			srv, stateDir, _ := newLVTestServer(t, b)
			ended := pinnedMark("lifecycle-a", 5, testLVSource(), preserve)
			ended["ended"] = true
			ended["endedUIDs"] = []string{"lifecycle-a"}
			seeded := seedMark(t, stateDir, ended)

			_, err := srv.CreateVolume(context.Background(), lvCreateRequest(token2("lifecycle-b", 1)))
			requireCode(t, "CreateVolume(B) over an ended pinned mark", err, codes.FailedPrecondition)
			requireLVUntouched(t, b.mockBackend, "CreateVolume(B)")
			requireMarkUnchanged(t, stateDir, seeded, "CreateVolume(B)")
		})
	}
}

// A Managed adopted LV is resized or deleted only while it still is the
// pinned LV: a missing or replaced LV refuses the mutation before the
// backend runs, and the mark stays byte-identical.
func TestPinnedSource_ManagedVerifyFailureRefusesDeleteExpand(t *testing.T) {
	t.Parallel()
	changes := map[string]func(*mockLVBackend){
		"replaced": func(b *mockLVBackend) { b.replaceLV() },
		"missing":  func(b *mockLVBackend) { delete(b.lvs, testVolumeID) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newMockLVBackend()
			change(b)
			srv, stateDir, _ := newLVTestServer(t, b)
			ctx := context.Background()
			seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), false))

			_, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 6)))
			requireCode(t, "DeleteVolume of a "+name+" LV", err, codes.FailedPrecondition)
			requireLVUntouched(t, b.mockBackend, "DeleteVolume")
			requireMarkUnchanged(t, stateDir, seeded, "DeleteVolume")

			_, err = srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 6)))
			requireCode(t, "ExpandVolume of a "+name+" LV", err, codes.FailedPrecondition)
			requireLVUntouched(t, b.mockBackend, "ExpandVolume")
			requireMarkUnchanged(t, stateDir, seeded, "ExpandVolume")
		})
	}
}

// A pinned source fails closed on a backend that cannot verify LVs.
func TestPinnedSource_BackendWithoutVerifierRefusesDelete(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{backendType: agentv1.BackendType_BACKEND_TYPE_LVM, backingResourcePresent: true}
	srv, stateDir, _ := newLVTestServer(t, mb)
	seeded := seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), false))

	_, err := srv.DeleteVolume(context.Background(), lvDeleteRequest(token2("lifecycle-a", 6)))
	if code := status.Code(err); code == codes.OK {
		t.Fatal("DeleteVolume of a pinned LV ran without identity verification")
	}
	requireLVUntouched(t, mb, "DeleteVolume")
	requireMarkUnchanged(t, stateDir, seeded, "DeleteVolume")
}

// A verified Managed adopted LV behaves as a managed volume: it can be
// resized and deleted.  After the deletion and an agent restart the deleted
// lifecycle is retired and the volume ID stays pinned, so neither a new
// CreateVolume nor an import of another LV can take it.
func TestPinnedSource_ManagedVerifiedDeleteKeepsPin(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, cfgRoot := newLVTestServer(t, b)
	ctx := context.Background()
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), false))

	if _, err := srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 6))); err != nil {
		t.Fatalf("ExpandVolume of a verified Managed LV: %v", err)
	}
	if _, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 7))); err != nil {
		t.Fatalf("DeleteVolume of a verified Managed LV: %v", err)
	}
	if len(b.expandCalledWith) != 1 || len(b.deleteCalledWith) != 1 || b.backingResourcePresent {
		t.Fatalf("backend expand=%v delete=%v present=%t, want one resize and one delete",
			b.expandCalledWith, b.deleteCalledWith, b.backingResourcePresent)
	}

	srv = restartLVServer(t, b, stateDir, cfgRoot)
	_, err := srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 8)))
	requireCode(t, "ExpandVolume of the deleted lifecycle", err, codes.FailedPrecondition)
	_, err = srv.CreateVolume(ctx, lvCreateRequest(token2("lifecycle-b", 1)))
	requireCode(t, "CreateVolume over the deleted pinned LV", err, codes.FailedPrecondition)
	if len(b.createCalledWith) != 0 || len(b.expandCalledWith) != 1 {
		t.Fatalf("backend create=%v expand=%v after the lifecycle ended", b.createCalledWith, b.expandCalledWith)
	}
	requireRetargetRefused(t, srv, b, token2("lifecycle-b", 1))
}

// Control: a managed LV the agent created itself has no pin, so it is
// created, resized and deleted even though no adopted LV identity backs it.
func TestManagedLV_UnpinnedNeedsNoVerification(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	delete(b.lvs, testVolumeID)
	b.backingResourcePresent = false
	b.createDevicePath = testLVDevicePath
	b.createAllocated = 1 << 30
	srv, _, _ := newLVTestServer(t, b)
	ctx := context.Background()

	if _, err := srv.CreateVolume(ctx, lvCreateRequest(token2("lifecycle-a", 1))); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := srv.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 2))); err != nil {
		t.Fatalf("ExpandVolume: %v", err)
	}
	if _, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 3))); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if len(b.createCalledWith) != 1 || len(b.expandCalledWith) != 1 || len(b.deleteCalledWith) != 1 {
		t.Errorf("backend create=%v expand=%v delete=%v, want one of each",
			b.createCalledWith, b.expandCalledWith, b.deleteCalledWith)
	}
}

// A delayed terminal retry of a pinned Managed LV's delete never destroys a
// replacement: after the adopted LV was deleted, another actor recreated an
// LV with a new UUID under the same locator.  The old lifecycle's retry of
// the same delete (same UID and generation, also after an agent restart)
// succeeds idempotently without a second backend delete, the replacement
// stays, and the old lifecycle can grant nothing on it.
func TestPinnedSource_DeleteRetryNeverDestroysReplacement(t *testing.T) {
	t.Parallel()
	b := newMockLVBackend()
	srv, stateDir, cfgRoot := newLVTestServer(t, b)
	ctx := context.Background()
	seedMark(t, stateDir, pinnedMark("lifecycle-a", 5, testLVSource(), false))

	if _, err := srv.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 7))); err != nil {
		t.Fatalf("DeleteVolume of the verified Managed LV: %v", err)
	}
	if len(b.deleteCalledWith) != 1 {
		t.Fatalf("backend delete=%v, want one", b.deleteCalledWith)
	}
	// Another actor recreates an LV under the same locator.
	b.replaceLV()
	b.backingResourcePresent = true
	ended := markBytes(t, stateDir)

	for _, s := range []*agent.Server{srv, restartLVServer(t, b, stateDir, cfgRoot)} {
		if _, err := s.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 7))); err != nil {
			t.Fatalf("terminal DeleteVolume retry: %v", err)
		}
		if len(b.deleteCalledWith) != 1 || !b.backingResourcePresent {
			t.Fatalf("delete retry destroyed the replacement LV: delete=%v present=%t",
				b.deleteCalledWith, b.backingResourcePresent)
		}
		requireMarkUnchanged(t, stateDir, ended, "terminal delete retry")
		_, err := s.ExpandVolume(ctx, lvExpandRequest(token2("lifecycle-a", 8)))
		requireCode(t, "ExpandVolume of the ended lifecycle", err, codes.FailedPrecondition)
		_, err = s.DeleteVolume(ctx, lvDeleteRequest(token2("lifecycle-a", 8)))
		requireCode(t, "DeleteVolume of the ended lifecycle at a newer generation", err, codes.FailedPrecondition)
	}
	if len(b.expandCalledWith) != 0 || len(b.deleteCalledWith) != 1 {
		t.Fatalf("backend expand=%v delete=%v touched the replacement", b.expandCalledWith, b.deleteCalledWith)
	}
}
