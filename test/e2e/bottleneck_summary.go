package e2e

// bottleneck_summary.go — Sub-AC 6.3 / Sub-AC 3: bottleneck identification
// post-processing.
//
// When -e2e.profile is set, the suite emits a formatted ASCII table to stdout
// after all TCs complete.  The table:
//
//  1. Ranks TCs by wall-clock duration descending.
//  2. Flags TCs whose percentage of suite runtime strictly exceeds the
//     configurable threshold (-e2e.profile.threshold, default 10.0%) with
//     "⚠ OUTLIER".
//  3. Ranks setup phases (before_suite, after_suite, tc_setup) by duration.
//
// Threshold precedence:
//
//	-e2e.profile.threshold=<float>  flag (takes precedence)
//	E2E_PROFILE_THRESHOLD=<float>   env var (fallback when flag is at sentinel)
//	10.0                            built-in default
//
// Activation: any non-empty -e2e.profile path enables the summary.
// The table is suppressed when the profile flag is not set.

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2/types"
)

// OutlierEntry identifies a TC whose wall-clock duration exceeded the
// configured outlier threshold (expressed as a percentage of total suite
// runtime).  Outlier entries are included in the bottleneck summary table
// printed to stdout when -e2e.profile is set.
type OutlierEntry struct {
	// TCID is the canonical TC identifier (e.g. "E5.1", "F27.1").
	TCID string

	// TotalNanos is the TC's total wall-clock duration in nanoseconds.
	TotalNanos int64

	// PctOfSuiteRuntime is the percentage of suite runtime consumed by this TC.
	// This is the value compared against the configurable threshold.
	PctOfSuiteRuntime float64
}

// TotalDuration returns the outlier TC duration as a time.Duration.
func (o OutlierEntry) TotalDuration() time.Duration { return time.Duration(o.TotalNanos) }

// detectOutliers inspects tcs and returns entries whose percentage of
// suiteRuntimeNanos strictly exceeds thresholdPct (pct > threshold, not >=).
//
// The returned slice is ordered by PctOfSuiteRuntime descending (slowest first).
// Ties are broken by TCID ascending for deterministic output.
// Returns nil when suiteRuntimeNanos is ≤ 0 or tcs is empty.
func detectOutliers(tcs []TCProfile, suiteRuntimeNanos int64, thresholdPct float64) []OutlierEntry {
	if suiteRuntimeNanos <= 0 || len(tcs) == 0 {
		return nil
	}
	var outliers []OutlierEntry
	for _, tc := range tcs {
		if tc.TotalNanos <= 0 {
			continue
		}
		pct := float64(tc.TotalNanos) / float64(suiteRuntimeNanos) * 100
		if pct > thresholdPct {
			outliers = append(outliers, OutlierEntry{
				TCID:              tc.TCID,
				TotalNanos:        tc.TotalNanos,
				PctOfSuiteRuntime: pct,
			})
		}
	}
	sort.Slice(outliers, func(i, j int) bool {
		if outliers[i].PctOfSuiteRuntime == outliers[j].PctOfSuiteRuntime {
			return outliers[i].TCID < outliers[j].TCID
		}
		return outliers[i].PctOfSuiteRuntime > outliers[j].PctOfSuiteRuntime
	})
	return outliers
}

// isOutlier returns true when tcID appears in the outliers slice.
func isOutlier(tcID string, outliers []OutlierEntry) bool {
	for _, o := range outliers {
		if o.TCID == tcID {
			return true
		}
	}
	return false
}

// allPhasesRankedByDuration returns all setup phase entries from the
// ProfileReport ordered by TotalNanos descending.  Unlike
// ProfileReport.SlowSetupPhases (which is capped at N), this function copies
// the full set available in the report and re-sorts it.
// Returns nil when the SlowSetupPhases slice is empty.
func allPhasesRankedByDuration(report ProfileReport) []SetupPhaseBottleneck {
	if len(report.SlowSetupPhases) == 0 {
		return nil
	}
	phases := make([]SetupPhaseBottleneck, len(report.SlowSetupPhases))
	copy(phases, report.SlowSetupPhases)
	sort.Slice(phases, func(i, j int) bool {
		if phases[i].TotalNanos == phases[j].TotalNanos {
			if phases[i].Phase == phases[j].Phase {
				return phases[i].TCID < phases[j].TCID
			}
			return phases[i].Phase < phases[j].Phase
		}
		return phases[i].TotalNanos > phases[j].TotalNanos
	})
	return phases
}

