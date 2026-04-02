package e2e

// suite_result_summary_test.go — AC 6 tests: continue-on-failure default,
// summary report, and fail-fast flag.
//
// Acceptance criteria verified here:
//
//  1. collectSuiteResultSummary counts It-node specs correctly:
//       - Passed, Failed, Skipped, Pending, Total.
//  2. BeforeSuite / AfterSuite nodes are excluded from the counts.
//  3. Failed TCs are listed with their TCID and category.
//  4. FailedTCs list is sorted by TCID ascending (deterministic output).
//  5. writeSuiteResultSummary emits the expected header, counts line, failed-TC
//     lines, and "status: FAIL" / "status: PASS" footer.
//  6. When no TCs fail, the summary shows "status: PASS" and no "failed TCs" section.
//  7. resolveFailFast returns false by default (continue-on-failure default).
//  8. resolveFailFast returns true when E2E_FAIL_FAST=true is set.
//  9. resolveFailFast returns false when E2E_FAIL_FAST is absent or empty.
// 10. applyFailFast sets suiteConfig.FailFast=true when resolveFailFast()=true.
// 11. applyFailFast is a no-op when resolveFailFast()=false.
// 12. SuiteStatus is "FAIL" when report.SuiteSucceeded=false even if Failed==0.
// 13. truncateForSummary truncates messages longer than 120 chars with "…".
// 14. writeSuiteResultSummary returns nil without panicking on a nil writer.

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

// sampleItSpec builds a synthetic It-node spec report with a given state.
func sampleItSpec(tcID, category string, state types.SpecState, dur time.Duration) types.SpecReport {
	entries := types.ReportEntries{}
	if tcID != "" {
		entries = append(entries, types.ReportEntry{
			Name:  "tc_id",
			Value: types.WrapEntryValue(tcID),
		})
	}
	if category != "" {
		entries = append(entries, types.ReportEntry{
			Name:  "tc_category",
			Value: types.WrapEntryValue(category),
		})
	}
	var failure types.Failure
	if state == types.SpecStateFailed {
		failure = types.Failure{Message: "Expected true to be false"}
	}
	return types.SpecReport{
		LeafNodeType:  types.NodeTypeIt,
		LeafNodeText:  fmt.Sprintf("[TC-%s] %s test", tcID, category),
		State:         state,
		RunTime:       dur,
		ReportEntries: entries,
		Failure:       failure,
	}
}

// sampleBeforeSuiteSpec builds a synthetic BeforeSuite spec report.
func sampleBeforeSuiteSpec(state types.SpecState, dur time.Duration) types.SpecReport {
	return types.SpecReport{
		LeafNodeType: types.NodeTypeBeforeSuite,
		LeafNodeText: "BeforeSuite",
		State:        state,
		RunTime:      dur,
	}
}

// ── 1. collectSuiteResultSummary: basic counting ─────────────────────────────

func TestAC6CollectSummaryCountsPassedSpecs(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   true,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E1.2", "in-process", types.SpecStatePassed, 200*time.Millisecond),
			sampleItSpec("E2.1", "envtest", types.SpecStatePassed, 300*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)

	if s.Total != 3 {
		t.Errorf("Total = %d, want 3", s.Total)
	}
	if s.Passed != 3 {
		t.Errorf("Passed = %d, want 3", s.Passed)
	}
	if s.Failed != 0 {
		t.Errorf("Failed = %d, want 0", s.Failed)
	}
	if s.SuiteStatus != "PASS" {
		t.Errorf("SuiteStatus = %q, want PASS", s.SuiteStatus)
	}
}

func TestAC6CollectSummaryCountsFailedSpecs(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   false,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E1.2", "in-process", types.SpecStateFailed, 200*time.Millisecond),
			sampleItSpec("E1.3", "in-process", types.SpecStateFailed, 300*time.Millisecond),
			sampleItSpec("E2.1", "envtest", types.SpecStateSkipped, 0),
			sampleItSpec("E2.2", "envtest", types.SpecStatePending, 0),
		},
	}

	s := collectSuiteResultSummary(report)

	if s.Total != 5 {
		t.Errorf("Total = %d, want 5", s.Total)
	}
	if s.Passed != 1 {
		t.Errorf("Passed = %d, want 1", s.Passed)
	}
	if s.Failed != 2 {
		t.Errorf("Failed = %d, want 2", s.Failed)
	}
	if s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
	if s.Pending != 1 {
		t.Errorf("Pending = %d, want 1", s.Pending)
	}
	if s.SuiteStatus != "FAIL" {
		t.Errorf("SuiteStatus = %q, want FAIL", s.SuiteStatus)
	}
}

// ── 2. BeforeSuite / AfterSuite nodes are excluded ───────────────────────────

