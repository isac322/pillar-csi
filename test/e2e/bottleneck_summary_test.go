package e2e

// bottleneck_summary_test.go — Sub-AC 3 unit tests.
//
// Acceptance criteria verified here:
//
//  1.  detectOutliers flags TCs whose pct strictly exceeds the threshold.
//  2.  detectOutliers excludes TCs at or below the threshold.
//  3.  detectOutliers returns nil when suiteRuntimeNanos ≤ 0.
//  4.  detectOutliers returns nil when tcs is empty.
//  5.  detectOutliers sorts results by PctOfSuiteRuntime descending.
//  6.  detectOutliers breaks ties by TCID ascending.
//  7.  detectOutliers skips TCs with TotalNanos ≤ 0.
//  8.  isOutlier returns true for a TCID present in the outliers slice.
//  9.  isOutlier returns false for a TCID absent from the outliers slice.
// 10.  allPhasesRankedByDuration returns phases sorted by TotalNanos descending.
// 11.  allPhasesRankedByDuration returns nil when SlowSetupPhases is empty.
// 12.  printBottleneckSummaryTable writes the expected header section.
// 13.  printBottleneckSummaryTable writes "=== E2E Bottleneck Summary ===" header.
// 14.  printBottleneckSummaryTable flags outliers with outlierFlagLabel.
// 15.  printBottleneckSummaryTable does not flag non-outlier TCs.
// 16.  printBottleneckSummaryTable is a no-op when out is nil.
// 17.  printBottleneckSummaryTable is a no-op when report.TCs is empty.
// 18.  printBottleneckSummaryTable includes TC ranking section with "RANK" header.
// 19.  printBottleneckSummaryTable includes phase ranking section when phases available.
// 20.  printBottleneckSummaryTable omits phase section when SlowSetupPhases empty.
// 21.  printBottleneckSummaryTable ranks TCs in order (rank 1 = slowest).
// 22.  printBottleneckSummaryTable shows correct outlier count in header.
// 23.  emitBottleneckSummary is a no-op when profiling is disabled.
// 24.  emitBottleneckSummary is a no-op when SummaryWriter is nil.
// 25.  emitBottleneckSummary produces non-empty output when enabled.
// 26.  configureSuiteExecution sets SummaryWriter to io.Discard when disabled.
// 27.  configureSuiteExecution sets SummaryWriter to os.Stdout when enabled.
// 28.  configureSuiteExecution sets OutlierThresholdPct from flag.
// 29.  resolveProfileOutlierThreshold uses flag value when ≥ 0.
// 30.  resolveProfileOutlierThreshold uses env var when flag is at sentinel.
// 31.  resolveProfileOutlierThreshold falls back to defaultOutlierThresholdPct.
// 32.  resolveProfileOutlierThreshold returns 0 when flag is explicitly 0.0.
// 33.  OutlierEntry.TotalDuration returns correct time.Duration.
// 34.  emitBottleneckSummary threshold configurable via OutlierThresholdPct.

