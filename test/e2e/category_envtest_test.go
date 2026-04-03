package e2e

// category_envtest_test.go — Sub-AC 3: Real test assertions for all
// "envtest" category TCs.
//
// Envtest TCs cover the following spec groups:
//
//	E19 — CRD validation: PillarTarget
//	E20 — CRD validation: PillarPool
//	E23 — CRD validation: PillarProtocol
//	E25 — Webhook validation
//	E26 — Compatibility: schema evolution
//	E32 — Manifest contracts
//
// These TCs validate that the CRD schemas, webhook rules, and manifest
// contracts produced by `make manifests` / `make generate` are consistent with
// the runtime behaviour of the controller.  They use the CRD local verifier
// (verifyCRDLocalContracts) which runs fully in-process using
// controller-runtime's envtest or fake client infrastructure — no external
// cluster required.
//
// Every assertion embeds tc.tcNodeLabel() and tc.SectionTitle in its message so
// that the tc_failure_output.go ReportAfterEach hook can emit a structured
// single-line failure that is grep-addressable by TC ID.

import (
	. "github.com/onsi/gomega"
)

// runEnvtestTCBody executes the assertion body for an envtest TC.
//
// Strategy: identical to runInProcessTCBody — run all cached verifiers and
// assert that each produced no error.  The envtest TCs use the
// localVerifierCRD verifier, which exercises CRD schema validation, defaulting
// webhooks, and manifest contracts using a fake controller-runtime client.
func runEnvtestTCBody(tc documentedCase, plan localExecutionPlan) {
	for _, verifierName := range plan.Verifiers {
		result := suiteLocalVerifierRegistry.Result(verifierName)
		Expect(result.Err).NotTo(HaveOccurred(),
			"%s[%s] FAIL: envtest verifier %q failed after %s: %v",
			tc.tcNodeLabel(), tc.SectionTitle, verifierName, result.Duration, result.Err,
		)
	}
}
