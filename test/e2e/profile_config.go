package e2e

import (
	"flag"
	"io"
	"os"
	"strconv"
	"sync"
)

// defaultOutlierThresholdPct is the default percentage of suite runtime above
// which a TC is flagged as an outlier in the bottleneck summary table printed
// to stdout when -e2e.profile is set (Sub-AC 3).
const defaultOutlierThresholdPct = 10.0

const (
	// defaultTimingReportSlowSpecLimit is the number of slowest TCs retained
	// in the text-format suite timing summary when -e2e.profile.top-n is not set.
	defaultTimingReportSlowSpecLimit = 10

	// profileReportBottleneckLimit is the default number of slowest TCs captured in
	// the JSON ProfileReport.Bottlenecks list when -e2e.profile.top-n is not set.
	profileReportBottleneckLimit = 5

	// defaultSlowSetupPhaseLimit is the default number of slowest setup phases
	// captured in the JSON ProfileReport.SlowSetupPhases list.
	defaultSlowSetupPhaseLimit = 5

	// envProfilePath is the environment variable name used as a fallback for
	// the -e2e.profile flag.  The flag value takes precedence when both are set.
	envProfilePath = "E2E_PROFILE"

	// envProfileTopN is the environment variable name used as a fallback for
	// the -e2e.profile.top-n flag.  The flag value takes precedence when both
	// are set.
	envProfileTopN = "E2E_PROFILE_TOP_N"

	// envDebugTCDuration is the environment variable name used as a fallback for
	// the -e2e.debug-tc-duration flag. The flag value takes precedence when both
	// are set. Any non-empty value enables per-TC duration output.
	envDebugTCDuration = "E2E_DEBUG_TC_DURATION"

	// envTimingTC is the canonical AC-7 environment variable name for per-TC
	// duration output. It is checked as an alias for envDebugTCDuration; either
	// variable enables the feature. Set to any non-empty value (e.g. "1").
	envTimingTC = "E2E_TIMING_TC"

	// envDebugTCSteps is the environment variable name used as a fallback for
	// the -e2e.debug-tc-steps flag. The flag value takes precedence when both
	// are set. Any non-empty value enables per-TC step timing output.
	envDebugTCSteps = "E2E_DEBUG_TC_STEPS"

	// envTimingSteps is the canonical AC-7 environment variable name for per-TC
	// step breakdown output. It is checked as an alias for envDebugTCSteps; either
	// variable enables the feature. Set to any non-empty value (e.g. "1").
	envTimingSteps = "E2E_TIMING_STEPS"

	// envDebugPipeline is the environment variable name used as a fallback for
	// the -e2e.debug-pipeline flag. The flag value takes precedence when both
	// are set. Any non-empty value enables the end-to-end pipeline timeline output.
	envDebugPipeline = "E2E_DEBUG_PIPELINE"

	// envProfileOutlierThreshold is the environment variable name for the
	// configurable outlier threshold percentage (Sub-AC 3).
	// The flag value takes precedence when both are set.
	// Example: E2E_PROFILE_THRESHOLD=15 flags TCs consuming > 15% of suite runtime.
	envProfileOutlierThreshold = "E2E_PROFILE_THRESHOLD"

	// outlierThresholdFlagSentinel is the sentinel value used as the default for
	// e2eProfileOutlierThresholdFlag to distinguish "not set on the command line"
	// from an explicit 0.0 (which means "flag every TC with any runtime").
	// When the flag is still at the sentinel, the env var and then the built-in
	// default (defaultOutlierThresholdPct) are consulted.
	outlierThresholdFlagSentinel = -1.0

	// envTimingPipeline is the canonical AC-7 environment variable name for the
	// full pipeline timeline output. It is checked as an alias for
	// envDebugPipeline; either variable enables the feature. Set to any non-empty
	// value (e.g. "1").
	envTimingPipeline = "E2E_TIMING_PIPELINE"
)

