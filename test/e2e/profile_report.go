package e2e

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// timedSpecRecord is an internal helper used by the human-readable text
// summary. It is separate from TCProfile so the text format can be computed
// without the full JSON marshalling path.
type timedSpecRecord struct {
	TCID     string
	TestName string
	Duration time.Duration
}

// suiteTimingSummary is the intermediate data structure for the text-format
// timing report (the pre-existing "=== E2E Timing Profile ===" output).
// Sub-AC 6.3 adds slowSetupPhases for setup phase bottleneck reporting.
type suiteTimingSummary struct {
	SuiteName       string
	TotalSpecs      int
	SelectedSpecs   int
	RunTime         time.Duration
	SlowSpecs       []timedSpecRecord
	Bottleneck      string
	slowSetupPhases []timedSetupPhaseRecord // Sub-AC 6.3: populated when setup phase data available
}

// ReportAfterSuite hook – runs on the primary Ginkgo process after all specs
// have finished. Emits the legacy text summary to stderr and the JSON
// ProfileReport to the file path specified by -e2e.profile when the flag is set.
// Also appends BeforeSuite timing to the setup-phase log (Sub-AC 6.2).
//
// Sub-AC 6.3: passes SlowSetupPhaseLimit to ProfileCollector.Flush so the
// JSON report includes the slowest N setup phases.
//
// Sub-AC 7c: calls emitDebugPipelineTimeline to print the full end-to-end
// pipeline timeline when -e2e.debug-pipeline (or E2E_DEBUG_PIPELINE) is set.
var _ = ReportAfterSuite("suite timing profile", func(report types.Report) {
	cfg := currentSuiteExecutionConfig()
	_ = emitSuiteTimingReport(cfg, report)
	if cfg.TimingReport.ProfilePath != "" {
		collector := newProfileCollector(
			cfg.TimingReport.ProfilePath,
			cfg.TimingReport.BottleneckLimit,
			cfg.TimingReport.SlowSetupPhaseLimit,
		)
		_ = collector.Flush(report)
	}
	// AC 6.2: append BeforeSuite / SynchronizedBeforeSuite timing entries to
	// the structured setup-phase log.  The consolidated Ginkgo report is the
	// authoritative source for suite-level phase runtimes.
	appendBeforeSuiteToSetupPhaseLog(report)

	// Sub-AC 7c: render the full end-to-end pipeline timeline when the flag
	// is set.  The consolidated report is required so this must run here rather
	// than in an AfterEach hook.
	emitDebugPipelineTimeline(report, cfg.TimingReport)
})

// emitSuiteTimingReport is the main entry point called by the hook. It emits
// a human-readable text timing summary to cfg.TimingReport.Output (stderr).
// The JSON ProfileReport is written separately by ProfileCollector.Flush via
// the ReportAfterSuite hook above.
//
// Sub-AC 6.3: uses summarizeSuiteTimingFull with SlowSetupPhaseLimit so the
// text output also lists the slowest N setup phases when available.
func emitSuiteTimingReport(cfg suiteExecutionConfig, report types.Report) error {
	if !cfg.TimingReport.Enabled {
		return nil
	}

	// --- Text summary with setup phase reporting (Sub-AC 6.3) ---
	summary := summarizeSuiteTimingFull(report, cfg.TimingReport.SlowSpecLimit, cfg.TimingReport.SlowSetupPhaseLimit)
	return writeSuiteTimingReportFull(cfg.TimingReport.Output, summary)
}

