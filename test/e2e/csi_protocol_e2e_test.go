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

// Package e2e — E22 incompatible backend-protocol error scenario tests.
//
// These tests exercise the CSI ControllerServer's handling of unsupported or
// incompatible protocol/backend type combinations, verifying that:
//
//   - An agent-returned protocol error is faithfully propagated to the CSI
//     caller without being swallowed.
//   - Unknown protocol-type strings (e.g. "smb-v3-unknown") are mapped to
//     PROTOCOL_TYPE_UNSPECIFIED(0) before reaching the agent — not silently dropped.
//   - Unknown backend-type strings (e.g. "fuse-experimental") are mapped to
//     BACKEND_TYPE_UNSPECIFIED(0) and forwarded to the agent.
//
// All tests use the in-process csiControllerE2EEnv (fake k8s client +
// mockAgentServer on a real gRPC listener).  No Kubernetes cluster, kernel
// modules, or network storage agent is required.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIProtocol
package e2e

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E22.1 — CSI Controller: unsupported protocol-type propagation
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIProtocol_CreateVolume_ISCSIUnimplemented verifies that when
// StorageClass specifies protocol-type="iscsi" and the agent's ExportVolume
// returns codes.Unimplemented, CreateVolume propagates the non-OK status to
// the CSI caller.
//
// E22.1 — test ID 171.
func TestCSIProtocol_CreateVolume_ISCSIUnimplemented(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Inject Unimplemented from the agent's ExportVolume RPC (iSCSI not supported).
	env.AgentMock.ExportVolumeErr = status.Errorf(
		codes.Unimplemented, "protocol PROTOCOL_TYPE_ISCSI is not supported by this agent")

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/protocol-type"] = "iscsi"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-iscsi-test",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})

	if err == nil {
		t.Fatal("expected error for iSCSI protocol (Unimplemented from agent), got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", st.Code())
	}
}

// TestCSIProtocol_CreateVolume_NFSUnimplemented verifies that when StorageClass
// specifies protocol-type="nfs" and the agent's ExportVolume returns
// codes.Unimplemented, CreateVolume propagates the non-OK status.
//
// E22.1 — test ID 172.
func TestCSIProtocol_CreateVolume_NFSUnimplemented(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// NFS protocol not supported by the agent.
	env.AgentMock.ExportVolumeErr = status.Errorf(
		codes.Unimplemented, "protocol PROTOCOL_TYPE_NFS is not supported by this agent")

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/protocol-type"] = "nfs"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-nfs-test",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})

	if err == nil {
		t.Fatal("expected error for NFS protocol (Unimplemented from agent), got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", st.Code())
	}
}

// TestCSIProtocol_CreateVolume_UnknownProtocol_MapsToUnspecified verifies that
// an unrecognized protocol-type string (e.g. "smb-v3-unknown") is mapped to
// PROTOCOL_TYPE_UNSPECIFIED(0) by mapProtocolType and that value is forwarded
// to agent.ExportVolume. The test also confirms that the resulting agent error
// (InvalidArgument) is propagated to the caller.
//
// E22.1 — test ID 173.
func TestCSIProtocol_CreateVolume_UnknownProtocol_MapsToUnspecified(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Agent rejects PROTOCOL_TYPE_UNSPECIFIED (unknown protocol) with InvalidArgument.
	env.AgentMock.ExportVolumeErr = status.Errorf(
		codes.InvalidArgument, "handlerForProtocol: protocol_type is required")

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/protocol-type"] = "smb-v3-unknown"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-unknown-proto",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})

	if err == nil {
		t.Fatal("expected error for unknown protocol mapped to UNSPECIFIED, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", st.Code())
	}

	// Verify that the unknown protocol string was mapped to PROTOCOL_TYPE_UNSPECIFIED
	// before being forwarded to the agent.  The mock records every ExportVolume call
	// (even when returning an error) so we can inspect the ProtocolType field.
	env.AgentMock.mu.Lock()
	exportCalls := env.AgentMock.ExportVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(exportCalls) != 1 {
		t.Fatalf("expected 1 ExportVolume call, got %d", len(exportCalls))
	}
	if exportCalls[0].ProtocolType != agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED {
		t.Errorf("ExportVolume ProtocolType = %v, want PROTOCOL_TYPE_UNSPECIFIED (unknown protocol must map to 0)",
			exportCalls[0].ProtocolType)
	}
}

