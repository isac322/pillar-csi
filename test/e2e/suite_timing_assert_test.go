package e2e

// suite_timing_assert_test.go — unit tests for the suite elapsed-time budget assertion.
//
// Acceptance criteria verified here:
//
//  1. checkSuiteElapsedBudget returns empty string when elapsed < budget (no violation).
//  2. checkSuiteElapsedBudget returns empty string when elapsed == budget (boundary ≤).
//  3. checkSuiteElapsedBudget returns non-empty message when elapsed > budget.
//  4. The violation message contains the actual elapsed time.
//  5. The violation message contains the budget limit in seconds.
//  6. The violation message contains the word "exceeded".
//  7. suiteElapsedFromReport uses report.RunTime when > 0.
//  8. suiteElapsedFromReport falls back to EndTime − StartTime when RunTime is zero.
//  9. suiteElapsedFromReport returns 0 when report has no timing data.
// 10. stageBudgetSeconds == 120 (matches suiteLevelTimeout from main_test.go).
// 11. checkSuiteElapsedBudget with elapsed=0 returns empty (no violation).
// 12. Violation message format: starts with "E2E suite exceeded".
//
// These tests run as plain Go unit tests (no Ginkgo suite) so they execute
// quickly via `go test -run TestSuiteTimingAssert ./test/e2e/`.

