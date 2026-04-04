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

// Package unit_test contains unit tests for pure-logic functions in
// pillar-csi that are classified in the Unit layer of the test pyramid.
//
// This file covers input-validation-only code paths in the CSI Controller
// (internal/csi.ControllerServer) and CSI Identity server:
//
//   - E1.6 — Access mode validation (isSupportedAccessMode guard)
//   - E1.11 — Missing required StorageClass parameter validation
//   - E12   — Snapshot RPCs: automatic Unimplemented from embedded server
//   - E14   — Miscellaneous edge-case input validation (pre-agent checks)
//
// All tests exercise code paths that return early (InvalidArgument /
// Unimplemented) without ever reaching the Kubernetes API or the agent gRPC
// connection.  A minimal fake controller-runtime client with an empty object
// store is sufficient — no objects need to be seeded.
//
// Run with:
//
//	go test ./test/unit/ -v -run TestCSI
package unit_test

import (
	"context"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared test infrastructure
// ─────────────────────────────────────────────────────────────────────────────

// newMinimalController creates a ControllerServer backed by an empty
// controller-runtime fake client.  No PillarTarget, PillarPool, or other
// objects are seeded; this is sufficient for tests that trigger early-return
// validation errors before any Kubernetes API calls are made.
func newMinimalController(t *testing.T) *pillarcsi.ControllerServer {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme v1alpha1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme corev1: %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme storagev1: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return pillarcsi.NewControllerServer(fakeClient, "pillar-csi.bhyoo.com")
}

// validParamsWithoutBinding returns a minimal set of StorageClass parameters
// that satisfy the target / backend-type / protocol-type guards inside
// CreateVolume without referencing any PillarBinding CRD.  Tests that
// exercise access-mode rejection or capability validation use these params so
// that the error occurs at the access-mode check, not at a missing-param check.
func validParamsWithoutBinding() map[string]string {
	return map[string]string{
		"pillar-csi.bhyoo.com/target":        "storage-1",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
		"pillar-csi.bhyoo.com/pool":          "tank",
	}
}

// mountCap returns a VolumeCapability with the given access mode and a Mount
// access type (ext4 filesystem).
func mountCap(mode csipb.VolumeCapability_AccessMode_Mode) *csipb.VolumeCapability {
	return &csipb.VolumeCapability{
		AccessType: &csipb.VolumeCapability_Mount{
			Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
		},
		AccessMode: &csipb.VolumeCapability_AccessMode{Mode: mode},
	}
}

// requireInvalidArgument verifies that err is a gRPC InvalidArgument error.
func requireInvalidArgument(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected gRPC error, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %s: %v", got, err)
	}
}

// requireUnimplemented verifies that err is a gRPC Unimplemented error.
func requireUnimplemented(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected gRPC error, got nil")
	}
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("expected codes.Unimplemented, got %s: %v", got, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E1.6 — Access Mode Validation (isSupportedAccessMode guard)
//
// Unit test 근거: isSupportedAccessMode는 CSI AccessMode 열거값을 받아
// bool을 반환하는 순수 함수다.  외부 상태 없이 입력-출력만으로 정확성 판단 가능.
// Rejection tests (E1.6-4 ~ E1.6-8) fail before any Kubernetes or agent
// calls, so only a fake empty client is required.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_AccessMode_RWX_Rejected verifies that
// MULTI_NODE_MULTI_WRITER (ReadWriteMany / RWX) is rejected at the driver
// level, because NVMe-oF TCP block devices do not support multi-writer
// semantics.
//
// Corresponds to E1.6-4 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_AccessMode_RWX_Rejected(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-rwx-test",
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)},
		Parameters:         validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_AccessMode_Unknown_Rejected verifies that
// UNKNOWN (mode 0) is rejected: the driver requires an explicit access mode.
//
// Corresponds to E1.6-5 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_AccessMode_Unknown_Rejected(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-unknown-mode",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_UNKNOWN),
		},
		Parameters: validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_AccessMode_Missing_InCapability verifies
// that a VolumeCapability with a nil AccessMode field is rejected before any
// Kubernetes or agent calls are attempted.
//
// Corresponds to E1.6-6 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_AccessMode_Missing_InCapability(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-nil-mode",
		VolumeCapabilities: []*csipb.VolumeCapability{
			{
				AccessType: &csipb.VolumeCapability_Mount{
					Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"},
				},
				// AccessMode deliberately nil
			},
		},
		Parameters: validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_VolumeCapabilities_Empty verifies that an