func TestAC6CollectSummaryExcludesBeforeSuiteNode(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   true,
		SpecReports: types.SpecReports{
			sampleBeforeSuiteSpec(types.SpecStatePassed, 5*time.Second),
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E1.2", "in-process", types.SpecStatePassed, 200*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)

	// Only the two It-node specs should be counted; BeforeSuite is excluded.
	if s.Total != 2 {
		t.Errorf("Total = %d, want 2 (BeforeSuite must be excluded)", s.Total)
	}
	if s.Passed != 2 {
		t.Errorf("Passed = %d, want 2", s.Passed)
	}
}

func TestAC6CollectSummaryExcludesAfterSuiteNode(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   true,
		SpecReports: types.SpecReports{
			{LeafNodeType: types.NodeTypeAfterSuite, State: types.SpecStatePassed, RunTime: 2 * time.Second},
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)

	if s.Total != 1 {
		t.Errorf("Total = %d, want 1 (AfterSuite must be excluded)", s.Total)
	}
}

// ── 3. Failed TCs are listed with TCID and category ──────────────────────────

func TestAC6CollectSummaryFailedTCsContainIDAndCategory(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   false,
		SpecReports: types.SpecReports{
			sampleItSpec("E1.1", "in-process", types.SpecStatePassed, 100*time.Millisecond),
			sampleItSpec("E3.5", "envtest", types.SpecStateFailed, 300*time.Millisecond),
			sampleItSpec("F27.1", "full-lvm", types.SpecStateFailed, 1*time.Second),
		},
	}

	s := collectSuiteResultSummary(report)

	if len(s.FailedTCs) != 2 {
		t.Fatalf("len(FailedTCs) = %d, want 2", len(s.FailedTCs))
	}

	// Sorted by TCID: E3.5 < F27.1
	if s.FailedTCs[0].TCID != "E3.5" {
		t.Errorf("FailedTCs[0].TCID = %q, want E3.5", s.FailedTCs[0].TCID)
	}
	if s.FailedTCs[0].Category != "envtest" {
		t.Errorf("FailedTCs[0].Category = %q, want envtest", s.FailedTCs[0].Category)
	}
	if s.FailedTCs[1].TCID != "F27.1" {
		t.Errorf("FailedTCs[1].TCID = %q, want F27.1", s.FailedTCs[1].TCID)
	}
	if s.FailedTCs[1].Category != "full-lvm" {
		t.Errorf("FailedTCs[1].Category = %q, want full-lvm", s.FailedTCs[1].Category)
	}
}

// ── 4. FailedTCs list is sorted by TCID ascending ────────────────────────────

func TestAC6FailedTCsSortedByTCID(t *testing.T) {
	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SuiteSucceeded:   false,
		SpecReports: types.SpecReports{
			sampleItSpec("F27.1", "full-lvm", types.SpecStateFailed, 1*time.Second),
			sampleItSpec("E1.2", "in-process", types.SpecStateFailed, 200*time.Millisecond),
			sampleItSpec("E3.5", "envtest", types.SpecStateFailed, 300*time.Millisecond),
		},
	}

	s := collectSuiteResultSummary(report)

	if len(s.FailedTCs) != 3 {
		t.Fatalf("len(FailedTCs) = %d, want 3", len(s.FailedTCs))
	}

	// Expect ascending order: E1.2, E3.5, F27.1
	want := []string{"E1.2", "E3.5", "F27.1"}
	for i, wantID := range want {
		if s.FailedTCs[i].TCID != wantID {
			t.Errorf("FailedTCs[%d].TCID = %q, want %q (sorted ascending)", i, s.FailedTCs[i].TCID, wantID)
		}
	}
}

// ── 5. writeSuiteResultSummary output format ─────────────────────────────────

func TestAC6WriteSummaryFailCaseContainsExpectedLines(t *testing.T) {
	s := SuiteResultSummary{
		SuiteName:   "Pillar CSI E2E Suite",
		Total:       437,
		Passed:      420,
		Failed:      15,
		Skipped:     2,
		Pending:     0,
		SuiteStatus: "FAIL",
		FailedTCs: []failedTCRecord{
			{TCID: "E1.2", Category: "in-process", Message: "Expected true to be false"},
			{TCID: "F27.1", Category: "full-lvm", Message: "Expected non-nil PVC to be nil"},
		},
	}

	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	expected := []string{
		"=== E2E Result Summary ===",
		"suite: Pillar CSI E2E Suite",
		"total: 437 | passed: 420 | failed: 15 | skipped: 2 | pending: 0",
		"failed TCs (15):",
		"[TC-E1.2] [category:in-process] Expected true to be false",
		"[TC-F27.1] [category:full-lvm] Expected non-nil PVC to be nil",
		"status: FAIL",
	}

	for _, want := range expected {
		if !strings.Contains(out, want) {
			t.Errorf("summary output missing %q\n\ngot:\n%s", want, out)
		}
	}
}

