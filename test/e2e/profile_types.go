package e2e

import (
	"encoding/json"
	"time"
)

// PhaseTimings captures wall-clock durations for each of the five named
// execution phases that bound a single test-case invocation.
//
// Phase definitions:
//
//	GroupSetup    – BeforeSuite / outermost BeforeAll shared by the TC group.
//	TCSetup       – Per-TC setup (scope allocation, baseline construction,
//	                seed functions). Corresponds to phaseSetupTotal.
//	TCExecute     – The spec body (It block). Corresponds to phaseSpecBody.
//	TCTeardown    – Per-TC teardown (resource cleanup, temp-dir removal).
//	                Corresponds to phaseTeardownTotal.
//	GroupTeardown – AfterSuite / outermost AfterAll shared by the TC group.
//
// All duration fields are stored as nanoseconds (int64) so they round-trip
// cleanly through JSON without floating-point precision loss.
type PhaseTimings struct {
	GroupSetupNanos    int64 `json:"groupSetupNanos"`
	TCSetupNanos       int64 `json:"tcSetupNanos"`
	TCExecuteNanos     int64 `json:"tcExecuteNanos"`
	TCTeardownNanos    int64 `json:"tcTeardownNanos"`
	GroupTeardownNanos int64 `json:"groupTeardownNanos"`
}

// GroupSetup returns the GroupSetup phase duration.
func (p PhaseTimings) GroupSetup() time.Duration { return time.Duration(p.GroupSetupNanos) }

// TCSetup returns the TCSetup phase duration.
func (p PhaseTimings) TCSetup() time.Duration { return time.Duration(p.TCSetupNanos) }

// TCExecute returns the TCExecute phase duration.
func (p PhaseTimings) TCExecute() time.Duration { return time.Duration(p.TCExecuteNanos) }

// TCTeardown returns the TCTeardown phase duration.
func (p PhaseTimings) TCTeardown() time.Duration { return time.Duration(p.TCTeardownNanos) }

// GroupTeardown returns the GroupTeardown phase duration.
func (p PhaseTimings) GroupTeardown() time.Duration { return time.Duration(p.GroupTeardownNanos) }

// TCProfile holds the timing profile for a single test case.
// It is the per-TC element of ProfileReport.TCs.
type TCProfile struct {
	// TCID is the canonical TC identifier as it appears in docs/testing/{COMPONENT,INTEGRATION,E2E}-TESTS.md
	// (e.g. "E1.2", "F27.1"). It matches the [TC-<TCID>] Ginkgo node label.
	TCID string `json:"tcID"`

	// Category is the spec-document category string (e.g. "Type-E", "Type-F").
	Category string `json:"category"`

	// TestName is the human-readable test name from the Ginkgo leaf node.
	TestName string `json:"testName"`

	// Passed is true when the spec completed with SpecStatePassed; false for
	// any non-passing outcome (failed, panicked, timed out, skipped, pending).
	// This field provides per-TC pass/fail traceability in the JSON report so
	// consumers can correlate timing data with test outcomes without re-parsing
	// the full Ginkgo JSON report.
	Passed bool `json:"passed"`

	// TotalNanos is the wall-clock duration of the entire TC invocation in
	// nanoseconds, spanning from BeforeEach start to AfterEach end.
	TotalNanos int64 `json:"totalNanos"`

	// Phases breaks the total duration into the five named sub-phases.
	Phases PhaseTimings `json:"phases"`
}

// TotalDuration returns the total TC duration as a time.Duration.
func (p TCProfile) TotalDuration() time.Duration { return time.Duration(p.TotalNanos) }

// BottleneckEntry identifies a TC that was flagged as a suite-runtime
// bottleneck. The ProfileReport includes the slowest five TCs as
// bottleneck entries, ordered by TotalNanos descending.
type BottleneckEntry struct {
	// Rank is the 1-based position in the slowest-N list (1 = slowest).
	Rank int `json:"rank"`

	// TCID identifies the slow TC.
	TCID string `json:"tcID"`

	// TotalNanos is the TC's total wall-clock duration in nanoseconds.
	TotalNanos int64 `json:"totalNanos"`

	// PctOfSuiteRuntime is the percentage of the total suite runtime this TC
	// consumed, expressed as a value in [0, 100]. It is 0 when the suite
	// runtime baseline is unavailable.
	PctOfSuiteRuntime float64 `json:"pctOfSuiteRuntime"`
}

// TotalDuration returns the bottleneck TC duration as a time.Duration.
func (b BottleneckEntry) TotalDuration() time.Duration { return time.Duration(b.TotalNanos) }

