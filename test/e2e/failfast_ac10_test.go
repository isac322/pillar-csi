package e2e

// failfast_ac10_test.go — AC 10: Default continue-on-failure with summary
// report and E2E_FAILFAST=1 stops on first failure.
//
// Acceptance criteria verified here:
//
//  1. resolveFailFast returns false by default (continue-on-failure).
//  2. E2E_FAILFAST=1 (canonical no-underscore form) activates fail-fast mode.
//  3. E2E_FAILFAST=true activates fail-fast mode.
//  4. E2E_FAILFAST=yes activates fail-fast mode.
//  5. E2E_FAILFAST is case-insensitive ("TRUE", "True", "YES", "Yes").
//  6. E2E_FAILFAST= (empty) does NOT activate fail-fast mode.
//  7. E2E_FAILFAST=false does NOT activate fail-fast mode.
//  8. E2E_FAILFAST=0 does NOT activate fail-fast mode.
//  9. Legacy E2E_FAIL_FAST=true still activates fail-fast (backward compat).
// 10. Legacy E2E_FAIL_FAST=1 still activates fail-fast (backward compat).
// 11. E2E_FAILFAST takes priority over E2E_FAIL_FAST when both are set.
// 12. applyFailFast sets suiteConfig.FailFast=true when E2E_FAILFAST=1.
// 13. applyFailFast is a no-op when E2E_FAILFAST is absent.
// 14. Summary report header "=== E2E Result Summary ===" is always emitted.
// 15. Summary report includes "status: PASS" when all specs pass.
// 16. Summary report includes "status: FAIL" when any spec fails.
// 17. Summary report lists all failed TCs (continue-on-failure collects all).
// 18. Summary report does NOT emit "failed TCs" section when all pass.
// 19. writeSuiteResultSummary is idempotent — same input always produces same output.
// 20. envFailFastAlt constant equals "E2E_FAILFAST" (canonical AC 10 env var name).
//
// These tests run as plain Go unit tests (no Ginkgo suite), so they execute
// quickly via `go test -run TestAC10 ./test/e2e/`.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── 1. Default is false (continue-on-failure) ────────────────────────────────

// TestAC10DefaultContinueOnFailure verifies that resolveFailFast returns false
// when neither E2E_FAILFAST nor E2E_FAIL_FAST is set, confirming the AC 10
// "continue on failure" default contract.
func TestAC10DefaultContinueOnFailure(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "")

	original := os.Getenv(envFailFastAlt)
	if original != "" {
		t.Skipf("%s=%q is set; skipping default-false test", envFailFastAlt, original)
	}
	originalLegacy := os.Getenv(envFailFast)
	if originalLegacy != "" {
		t.Skipf("%s=%q is set; skipping default-false test", envFailFast, originalLegacy)
	}

	if resolveFailFast() {
		t.Error("resolveFailFast() = true, want false (AC 10: default continue-on-failure)")
	}
}

// ── 2. E2E_FAILFAST=1 activates fail-fast ───────────────────────────────────

// TestAC10FailFastActivatedByCanonicalEnvVar1 verifies that E2E_FAILFAST=1
// (the canonical AC 10 form with numeric value) activates fail-fast mode.
func TestAC10FailFastActivatedByCanonicalEnvVar1(t *testing.T) {
	t.Setenv(envFailFastAlt, "1")
	t.Setenv(envFailFast, "")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true when %s=1", envFailFastAlt)
	}
}

// ── 3. E2E_FAILFAST=true activates fail-fast ────────────────────────────────

// TestAC10FailFastActivatedByCanonicalEnvVarTrue verifies that E2E_FAILFAST=true
// activates fail-fast mode.
func TestAC10FailFastActivatedByCanonicalEnvVarTrue(t *testing.T) {
	t.Setenv(envFailFastAlt, "true")
	t.Setenv(envFailFast, "")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true when %s=true", envFailFastAlt)
	}
}

// ── 4. E2E_FAILFAST=yes activates fail-fast ─────────────────────────────────

// TestAC10FailFastActivatedByCanonicalEnvVarYes verifies that E2E_FAILFAST=yes
// activates fail-fast mode.
func TestAC10FailFastActivatedByCanonicalEnvVarYes(t *testing.T) {
	t.Setenv(envFailFastAlt, "yes")
	t.Setenv(envFailFast, "")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true when %s=yes", envFailFastAlt)
	}
}

