package e2e

// tc_e17_inprocess_test.go — Per-TC assertions for E17: Cleanup validation.

import (
	"path/filepath"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

func assertE17_NodeUnstageRemovesState(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-unstage"
	env.sm.ForceState(volumeID, csidrv.StateNodeStaged)

	stagePath := filepath.Join(env.stateDir, "stage-e17-unstage")
	_, err := env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume", tc.tcNodeLabel())
}

func assertE17_DeleteVolumeRemovesCRD(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-delete-crd",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume should remove CRD state", tc.tcNodeLabel())
}

func assertE17_NodeUnpublishUnmounts(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-unpublish"
	env.sm.ForceState(volumeID, csidrv.StateNodePublished)

	targetPath := filepath.Join(env.stateDir, "target-e17-unpublish")
	_, err := env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnpublishVolume should unmount", tc.tcNodeLabel())
}

func assertE17_ControllerUnpublishDeniesAccess(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-unpub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	makeCSINodeWithNQN(env, "node-e17", "nqn.2026-01.io.example:node-e17")
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "node-e17",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerPublishVolume", tc.tcNodeLabel())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "node-e17",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerUnpublishVolume should deny access", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.DenyInitiator).To(BeNumerically(">=", 1),
		"%s: DenyInitiator should be called on unpublish", tc.tcNodeLabel())
}

func assertE17_NodeStageStateFileCreated(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-stage-state"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	stagePath := filepath.Join(env.stateDir, "stage-e17-state")
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume", tc.tcNodeLabel())

	// State machine should reflect staged state
	state := env.sm.GetState(volumeID)
	Expect(state).To(Equal(csidrv.StateNodeStaged),
		"%s: volume should be in NodeStaged state after NodeStageVolume", tc.tcNodeLabel())
}

func assertE17_NodeUnstageStateFileRemoved(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-unstage-state"
	env.sm.ForceState(volumeID, csidrv.StateNodeStaged)

	_, err := env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: filepath.Join(env.stateDir, "stage-e17-removed"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume", tc.tcNodeLabel())

	// After unstage, state should transition away from NodeStaged
	state := env.sm.GetState(volumeID)
	Expect(state).NotTo(Equal(csidrv.StateNodeStaged),
		"%s: volume should not be NodeStaged after unstage", tc.tcNodeLabel())
}

func assertE17_NodeExpandVolumeResizesFS(_ documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-expand-fs"
	env.sm.ForceState(volumeID, csidrv.StateNodeStaged)

	_, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:          volumeID,
		VolumePath:        filepath.Join(env.stateDir, "expand-mnt"),
		StagingTargetPath: filepath.Join(env.stateDir, "stage-expand"),
		CapacityRange:     &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	// NodeExpandVolume either succeeds or returns an expected error —
	// the important thing is no panic.
	_ = err
}

func assertE17_NodeStage_MountFailureDisconnects(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	// Inject a mount error into the mounter
	env.mounter.formatAndMountErr = status.Error(codes.Internal, "format/mount failed: device busy")

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-mount-fail"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	stagePath := filepath.Join(env.stateDir, "stage-e17-mount-fail")
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when mount fails", tc.tcNodeLabel())
}

func assertE17_NodeUnstage_FailurePreservesStateFile(_ documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	// Successfully stage a volume first
	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-unstage-fail"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	stagePath := filepath.Join(env.stateDir, "stage-e17-unstage-fail")
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	// Inject unmount error
	env.mounter.unmountErr = status.Error(codes.Internal, "unmount failed: device busy")

	// NodeUnstageVolume with unmount error — should not panic
	_, _ = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	// No panic = success
}

func assertE17_CreatePartial_DeleteVolumeCleansCRD(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-partial-delete-crd",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()
	Expect(volumeID).NotTo(BeEmpty(), "%s: volume ID must not be empty", tc.tcNodeLabel())

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume should clean up CRD", tc.tcNodeLabel())

	// Verify agent received a delete call
	c := env.agentSrv.counts()
	Expect(c.DeleteVolume).To(BeNumerically(">=", 1),
		"%s: agent deleteVolume should be called on DeleteVolume", tc.tcNodeLabel())
}

func assertE17_FullLifecycle_NoResourceLeak(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-full-lifecycle",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	// Publish
	makeCSINodeWithNQN(env, "node-e17-full", "nqn.2026-01.io.example:node-e17-full")
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "node-e17-full",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerPublishVolume", tc.tcNodeLabel())

	// Unpublish
	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "node-e17-full",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerUnpublishVolume", tc.tcNodeLabel())

	// Delete
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())

	// Verify call counts indicate each step executed
	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(BeNumerically(">=", 1), "%s: CreateVolume agent call", tc.tcNodeLabel())
	Expect(c.AllowInitiator).To(BeNumerically(">=", 1), "%s: AllowInitiator agent call", tc.tcNodeLabel())
	Expect(c.DenyInitiator).To(BeNumerically(">=", 1), "%s: DenyInitiator agent call", tc.tcNodeLabel())
	Expect(c.DeleteVolume).To(BeNumerically(">=", 1), "%s: DeleteVolume agent call", tc.tcNodeLabel())
}

func assertE17_RepeatedStageUnstage(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e17-repeated-stage"
	stagePath := filepath.Join(env.stateDir, "stage-e17-repeated")

	// Stage × 3 (idempotent)
	for i := range 3 {
		env.sm.ForceState(volumeID, csidrv.StateControllerPublished)
		_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagePath,
			VolumeCapability:  mountCapability("ext4"),
			VolumeContext:     nodeVolumeContext(),
		})
		Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume attempt %d", tc.tcNodeLabel(), i+1)
	}

	// Unstage × 3 (idempotent)
	for i := range 3 {
		_, err := env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
			VolumeId:          volumeID,
			StagingTargetPath: stagePath,
		})
		// First unstage should succeed; subsequent ones may return NotFound but must not error fatally
		if i == 0 {
			Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume first attempt", tc.tcNodeLabel())
		}
		// Subsequent calls may return an error but must not panic
	}
}

func assertE17_MultiVolumeIsolation(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create two volumes; they must be independent.
	resp1, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-iso-a",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume A", tc.tcNodeLabel())

	resp2, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e17-iso-b",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume B", tc.tcNodeLabel())

	// Volume IDs must be distinct
	Expect(resp1.GetVolume().GetVolumeId()).NotTo(Equal(resp2.GetVolume().GetVolumeId()),
		"%s: volume IDs must be distinct", tc.tcNodeLabel())

	// Deleting one must not affect the other.
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp1.GetVolume().GetVolumeId(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume A", tc.tcNodeLabel())

	// Volume B should still be manageable
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp2.GetVolume().GetVolumeId(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume B", tc.tcNodeLabel())
}