// e2eTimingReportFlag corresponds to -e2e.profile. When set to a non-empty
// file path the suite emits:
//  1. A human-readable text timing summary to stderr (existing behaviour).
//  2. A machine-readable JSON ProfileReport written to the specified file path,
//     containing per-TC TCProfile entries with PhaseTimings breakdowns, the
//     slowest N TCs flagged as BottleneckEntry items, and the slowest N setup
//     phases flagged as SetupPhaseBottleneck items.
//
// An empty string (the default) disables all profile output.
// The E2E_PROFILE environment variable is used as a fallback when the flag is
// not explicitly set on the command line.
var e2eTimingReportFlag = flag.String(
	"e2e.profile",
	"",
	"file path for JSON-structured timing profile: per-TC duration, "+
		"per-phase breakdown (group-setup/tc-setup/tc-execute/tc-teardown/group-teardown), "+
		"and the slowest 5 TCs flagged as bottlenecks; empty string disables profiling "+
		"(env: E2E_PROFILE)",
)

// e2eProfileTopNFlag corresponds to -e2e.profile.top-n. Controls the number
// of slowest TCs (N) surfaced in the text timing summary, the
// ProfileReport.Bottlenecks list, and the ProfileReport.SlowSetupPhases list.
//
// A value of 0 means "use the default" (profileReportBottleneckLimit = 5 for
// the JSON bottleneck list, defaultTimingReportSlowSpecLimit = 10 for the text
// summary, and defaultSlowSetupPhaseLimit = 5 for slow setup phases).
// Negative values are treated as 0.
//
// The E2E_PROFILE_TOP_N environment variable is used as a fallback when the
// flag is not explicitly set.
var e2eProfileTopNFlag = flag.Int(
	"e2e.profile.top-n",
	0,
	"number of slowest TCs and setup phases to include in the profile "+
		"report (0 = default: 5 for JSON bottlenecks and setup phases, "+
		"10 for text summary); env: E2E_PROFILE_TOP_N",
)

// e2eSetupTimingLogFlag corresponds to -e2e.setup-timing-log (Sub-AC 6.2).
// When set to a non-empty file path the suite appends one JSON-Lines record
// per setup-phase event to the specified file during the run:
//
//   - BeforeSuite: one record per SynchronizedBeforeSuite / BeforeSuite node,
//     appended via the ReportAfterSuite hook when the consolidated report is available.
//   - BeforeEach:  one record per TC, appended after BeforeEach completes.
//   - JustBeforeEach: one record per TC, appended after JustBeforeEach completes.
//
// The file path must be under /tmp to satisfy the environment hygiene
// constraint (no files outside /tmp during the test lifecycle).
// An empty string (the default) disables setup-phase log output.
var e2eSetupTimingLogFlag = flag.String(
	"e2e.setup-timing-log",
	"",
	"file path for append-only JSON-Lines setup-phase timing log "+
		"(Sub-AC 6.2: BeforeSuite / BeforeEach / JustBeforeEach durations); "+
		"empty string disables the log",
)

// e2eDebugTCDurationFlag corresponds to -e2e.debug-tc-duration (Sub-AC 7a).
// When true the suite writes a one-line elapsed-time summary to stderr
// immediately after each TC completes, in the form:
//
//	[TC-E1.2] elapsed: 12.5ms
//
// The line is always written regardless of verbose mode or test outcome,
// making it straightforward to identify slow TCs during interactive runs.
//
// The E2E_DEBUG_TC_DURATION environment variable is used as a fallback when
// the flag is not explicitly set. Any non-empty env value enables the output.
var e2eDebugTCDurationFlag = flag.Bool(
	"e2e.debug-tc-duration",
	false,
	"print wall-clock duration for each TC to stderr upon completion "+
		"(Sub-AC 7a: format '[TC-<id>] elapsed: <duration>'); "+
		"env: E2E_TIMING_TC or E2E_DEBUG_TC_DURATION",
)

