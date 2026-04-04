package e2e

// tc_e15_inprocess_test.go — Per-TC assertions for E15: Resource exhaustion.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertE15_CreateVolume_AgentFullDisk(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.ResourceExhausted, "no space left")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-full-disk",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for full disk", tc.tcNodeLabel())
}

func assertE15_CreateVolume_Timeout(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.DeadlineExceeded, "timeout")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-timeout",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for timeout", tc.tcNodeLabel())
}

func assertE15_ExpandVolume_ExceedsCapacity(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-expand-exceed",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	env.agentSrv.mu.Lock()
	env.agentSrv.expandVolumeErr = status.Error(codes.ResourceExhausted, "exceeds capacity")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 1000 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when expand exceeds capacity", tc.tcNodeLabel())
}

func assertE15_GetCapacity_AgentErr(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.getCapacityErr = status.Error(codes.Internal, "agent capacity error")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.GetCapacity(env.ctx, &csiapi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":       env.target.Name,
			"pillar-csi.bhyoo.com/pool":         "tank",
			"pillar-csi.bhyoo.com/backend-type": "zfs-zvol",
		},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when agent GetCapacity fails", tc.tcNodeLabel())
}

func assertE15_DeleteVolume_AgentErr(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-delete-err",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	env.agentSrv.deleteVolumeErr = status.Error(codes.Internal, "delete error")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when agent delete fails", tc.tcNodeLabel())
}

func assertE15_ExpandVolume_ExceedsPoolCapacity(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-expand-pool-cap",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	env.agentSrv.mu.Lock()
	env.agentSrv.expandVolumeErr = status.Error(codes.ResourceExhausted, "pool capacity exceeded")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      volumeID,
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 10000 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when expand exceeds pool capacity", tc.tcNodeLabel())
}

func assertE15_ControllerPublish_AgentErr(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e15-pub-err",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	env.agentSrv.allowInitiatorErr = status.Error(codes.Internal, "publish error")
	env.agentSrv.mu.Unlock()

	makeCSINodeWithNQN(env, "node-e15", "nqn.2026-01.io.example:node-e15")

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         resp.GetVolume().GetVolumeId(),
		NodeId:           "node-e15",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when agent publish fails", tc.tcNodeLabel())
}