import (
	"bytes"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// makeTC builds a TCProfile with the given TCID and total duration.
func makeTC(tcID string, dur time.Duration) TCProfile {
	return TCProfile{
		TCID:       tcID,
		TestName:   "Test" + tcID,
		Passed:     true,
		TotalNanos: dur.Nanoseconds(),
	}
}

// makeReport builds a ProfileReport with a specified suite runtime and the
// provided TC list already sorted descending (as buildProfileReport guarantees).
func makeReport(suiteRuntime time.Duration, tcs []TCProfile, phases []SetupPhaseBottleneck) ProfileReport {
	return ProfileReport{
		SuiteName:         "Test Suite",
		TotalSpecs:        len(tcs),
		SelectedSpecs:     len(tcs),
		SuiteRuntimeNanos: suiteRuntime.Nanoseconds(),
		TCs:               tcs,
		Bottlenecks:       nil,
		SlowSetupPhases:   phases,
	}
}

// ── 1. detectOutliers flags TCs exceeding threshold ──────────────────────────

func TestDetectOutliersFlagsExceedingThreshold(t *testing.T) {
	t.Parallel()
	suiteNanos := (10 * time.Second).Nanoseconds()
	tcs := []TCProfile{
		makeTC("E1.1", 2*time.Second),        // 20% > 10% → outlier
		makeTC("E1.2", 500*time.Millisecond), // 5% ≤ 10% → not outlier
	}
	outliers := detectOutliers(tcs, suiteNanos, 10.0)
	if len(outliers) != 1 {
		t.Fatalf("detectOutliers: got %d outliers, want 1", len(outliers))
	}
	if outliers[0].TCID != "E1.1" {
		t.Errorf("outliers[0].TCID = %q, want E1.1", outliers[0].TCID)
	}
}

// ── 2. detectOutliers excludes TCs at or below threshold ─────────────────────

func TestDetectOutliersExcludesBelowThreshold(t *testing.T) {
	t.Parallel()
	suiteNanos := (10 * time.Second).Nanoseconds()
	tcs := []TCProfile{
		makeTC("E1.1", time.Second),          // 10% — not strictly > 10% → not outlier
		makeTC("E1.2", 500*time.Millisecond), // 5% < 10% → not outlier
	}
	outliers := detectOutliers(tcs, suiteNanos, 10.0)
	if len(outliers) != 0 {
		t.Errorf("detectOutliers: got %d outliers, want 0 (threshold is strict >)", len(outliers))
	}
}

// ── 3. detectOutliers returns nil when suiteRuntimeNanos ≤ 0 ─────────────────

func TestDetectOutliersNilWhenZeroSuiteRuntime(t *testing.T) {
	t.Parallel()
	tcs := []TCProfile{makeTC("E1.1", 5*time.Second)}
	if got := detectOutliers(tcs, 0, 10.0); got != nil {
		t.Errorf("detectOutliers with zero runtime = %v, want nil", got)
	}
	if got := detectOutliers(tcs, -1, 10.0); got != nil {
		t.Errorf("detectOutliers with negative runtime = %v, want nil", got)
	}
}

// ── 4. detectOutliers returns nil when tcs is empty ──────────────────────────

func TestDetectOutliersNilWhenEmptyTCs(t *testing.T) {
	t.Parallel()
	if got := detectOutliers(nil, (10 * time.Second).Nanoseconds(), 10.0); got != nil {
		t.Errorf("detectOutliers with nil tcs = %v, want nil", got)
	}
	if got := detectOutliers([]TCProfile{}, (10 * time.Second).Nanoseconds(), 10.0); got != nil {
		t.Errorf("detectOutliers with empty tcs = %v, want nil", got)
	}
}

// ── 5. detectOutliers sorts by PctOfSuiteRuntime descending ──────────────────

func TestDetectOutliersSortedByPctDescending(t *testing.T) {
	t.Parallel()
	suiteNanos := (10 * time.Second).Nanoseconds()
	tcs := []TCProfile{
		makeTC("E1.2", 2*time.Second),         // 20%
		makeTC("E1.1", 4*time.Second),         // 40% – largest
		makeTC("E1.3", 1500*time.Millisecond), // 15%
	}
	outliers := detectOutliers(tcs, suiteNanos, 10.0)
	if len(outliers) != 3 {
		t.Fatalf("detectOutliers: got %d outliers, want 3", len(outliers))
	}
	for i := 1; i < len(outliers); i++ {
		if outliers[i].PctOfSuiteRuntime > outliers[i-1].PctOfSuiteRuntime {
			t.Errorf("outliers not sorted descending at index %d: %.2f > %.2f",
				i, outliers[i].PctOfSuiteRuntime, outliers[i-1].PctOfSuiteRuntime)
		}
	}
	if outliers[0].TCID != "E1.1" {
		t.Errorf("outliers[0].TCID = %q, want E1.1 (slowest)", outliers[0].TCID)
	}
}

// ── 6. detectOutliers breaks ties by TCID ascending ──────────────────────────

func TestDetectOutliersTiesBrokenByTCIDAscending(t *testing.T) {
	t.Parallel()
	suiteNanos := (10 * time.Second).Nanoseconds()
	// Both TCs have exactly the same duration → same pct.
	tcs := []TCProfile{
		makeTC("E1.2", 2*time.Second),
		makeTC("E1.1", 2*time.Second),
	}
	outliers := detectOutliers(tcs, suiteNanos, 10.0)
	if len(outliers) != 2 {
		t.Fatalf("detectOutliers: got %d outliers, want 2", len(outliers))
	}
	if outliers[0].TCID != "E1.1" {
		t.Errorf("tie-break: outliers[0].TCID = %q, want E1.1 (alphabetically first)", outliers[0].TCID)
	}
	if outliers[1].TCID != "E1.2" {
		t.Errorf("tie-break: outliers[1].TCID = %q, want E1.2", outliers[1].TCID)
	}
}

// ── 7. detectOutliers skips TCs with TotalNanos ≤ 0 ─────────────────────────

func TestDetectOutliersSkipsZeroNanosTCs(t *testing.T) {
	t.Parallel()
	suiteNanos := (10 * time.Second).Nanoseconds()
	tcs := []TCProfile{
		{TCID: "E1.0", TotalNanos: 0},  // zero → skip
		{TCID: "E1.1", TotalNanos: -1}, // negative → skip
		makeTC("E1.2", 2*time.Second),  // 20% → outlier
	}
	outliers := detectOutliers(tcs, suiteNanos, 10.0)
	if len(outliers) != 1 {
		t.Fatalf("detectOutliers: got %d outliers, want 1 (zero/negative TotalNanos skipped)", len(outliers))
	}
	if outliers[0].TCID != "E1.2" {
		t.Errorf("outliers[0].TCID = %q, want E1.2", outliers[0].TCID)
	}
}

// ── 8. isOutlier returns true for known TCID ─────────────────────────────────

func TestIsOutlierReturnsTrueForKnownTCID(t *testing.T) {
	t.Parallel()
	outliers := []OutlierEntry{
		{TCID: "E1.1", PctOfSuiteRuntime: 20},
		{TCID: "F2.3", PctOfSuiteRuntime: 15},
	}
	if !isOutlier("E1.1", outliers) {
		t.Error("isOutlier(E1.1) = false, want true")
	}
	if !isOutlier("F2.3", outliers) {
		t.Error("isOutlier(F2.3) = false, want true")
	}
}

// ── 9. isOutlier returns false for absent TCID ───────────────────────────────

func TestIsOutlierReturnsFalseForAbsentTCID(t *testing.T) {
	t.Parallel()
	outliers := []OutlierEntry{{TCID: "E1.1"}}
	if isOutlier("E1.2", outliers) {
		t.Error("isOutlier(E1.2) = true, want false (not in slice)")
	}
	if isOutlier("E1.1_extra", outliers) {
		t.Error("isOutlier(E1.1_extra) = true, want false (prefix match must be exact)")
	}
	if isOutlier("", nil) {
		t.Error("isOutlier empty TCID with nil slice = true, want false")
	}
}

// ── 10. allPhasesRankedByDuration sorts descending ───────────────────────────

func TestAllPhasesRankedByDurationSortedDescending(t *testing.T) {
	t.Parallel()
	report := ProfileReport{
		SlowSetupPhases: []SetupPhaseBottleneck{
			{Rank: 1, Phase: "tc_setup", TCID: "E1.2", TotalNanos: (200 * time.Millisecond).Nanoseconds()},
			{Rank: 2, Phase: "before_suite", TotalNanos: (500 * time.Millisecond).Nanoseconds()},
			{Rank: 3, Phase: "after_suite", TotalNanos: (100 * time.Millisecond).Nanoseconds()},
		},
	}
	phases := allPhasesRankedByDuration(report)
	if len(phases) != 3 {
		t.Fatalf("allPhasesRankedByDuration: got %d phases, want 3", len(phases))
	}
	for i := 1; i < len(phases); i++ {
		if phases[i].TotalNanos > phases[i-1].TotalNanos {
			t.Errorf("phases not sorted descending at index %d: %d > %d",
				i, phases[i].TotalNanos, phases[i-1].TotalNanos)
		}
	}
	if phases[0].Phase != "before_suite" {
		t.Errorf("phases[0].Phase = %q, want before_suite (slowest)", phases[0].Phase)
	}
}

// ── 11. allPhasesRankedByDuration returns nil when SlowSetupPhases empty ─────

func TestAllPhasesRankedByDurationNilWhenEmpty(t *testing.T) {
	t.Parallel()
	if got := allPhasesRankedByDuration(ProfileReport{}); got != nil {
		t.Errorf("allPhasesRankedByDuration with empty SlowSetupPhases = %v, want nil", got)
	}
}

// ── 12. printBottleneckSummaryTable writes header section ────────────────────

func TestPrintBottleneckSummaryTableWritesThresholdHeader(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	report := makeReport(10*time.Second, []TCProfile{makeTC("E1.1", 2*time.Second)}, nil)
	outliers := []OutlierEntry{{TCID: "E1.1", PctOfSuiteRuntime: 20}}

	if err := printBottleneckSummaryTable(&buf, report, outliers, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "threshold: 10.0%") {
		t.Errorf("output missing threshold header; got:\n%s", output)
	}
	if !strings.Contains(output, "outliers flagged: 1") {
		t.Errorf("output missing outlier count; got:\n%s", output)
	}
	if !strings.Contains(output, "TCs: 1") {
		t.Errorf("output missing TC count; got:\n%s", output)
	}
}

// ── 13. printBottleneckSummaryTable writes "=== E2E Bottleneck Summary ===" ──

func TestPrintBottleneckSummaryTableWritesTitle(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	report := makeReport(5*time.Second, []TCProfile{makeTC("E1.1", time.Second)}, nil)

	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	if !strings.Contains(buf.String(), "=== E2E Bottleneck Summary ===") {
		t.Errorf("output missing title line; got:\n%s", buf.String())
	}
}

// ── 14. printBottleneckSummaryTable flags outliers with outlierFlagLabel ──────

func TestPrintBottleneckSummaryTableFlagsOutliers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tcs := []TCProfile{
		makeTC("E1.1", 3*time.Second), // 30% → outlier
		makeTC("E1.2", time.Second),   // 10% → not outlier (threshold is strict >)
	}
	report := makeReport(10*time.Second, tcs, nil)
	outliers := detectOutliers(tcs, report.SuiteRuntimeNanos, 10.0)

	if err := printBottleneckSummaryTable(&buf, report, outliers, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, outlierFlagLabel) {
		t.Errorf("output missing outlier flag label %q; got:\n%s", outlierFlagLabel, output)
	}
}