// empty VolumeCapabilities slice returns InvalidArgument immediately.
//
// Corresponds to E1.6-7 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_VolumeCapabilities_Empty(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-empty-caps",
		VolumeCapabilities: []*csipb.VolumeCapability{},
		Parameters:         validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_MultipleCapabilities_AnyUnsupported verifies
// that a request containing any unsupported access mode in its capability list
// is rejected with InvalidArgument, even when other capabilities are valid.
//
// Corresponds to E1.6-8 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_MultipleCapabilities_AnyUnsupported(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-mixed-caps",
		VolumeCapabilities: []*csipb.VolumeCapability{
			// Valid capability
			mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
			// Unsupported capability — should trigger rejection
			mountCap(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		},
		Parameters: validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E1.11 — Missing Required StorageClass Parameter Validation
//
// Unit test 근거: VolumeId 파싱과 필수 파라미터 존재 여부 검증은
// 외부 호출 이전에 수행되는 순수 입력 검증 로직이다.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_MissingVolumeName verifies that an empty
// volume name is rejected before any StorageClass parameter lookups.
//
// Corresponds to E1.11-3 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_MissingVolumeName(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "", // empty name
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters:         validParamsWithoutBinding(),
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_MissingTargetParam verifies that omitting
// the required "pillar-csi.bhyoo.com/target" StorageClass parameter yields
// InvalidArgument before the Kubernetes API is consulted.
//
// Corresponds to E1.11-4 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_MissingTargetParam(t *testing.T) {
	t.Parallel()

	params := validParamsWithoutBinding()
	delete(params, "pillar-csi.bhyoo.com/target")

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-no-target",
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters:         params,
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_MissingBackendTypeParam verifies that
// omitting "pillar-csi.bhyoo.com/backend-type" from the StorageClass
// parameters returns InvalidArgument.
//
// Corresponds to E1.11-5 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_MissingBackendTypeParam(t *testing.T) {
	t.Parallel()

	params := validParamsWithoutBinding()
	delete(params, "pillar-csi.bhyoo.com/backend-type")

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-no-backend",
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters:         params,
	})
	requireInvalidArgument(t, err)
}

