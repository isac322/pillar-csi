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

// Package unit_test — E13 clone source, E14 capacity edge cases, E22 protocol
// error propagation tests using the unitControllerEnv mock infrastructure.
//
// These tests exercise CSI ControllerServer paths that require a mock agent
// (unitMockAgent) and a fake K8s client seeded with a PillarTarget.  The
// mock agent's function fields are overridden per-test to simulate specific
// agent responses (success, Unimplemented, shrink-rejection, etc.).
//
//   - E13-100: VolumeContentSource.Snapshot set — CreateVolume still succeeds
//     (clone unimplemented: snapshot source is silently ignored / forwarded
//     to the agent which also ignores it)
//   - E13-101: VolumeContentSource.Volume set — same behavior
//   - E14-108: agent.ExpandVolume returns "volsize cannot be decreased" —
//     ControllerExpandVolume propagates the error
//   - E14-109: RequiredBytes == LimitBytes == 1 GiB — CreateVolume succeeds
//     (exact-limit edge case does not cause an error)
//   - E22-172: agent.ExportVolume returns codes.Unimplemented ("nfs" protocol)
//     — CreateVolume propagates the error
//   - E22-182: agent.CreateVolume returns codes.Unimplemented ("lvm" backend)
//     — CreateVolume propagates the error
//
// Run with:
//
//	go test ./test/unit/ -v -run 'TestCSIClone|TestCSIEdge_ControllerExpand|TestCSIEdge_CreateVolume_Exact'
package unit_test

import (
	"context"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E13 — Clone / Snapshot source (unimplemented, source ignored)
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIClone_CreateVolume_SnapshotSourceIgnored verifies that CreateVolume
// with a VolumeContentSource.Snapshot set does not panic and either succeeds
// or returns a non-panic gRPC error.
//
// The pillar-csi driver does not implement snapshot-sourced clones; the CSI
// spec allows drivers to return Unimplemented or simply ignore the source and
// provision a fresh volume.  Either outcome satisfies the "no panic" invariant.
//
// Corresponds to E13 ID 100 in docs/testing/UNIT-TESTS.md.
func TestCSIClone_CreateVolume_SnapshotSourceIgnored(t *testing.T) {
	t.Parallel()

	env := newUnitControllerEnv(t)

	resp, err := env.srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-clone-from-snap",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: validParamsWithoutBinding(),
		// VolumeContentSource referencing a snapshot — the driver must not panic.
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{
					SnapshotId: "snap-abcdef01",
				},
			},
		},
	})

	// The driver must not panic.  Either success or a gRPC error is acceptable.
	if err != nil {
		st, _ := status.FromError(err)
		t.Logf("CreateVolume with snapshot source returned gRPC %s: %v", st.Code(), err)
	} else {
		t.Logf("CreateVolume with snapshot source returned OK: volumeID=%q", resp.GetVolume().GetVolumeId())
	}
}

// TestCSIClone_CreateVolume_VolumeSourceIgnored verifies that CreateVolume
// with a VolumeContentSource.Volume set does not panic and either succeeds
// or returns a non-panic gRPC error.
//
// Volume-to-volume cloning is not implemented by pillar-csi; the driver
// either ignores the source (provision fresh volume) or returns Unimplemented.
// Either outcome satisfies the "no panic" invariant.
//
// Corresponds to E13 ID 101 in docs/testing/UNIT-TESTS.md.
func TestCSIClone_CreateVolume_VolumeSourceIgnored(t *testing.T) {
	t.Parallel()

	env := newUnitControllerEnv(t)

	resp, err := env.srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-clone-from-vol",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: validParamsWithoutBinding(),
		// VolumeContentSource referencing an existing volume — must not panic.
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Volume{
				Volume: &csipb.VolumeContentSource_VolumeSource{
					VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-existing",
				},
			},
		},
	})

	// Driver must not panic.  Either success or a gRPC error is acceptable.
	if err != nil {
		st, _ := status.FromError(err)
		t.Logf("CreateVolume with volume source returned gRPC %s: %v", st.Code(), err)
	} else {
		t.Logf("CreateVolume with volume source returned OK: volumeID=%q", resp.GetVolume().GetVolumeId())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14 — Capacity edge cases (extended, agent-level)
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_ControllerExpand_ShrinkRequest verifies that ControllerExpandVolume
// propagates an agent-side shrink-rejection error to the caller.
//
// When the agent returns an error containing "volsize cannot be decreased" the
// CSI controller must surface it as a non-OK gRPC response, ensuring the CO
// receives feedback that the operation is impossible.
//
// Corresponds to E14 ID 108 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_ControllerExpand_ShrinkRequest(t *testing.T) {
	t.Parallel()

	env := newUnitControllerEnv(t)
	// Configure the mock agent to reject expand requests with a shrink error.
	env.agent.expandVolumeFn = func(_ *agentv1.ExpandVolumeRequest) (*agentv1.ExpandVolumeResponse, error) {
		return nil, status.Errorf(codes.InvalidArgument, "volsize cannot be decreased")
	}

	_, err := env.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-shrink-108",
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: 1 * 1024 * 1024 * 1024, // 1 GiB (simulated shrink target)
		},
	})

	// The agent returned an error — ControllerExpandVolume must propagate it.
	if err == nil {
		t.Fatal("expected error from ControllerExpandVolume when agent rejects shrink, got nil")
	}
	t.Logf("ControllerExpandVolume correctly propagated agent shrink error: %v", err)
}

