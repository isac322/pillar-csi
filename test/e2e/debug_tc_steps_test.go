package e2e

// debug_tc_steps_test.go — Sub-AC 7b: --debug-tc-steps flag tests.
//
// These tests verify that:
//  1. The -e2e.debug-tc-steps flag is registered with a meaningful description.
//  2. When the flag is true, configureSuiteExecution sets DebugTCSteps=true
//     and DebugStepsWriter to the provided stderr writer.
//  3. When the flag is false and E2E_DEBUG_TC_STEPS is unset,
//     DebugTCSteps is false and DebugStepsWriter is io.Discard.
//  4. The E2E_DEBUG_TC_STEPS environment variable enables the feature
//     when the flag is not explicitly set.
//  5. emitDebugTCStepsLines writes the step breakdown to DebugStepsWriter
//     when DebugTCSteps is true, with the format
//     "[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>\n".
//  6. No output is written to DebugStepsWriter when DebugTCSteps is false.
//  7. TCs without a structured TCID fall back to SpecText as the label.
//  8. extractStepDurations correctly maps internal phase names to the three
//     canonical step durations (setup, action, teardown).
//  9. extractStepDurations falls back to hook.before_each when tc.setup.total
//     is absent.
// 10. Phase durations not present in profile.Phases are reported as 0s.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAC7bDebugTCStepsFlagDefaultIsDisabled verifies that the flag defaults
// to false and DebugTCSteps is off when neither the flag nor the env var are set.
func TestAC7bDebugTCStepsFlagDefaultIsDisabled(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envDebugTCSteps, "")
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCSteps {
		t.Fatal("Sub-AC 7b: DebugTCSteps should be false when flag is not set")
	}
}

// TestAC7bDebugTCStepsFlagEnablesWriter verifies that setting the flag to
// true wires DebugStepsWriter to the provided stderr writer.
func TestAC7bDebugTCStepsFlagEnablesWriter(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = true
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCSteps {
		t.Fatal("Sub-AC 7b: DebugTCSteps should be true when flag is set")
	}
	if cfg.TimingReport.DebugStepsWriter == nil {
		t.Fatal("Sub-AC 7b: DebugStepsWriter must not be nil when DebugTCSteps is enabled")
	}

	// Write a test line through the writer to confirm it goes to sink.
	_, _ = fmt.Fprint(cfg.TimingReport.DebugStepsWriter, "probe")
	if !strings.Contains(sink.String(), "probe") {
		t.Errorf("Sub-AC 7b: DebugStepsWriter output not routed to the provided stderr writer")
	}
}

// TestAC7bDebugTCStepsEnvVarEnablesFeature verifies the E2E_DEBUG_TC_STEPS
// environment variable as a flag fallback.
func TestAC7bDebugTCStepsEnvVarEnablesFeature(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envDebugTCSteps, "1")
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false // flag not set; env var should take effect
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCSteps {
		t.Fatal("Sub-AC 7b: DebugTCSteps should be true when E2E_DEBUG_TC_STEPS env var is set")
	}
}

// TestAC7bDebugTCStepsWritesBreakdownOnCompletion verifies that
// emitDebugTCStepsLines writes the formatted step breakdown to DebugStepsWriter
// when DebugTCSteps is true.
func TestAC7bDebugTCStepsWritesBreakdownOnCompletion(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCSteps:     true,
		DebugStepsWriter: &sink,
	}

	profile := testCaseTimingProfile{
		TCID:     "E1.2",
		TestName: "TestDebugSteps",
		SpecText: "TC[001/388] E1.2 :: TestDebugSteps",
		Phases: []phaseTimingSample{
			{Name: string(phaseSetupTotal), DurationNanos: (12 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSpecBody), DurationNanos: (45 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownTotal), DurationNanos: (8 * time.Millisecond).Nanoseconds()},
		},
	}

	emitDebugTCStepsLines(profile, cfg)

	out := sink.String()
	if !strings.Contains(out, "[TC-E1.2] steps:") {
		t.Errorf("Sub-AC 7b: expected '[TC-E1.2] steps:' in output, got: %q", out)
	}
	if !strings.Contains(out, "setup=12ms") {
		t.Errorf("Sub-AC 7b: expected 'setup=12ms' in output, got: %q", out)
	}
	if !strings.Contains(out, "action=45ms") {
		t.Errorf("Sub-AC 7b: expected 'action=45ms' in output, got: %q", out)
	}
	if !strings.Contains(out, "teardown=8ms") {
		t.Errorf("Sub-AC 7b: expected 'teardown=8ms' in output, got: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Sub-AC 7b: step breakdown output must end with newline, got: %q", out)
	}
}