// buildProfileReport constructs a ProfileReport from the Ginkgo suite report.
// It harvests per-TC timing from the tc_timing report entries (written by
// timing_capture.go) and from the tc_id / tc_category / tc_test_name entries.
// Group-level setup and teardown durations are extracted from BeforeSuite /
// AfterSuite spec reports (including SynchronizedBeforeSuite variants) via
// collectGroupTimingFromReport and attached to every TCProfile.
//
// Sub-AC 6.3: slowSetupPhaseLimit controls how many setup phases appear in
// ProfileReport.SlowSetupPhases. Pass ≤0 to use defaultSlowSetupPhaseLimit.
func buildProfileReport(report types.Report, bottleneckLimit int, slowSetupPhaseLimit ...int) ProfileReport {
	pr := ProfileReport{
		SuiteName:         strings.TrimSpace(report.SuiteDescription),
		TotalSpecs:        report.PreRunStats.TotalSpecs,
		SelectedSpecs:     report.PreRunStats.SpecsThatWillRun,
		SuiteRuntimeNanos: report.RunTime.Nanoseconds(),
		GeneratedAt:       time.Now().UTC(),
	}

	// Collect group-level phase durations once; they are shared by all TCs.
	groupSetupNanos, groupTeardownNanos := collectGroupTimingFromReport(report)

	for _, spec := range report.SpecReports {
		tcID, ok := reportEntryValue(spec.ReportEntries, "tc_id")
		if !ok {
			continue
		}

		category, _ := reportEntryValue(spec.ReportEntries, "tc_category")
		testName, _ := reportEntryValue(spec.ReportEntries, "tc_test_name")
		if testName == "" {
			testName = strings.TrimSpace(spec.LeafNodeText)
		}

		phases := phaseTimingsFromInternalProfile(spec.ReportEntries, groupSetupNanos, groupTeardownNanos)

		pr.TCs = append(pr.TCs, TCProfile{
			TCID:       tcID,
			Category:   category,
			TestName:   testName,
			TotalNanos: spec.RunTime.Nanoseconds(),
			Phases:     phases,
		})
	}

	// Sort descending by total duration, then ascending by TCID for
	// deterministic ordering when durations are equal.
	sort.Slice(pr.TCs, func(i, j int) bool {
		if pr.TCs[i].TotalNanos == pr.TCs[j].TotalNanos {
			return pr.TCs[i].TCID < pr.TCs[j].TCID
		}
		return pr.TCs[i].TotalNanos > pr.TCs[j].TotalNanos
	})

	// Build the Bottlenecks list from the top-N slowest TCs.
	limit := bottleneckLimit
	if limit <= 0 {
		limit = profileReportBottleneckLimit
	}
	if limit > len(pr.TCs) {
		limit = len(pr.TCs)
	}

	for rank, tc := range pr.TCs[:limit] {
		var pct float64
		if report.RunTime > 0 {
			pct = (float64(tc.TotalNanos) / float64(report.RunTime.Nanoseconds())) * 100
		}
		pr.Bottlenecks = append(pr.Bottlenecks, BottleneckEntry{
			Rank:              rank + 1,
			TCID:              tc.TCID,
			TotalNanos:        tc.TotalNanos,
			PctOfSuiteRuntime: pct,
		})
	}

	// Sub-AC 6.3: build the SlowSetupPhases list.
	setupPhaseLimit := defaultSlowSetupPhaseLimit
	if len(slowSetupPhaseLimit) > 0 && slowSetupPhaseLimit[0] > 0 {
		setupPhaseLimit = slowSetupPhaseLimit[0]
	}
	pr.SlowSetupPhases = buildSlowSetupPhases(report, pr.TCs, setupPhaseLimit)

	return pr
}

// buildSlowSetupPhases collects setup phase timings from the suite report and
// TC profiles, sorts them by duration descending, and returns the slowest N.
//
// Setup phase sources:
//   - "before_suite": BeforeSuite / SynchronizedBeforeSuite spec report.
//   - "after_suite":  AfterSuite / SynchronizedAfterSuite spec report.
//   - "tc_setup":     Per-TC setup phase (TCSetupNanos) from each TCProfile.
//     Only included when TCSetupNanos > 0 (i.e. when tc_timing instrumentation
//     is active).
func buildSlowSetupPhases(report types.Report, tcs []TCProfile, limit int) []SetupPhaseBottleneck {
	if limit <= 0 {
		limit = defaultSlowSetupPhaseLimit
	}

	type rawPhase struct {
		phase      string
		tcID       string
		totalNanos int64
	}

	var phases []rawPhase

	// Suite-level setup / teardown from Ginkgo spec reports.
	for _, spec := range report.SpecReports {
		n := spec.RunTime.Nanoseconds()
		if n <= 0 {
			continue
		}
		switch spec.LeafNodeType {
		case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
			phases = append(phases, rawPhase{phase: setupPhaseBeforeSuite, totalNanos: n})
		case types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
			phases = append(phases, rawPhase{phase: "after_suite", totalNanos: n})
		}
	}

	// Per-TC setup phases from TCProfile.Phases.TCSetupNanos.
	for _, tc := range tcs {
		if tc.Phases.TCSetupNanos > 0 {
			phases = append(phases, rawPhase{
				phase:      "tc_setup",
				tcID:       tc.TCID,
				totalNanos: tc.Phases.TCSetupNanos,
			})
		}
	}

	// Sort descending by duration; break ties by phase name then TCID.
	sort.Slice(phases, func(i, j int) bool {
		if phases[i].totalNanos == phases[j].totalNanos {
			if phases[i].phase == phases[j].phase {
				return phases[i].tcID < phases[j].tcID
			}
			return phases[i].phase < phases[j].phase
		}
		return phases[i].totalNanos > phases[j].totalNanos
	})

	if limit > len(phases) {
		limit = len(phases)
	}

	var result []SetupPhaseBottleneck
	for rank, p := range phases[:limit] {
		var pct float64
		if report.RunTime > 0 {
			pct = (float64(p.totalNanos) / float64(report.RunTime.Nanoseconds())) * 100
		}
		result = append(result, SetupPhaseBottleneck{
			Rank:              rank + 1,
			Phase:             p.phase,
			TCID:              p.tcID,
			TotalNanos:        p.totalNanos,
			PctOfSuiteRuntime: pct,
		})
	}
	return result
}

