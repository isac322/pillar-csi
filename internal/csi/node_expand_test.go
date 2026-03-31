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

// Tests for NodeExpandVolume.
//
// All tests use an injectable mock Resizer so no real resize tools (resize2fs,
// xfs_growfs) or block devices are required.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeExpandVolume

import (
	"context"
	"errors"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock Resizer
// ─────────────────────────────────────────────────────────────────────────────.

// mockResizer is a test double for the Resizer interface.
type mockResizer struct {
	// err is returned by ResizeFS, or nil for success.
	err error

	// capturedMount records the mountPath passed to ResizeFS.
	capturedMount string
	// capturedFsType records the fsType passed to ResizeFS.
	capturedFsType string
	// called is incremented each time ResizeFS is invoked.
	called int
}

func (m *mockResizer) ResizeFS(mountPath, fsType string) error {
	m.called++
	m.capturedMount = mountPath
	m.capturedFsType = fsType
	return m.err
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────.

// newExpandServer creates a NodeServer with a mock Resizer for testing.
func newExpandServer(t *testing.T, r Resizer) *NodeServer {
	t.Helper()
	srv := NewNodeServerWithStateDir("test-node", &mockConnector{devicePath: "/dev/nvme0n1"}, &mockMounter{}, t.TempDir())
	srv.WithResizer(r)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeExpandVolume tests
// ─────────────────────────────────────────────────────────────────────────────.

func TestNodeExpandVolume_MissingVolumeID(t *testing.T) {
	srv := newExpandServer(t, &mockResizer{})
	_, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumePath: "/mnt/staging/vol1",
	})
	if err == nil {
		t.Fatal("expected error for missing volume_id, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s: %s", st.Code(), st.Message())
	}
}

func TestNodeExpandVolume_MissingVolumePath(t *testing.T) {
	srv := newExpandServer(t, &mockResizer{})
	_, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId: "vol-1",
	})
	if err == nil {
		t.Fatal("expected error for missing volume_path, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s: %s", st.Code(), st.Message())
	}
}

func TestNodeExpandVolume_Ext4_DefaultFsType(t *testing.T) {
	// When no VolumeCapability is provided the server defaults to ext4.
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	resp, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-1",
		VolumePath: "/mnt/staging/vol-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.called != 1 {
		t.Fatalf("expected ResizeFS called once, got %d", mock.called)
	}
	if mock.capturedMount != "/mnt/staging/vol-1" {
		t.Errorf("expected mountPath %q, got %q", "/mnt/staging/vol-1", mock.capturedMount)
	}
	if mock.capturedFsType != defaultFsType {
		t.Errorf("expected fsType %q (default), got %q", defaultFsType, mock.capturedFsType)
	}
}

func TestNodeExpandVolume_Ext4_ExplicitFsType(t *testing.T) {
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	resp, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-2",
		VolumePath: "/mnt/staging/vol-2",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: defaultFsType},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.capturedFsType != defaultFsType {
		t.Errorf("expected fsType %q, got %q", defaultFsType, mock.capturedFsType)
	}
}

func TestNodeExpandVolume_XFS(t *testing.T) {
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	resp, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-xfs",
		VolumePath: "/mnt/staging/vol-xfs",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if mock.capturedFsType != "xfs" {
		t.Errorf("expected fsType %q, got %q", "xfs", mock.capturedFsType)
	}
	if mock.capturedMount != "/mnt/staging/vol-xfs" {
		t.Errorf("expected mountPath %q, got %q", "/mnt/staging/vol-xfs", mock.capturedMount)
	}
}

func TestNodeExpandVolume_CapacityRangeEchoed(t *testing.T) {
	// When the CO supplies a capacity_range the response must echo
	// required_bytes so the CO can update PV status.
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	const reqBytes int64 = 10 * 1024 * 1024 * 1024 // 10 GiB

	resp, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:      "vol-3",
		VolumePath:    "/mnt/staging/vol-3",
		CapacityRange: &csi.CapacityRange{RequiredBytes: reqBytes},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != reqBytes {
		t.Errorf("expected CapacityBytes=%d, got %d", reqBytes, resp.GetCapacityBytes())
	}
}

func TestNodeExpandVolume_NoCapacityRange_ZeroBytes(t *testing.T) {
	// When no capacity_range is supplied the response capacity_bytes is 0.
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	resp, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-4",
		VolumePath: "/mnt/staging/vol-4",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetCapacityBytes() != 0 {
		t.Errorf("expected CapacityBytes=0 when no capacity_range, got %d", resp.GetCapacityBytes())
	}
}

func TestNodeExpandVolume_ResizerError_ReturnsInternal(t *testing.T) {
	// When the Resizer returns an error the RPC must fail with Internal.
	resizerErr := errors.New("resize2fs: /dev/nvme0n1: device not found")
	mock := &mockResizer{err: resizerErr}
	srv := newExpandServer(t, mock)

	_, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-fail",
		VolumePath: "/mnt/staging/vol-fail",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("expected Internal, got %s: %s", st.Code(), st.Message())
	}
	if !errors.Is(err, resizerErr) && !containsSubstring(st.Message(), resizerErr.Error()) {
		t.Errorf("expected error message to contain %q, got %q", resizerErr.Error(), st.Message())
	}
}

func TestNodeExpandVolume_NilCapabilityMountBlock(t *testing.T) {
	// VolumeCapability present but no fsType set → default ext4.
	mock := &mockResizer{}
	srv := newExpandServer(t, mock)

	_, err := srv.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-5",
		VolumePath: "/mnt/staging/vol-5",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: ""},
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.capturedFsType != defaultFsType {
		t.Errorf("expected default fsType %q when FsType is empty, got %q", defaultFsType, mock.capturedFsType)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// execResizer unit tests (no actual block device — just verify routing)
// ─────────────────────────────────────────────────────────────────────────────.

func TestExecResizer_UnsupportedFsType(t *testing.T) {
	r := &execResizer{}
	err := r.ResizeFS("/mnt/vol", "btrfs")
	if err == nil {
		t.Fatal("expected error for unsupported fsType, got nil")
	}
	if !containsSubstring(err.Error(), "unsupported") {
		t.Errorf("expected 'unsupported' in error message, got %q", err.Error())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// nvmeControllerName unit tests
// ─────────────────────────────────────────────────────────────────────────────.

func TestNvmeControllerName(t *testing.T) {
	tests := []struct {
		device string
		want   string
	}{
		{"/dev/nvme0n1", "nvme0"},
		{"/dev/nvme10n1", "nvme10"},
		{"/dev/nvme0n2", "nvme0"},
		{"/dev/sda", ""},
		{"/dev/dm-0", ""},
		{"nvme0n1", "nvme0"},        // no leading /dev/
		{"/dev/nvme", ""},           // no namespace
		{"/dev/nvmen1", ""},         // 'n' at position 0 after "nvme" — malformed
		{"/dev/loop0", ""},          // loopback device
		{"/dev/nvme2n1p1", "nvme2"}, // partition: controller is still nvme2
	}
	for _, tt := range tests {
		got := nvmeControllerName(tt.device)
		if got != tt.want {
			t.Errorf("nvmeControllerName(%q) = %q, want %q", tt.device, got, tt.want)
		}
	}
}

// ContainsSubstring is defined in statemachine_test.go (same package).