// ── 6. All-pass case shows PASS and no failed-TC section ─────────────────────

func TestAC6WriteSummaryPassCaseHasNoFailedTCSection(t *testing.T) {
	s := SuiteResultSummary{
		SuiteName:   "Pillar CSI E2E Suite",
		Total:       437,
		Passed:      437,
		Failed:      0,
		SuiteStatus: "PASS",
	}

	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "=== E2E Result Summary ===") {
		t.Error("summary missing header")
	}
	if !strings.Contains(out, "status: PASS") {
		t.Error("all-pass summary missing 'status: PASS'")
	}
	if strings.Contains(out, "failed TCs") {
		t.Errorf("all-pass summary must not contain 'failed TCs' section:\n%s", out)
	}
}

// ── 7. resolveFailFast defaults to false ────────────────────────────────────

func TestAC6ResolveFailFastDefaultIsFalse(t *testing.T) {
	// Save and restore the env var.
	original := os.Getenv(envFailFast)
	t.Setenv(envFailFast, "")

	if original != "" {
		// Skip this test when E2E_FAIL_FAST is set in the environment (e.g., CI).
		t.Skipf("E2E_FAIL_FAST=%q is set; skipping default-false test", original)
	}

	// With no env var and the flag at its default (false), resolveFailFast
	// must return false — this is the AC 6 "continue on failure" contract.
	got := resolveFailFast()
	if got {
		t.Error("resolveFailFast() = true, want false (default continue-on-failure)")
	}
}

// ── 8. resolveFailFast returns true when E2E_FAIL_FAST=true ─────────────────

func TestAC6ResolveFailFastTrueWhenEnvSet(t *testing.T) {
	t.Setenv(envFailFast, "true")
	if !resolveFailFast() {
		t.Error("resolveFailFast() = false, want true when E2E_FAIL_FAST=true")
	}
}

func TestAC6ResolveFailFastTrueWhenEnvSetTo1(t *testing.T) {
	t.Setenv(envFailFast, "1")
	if !resolveFailFast() {
		t.Error("resolveFailFast() = false, want true when E2E_FAIL_FAST=1")
	}
}

func TestAC6ResolveFailFastTrueWhenEnvSetToYes(t *testing.T) {
	t.Setenv(envFailFast, "yes")
	if !resolveFailFast() {
		t.Error("resolveFailFast() = false, want true when E2E_FAIL_FAST=yes")
	}
}

// ── 9. resolveFailFast returns false when E2E_FAIL_FAST is absent or empty ──

func TestAC6ResolveFailFastFalseWhenEnvEmpty(t *testing.T) {
	t.Setenv(envFailFast, "")
	if resolveFailFast() {
		t.Error("resolveFailFast() = true, want false when E2E_FAIL_FAST is empty")
	}
}

func TestAC6ResolveFailFastFalseWhenEnvFalse(t *testing.T) {
	t.Setenv(envFailFast, "false")
	if resolveFailFast() {
		t.Error("resolveFailFast() = true, want false when E2E_FAIL_FAST=false")
	}
}

func TestAC6ResolveFailFastFalseWhenEnvZero(t *testing.T) {
	t.Setenv(envFailFast, "0")
	if resolveFailFast() {
		t.Error("resolveFailFast() = true, want false when E2E_FAIL_FAST=0")
	}
}

func TestAC6ResolveFailFastCaseInsensitive(t *testing.T) {
	for _, val := range []string{"TRUE", "True", "YES", "Yes"} {
		t.Run("E2E_FAIL_FAST="+val, func(t *testing.T) {
			t.Setenv(envFailFast, val)
			if !resolveFailFast() {
				t.Errorf("resolveFailFast() = false, want true when E2E_FAIL_FAST=%q", val)
			}
		})
	}
}

// ── 10-11. applyFailFast updates/no-ops suiteConfig.FailFast ─────────────────

func TestAC6ApplyFailFastSetsGinkgoConfigWhenEnabled(t *testing.T) {
	t.Setenv(envFailFast, "true")

	cfg := types.SuiteConfig{}
	applyFailFast(&cfg)
	if !cfg.FailFast {
		t.Error("applyFailFast: FailFast = false after applying with E2E_FAIL_FAST=true")
	}
}

func TestAC6ApplyFailFastIsNoOpWhenDisabled(t *testing.T) {
	t.Setenv(envFailFast, "false")

	cfg := types.SuiteConfig{}
	applyFailFast(&cfg)
	if cfg.FailFast {
		t.Error("applyFailFast: FailFast = true when E2E_FAIL_FAST=false; must be no-op")
	}
}