// phaseTimingsFromInternalProfile extracts the five public PhaseTimings values
// from the internal tc_timing JSON report entry written by timing_capture.go.
// groupSetupNanos and groupTeardownNanos are the suite-level group phase
// durations collected by collectGroupTimingFromReport; they are the same for
// every TC in a given suite run.
//
// When no tc_timing entry is present (e.g. specs without per-TC timing
// instrumentation), a PhaseTimings value containing only the group-level
// durations is returned.
func phaseTimingsFromInternalProfile(entries types.ReportEntries, groupSetupNanos, groupTeardownNanos int64) PhaseTimings {
	pt := PhaseTimings{
		GroupSetupNanos:    groupSetupNanos,
		GroupTeardownNanos: groupTeardownNanos,
	}

	internalProfile, ok, err := timingProfileFromReportEntries(entries)
	if !ok || err != nil {
		return pt
	}

	for _, sample := range internalProfile.Phases {
		switch executionPhase(sample.Name) {
		case phaseSetupTotal:
			pt.TCSetupNanos = sample.DurationNanos
		case phaseSpecBody:
			pt.TCExecuteNanos = sample.DurationNanos
		case phaseTeardownTotal:
			pt.TCTeardownNanos = sample.DurationNanos
		}
	}
	return pt
}

// ---- text-format report helpers --------------------------------------------

// timedSetupPhaseRecord is an internal helper for the text summary of
// setup phases (Sub-AC 6.3).
type timedSetupPhaseRecord struct {
	Phase    string
	TCID     string
	Duration time.Duration
}

// suiteTimingSummary extends the existing summary with setup phase timing
// (Sub-AC 6.3).
// Note: slowSetupPhases is added here; the existing fields are unchanged so
// callers that only use SlowSpecs continue to work without modification.
func summarizeSuiteTiming(report types.Report, slowSpecLimit int) suiteTimingSummary {
	return summarizeSuiteTimingFull(report, slowSpecLimit, defaultSlowSetupPhaseLimit)
}

// summarizeSuiteTimingFull is the canonical implementation used by
// emitSuiteTimingReport when Sub-AC 6.3 setup-phase reporting is active.
func summarizeSuiteTimingFull(report types.Report, slowSpecLimit, setupPhaseLimit int) suiteTimingSummary {
	summary := suiteTimingSummary{
		SuiteName:     strings.TrimSpace(report.SuiteDescription),
		TotalSpecs:    report.PreRunStats.TotalSpecs,
		SelectedSpecs: report.PreRunStats.SpecsThatWillRun,
		RunTime:       report.RunTime,
	}

	for _, spec := range report.SpecReports {
		tcID, ok := reportEntryValue(spec.ReportEntries, "tc_id")
		if !ok {
			continue
		}

		testName, ok := reportEntryValue(spec.ReportEntries, "tc_test_name")
		if !ok {
			testName = strings.TrimSpace(spec.LeafNodeText)
		}

		summary.SlowSpecs = append(summary.SlowSpecs, timedSpecRecord{
			TCID:     tcID,
			TestName: testName,
			Duration: spec.RunTime,
		})
	}

	sort.Slice(summary.SlowSpecs, func(i, j int) bool {
		if summary.SlowSpecs[i].Duration == summary.SlowSpecs[j].Duration {
			return summary.SlowSpecs[i].TCID < summary.SlowSpecs[j].TCID
		}
		return summary.SlowSpecs[i].Duration > summary.SlowSpecs[j].Duration
	})

	if slowSpecLimit >= 0 && len(summary.SlowSpecs) > slowSpecLimit {
		summary.SlowSpecs = summary.SlowSpecs[:slowSpecLimit]
	}

	switch {
	case len(summary.SlowSpecs) == 0:
		summary.Bottleneck = "no TC timing samples collected"
	case summary.RunTime <= 0:
		summary.Bottleneck = fmt.Sprintf("slowest TC %s has no suite runtime baseline", summary.SlowSpecs[0].TCID)
	default:
		top := summary.SlowSpecs[0]
		pct := (float64(top.Duration) / float64(summary.RunTime)) * 100
		summary.Bottleneck = fmt.Sprintf("slowest TC %s consumed %.1f%% of suite runtime", top.TCID, pct)
	}

	// Sub-AC 6.3: collect setup phase timings.
	_ = setupPhaseLimit // used by writeSuiteTimingReportFull via the caller
	summary.slowSetupPhases = collectTextSetupPhases(report, setupPhaseLimit)

	return summary
}

