package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

const timingReportEntryName = "tc_timing"

// timingElapsedEntryName is the Ginkgo report entry name for the human-readable
// per-TC elapsed time summary (AC 6.1). The entry is emitted with
// ReportEntryVisibilityFailureOrVerbose so it appears in -v output and on
// failure without cluttering normal passing output.
const timingElapsedEntryName = "tc_elapsed"

type executionPhase string

const (
	// phaseBeforeEach measures the wall-clock duration of the Ginkgo BeforeEach
	// phase for a single TC, spanning from when timing_capture.go's BeforeEach
	// hook fires to when timing_capture.go's JustBeforeEach hook fires.  This
	// includes all per-TC setup work (scope allocation, baseline construction,
	// seed functions) because those run in nested BeforeEach blocks that execute
	// after timing_capture.go's BeforeEach but before JustBeforeEach.
	phaseBeforeEach executionPhase = "hook.before_each"

	// phaseJustBeforeEach measures the wall-clock duration of the Ginkgo
	// JustBeforeEach phase for a single TC, spanning from when
	// timing_capture.go's JustBeforeEach hook fires to when the spec body
	// (phaseSpecBody) starts.  In the current suite this captures
	// timing_capture.go's own JustBeforeEach overhead (typically sub-
	// microsecond) plus any spec-registered JustBeforeEach blocks whose
	// Ginkgo execution order places them before timing_capture.go's hook.
	phaseJustBeforeEach executionPhase = "hook.just_before_each"

	phaseSpecBody            executionPhase = "spec.body"
	phaseSetupTotal          executionPhase = "tc.setup.total"
	phaseSetupScope          executionPhase = "tc.setup.scope"
	phaseSetupCallback       executionPhase = "tc.setup.callback"
	phaseSetupBaselineTotal  executionPhase = "tc.setup.baseline.total"
	phaseSetupTempDirs       executionPhase = "tc.setup.baseline.tempdirs"
	phaseSetupKubeconfigs    executionPhase = "tc.setup.baseline.kubeconfigs"
	phaseSetupBackendObjects executionPhase = "tc.setup.baseline.backendobjects"
	phaseSetupLoopbackPorts  executionPhase = "tc.setup.baseline.loopbackports"
	phaseSetupSeed           executionPhase = "tc.setup.baseline.seed"
	phaseTeardownTotal       executionPhase = "tc.teardown.total"
	phaseTeardownResources   executionPhase = "tc.teardown.tracked_resources"
	phaseTeardownPortLeases  executionPhase = "tc.teardown.port_leases"
	phaseTeardownRootDir     executionPhase = "tc.teardown.root_dir"
)

