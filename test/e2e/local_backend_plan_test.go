package e2e

import "testing"

func TestResolveLocalExecutionPlanForDefaultProfile(t *testing.T) {
	t.Parallel()

	// Use a fresh registry instance to check verifier membership without
	// relying on the suiteLocalVerifierRegistry singleton (which is only
	// initialised during SynchronizedBeforeSuite in a Ginkgo run).
	reg := newLocalVerifierRegistry()

	for _, tc := range mustBuildDefaultProfile() {
		plan, err := resolveLocalExecutionPlan(tc)
		if err != nil {
			t.Fatalf("resolveLocalExecutionPlan(%s/%s): %v", tc.GroupKey, tc.DocID, err)
		}
		if plan.Summary == "" {
			t.Fatalf("empty summary for %s/%s", tc.GroupKey, tc.DocID)
		}
		if len(plan.Verifiers) == 0 {
			t.Fatalf("no verifiers for %s/%s", tc.GroupKey, tc.DocID)
		}
		for _, verifier := range plan.Verifiers {
			if !reg.Has(verifier) {
				t.Fatalf("missing verifier registry entry for %s (%s/%s)", verifier, tc.GroupKey, tc.DocID)
			}
		}
	}
}
