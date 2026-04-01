package e2e

// tc_e30_inprocess_test.go — Per-TC assertions for E30: LVM LV no-duplication.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertE30_LVM_NoDup_ExportFailureRetry(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)

	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failure")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e30-lvm-nodup-retry",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: expected export failure on first attempt", tc.tcNodeLabel())
	firstCreateCalls := env.agentSrv.counts().CreateVolume

	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e30-lvm-nodup-retry",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: second CreateVolume after export-retry should succeed", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: volumeID empty after retry", tc.tcNodeLabel())

	secondCreateCalls := env.agentSrv.counts().CreateVolume
	Expect(secondCreateCalls).To(BeNumerically("<=", firstCreateCalls+1),
		"%s: agent CreateVolume called more than once on retry", tc.tcNodeLabel())
}

func assertE30_LVM_NoDup_DeleteAfterPartial(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)

	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e30-lvm-nodup-del",
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected partial create failure", tc.tcNodeLabel())
	agentCreateCalls := env.agentSrv.counts().CreateVolume

	Expect(agentCreateCalls).To(Equal(1),
		"%s: expected exactly 1 agent CreateVolume call for partial-create, got %d",
		tc.tcNodeLabel(), agentCreateCalls)
}

func assertE30_LVM_NoDup_MultipleRetries(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)

	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "persistent export failure")
	env.agentSrv.mu.Unlock()

	for range 3 {
		_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               "pvc-e30-lvm-nodup-multi",
			Parameters:         params,
			VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		})
		Expect(err).To(HaveOccurred(), "%s: expected failure during retry loop", tc.tcNodeLabel())
	}

	agentCreateCalls := env.agentSrv.counts().CreateVolume
	Expect(agentCreateCalls).To(Equal(1),
		"%s: agent CreateVolume called %d times across 3 retries — LV duplication detected",
		tc.tcNodeLabel(), agentCreateCalls)
}