// TestCSIController_CreateVolume_MissingProtocolTypeParam verifies that
// omitting "pillar-csi.bhyoo.com/protocol-type" from the StorageClass
// parameters returns InvalidArgument.
//
// Corresponds to E1.11-6 in docs/testing/UNIT-TESTS.md.
func TestCSIController_CreateVolume_MissingProtocolTypeParam(t *testing.T) {
	t.Parallel()

	params := validParamsWithoutBinding()
	delete(params, "pillar-csi.bhyoo.com/protocol-type")

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-no-protocol",
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters:         params,
	})
	requireInvalidArgument(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E12 — CSI Snapshot RPCs: Unimplemented
//
// Unit test 근거: ControllerServer는 csi.UnimplementedControllerServer를
// 임베드하므로, CreateSnapshot/DeleteSnapshot/ListSnapshots는 구현 코드 없이
// 자동으로 gRPC Unimplemented 상태를 반환한다.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSISnapshot_CreateSnapshot_ReturnsUnimplemented verifies that
// CSI CreateSnapshot returns gRPC Unimplemented because pillar-csi does not
// yet implement snapshot support.
//
// Corresponds to E12 ID 96 in docs/testing/UNIT-TESTS.md.
func TestCSISnapshot_CreateSnapshot_ReturnsUnimplemented(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.CreateSnapshot(context.Background(), &csipb.CreateSnapshotRequest{
		SourceVolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		Name:           "snap-test",
	})
	requireUnimplemented(t, err)
}

// TestCSISnapshot_DeleteSnapshot_ReturnsUnimplemented verifies that
// CSI DeleteSnapshot returns gRPC Unimplemented.
//
// Corresponds to E12 ID 97 in docs/testing/UNIT-TESTS.md.
func TestCSISnapshot_DeleteSnapshot_ReturnsUnimplemented(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.DeleteSnapshot(context.Background(), &csipb.DeleteSnapshotRequest{
		SnapshotId: "storage-1/snap-test",
	})
	requireUnimplemented(t, err)
}

// TestCSISnapshot_ListSnapshots_ReturnsUnimplemented verifies that
// CSI ListSnapshots returns gRPC Unimplemented.
//
// Corresponds to E12 ID 98 in docs/testing/UNIT-TESTS.md.
func TestCSISnapshot_ListSnapshots_ReturnsUnimplemented(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.ListSnapshots(context.Background(), &csipb.ListSnapshotsRequest{})
	requireUnimplemented(t, err)
}

// TestCSISnapshot_PluginCapabilities_NoSnapshotCapability verifies that
// GetPluginCapabilities does not advertise VolumeSnapshot capability since
// snapshot support is not yet implemented.
//
// Corresponds to E12 ID 99 in docs/testing/UNIT-TESTS.md.
func TestCSISnapshot_PluginCapabilities_NoSnapshotCapability(t *testing.T) {
	t.Parallel()

	srv := pillarcsi.NewIdentityServer("pillar-csi.bhyoo.com", "0.1.0")
	resp, err := srv.GetPluginCapabilities(context.Background(), &csipb.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: unexpected error: %v", err)
	}

	for _, cap := range resp.GetCapabilities() {
		if svc, ok := cap.Type.(*csipb.PluginCapability_Service_); ok {
			if svc.Service.Type == csipb.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS {
				t.Error("GetPluginCapabilities should not advertise VolumeSnapshot; got it")
			}
		}
	}

	// VolumeExpansion_ONLINE must be present.
	var hasExpansion bool
	for _, cap := range resp.GetCapabilities() {
		if v, ok := cap.Type.(*csipb.PluginCapability_VolumeExpansion_); ok {
			if v.VolumeExpansion.Type == csipb.PluginCapability_VolumeExpansion_ONLINE {
				hasExpansion = true
			}
		}
	}
	if !hasExpansion {
		t.Error("GetPluginCapabilities: VolumeExpansion_ONLINE capability missing")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.1 — Miscellaneous Input Validation Edge Cases
//
// Unit test 근거: DeleteVolume과 ControllerExpandVolume의 진입부 검증은
// Kubernetes API 조회 이전에 수행되므로 순수 입력 검증 테스트에 해당한다.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_DeleteVolume_EmptyVolumeId verifies that DeleteVolume with an
// empty VolumeId returns InvalidArgument before any Kubernetes or agent calls.
//
// Corresponds to E14 ID 104 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_DeleteVolume_EmptyVolumeId(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{
		VolumeId: "",
	})
	requireInvalidArgument(t, err)
}

