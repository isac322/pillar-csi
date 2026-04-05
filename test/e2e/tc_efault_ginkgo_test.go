package e2e

// tc_efault_ginkgo_test.go — Ginkgo It-node wrappers for E-FAULT and E-NEW test cases.
// These specs run in-process using the mock agent infrastructure.
// They appear in ginkgo --dry-run output (no build tag) so that verify-tc-ids passes.

import (
	. "github.com/onsi/ginkgo/v2"
)

var _ = Describe("E-FAULT: Fault injection scenarios", Label("e-fault"), func() {
	It("[TC-E-FAULT-1-1] TestE2E_NodeReboot_AgentRecovery", func() {
		assertEFAULT_NodeReboot_AgentRecovery(documentedCase{
			DocID: "E-FAULT-1-1", TestName: "TestE2E_NodeReboot_AgentRecovery",
		})
	})
	It("[TC-E-FAULT-2-1] TestE2E_AgentNetworkPartition_CreateVolumeFails", func() {
		assertEFAULT_AgentNetworkPartition_CreateVolumeFails(documentedCase{
			DocID: "E-FAULT-2-1", TestName: "TestE2E_AgentNetworkPartition_CreateVolumeFails",
		})
	})
	It("[TC-E-FAULT-2-2] TestE2E_AgentNetworkPartition_Recovery", func() {
		assertEFAULT_AgentNetworkPartition_Recovery(documentedCase{
			DocID: "E-FAULT-2-2", TestName: "TestE2E_AgentNetworkPartition_Recovery",
		})
	})
	It("[TC-E-FAULT-3-1] TestE2E_PoolExhaustion_CreateVolumeFails", func() {
		assertEFAULT_PoolExhaustion_CreateVolumeFails(documentedCase{
			DocID: "E-FAULT-3-1", TestName: "TestE2E_PoolExhaustion_CreateVolumeFails",
		})
	})
	It("[TC-E-FAULT-4-1] TestE2E_BackingDeviceRemoved_GracefulError", func() {
		assertEFAULT_BackingDeviceRemoved_GracefulError(documentedCase{
			DocID: "E-FAULT-4-1", TestName: "TestE2E_BackingDeviceRemoved_GracefulError",
		})
	})
	It("[TC-E-FAULT-5-1] TestE2E_MultiNode_VolumeAccessFromDifferentWorker", func() {
		assertEFAULT_MultiNode_VolumeAccessFromDifferentWorker(documentedCase{
			DocID: "E-FAULT-5-1", TestName: "TestE2E_MultiNode_VolumeAccessFromDifferentWorker",
		})
	})
})

var _ = Describe("E-NEW: PRD gap E2E scenarios", Label("e-new"), func() {
	It("[TC-E-NEW-1-1] TestHelm_InitContainer_ModprobeFailure_PodStarts", func() {
		assertENEW_Helm_InitContainer_ModprobeFailure_PodStarts(documentedCase{
			DocID: "E-NEW-1-1", TestName: "TestHelm_InitContainer_ModprobeFailure_PodStarts",
		})
	})
})