// SetupPhaseBottleneck identifies a suite setup or teardown phase that was
// flagged as a bottleneck.  The ProfileReport includes the slowest N setup
// phases as SetupPhaseBottleneck entries, ordered by TotalNanos descending.
//
// Sources of setup phase data:
//   - "before_suite": BeforeSuite / SynchronizedBeforeSuite spec report.
//   - "after_suite":  AfterSuite / SynchronizedAfterSuite spec report.
//   - "tc_setup":     Per-TC setup phase extracted from the tc_timing report
//     entry (written by timing_capture.go BeforeEach instrumentation).
type SetupPhaseBottleneck struct {
	// Rank is the 1-based position in the slowest-N setup phase list (1 = slowest).
	Rank int `json:"rank"`

	// Phase identifies the Ginkgo hook that produced this entry.
	// One of: "before_suite", "after_suite", "tc_setup".
	Phase string `json:"phase"`

	// TCID is the test-case identifier for per-TC setup phases.
	// Empty for suite-level phases (before_suite, after_suite).
	TCID string `json:"tcID,omitempty"`

	// TotalNanos is the wall-clock duration of the setup phase in nanoseconds.
	TotalNanos int64 `json:"totalNanos"`

	// PctOfSuiteRuntime is the percentage of the total suite runtime this
	// phase consumed, expressed as a value in [0, 100].
	PctOfSuiteRuntime float64 `json:"pctOfSuiteRuntime"`
}

// TotalDuration returns the setup phase duration as a time.Duration.
func (s SetupPhaseBottleneck) TotalDuration() time.Duration {
	return time.Duration(s.TotalNanos)
}

// ProfileReport is the top-level JSON document emitted to stderr (and
// optionally to a file) when the -e2e.profile flag is set.
//
// The report is a single JSON object followed by a newline:
//
//	{"suiteName":"...","totalSpecs":437,...}
//
// Consumers should unmarshal using json.Unmarshal or json.NewDecoder.
type ProfileReport struct {
	// SuiteName is the Ginkgo suite description string.
	SuiteName string `json:"suiteName"`

	// TotalSpecs is the number of specs in the compiled test binary.
	TotalSpecs int `json:"totalSpecs"`

	// SelectedSpecs is the number of specs that matched the active label
	// filter and were scheduled to run.
	SelectedSpecs int `json:"selectedSpecs"`

	// SuiteRuntimeNanos is the total wall-clock duration of the suite run in
	// nanoseconds, as reported by the Ginkgo runner.
	SuiteRuntimeNanos int64 `json:"suiteRuntimeNanos"`

	// GeneratedAt is the UTC timestamp at which the report was assembled.
	GeneratedAt time.Time `json:"generatedAt"`

	// TCs holds one entry per TC that completed during this run. TCs without
	// timing instrumentation (no tc_timing report entry) are omitted.
	TCs []TCProfile `json:"tcs,omitempty"`

	// Bottlenecks lists the slowest N TCs ordered by TotalNanos descending.
	// N is controlled by -e2e.profile.top-n (default 5).
	// The list may be shorter when fewer than N TCs were instrumented.
	Bottlenecks []BottleneckEntry `json:"bottlenecks,omitempty"`

	// SlowSetupPhases lists the slowest N setup phases (BeforeSuite,
	// AfterSuite, per-TC setup) ordered by TotalNanos descending.
	// N is controlled by -e2e.profile.top-n (default 5).
	// The list may be shorter when fewer than N setup phases are available.
	SlowSetupPhases []SetupPhaseBottleneck `json:"slowSetupPhases,omitempty"`
}

// SuiteRuntime returns the suite run duration as a time.Duration.
func (r ProfileReport) SuiteRuntime() time.Duration {
	return time.Duration(r.SuiteRuntimeNanos)
}

// MarshalJSON implements json.Marshaler so that ProfileReport is always
// serialized as a compact single-line JSON object without a trailing newline.
// Callers that want a newline-terminated record should use EncodeProfileReport.
func (r ProfileReport) MarshalJSON() ([]byte, error) {
	// Use an alias to avoid infinite recursion while still invoking the
	// standard struct marshaler.
	type alias ProfileReport
	return json.Marshal(alias(r))
}

// EncodeProfileReport serialises r as a single compact JSON line (terminated
// by "\n") and writes it to out. It is the canonical write path used by the
// suite's ReportAfterSuite hook when -e2e.profile is set.
func EncodeProfileReport(out interface{ Write([]byte) (int, error) }, r ProfileReport) error {
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = out.Write(payload)
	return err
}