// ── 15. printBottleneckSummaryTable does not flag non-outlier TCs ─────────────

func TestPrintBottleneckSummaryTableDoesNotFlagNonOutliers(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tcs := []TCProfile{
		makeTC("E1.1", time.Second),          // 10% — not strictly > 10%
		makeTC("E1.2", 500*time.Millisecond), // 5%
	}
	report := makeReport(10*time.Second, tcs, nil)
	// threshold 10.0: neither TC exceeds threshold.
	outliers := detectOutliers(tcs, report.SuiteRuntimeNanos, 10.0)

	if err := printBottleneckSummaryTable(&buf, report, outliers, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	if strings.Contains(buf.String(), outlierFlagLabel) {
		t.Errorf("output contains outlier flag label %q but no TCs exceed threshold:\n%s",
			outlierFlagLabel, buf.String())
	}
}

// ── 16. printBottleneckSummaryTable is no-op when out is nil ─────────────────

func TestPrintBottleneckSummaryTableNoOpNilWriter(t *testing.T) {
	t.Parallel()
	report := makeReport(10*time.Second, []TCProfile{makeTC("E1.1", 2*time.Second)}, nil)
	if err := printBottleneckSummaryTable(nil, report, nil, 10.0); err != nil {
		t.Errorf("printBottleneckSummaryTable(nil writer): %v", err)
	}
}

// ── 17. printBottleneckSummaryTable is no-op when report.TCs is empty ────────

func TestPrintBottleneckSummaryTableNoOpEmptyTCs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	report := makeReport(10*time.Second, nil, nil)
	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Errorf("printBottleneckSummaryTable(empty TCs): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printBottleneckSummaryTable(empty TCs) wrote %d bytes, want 0: %q",
			buf.Len(), buf.String())
	}
}

