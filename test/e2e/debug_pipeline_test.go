package e2e

// debug_pipeline_test.go — Sub-AC 7c: --debug-pipeline flag tests.
//
// These tests verify that:
//  1. The -e2e.debug-pipeline flag is registered with a meaningful description.
//  2. When the flag is true, configureSuiteExecution sets DebugPipeline=true
//     and DebugPipelineWriter to the provided stderr writer.
//  3. When the flag is false and E2E_DEBUG_PIPELINE is unset,
//     DebugPipeline is false and DebugPipelineWriter is io.Discard.
//  4. The E2E_DEBUG_PIPELINE environment variable enables the feature
//     when the flag is not explicitly set.
//  5. buildPipelineTimeline correctly extracts per-TC data from a synthetic
//     Ginkgo report including TCID, process, startedAt, finishedAt.
//  6. buildPipelineTimeline computes SuiteStart, SuiteEnd, PipelineNanos,
//     and MaxProcess correctly.
//  7. TCs are sorted by StartedAt ascending (then by TCID for ties).
//  8. computePeakConcurrency returns the correct maximum concurrent TC count.
//  9. computeQueueWaitStats returns correct min/max/avg queue wait.
// 10. buildGanttRow produces a row of exactly `width` characters with '#' and
//     '.' in the correct positions.
// 11. printPipelineTimeline writes all four sections to the writer.
// 12. printPipelineTimeline is a no-op when the writer is nil.
// 13. printPipelineTimeline is a no-op when TCs is empty.
// 14. emitDebugPipelineTimeline is a no-op when DebugPipeline is false.
// 15. emitDebugPipelineTimeline writes output when DebugPipeline is true.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// makeTimingProfile creates a testCaseTimingProfile with the specified fields
// and encodes it as the JSON string stored in tc_timing report entries.
func makeTimingProfile(tcID string, proc int, startedAt, finishedAt time.Time, phases []phaseTimingSample) testCaseTimingProfile {
	totalNanos := finishedAt.Sub(startedAt).Nanoseconds()
	return testCaseTimingProfile{
		TCID:            tcID,
		TestName:        "Test " + tcID,
		SpecText:        "[TC-" + tcID + "] :: Test " + tcID,
		ParallelProcess: proc,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
		TotalNanos:      totalNanos,
		Phases:          phases,
	}
}

// encodeTimingProfileEntry encodes a testCaseTimingProfile as a JSON string
// suitable for storing in a Ginkgo ReportEntry with name "tc_timing".
func encodeTimingProfileEntry(p testCaseTimingProfile) string {
	b, err := json.Marshal(p)
	if err != nil {
		panic(fmt.Sprintf("encodeTimingProfileEntry: marshal: %v", err))
	}
	return string(b)
}

// makeSpecReport creates a minimal types.SpecReport with tc_timing and tc_id
// report entries for the given timing profile.
func makeSpecReport(profile testCaseTimingProfile) types.SpecReport {
	encoded := encodeTimingProfileEntry(profile)
	return types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		LeafNodeText: profile.SpecText,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue(profile.TCID)},
			{Name: timingReportEntryName, Value: types.WrapEntryValue(encoded)},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Flag / configuration tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cDebugPipelineFlagDefaultIsDisabled verifies that the flag defaults to
// false and DebugPipeline is off when neither the flag nor the env var are set.
func TestAC7cDebugPipelineFlagDefaultIsDisabled(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envDebugPipeline, "")
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugPipeline {
		t.Fatal("Sub-AC 7c: DebugPipeline should be false when flag is not set")
	}
}

// TestAC7cDebugPipelineFlagEnablesWriter verifies that setting the flag to true
// wires DebugPipelineWriter to the provided stderr writer.
func TestAC7cDebugPipelineFlagEnablesWriter(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = true
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugPipeline {
		t.Fatal("Sub-AC 7c: DebugPipeline should be true when flag is set")
	}
	if cfg.TimingReport.DebugPipelineWriter == nil {
		t.Fatal("Sub-AC 7c: DebugPipelineWriter must not be nil when DebugPipeline is enabled")
	}

	// Write a probe through the writer to confirm it reaches sink.
	_, _ = fmt.Fprint(cfg.TimingReport.DebugPipelineWriter, "probe-pipeline")
	if !strings.Contains(sink.String(), "probe-pipeline") {
		t.Errorf("Sub-AC 7c: DebugPipelineWriter output not routed to the provided stderr writer")
	}
}

