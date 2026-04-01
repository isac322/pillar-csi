package e2e

// tc_e14_inprocess_test.go — Per-TC assertions for E14: Invalid inputs / edge cases.

import (
	"path/filepath"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// makeNodeVolumeContext returns a minimal VolumeContext map for NodeStageVolume calls.
func makeNodeVolumeContext() map[string]string {
	return map[string]string{
		csidrv.VolumeContextKeyTargetNQN: "nqn.2026-01.com.test:fake",
		csidrv.VolumeContextKeyAddress:   "127.0.0.1",
		csidrv.VolumeContextKeyPort:      "4420",
	}
}

func assertE14_CreateVolume_EmptyName(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty name", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for empty name", tc.tcNodeLabel())
}

func assertE14_CreateVolume_NilCapabilities(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e14-nil-caps",
		Parameters:         env.params,
		VolumeCapabilities: nil,
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for nil capabilities", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for nil capabilities", tc.tcNodeLabel())
}

func assertE14_CreateVolume_UnknownParam(_ documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := copyParams(env.params)
	params["unknown-param"] = "some-value"

	// Unknown params should be either accepted silently or rejected — either is valid.
	// The important thing is it doesn't panic.
	_, _ = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e14-unknown-param",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	// No panic = success. The test just verifies graceful handling.
}

func assertE14_DeleteVolume_EmptyID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: "",
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for empty volume ID", tc.tcNodeLabel())
}

func assertE14_NodeStageVolume_EmptyID(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: filepath.Join(env.stateDir, "stage"),
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     makeNodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_NodeStageVolume_EmptyPath(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e14-empty-path"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: "",
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     makeNodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty staging path", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for empty staging path", tc.tcNodeLabel())
}

func assertE14_NodePublishVolume_EmptyID(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	_, err := env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: filepath.Join(env.stateDir, "stage"),
		TargetPath:        filepath.Join(env.stateDir, "target"),
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_NodePublishVolume_EmptyTargetPath(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e14-pub-empty-target"
	env.sm.ForceState(volumeID, csidrv.StateNodeStaged)

	_, err := env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: filepath.Join(env.stateDir, "stage"),
		TargetPath:        "",
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty target path", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for empty target path", tc.tcNodeLabel())
}

func assertE14_NodeUnpublishVolume_EmptyID(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	_, err := env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   "",
		TargetPath: filepath.Join(env.stateDir, "target"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_NodeUnstageVolume_EmptyID(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	_, err := env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          "",
		StagingTargetPath: filepath.Join(env.stateDir, "stage"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_GetCapacity_NoParams(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// GetCapacity without params should return an error (missing target param)
	_, err := env.controller.GetCapacity(env.ctx, &csiapi.GetCapacityRequest{
		Parameters: map[string]string{},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for no params", tc.tcNodeLabel())
}

func assertE14_GetCapacity_UnknownTarget(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.GetCapacity(env.ctx, &csiapi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target": "nonexistent-target",
		},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for unknown target", tc.tcNodeLabel())
}

func assertE14_ControllerPublish_EmptyID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "",
		NodeId:           "node-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_ControllerUnpublish_EmptyID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: "",
		NodeId:   "node-1",
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE14_ValidateVolumeCapabilities(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// ValidateVolumeCapabilities may or may not be implemented.
	// If implemented, it should handle valid requests without panicking.
	resp, err := env.controller.ValidateVolumeCapabilities(env.ctx, &csiapi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "target-storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-validate",
		VolumeCapabilities: []*csiapi.VolumeCapability{
			mountCapability("ext4"),
		},
	})
	if err != nil {
		// Unimplemented is acceptable
		Expect(status.Code(err)).To(BeElementOf(codes.Unimplemented, codes.NotFound),
			"%s: unexpected error code for ValidateVolumeCapabilities", tc.tcNodeLabel())
	} else {
		Expect(resp).NotTo(BeNil(), "%s: response should not be nil", tc.tcNodeLabel())
	}
}