// ── 18. printBottleneckSummaryTable includes TC ranking with "RANK" header ───

func TestPrintBottleneckSummaryTableHasTCRankingSection(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	report := makeReport(10*time.Second, []TCProfile{makeTC("E1.1", 2*time.Second)}, nil)

	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "RANK") {
		t.Errorf("output missing 'RANK' header in TC table; got:\n%s", output)
	}
	if !strings.Contains(output, "TCs ranked by duration:") {
		t.Errorf("output missing TC ranking section header; got:\n%s", output)
	}
}

// ── 19. printBottleneckSummaryTable includes phase section when phases available

func TestPrintBottleneckSummaryTableIncludesPhaseSection(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phases := []SetupPhaseBottleneck{
		{Rank: 1, Phase: "before_suite", TotalNanos: (400 * time.Millisecond).Nanoseconds()},
	}
	report := makeReport(10*time.Second, []TCProfile{makeTC("E1.1", 2*time.Second)}, phases)

	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Phases ranked by duration:") {
		t.Errorf("output missing phase ranking section; got:\n%s", output)
	}
	if !strings.Contains(output, "before_suite") {
		t.Errorf("output missing before_suite phase; got:\n%s", output)
	}
}

// ── 20. printBottleneckSummaryTable omits phase section when empty ────────────

