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

// Package e2e — E14: invalid inputs & edge case tests.
//
// TestCSIEdge_* validates that malformed CSI requests are rejected with the
// correct gRPC status codes and that no agent RPCs are issued when the input
// is invalid before reaching the backend.
//
// All tests use the in-process mock harness (csiControllerE2EEnv /
// csiNodeE2EEnv) and require no running cluster, NVMe-oF kernel modules, or
// external processes.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIEdge
package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csisrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// E14.1 VolumeId format violations
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_CreateVolume_ExtremelyLongVolumeName verifies that CreateVolume
// handles a volume name of extreme length (2 048 characters) without panicking.
//
// The CSI spec does not mandate a maximum name length, so the controller may
// either accept the name and proceed normally, or reject it with
// InvalidArgument.  The important invariant is that no server panic occurs.
//
// E14.1 / ID 102.
func TestCSIEdge_CreateVolume_ExtremelyLongVolumeName(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Build a 2 048-character volume name ("pvc-" + 2044 'x' characters).
	longName := "pvc-" + strings.Repeat("x", 2044)

	// The call must not panic; it may succeed or return a gRPC error.
	resp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               longName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultCreateVolumeParams(),
	})

	if err != nil {
		// A gRPC error is acceptable; verify it carries a known code.
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("expected a gRPC status error, got: %v", err)
		}
		switch st.Code() {
		case codes.InvalidArgument, codes.Internal, codes.ResourceExhausted:
			// All of these are valid rejections for an extremely long name.
		default:
			t.Errorf("unexpected gRPC code %s for extremely long name: %s",
				st.Code(), st.Message())
		}
		return
	}

	// If successful, the response must contain a non-empty VolumeId.
	if resp.GetVolume().GetVolumeId() == "" {
		t.Error("CreateVolume succeeded but returned an empty VolumeId")
	}
}

// TestCSIEdge_DeleteVolume_EmptyVolumeId verifies that DeleteVolume returns
// InvalidArgument when VolumeId is an empty string.
//
// The CSI spec §4.3 requires volume_id to be non-empty; the controller must
// reject the request before any agent interaction.
//
// E14.1 / ID 104.
func TestCSIEdge_DeleteVolume_EmptyVolumeId(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	_, err := env.Controller.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: "",
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "DeleteVolume with empty VolumeId")

	// No agent calls must have been issued.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.UnexportVolumeCalls) != 0 || len(env.AgentMock.DeleteVolumeCalls) != 0 {
		t.Error("agent UnexportVolume/DeleteVolume should not be called for empty VolumeId")
	}
}

// TestCSIEdge_ControllerPublish_EmptyNodeId verifies that
// ControllerPublishVolume returns InvalidArgument when NodeId is empty.
//
// E14.1 / ID 105.
func TestCSIEdge_ControllerPublish_EmptyNodeId(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-edge-pub"

	_, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "", // deliberately empty
		VolumeCapability: defaultVolumeCapabilities()[0],
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "ControllerPublishVolume with empty NodeId")

	// AllowInitiator must NOT have been called.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.AllowInitiatorCalls) != 0 {
		t.Error("agent AllowInitiator should not be called when NodeId is empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.2 CapacityRange boundary values
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_ControllerExpand_ShrinkRequest verifies that an agent error
// signalling that volume shrink is not allowed is propagated to the CSI caller
// as a non-OK gRPC status (Internal).
//
// The controller relays the agent's error code verbatim, so the caller sees a
// non-OK response when the storage backend rejects a shrink operation.
//
// E14.2 / ID 108.
func TestCSIEdge_ControllerExpand_ShrinkRequest(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Simulate an agent that refuses to shrink.
	env.AgentMock.ExpandVolumeErr = status.Error(codes.Internal,
		"volsize cannot be decreased")

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-edge-shrink"

	_, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 512 << 20, // 512 MiB — smaller than a hypothetical 1 GiB volume
		},
	})

	if err == nil {
		t.Fatal("expected ControllerExpandVolume to fail when agent rejects shrink, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() == codes.OK {
		t.Errorf("expected non-OK gRPC code, got OK")
	}
}

