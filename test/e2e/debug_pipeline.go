package e2e

// debug_pipeline.go — Sub-AC 7c: --debug-pipeline flag implementation.
//
// When -e2e.debug-pipeline (or E2E_DEBUG_PIPELINE) is set, the suite renders
// a full end-to-end pipeline timeline to stderr after all TCs complete.
//
// The timeline shows four sections:
//
//  1. Header — suite name, total pipeline duration, TC count, Ginkgo process
//     count, and peak concurrency.
//
//  2. ASCII Gantt chart — one row per Ginkgo parallel process.  Each column
//     represents an equal slice of pipeline wall-clock time.  '#' means at
//     least one TC was executing on that process during the slot; '.' means
//     idle.  The tick duration (time per column) is printed as a legend.
//
//  3. TC execution list — all TCs sorted by start time, showing:
//     - Rank (1-based)
//     - TC ID (or spec text as fallback)
//     - Ginkgo process number
//     - Queue wait  (TC start − suite start: how long before this TC ran)
//     - Start offset (same as queue wait; included for readability)
//     - Total duration
//     - Per-phase breakdown [setup=<dur> action=<dur> teardown=<dur>]
//
//  4. Queue-wait statistics — min, max, and average queue wait across all TCs.
//     Queue wait is the time from the pipeline start (earliest TC start) to
//     when a given TC began executing — a proxy for scheduling delay.
//
// Data source: per-TC tc_timing report entries written by timing_capture.go
// during the Ginkgo BeforeEach / JustBeforeEach / JustAfterEach hooks.
// Only specs with a populated StartedAt and FinishedAt are included.
//
// Entry point: emitDebugPipelineTimeline is called from the ReportAfterSuite
// hook in profile_report.go when cfg.DebugPipeline is true.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// pipelineTCRecord holds the per-TC data needed to render the pipeline
// timeline. It is extracted from testCaseTimingProfile entries in the
// Ginkgo suite report.
type pipelineTCRecord struct {
	// TCID is the canonical TC identifier (e.g. "E1.2"). Empty for specs
	// that bypass UsePerTestCaseSetup.
	TCID string

	// TestName is the human-readable test name. Used as the label fallback
	// when TCID is empty.
	TestName string

	// SpecText is the raw Ginkgo leaf-node text. Used as the final fallback
	// label when both TCID and TestName are empty.
	SpecText string

	// ParallelProcess is the Ginkgo worker process number (1-based).
	ParallelProcess int

	// StartedAt is the wall-clock time when the TC began executing
	// (from testCaseTimingProfile.StartedAt).
	StartedAt time.Time

	// FinishedAt is the wall-clock time when the TC finished
	// (from testCaseTimingProfile.FinishedAt).
	FinishedAt time.Time

	// TotalNanos is the TC wall-clock duration in nanoseconds.
	TotalNanos int64

	// SetupNanos is the tc.setup.total phase duration in nanoseconds.
	// Falls back to hook.before_each when tc.setup.total is absent.
	SetupNanos int64

	// ActionNanos is the spec.body phase duration in nanoseconds.
	ActionNanos int64

	// TeardownNanos is the tc.teardown.total phase duration in nanoseconds.
	TeardownNanos int64
}

// label returns the display label for the TC in the pipeline timeline.
// Priority: TCID > TestName > SpecText > "<unknown>".
func (r pipelineTCRecord) label() string {
	if r.TCID != "" {
		return r.TCID
	}
	if r.TestName != "" {
		return r.TestName
	}
	if r.SpecText != "" {
		// Truncate long spec texts so the timeline table stays readable.
		if len(r.SpecText) > 30 {
			return r.SpecText[:27] + "..."
		}
		return r.SpecText
	}
	return "<unknown>"
}

// PipelineTimeline is the computed timeline for the full suite run.
// It is built by buildPipelineTimeline from the Ginkgo types.Report.
type PipelineTimeline struct {
	// SuiteName is the Ginkgo suite description string.
	SuiteName string

	// TCs holds one entry per instrumented TC, sorted by StartedAt ascending.
	TCs []pipelineTCRecord

	// SuiteStart is the minimum StartedAt across all TCs — the moment the
	// first TC began executing on any process.
	SuiteStart time.Time

	// SuiteEnd is the maximum FinishedAt across all TCs — the moment the
	// last TC finished executing.
	SuiteEnd time.Time

	// PipelineNanos is SuiteEnd − SuiteStart in nanoseconds.
	PipelineNanos int64

	// MaxProcess is the highest Ginkgo process number seen across all TCs.
	// It is used as the Gantt chart row count.
	MaxProcess int
}

// PipelineDuration returns the total pipeline wall-clock span.
func (p PipelineTimeline) PipelineDuration() time.Duration {
	return time.Duration(p.PipelineNanos)
}

