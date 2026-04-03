package e2e

// category_cluster_test.go — Sub-AC 3: Real test assertions for all
// "cluster" category TCs.
//
// Cluster TCs cover the following spec groups:
//
//	E10 — Kind bootstrap and invocation-scoped lifecycle contracts
//	E33 — Kind + LVM NVMe-oF integration
//	E34 — Kind + iSCSI integration
//	E35 — Kind + ZFS integration
//
// These TCs validate that the Kind cluster lifecycle (create/destroy per
// go test invocation) and the storage backend integrations work correctly in a
// real Kubernetes environment.
//
// Local execution: all cluster TCs run locally using the kind bootstrap
// verifier (verifyKindBootstrapLocalContracts) together with the appropriate
// backend verifier (verifyLVMLocalBackend or verifyZFSLocalBackend). No real
// NVMe/iSCSI hardware is required — the backend verifiers use local stub
// implementations.
//
// Every assertion embeds tc.tcNodeLabel() and tc.SectionTitle in its message so
// that the tc_failure_output.go ReportAfterEach hook can emit a structured
// single-line failure that is grep-addressable by TC ID.

import (
	. "github.com/onsi/gomega"
)

// runClusterTCBody executes the assertion body for a cluster TC.
//
// Strategy: run all verifiers listed in the local execution plan and assert
// that each produced no error. Cluster TCs typically list both the kind
// bootstrap verifier and a backend verifier so that a single TC exercises the
// full cluster+backend integration path.
func runClusterTCBody(tc documentedCase, plan localExecutionPlan) {
	for _, verifierName := range plan.Verifiers {
		result := suiteLocalVerifierRegistry.Result(verifierName)
		Expect(result.Err).NotTo(HaveOccurred(),
			"%s[%s] FAIL: cluster verifier %q failed after %s: %v",
			tc.tcNodeLabel(), tc.SectionTitle, verifierName, result.Duration, result.Err,
		)
	}
}