func TestPrintBottleneckSummaryTableOmitsPhaseSectionWhenEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	report := makeReport(10*time.Second, []TCProfile{makeTC("E1.1", 2*time.Second)}, nil)

	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	if strings.Contains(buf.String(), "Phases ranked by duration:") {
		t.Errorf("output contains phase section when SlowSetupPhases is empty:\n%s", buf.String())
	}
}

// ── 21. printBottleneckSummaryTable ranks TCs in order (rank 1 = slowest) ────

func TestPrintBottleneckSummaryTableRankOneIsSlowest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// TCs must already be sorted descending (as buildProfileReport guarantees).
	tcs := []TCProfile{
		makeTC("E1.1", 3*time.Second), // slowest
		makeTC("E1.2", 2*time.Second),
		makeTC("E1.3", 500*time.Millisecond), // fastest
	}
	report := makeReport(10*time.Second, tcs, nil)

	if err := printBottleneckSummaryTable(&buf, report, nil, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	lines := strings.Split(buf.String(), "\n")
	// Find the first data row (after separator line) in the TC table.
	inTable := false
	seenSep := false
	for _, line := range lines {
		if strings.Contains(line, "TCs ranked by duration:") {
			inTable = true
		}
		if inTable && strings.Contains(line, "---") {
			seenSep = true
			continue
		}
		if seenSep && strings.TrimSpace(line) != "" {
			// First data row: must start with rank 1 and contain E1.1.
			if !strings.HasPrefix(strings.TrimSpace(line), "1") {
				t.Errorf("first TC row does not start with rank 1: %q", line)
			}
			if !strings.Contains(line, "E1.1") {
				t.Errorf("first TC row (rank 1) does not contain E1.1: %q", line)
			}
			break
		}
	}
}

// ── 22. printBottleneckSummaryTable shows correct outlier count ───────────────

func TestPrintBottleneckSummaryTableShowsCorrectOutlierCount(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	suiteRuntime := 10 * time.Second
	tcs := []TCProfile{
		makeTC("E1.1", 3*time.Second),        // 30% → outlier
		makeTC("E1.2", 2*time.Second),        // 20% → outlier
		makeTC("E1.3", 500*time.Millisecond), // 5% → not
	}
	report := makeReport(suiteRuntime, tcs, nil)
	outliers := detectOutliers(tcs, report.SuiteRuntimeNanos, 10.0)

	if err := printBottleneckSummaryTable(&buf, report, outliers, 10.0); err != nil {
		t.Fatalf("printBottleneckSummaryTable: %v", err)
	}

	if !strings.Contains(buf.String(), "outliers flagged: 2") {
		t.Errorf("output missing 'outliers flagged: 2'; got:\n%s", buf.String())
	}
}

// ── 23. emitBottleneckSummary is no-op when profiling is disabled ─────────────

func TestEmitBottleneckSummaryNoOpWhenDisabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             false, // disabled
			SummaryWriter:       &buf,
			OutlierThresholdPct: defaultOutlierThresholdPct,
		},
	}
	report := types.Report{
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E1.1", "Test", 3*time.Second),
		},
	}
	if err := emitBottleneckSummary(cfg, report); err != nil {
		t.Fatalf("emitBottleneckSummary: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("emitBottleneckSummary when disabled wrote %d bytes, want 0", buf.Len())
	}
}

// ── 24. emitBottleneckSummary is no-op when SummaryWriter is nil ─────────────

func TestEmitBottleneckSummaryNoOpNilWriter(t *testing.T) {
	t.Parallel()
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             true,
			SummaryWriter:       nil, // nil writer
			OutlierThresholdPct: defaultOutlierThresholdPct,
		},
	}
	report := types.Report{
		RunTime: 10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E1.1", "Test", 3*time.Second),
		},
	}
	// Must not panic or return error.
	if err := emitBottleneckSummary(cfg, report); err != nil {
		t.Fatalf("emitBottleneckSummary with nil SummaryWriter: %v", err)
	}
}