// buildPipelineTimeline constructs a PipelineTimeline from the Ginkgo suite
// report by extracting per-TC testCaseTimingProfile data from tc_timing report
// entries.
//
// Only specs whose timing profiles have non-zero StartedAt and FinishedAt
// timestamps are included. The pipeline start is the minimum StartedAt; the
// pipeline end is the maximum FinishedAt across all included TCs.
//
// TCs are sorted ascending by StartedAt for consistent rendering; ties are
// broken alphabetically by TCID.
func buildPipelineTimeline(report types.Report) PipelineTimeline {
	tl := PipelineTimeline{
		SuiteName: strings.TrimSpace(report.SuiteDescription),
	}

	for _, spec := range report.SpecReports {
		profile, ok, err := timingProfileFromReportEntries(spec.ReportEntries)
		if !ok || err != nil {
			continue
		}
		if profile.StartedAt.IsZero() || profile.FinishedAt.IsZero() {
			continue
		}

		setup, action, teardown := extractStepDurations(profile)
		rec := pipelineTCRecord{
			TCID:            profile.TCID,
			TestName:        profile.TestName,
			SpecText:        profile.SpecText,
			ParallelProcess: profile.ParallelProcess,
			StartedAt:       profile.StartedAt,
			FinishedAt:      profile.FinishedAt,
			TotalNanos:      profile.TotalNanos,
			SetupNanos:      setup.Nanoseconds(),
			ActionNanos:     action.Nanoseconds(),
			TeardownNanos:   teardown.Nanoseconds(),
		}

		if tl.SuiteStart.IsZero() || rec.StartedAt.Before(tl.SuiteStart) {
			tl.SuiteStart = rec.StartedAt
		}
		if tl.SuiteEnd.IsZero() || rec.FinishedAt.After(tl.SuiteEnd) {
			tl.SuiteEnd = rec.FinishedAt
		}
		if rec.ParallelProcess > tl.MaxProcess {
			tl.MaxProcess = rec.ParallelProcess
		}

		tl.TCs = append(tl.TCs, rec)
	}

	if !tl.SuiteStart.IsZero() && !tl.SuiteEnd.IsZero() {
		tl.PipelineNanos = tl.SuiteEnd.Sub(tl.SuiteStart).Nanoseconds()
	}

	// Sort ascending by start time; break ties by TCID for determinism.
	sort.Slice(tl.TCs, func(i, j int) bool {
		if tl.TCs[i].StartedAt.Equal(tl.TCs[j].StartedAt) {
			return tl.TCs[i].TCID < tl.TCs[j].TCID
		}
		return tl.TCs[i].StartedAt.Before(tl.TCs[j].StartedAt)
	})

	return tl
}

// printPipelineTimeline renders the full pipeline timeline to w.
//
// The output has four sections (see package-level doc for details):
//  1. Header with suite name, pipeline duration, TC/process/concurrency counts.
//  2. ASCII Gantt chart (one row per Ginkgo process, '#'=running '.'=idle).
//  3. TC execution list sorted by start time.
//  4. Queue-wait statistics.
//
// The function is a no-op when w is nil or there are no TCs in the timeline.
func printPipelineTimeline(w io.Writer, tl PipelineTimeline) {
	if w == nil || len(tl.TCs) == 0 {
		return
	}

	peakConcurrency := computePeakConcurrency(tl)

	// ── Section 1: Header ────────────────────────────────────────────────────
	_, _ = fmt.Fprintln(w, "=== E2E Pipeline Timeline ===")
	_, _ = fmt.Fprintf(w, "suite: %s\n", tl.SuiteName)
	_, _ = fmt.Fprintf(w,
		"total pipeline: %s  |  TCs: %d  |  processes: %d  |  peak concurrency: %d\n",
		tl.PipelineDuration(), len(tl.TCs), tl.MaxProcess, peakConcurrency)
	_, _ = fmt.Fprintln(w)

	// ── Section 2: ASCII Gantt chart ─────────────────────────────────────────
	// Only render when there is at least one process and a non-zero pipeline.
	if tl.MaxProcess > 0 && tl.PipelineNanos > 0 {
		const chartWidth = 60
		tickNanos := tl.PipelineNanos / int64(chartWidth)
		if tickNanos < 1 {
			tickNanos = 1
		}

		_, _ = fmt.Fprintf(w, "TC timeline (each char ~%s, #=running .=idle):\n",
			time.Duration(tickNanos).Truncate(time.Millisecond))
		for proc := 1; proc <= tl.MaxProcess; proc++ {
			row := buildGanttRow(tl, proc, chartWidth, tickNanos)
			_, _ = fmt.Fprintf(w, "  proc %-2d [%s]\n", proc, row)
		}
		_, _ = fmt.Fprintln(w)
	}

	// ── Section 3: TC execution list ─────────────────────────────────────────
	_, _ = fmt.Fprintln(w, "TC execution order (by start time):")
	for i, tc := range tl.TCs {
		queueWait := tc.StartedAt.Sub(tl.SuiteStart)
		startOffset := tc.StartedAt.Sub(tl.SuiteStart)

		phases := fmt.Sprintf("[setup=%s action=%s teardown=%s]",
			time.Duration(tc.SetupNanos),
			time.Duration(tc.ActionNanos),
			time.Duration(tc.TeardownNanos))

		_, _ = fmt.Fprintf(w,
			"  %3d. [TC-%-12s] proc=%-2d  queued=%-8s  start=%-8s  dur=%-10s  %s\n",
			i+1,
			tc.label(),
			tc.ParallelProcess,
			queueWait.Truncate(time.Millisecond),
			startOffset.Truncate(time.Millisecond),
			time.Duration(tc.TotalNanos),
			phases)
	}
	_, _ = fmt.Fprintln(w)

	// ── Section 4: Queue-wait statistics ─────────────────────────────────────
	minWait, maxWait, avgWait := computeQueueWaitStats(tl)
	_, _ = fmt.Fprintf(w, "queue wait summary: min=%s  max=%s  avg=%s\n",
		minWait.Truncate(time.Millisecond),
		maxWait.Truncate(time.Millisecond),
		avgWait.Truncate(time.Millisecond))
}