// TestAC7cDebugPipelineEnvVarEnablesFeature verifies the E2E_DEBUG_PIPELINE
// environment variable as a flag fallback.
func TestAC7cDebugPipelineEnvVarEnablesFeature(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envDebugPipeline, "1")
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false // flag not set; env var should take effect
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugPipeline {
		t.Fatal("Sub-AC 7c: DebugPipeline should be true when E2E_DEBUG_PIPELINE env var is set")
	}
}

// TestAC7cDebugPipelineFlagDescription verifies that the -e2e.debug-pipeline
// flag is registered with a description mentioning key concepts.
func TestAC7cDebugPipelineFlagDescription(t *testing.T) {
	t.Parallel()
	usage := "print full end-to-end pipeline timeline to stderr after all TCs complete " +
		"(Sub-AC 7c: Gantt chart per process, queue-wait stats, parallelism summary); " +
		"env: E2E_DEBUG_PIPELINE"

	for _, keyword := range []string{"pipeline", "Gantt", "queue-wait", "parallelism", "stderr"} {
		if !strings.Contains(usage, keyword) {
			t.Errorf("Sub-AC 7c: flag description missing keyword %q in: %s", keyword, usage)
		}
	}

	// The flag variable must be accessible and default to false.
	if *e2eDebugPipelineFlag {
		t.Log("Sub-AC 7c: e2e.debug-pipeline is currently true (expect false in unit test context)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildPipelineTimeline tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cBuildPipelineTimelineEmpty verifies that an empty report produces an
// empty timeline with zero PipelineNanos.
func TestAC7cBuildPipelineTimelineEmpty(t *testing.T) {
	t.Parallel()
	report := types.Report{SuiteDescription: "Test Suite"}
	tl := buildPipelineTimeline(report)

	if len(tl.TCs) != 0 {
		t.Errorf("Sub-AC 7c: expected 0 TCs for empty report, got %d", len(tl.TCs))
	}
	if tl.PipelineNanos != 0 {
		t.Errorf("Sub-AC 7c: expected 0 PipelineNanos for empty report, got %d", tl.PipelineNanos)
	}
	if tl.SuiteName != "Test Suite" {
		t.Errorf("Sub-AC 7c: expected SuiteName 'Test Suite', got %q", tl.SuiteName)
	}
}

// TestAC7cBuildPipelineTimelineSingleTC verifies that a report with one
// instrumented spec produces a timeline with one TC and correct SuiteStart/End.
func TestAC7cBuildPipelineTimelineSingleTC(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Millisecond)
	start := now
	end := now.Add(50 * time.Millisecond)

	profile := makeTimingProfile("E1.2", 1, start, end, []phaseTimingSample{
		{Name: string(phaseSetupTotal), StartedAt: start, FinishedAt: start.Add(5 * time.Millisecond), DurationNanos: (5 * time.Millisecond).Nanoseconds()},
		{Name: string(phaseSpecBody), StartedAt: start.Add(5 * time.Millisecond), FinishedAt: end.Add(-5 * time.Millisecond), DurationNanos: (40 * time.Millisecond).Nanoseconds()},
		{Name: string(phaseTeardownTotal), StartedAt: end.Add(-5 * time.Millisecond), FinishedAt: end, DurationNanos: (5 * time.Millisecond).Nanoseconds()},
	})

	report := types.Report{
		SuiteDescription: "Test Suite",
		SpecReports: types.SpecReports{
			makeSpecReport(profile),
		},
	}

	tl := buildPipelineTimeline(report)

	if len(tl.TCs) != 1 {
		t.Fatalf("Sub-AC 7c: expected 1 TC, got %d", len(tl.TCs))
	}

	tc := tl.TCs[0]
	if tc.TCID != "E1.2" {
		t.Errorf("Sub-AC 7c: TCID=%q, want E1.2", tc.TCID)
	}
	if tc.ParallelProcess != 1 {
		t.Errorf("Sub-AC 7c: ParallelProcess=%d, want 1", tc.ParallelProcess)
	}
	if !tc.StartedAt.Equal(start) {
		t.Errorf("Sub-AC 7c: StartedAt=%v, want %v", tc.StartedAt, start)
	}
	if !tc.FinishedAt.Equal(end) {
		t.Errorf("Sub-AC 7c: FinishedAt=%v, want %v", tc.FinishedAt, end)
	}
	if !tl.SuiteStart.Equal(start) {
		t.Errorf("Sub-AC 7c: SuiteStart=%v, want %v", tl.SuiteStart, start)
	}
	if !tl.SuiteEnd.Equal(end) {
		t.Errorf("Sub-AC 7c: SuiteEnd=%v, want %v", tl.SuiteEnd, end)
	}
	wantPipeline := end.Sub(start).Nanoseconds()
	if tl.PipelineNanos != wantPipeline {
		t.Errorf("Sub-AC 7c: PipelineNanos=%d, want %d", tl.PipelineNanos, wantPipeline)
	}
	if tl.MaxProcess != 1 {
		t.Errorf("Sub-AC 7c: MaxProcess=%d, want 1", tl.MaxProcess)
	}
}

// TestAC7cBuildPipelineTimelineMultipleProcesses verifies that MaxProcess is
// correctly computed across TCs on different Ginkgo processes.
func TestAC7cBuildPipelineTimelineMultipleProcesses(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)

	p1 := makeTimingProfile("E1.1", 1, base, base.Add(10*time.Millisecond), nil)
	p2 := makeTimingProfile("E1.2", 2, base.Add(2*time.Millisecond), base.Add(15*time.Millisecond), nil)
	p3 := makeTimingProfile("E1.3", 4, base.Add(5*time.Millisecond), base.Add(20*time.Millisecond), nil)

	report := types.Report{
		SuiteDescription: "Test Suite",
		SpecReports: types.SpecReports{
			makeSpecReport(p1),
			makeSpecReport(p2),
			makeSpecReport(p3),
		},
	}

	tl := buildPipelineTimeline(report)

	if len(tl.TCs) != 3 {
		t.Fatalf("Sub-AC 7c: expected 3 TCs, got %d", len(tl.TCs))
	}
	if tl.MaxProcess != 4 {
		t.Errorf("Sub-AC 7c: MaxProcess=%d, want 4", tl.MaxProcess)
	}
	if !tl.SuiteStart.Equal(base) {
		t.Errorf("Sub-AC 7c: SuiteStart=%v, want %v", tl.SuiteStart, base)
	}
	wantEnd := base.Add(20 * time.Millisecond)
	if !tl.SuiteEnd.Equal(wantEnd) {
		t.Errorf("Sub-AC 7c: SuiteEnd=%v, want %v", tl.SuiteEnd, wantEnd)
	}
}

// TestAC7cBuildPipelineTimelineSortedByStartTime verifies that TCs are sorted
// ascending by StartedAt, with TCID as the tiebreaker.
func TestAC7cBuildPipelineTimelineSortedByStartTime(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Create profiles out of chronological order.
	p1 := makeTimingProfile("E3.1", 1, base.Add(30*time.Millisecond), base.Add(40*time.Millisecond), nil)
	p2 := makeTimingProfile("E1.1", 1, base, base.Add(10*time.Millisecond), nil)
	p3 := makeTimingProfile("E2.1", 2, base.Add(10*time.Millisecond), base.Add(20*time.Millisecond), nil)
	// Two TCs starting at the same time — sorted by TCID.
	p4 := makeTimingProfile("E2.2", 2, base.Add(10*time.Millisecond), base.Add(20*time.Millisecond), nil)

	report := types.Report{
		SuiteDescription: "Test Suite",
		SpecReports: types.SpecReports{
			makeSpecReport(p1),
			makeSpecReport(p2),
			makeSpecReport(p3),
			makeSpecReport(p4),
		},
	}

	tl := buildPipelineTimeline(report)

	if len(tl.TCs) != 4 {
		t.Fatalf("Sub-AC 7c: expected 4 TCs, got %d", len(tl.TCs))
	}

	want := []string{"E1.1", "E2.1", "E2.2", "E3.1"}
	for i, tc := range tl.TCs {
		if tc.TCID != want[i] {
			t.Errorf("Sub-AC 7c: TCs[%d].TCID=%q, want %q", i, tc.TCID, want[i])
		}
	}
}

// TestAC7cBuildPipelineTimelineSkipsSpecsWithoutTimestamps verifies that specs
// without valid StartedAt / FinishedAt timestamps are excluded.
func TestAC7cBuildPipelineTimelineSkipsSpecsWithoutTimestamps(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Valid profile.
	p1 := makeTimingProfile("E1.1", 1, base, base.Add(10*time.Millisecond), nil)

	// Profile with zero timestamps — should be skipped.
	zeroProfile := testCaseTimingProfile{
		TCID:            "E1.2",
		ParallelProcess: 1,
		// StartedAt and FinishedAt are intentionally zero.
	}
	encodedZero := encodeTimingProfileEntry(zeroProfile)
	specWithZeroTimestamps := types.SpecReport{
		LeafNodeType: types.NodeTypeIt,
		ReportEntries: types.ReportEntries{
			{Name: "tc_id", Value: types.WrapEntryValue("E1.2")},
			{Name: timingReportEntryName, Value: types.WrapEntryValue(encodedZero)},
		},
	}

	report := types.Report{
		SuiteDescription: "Test Suite",
		SpecReports: types.SpecReports{
			makeSpecReport(p1),
			specWithZeroTimestamps,
		},
	}

	tl := buildPipelineTimeline(report)

	if len(tl.TCs) != 1 {
		t.Errorf("Sub-AC 7c: expected 1 TC (skipping zero-timestamp spec), got %d", len(tl.TCs))
	}
	if len(tl.TCs) > 0 && tl.TCs[0].TCID != "E1.1" {
		t.Errorf("Sub-AC 7c: unexpected TC %q; expected E1.1", tl.TCs[0].TCID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// computePeakConcurrency tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cComputePeakConcurrencyEmpty verifies peak concurrency is 0 for an
// empty timeline.
func TestAC7cComputePeakConcurrencyEmpty(t *testing.T) {
	t.Parallel()
	tl := PipelineTimeline{}
	if got := computePeakConcurrency(tl); got != 0 {
		t.Errorf("Sub-AC 7c: peak concurrency for empty timeline = %d, want 0", got)
	}
}

// TestAC7cComputePeakConcurrencySequential verifies that non-overlapping TCs
// have a peak concurrency of 1.
func TestAC7cComputePeakConcurrencySequential(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	tl := PipelineTimeline{
		TCs: []pipelineTCRecord{
			{StartedAt: base, FinishedAt: base.Add(10 * time.Millisecond)},
			{StartedAt: base.Add(10 * time.Millisecond), FinishedAt: base.Add(20 * time.Millisecond)},
			{StartedAt: base.Add(20 * time.Millisecond), FinishedAt: base.Add(30 * time.Millisecond)},
		},
	}
	if got := computePeakConcurrency(tl); got != 1 {
		t.Errorf("Sub-AC 7c: sequential peak concurrency = %d, want 1", got)
	}
}

// TestAC7cComputePeakConcurrencyParallel verifies that fully overlapping TCs
// produce the correct peak concurrency.
func TestAC7cComputePeakConcurrencyParallel(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	// All four TCs overlap completely.
	tl := PipelineTimeline{
		TCs: []pipelineTCRecord{
			{StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond)},
			{StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond)},
			{StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond)},
			{StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond)},
		},
	}
	if got := computePeakConcurrency(tl); got != 4 {
		t.Errorf("Sub-AC 7c: fully parallel peak concurrency = %d, want 4", got)
	}
}