// TestCSIEdge_CreateVolume_ExactLimitEqualsRequired verifies that CreateVolume
// with RequiredBytes == LimitBytes (an exact capacity constraint) does not
// return an error when the agent honors the request.
//
// The CO may issue an exact-size request (Required == Limit) to prevent the
// driver from over-provisioning.  The controller must forward this to the agent
// without rejecting it as "limit < required".
//
// Corresponds to E14 ID 109 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_CreateVolume_ExactLimitEqualsRequired(t *testing.T) {
	t.Parallel()

	const oneGiB = int64(1 * 1024 * 1024 * 1024)

	env := newUnitControllerEnv(t)
	// Mock agent returns exactly the requested capacity.
	env.agent.createVolumeFn = func(req *agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error) {
		return &agentv1.CreateVolumeResponse{
			DevicePath:    "/dev/zvol/tank/pvc-exact-109",
			CapacityBytes: req.GetCapacityBytes(),
		}, nil
	}

	_, err := env.srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-exact-limit-109",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: validParamsWithoutBinding(),
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: oneGiB,
			LimitBytes:    oneGiB, // exact: Required == Limit
		},
	})

	if err != nil {
		t.Errorf("CreateVolume with exact capacity range (Required==Limit==1GiB): unexpected error: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E22 — Protocol/Backend error propagation
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIProtocol_CreateVolume_NFSUnimplemented verifies that when the agent's
// ExportVolume returns codes.Unimplemented (NFS protocol is not supported),
// CreateVolume propagates the error to the caller.
//
// The test uses protocol-type="nfs" in StorageClass parameters; the CSI
// controller maps "nfs" to PROTOCOL_TYPE_NFS and passes it to
// agent.ExportVolume.  The mock agent's ExportVolume is set to return
// Unimplemented, simulating a deployment where only NVMe-oF TCP is available.
//
// Corresponds to E22 ID 172 in docs/testing/UNIT-TESTS.md.
func TestCSIProtocol_CreateVolume_NFSUnimplemented(t *testing.T) {
	t.Parallel()

	env := newUnitControllerEnv(t)
	// Mock agent: CreateVolume succeeds (backend provisions the block device),
	// but ExportVolume returns Unimplemented because NFS is not supported.
	env.agent.exportVolumeFn = func(_ *agentv1.ExportVolumeRequest) (*agentv1.ExportVolumeResponse, error) {
		return nil, status.Errorf(codes.Unimplemented, "only NVMe-oF TCP is supported")
	}

	// Use nfs protocol type — mapProtocolType("nfs") → PROTOCOL_TYPE_NFS.
	params := validParamsWithoutBinding()
	params["pillar-csi.bhyoo.com/protocol-type"] = "nfs"

	_, err := env.srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-nfs-unimplemented-172",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: params,
	})

	// agent.ExportVolume returned Unimplemented; CreateVolume must propagate it.
	if err == nil {
		t.Fatal("expected error from CreateVolume when NFS export is unimplemented, got nil")
	}
	t.Logf("CreateVolume correctly propagated NFS Unimplemented error: %v", err)
}

// TestCSIProtocol_CreateVolume_LVMBackendUnimplemented verifies that when the
// agent's CreateVolume returns codes.Unimplemented (LVM backend is not
// supported in this deployment), CreateVolume propagates the error.
//
// The test uses backend-type="lvm" in StorageClass parameters; "lvm" is not a
// known BackendType constant ("lvm-lv" is the correct value), so
// mapBackendType("lvm") returns BACKEND_TYPE_UNSPECIFIED which the mock agent
// rejects with Unimplemented — simulating an unsupported backend scenario.
//
// Corresponds to E22 ID 182 in docs/testing/UNIT-TESTS.md.
func TestCSIProtocol_CreateVolume_LVMBackendUnimplemented(t *testing.T) {
	t.Parallel()

	env := newUnitControllerEnv(t)
	// Mock agent: CreateVolume returns Unimplemented to simulate a deployment
	// where the LVM backend plugin is not installed.
	env.agent.createVolumeFn = func(_ *agentv1.CreateVolumeRequest) (*agentv1.CreateVolumeResponse, error) {
		return nil, status.Errorf(codes.Unimplemented, "LVM backend not supported in this deployment")
	}

	// Use "lvm" — not a known constant (the correct value is "lvm-lv").
	// mapBackendType("lvm") returns BACKEND_TYPE_UNSPECIFIED.
	params := validParamsWithoutBinding()
	params["pillar-csi.bhyoo.com/backend-type"] = "lvm"

	_, err := env.srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-lvm-unimplemented-182",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		},
		Parameters: params,
	})

	// agent.CreateVolume returned Unimplemented; CreateVolume must propagate it.
	if err == nil {
		t.Fatal("expected error from CreateVolume when LVM backend is unimplemented, got nil")
	}
	t.Logf("CreateVolume correctly propagated LVM Unimplemented error: %v", err)
}