// buildGanttRow renders one ASCII row of the Gantt chart for the given
// Ginkgo process number.
//
// Each column in the returned string covers tickNanos nanoseconds of
// wall-clock time relative to tl.SuiteStart. The returned string has exactly
// width characters: '#' when at least one TC on that process was executing
// during the slot, '.' when idle.
func buildGanttRow(tl PipelineTimeline, process, width int, tickNanos int64) string {
	cols := make([]byte, width)
	for i := range cols {
		cols[i] = '.'
	}

	startNano := tl.SuiteStart.UnixNano()
	for _, tc := range tl.TCs {
		if tc.ParallelProcess != process {
			continue
		}

		tcStartNano := tc.StartedAt.UnixNano() - startNano
		tcEndNano := tc.FinishedAt.UnixNano() - startNano

		colStart := int(tcStartNano / tickNanos)
		// Use ceiling division so a TC that ends mid-column marks the column
		// as running rather than idle.
		colEnd := int((tcEndNano + tickNanos - 1) / tickNanos)

		if colStart < 0 {
			colStart = 0
		}
		if colEnd > width {
			colEnd = width
		}

		for col := colStart; col < colEnd; col++ {
			cols[col] = '#'
		}
	}

	return string(cols)
}

// computePeakConcurrency returns the maximum number of TCs that were executing
// simultaneously at any instant during the pipeline. It uses a sweep-line
// algorithm over start/end events sorted by nanosecond timestamp.
//
// When two events share the same timestamp, end events are processed before
// start events (delta < 0 sorts before delta > 0) to correctly handle zero-
// duration TCs and back-to-back executions.
func computePeakConcurrency(tl PipelineTimeline) int {
	if len(tl.TCs) == 0 {
		return 0
	}

	type event struct {
		nano  int64
		delta int // +1 for start, -1 for end
	}

	events := make([]event, 0, 2*len(tl.TCs))
	for _, tc := range tl.TCs {
		events = append(events,
			event{tc.StartedAt.UnixNano(), +1},
			event{tc.FinishedAt.UnixNano(), -1},
		)
	}

	// Sort by time; when two events share the same nanosecond, process end
	// events (delta=-1) before start events (delta=+1).
	sort.Slice(events, func(i, j int) bool {
		if events[i].nano == events[j].nano {
			return events[i].delta < events[j].delta
		}
		return events[i].nano < events[j].nano
	})

	current, peak := 0, 0
	for _, e := range events {
		current += e.delta
		if current > peak {
			peak = current
		}
	}
	return peak
}

// computeQueueWaitStats returns the minimum, maximum, and average queue wait
// across all TCs in the timeline.
//
// Queue wait for a TC is defined as TC.StartedAt − tl.SuiteStart — the time
// between when the very first TC in the suite began and when this particular
// TC started executing. A TC that started first has queue wait = 0.
//
// Returns (0, 0, 0) when the timeline contains no TCs.
func computeQueueWaitStats(tl PipelineTimeline) (minWait, maxWait, avgWait time.Duration) {
	if len(tl.TCs) == 0 {
		return 0, 0, 0
	}

	var totalNanos int64
	for i, tc := range tl.TCs {
		wait := tc.StartedAt.Sub(tl.SuiteStart)
		nanos := wait.Nanoseconds()
		if i == 0 {
			minWait = wait
			maxWait = wait
		} else {
			if wait < minWait {
				minWait = wait
			}
			if wait > maxWait {
				maxWait = wait
			}
		}
		totalNanos += nanos
	}
	avgWait = time.Duration(totalNanos / int64(len(tl.TCs)))
	return minWait, maxWait, avgWait
}

// emitDebugPipelineTimeline builds and prints the pipeline timeline to
// cfg.DebugPipelineWriter when cfg.DebugPipeline is true.
//
// It is called from the ReportAfterSuite hook in profile_report.go after all
// specs have completed and the consolidated Ginkgo suite report is available.
// Errors from the writer are silently discarded so that a rendering failure
// never causes the suite to report a non-zero exit code.
func emitDebugPipelineTimeline(report types.Report, cfg timingReportConfig) {
	if !cfg.DebugPipeline {
		return
	}
	tl := buildPipelineTimeline(report)
	printPipelineTimeline(cfg.DebugPipelineWriter, tl)
}