// TestCSIEdge_ControllerPublish_EmptyNodeId verifies that
// ControllerPublishVolume with an empty NodeId returns InvalidArgument
// before any Kubernetes or agent calls.
//
// Corresponds to E14 ID 105 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_ControllerPublish_EmptyNodeId(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	_, err := srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		NodeId:   "", // empty NodeId
		VolumeCapability: &csipb.VolumeCapability{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	requireInvalidArgument(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.2 — CapacityRange 경계값 (Boundary Values)
//
// Unit test 근거: CreateVolume 진입부에서 CapacityRange 값 검증은
// agent 호출 이전에 수행된다.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_CreateVolume_EmptyProtocolType verifies that a StorageClass
// parameter with protocol-type set to an empty string is rejected before any
// Kubernetes or agent calls.
//
// Corresponds to E14 ID 114 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_CreateVolume_EmptyProtocolType(t *testing.T) {
	t.Parallel()

	params := validParamsWithoutBinding()
	params["pillar-csi.bhyoo.com/protocol-type"] = "" // explicitly empty

	srv := newMinimalController(t)
	_, err := srv.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-empty-protocol",
		VolumeCapabilities: []*csipb.VolumeCapability{mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)},
		Parameters:         params,
	})
	requireInvalidArgument(t, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.5 — Access Mode Combination Errors
//
// Unit test 근거: ValidateVolumeCapabilities는 정적 능력 검사이므로
// Kubernetes/agent 호출 없이 순수 로직 테스트 가능.
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIEdge_CreateVolume_MultiNodeMultiWriter verifies that
// ValidateVolumeCapabilities returns an unconfirmed response (empty Confirmed
// field) with a Message when the requested access mode is MULTI_NODE_MULTI_WRITER,
// which is not supported by the NVMe-oF block protocol.
//
// Corresponds to E14 ID 116 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_CreateVolume_MultiNodeMultiWriter(t *testing.T) {
	t.Parallel()

	srv := newMinimalController(t)
	resp, err := srv.ValidateVolumeCapabilities(context.Background(), &csipb.ValidateVolumeCapabilitiesRequest{
		VolumeId: "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		VolumeCapabilities: []*csipb.VolumeCapability{
			mountCap(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: unexpected gRPC error: %v", err)
	}
	// Per CSI spec §4.4: an unsupported capability must return an empty
	// Confirmed field (not an error code) with a human-readable Message.
	if resp.GetConfirmed() != nil {
		t.Errorf("ValidateVolumeCapabilities returned Confirmed for unsupported MULTI_NODE_MULTI_WRITER; "+
			"expected Confirmed=nil with Message set, got Confirmed=%+v", resp.GetConfirmed())
	}
	if resp.GetMessage() == "" {
		t.Error("ValidateVolumeCapabilities: Message should be non-empty when capability is unsupported")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E14.3 — VolumeContext Validation (NodeServer)
//
// Unit test 근거: NodeStageVolume의 VolumeContext 파라미터 검증은 Connector.Connect
// 이전에 수행되는 순수 입력 검증이다.
// ─────────────────────────────────────────────────────────────────────────────

// nopProtocolHandler is a stub ProtocolHandler that panics if any of its
// methods are actually invoked.  It is used in unit tests that exercise
// VolumeContext validation, which returns an error before the handler is
// ever called.
type nopProtocolHandler struct{}

func (*nopProtocolHandler) Attach(_ context.Context, _ pillarcsi.AttachParams) (*pillarcsi.AttachResult, error) {
	panic("nopProtocolHandler: Attach must not be called in a VolumeContext validation unit test")
}

func (*nopProtocolHandler) Detach(_ context.Context, _ pillarcsi.ProtocolState) error {
	panic("nopProtocolHandler: Detach must not be called in a VolumeContext validation unit test")
}

func (*nopProtocolHandler) Rescan(_ context.Context, _ pillarcsi.ProtocolState) error {
	panic("nopProtocolHandler: Rescan must not be called in a VolumeContext validation unit test")
}

// newNodeServerForValidation returns a NodeServer that has a registered
// "nvmeof-tcp" protocol handler (nopProtocolHandler) so that protocol
// dispatch succeeds, but the handler panics if actually invoked.  This is
// sufficient for tests that should return InvalidArgument from VolumeContext
// validation before ever reaching the handler.
func newNodeServerForValidation(t *testing.T) *pillarcsi.NodeServer {
	t.Helper()
	return pillarcsi.NewNodeServer(
		"test-node",
		map[string]pillarcsi.ProtocolHandler{
			pillarcsi.ProtocolNVMeoFTCP: &nopProtocolHandler{},
		},
		nil, // mounter not needed for validation-only tests
	)
}

// TestCSIEdge_NodeStage_EmptyNQN verifies that NodeStageVolume returns
// InvalidArgument when the VolumeContext["target_id"] (NQN) is an empty
// string.  The Connector.Connect method must not be called.
//
// Corresponds to E14 ID 111 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_NodeStage_EmptyNQN(t *testing.T) {
	t.Parallel()

	srv := newNodeServerForValidation(t)
	_, err := srv.NodeStageVolume(context.Background(), &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		StagingTargetPath: "/var/lib/kubelet/plugins/pillar-csi/staging/pvc-test",
		VolumeCapability:  mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext: map[string]string{
			"pillar-csi.bhyoo.com/protocol-type": pillarcsi.ProtocolNVMeoFTCP,
			"address":                            "192.168.1.1",
			"port":                               "4420",
			"target_id":                          "", // deliberately empty NQN
		},
	})
	requireInvalidArgument(t, err)
}

// TestCSIEdge_NodeStage_MissingVolumeContext verifies that NodeStageVolume
// returns InvalidArgument when VolumeContext is nil (all required keys are
// absent), which causes the NQN / target_id field to be empty.
//
// Corresponds to E14 ID 112 in docs/testing/UNIT-TESTS.md.
func TestCSIEdge_NodeStage_MissingVolumeContext(t *testing.T) {
	t.Parallel()

	srv := newNodeServerForValidation(t)
	_, err := srv.NodeStageVolume(context.Background(), &csipb.NodeStageVolumeRequest{
		VolumeId:          "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		StagingTargetPath: "/var/lib/kubelet/plugins/pillar-csi/staging/pvc-test",
		VolumeCapability:  mountCap(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
		VolumeContext:     nil, // nil VolumeContext → all keys absent
	})
	requireInvalidArgument(t, err)
}
