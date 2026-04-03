package e2e

// suite_result_summary.go — AC 6: Continue-on-failure with end-of-suite summary report.
//
// Default behavior: the suite continues running all specs even after a failure
// and emits a summary report at the end.  This ensures the full set of failures
// is visible in a single run rather than requiring re-runs to discover each one.
//
// Fail-fast mode: the -e2e.fail-fast flag (or E2E_FAILFAST=1 / E2E_FAIL_FAST=true
// env vars) enables stopping after the first spec failure — useful in CI fast
// paths or when debugging a single known-broken TC.
// E2E_FAILFAST (no underscore) is the canonical AC 10 interface; E2E_FAIL_FAST
// (with underscore) is the legacy alias accepted for backward compatibility.
//
// Summary report: always written to stderr by the ReportAfterSuite hook,
// regardless of whether -e2e.profile is set.  The report is intentionally
// machine-parseable: each line begins with a known prefix so CI tools can grep.
//
// Example output (failure case):
//
//	=== E2E Result Summary ===
//	suite: Pillar CSI E2E Suite
//	total: 416 | passed: 406 | failed: 15 | skipped: 0 | pending: 0
//	failed TCs (15):
//	  [TC-E1.2] [category:in-process] Expected true to be false
//	  [TC-F27.1] [category:full-lvm] Expected non-nil PVC to be nil
//	status: FAIL
//
// Example output (all-pass case):
//
//	=== E2E Result Summary ===
//	suite: Pillar CSI E2E Suite
//	total: 416 | passed: 416 | failed: 0 | skipped: 0 | pending: 0
//	status: PASS

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

const (
	// envFailFast is the primary environment variable name for fail-fast mode.
	// Priority: -e2e.fail-fast flag (when explicitly set) > E2E_FAILFAST env var >
	// E2E_FAIL_FAST env var > false.
	envFailFast = "E2E_FAIL_FAST"

	// envFailFastAlt is the alternate (no-underscore) environment variable name
	// for fail-fast mode.  E2E_FAILFAST=1 is the documented AC 10 interface;
	// E2E_FAIL_FAST is kept as a legacy alias for backward compatibility.
	// Both names are accepted and either value "1", "true", or "yes" activates
	// fail-fast mode.
	envFailFastAlt = "E2E_FAILFAST"
)

// e2eFailFastFlag corresponds to -e2e.fail-fast.
// When true the Ginkgo suite is configured with FailFast=true so that the first
// failing spec causes the run to stop immediately.
// Default false (AC 6 "continue on failure" default): the suite runs all specs
// and emits the full summary report at the end.
var e2eFailFastFlag = flag.Bool(
	"e2e.fail-fast",
	false,
	"stop after the first spec failure (env: E2E_FAILFAST or E2E_FAIL_FAST); "+
		"default false runs all specs and emits a complete summary report",
)

// resolveFailFast returns the effective fail-fast setting.
// Priority: -e2e.fail-fast flag (when explicitly set via command line) >
// E2E_FAILFAST env var > E2E_FAIL_FAST env var > false (default).
//
// E2E_FAILFAST (no underscore between FAIL and FAST) is the canonical AC 10
// interface.  E2E_FAIL_FAST (with underscore) is accepted as a legacy alias
// so that existing scripts and CI configs continue to work without changes.
func resolveFailFast() bool {
	// Check whether the flag was explicitly set on the command line.
	// flag.Visit only visits flags that were explicitly set, so when
	// -e2e.fail-fast=true is on the command line, this returns true.
	if flag.Parsed() {
		explicitly := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "e2e.fail-fast" {
				explicitly = true
			}
		})
		if explicitly {
			return *e2eFailFastFlag
		}
	}

	// Check the canonical AC 10 env var: E2E_FAILFAST (no underscore).
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envFailFastAlt))) {
	case "true", "1", "yes":
		return true
	}

	// Fall back to the legacy E2E_FAIL_FAST env var (with underscore).
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envFailFast))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// applyFailFast configures the Ginkgo suite config with FailFast=true when
// fail-fast mode is enabled.  Must be called before RunSpecs.
func applyFailFast(cfg *types.SuiteConfig) {
	if resolveFailFast() {
		cfg.FailFast = true
	}
}

// ── Result summary types ──────────────────────────────────────────────────────

// failedTCRecord holds the minimal information needed to describe a failed TC
// in the result summary.
type failedTCRecord struct {
	TCID     string
	Category string
	Message  string
}