// TestCSIEdge_CreateVolume_ExactLimitEqualsRequired verifies that CreateVolume
// with RequiredBytes == LimitBytes succeeds and that the agent is called with
// the exact capacity value.
//
// E14.2 / ID 109.
func TestCSIEdge_CreateVolume_ExactLimitEqualsRequired(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	const capacity = 1 << 30 // 1 GiB

	resp, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-edge-exact-limit",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: capacity,
			LimitBytes:    capacity, // RequiredBytes == LimitBytes
		},
		Parameters: env.defaultCreateVolumeParams(),
	})
	assertNoError(t, err, "CreateVolume RequiredBytes==LimitBytes")

	// The agent must have been called with the exact capacity.
	env.AgentMock.mu.Lock()
	createCalls := env.AgentMock.CreateVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(createCalls) != 1 {
		t.Fatalf("expected 1 agent CreateVolume call, got %d", len(createCalls))
	}
	if createCalls[0].CapacityBytes != capacity {
		t.Errorf("agent CreateVolume CapacityBytes = %d, want %d",
			createCalls[0].CapacityBytes, capacity)
	}

	// The response must carry the returned capacity.
	if resp.GetVolume().GetCapacityBytes() == 0 {
		t.Error("CreateVolume response has zero CapacityBytes")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.3 VolumeContext value validation
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_NodeStage_EmptyNQN verifies that NodeStageVolume returns
// InvalidArgument when the target_id (NQN) key in VolumeContext is an empty
// string.
//
// E14.3 / ID 111.
func TestCSIEdge_NodeStage_EmptyNQN(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	stagingPath := filepath.Join(t.TempDir(), "staging")

	// Provide all required keys but set target_id to the empty string.
	volCtx := map[string]string{
		csisrv.VolumeContextKeyTargetNQN: "", // deliberately empty
		csisrv.VolumeContextKeyAddress:   "127.0.0.1",
		csisrv.VolumeContextKeyPort:      "4420",
	}

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/vol-edge-empty-nqn",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     volCtx,
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "NodeStageVolume empty NQN")

	// Connector must not have been called.
	if len(env.Connector.ConnectCalls) != 0 {
		t.Error("Connector.Connect should not be called when NQN is empty")
	}
}