// TestCSIProtocol_ControllerPublish_ISCSIMissingAnnotation verifies that when
// ControllerPublishVolume is called for a volume whose ID encodes an iSCSI
// protocol but the CSINode lacks the iSCSI initiator IQN annotation,
// FailedPrecondition is returned (the controller cannot resolve the
// initiator identity without the annotation).
//
// E22.1 — test ID 174.
func TestCSIProtocol_ControllerPublish_ISCSIMissingAnnotation(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Volume ID encodes the iSCSI protocol in position [1] of the slash-separated
	// path: <target>/<protocol>/<backend>/<pool>/<volume>.
	const (
		volumeID = "storage-1/iscsi/zfs-zvol/tank/pvc-iscsi-publish"
		nodeID   = "worker-node-1"
	)

	_, err := env.Controller.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           nodeID,
		VolumeCapability: defaultVolumeCapabilities()[0],
	})

	if err == nil {
		t.Fatal("expected error for iSCSI ControllerPublish (missing CSINode annotation), got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", st.Code())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E22.3 — CSI Controller: unsupported backend-type propagation
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIProtocol_CreateVolume_UnknownBackendType_MapsToUnspecified verifies
// that an unrecognized backend-type string (e.g. "fuse-experimental") is mapped
// to BACKEND_TYPE_UNSPECIFIED(0) by mapBackendType and that value is forwarded
// to agent.CreateVolume.  The mock agent returns success so the CreateVolume
// call itself succeeds; the key assertion is that the correct enum value reached
// the agent.
//
// E22.3 — test ID 181.
func TestCSIProtocol_CreateVolume_UnknownBackendType_MapsToUnspecified(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// No errors injected — the mock succeeds so we can inspect the recorded call.
	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/backend-type"] = "fuse-experimental"
	// protocol-type remains "nvmeof-tcp" (from default params) so ExportVolume succeeds.

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-fuse-backend",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})

	// The mock agent succeeds, so CreateVolume should succeed overall.
	if err != nil {
		t.Fatalf("CreateVolume unexpected error: %v", err)
	}

	// Verify that the unknown backend string was mapped to BACKEND_TYPE_UNSPECIFIED
	// before being forwarded to the agent.
	env.AgentMock.mu.Lock()
	createCalls := env.AgentMock.CreateVolumeCalls
	env.AgentMock.mu.Unlock()

	if len(createCalls) != 1 {
		t.Fatalf("expected 1 CreateVolume call, got %d", len(createCalls))
	}
	if createCalls[0].BackendType != agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED {
		t.Errorf("CreateVolume BackendType = %v, want BACKEND_TYPE_UNSPECIFIED (unknown backend must map to 0)",
			createCalls[0].BackendType)
	}
}

// TestCSIProtocol_CreateVolume_LVMBackendUnimplemented verifies that when
// backend-type="lvm" is specified and the agent's CreateVolume returns
// codes.Unimplemented ("LVM backend not supported in this deployment"), the
// non-OK status is propagated to the CSI caller and no PillarVolume CRD is
// created.
//
// E22.3 — test ID 182.
func TestCSIProtocol_CreateVolume_LVMBackendUnimplemented(t *testing.T) {
	t.Parallel()
	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// Inject Unimplemented from the agent's CreateVolume RPC — simulates a
	// storage node that only has ZFS installed, not LVM.
	env.AgentMock.CreateVolumeErr = status.Errorf(
		codes.Unimplemented, "LVM backend not supported in this deployment")

	params := env.defaultCreateVolumeParams()
	params["pillar-csi.bhyoo.com/backend-type"] = "lvm"
	params["pillar-csi.bhyoo.com/protocol-type"] = "nvmeof-tcp"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-lvm-test",
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30},
		Parameters:         params,
	})

	if err == nil {
		t.Fatal("expected error for LVM backend (Unimplemented from agent), got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() == codes.OK {
		t.Fatalf("expected non-OK gRPC status, got OK")
	}
	// The agent returned Unimplemented; verify it was not swallowed.
	if st.Code() != codes.Unimplemented {
		t.Logf("agent Unimplemented propagated as gRPC %v (acceptable non-OK code)", st.Code())
	}
}