// ── 5. E2E_FAILFAST is case-insensitive ─────────────────────────────────────

// TestAC10FailFastCaseInsensitive verifies that E2E_FAILFAST is matched
// case-insensitively so "TRUE", "True", "YES", "Yes" all activate fail-fast.
func TestAC10FailFastCaseInsensitive(t *testing.T) {
	for _, val := range []string{"TRUE", "True", "YES", "Yes", "1"} {
		val := val
		t.Run("E2E_FAILFAST="+val, func(t *testing.T) {
			t.Setenv(envFailFastAlt, val)
			t.Setenv(envFailFast, "")
			if !resolveFailFast() {
				t.Errorf("resolveFailFast() = false; want true when %s=%q", envFailFastAlt, val)
			}
		})
	}
}

// ── 6. E2E_FAILFAST= (empty) does NOT activate fail-fast ────────────────────

// TestAC10FailFastNotActivatedByEmptyEnvVar verifies that setting E2E_FAILFAST
// to an empty string does NOT activate fail-fast mode.
func TestAC10FailFastNotActivatedByEmptyEnvVar(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "")

	if resolveFailFast() {
		t.Errorf("resolveFailFast() = true; want false when %s is empty", envFailFastAlt)
	}
}

// ── 7. E2E_FAILFAST=false does NOT activate fail-fast ───────────────────────

// TestAC10FailFastNotActivatedByFalse verifies that E2E_FAILFAST=false
// explicitly disables fail-fast mode.
func TestAC10FailFastNotActivatedByFalse(t *testing.T) {
	t.Setenv(envFailFastAlt, "false")
	t.Setenv(envFailFast, "")

	if resolveFailFast() {
		t.Errorf("resolveFailFast() = true; want false when %s=false", envFailFastAlt)
	}
}

// ── 8. E2E_FAILFAST=0 does NOT activate fail-fast ───────────────────────────

// TestAC10FailFastNotActivatedByZero verifies that E2E_FAILFAST=0 does NOT
// activate fail-fast mode.
func TestAC10FailFastNotActivatedByZero(t *testing.T) {
	t.Setenv(envFailFastAlt, "0")
	t.Setenv(envFailFast, "")

	if resolveFailFast() {
		t.Errorf("resolveFailFast() = true; want false when %s=0", envFailFastAlt)
	}
}

// ── 9. Legacy E2E_FAIL_FAST=true still works ────────────────────────────────

// TestAC10LegacyEnvVarTrueActivatesFailFast verifies that the legacy
// E2E_FAIL_FAST=true env var still activates fail-fast for backward
// compatibility with existing CI scripts and workflows.
func TestAC10LegacyEnvVarTrueActivatesFailFast(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "true")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true for legacy %s=true", envFailFast)
	}
}

// ── 10. Legacy E2E_FAIL_FAST=1 still works ──────────────────────────────────

// TestAC10LegacyEnvVar1ActivatesFailFast verifies that the legacy
// E2E_FAIL_FAST=1 env var still activates fail-fast for backward compatibility.
func TestAC10LegacyEnvVar1ActivatesFailFast(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "1")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true for legacy %s=1", envFailFast)
	}
}

// ── 11. E2E_FAILFAST takes priority over E2E_FAIL_FAST ──────────────────────

// TestAC10CanonicalTakesPriorityOverLegacy verifies that when both E2E_FAILFAST
// and E2E_FAIL_FAST are set, E2E_FAILFAST is checked first (canonical wins).
// This ensures "E2E_FAILFAST=1 E2E_FAIL_FAST=false" still activates fail-fast.
func TestAC10CanonicalTakesPriorityOverLegacy(t *testing.T) {
	t.Setenv(envFailFastAlt, "1")
	t.Setenv(envFailFast, "false")

	if !resolveFailFast() {
		t.Errorf("resolveFailFast() = false; want true when %s=1 takes priority over %s=false",
			envFailFastAlt, envFailFast)
	}
}

// TestAC10LegacyFalseDoesNotOverrideCanonical verifies that when E2E_FAILFAST
// is unset but E2E_FAIL_FAST=false, fail-fast is correctly disabled — i.e.,
// the legacy var's "false" value is respected.
func TestAC10LegacyFalseDoesNotOverrideCanonical(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "false")

	if resolveFailFast() {
		t.Errorf("resolveFailFast() = true; want false when both %s= and %s=false",
			envFailFastAlt, envFailFast)
	}
}

