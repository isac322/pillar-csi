package e2e

// tc_e11_inprocess_test.go — Per-TC assertions for E11: ControllerExpandVolume / NodeExpandVolume.

import (
	"path/filepath"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

func assertE11_ControllerExpandVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e11-expand",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	expandResp, err := env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerExpandVolume", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", 2<<30),
		"%s: expanded capacity", tc.tcNodeLabel())
	Expect(expandResp.GetNodeExpansionRequired()).To(BeTrue(),
		"%s: node expansion required", tc.tcNodeLabel())
}

func assertE11_NodeExpandVolume(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e11-node-expand"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, stageErr := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(stageErr).NotTo(HaveOccurred(), "%s: setup NodeStageVolume", tc.tcNodeLabel())

	expandResp, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       stagePath,
		VolumeCapability: mountCapability("ext4"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeExpandVolume", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", 2<<30),
		"%s: node expanded capacity", tc.tcNodeLabel())
}

func assertE11_ControllerExpandVolume_AgentErr(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e11-expand-agent-err",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	env.agentSrv.mu.Lock()
	env.agentSrv.expandVolumeErr = status.Error(codes.Internal, "expand failed")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when agent expand fails", tc.tcNodeLabel())
}

func assertE11_NodeExpandVolume_ResizerErr(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e11-resizer-err"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, stageErr := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(stageErr).NotTo(HaveOccurred(), "%s: setup NodeStageVolume", tc.tcNodeLabel())

	env.resizer.err = status.Error(codes.Internal, "resize failed")

	_, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       stagePath,
		VolumeCapability: mountCapability("ext4"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected resizer error", tc.tcNodeLabel())
}

func assertE11_ControllerExpand_Idempotency(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e11-expand-idem",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	}
	_, err = env.controller.ControllerExpandVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first ControllerExpandVolume", tc.tcNodeLabel())

	_, err = env.controller.ControllerExpandVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second ControllerExpandVolume (idempotent)", tc.tcNodeLabel())
}

func assertE11_NodeExpand_XFS(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e11-xfs"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, stageErrXFS := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("xfs"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(stageErrXFS).NotTo(HaveOccurred(), "%s: setup NodeStageVolume", tc.tcNodeLabel())

	expandResp, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       stagePath,
		VolumeCapability: mountCapability("xfs"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeExpandVolume XFS", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", 2<<30),
		"%s: XFS expanded capacity", tc.tcNodeLabel())
}

func assertE11_NodeExpand_EmptyPath(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "target-local/nvmeof-tcp/zfs-zvol/tank/pvc-e11-empty-path"

	_, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         volumeID,
		VolumePath:       "",
		VolumeCapability: mountCapability("ext4"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty path", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE11_NodeExpand_MissingVolumeID(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	_, err := env.node.NodeExpandVolume(env.ctx, &csiapi.NodeExpandVolumeRequest{
		VolumeId:         "",
		VolumePath:       "/some/path",
		VolumeCapability: mountCapability("ext4"),
		CapacityRange:    &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for missing volume ID", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}