// e2eDebugTCStepsFlag corresponds to -e2e.debug-tc-steps (Sub-AC 7b).
// When true the suite writes a per-step timing breakdown to stderr immediately
// after each TC completes, showing setup, action, and teardown phase durations:
//
//	[TC-E1.2] steps: setup=12.3ms action=45.6ms teardown=8.9ms
//
// The line is always written regardless of verbose mode or test outcome.
// "setup" maps to the tc.setup.total phase, "action" to spec.body, and
// "teardown" to tc.teardown.total. When a phase was not instrumented its
// duration is shown as 0s.
//
// The E2E_DEBUG_TC_STEPS environment variable is used as a fallback when
// the flag is not explicitly set. Any non-empty env value enables the output.
var e2eDebugTCStepsFlag = flag.Bool(
	"e2e.debug-tc-steps",
	false,
	"print per-step timing breakdown for each TC to stderr upon completion "+
		"(Sub-AC 7b: format '[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>'); "+
		"env: E2E_TIMING_STEPS or E2E_DEBUG_TC_STEPS",
)

// e2eDebugPipelineFlag corresponds to -e2e.debug-pipeline (Sub-AC 7c).
// When true the suite renders a full end-to-end pipeline timeline to stderr
// after all TCs have completed. The timeline includes:
//   - Total pipeline wall-clock duration (first TC start → last TC finish).
//   - An ASCII Gantt chart showing which Ginkgo process ran TCs and when,
//     making parallelism and idle periods visually obvious.
//   - Per-TC execution list sorted by start time with queue-wait (time from
//     suite start to TC start), total duration, and setup/action/teardown
//     phase breakdown.
//   - Queue-wait statistics: min, max, and average across all TCs.
//
// The E2E_DEBUG_PIPELINE environment variable is used as a fallback when
// the flag is not explicitly set. Any non-empty env value enables the output.
var e2eDebugPipelineFlag = flag.Bool(
	"e2e.debug-pipeline",
	false,
	"print full end-to-end pipeline timeline to stderr after all TCs complete "+
		"(Sub-AC 7c: Gantt chart per process, queue-wait stats, parallelism summary); "+
		"env: E2E_TIMING_PIPELINE or E2E_DEBUG_PIPELINE",
)

// e2eProfileOutlierThresholdFlag corresponds to -e2e.profile.threshold (Sub-AC 3).
// It sets the percentage of suite runtime above which a TC is flagged as an
// outlier in the bottleneck summary table printed to stdout when -e2e.profile
// is set.
//
// The sentinel default (-1.0) indicates the flag was not set on the command
// line; in that case the E2E_PROFILE_THRESHOLD env var is checked, falling back
// to the built-in default of 10.0%.
//
// Setting the flag to 0.0 explicitly means "flag every TC with any runtime".
// Setting it to 100.0 effectively disables outlier flagging.
var e2eProfileOutlierThresholdFlag = flag.Float64(
	"e2e.profile.threshold",
	outlierThresholdFlagSentinel,
	"percentage of suite runtime above which a TC is flagged as an outlier "+
		"in the bottleneck summary table (Sub-AC 3: default 10.0); "+
		"use 0.0 to flag every TC, 100.0 to disable outlier flagging; "+
		"env: E2E_PROFILE_THRESHOLD",
)

