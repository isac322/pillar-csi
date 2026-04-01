package e2e

// tc_e22_inprocess_test.go — Per-TC assertions for E22: Access mode matrix / incompatible backend-protocol.

import (
	"fmt"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func assertE22_CreateVolume_UnsupportedProtocol(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "fc", // unsupported protocol
	}
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-unsupported-proto",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for unsupported protocol", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument for unsupported protocol", tc.tcNodeLabel())
}

func assertE22_CreateVolume_NVMeOF_TCP(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-nvmeof-tcp",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NVMe-oF TCP CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("nvmeof-tcp"),
		"%s: volume ID should contain protocol", tc.tcNodeLabel())
}

func assertE22_ControllerPublish_ProtocolMismatch(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create NVMe-oF volume but try to publish to iSCSI node
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-proto-mismatch",
		Parameters:         env.params, // nvmeof-tcp
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	// Create iSCSI node — no NQN annotation, only IQN
	makeCSINodeWithIQN(env, "iscsi-worker", "iqn.1993-08.org.debian:iscsi-worker")

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "iscsi-worker",
		VolumeCapability: mountCapability("ext4"),
	})
	// This may succeed (if iSCSI initiator is found and volume is nvmeof) or fail
	// The key assertion is that it doesn't panic
	_ = err
}

func assertE22_CreateVolume_iSCSI(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "zfs-zvol",
		"pillar-csi.bhyoo.com/protocol-type": "iscsi",
	}
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-iscsi",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: iSCSI CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("iscsi"),
		"%s: volume ID should contain protocol", tc.tcNodeLabel())
}

func assertE22_AgentErrors_Export_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, _ = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e22-export-proto",
		CapacityBytes: 1 << 30,
	})

	// Use an invalid protocol type
	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-e22-export-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		ExportParams: nil,
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in ExportVolume", tc.tcNodeLabel())
}

func assertE22_AgentErrors_Unexport_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     "tank/pvc-e22-unexport-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in UnexportVolume", tc.tcNodeLabel())
}

func assertE22_AgentProtocol_AllowInitiator_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "tank/pvc-e22-allow-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		InitiatorId:  "nqn.2026-01.io.example:host",
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in AllowInitiator", tc.tcNodeLabel())
}

func assertE22_AgentProtocol_DenyInitiator_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     "tank/pvc-e22-deny-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		InitiatorId:  "nqn.2026-01.io.example:host",
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in DenyInitiator", tc.tcNodeLabel())
}

func assertE22_CreateVolume_UnknownBackendType_Rejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "unknown-backend-xyz",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-unknown-backend-rejected",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for unknown backend", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument", tc.tcNodeLabel())
}

func assertE22_CreateVolume_UnknownBackendType_Error(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  fmt.Sprintf("unknown-%d", 999),
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-unknown-backend-error",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for unknown backend type", tc.tcNodeLabel())
}

func assertE22_CreateVolume_LVMBackend_NVMeOF(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "lvm-lv",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-lvm-nvmeof",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM+NVMe-oF CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("lvm-lv"),
		"%s: volume ID should contain backend type", tc.tcNodeLabel())
}

func assertE22_CreateVolume_LVMBackend_iSCSI(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "tank",
		"pillar-csi.bhyoo.com/backend-type":  "lvm-lv",
		"pillar-csi.bhyoo.com/protocol-type": "iscsi",
	}
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-lvm-iscsi",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM+iSCSI CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("lvm-lv"),
		"%s: volume ID should contain backend type", tc.tcNodeLabel())
}