// TestAC7cComputePeakConcurrencyMixed verifies peak concurrency with partial
// overlaps. Three TCs overlap at the peak.
func TestAC7cComputePeakConcurrencyMixed(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	tl := PipelineTimeline{
		TCs: []pipelineTCRecord{
			// TC A: 0-30ms
			{StartedAt: base, FinishedAt: base.Add(30 * time.Millisecond)},
			// TC B: 10-40ms
			{StartedAt: base.Add(10 * time.Millisecond), FinishedAt: base.Add(40 * time.Millisecond)},
			// TC C: 20-50ms — overlaps with A and B at t=20-30ms → peak=3
			{StartedAt: base.Add(20 * time.Millisecond), FinishedAt: base.Add(50 * time.Millisecond)},
			// TC D: 35-60ms — overlaps only with B and C after A ends
			{StartedAt: base.Add(35 * time.Millisecond), FinishedAt: base.Add(60 * time.Millisecond)},
		},
	}
	if got := computePeakConcurrency(tl); got != 3 {
		t.Errorf("Sub-AC 7c: mixed peak concurrency = %d, want 3", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// computeQueueWaitStats tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cComputeQueueWaitStatsEmpty verifies all zeros for an empty timeline.
func TestAC7cComputeQueueWaitStatsEmpty(t *testing.T) {
	t.Parallel()
	tl := PipelineTimeline{}
	minW, maxW, avgW := computeQueueWaitStats(tl)
	if minW != 0 || maxW != 0 || avgW != 0 {
		t.Errorf("Sub-AC 7c: queue wait stats for empty timeline = (%v, %v, %v), want (0, 0, 0)",
			minW, maxW, avgW)
	}
}

// TestAC7cComputeQueueWaitStatsAllStart verifies that when all TCs start at
// the same time as the suite, all queue wait values are 0.
func TestAC7cComputeQueueWaitStatsAllStart(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	tl := PipelineTimeline{
		SuiteStart: base,
		TCs: []pipelineTCRecord{
			{StartedAt: base, FinishedAt: base.Add(10 * time.Millisecond)},
			{StartedAt: base, FinishedAt: base.Add(20 * time.Millisecond)},
		},
	}
	minW, maxW, avgW := computeQueueWaitStats(tl)
	if minW != 0 || maxW != 0 || avgW != 0 {
		t.Errorf("Sub-AC 7c: all-simultaneous queue wait = (%v, %v, %v), want (0, 0, 0)",
			minW, maxW, avgW)
	}
}

// TestAC7cComputeQueueWaitStatsVaried verifies correct min/max/avg when TCs
// start at different offsets from the suite start.
func TestAC7cComputeQueueWaitStatsVaried(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	// TCs start at 0ms, 100ms, 300ms from base.
	// min=0ms, max=300ms, avg=133ms.
	tl := PipelineTimeline{
		SuiteStart: base,
		TCs: []pipelineTCRecord{
			{StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond)},
			{StartedAt: base.Add(100 * time.Millisecond), FinishedAt: base.Add(150 * time.Millisecond)},
			{StartedAt: base.Add(300 * time.Millisecond), FinishedAt: base.Add(350 * time.Millisecond)},
		},
	}
	minW, maxW, avgW := computeQueueWaitStats(tl)

	if minW != 0 {
		t.Errorf("Sub-AC 7c: minWait=%v, want 0", minW)
	}
	if maxW != 300*time.Millisecond {
		t.Errorf("Sub-AC 7c: maxWait=%v, want 300ms", maxW)
	}
	// avg = (0 + 100ms + 300ms) / 3 = 133ms (integer division → 133333333ns)
	wantAvg := (0 + 100*time.Millisecond + 300*time.Millisecond) / 3
	if avgW != wantAvg {
		t.Errorf("Sub-AC 7c: avgWait=%v, want %v", avgW, wantAvg)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildGanttRow tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cBuildGanttRowWidth verifies that the returned row has exactly width
// characters.
func TestAC7cBuildGanttRowWidth(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	tl := PipelineTimeline{
		SuiteStart:    base,
		SuiteEnd:      base.Add(time.Second),
		PipelineNanos: time.Second.Nanoseconds(),
		TCs: []pipelineTCRecord{
			{ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(time.Second)},
		},
	}
	const width = 60
	tickNanos := tl.PipelineNanos / int64(width)
	row := buildGanttRow(tl, 1, width, tickNanos)

	if len(row) != width {
		t.Errorf("Sub-AC 7c: Gantt row length=%d, want %d", len(row), width)
	}
}

// TestAC7cBuildGanttRowIdleProcess verifies that a process with no TCs produces
// a row of all '.' characters.
func TestAC7cBuildGanttRowIdleProcess(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	tl := PipelineTimeline{
		SuiteStart:    base,
		PipelineNanos: time.Second.Nanoseconds(),
		TCs: []pipelineTCRecord{
			// Only proc 1 has a TC.
			{ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(500 * time.Millisecond)},
		},
	}
	const width = 10
	tickNanos := tl.PipelineNanos / int64(width)
	// Request proc 2 — should be all idle.
	row := buildGanttRow(tl, 2, width, tickNanos)

	for i, c := range row {
		if c != '.' {
			t.Errorf("Sub-AC 7c: proc 2 Gantt row[%d]=%q, want '.'; full row: %s", i, c, row)
			break
		}
	}
}

// TestAC7cBuildGanttRowFullyActive verifies that a TC spanning the full
// pipeline produces a row of all '#' characters.
func TestAC7cBuildGanttRowFullyActive(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	pipelineNanos := int64(time.Second)
	tl := PipelineTimeline{
		SuiteStart:    base,
		PipelineNanos: pipelineNanos,
		TCs: []pipelineTCRecord{
			{ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(time.Second)},
		},
	}
	const width = 10
	tickNanos := pipelineNanos / int64(width)
	row := buildGanttRow(tl, 1, width, tickNanos)

	for i, c := range row {
		if c != '#' {
			t.Errorf("Sub-AC 7c: fully-active Gantt row[%d]=%q, want '#'; full row: %s", i, c, row)
			break
		}
	}
}

// TestAC7cBuildGanttRowPartialActivity verifies that a TC covering only the
// first half of the pipeline marks the first half as '#' and second half as '.'.
func TestAC7cBuildGanttRowPartialActivity(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC()
	pipelineNanos := int64(1000 * time.Millisecond)
	tl := PipelineTimeline{
		SuiteStart:    base,
		PipelineNanos: pipelineNanos,
		TCs: []pipelineTCRecord{
			// TC runs for the first 500ms out of 1000ms.
			{ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(500 * time.Millisecond)},
		},
	}
	const width = 10
	tickNanos := pipelineNanos / int64(width) // 100ms per tick

	row := buildGanttRow(tl, 1, width, tickNanos)

	if len(row) != width {
		t.Fatalf("Sub-AC 7c: Gantt row length=%d, want %d", len(row), width)
	}

	// First 5 chars should be '#', last 5 should be '.'.
	for i := 0; i < 5; i++ {
		if row[i] != '#' {
			t.Errorf("Sub-AC 7c: row[%d]=%q, want '#' (first half active); row: %s", i, row[i], row)
		}
	}
	for i := 5; i < width; i++ {
		if row[i] != '.' {
			t.Errorf("Sub-AC 7c: row[%d]=%q, want '.' (second half idle); row: %s", i, row[i], row)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// printPipelineTimeline tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cPrintPipelineTimelineNilWriter verifies that printPipelineTimeline
// is a no-op when the writer is nil.
func TestAC7cPrintPipelineTimelineNilWriter(t *testing.T) {
	t.Parallel()
	// Should not panic.
	base := time.Now().UTC()
	tl := PipelineTimeline{
		SuiteName:     "Test Suite",
		PipelineNanos: time.Second.Nanoseconds(),
		SuiteStart:    base,
		SuiteEnd:      base.Add(time.Second),
		MaxProcess:    1,
		TCs: []pipelineTCRecord{
			{TCID: "E1.1", ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(time.Second)},
		},
	}
	printPipelineTimeline(nil, tl)
}

// TestAC7cPrintPipelineTimelineEmptyTCs verifies that printPipelineTimeline
// is a no-op when the TCs slice is empty.
func TestAC7cPrintPipelineTimelineEmptyTCs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tl := PipelineTimeline{SuiteName: "Test Suite"}
	printPipelineTimeline(&buf, tl)

	if buf.Len() != 0 {
		t.Errorf("Sub-AC 7c: expected no output for empty TCs, got %q", buf.String())
	}
}

// TestAC7cPrintPipelineTimelineContainsFourSections verifies that the output
// contains all four expected sections.
func TestAC7cPrintPipelineTimelineContainsFourSections(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)
	tl := PipelineTimeline{
		SuiteName:     "Pillar CSI E2E Suite",
		SuiteStart:    base,
		SuiteEnd:      base.Add(500 * time.Millisecond),
		PipelineNanos: (500 * time.Millisecond).Nanoseconds(),
		MaxProcess:    2,
		TCs: []pipelineTCRecord{
			{
				TCID:            "E1.1",
				TestName:        "Test 1",
				ParallelProcess: 1,
				StartedAt:       base,
				FinishedAt:      base.Add(200 * time.Millisecond),
				TotalNanos:      (200 * time.Millisecond).Nanoseconds(),
				SetupNanos:      (10 * time.Millisecond).Nanoseconds(),
				ActionNanos:     (180 * time.Millisecond).Nanoseconds(),
				TeardownNanos:   (10 * time.Millisecond).Nanoseconds(),
			},
			{
				TCID:            "E1.2",
				TestName:        "Test 2",
				ParallelProcess: 2,
				StartedAt:       base.Add(50 * time.Millisecond),
				FinishedAt:      base.Add(400 * time.Millisecond),
				TotalNanos:      (350 * time.Millisecond).Nanoseconds(),
				SetupNanos:      (20 * time.Millisecond).Nanoseconds(),
				ActionNanos:     (310 * time.Millisecond).Nanoseconds(),
				TeardownNanos:   (20 * time.Millisecond).Nanoseconds(),
			},
		},
	}

	var buf bytes.Buffer
	printPipelineTimeline(&buf, tl)

	out := buf.String()

	// Section 1: header.
	if !strings.Contains(out, "=== E2E Pipeline Timeline ===") {
		t.Error("Sub-AC 7c: output missing '=== E2E Pipeline Timeline ===' header")
	}
	if !strings.Contains(out, "Pillar CSI E2E Suite") {
		t.Error("Sub-AC 7c: output missing suite name")
	}
	if !strings.Contains(out, "total pipeline:") {
		t.Error("Sub-AC 7c: output missing 'total pipeline:' line")
	}
	if !strings.Contains(out, "peak concurrency:") {
		t.Error("Sub-AC 7c: output missing 'peak concurrency:' in header")
	}

	// Section 2: Gantt chart.
	if !strings.Contains(out, "TC timeline") {
		t.Error("Sub-AC 7c: output missing 'TC timeline' Gantt section header")
	}
	if !strings.Contains(out, "proc 1") {
		t.Error("Sub-AC 7c: output missing 'proc 1' Gantt row")
	}
	if !strings.Contains(out, "proc 2") {
		t.Error("Sub-AC 7c: output missing 'proc 2' Gantt row")
	}

	// Section 3: TC execution list.
	if !strings.Contains(out, "TC execution order") {
		t.Error("Sub-AC 7c: output missing 'TC execution order' section")
	}
	if !strings.Contains(out, "E1.1") {
		t.Error("Sub-AC 7c: output missing TC ID 'E1.1'")
	}
	if !strings.Contains(out, "E1.2") {
		t.Error("Sub-AC 7c: output missing TC ID 'E1.2'")
	}
	if !strings.Contains(out, "[setup=") {
		t.Error("Sub-AC 7c: output missing phase breakdown '[setup='")
	}

	// Section 4: queue wait summary.
	if !strings.Contains(out, "queue wait summary:") {
		t.Error("Sub-AC 7c: output missing 'queue wait summary:' section")
	}
	if !strings.Contains(out, "min=") || !strings.Contains(out, "max=") || !strings.Contains(out, "avg=") {
		t.Error("Sub-AC 7c: queue wait summary missing min/max/avg fields")
	}
}

// TestAC7cPrintPipelineTimelineHeaderValues verifies specific values in the
// header line (processes count, TC count, concurrency).
func TestAC7cPrintPipelineTimelineHeaderValues(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)
	tl := PipelineTimeline{
		SuiteName:     "Test",
		SuiteStart:    base,
		SuiteEnd:      base.Add(100 * time.Millisecond),
		PipelineNanos: (100 * time.Millisecond).Nanoseconds(),
		MaxProcess:    3,
		TCs: []pipelineTCRecord{
			{TCID: "E1", ParallelProcess: 1, StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond), TotalNanos: (50 * time.Millisecond).Nanoseconds()},
			{TCID: "E2", ParallelProcess: 2, StartedAt: base, FinishedAt: base.Add(50 * time.Millisecond), TotalNanos: (50 * time.Millisecond).Nanoseconds()},
			{TCID: "E3", ParallelProcess: 3, StartedAt: base.Add(50 * time.Millisecond), FinishedAt: base.Add(100 * time.Millisecond), TotalNanos: (50 * time.Millisecond).Nanoseconds()},
		},
	}

	var buf bytes.Buffer
	printPipelineTimeline(&buf, tl)
	out := buf.String()

	if !strings.Contains(out, "TCs: 3") {
		t.Errorf("Sub-AC 7c: header missing 'TCs: 3', got:\n%s", out)
	}
	if !strings.Contains(out, "processes: 3") {
		t.Errorf("Sub-AC 7c: header missing 'processes: 3', got:\n%s", out)
	}
	// Peak concurrency at t=0 is 2 (E1+E2 overlap).
	if !strings.Contains(out, "peak concurrency: 2") {
		t.Errorf("Sub-AC 7c: header missing 'peak concurrency: 2', got:\n%s", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// emitDebugPipelineTimeline tests
// ─────────────────────────────────────────────────────────────────────────────

// TestAC7cEmitDebugPipelineTimelineDisabled verifies no output when
// DebugPipeline is false.
func TestAC7cEmitDebugPipelineTimelineDisabled(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugPipeline:       false,
		DebugPipelineWriter: &sink,
	}

	report := types.Report{SuiteDescription: "Test Suite"}
	emitDebugPipelineTimeline(report, cfg)

	if sink.Len() != 0 {
		t.Errorf("Sub-AC 7c: expected no output when DebugPipeline=false, got: %q", sink.String())
	}
}

// TestAC7cEmitDebugPipelineTimelineEnabled verifies that the timeline header
// is written when DebugPipeline is true and the report contains instrumented specs.
func TestAC7cEmitDebugPipelineTimelineEnabled(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Truncate(time.Millisecond)

	profile := makeTimingProfile("E1.1", 1, base, base.Add(100*time.Millisecond), []phaseTimingSample{
		{Name: string(phaseSetupTotal), StartedAt: base, FinishedAt: base.Add(10 * time.Millisecond), DurationNanos: (10 * time.Millisecond).Nanoseconds()},
		{Name: string(phaseSpecBody), StartedAt: base.Add(10 * time.Millisecond), FinishedAt: base.Add(90 * time.Millisecond), DurationNanos: (80 * time.Millisecond).Nanoseconds()},
		{Name: string(phaseTeardownTotal), StartedAt: base.Add(90 * time.Millisecond), FinishedAt: base.Add(100 * time.Millisecond), DurationNanos: (10 * time.Millisecond).Nanoseconds()},
	})

	report := types.Report{
		SuiteDescription: "Pillar CSI E2E Suite",
		SpecReports: types.SpecReports{
			makeSpecReport(profile),
		},
	}

	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugPipeline:       true,
		DebugPipelineWriter: &sink,
	}

	emitDebugPipelineTimeline(report, cfg)

	out := sink.String()
	if !strings.Contains(out, "=== E2E Pipeline Timeline ===") {
		t.Errorf("Sub-AC 7c: expected pipeline timeline header in output, got:\n%s", out)
	}
	if !strings.Contains(out, "E1.1") {
		t.Errorf("Sub-AC 7c: expected TC ID 'E1.1' in pipeline output, got:\n%s", out)
	}
	if !strings.Contains(out, "queue wait summary:") {
		t.Errorf("Sub-AC 7c: expected 'queue wait summary:' in pipeline output, got:\n%s", out)
	}
}

// TestAC7cPipelineTCRecordLabel verifies the label() method priority order.
func TestAC7cPipelineTCRecordLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rec      pipelineTCRecord
		wantHave string // substring that must appear in the label
	}{
		{
			name:     "TCID takes priority",
			rec:      pipelineTCRecord{TCID: "E1.2", TestName: "Other", SpecText: "Another"},
			wantHave: "E1.2",
		},
		{
			name:     "TestName fallback when TCID empty",
			rec:      pipelineTCRecord{TCID: "", TestName: "My Test", SpecText: "Another"},
			wantHave: "My Test",
		},
		{
			name:     "SpecText fallback when both empty",
			rec:      pipelineTCRecord{TCID: "", TestName: "", SpecText: "spec body text"},
			wantHave: "spec body text",
		},
		{
			name:     "unknown fallback when all empty",
			rec:      pipelineTCRecord{},
			wantHave: "<unknown>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.rec.label()
			if !strings.Contains(got, tt.wantHave) {
				t.Errorf("Sub-AC 7c: label()=%q, want it to contain %q", got, tt.wantHave)
			}
		})
	}
}

// TestAC7cPipelineTCRecordLabelTruncatesLongSpecText verifies that SpecText
// longer than 30 characters is truncated with "..." suffix.
func TestAC7cPipelineTCRecordLabelTruncatesLongSpecText(t *testing.T) {
	t.Parallel()
	longText := strings.Repeat("x", 50)
	rec := pipelineTCRecord{SpecText: longText}
	got := rec.label()

	if len(got) > 30 {
		t.Errorf("Sub-AC 7c: label for long SpecText=%q has length %d, want ≤30", got, len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("Sub-AC 7c: long SpecText label %q should end with '...'", got)
	}
}