// timingReportConfig bundles the runtime settings derived from the
// -e2e.profile, -e2e.profile.top-n, -e2e.setup-timing-log, and
// -e2e.debug-tc-duration flag values.
// It is populated once in configureSuiteExecution and then read-only for the
// remainder of the suite run.
type timingReportConfig struct {
	// Enabled is true when -e2e.profile (or E2E_PROFILE) was set to a
	// non-empty path on the command line.
	Enabled bool

	// ProfilePath is the destination file path for the JSON ProfileReport.
	// It is the non-empty value of the -e2e.profile flag (or E2E_PROFILE env
	// var); empty when disabled.
	ProfilePath string

	// Output is the io.Writer to which the human-readable text timing summary
	// is written. It is io.Discard when Enabled is false.
	Output io.Writer

	// SlowSpecLimit controls how many TCs appear in the text-format slow-spec
	// list. When -e2e.profile.top-n (or E2E_PROFILE_TOP_N) is non-zero it is
	// set to that value; otherwise it defaults to defaultTimingReportSlowSpecLimit.
	// It does not affect ProfileReport.Bottlenecks (controlled by BottleneckLimit).
	SlowSpecLimit int

	// BottleneckLimit is the number of TCs captured in ProfileReport.Bottlenecks.
	// When -e2e.profile.top-n (or E2E_PROFILE_TOP_N) is non-zero it is set to
	// that value; otherwise it defaults to profileReportBottleneckLimit (5).
	BottleneckLimit int

	// SlowSetupPhaseLimit is the number of slowest setup phases captured in
	// ProfileReport.SlowSetupPhases. Equals BottleneckLimit by default.
	SlowSetupPhaseLimit int

	// SetupTimingLogPath is the destination file path for the append-only
	// JSON-Lines setup-phase timing log (Sub-AC 6.2). Empty means disabled.
	SetupTimingLogPath string

	// DebugTCDuration is true when -e2e.debug-tc-duration (or
	// E2E_DEBUG_TC_DURATION) is set. When true, a one-line elapsed-time summary
	// is written to DebugWriter immediately after each TC completes.
	// Format: "[TC-<id>] elapsed: <duration>\n"
	DebugTCDuration bool

	// DebugWriter is the io.Writer to which per-TC elapsed-time lines are
	// written when DebugTCDuration is true. It defaults to os.Stderr.
	// It is io.Discard when DebugTCDuration is false.
	DebugWriter io.Writer

	// DebugTCSteps is true when -e2e.debug-tc-steps (or E2E_DEBUG_TC_STEPS)
	// is set. When true, a per-step timing breakdown is written to
	// DebugStepsWriter immediately after each TC completes.
	// Format: "[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>\n"
	DebugTCSteps bool

	// DebugStepsWriter is the io.Writer to which per-TC step timing lines are
	// written when DebugTCSteps is true. It defaults to os.Stderr.
	// It is io.Discard when DebugTCSteps is false.
	DebugStepsWriter io.Writer

	// DebugPipeline is true when -e2e.debug-pipeline (or E2E_DEBUG_PIPELINE)
	// is set. When true, a full end-to-end pipeline timeline is written to
	// DebugPipelineWriter after all TCs complete (in ReportAfterSuite).
	// The timeline shows an ASCII Gantt chart, per-TC queue wait and duration,
	// and queue-wait statistics.
	DebugPipeline bool

	// DebugPipelineWriter is the io.Writer to which the pipeline timeline is
	// written when DebugPipeline is true. It defaults to os.Stderr.
	// It is io.Discard when DebugPipeline is false.
	DebugPipelineWriter io.Writer

	// OutlierThresholdPct is the configurable percentage of suite runtime above
	// which a TC is flagged as an outlier in the bottleneck summary table
	// (Sub-AC 3). It is resolved from -e2e.profile.threshold, then
	// E2E_PROFILE_THRESHOLD, then the built-in default (10.0%).
	OutlierThresholdPct float64

	// SummaryWriter is the io.Writer for the stdout bottleneck summary table
	// (Sub-AC 3). It is os.Stdout when Enabled is true; io.Discard otherwise.
	// Tests may inspect this field to confirm wiring without capturing actual
	// stdout output.
	SummaryWriter io.Writer
}

// suiteExecutionConfig is the top-level runtime configuration snapshot used
// by the ReportAfterSuite hooks.
type suiteExecutionConfig struct {
	TimingReport timingReportConfig
}

var (
	suiteExecutionConfigMu sync.RWMutex
	suiteExecutionState    = suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Output:              io.Discard,
			SlowSpecLimit:       defaultTimingReportSlowSpecLimit,
			BottleneckLimit:     profileReportBottleneckLimit,
			SlowSetupPhaseLimit: defaultSlowSetupPhaseLimit,
			DebugWriter:         io.Discard,
			DebugStepsWriter:    io.Discard,
			DebugPipelineWriter: io.Discard,
			OutlierThresholdPct: defaultOutlierThresholdPct,
			SummaryWriter:       io.Discard,
		},
	}
)

