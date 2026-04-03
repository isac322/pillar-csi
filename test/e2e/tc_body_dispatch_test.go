package e2e

// tc_body_dispatch_test.go — Sub-AC 3: Central dispatcher that routes each
// documented TC to the appropriate category-specific assertion body.
//
// runTCBody is the single entry point called by every It-node in
// default_profile_test.go. It resolves the local execution plan for the TC,
// validates that it is non-empty, then invokes the category-specific body
// function that performs the real assertions.
//
// Category routing (catalog-driven TCs only):
//
//	"in-process" → runInProcessTCBody(tc)
//	"envtest"    → runEnvtestTCBody(tc)
//	"cluster"    → runClusterTCBody(tc)
//
// E33, E34, E35, F27–F31 are NOT dispatched through runTCBody.
// Their specs live directly in *_e2e_test.go files with Label("default-profile")
// and run under the default label filter without going through the catalog.
//
// The category files (category_inprocess_test.go, category_envtest_test.go,
// category_cluster_test.go) each define their body function targeting their
// subset of TCs without overlap.
//
// Failure output: each body function is responsible for embedding the
// tc.tcNodeLabel() and tc.SectionTitle in its Expect/Fail messages so that the
// ReportAfterEach hook in tc_failure_output.go can surface them on one line.

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// runTCBody is the It-node body used for every TC in the default profile.
// It resolves the execution plan, then delegates to the appropriate
// category assertion function.
func runTCBody(tc documentedCase) {
	// Validate that a local execution plan exists for this TC.
	plan, err := resolveLocalExecutionPlan(tc)
	Expect(err).NotTo(HaveOccurred(),
		"%s[%s] FAIL: could not resolve local execution plan — group %q not covered",
		tc.tcNodeLabel(), tc.SectionTitle, tc.GroupKey,
	)
	Expect(plan.Summary).NotTo(BeEmpty(),
		"%s[%s] FAIL: local execution plan has empty summary for group %q",
		tc.tcNodeLabel(), tc.SectionTitle, tc.GroupKey,
	)
	Expect(plan.Verifiers).NotTo(BeEmpty(),
		"%s[%s] FAIL: local execution plan has no verifiers for group %q",
		tc.tcNodeLabel(), tc.SectionTitle, tc.GroupKey,
	)

	// Route to category-specific body.
	switch tc.Category {
	case "in-process":
		runInProcessTCBody(tc, plan)
	case "envtest":
		runEnvtestTCBody(tc, plan)
	case "cluster":
		runClusterTCBody(tc, plan)
	default:
		Fail(fmt.Sprintf("%s[%s] FAIL: unknown TC category %q",
			tc.tcNodeLabel(), tc.SectionTitle, tc.Category))
	}
}