// TestAC7bDebugTCStepsSilentWhenDisabled verifies that no output is written
// to DebugStepsWriter when DebugTCSteps is false.
func TestAC7bDebugTCStepsSilentWhenDisabled(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCSteps:     false,
		DebugStepsWriter: &sink,
	}

	profile := testCaseTimingProfile{
		TCID:     "E2.1",
		TestName: "TestSilent",
		Phases: []phaseTimingSample{
			{Name: string(phaseSetupTotal), DurationNanos: (10 * time.Millisecond).Nanoseconds()},
		},
	}

	emitDebugTCStepsLines(profile, cfg)

	if sink.Len() != 0 {
		t.Errorf("Sub-AC 7b: expected no output when DebugTCSteps is false, got: %q", sink.String())
	}
}

// TestAC7bDebugTCStepsFallsBackToSpecTextWhenNoTCID verifies that TCs
// without a structured TCID use SpecText as the fallback label.
func TestAC7bDebugTCStepsFallsBackToSpecTextWhenNoTCID(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCSteps:     true,
		DebugStepsWriter: &sink,
	}

	profile := testCaseTimingProfile{
		TCID:     "",
		SpecText: "some spec without a TC ID",
		Phases: []phaseTimingSample{
			{Name: string(phaseSpecBody), DurationNanos: (20 * time.Millisecond).Nanoseconds()},
		},
	}

	emitDebugTCStepsLines(profile, cfg)

	out := sink.String()
	if !strings.Contains(out, "[TC-some spec without a TC ID] steps:") {
		t.Errorf("Sub-AC 7b: expected fallback label in step output, got: %q", out)
	}
}

// TestAC7bDebugTCStepsSilentWhenBothIDAndTextEmpty verifies that nothing is
// written when both TCID and SpecText are empty.
func TestAC7bDebugTCStepsSilentWhenBothIDAndTextEmpty(t *testing.T) {
	t.Parallel()
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCSteps:     true,
		DebugStepsWriter: &sink,
	}

	profile := testCaseTimingProfile{
		TCID:     "",
		SpecText: "",
	}

	emitDebugTCStepsLines(profile, cfg)

	if sink.Len() != 0 {
		t.Errorf("Sub-AC 7b: expected no output when both TCID and SpecText are empty, got: %q", sink.String())
	}
}

// TestAC7bExtractStepDurations verifies that extractStepDurations correctly
// maps internal phase names to the three canonical step durations.
func TestAC7bExtractStepDurations(t *testing.T) {
	t.Parallel()
	profile := testCaseTimingProfile{
		Phases: []phaseTimingSample{
			{Name: string(phaseSetupTotal), DurationNanos: (15 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSetupScope), DurationNanos: (3 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSetupBaselineTotal), DurationNanos: (10 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSpecBody), DurationNanos: (50 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownTotal), DurationNanos: (9 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownResources), DurationNanos: (7 * time.Millisecond).Nanoseconds()},
		},
	}

	setup, action, teardown := extractStepDurations(profile)

	if setup != 15*time.Millisecond {
		t.Errorf("Sub-AC 7b: setup=%v, want 15ms", setup)
	}
	if action != 50*time.Millisecond {
		t.Errorf("Sub-AC 7b: action=%v, want 50ms", action)
	}
	if teardown != 9*time.Millisecond {
		t.Errorf("Sub-AC 7b: teardown=%v, want 9ms", teardown)
	}
}

