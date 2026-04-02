package e2e

// suite_timing_assert.go — Sub-AC 3 of AC 6: suite elapsed-time budget assertion.
//
// Adds a ReportAfterSuite hook that records the total wall-clock elapsed time
// of the Ginkgo suite and fails with a clear message if it exceeds the 120s
// budget.
//
// This is distinct from the Ginkgo suite Timeout (which aborts the suite with
// a generic "timed out" error) — this explicit assertion produces a
// human-readable failure message showing the actual elapsed time so a budget
// overrun is immediately diagnosable from CI output:
//
//	E2E suite exceeded 120s budget: elapsed 145.0s (limit 120s)
//
// The assertion runs in a ReportAfterSuite hook (primary Ginkgo process) after
// all spec results are collected. It uses report.RunTime (Ginkgo's authoritative
// wall-clock total) and falls back to EndTime − StartTime when RunTime is zero.

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// checkSuiteElapsedBudget returns a non-empty violation message when elapsed
// exceeds budgetSecs seconds, or an empty string when within budget (elapsed ≤ limit).
//
// Message format: "E2E suite exceeded %ds budget: elapsed %s (limit %ds)"
// Example: "E2E suite exceeded 120s budget: elapsed 145.0s (limit 120s)"
//
// This function is pure and dependency-free so it can be unit-tested without
// a Ginkgo suite context.
func checkSuiteElapsedBudget(elapsed time.Duration, budgetSecs int) string {
	limit := time.Duration(budgetSecs) * time.Second
	if elapsed <= limit {
		return ""
	}
	return fmt.Sprintf(
		"E2E suite exceeded %ds budget: elapsed %s (limit %ds)",
		budgetSecs, fmtDur(elapsed), budgetSecs,
	)
}

// suiteElapsedFromReport returns the total elapsed duration from the
// consolidated Ginkgo suite report.
//
// Preference order:
//  1. report.RunTime when > 0 (Ginkgo's authoritative duration)
//  2. report.EndTime − report.StartTime when both are non-zero (fallback)
//  3. 0 when neither source provides valid timing data
//
// This function is pure and dependency-free so it can be unit-tested without
// a Ginkgo suite context.
func suiteElapsedFromReport(report types.Report) time.Duration {
	if report.RunTime > 0 {
		return report.RunTime
	}
	if !report.StartTime.IsZero() && !report.EndTime.IsZero() {
		return report.EndTime.Sub(report.StartTime)
	}
	return 0
}

// suiteBudgetAssertionHook is the ReportAfterSuite handler that asserts the
// total suite elapsed time stayed within the 120-second budget.
// Runs on the primary Ginkgo process after all spec results are collected.
//
// When the budget is exceeded, Fail() is called with a human-readable message
// so the violation appears as an explicit test failure (not just an aborted
// timeout), making the actual elapsed time visible in CI output.
var _ = ReportAfterSuite("suite elapsed budget", func(report types.Report) {
	elapsed := suiteElapsedFromReport(report)
	if msg := checkSuiteElapsedBudget(elapsed, stageBudgetSeconds); msg != "" {
		Fail(msg)
	}
})
