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

// Tests for ControllerExpandVolume protocol-aware NodeExpansionRequired (AC 13d).
//
// RFC §5.7.3: For block protocols (nvmeof-tcp, iscsi) node_expansion_required
// must be true so that the CO subsequently calls NodeExpandVolume to rescan the
// block device and resize the filesystem.  For file protocols (nfs, smb)
// node_expansion_required must be false because resize is fully server-side.
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestControllerExpandVolume

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// expandRequest builds a valid ControllerExpandVolumeRequest for the given
// volumeID and requested size in bytes.
func expandRequest(volumeID string, requiredBytes int64) *csi.ControllerExpandVolumeRequest {
	return &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: requiredBytes,
		},
	}
}

// expandVolumeID formats a volume ID using the pillar-csi encoding:
// <target>/<protocol>/<backend>/<agent-vol-id>.
// Because strings.SplitN is called with limit 4, the agentVolID may itself
// contain slashes (e.g. "tank/pvc-abc123").
//
// Target is the PillarTarget name; all tests use "storage-node-1".
func expandVolumeID(protocol, backend, agentVolID string) string {
	return "storage-node-1/" + protocol + "/" + backend + "/" + agentVolID
}

// ─────────────────────────────────────────────────────────────────────────────
// Block protocols — NodeExpansionRequired == true
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerExpandVolume_NVMeoFTCP_NodeExpansionRequired verifies that
// ControllerExpandVolume returns NodeExpansionRequired=true for "nvmeof-tcp"
// volumes because the CO must subsequently call NodeExpandVolume to rescan the
// block device and grow the filesystem.
func TestControllerExpandVolume_NVMeoFTCP_NodeExpansionRequired(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	const wantBytes int64 = 2147483648 // 2 GiB
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: wantBytes}

	volumeID := expandVolumeID("nvmeof-tcp", "zfs-zvol", "tank/pvc-nvme-expand")
	resp, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, wantBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if !resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired = false for nvmeof-tcp; want true (block device needs rescan)")
	}
	if resp.GetCapacityBytes() != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), wantBytes)
	}
}

// TestControllerExpandVolume_ISCSI_NodeExpansionRequired verifies that
// ControllerExpandVolume returns NodeExpansionRequired=true for "iscsi" volumes.
func TestControllerExpandVolume_ISCSI_NodeExpansionRequired(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	const wantBytes int64 = 3221225472 // 3 GiB
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: wantBytes}

	volumeID := expandVolumeID("iscsi", "zfs-zvol", "tank/pvc-iscsi-expand")
	resp, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, wantBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if !resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired = false for iscsi; want true (block device needs rescan)")
	}
	if resp.GetCapacityBytes() != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), wantBytes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// File protocols — NodeExpansionRequired == false
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerExpandVolume_NFS_NoNodeExpansion verifies that
// ControllerExpandVolume returns NodeExpansionRequired=false for "nfs" volumes.
// NFS resize is handled entirely on the server side; the CO must not call
// NodeExpandVolume on the client node.
func TestControllerExpandVolume_NFS_NoNodeExpansion(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	const wantBytes int64 = 5368709120 // 5 GiB
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: wantBytes}

	volumeID := expandVolumeID("nfs", "zfs-dataset", "tank/pvc-nfs-expand")
	resp, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, wantBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired = true for nfs; want false (server-side resize only)")
	}
	if resp.GetCapacityBytes() != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), wantBytes)
	}
}

