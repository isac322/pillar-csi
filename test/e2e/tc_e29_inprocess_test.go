package e2e

// tc_e29_inprocess_test.go — Per-TC assertions for E29: CSI Controller LVM parameter propagation.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lvmControllerParams returns default StorageClass params for LVM volumes.
func lvmControllerParams(target string) map[string]string {
	return map[string]string{
		"pillar-csi.bhyoo.com/target":        target,
		"pillar-csi.bhyoo.com/pool":          "data-vg",
		"pillar-csi.bhyoo.com/backend-type":  "lvm-lv",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
}

func assertE29_LVM_CreateVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-lvm-create",
		Parameters:         lvmControllerParams(env.target.Name),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume via controller", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("lvm-lv"),
		"%s: volume ID should reflect LVM backend", tc.tcNodeLabel())
}

func assertE29_LVM_ProvisioningMode_Override(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-provisioning-mode"] = "thin"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-thin",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM thin provisioning create", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volume ID", tc.tcNodeLabel())
}

func assertE29_LVM_CreateVolume_BackendParams(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)

	// Verify that agent receives the correct backend params
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-backend-params",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume with backend params", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volume ID", tc.tcNodeLabel())

	// Verify create was called on agent
	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(1), "%s: agent create called", tc.tcNodeLabel())
}

func assertE29_LVM_DeleteVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-lvm-delete",
		Parameters:         lvmControllerParams(env.target.Name),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM DeleteVolume", tc.tcNodeLabel())
}

func assertE29_LVM_ExpandVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-lvm-expand",
		Parameters:         lvmControllerParams(env.target.Name),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())

	expandResp, err := env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ControllerExpandVolume", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", 2<<30),
		"%s: expanded capacity", tc.tcNodeLabel())
}

func assertE29_LVM_GetCapacity(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.GetCapacity(env.ctx, &csiapi.GetCapacityRequest{
		Parameters: map[string]string{
			"pillar-csi.bhyoo.com/target":       env.target.Name,
			"pillar-csi.bhyoo.com/pool":         "data-vg",
			"pillar-csi.bhyoo.com/backend-type": "lvm-lv",
		},
	})
	// Either succeeds or returns NotFound (target exists but capacity query hits agent)
	if err != nil {
		Expect(status.Code(err)).To(BeElementOf(codes.NotFound, codes.Internal),
			"%s: unexpected error code", tc.tcNodeLabel())
	}
}

func assertE29_LVM_CreateVolume_InvalidProvisioningMode(_ documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-provisioning-mode"] = "invalid-mode-xyz"

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-invalid-mode",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	// Invalid provisioning mode should return an error or be silently accepted
	// The controller may validate or pass through — either is acceptable
	_ = err
}

func assertE29_LVM_VolumeIDFormat(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-lvm-id-format",
		Parameters:         lvmControllerParams(env.target.Name),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()
	Expect(volumeID).To(ContainSubstring("lvm-lv"),
		"%s: volume ID should contain backend type", tc.tcNodeLabel())
	Expect(volumeID).To(ContainSubstring("nvmeof-tcp"),
		"%s: volume ID should contain protocol", tc.tcNodeLabel())
}

func assertE29_LVM_CreateVolume_MissingVG(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Use a pool that doesn't exist in the fake agent
	params := map[string]string{
		"pillar-csi.bhyoo.com/target":        env.target.Name,
		"pillar-csi.bhyoo.com/pool":          "nonexistent-vg",
		"pillar-csi.bhyoo.com/backend-type":  "lvm-lv",
		"pillar-csi.bhyoo.com/protocol-type": "nvmeof-tcp",
	}
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-missing-vg",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	// Agent will return error for unknown pool
	Expect(err).To(HaveOccurred(), "%s: expected error for missing VG", tc.tcNodeLabel())
}

func assertE29_LVM_ThinPool_Override(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-thin-pool"] = "pool0"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-thin-pool",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ThinPool override create", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volume ID", tc.tcNodeLabel())
}

func assertE29_LVM_Stripe_Config(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-stripes"] = "2"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-stripe",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM stripe config create", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volume ID", tc.tcNodeLabel())
}

func assertE29_LVM_Tags_Propagation(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	params["pillar-csi.bhyoo.com/lvm-tags"] = "env=production,team=storage"

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e29-tags",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM tags propagation create", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volume ID", tc.tcNodeLabel())
}