import (
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── 1. Within budget: no violation ───────────────────────────────────────────

// TestSuiteTimingAssertNoViolationWhenWithinBudget verifies that
// checkSuiteElapsedBudget returns empty when elapsed < budget limit.
func TestSuiteTimingAssertNoViolationWhenWithinBudget(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(90*time.Second, 120)
	if msg != "" {
		t.Errorf("checkSuiteElapsedBudget(90s, 120) = %q, want empty string (within budget)", msg)
	}
}

// ── 2. Exactly at limit: no violation (≤, not <) ─────────────────────────────

// TestSuiteTimingAssertNoViolationWhenExactlyAtLimit verifies the boundary
// condition: elapsed == limit is NOT a violation (the check is ≤, not <).
func TestSuiteTimingAssertNoViolationWhenExactlyAtLimit(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(120*time.Second, 120)
	if msg != "" {
		t.Errorf("checkSuiteElapsedBudget(120s, 120) = %q, want empty (boundary at limit must pass)", msg)
	}
}

// ── 3. Over limit: violation reported ────────────────────────────────────────

// TestSuiteTimingAssertViolationWhenOverBudget verifies that
// checkSuiteElapsedBudget returns a non-empty message when elapsed > budget.
func TestSuiteTimingAssertViolationWhenOverBudget(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(121*time.Second, 120)
	if msg == "" {
		t.Error("checkSuiteElapsedBudget(121s, 120) = empty; expected violation message when over budget")
	}
}

// ── 4. Violation message contains elapsed time ───────────────────────────────

// TestSuiteTimingAssertViolationMessageContainsElapsed verifies that the
// violation message includes the actual elapsed time so CI logs clearly show
// how far over budget the run was.
func TestSuiteTimingAssertViolationMessageContainsElapsed(t *testing.T) {
	t.Parallel()
	elapsed := 145 * time.Second
	msg := checkSuiteElapsedBudget(elapsed, 120)
	if msg == "" {
		t.Fatal("expected non-empty violation message for elapsed=145s")
	}
	// fmtDur(145s) produces "145.0s" — the message must contain "145".
	if !strings.Contains(msg, "145") {
		t.Errorf("violation message = %q; must contain actual elapsed time (145…)", msg)
	}
}

// ── 5. Violation message contains budget limit ───────────────────────────────

// TestSuiteTimingAssertViolationMessageContainsLimit verifies that the
// violation message includes the budget limit so the threshold is visible.
func TestSuiteTimingAssertViolationMessageContainsLimit(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(130*time.Second, 120)
	if msg == "" {
		t.Fatal("expected non-empty violation message for elapsed=130s")
	}
	if !strings.Contains(msg, "120") {
		t.Errorf("violation message = %q; must contain budget limit (120)", msg)
	}
}

// ── 6. Violation message contains "exceeded" ─────────────────────────────────

// TestSuiteTimingAssertViolationMessageContainsExceeded verifies that the
// violation message uses the word "exceeded" so the severity is unambiguous.
func TestSuiteTimingAssertViolationMessageContainsExceeded(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(125*time.Second, 120)
	if !strings.Contains(msg, "exceeded") {
		t.Errorf("violation message = %q; must contain 'exceeded'", msg)
	}
}

// ── 7. suiteElapsedFromReport uses report.RunTime ────────────────────────────

// TestSuiteElapsedFromReportUsesRunTime verifies that when report.RunTime > 0
// it is used as the authoritative elapsed duration.
func TestSuiteElapsedFromReportUsesRunTime(t *testing.T) {
	t.Parallel()
	want := 95 * time.Second
	report := types.Report{RunTime: want}
	got := suiteElapsedFromReport(report)
	if got != want {
		t.Errorf("suiteElapsedFromReport = %s, want %s (from RunTime)", got, want)
	}
}

// ── 8. suiteElapsedFromReport falls back to EndTime − StartTime ──────────────

// TestSuiteElapsedFromReportFallsBackToEndMinusStart verifies that when
// RunTime is zero, the function computes elapsed from EndTime − StartTime.
func TestSuiteElapsedFromReportFallsBackToEndMinusStart(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(80 * time.Second)
	report := types.Report{
		StartTime: start,
		EndTime:   end,
		// RunTime is zero — must fall back to EndTime − StartTime.
	}
	want := 80 * time.Second
	got := suiteElapsedFromReport(report)
	if got != want {
		t.Errorf("suiteElapsedFromReport = %s, want %s (EndTime − StartTime fallback)", got, want)
	}
}

// ── 9. suiteElapsedFromReport returns 0 with no timing data ──────────────────

// TestSuiteElapsedFromReportReturnsZeroWithNoTimingData verifies that when
// neither RunTime nor StartTime/EndTime is set, the function returns 0 (no
// panic, no garbage value).
func TestSuiteElapsedFromReportReturnsZeroWithNoTimingData(t *testing.T) {
	t.Parallel()
	report := types.Report{}
	got := suiteElapsedFromReport(report)
	if got != 0 {
		t.Errorf("suiteElapsedFromReport with zero report = %s, want 0", got)
	}
}

// ── 10. stageBudgetSeconds == 120 ────────────────────────────────────────────

// TestSuiteTimingAssertBudgetMatchesSuiteTimeout verifies that stageBudgetSeconds
// is 120 seconds, consistent with suiteLevelTimeout = 2 * time.Minute so
// the timing assertion and the Ginkgo suite timeout enforce the same limit.
func TestSuiteTimingAssertBudgetMatchesSuiteTimeout(t *testing.T) {
	t.Parallel()
	const wantSecs = 120
	if stageBudgetSeconds != wantSecs {
		t.Errorf("stageBudgetSeconds = %d, want %d (must match suiteLevelTimeout=2min)", stageBudgetSeconds, wantSecs)
	}
	// Cross-check: suiteLevelTimeout is 2 minutes = 120 seconds.
	if suiteLevelTimeout != 2*time.Minute {
		t.Errorf("suiteLevelTimeout = %s, want 2m0s", suiteLevelTimeout)
	}
}

// ── 11. Zero elapsed: no violation ───────────────────────────────────────────

// TestSuiteTimingAssertNoViolationWhenElapsedIsZero verifies that zero elapsed
// time does not produce a violation (e.g., when timing data is unavailable).
func TestSuiteTimingAssertNoViolationWhenElapsedIsZero(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(0, 120)
	if msg != "" {
		t.Errorf("checkSuiteElapsedBudget(0, 120) = %q, want empty (zero elapsed must not violate)", msg)
	}
}

// ── 12. Violation message format ─────────────────────────────────────────────

// TestSuiteTimingAssertViolationMessageFormat verifies that the violation
// message starts with the canonical prefix "E2E suite exceeded" so CI log
// parsers can reliably detect the assertion failure.
func TestSuiteTimingAssertViolationMessageFormat(t *testing.T) {
	t.Parallel()
	msg := checkSuiteElapsedBudget(200*time.Second, 120)
	if !strings.HasPrefix(msg, "E2E suite exceeded") {
		t.Errorf("violation message = %q; must start with 'E2E suite exceeded'", msg)
	}
}
