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

package csi

// Tests for NodeGetVolumeStats.
//
// The test suite covers both the filesystem (MOUNT) path — which uses
// syscall.Statfs under the hood — and the block-device (BLOCK) path.
// Block-device tests inject fakes via NodeServer.statFn and
// NodeServer.blockDeviceSizeFn so that no real block device or root
// privileges are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeGetVolumeStats

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// fakeDeviceFileInfo
// ─────────────────────────────────────────────────────────────────────────────.

// fakeDeviceFileInfo implements os.FileInfo and reports os.ModeDevice in its
// mode so that NodeGetVolumeStats takes the block-device branch without
// needing a real block device on the host.
type fakeDeviceFileInfo struct{}

func (*fakeDeviceFileInfo) Name() string       { return "fake-device" }
func (*fakeDeviceFileInfo) Size() int64        { return 0 }
func (*fakeDeviceFileInfo) Mode() os.FileMode  { return os.ModeDevice | 0o600 }
func (*fakeDeviceFileInfo) ModTime() time.Time { return time.Time{} }
func (*fakeDeviceFileInfo) IsDir() bool        { return false }
func (*fakeDeviceFileInfo) Sys() any           { return nil }

// ─────────────────────────────────────────────────────────────────────────────
// Input-validation tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeGetVolumeStats_MissingVolumeID verifies that an empty volume_id is
// rejected with InvalidArgument.
func TestNodeGetVolumeStats_MissingVolumeID(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)

	_, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "",
		VolumePath: "/some/path",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	s, _ := status.FromError(err)
	if s.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", s.Code())
	}
}

// TestNodeGetVolumeStats_MissingVolumePath verifies that an empty volume_path
// is rejected with InvalidArgument.
func TestNodeGetVolumeStats_MissingVolumePath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)

	_, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "tank/pvc-abc",
		VolumePath: "",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	s, _ := status.FromError(err)
	if s.Code() != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", s.Code())
	}
}

// TestNodeGetVolumeStats_NonExistentPath verifies that a non-existent
// volume_path is rejected with NotFound.
func TestNodeGetVolumeStats_NonExistentPath(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)

	_, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "tank/pvc-missing",
		VolumePath: "/this/path/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	s, _ := status.FromError(err)
	if s.Code() != codes.NotFound {
		t.Errorf("code = %s, want NotFound", s.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Filesystem (MOUNT) volume tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeGetVolumeStats_FilesystemVolume verifies that NodeGetVolumeStats
// returns both BYTES and INODES usage entries for a real filesystem path.
// It uses t.TempDir() so that syscall.Statfs runs against a genuine mount
// point without requiring root.
func TestNodeGetVolumeStats_FilesystemVolume(t *testing.T) {
	t.Parallel()
	env := newNodeTestEnv(t)
	mountDir := t.TempDir() // guaranteed to be on a real filesystem

	resp, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "tank/pvc-fs",
		VolumePath: mountDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}

	// Expect exactly two usage entries: BYTES and INODES.
	if len(resp.Usage) != 2 {
		t.Fatalf("len(usage) = %d, want 2", len(resp.Usage))
	}

	var bytesEntry, inodesEntry *csi.VolumeUsage
	for _, u := range resp.Usage {
		switch u.Unit {
		case csi.VolumeUsage_BYTES:
			bytesEntry = u
		case csi.VolumeUsage_INODES:
			inodesEntry = u
		}
	}
	if bytesEntry == nil {
		t.Error("missing BYTES usage entry")
	}
	if inodesEntry == nil {
		t.Error("missing INODES usage entry")
	}
	if bytesEntry != nil && bytesEntry.Total <= 0 {
		t.Errorf("BYTES total = %d, want > 0", bytesEntry.Total)
	}
	if inodesEntry != nil && inodesEntry.Total <= 0 {
		t.Errorf("INODES total = %d, want > 0", inodesEntry.Total)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Block device (BLOCK) volume tests
// ─────────────────────────────────────────────────────────────────────────────.

// TestNodeGetVolumeStats_BlockDevice_HappyPath verifies that when volume_path
// refers to a block device file, NodeGetVolumeStats returns a single BYTES
// entry with only Total populated (no Used/Available).
//
// Because creating a real block device requires root, this test injects a
// fakeDeviceFileInfo (mode = os.ModeDevice) via NodeServer.statFn and a stub
// size via NodeServer.blockDeviceSizeFn.  Both are per-instance fields so the
// test is safe to run in parallel with other NodeGetVolumeStats tests.
func TestNodeGetVolumeStats_BlockDevice_HappyPath(t *testing.T) {
	t.Parallel()

	const expectedBytes int64 = 107374182400 // 100 GiB

	env := newNodeTestEnv(t)
	env.srv.statFn = func(_ string) (os.FileInfo, error) {
		return &fakeDeviceFileInfo{}, nil
	}
	env.srv.blockDeviceSizeFn = func(_ string) (int64, error) {
		return expectedBytes, nil
	}

	resp, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "pool/pvc-block",
		VolumePath: "/dev/fake-nvme0n1",
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}

	// Block volumes return exactly one BYTES entry (no INODES).
	if len(resp.Usage) != 1 {
		t.Fatalf("len(usage) = %d, want 1 for block device", len(resp.Usage))
	}
	u := resp.Usage[0]
	if u.Unit != csi.VolumeUsage_BYTES {
		t.Errorf("unit = %v, want BYTES", u.Unit)
	}
	if u.Total != expectedBytes {
		t.Errorf("total = %d, want %d", u.Total, expectedBytes)
	}
	// Used and Available must be zero for raw block devices (no filesystem).
	if u.Used != 0 || u.Available != 0 {
		t.Errorf("used=%d available=%d, want both 0 for raw block device",
			u.Used, u.Available)
	}
}

// TestNodeGetVolumeStats_BlockDevice_IoctlError verifies that an ioctl failure
// is propagated as a gRPC Internal error.
func TestNodeGetVolumeStats_BlockDevice_IoctlError(t *testing.T) {
	t.Parallel()

	env := newNodeTestEnv(t)
	env.srv.statFn = func(_ string) (os.FileInfo, error) {
		return &fakeDeviceFileInfo{}, nil
	}
	env.srv.blockDeviceSizeFn = func(_ string) (int64, error) {
		return 0, errors.New("ioctl failed: no such device")
	}

	_, err := env.srv.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "pool/pvc-block-err",
		VolumePath: "/dev/fake-nvme0n1",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	s, _ := status.FromError(err)
	if s.Code() != codes.Internal {
		t.Errorf("code = %s, want Internal", s.Code())
	}
}