// ── 25. emitBottleneckSummary produces non-empty output when enabled ──────────

func TestEmitBottleneckSummaryProducesOutputWhenEnabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             true,
			SummaryWriter:       &buf,
			BottleneckLimit:     profileReportBottleneckLimit,
			SlowSetupPhaseLimit: defaultSlowSetupPhaseLimit,
			OutlierThresholdPct: 10.0,
		},
	}
	report := types.Report{
		SuiteDescription: "Test Suite",
		RunTime:          10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E1.1", "TestSlow", 3*time.Second),
			sampleTimedSpecReport("E1.2", "TestFast", 500*time.Millisecond),
		},
	}
	if err := emitBottleneckSummary(cfg, report); err != nil {
		t.Fatalf("emitBottleneckSummary: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("emitBottleneckSummary produced no output when enabled")
	}
	if !strings.Contains(buf.String(), "=== E2E Bottleneck Summary ===") {
		t.Errorf("output missing title; got:\n%s", buf.String())
	}
	// E1.1 at 30% should be flagged (> 10% threshold).
	if !strings.Contains(buf.String(), outlierFlagLabel) {
		t.Errorf("output missing outlier flag for E1.1 (30%% > 10%% threshold); got:\n%s", buf.String())
	}
}

// ── 26. configureSuiteExecution sets SummaryWriter to io.Discard when disabled

func TestConfigureSuiteExecutionSummaryWriterDiscardWhenDisabled(t *testing.T) {
	savedFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eTimingReportFlag = "" // disabled
	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	// When disabled, SummaryWriter must be io.Discard (writes must not reach stdout).
	n, err := cfg.TimingReport.SummaryWriter.Write([]byte("should-be-discarded"))
	if err != nil {
		t.Fatalf("SummaryWriter.Write: %v", err)
	}
	// io.Discard always returns len(p), nil.
	if n != len("should-be-discarded") {
		t.Errorf("SummaryWriter.Write n = %d, want %d (io.Discard behaviour)", n, len("should-be-discarded"))
	}
	// The writer must be io.Discard (not os.Stdout).
	if cfg.TimingReport.SummaryWriter == os.Stdout {
		t.Error("SummaryWriter is os.Stdout when profiling is disabled, want io.Discard")
	}
}

// ── 27. configureSuiteExecution sets SummaryWriter to os.Stdout when enabled ─

func TestConfigureSuiteExecutionSummaryWriterIsStdoutWhenEnabled(t *testing.T) {
	savedFlag := *e2eTimingReportFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = savedFlag
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	*e2eTimingReportFlag = filepath.Join(dir, "profile.json")
	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.SummaryWriter == io.Discard {
		t.Error("SummaryWriter is io.Discard when profiling is enabled, want os.Stdout")
	}
	if cfg.TimingReport.SummaryWriter != os.Stdout {
		t.Errorf("SummaryWriter = %T, want *os.File (os.Stdout)", cfg.TimingReport.SummaryWriter)
	}
}

// ── 28. configureSuiteExecution sets OutlierThresholdPct from flag ────────────

func TestConfigureSuiteExecutionOutlierThresholdFromFlag(t *testing.T) {
	savedFlag := *e2eTimingReportFlag
	savedThreshold := *e2eProfileOutlierThresholdFlag
	t.Cleanup(func() {
		*e2eTimingReportFlag = savedFlag
		*e2eProfileOutlierThresholdFlag = savedThreshold
		configureSuiteExecution(nil)
	})

	dir := t.TempDir()
	*e2eTimingReportFlag = filepath.Join(dir, "profile.json")
	*e2eProfileOutlierThresholdFlag = 25.0 // explicit flag value

	configureSuiteExecution(nil)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.OutlierThresholdPct != 25.0 {
		t.Errorf("OutlierThresholdPct = %.1f, want 25.0", cfg.TimingReport.OutlierThresholdPct)
	}
}

// ── 29. resolveProfileOutlierThreshold uses flag value when ≥ 0 ──────────────