// resolveProfilePath returns the effective profile path: the flag value takes
// precedence; if empty the E2E_PROFILE environment variable is used.
func resolveProfilePath() string {
	if v := *e2eTimingReportFlag; v != "" {
		return v
	}
	return os.Getenv(envProfilePath)
}

// resolveProfileTopN returns the effective N value for -e2e.profile.top-n.
// Priority: flag value > E2E_PROFILE_TOP_N env var > 0 (caller applies default).
// Negative values are clamped to 0.
func resolveProfileTopN() int {
	if n := *e2eProfileTopNFlag; n > 0 {
		return n
	}
	if env := os.Getenv(envProfileTopN); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// resolveDebugTCDuration returns true when per-TC elapsed-time output is
// enabled. Priority: -e2e.debug-tc-duration flag > E2E_TIMING_TC env var >
// E2E_DEBUG_TC_DURATION env var > false (disabled by default).
//
// E2E_TIMING_TC is the canonical AC-7 name; E2E_DEBUG_TC_DURATION is the
// legacy alias. Either variable independently enables the feature.
func resolveDebugTCDuration() bool {
	if *e2eDebugTCDurationFlag {
		return true
	}
	if os.Getenv(envTimingTC) != "" {
		return true
	}
	return os.Getenv(envDebugTCDuration) != ""
}

// resolveDebugTCSteps returns true when per-TC step timing output is enabled
// (Sub-AC 7b). Priority: -e2e.debug-tc-steps flag > E2E_TIMING_STEPS env var >
// E2E_DEBUG_TC_STEPS env var > false (disabled by default).
//
// E2E_TIMING_STEPS is the canonical AC-7 name; E2E_DEBUG_TC_STEPS is the
// legacy alias. Either variable independently enables the feature.
func resolveDebugTCSteps() bool {
	if *e2eDebugTCStepsFlag {
		return true
	}
	if os.Getenv(envTimingSteps) != "" {
		return true
	}
	return os.Getenv(envDebugTCSteps) != ""
}

// resolveDebugPipeline returns true when the end-to-end pipeline timeline output
// is enabled (Sub-AC 7c). Priority: -e2e.debug-pipeline flag >
// E2E_TIMING_PIPELINE env var > E2E_DEBUG_PIPELINE env var > false (disabled
// by default).
//
// E2E_TIMING_PIPELINE is the canonical AC-7 name; E2E_DEBUG_PIPELINE is the
// legacy alias. Either variable independently enables the feature.
func resolveDebugPipeline() bool {
	if *e2eDebugPipelineFlag {
		return true
	}
	if os.Getenv(envTimingPipeline) != "" {
		return true
	}
	return os.Getenv(envDebugPipeline) != ""
}

// resolveProfileOutlierThreshold returns the effective outlier threshold
// percentage for the bottleneck summary table (Sub-AC 3).
//
// Priority:
//  1. -e2e.profile.threshold flag when it has been set (i.e. is ≥ 0, not at
//     the sentinel value of -1.0).
//  2. E2E_PROFILE_THRESHOLD env var parsed as a float64 ≥ 0.
//  3. Built-in default: defaultOutlierThresholdPct (10.0).
func resolveProfileOutlierThreshold() float64 {
	if v := *e2eProfileOutlierThresholdFlag; v >= 0 {
		return v // flag was explicitly set on the command line
	}
	if env := os.Getenv(envProfileOutlierThreshold); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil && v >= 0 {
			return v
		}
	}
	return defaultOutlierThresholdPct
}

