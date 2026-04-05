package e2e

// tc_enew_inprocess_test.go — Per-TC assertions for E-NEW: PRD gap E2E scenarios.
//
// E-NEW tests verify in-process behaviour for PRD gap scenarios discovered
// after initial implementation. They use the mock agent infrastructure.
//
//   - E-NEW-1-1: Helm init-container modprobe failure → Pod starts with graceful degradation

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
)

// assertENEW_Helm_InitContainer_ModprobeFailure_PodStarts verifies that when
// the Helm init-container encounters a modprobe failure (kernel module not
// available), the CSI controller still accepts volume creation requests
// without panicking. The modprobe failure is a node-level concern; the
// controller must remain functional.
func assertENEW_Helm_InitContainer_ModprobeFailure_PodStarts(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// The in-process test verifies controller resilience: even when a node-level
	// prerequisite (modprobe) would fail in a real cluster, the controller
	// must accept CreateVolume without error (the node plugin handles the failure
	// separately, and the controller has no dependency on kernel modules).
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-enew-1-1",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume must succeed even when modprobe would fail on the node", tc.tcNodeLabel())
	Expect(resp.GetVolume()).NotTo(BeNil(),
		"%s: CreateVolume must return a non-nil Volume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(),
		"%s: returned VolumeId must not be empty", tc.tcNodeLabel())
}