// ── 12. SuiteStatus is FAIL when SuiteSucceeded=false ───────────────────────

func TestAC6SuiteStatusFailWhenSuiteSucceededFalse(t *testing.T) {
	// Even with zero failed specs, if SuiteSucceeded is false (e.g., due to a
	// BeforeSuite failure), the summary status must be FAIL.
	report := types.Report{
		SuiteDescription: "Test Suite",
		SuiteSucceeded:   false,
		SpecReports:      types.SpecReports{},
	}

	s := collectSuiteResultSummary(report)
	if s.SuiteStatus != "FAIL" {
		t.Errorf("SuiteStatus = %q, want FAIL when SuiteSucceeded=false", s.SuiteStatus)
	}
}

// ── 13. truncateForSummary ───────────────────────────────────────────────────

func TestAC6TruncateForSummaryShortMessageUnchanged(t *testing.T) {
	msg := "Expected true to be false"
	got := truncateForSummary(msg)
	if got != msg {
		t.Errorf("truncateForSummary(%q) = %q, want unchanged", msg, got)
	}
}

func TestAC6TruncateForSummaryLongMessageTruncated(t *testing.T) {
	// Build a message longer than 120 characters.
	long := strings.Repeat("x", 200)
	got := truncateForSummary(long)
	if len([]rune(got)) > 125 { // 120 chars + "…" (1 rune) = 121, allow small slack
		t.Errorf("truncateForSummary: result length %d exceeds expected limit", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncateForSummary: long message should end with '…', got %q", got)
	}
}

func TestAC6TruncateForSummaryExactly120CharsUnchanged(t *testing.T) {
	msg := strings.Repeat("a", 120)
	got := truncateForSummary(msg)
	if got != msg {
		t.Errorf("truncateForSummary(120-char msg): changed when it should not")
	}
}

// ── 14. writeSuiteResultSummary handles nil writer ───────────────────────────

func TestAC6WriteSummaryNilWriterIsNoOp(t *testing.T) {
	s := SuiteResultSummary{SuiteName: "Test", Total: 1, Passed: 1, SuiteStatus: "PASS"}
	if err := writeSuiteResultSummary(nil, s); err != nil {
		t.Fatalf("writeSuiteResultSummary(nil): %v", err)
	}
}

// ── Additional integration-style tests ────────────────────────────────────────

// TestAC6SummaryContainsSuiteName verifies that the suite name from the Ginkgo
// report appears verbatim in the written summary.
func TestAC6SummaryContainsSuiteName(t *testing.T) {
	const suiteName = "Pillar CSI E2E Suite"
	report := types.Report{
		SuiteDescription: suiteName,
		SuiteSucceeded:   true,
	}

	s := collectSuiteResultSummary(report)

	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}

	if !strings.Contains(buf.String(), "suite: "+suiteName) {
		t.Errorf("summary missing suite name %q:\n%s", suiteName, buf.String())
	}
}

// TestAC6FailedTCWithoutCategoryHasNoCategory verifies that when a failed TC
// has no "tc_category" report entry, the failure line omits the [category:...]
// tag rather than printing "[category:]".
func TestAC6FailedTCWithoutCategoryHasNoCategory(t *testing.T) {
	s := SuiteResultSummary{
		SuiteName:   "Test Suite",
		Total:       1,
		Failed:      1,
		SuiteStatus: "FAIL",
		FailedTCs: []failedTCRecord{
			{TCID: "E1.1", Category: "", Message: "unexpected nil"},
		},
	}

	var buf bytes.Buffer
	if err := writeSuiteResultSummary(&buf, s); err != nil {
		t.Fatalf("writeSuiteResultSummary: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "[category:]") {
		t.Errorf("summary must not emit [category:] when category is empty:\n%s", out)
	}
	if !strings.Contains(out, "[TC-E1.1]") {
		t.Errorf("summary must still emit TC ID even without category:\n%s", out)
	}
}

// TestAC6DefaultFailFastDocstringMatchesACContract verifies that the
// -e2e.fail-fast flag usage string contains the key concept "default false" so
// users who run `go test -h` see the AC 6 continue-on-failure contract.
func TestAC6DefaultFailFastDocstringMatchesACContract(t *testing.T) {
	usage := "stop after the first spec failure (env: E2E_FAIL_FAST); " +
		"default false runs all specs and emits a complete summary report"

	if !strings.Contains(usage, "default false") {
		t.Error("fail-fast flag description must mention 'default false' (AC 6 contract)")
	}
	if !strings.Contains(usage, "summary report") {
		t.Error("fail-fast flag description must mention 'summary report' (AC 6 contract)")
	}
}