// ── 12. applyFailFast sets FailFast=true when E2E_FAILFAST=1 ─────────────────

// TestAC10ApplyFailFastSetsGinkgoConfigWhenEnabled verifies that applyFailFast
// sets suiteConfig.FailFast=true when E2E_FAILFAST=1 is set, confirming the
// Ginkgo suite will stop after the first spec failure.
func TestAC10ApplyFailFastSetsGinkgoConfigWhenEnabled(t *testing.T) {
	t.Setenv(envFailFastAlt, "1")
	t.Setenv(envFailFast, "")

	cfg := types.SuiteConfig{}
	applyFailFast(&cfg)
	if !cfg.FailFast {
		t.Errorf("applyFailFast: FailFast = false after %s=1; want true", envFailFastAlt)
	}
}

// ── 13. applyFailFast is a no-op when E2E_FAILFAST is absent ─────────────────

// TestAC10ApplyFailFastIsNoOpWhenDisabled verifies that applyFailFast does NOT
// set FailFast=true when E2E_FAILFAST is absent or empty, preserving the
// AC 10 continue-on-failure default.
func TestAC10ApplyFailFastIsNoOpWhenDisabled(t *testing.T) {
	t.Setenv(envFailFastAlt, "")
	t.Setenv(envFailFast, "")

	cfg := types.SuiteConfig{}
	applyFailFast(&cfg)
	if cfg.FailFast {
		t.Errorf("applyFailFast: FailFast = true when both env vars are absent; want false")
	}
}

// ── 14. Summary header is always emitted ────────────────────────────────────

// TestAC10SummaryHeaderAlwaysEmitted verifies that writeSuiteResultSummary
// always writes the "=== E2E Result Summary ===" header, regardless of whether
// any tests failed.
func TestAC10SummaryHeaderAlwaysEmitted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		failed    int
		succeeded bool
	}{
		{"all-pass", "PASS", 0, true},
		{"some-fail", "FAIL", 3, false},
		{"zero-specs", "PASS", 0, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := SuiteResultSummary{
				SuiteName:   "Pillar CSI E2E Suite",
				Total:       max10(tc.failed, 5),
				Passed:      max10(tc.failed, 5) - tc.failed,
				Failed:      tc.failed,
				SuiteStatus: tc.status,
			}
			if tc.failed > 0 {
				s.FailedTCs = []failedTCRecord{
					{TCID: "E1.1", Category: "in-process", Message: "test failed"},
				}
			}

			var buf bytes.Buffer
			if err := writeSuiteResultSummary(&buf, s); err != nil {
				t.Fatalf("writeSuiteResultSummary: %v", err)
			}
			if !strings.Contains(buf.String(), "=== E2E Result Summary ===") {
				t.Errorf("summary header missing in %q case:\n%s", tc.name, buf.String())
			}
		})
	}
}

// max10 returns the larger of a and b; used to build valid SuiteResultSummary values.
func max10(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ── 15. Summary shows "status: PASS" when all pass ───────────────────────────

// TestAC10SummaryPassStatusWhenNoFailures verifies that the summary emits
// "status: PASS" when no specs failed, confirming the continue-on-failure path.
func TestAC10SummaryPassStatusWhenNoFailures(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   true,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E2.1", "envtest", types.SpecStatePassed, 200*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)
	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "status: PASS") {
		t.Errorf("summary missing 'status: PASS' when all specs passed:\n%s", out)
	}
}

// ── 16. Summary shows "status: FAIL" when any spec fails ─────────────────────

// TestAC10SummaryFailStatusWhenAnySpecFails verifies that the summary emits
// "status: FAIL" when at least one spec failed, confirming the failure is
// visible in the summary even when the suite continues after failure.
func TestAC10SummaryFailStatusWhenAnySpecFails(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   false,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E1.2", "in-process", types.SpecStateFailed, 200*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)
	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "status: FAIL") {
		t.Errorf("summary missing 'status: FAIL' when spec E1.2 failed:\n%s", out)
	}
}

// ── 17. Summary lists ALL failed TCs (continue-on-failure) ───────────────────

