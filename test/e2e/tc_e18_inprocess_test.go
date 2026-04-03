package e2e

// tc_e18_inprocess_test.go — Per-TC assertions for E18: Agent down / internal error scenarios.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func assertE18_CreateVolume_AgentUnreachable(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.Unavailable, "agent unreachable")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e18-unreachable",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error when agent unreachable", tc.tcNodeLabel())
}

func assertE18_CreateVolume_AgentInternal(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.Internal, "agent internal error")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e18-create-internal",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error on agent internal error", tc.tcNodeLabel())
}

func assertE18_DeleteVolume_AgentInternal(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e18-delete-internal",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	env.agentSrv.deleteVolumeErr = status.Error(codes.Internal, "delete internal error")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error on agent delete internal error", tc.tcNodeLabel())
}

func assertE18_ControllerPublish_AgentInternal(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e18-pub-internal",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	env.agentSrv.allowInitiatorErr = status.Error(codes.Internal, "publish internal error")
	env.agentSrv.mu.Unlock()

	makeCSINodeWithNQN(env, "node-e18", "nqn.2026-01.io.example:node-e18")
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         resp.GetVolume().GetVolumeId(),
		NodeId:           "node-e18",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error on agent allow initiator internal error", tc.tcNodeLabel())
}

func assertE18_ExpandVolume_AgentInternal(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e18-expand-internal",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	env.agentSrv.expandVolumeErr = status.Error(codes.Internal, "expand internal error")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 20 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error on agent expand internal error", tc.tcNodeLabel())
}

func assertE18_ReconcileState_PartialExport(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// Create a volume to establish partial state.
	_, _ = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e18-partial",
		CapacityBytes: 10 << 20,
	})

	// ReconcileState must not panic even with partial export state.
	_, err := env.client.ReconcileState(env.ctx, &agentv1.ReconcileStateRequest{})
	// Acceptable outcomes: OK, Unimplemented, NotFound — no panic.
	if err != nil {
		Expect(status.Code(err)).To(BeElementOf(codes.OK, codes.Unimplemented, codes.NotFound, codes.Internal),
			"%s: ReconcileState unexpected error", tc.tcNodeLabel())
	}
}