// collectTextSetupPhases gathers the slowest N setup phases for inclusion in
// the text timing summary (Sub-AC 6.3). Returns at most limit entries sorted
// by duration descending.
func collectTextSetupPhases(report types.Report, limit int) []timedSetupPhaseRecord {
	if limit <= 0 {
		limit = defaultSlowSetupPhaseLimit
	}

	var phases []timedSetupPhaseRecord
	for _, spec := range report.SpecReports {
		n := spec.RunTime
		if n <= 0 {
			continue
		}
		switch spec.LeafNodeType {
		case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
			phases = append(phases, timedSetupPhaseRecord{Phase: setupPhaseBeforeSuite, Duration: n})
		case types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
			phases = append(phases, timedSetupPhaseRecord{Phase: "after_suite", Duration: n})
		}
	}

	sort.Slice(phases, func(i, j int) bool {
		return phases[i].Duration > phases[j].Duration
	})
	if limit > len(phases) {
		limit = len(phases)
	}
	return phases[:limit]
}

func writeSuiteTimingReport(output io.Writer, summary suiteTimingSummary) error {
	return writeSuiteTimingReportFull(output, summary)
}

func writeSuiteTimingReportFull(output io.Writer, summary suiteTimingSummary) error {
	if output == nil {
		return nil
	}

	if _, err := fmt.Fprintln(output, "=== E2E Timing Profile ==="); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "suite: %s\n", summary.SuiteName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "selected specs: %d/%d\n", summary.SelectedSpecs, summary.TotalSpecs); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "suite runtime: %s\n", summary.RunTime); err != nil {
		return err
	}

	if len(summary.SlowSpecs) == 0 {
		if _, err := fmt.Fprintln(output, "slowest specs: none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(output, "slowest specs:"); err != nil {
			return err
		}
		for i, spec := range summary.SlowSpecs {
			if _, err := fmt.Fprintf(output, "%d. %s :: %s (%s)\n", i+1, spec.TCID, spec.TestName, spec.Duration); err != nil {
				return err
			}
		}
	}

	// Sub-AC 6.3: emit setup phase section when data is available.
	if len(summary.slowSetupPhases) > 0 {
		if _, err := fmt.Fprintln(output, "slowest setup phases:"); err != nil {
			return err
		}
		for i, p := range summary.slowSetupPhases {
			label := p.Phase
			if p.TCID != "" {
				label = fmt.Sprintf("%s[%s]", p.Phase, p.TCID)
			}
			if _, err := fmt.Fprintf(output, "%d. %s (%s)\n", i+1, label, p.Duration); err != nil {
				return err
			}
		}
	}

	_, err := fmt.Fprintf(output, "bottleneck: %s\n", summary.Bottleneck)
	return err
}

// appendBeforeSuiteToSetupPhaseLog scans the consolidated Ginkgo suite report
// for BeforeSuite and SynchronizedBeforeSuite spec reports and appends one
// setupPhaseLogEntry per report to suiteSetupPhaseLog (Sub-AC 6.2).
//
// Only reports with a positive RunTime are logged; reports with zero RunTime
// (e.g. skipped suite-level nodes) are silently ignored.
//
// The StartedAt timestamp is taken from spec.StartTime when available; if
// StartTime is zero the best available approximation is
// spec.EndTime.Add(-spec.RunTime). When neither is available the entry is
// still appended with a zero StartedAt so the log is complete.
func appendBeforeSuiteToSetupPhaseLog(report types.Report) {
	for _, spec := range report.SpecReports {
		switch spec.LeafNodeType {
		case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
			if spec.RunTime <= 0 {
				continue
			}
			startedAt := spec.StartTime
			if startedAt.IsZero() && !spec.EndTime.IsZero() {
				startedAt = spec.EndTime.Add(-spec.RunTime)
			}
			finishedAt := startedAt.Add(spec.RunTime)
			if !spec.EndTime.IsZero() {
				finishedAt = spec.EndTime
			}
			appendSetupPhaseEntry(setupPhaseLogEntry{
				Phase:         setupPhaseBeforeSuite,
				StartedAt:     startedAt,
				FinishedAt:    finishedAt,
				DurationNanos: spec.RunTime.Nanoseconds(),
			})
		}
	}
}

// reportEntryValue looks up an entry by name in a Ginkgo ReportEntries slice
// and returns its string representation. Returns ("", false) when the entry is
// absent or has an empty string value.
func reportEntryValue(entries types.ReportEntries, name string) (string, bool) {
	for _, entry := range entries {
		if entry.Name != name {
			continue
		}

		value := strings.TrimSpace(entry.StringRepresentation())
		if value == "" {
			return "", false
		}
		return value, true
	}

	return "", false
}