// configureSuiteExecution must be called from TestMain / TestE2E before
// RunSpecs. It snapshots the flag values and wires up the output destination.
// It also configures suiteSetupPhaseLog (Sub-AC 6.2) based on the
// -e2e.setup-timing-log flag value.
//
// Sub-AC 6.3: the effective N for bottleneck reporting is resolved via
// resolveProfileTopN, which checks -e2e.profile.top-n first, then the
// E2E_PROFILE_TOP_N env var. When N is zero the defaults are used:
//   - SlowSpecLimit:       defaultTimingReportSlowSpecLimit (10)
//   - BottleneckLimit:     profileReportBottleneckLimit     (5)
//   - SlowSetupPhaseLimit: defaultSlowSetupPhaseLimit       (5)
//
// The profile path is resolved via resolveProfilePath, which checks
// -e2e.profile first, then the E2E_PROFILE env var.
func configureSuiteExecution(stderr io.Writer) {
	suiteExecutionConfigMu.Lock()
	defer suiteExecutionConfigMu.Unlock()

	profilePath := resolveProfilePath()
	enabled := profilePath != ""

	output := io.Discard
	if enabled && stderr != nil {
		output = stderr
	}

	setupLogPath := *e2eSetupTimingLogFlag

	// Resolve the configurable N for bottleneck and slow-phase reporting.
	n := resolveProfileTopN()
	slowSpecLimit := defaultTimingReportSlowSpecLimit
	bottleneckLimit := profileReportBottleneckLimit
	slowSetupPhaseLimit := defaultSlowSetupPhaseLimit
	if n > 0 {
		slowSpecLimit = n
		bottleneckLimit = n
		slowSetupPhaseLimit = n
	}

	// Sub-AC 7a: resolve debug-tc-duration flag.
	debugTCDuration := resolveDebugTCDuration()
	debugWriter := io.Discard
	if debugTCDuration && stderr != nil {
		debugWriter = stderr
	}

	// Sub-AC 7b: resolve debug-tc-steps flag.
	debugTCSteps := resolveDebugTCSteps()
	debugStepsWriter := io.Discard
	if debugTCSteps && stderr != nil {
		debugStepsWriter = stderr
	}

	// Sub-AC 7c: resolve debug-pipeline flag.
	debugPipeline := resolveDebugPipeline()
	debugPipelineWriter := io.Discard
	if debugPipeline && stderr != nil {
		debugPipelineWriter = stderr
	}

	// Sub-AC 3: resolve outlier threshold and wire the stdout summary writer.
	// The summary writer is os.Stdout when profiling is enabled so the
	// bottleneck summary table always appears on stdout (not on the stderr
	// parameter, which is used for the human-readable text summary).
	outlierThreshold := resolveProfileOutlierThreshold()
	summaryWriter := io.Writer(io.Discard) //nolint:unconvert
	if enabled {
		summaryWriter = os.Stdout
	}

	suiteExecutionState = suiteExecutionConfig{
		TimingReport: timingReportConfig{
			Enabled:             enabled,
			ProfilePath:         profilePath,
			Output:              output,
			SlowSpecLimit:       slowSpecLimit,
			BottleneckLimit:     bottleneckLimit,
			SlowSetupPhaseLimit: slowSetupPhaseLimit,
			SetupTimingLogPath:  setupLogPath,
			DebugTCDuration:     debugTCDuration,
			DebugWriter:         debugWriter,
			DebugTCSteps:        debugTCSteps,
			DebugStepsWriter:    debugStepsWriter,
			DebugPipeline:       debugPipeline,
			DebugPipelineWriter: debugPipelineWriter,
			OutlierThresholdPct: outlierThreshold,
			SummaryWriter:       summaryWriter,
		},
	}

	// Wire up the package-level setup-phase log writer (Sub-AC 6.2).
	// newFileSetupPhaseLogger("") is a no-op logger when the path is empty,
	// so no file is created unless the flag is explicitly set.
	suiteSetupPhaseLog = newFileSetupPhaseLogger(setupLogPath)
}

// currentSuiteExecutionConfig returns a snapshot of the active suite
// execution config. Safe to call concurrently from multiple goroutines.
func currentSuiteExecutionConfig() suiteExecutionConfig {
	suiteExecutionConfigMu.RLock()
	defer suiteExecutionConfigMu.RUnlock()

	return suiteExecutionState
}