// TestControllerExpandVolume_SMB_NoNodeExpansion verifies that
// ControllerExpandVolume returns NodeExpansionRequired=false for "smb" volumes.
func TestControllerExpandVolume_SMB_NoNodeExpansion(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	const wantBytes int64 = 10737418240 // 10 GiB
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: wantBytes}

	volumeID := expandVolumeID("smb", "zfs-dataset", "tank/pvc-smb-expand")
	resp, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, wantBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if resp.GetNodeExpansionRequired() {
		t.Error("NodeExpansionRequired = true for smb; want false (server-side resize only)")
	}
	if resp.GetCapacityBytes() != wantBytes {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), wantBytes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Capacity fallback when agent returns zero
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerExpandVolume_CapacityFallbackToRequested verifies that when
// the agent's ExpandVolume response carries CapacityBytes == 0, the controller
// falls back to the requested size so the CO can update the PVC status.
func TestControllerExpandVolume_CapacityFallbackToRequested(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	// Agent returns zero — simulates an agent that does not report the new size.
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: 0}

	const requestedBytes int64 = 1073741824 // 1 GiB
	volumeID := expandVolumeID("nvmeof-tcp", "zfs-zvol", "tank/pvc-fallback")
	resp, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, requestedBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if resp.GetCapacityBytes() != requestedBytes {
		t.Errorf("CapacityBytes = %d, want %d (fallback to requested)", resp.GetCapacityBytes(), requestedBytes)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Validation error paths
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerExpandVolume_MissingVolumeID verifies that an empty volume_id
// returns codes.InvalidArgument.
func TestControllerExpandVolume_MissingVolumeID(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1073741824,
		},
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for empty volume_id, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestControllerExpandVolume_MissingCapacityRange verifies that a nil
// capacity_range returns codes.InvalidArgument.
func TestControllerExpandVolume_MissingCapacityRange(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	volumeID := expandVolumeID("nvmeof-tcp", "zfs-zvol", "tank/pvc-no-range")
	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: nil,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for nil capacity_range, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestControllerExpandVolume_NegativeRequiredBytes verifies that a negative
// required_bytes returns codes.InvalidArgument.
func TestControllerExpandVolume_NegativeRequiredBytes(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	volumeID := expandVolumeID("nvmeof-tcp", "zfs-zvol", "tank/pvc-neg-bytes")
	_, err := env.srv.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: -1,
		},
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for negative required_bytes, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// TestControllerExpandVolume_MalformedVolumeID verifies that a volumeID with
// fewer than 4 slash-separated components returns codes.InvalidArgument.
func TestControllerExpandVolume_MalformedVolumeID(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	// Only 2 components — missing backend and agentVolID parts.
	_, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(
		"storage-node-1/nvmeof-tcp",
		1073741824,
	))
	if err == nil {
		t.Fatal("expected InvalidArgument for malformed volume_id, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("error code = %v, want InvalidArgument", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent RPC call verification
// ─────────────────────────────────────────────────────────────────────────────

// TestControllerExpandVolume_CallsAgentExpandVolume verifies that
// ControllerExpandVolume calls agent.ExpandVolume exactly once.
func TestControllerExpandVolume_CallsAgentExpandVolume(t *testing.T) {
	t.Parallel()

	env := newControllerTestEnv(t)
	const wantBytes int64 = 2147483648
	env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: wantBytes}

	volumeID := expandVolumeID("nvmeof-tcp", "zfs-zvol", "tank/pvc-agent-call")
	_, err := env.srv.ControllerExpandVolume(context.Background(), expandRequest(volumeID, wantBytes))
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}

	if env.agent.expandVolumeCalls != 1 {
		t.Errorf("agent.ExpandVolume call count = %d, want 1", env.agent.expandVolumeCalls)
	}
}

// TestControllerExpandVolume_AllBlockProtocols_NodeExpansionRequired verifies
// that all block protocol types require node-side expansion.
func TestControllerExpandVolume_AllBlockProtocols_NodeExpansionRequired(t *testing.T) {
	t.Parallel()

	blockProtocols := []string{"nvmeof-tcp", "iscsi"}
	for _, proto := range blockProtocols {
		t.Run(proto, func(t *testing.T) {
			t.Parallel()

			env := newControllerTestEnv(t)
			env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: 1073741824}

			volumeID := expandVolumeID(proto, "zfs-zvol", "tank/pvc-block-"+proto)
			resp, err := env.srv.ControllerExpandVolume(
				context.Background(),
				expandRequest(volumeID, 1073741824),
			)
			if err != nil {
				t.Fatalf("ControllerExpandVolume(%s): %v", proto, err)
			}
			if !resp.GetNodeExpansionRequired() {
				t.Errorf("NodeExpansionRequired = false for %s; want true", proto)
			}
		})
	}
}

// TestControllerExpandVolume_AllFileProtocols_NoNodeExpansion verifies that
// all file protocol types do NOT require node-side expansion.
func TestControllerExpandVolume_AllFileProtocols_NoNodeExpansion(t *testing.T) {
	t.Parallel()

	fileProtocols := []string{"nfs", "smb"}
	for _, proto := range fileProtocols {
		t.Run(proto, func(t *testing.T) {
			t.Parallel()

			env := newControllerTestEnv(t)
			env.agent.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: 1073741824}

			volumeID := expandVolumeID(proto, "zfs-dataset", "tank/pvc-file-"+proto)
			resp, err := env.srv.ControllerExpandVolume(
				context.Background(),
				expandRequest(volumeID, 1073741824),
			)
			if err != nil {
				t.Fatalf("ControllerExpandVolume(%s): %v", proto, err)
			}
			if resp.GetNodeExpansionRequired() {
				t.Errorf("NodeExpansionRequired = true for %s; want false", proto)
			}
		})
	}
}