// SuiteResultSummary is the structured data produced by collectSuiteResultSummary
// and rendered to stderr by the ReportAfterSuite hook.
type SuiteResultSummary struct {
	// SuiteName is the Ginkgo suite description.
	SuiteName string

	// Total, Passed, Failed, Skipped, Pending are spec counts.
	// Only It-node specs are counted; BeforeSuite/AfterSuite nodes are excluded.
	Total   int
	Passed  int
	Failed  int
	Skipped int
	Pending int

	// FailedTCs holds one entry per failed TC, sorted by TCID ascending.
	// Non-TC framework specs (e.g. BeforeSuite) do not appear here even if
	// they failed; their failures are surfaced by Ginkgo's own output.
	FailedTCs []failedTCRecord

	// SuiteStatus is "PASS" when Failed == 0 and the Ginkgo suite succeeded,
	// "FAIL" otherwise.
	SuiteStatus string
}

// collectSuiteResultSummary builds a SuiteResultSummary from the Ginkgo suite report.
// Only It-node specs are counted; suite-level setup/teardown nodes are skipped.
func collectSuiteResultSummary(report types.Report) SuiteResultSummary {
	s := SuiteResultSummary{
		SuiteName: strings.TrimSpace(report.SuiteDescription),
	}

	for _, spec := range report.SpecReports {
		// Only count leaf-node specs (It / Entry).  BeforeSuite, AfterSuite, and
		// other container hooks are not test cases and must not inflate the counts.
		if spec.LeafNodeType != types.NodeTypeIt {
			continue
		}

		s.Total++
		switch spec.State {
		case types.SpecStatePassed:
			s.Passed++

		case types.SpecStateFailed, types.SpecStatePanicked, types.SpecStateTimedout:
			s.Failed++
			tcID := extractTCIDFromReport(spec)
			category := extractCategoryFromReport(spec)
			msg := truncateForSummary(extractFailureMessage(spec))
			// Only record TCs we can identify; anonymous specs are still counted
			// in s.Failed but are omitted from the FailedTCs list.
			if tcID != "" {
				s.FailedTCs = append(s.FailedTCs, failedTCRecord{
					TCID:     tcID,
					Category: category,
					Message:  msg,
				})
			}

		case types.SpecStateSkipped:
			s.Skipped++

		case types.SpecStatePending:
			s.Pending++
		}
	}

	// Sort failed TCs by TCID for deterministic, human-readable output.
	sort.Slice(s.FailedTCs, func(i, j int) bool {
		return s.FailedTCs[i].TCID < s.FailedTCs[j].TCID
	})

	if s.Failed > 0 || !report.SuiteSucceeded {
		s.SuiteStatus = "FAIL"
	} else {
		s.SuiteStatus = "PASS"
	}

	return s
}

// truncateForSummary limits a failure message to 120 characters so the summary
// table stays readable when printed to a narrow terminal.  The full message is
// always available in the per-TC failure line emitted by tc_failure_output.go.
func truncateForSummary(msg string) string {
	const maxLen = 120
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen] + "…"
}

// writeSuiteResultSummary emits the result summary to the given writer.
// The format is both human-readable and machine-parseable: each line starts
// with a known keyword so CI tools can parse it with simple grep/awk.
func writeSuiteResultSummary(w io.Writer, s SuiteResultSummary) error {
	if w == nil {
		return nil
	}

	if _, err := fmt.Fprintln(w, "=== E2E Result Summary ==="); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "suite: %s\n", s.SuiteName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w,
		"total: %d | passed: %d | failed: %d | skipped: %d | pending: %d\n",
		s.Total, s.Passed, s.Failed, s.Skipped, s.Pending); err != nil {
		return err
	}

	if s.Failed > 0 {
		if _, err := fmt.Fprintf(w, "failed TCs (%d):\n", s.Failed); err != nil {
			return err
		}
		for _, tc := range s.FailedTCs {
			var line string
			if tc.Category != "" {
				line = fmt.Sprintf("  [TC-%s] [category:%s] %s\n", tc.TCID, tc.Category, tc.Message)
			} else {
				line = fmt.Sprintf("  [TC-%s] %s\n", tc.TCID, tc.Message)
			}
			if _, err := fmt.Fprint(w, line); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintf(w, "status: %s\n", s.SuiteStatus)
	return err
}

// suiteResultSummaryHook is the ReportAfterSuite handler that writes the
// result summary to stderr after all specs have finished.  It runs on the
// primary Ginkgo process, so the consolidated report it receives reflects the
// outcome of all parallel workers.
//
// The summary is always emitted regardless of -e2e.profile, because it serves
// a different purpose: a quick human-readable digest of the run outcome.
var _ = ReportAfterSuite("suite result summary", func(report types.Report) {
	summary := collectSuiteResultSummary(report)
	_ = writeSuiteResultSummary(os.Stderr, summary)
})