// TestCSIEdge_NodeStage_MissingVolumeContext verifies that NodeStageVolume
// returns InvalidArgument when VolumeContext is nil.
//
// A nil map read produces the empty string for any key, so the missing-NQN
// guard fires before any privileged work is performed.
//
// E14.3 / ID 112.
func TestCSIEdge_NodeStage_MissingVolumeContext(t *testing.T) {
	t.Parallel()
	env := newCSINodeE2EEnv(t, "worker-1")
	ctx := context.Background()

	stagingPath := filepath.Join(t.TempDir(), "staging")

	_, err := env.Node.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/vol-edge-nil-ctx",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountVolumeCapability("ext4", csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     nil, // deliberately absent
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "NodeStageVolume nil VolumeContext")

	// Connector must not have been called.
	if len(env.Connector.ConnectCalls) != 0 {
		t.Error("Connector.Connect should not be called when VolumeContext is nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.4 StorageClass parameter combination errors
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_CreateVolume_EmptyProtocolType verifies that CreateVolume returns
// InvalidArgument when the "protocol-type" StorageClass parameter is present
// but set to an empty string.
//
// E14.4 / ID 114.
func TestCSIEdge_CreateVolume_EmptyProtocolType(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/protocol-type"] = "" // deliberately empty

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-edge-empty-proto",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "CreateVolume empty protocol-type")

	// No agent calls must have been issued.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.CreateVolumeCalls) != 0 {
		t.Error("agent CreateVolume should not be called when protocol-type is empty")
	}
}

// TestCSIEdge_CreateVolume_MissingBackendType verifies that CreateVolume
// returns InvalidArgument when the "backend-type" StorageClass parameter is
// entirely absent from the parameters map.
//
// E14.4 (complement of ID 113 — no-key variant rather than unsupported value).
func TestCSIEdge_CreateVolume_MissingBackendType(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.TargetName,
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
		"pillar-csi.bhyoo.com/zfs-pool":      "tank",
		// backend-type deliberately omitted
	}

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-edge-no-backend",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "CreateVolume missing backend-type")

	// No agent calls must have been issued.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.CreateVolumeCalls) != 0 {
		t.Error("agent CreateVolume should not be called when backend-type is missing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.5 Access mode combination errors
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_CreateVolume_UnsupportedAccessMode verifies that CreateVolume
// returns InvalidArgument when the requested VolumeCapability contains an
// access mode that pillar-csi does not support (MULTI_NODE_MULTI_WRITER).
//
// Pillar-csi exposes raw NVMe-oF block devices; multi-writer block access is
// not supported because NVMe-oF does not provide write-sharing semantics.
//
// E14.5 / ID 116 (CreateVolume path).
func TestCSIEdge_CreateVolume_UnsupportedAccessMode(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	unsupportedCap := []*csi.VolumeCapability{
		{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-edge-mnmw",
		VolumeCapabilities: unsupportedCap,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         env.defaultCreateVolumeParams(),
	})
	assertGRPCCode(t, err, codes.InvalidArgument, "CreateVolume MULTI_NODE_MULTI_WRITER")

	// No agent calls must have been issued.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.CreateVolumeCalls) != 0 {
		t.Error("agent CreateVolume should not be called for unsupported access mode")
	}
}

// TestCSIEdge_ValidateVolumeCapabilities_MultiNodeMultiWriter verifies that
// ValidateVolumeCapabilities returns a response with an empty Confirmed field
// (and no error) when MULTI_NODE_MULTI_WRITER is requested, indicating that
// the capability is not supported.
//
// E14.5 / ID 116 (ValidateVolumeCapabilities path).
func TestCSIEdge_ValidateVolumeCapabilities_MultiNodeMultiWriter(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-edge-validate"

	resp, err := env.Controller.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: volumeID,
		VolumeCapabilities: []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
				},
			},
		},
	})
	assertNoError(t, err, "ValidateVolumeCapabilities MULTI_NODE_MULTI_WRITER")

	// The Confirmed field must be nil/empty — the capability is unsupported.
	if resp.GetConfirmed() != nil {
		t.Errorf("expected Confirmed=nil for unsupported MULTI_NODE_MULTI_WRITER, got non-nil: %v",
			resp.GetConfirmed())
	}
	// The Message field should describe why it is not supported.
	if resp.GetMessage() == "" {
		t.Error("expected a non-empty Message explaining why MULTI_NODE_MULTI_WRITER is unsupported")
	}
}

// TestCSIEdge_ControllerPublishVolume_EmptyVolumeId verifies that
// ControllerPublishVolume returns InvalidArgument when VolumeId is empty.
//
// E14.1 (complement of ID 105 for the publish path).
func TestCSIEdge_ControllerPublishVolume_EmptyVolumeId(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	_, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         "", // deliberately empty
		NodeId:           "worker-1",
		VolumeCapability: defaultVolumeCapabilities()[0],
	})
	assertGRPCCode(t, err, codes.InvalidArgument,
		"ControllerPublishVolume with empty VolumeId")

	// AllowInitiator must NOT have been called.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.AllowInitiatorCalls) != 0 {
		t.Error("agent AllowInitiator should not be called when VolumeId is empty")
	}
}

// TestCSIEdge_ControllerExpandVolume_MalformedVolumeId verifies that
// ControllerExpandVolume returns InvalidArgument for a VolumeId that does not
// follow the expected "<target>/<protocol>/<backend>/<vol-id>" format.
//
// E14.1 (expansion path with bad VolumeId format).
func TestCSIEdge_ControllerExpandVolume_MalformedVolumeId(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// A volume ID with only two segments instead of the required four.
	_, err := env.Controller.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId: "only/two-segments",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 2 << 30,
		},
	})
	assertGRPCCode(t, err, codes.InvalidArgument,
		"ControllerExpandVolume malformed VolumeId")

	// No agent ExpandVolume calls must have been issued.
	env.AgentMock.mu.Lock()
	defer env.AgentMock.mu.Unlock()
	if len(env.AgentMock.ExpandVolumeCalls) != 0 {
		t.Error("agent ExpandVolume should not be called for a malformed VolumeId")
	}
}