type phaseTimingSample struct {
	Name          string    `json:"name"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
	DurationNanos int64     `json:"durationNanos"`
}

func (s phaseTimingSample) Duration() time.Duration {
	return time.Duration(s.DurationNanos)
}

type testCaseTimingProfile struct {
	TCID            string              `json:"tcID,omitempty"`
	TestName        string              `json:"testName,omitempty"`
	SpecText        string              `json:"specText,omitempty"`
	ParallelProcess int                 `json:"parallelProcess,omitempty"`
	StartedAt       time.Time           `json:"startedAt"`
	FinishedAt      time.Time           `json:"finishedAt"`
	TotalNanos      int64               `json:"totalNanos"`
	Phases          []phaseTimingSample `json:"phases,omitempty"`
}

func (p testCaseTimingProfile) TotalDuration() time.Duration {
	return time.Duration(p.TotalNanos)
}

func (p testCaseTimingProfile) encode() (string, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func decodeTimingProfile(raw string) (testCaseTimingProfile, error) {
	var profile testCaseTimingProfile
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return testCaseTimingProfile{}, err
	}
	return profile, nil
}

func timingProfileFromReportEntries(entries types.ReportEntries) (testCaseTimingProfile, bool, error) {
	raw, ok := reportEntryValue(entries, timingReportEntryName)
	if !ok {
		return testCaseTimingProfile{}, false, nil
	}

	profile, err := decodeTimingProfile(raw)
	if err != nil {
		return testCaseTimingProfile{}, true, err
	}
	return profile, true, nil
}

type activeTimingProfile struct {
	profile   testCaseTimingProfile
	inFlight  map[string]time.Time
	completed []phaseTimingSample
}

type timingRecorder struct {
	mu      sync.Mutex
	now     func() time.Time
	current *activeTimingProfile
}

func newSuiteTimingRecorder(now func() time.Time) *timingRecorder {
	if now == nil {
		now = time.Now
	}

	return &timingRecorder{now: now}
}

func (r *timingRecorder) start(report types.SpecReport) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if report.LeafNodeType != types.NodeTypeIt {
		r.current = nil
		return
	}

	tcID, testName := inferTimingIdentity(report.LeafNodeText)
	startedAt := r.now()
	if !report.StartTime.IsZero() {
		startedAt = report.StartTime
	}

	r.current = &activeTimingProfile{
		profile: testCaseTimingProfile{
			TCID:            tcID,
			TestName:        testName,
			SpecText:        strings.TrimSpace(report.LeafNodeText),
			ParallelProcess: report.ParallelProcess,
			StartedAt:       startedAt,
		},
		inFlight: make(map[string]time.Time),
	}
}

func (r *timingRecorder) beginPhase(phase executionPhase) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current == nil {
		return
	}

	r.current.inFlight[string(phase)] = r.now()
}

func (r *timingRecorder) endPhase(phase executionPhase) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current == nil {
		return
	}

	key := string(phase)
	startedAt, ok := r.current.inFlight[key]
	if !ok {
		return
	}
	delete(r.current.inFlight, key)

	finishedAt := r.now()
	r.current.completed = append(r.current.completed, newPhaseTimingSample(key, startedAt, finishedAt))
}

func (r *timingRecorder) measureErr(phase executionPhase, fn func() error) error {
	if r == nil || fn == nil {
		return nil
	}

	r.beginPhase(phase)
	defer r.endPhase(phase)

	return fn()
}

func (r *timingRecorder) finalize(report types.SpecReport) (testCaseTimingProfile, bool) {
	if r == nil {
		return testCaseTimingProfile{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current == nil {
		return testCaseTimingProfile{}, false
	}

	finishedAt := r.now()
	for name, startedAt := range r.current.inFlight {
		r.current.completed = append(r.current.completed, newPhaseTimingSample(name, startedAt, finishedAt))
	}

	profile := r.current.profile
	profile.Phases = append(profile.Phases, r.current.completed...)
	profile.FinishedAt = finishedAt
	profile.TotalNanos = finishedAt.Sub(profile.StartedAt).Nanoseconds()

	if profile.TCID == "" {
		if tcID, ok := reportEntryValue(report.ReportEntries, "tc_id"); ok {
			profile.TCID = tcID
		}
	}
	if profile.TestName == "" {
		if testName, ok := reportEntryValue(report.ReportEntries, "tc_test_name"); ok {
			profile.TestName = testName
		}
	}
	if profile.TestName == "" {
		_, profile.TestName = inferTimingIdentity(profile.SpecText)
	}
	if profile.ParallelProcess == 0 {
		profile.ParallelProcess = report.ParallelProcess
	}
	if profile.SpecText == "" {
		profile.SpecText = strings.TrimSpace(report.LeafNodeText)
	}

	r.current = nil
	return profile, true
}

func newPhaseTimingSample(name string, startedAt, finishedAt time.Time) phaseTimingSample {
	return phaseTimingSample{
		Name:          name,
		StartedAt:     startedAt,
		FinishedAt:    finishedAt,
		DurationNanos: finishedAt.Sub(startedAt).Nanoseconds(),
	}
}

func inferTimingIdentity(specText string) (string, string) {
	specText = strings.TrimSpace(specText)
	if specText == "" {
		return "", ""
	}

	parts := strings.SplitN(specText, "::", 2)
	if len(parts) != 2 {
		return "", specText
	}

	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])

	if idx := strings.LastIndex(left, "]"); idx >= 0 {
		left = strings.TrimSpace(left[idx+1:])
	}

	return left, right
}

var suiteTimingRecorder = newSuiteTimingRecorder(time.Now)

func measureTimingPhaseErr(phase executionPhase, fn func() error) error {
	return suiteTimingRecorder.measureErr(phase, fn)
}

func measureTimingPhaseValue[T any](phase executionPhase, fn func() (T, error)) (T, error) {
	var zero T
	if fn == nil {
		return zero, nil
	}

	suiteTimingRecorder.beginPhase(phase)
	defer suiteTimingRecorder.endPhase(phase)

	return fn()
}

// formatElapsedEntry returns the human-readable elapsed time string that is
// emitted as the "tc_elapsed" Ginkgo report entry value for AC 6.1.
//
// Format: "[TC-{tcID}] elapsed: {duration}"
// Example: "[TC-E1.2] elapsed: 12.5ms"
func formatElapsedEntry(tcID string, totalNanos int64) string {
	return fmt.Sprintf("[TC-%s] elapsed: %s", tcID, time.Duration(totalNanos))
}

func emitCurrentTimingReportEntry(report types.SpecReport) {
	profile, ok := suiteTimingRecorder.finalize(report)
	if !ok {
		return
	}

	payload, err := profile.encode()
	if err != nil {
		AddReportEntry("tc_timing_encode_error", err.Error(), types.ReportEntryVisibilityNever)
		return
	}

	// Always emit the full timing profile (hidden from default output; accessible
	// via Ginkgo's --json-report and programmatically via ReportAfterEach).
	AddReportEntry(timingReportEntryName, payload, types.ReportEntryVisibilityNever)

	// AC 6.1: emit elapsed time in the Ginkgo v2 report so timing data is
	// visible when running with -v or on failure, enabling bottleneck
	// identification without requiring the separate JSON profile file.
	// ReportEntryVisibilityFailureOrVerbose keeps normal passing output clean
	// while surfacing timing data exactly when it matters.
	if profile.TCID != "" {
		AddReportEntry(
			timingElapsedEntryName,
			formatElapsedEntry(profile.TCID, profile.TotalNanos),
			types.ReportEntryVisibilityFailureOrVerbose,
		)
	}

	// Sub-AC 7a: when -e2e.debug-tc-duration (or E2E_DEBUG_TC_DURATION) is set,
	// write the elapsed time to stderr immediately so it is always visible,
	// regardless of verbose mode or test outcome.
	emitDebugTCDurationLine(profile, currentSuiteExecutionConfig().TimingReport)

	// Sub-AC 7b: when -e2e.debug-tc-steps (or E2E_DEBUG_TC_STEPS) is set,
	// write the per-step timing breakdown to stderr immediately.
	emitDebugTCStepsLines(profile, currentSuiteExecutionConfig().TimingReport)

	// AC 6.2: append BeforeEach and JustBeforeEach phase durations to the
	// structured setup-phase timing log. Errors are silently discarded so that
	// a logging failure never causes a TC to fail.
	appendSetupPhasesFromTimingProfile(profile)
}

// appendSetupPhasesFromTimingProfile reads the phaseBeforeEach and
// phaseJustBeforeEach samples from profile and appends them to the package-
// level suiteSetupPhaseLog (Sub-AC 6.2). A missing or empty TCID is not an
// error — the entry is appended with an empty tcID, which the log consumer
// can filter.
func appendSetupPhasesFromTimingProfile(profile testCaseTimingProfile) {
	for _, sample := range profile.Phases {
		switch executionPhase(sample.Name) {
		case phaseBeforeEach:
			appendSetupPhaseEntry(setupPhaseLogEntry{
				Phase:           setupPhaseBeforeEach,
				TCID:            profile.TCID,
				ParallelProcess: profile.ParallelProcess,
				StartedAt:       sample.StartedAt,
				FinishedAt:      sample.FinishedAt,
				DurationNanos:   sample.DurationNanos,
			})
		case phaseJustBeforeEach:
			appendSetupPhaseEntry(setupPhaseLogEntry{
				Phase:           setupPhaseJustBeforeEach,
				TCID:            profile.TCID,
				ParallelProcess: profile.ParallelProcess,
				StartedAt:       sample.StartedAt,
				FinishedAt:      sample.FinishedAt,
				DurationNanos:   sample.DurationNanos,
			})
		}
	}
}

var _ = BeforeEach(func() {
	suiteTimingRecorder.start(CurrentSpecReport())
	// AC 6.2: begin the BeforeEach phase so we can measure how long all
	// BeforeEach blocks (including per-TC setup) take before JustBeforeEach fires.
	suiteTimingRecorder.beginPhase(phaseBeforeEach)
	DeferCleanup(func() {
		emitCurrentTimingReportEntry(CurrentSpecReport())
	})
})

var _ = JustBeforeEach(func() {
	// AC 6.2: end the BeforeEach phase (started above) and measure the very
	// brief JustBeforeEach phase (from JustBeforeEach start to spec body start).
	// phaseBeforeEach spans all BeforeEach work; phaseJustBeforeEach captures
	// this hook's own overhead plus any JustBeforeEach work that runs before
	// the spec body begins.
	suiteTimingRecorder.endPhase(phaseBeforeEach)
	suiteTimingRecorder.beginPhase(phaseJustBeforeEach)
	suiteTimingRecorder.endPhase(phaseJustBeforeEach)
	suiteTimingRecorder.beginPhase(phaseSpecBody)
})

var _ = JustAfterEach(func() {
	suiteTimingRecorder.endPhase(phaseSpecBody)
})

// emitDebugTCDurationLine writes a one-line elapsed-time summary to
// cfg.DebugWriter when cfg.DebugTCDuration is true (Sub-AC 7a).
//
// Format: "[TC-<label>] elapsed: <duration>\n"
//
// When profile.TCID is non-empty it is used as the label; otherwise
// profile.SpecText is the fallback so that TCs without structured IDs also
// appear in the stream. When both are empty, nothing is written.
//
// This function is intentionally free of Ginkgo DSL calls so that it can
// be unit-tested from plain Go tests without a Ginkgo suite context.
func emitDebugTCDurationLine(profile testCaseTimingProfile, cfg timingReportConfig) {
	if !cfg.DebugTCDuration {
		return
	}
	label := profile.TCID
	if label == "" {
		label = profile.SpecText
	}
	if label == "" {
		return
	}
	_, _ = fmt.Fprintf(cfg.DebugWriter, "[TC-%s] elapsed: %s\n",
		label, time.Duration(profile.TotalNanos))
}

// emitDebugTCStepsLines writes a per-step timing breakdown to
// cfg.DebugStepsWriter when cfg.DebugTCSteps is true (Sub-AC 7b).
//
// Format: "[TC-<label>] steps: setup=<dur> action=<dur> teardown=<dur>\n"
//
// Phase mapping:
//   - setup    ← tc.setup.total (falls back to hook.before_each when absent)
//   - action   ← spec.body (the It block; encompasses action + assertions)
//   - teardown ← tc.teardown.total
//
// When a phase was not instrumented its duration is reported as 0s.
// When profile.TCID is non-empty it is used as the label; otherwise
// profile.SpecText is the fallback. When both are empty, nothing is written.
//
// This function is intentionally free of Ginkgo DSL calls so that it can
// be unit-tested from plain Go tests without a Ginkgo suite context.
func emitDebugTCStepsLines(profile testCaseTimingProfile, cfg timingReportConfig) {
	if !cfg.DebugTCSteps {
		return
	}
	label := profile.TCID
	if label == "" {
		label = profile.SpecText
	}
	if label == "" {
		return
	}

	setup, action, teardown := extractStepDurations(profile)
	_, _ = fmt.Fprintf(cfg.DebugStepsWriter,
		"[TC-%s] steps: setup=%s action=%s teardown=%s\n",
		label, setup, action, teardown)
}

// extractStepDurations scans profile.Phases and returns the three canonical
// step durations used by emitDebugTCStepsLines (Sub-AC 7b):
//
//   - setup    ← tc.setup.total; falls back to hook.before_each when absent
//   - action   ← spec.body (the It block, which encompasses action + assertions)
//   - teardown ← tc.teardown.total
//
// Each returned duration is zero when the corresponding phase was not
// captured. This function is pure and dependency-free so it can be unit-tested
// without a Ginkgo suite context.
func extractStepDurations(profile testCaseTimingProfile) (setup, action, teardown time.Duration) {
	var beforeEachNanos int64
	for _, s := range profile.Phases {
		switch executionPhase(s.Name) {
		case phaseSetupTotal:
			setup = time.Duration(s.DurationNanos)
		case phaseSpecBody:
			action = time.Duration(s.DurationNanos)
		case phaseTeardownTotal:
			teardown = time.Duration(s.DurationNanos)
		case phaseBeforeEach:
			beforeEachNanos = s.DurationNanos
		}
	}
	// Fall back to hook.before_each when tc.setup.total was not recorded
	// (e.g. specs that bypass UsePerTestCaseSetup).
	if setup == 0 && beforeEachNanos > 0 {
		setup = time.Duration(beforeEachNanos)
	}
	return setup, action, teardown
}