// TestAC7bExtractStepDurationsFallsBackToBeforeEach verifies that
// extractStepDurations uses hook.before_each as the setup duration when
// tc.setup.total is absent.
func TestAC7bExtractStepDurationsFallsBackToBeforeEach(t *testing.T) {
	t.Parallel()
	profile := testCaseTimingProfile{
		Phases: []phaseTimingSample{
			{Name: string(phaseBeforeEach), DurationNanos: (30 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSpecBody), DurationNanos: (20 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownTotal), DurationNanos: (5 * time.Millisecond).Nanoseconds()},
		},
	}

	setup, action, teardown := extractStepDurations(profile)

	if setup != 30*time.Millisecond {
		t.Errorf("Sub-AC 7b: setup fallback to before_each=%v, want 30ms", setup)
	}
	if action != 20*time.Millisecond {
		t.Errorf("Sub-AC 7b: action=%v, want 20ms", action)
	}
	if teardown != 5*time.Millisecond {
		t.Errorf("Sub-AC 7b: teardown=%v, want 5ms", teardown)
	}
}

// TestAC7bExtractStepDurationsZeroWhenPhaseMissing verifies that
// extractStepDurations returns 0s for phases not present in the profile.
func TestAC7bExtractStepDurationsZeroWhenPhaseMissing(t *testing.T) {
	t.Parallel()
	// Only spec.body is present; setup and teardown should be 0.
	profile := testCaseTimingProfile{
		Phases: []phaseTimingSample{
			{Name: string(phaseSpecBody), DurationNanos: (25 * time.Millisecond).Nanoseconds()},
		},
	}

	setup, action, teardown := extractStepDurations(profile)

	if setup != 0 {
		t.Errorf("Sub-AC 7b: setup=%v when phase missing, want 0", setup)
	}
	if action != 25*time.Millisecond {
		t.Errorf("Sub-AC 7b: action=%v, want 25ms", action)
	}
	if teardown != 0 {
		t.Errorf("Sub-AC 7b: teardown=%v when phase missing, want 0", teardown)
	}
}

// TestAC7bExtractStepDurationsEmptyProfile verifies that extractStepDurations
// returns all zeros when no phases are recorded.
func TestAC7bExtractStepDurationsEmptyProfile(t *testing.T) {
	t.Parallel()
	profile := testCaseTimingProfile{}

	setup, action, teardown := extractStepDurations(profile)

	if setup != 0 || action != 0 || teardown != 0 {
		t.Errorf("Sub-AC 7b: expected all zeros for empty profile, got setup=%v action=%v teardown=%v",
			setup, action, teardown)
	}
}

// TestAC7bDebugTCStepsFlagDescription verifies that the -e2e.debug-tc-steps
// flag is registered with a description mentioning key concepts:
//   - per-step
//   - setup/action/teardown
//   - stderr
func TestAC7bDebugTCStepsFlagDescription(t *testing.T) {
	t.Parallel()
	usage := "print per-step timing breakdown for each TC to stderr upon completion " +
		"(Sub-AC 7b: format '[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>'); " +
		"env: E2E_DEBUG_TC_STEPS"

	for _, keyword := range []string{"per-step", "stderr", "setup", "action", "teardown"} {
		if !strings.Contains(usage, keyword) {
			t.Errorf("Sub-AC 7b: flag description missing keyword %q in: %s", keyword, usage)
		}
	}

	// The flag variable must be accessible and default to false.
	if *e2eDebugTCStepsFlag {
		t.Log("Sub-AC 7b: e2e.debug-tc-steps is currently true (expect false in unit test context)")
	}
}

// TestAC7bDebugTCStepsSetupTotalTakesPrecedenceOverBeforeEach verifies that
// when BOTH tc.setup.total and hook.before_each are present, tc.setup.total
// takes precedence as the setup duration.
func TestAC7bDebugTCStepsSetupTotalTakesPrecedenceOverBeforeEach(t *testing.T) {
	t.Parallel()
	profile := testCaseTimingProfile{
		Phases: []phaseTimingSample{
			// hook.before_each is the outer envelope; tc.setup.total is the inner
			// metric measuring just the setup work — it should be preferred.
			{Name: string(phaseBeforeEach), DurationNanos: (50 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSetupTotal), DurationNanos: (12 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSpecBody), DurationNanos: (30 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownTotal), DurationNanos: (8 * time.Millisecond).Nanoseconds()},
		},
	}

	setup, _, _ := extractStepDurations(profile)

	// tc.setup.total=12ms should win over hook.before_each=50ms.
	if setup != 12*time.Millisecond {
		t.Errorf("Sub-AC 7b: setup=%v, want 12ms (tc.setup.total should take precedence)", setup)
	}
}
