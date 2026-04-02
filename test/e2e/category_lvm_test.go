package e2e

// category_lvm_test.go — Sub-AC 3: Real test assertions for all
// "full-lvm" category TCs.
//
// Full-LVM TCs cover the following spec groups:
//
//	F27 — LVM volume lifecycle (create, delete, expand)
//	F28 — LVM snapshot lifecycle
//	F29 — LVM thin-provisioning contracts
//	F30 — LVM error paths (VG not found, LV name collision, etc.)
//	F31 — LVM + NVMe-oF TCP export contracts
//
// These TCs validate the LVM backend stub implementation. All assertions run
// locally using verifyLVMLocalBackend and verifyKindBootstrapLocalContracts —
// no real LVM devices, VGs, or NVMe hardware are required.
//
// The "full-lvm" label in the default profile signals that these specs are
// gated behind a Kind + LVM integration scenario. In the local (no-hardware)
// execution model used by the default profile, the LVM verifier exercises the
// FakeLVMBackend from test/e2e/helpers/stubs.go.
//
// Every assertion embeds tc.tcNodeLabel() and tc.SectionTitle in its message so
// that the tc_failure_output.go ReportAfterEach hook can emit a structured
// single-line failure that is grep-addressable by TC ID.

import (
	. "github.com/onsi/gomega"
)

// runFullLVMTCBody executes the assertion body for a full-lvm TC.
//
// Strategy: run all verifiers in the local execution plan (kind bootstrap +
// LVM backend) and assert no errors. The LVM backend verifier exercises volume
// lifecycle, snapshot, thin-provisioning, error injection, and NVMe-oF export
// using the FakeLVMBackend stub — the tests are fully deterministic and
// require no real storage devices.
func runFullLVMTCBody(tc documentedCase, plan localExecutionPlan) {
	for _, verifierName := range plan.Verifiers {
		result := defaultLocalVerifierRegistry.Result(verifierName)
		Expect(result.Err).NotTo(HaveOccurred(),
			"%s[%s] FAIL: full-lvm verifier %q failed after %s: %v",
			tc.tcNodeLabel(), tc.SectionTitle, verifierName, result.Duration, result.Err,
		)
	}
}