func TestResolveProfileOutlierThresholdUsesFlagWhenSet(t *testing.T) {
	saved := *e2eProfileOutlierThresholdFlag
	t.Cleanup(func() { *e2eProfileOutlierThresholdFlag = saved })

	*e2eProfileOutlierThresholdFlag = 15.5
	got := resolveProfileOutlierThreshold()
	if math.Abs(got-15.5) > 1e-9 {
		t.Errorf("resolveProfileOutlierThreshold() = %.2f, want 15.5 (from flag)", got)
	}
}

// ── 30. resolveProfileOutlierThreshold uses env var when flag is at sentinel ──

func TestResolveProfileOutlierThresholdUsesEnvVarWhenFlagAtSentinel(t *testing.T) {
	saved := *e2eProfileOutlierThresholdFlag
	t.Cleanup(func() { *e2eProfileOutlierThresholdFlag = saved })

	*e2eProfileOutlierThresholdFlag = outlierThresholdFlagSentinel // not set
	t.Setenv(envProfileOutlierThreshold, "20.0")                   // t.Setenv auto-restores

	got := resolveProfileOutlierThreshold()
	if math.Abs(got-20.0) > 1e-9 {
		t.Errorf("resolveProfileOutlierThreshold() = %.2f, want 20.0 (from env var)", got)
	}
}

// ── 31. resolveProfileOutlierThreshold falls back to defaultOutlierThresholdPct

func TestResolveProfileOutlierThresholdDefaultsTo10(t *testing.T) {
	saved := *e2eProfileOutlierThresholdFlag
	savedEnv := os.Getenv(envProfileOutlierThreshold)
	t.Cleanup(func() {
		*e2eProfileOutlierThresholdFlag = saved
		_ = os.Setenv(envProfileOutlierThreshold, savedEnv)
	})

	*e2eProfileOutlierThresholdFlag = outlierThresholdFlagSentinel // sentinel
	_ = os.Unsetenv(envProfileOutlierThreshold)                    // no env var

	got := resolveProfileOutlierThreshold()
	if math.Abs(got-defaultOutlierThresholdPct) > 1e-9 {
		t.Errorf("resolveProfileOutlierThreshold() = %.2f, want %.2f (built-in default)",
			got, defaultOutlierThresholdPct)
	}
}

// ── 32. resolveProfileOutlierThreshold returns 0 when flag is explicitly 0.0 ─

func TestResolveProfileOutlierThresholdZeroFlagMeansZeroThreshold(t *testing.T) {
	saved := *e2eProfileOutlierThresholdFlag
	t.Cleanup(func() { *e2eProfileOutlierThresholdFlag = saved })

	*e2eProfileOutlierThresholdFlag = 0.0 // explicit zero (≥ 0, so not sentinel)

	got := resolveProfileOutlierThreshold()
	if got != 0.0 {
		t.Errorf("resolveProfileOutlierThreshold() = %.2f, want 0.0 (explicit flag)", got)
	}
}

// ── 33. OutlierEntry.TotalDuration returns correct time.Duration ──────────────

func TestOutlierEntryTotalDuration(t *testing.T) {
	t.Parallel()
	e := OutlierEntry{TotalNanos: (3 * time.Second).Nanoseconds()}
	if got := e.TotalDuration(); got != 3*time.Second {
		t.Errorf("OutlierEntry.TotalDuration() = %v, want 3s", got)
	}
}

// ── 34. emitBottleneckSummary threshold configurable via OutlierThresholdPct ──

func TestEmitBottleneckSummaryUsesConfigurableThreshold(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	// With a 50% threshold, only E1.1 (60%) is an outlier; E1.2 (20%) is not.
	cfg := suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             true,
			SummaryWriter:       &buf,
			BottleneckLimit:     profileReportBottleneckLimit,
			SlowSetupPhaseLimit: defaultSlowSetupPhaseLimit,
			OutlierThresholdPct: 50.0,
		},
	}
	report := types.Report{
		SuiteDescription: "Test Suite",
		RunTime:          10 * time.Second,
		SpecReports: types.SpecReports{
			sampleTimedSpecReport("E1.1", "TestSlow", 6*time.Second), // 60% → outlier
			sampleTimedSpecReport("E1.2", "TestMid", 2*time.Second),  // 20% → not
		},
	}
	if err := emitBottleneckSummary(cfg, report); err != nil {
		t.Fatalf("emitBottleneckSummary: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "outliers flagged: 1") {
		t.Errorf("expected 1 outlier (threshold 50%%, E1.1 at 60%%); got:\n%s", output)
	}
}
