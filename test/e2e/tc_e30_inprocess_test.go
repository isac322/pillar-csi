package e2e

// tc_e30_inprocess_test.go — Per-TC assertions for E30: LVM LV no-duplication.
//
// E30 verifies the CSI controller's skipBackend behaviour that prevents
// duplicate LV creation when CreateVolume is retried after an export failure.
// The first CreateVolume attempt creates the LV (agent.CreateVolume = 1) but
// fails during export; the PillarVolume CRD is persisted with
// Phase=CreatePartial. On retry, the controller detects the CreatePartial CRD
// and skips agent.CreateVolume entirely, going straight to agent.ExportVolume.
//
// All assertions use the controllerTestEnv (fakeAgentServer via bufconn).

import (
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCSILVMNoDup_ExactlyOneLVAfterExportFailureRetry — after export failure and
// successful retry, agent.CreateVolume was called exactly once (skipBackend).
func assertE30_ExactlyOneLV_AfterExportFailureRetry(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	volName := "pvc-e30-retry"
	caps := []*csiapi.VolumeCapability{mountCapability("ext4")}

	// Step 1: Inject export failure → CreateVolume returns error.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failure injected")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               volName,
		Parameters:         params,
		VolumeCapabilities: caps,
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).To(HaveOccurred(), "%s: first CreateVolume should fail (export error)", tc.tcNodeLabel())
	createCallsAfterFirst := env.agentSrv.counts().CreateVolume
	Expect(createCallsAfterFirst).To(Equal(1),
		"%s: agent.CreateVolume must be called exactly once on first attempt", tc.tcNodeLabel())

	// Step 2: Remove export failure → retry succeeds.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               volName,
		Parameters:         params,
		VolumeCapabilities: caps,
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: retry CreateVolume should succeed (skipBackend)", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId after retry", tc.tcNodeLabel())
	Expect(strings.Contains(resp.GetVolume().GetVolumeId(), "lvm-lv")).To(BeTrue(),
		"%s: VolumeId should contain 'lvm-lv'", tc.tcNodeLabel())

	// Step 3: Verify skipBackend — agent.CreateVolume total count is still 1.
	createCallsAfterRetry := env.agentSrv.counts().CreateVolume
	Expect(createCallsAfterRetry).To(Equal(createCallsAfterFirst),
		"%s: agent.CreateVolume must NOT be called again on retry (skipBackend): "+
			"before=%d after=%d", tc.tcNodeLabel(), createCallsAfterFirst, createCallsAfterRetry)

	// agent.ExportVolume must have been called twice (once failed, once succeeded).
	exportCalls := env.agentSrv.counts().ExportVolume
	Expect(exportCalls).To(BeNumerically(">=", 2),
		"%s: agent.ExportVolume must be called at least twice (fail+retry)", tc.tcNodeLabel())
}

// TestCSILVMNoDup_LVRegistryReflectsDeleteAfterPartialCreate — after a partial
// create (export fails), DeleteVolume cleans up the LV (registry goes 1→0).
func assertE30_LVRegistry_DeleteAfterPartialCreate(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	volName := "pvc-e30-del-partial"
	caps := []*csiapi.VolumeCapability{mountCapability("ext4")}

	// Step 1: Inject export failure → partial create.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed — partial create")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               volName,
		Parameters:         params,
		VolumeCapabilities: caps,
	})
	Expect(err).To(HaveOccurred(), "%s: partial create should fail", tc.tcNodeLabel())

	// Verify exactly 1 agent.CreateVolume call (LV "created" in partial state).
	Expect(env.agentSrv.counts().CreateVolume).To(Equal(1),
		"%s: exactly 1 agent.CreateVolume on partial create", tc.tcNodeLabel())

	// Step 2: Build the VolumeId from what the controller would have computed.
	// Format: <target>/nvmeof-tcp/lvm-lv/data-vg/<volName>
	vid := env.target.Name + "/nvmeof-tcp/lvm-lv/data-vg/" + volName

	// Step 3: Remove export error; DeleteVolume should unexport + delete.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: vid,
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: DeleteVolume on partial-created volume should succeed", tc.tcNodeLabel())

	// Verify agent.DeleteVolume was called (LV removed → registry 1→0).
	Expect(env.agentSrv.counts().DeleteVolume).To(BeNumerically(">=", 1),
		"%s: agent.DeleteVolume must be called (LV registry 1→0)", tc.tcNodeLabel())
}

// TestCSILVMNoDup_MultipleRetriesNeverDuplicate — 3 consecutive export failures
// followed by 1 success: agent.CreateVolume is called exactly once throughout.
func assertE30_MultipleRetries_NeverDuplicate(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := lvmControllerParams(env.target.Name)
	volName := "pvc-e30-multi-retry"
	caps := []*csiapi.VolumeCapability{mountCapability("ext4")}

	// Inject persistent export failure.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "persistent export failure")
	env.agentSrv.mu.Unlock()

	// Steps 1–3: Three consecutive failures.
	for i := range 3 {
		_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               volName,
			Parameters:         params,
			VolumeCapabilities: caps,
		})
		Expect(err).To(HaveOccurred(),
			"%s: CreateVolume attempt %d should fail (export error)", tc.tcNodeLabel(), i+1)

		// LV count must remain exactly 1 after each failure (skipBackend on retries).
		Expect(env.agentSrv.counts().CreateVolume).To(Equal(1),
			"%s: agent.CreateVolume count must be 1 after %d retries (no duplication)",
			tc.tcNodeLabel(), i+1)
	}

	// Step 4: Remove error → final attempt succeeds.
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               volName,
		Parameters:         params,
		VolumeCapabilities: caps,
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: 4th CreateVolume attempt should succeed", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(),
		"%s: VolumeId after final success", tc.tcNodeLabel())

	// Final check: agent.CreateVolume called exactly once throughout all retries.
	Expect(env.agentSrv.counts().CreateVolume).To(Equal(1),
		"%s: agent.CreateVolume must be exactly 1 across all 4 attempts (skipBackend)", tc.tcNodeLabel())
	// agent.ExportVolume called 4 times (3 fails + 1 success).
	Expect(env.agentSrv.counts().ExportVolume).To(Equal(4),
		"%s: agent.ExportVolume must be called 4 times (3 fail + 1 success)", tc.tcNodeLabel())
}
