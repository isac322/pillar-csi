package e2e

// tc_e21_inprocess_test.go — Per-TC assertions for E21: Invalid CR error scenarios.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertE21_MissingStore(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-missing-store",
		Parameters:         e2eHandWrittenParams("no-such-store", e2eDefaultProtocolName),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for a missing PillarStore", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
		"%s: expected FailedPrecondition, got %v", tc.tcNodeLabel(), err)
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call when resolution fails", tc.tcNodeLabel())
}

func assertE21_MissingAgent(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	Expect(env.k8sClient.Create(env.ctx, e2eZFSStore("orphan-store", "no-such-agent", "tank"))).To(Succeed())

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-missing-agent",
		Parameters:         e2eHandWrittenParams("orphan-store", e2eDefaultProtocolName),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for a PillarStore whose agent does not exist", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.NotFound),
		"%s: expected NotFound, got %v", tc.tcNodeLabel(), err)
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call when resolution fails", tc.tcNodeLabel())
}

func assertE21_MissingProtocol(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-missing-protocol",
		Parameters:         e2eHandWrittenParams(e2eDefaultStoreName, "no-such-protocol"),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for a missing PillarProtocol", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
		"%s: expected FailedPrecondition, got %v", tc.tcNodeLabel(), err)
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call when resolution fails", tc.tcNodeLabel())
}

func assertE21_MissingBinding(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-missing-binding",
		Parameters:         e2eBindingParams("no-such-binding"),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for a missing PillarStorageClass", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
		"%s: expected FailedPrecondition, got %v", tc.tcNodeLabel(), err)
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call when resolution fails", tc.tcNodeLabel())
}

func assertE21_EmptyTargetAddress(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Modify target to have empty resolved address.
	env.target.Status.ResolvedAddress = ""
	_ = env.k8sClient.Update(env.ctx, env.target)

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-empty-addr",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for empty target address", tc.tcNodeLabel())
}

func assertE21_TargetAddressFormat(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Target address is set (valid format) — this should succeed
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e21-addr-format",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: valid target address should succeed", tc.tcNodeLabel())
}