// TestAC10SummaryListsAllFailedTCs verifies that the summary report lists ALL
// failed TCs, not just the first one. This is the core AC 10 contract: the
// continue-on-failure default enables collecting every failure in a single run.
func TestAC10SummaryListsAllFailedTCs(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   false,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E1.2", "in-process", types.SpecStateFailed, 200*time.Millisecond),
			sampleItSpec("E3.5", "envtest", types.SpecStateFailed, 300*time.Millisecond),
			sampleItSpec("F27.1", "full-lvm", types.SpecStateFailed, 1*time.Second),
		},
	}

	s := collectSuiteResultSummary(report)
	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	// All three failed TCs must appear in the summary.
	for _, id := range []string{"E1.2", "E3.5", "F27.1"} {
		if !strings.Contains(out, "[TC-"+id+"]") {
			t.Errorf("summary missing [TC-%s] in continue-on-failure report:\n%s", id, out)
		}
	}

	// The count must match the actual number of failures.
	if s.Failed != 3 {
		t.Errorf("collectSuiteResultSummary: Failed = %d, want 3", s.Failed)
	}
}

// ── 18. No "failed TCs" section when all pass ────────────────────────────────

// TestAC10SummaryNoFailedTCsSectionWhenAllPass verifies that when no TCs fail,
// the summary does NOT emit a "failed TCs" section, keeping the output clean.
func TestAC10SummaryNoFailedTCsSectionWhenAllPass(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   true,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E2.1", "envtest", types.SpecStatePassed, 200*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)
	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "failed TCs") {
		t.Errorf("all-pass summary must not contain 'failed TCs' section:\n%s", out)
	}
}

// ── 19. writeSuiteResultSummary is idempotent ────────────────────────────────

// TestAC10SummaryWriteIsIdempotent verifies that calling writeSuiteResultSummary
// twice with the same input produces identical output, confirming that the
// function is purely functional and has no internal state.
func TestAC10SummaryWriteIsIdempotent(t *testing.T) {
	s := SuiteResultSummary{
		SuiteName:   "Pillar CSI E2E Suite",
		Total:       3,
		Passed:      2,
		Failed:      1,
		SuiteStatus: "FAIL",
		FailedTCs: []failedTCRecord{
			{TCID: "E1.2", Category: "in-process", Message: "Expected true to be false"},
		},
	}

	var buf1, buf2 bytes.Buffer
	if err := writeSuiteResultSummary(&buf1, s); err != nil {
		t.Fatalf("writeSuiteResultSummary (first call): %v", err)
	}
	if err := writeSuiteResultSummary(&buf2, s); err != nil {
		t.Fatalf("writeSuiteResultSummary (second call): %v", err)
	}

	if buf1.String() != buf2.String() {
		t.Errorf("writeSuiteResultSummary is not idempotent:\nfirst:  %q\nsecond: %q",
			buf1.String(), buf2.String())
	}
}

// ── 20. envFailFastAlt constant equals "E2E_FAILFAST" ───────────────────────

// TestAC10EnvFailFastAltIsE2EFailfast verifies that the envFailFastAlt constant
// equals the canonical AC 10 env var name "E2E_FAILFAST" (no underscore between
// FAIL and FAST). This ensures the Go code and documentation are in sync.
func TestAC10EnvFailFastAltIsE2EFailfast(t *testing.T) {
	const want = "E2E_FAILFAST"
	if envFailFastAlt != want {
		t.Errorf("envFailFastAlt = %q, want %q (AC 10 canonical env var)", envFailFastAlt, want)
	}
}

// ── Integration: verify both env vars are exported in main_test comment ──────

// TestAC10EnvVarDocumentedInMainTest verifies that the canonical env var name
// is listed in the expected constant value. This is a simple sanity check that
// the constant follows the documented AC 10 interface.
func TestAC10CanonicalEnvVarNameContainsFailfast(t *testing.T) {
	if !strings.Contains(envFailFastAlt, "FAILFAST") {
		t.Errorf("canonical fail-fast env var %q should contain 'FAILFAST'", envFailFastAlt)
	}
	if strings.Contains(envFailFastAlt, "FAIL_FAST") {
		t.Errorf("canonical fail-fast env var %q must not contain underscore 'FAIL_FAST'", envFailFastAlt)
	}
}