const (
	// outlierFlagLabel is the text appended to TC rows that exceed the threshold.
	outlierFlagLabel = "⚠ OUTLIER"
)

// printBottleneckSummaryTable writes a formatted ASCII summary table to out.
//
// The table contains three sections:
//
//  1. Header: configured threshold, suite runtime, TC count, outlier count.
//  2. TC ranking: all TCs from report.TCs sorted by duration descending;
//     TCs that exceed the threshold are flagged with outlierFlagLabel.
//  3. Phase ranking: setup phases from report.SlowSetupPhases sorted by
//     duration descending (section omitted when SlowSetupPhases is empty).
//
// The function is a no-op (returns nil) when out is nil or report.TCs is empty.
func printBottleneckSummaryTable(out io.Writer, report ProfileReport, outliers []OutlierEntry, thresholdPct float64) error {
	if out == nil || len(report.TCs) == 0 {
		return nil
	}

	suiteRuntime := time.Duration(report.SuiteRuntimeNanos)

	// ── Header ────────────────────────────────────────────────────────────────
	if _, err := fmt.Fprintln(out, "=== E2E Bottleneck Summary ==="); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out,
		"threshold: %.1f%% of suite runtime | suite: %s | TCs: %d\n",
		thresholdPct, fmtDur(suiteRuntime), len(report.TCs)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "outliers flagged: %d\n\n", len(outliers)); err != nil {
		return err
	}

	// ── TC ranking table ─────────────────────────────────────────────────────
	tcHeader := fmt.Sprintf("%-5s  %-14s  %-10s  %-8s  %s",
		"RANK", "TC-ID", "DURATION", "% SUITE", "FLAG")
	if _, err := fmt.Fprintln(out, "TCs ranked by duration:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, tcHeader); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, strings.Repeat("-", len(tcHeader))); err != nil {
		return err
	}
	for i, tc := range report.TCs {
		var pct float64
		if report.SuiteRuntimeNanos > 0 {
			pct = float64(tc.TotalNanos) / float64(report.SuiteRuntimeNanos) * 100
		}
		flag := ""
		if isOutlier(tc.TCID, outliers) {
			flag = outlierFlagLabel
		}
		line := fmt.Sprintf("%-5d  %-14s  %-10s  %7.1f%%  %s",
			i+1, tc.TCID, fmtDur(tc.TotalDuration()), pct, flag)
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}

	// ── Phase ranking table ──────────────────────────────────────────────────
	phases := allPhasesRankedByDuration(report)
	if len(phases) > 0 {
		phHeader := fmt.Sprintf("%-5s  %-14s  %-14s  %-10s  %s",
			"RANK", "PHASE", "TC-ID", "DURATION", "% SUITE")
		if _, err := fmt.Fprintln(out, "\nPhases ranked by duration:"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, phHeader); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, strings.Repeat("-", len(phHeader))); err != nil {
			return err
		}
		for i, ph := range phases {
			var pct float64
			if report.SuiteRuntimeNanos > 0 {
				pct = float64(ph.TotalNanos) / float64(report.SuiteRuntimeNanos) * 100
			}
			line := fmt.Sprintf("%-5d  %-14s  %-14s  %-10s  %7.1f%%",
				i+1, ph.Phase, ph.TCID, fmtDur(ph.TotalDuration()), pct)
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}

	return nil
}

// emitBottleneckSummary is the main entry point for Sub-AC 3 post-processing.
// It is called from the ReportAfterSuite hook in profile_report.go.
//
// It:
//  1. Builds the ProfileReport from the Ginkgo suite report.
//  2. Detects outliers exceeding cfg.TimingReport.OutlierThresholdPct.
//  3. Prints the bottleneck summary table to cfg.TimingReport.SummaryWriter
//     (os.Stdout when profiling is enabled).
//
// It is a no-op when profiling is disabled (cfg.TimingReport.Enabled is false)
// or SummaryWriter is nil.
func emitBottleneckSummary(cfg suiteExecutionConfig, report types.Report) error {
	if !cfg.TimingReport.Enabled || cfg.TimingReport.SummaryWriter == nil {
		return nil
	}
	pr := buildProfileReport(
		report,
		cfg.TimingReport.BottleneckLimit,
		cfg.TimingReport.SlowSetupPhaseLimit,
	)
	outliers := detectOutliers(pr.TCs, pr.SuiteRuntimeNanos, cfg.TimingReport.OutlierThresholdPct)
	return printBottleneckSummaryTable(
		cfg.TimingReport.SummaryWriter,
		pr,
		outliers,
		cfg.TimingReport.OutlierThresholdPct,
	)
}
