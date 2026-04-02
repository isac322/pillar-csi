package e2e

import "testing"

func TestResolveLocalExecutionPlanForDefaultProfile(t *testing.T) {
	t.Parallel()

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
			if !defaultLocalVerifierRegistry.Has(verifier) {
				t.Fatalf("missing verifier registry entry for %s (%s/%s)", verifier, tc.GroupKey, tc.DocID)
			}
		}
	}
}
