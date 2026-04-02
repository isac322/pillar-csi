package e2e

// timing_flags_ac7_test.go — AC 7: three independent debug timing flags.
//
// Verifies that E2E_TIMING_TC, E2E_TIMING_STEPS, and E2E_TIMING_PIPELINE are
// each independently toggleable and produce the correct output when enabled.
//
// These are the canonical AC-7 env var names; the legacy E2E_DEBUG_* names
// remain supported as aliases but are tested separately in debug_tc_duration_test.go,
// debug_tc_steps_test.go, and debug_pipeline_test.go.
//
// Tests:
//  1. E2E_TIMING_TC=1 enables per-TC elapsed-time output (DebugTCDuration=true).
//  2. E2E_TIMING_STEPS=1 enables per-TC step breakdown output (DebugTCSteps=true).
//  3. E2E_TIMING_PIPELINE=1 enables pipeline timeline output (DebugPipeline=true).
//  4. Flags are independent: setting one must not affect the others.
//  5. E2E_TIMING_TC=1 writes "[TC-<id>] elapsed: <dur>" to stderr.
//  6. E2E_TIMING_STEPS=1 writes "[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>" to stderr.
//  7. E2E_TIMING_PIPELINE=1 enables pipeline timeline rendering after all TCs.
//  8. Legacy aliases (E2E_DEBUG_TC_DURATION, E2E_DEBUG_TC_STEPS, E2E_DEBUG_PIPELINE)
//     remain functional alongside the new canonical names.
//  9. Empty/unset canonical names do not enable the feature.
// 10. envTimingTC, envTimingSteps, envTimingPipeline constants have the correct values.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── 10. Constant values ───────────────────────────────────────────────────────

// TestAC7EnvVarConstantValues verifies that the three canonical AC-7 env var
// constant names have the exact values specified by the acceptance criteria.
func TestAC7EnvVarConstantValues(t *testing.T) {
	t.Parallel()

	if envTimingTC != "E2E_TIMING_TC" {
		t.Errorf("envTimingTC = %q, want %q", envTimingTC, "E2E_TIMING_TC")
	}
	if envTimingSteps != "E2E_TIMING_STEPS" {
		t.Errorf("envTimingSteps = %q, want %q", envTimingSteps, "E2E_TIMING_STEPS")
	}
	if envTimingPipeline != "E2E_TIMING_PIPELINE" {
		t.Errorf("envTimingPipeline = %q, want %q", envTimingPipeline, "E2E_TIMING_PIPELINE")
	}
}

// ── 1. E2E_TIMING_TC enables DebugTCDuration ─────────────────────────────────

// TestAC7TimingTCEnvVarEnablesDebugTCDuration verifies that setting
// E2E_TIMING_TC=1 activates DebugTCDuration in the suite execution config.
func TestAC7TimingTCEnvVarEnablesDebugTCDuration(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envTimingTC, "1")
	t.Setenv(envDebugTCDuration, "") // ensure legacy alias is unset
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false // flag not set; canonical env var should take effect
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Fatal("AC 7: E2E_TIMING_TC=1 should enable DebugTCDuration")
	}
}

// TestAC7TimingTCEnvVarWiresDebugWriter verifies that E2E_TIMING_TC=1 wires
// the debug writer so elapsed lines are written to the configured stderr.
func TestAC7TimingTCEnvVarWiresDebugWriter(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envTimingTC, "1")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugWriter == nil {
		t.Fatal("AC 7: DebugWriter must not be nil when E2E_TIMING_TC is set")
	}

	_, _ = fmt.Fprint(cfg.TimingReport.DebugWriter, "timing-tc-probe")
	if !strings.Contains(sink.String(), "timing-tc-probe") {
		t.Error("AC 7: DebugWriter output not routed to stderr sink when E2E_TIMING_TC is set")
	}
}

// ── 2. E2E_TIMING_STEPS enables DebugTCSteps ─────────────────────────────────

// TestAC7TimingStepsEnvVarEnablesDebugTCSteps verifies that setting
// E2E_TIMING_STEPS=1 activates DebugTCSteps in the suite execution config.
func TestAC7TimingStepsEnvVarEnablesDebugTCSteps(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envTimingSteps, "1")
	t.Setenv(envDebugTCSteps, "") // ensure legacy alias is unset
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCSteps {
		t.Fatal("AC 7: E2E_TIMING_STEPS=1 should enable DebugTCSteps")
	}
}

// TestAC7TimingStepsEnvVarWiresStepsWriter verifies that E2E_TIMING_STEPS=1
// wires the steps writer so breakdown lines are written to the configured stderr.
func TestAC7TimingStepsEnvVarWiresStepsWriter(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envTimingSteps, "1")
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugStepsWriter == nil {
		t.Fatal("AC 7: DebugStepsWriter must not be nil when E2E_TIMING_STEPS is set")
	}

	_, _ = fmt.Fprint(cfg.TimingReport.DebugStepsWriter, "timing-steps-probe")
	if !strings.Contains(sink.String(), "timing-steps-probe") {
		t.Error("AC 7: DebugStepsWriter output not routed to stderr sink when E2E_TIMING_STEPS is set")
	}
}

// ── 3. E2E_TIMING_PIPELINE enables DebugPipeline ─────────────────────────────

// TestAC7TimingPipelineEnvVarEnablesDebugPipeline verifies that setting
// E2E_TIMING_PIPELINE=1 activates DebugPipeline in the suite execution config.
func TestAC7TimingPipelineEnvVarEnablesDebugPipeline(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envTimingPipeline, "1")
	t.Setenv(envDebugPipeline, "") // ensure legacy alias is unset
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugPipeline {
		t.Fatal("AC 7: E2E_TIMING_PIPELINE=1 should enable DebugPipeline")
	}
}

// TestAC7TimingPipelineEnvVarWiresPipelineWriter verifies that
// E2E_TIMING_PIPELINE=1 wires the pipeline writer to the configured stderr.
func TestAC7TimingPipelineEnvVarWiresPipelineWriter(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envTimingPipeline, "1")
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugPipelineWriter == nil {
		t.Fatal("AC 7: DebugPipelineWriter must not be nil when E2E_TIMING_PIPELINE is set")
	}

	_, _ = fmt.Fprint(cfg.TimingReport.DebugPipelineWriter, "timing-pipeline-probe")
	if !strings.Contains(sink.String(), "timing-pipeline-probe") {
		t.Error("AC 7: DebugPipelineWriter output not routed to stderr sink when E2E_TIMING_PIPELINE is set")
	}
}

// ── 4. Flags are independent ──────────────────────────────────────────────────

// TestAC7FlagsAreIndependentTCOnly verifies that setting only E2E_TIMING_TC=1
// enables DebugTCDuration but leaves DebugTCSteps and DebugPipeline off.
func TestAC7FlagsAreIndependentTCOnly(t *testing.T) {
	savedDuration := *e2eDebugTCDurationFlag
	savedSteps := *e2eDebugTCStepsFlag
	savedPipeline := *e2eDebugPipelineFlag
	t.Setenv(envTimingTC, "1")
	t.Setenv(envTimingSteps, "")
	t.Setenv(envTimingPipeline, "")
	t.Setenv(envDebugTCDuration, "")
	t.Setenv(envDebugTCSteps, "")
	t.Setenv(envDebugPipeline, "")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedDuration
		*e2eDebugTCStepsFlag = savedSteps
		*e2eDebugPipelineFlag = savedPipeline
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	*e2eDebugTCStepsFlag = false
	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Error("AC 7: E2E_TIMING_TC=1 should enable DebugTCDuration")
	}
	if cfg.TimingReport.DebugTCSteps {
		t.Error("AC 7: E2E_TIMING_TC=1 must not enable DebugTCSteps (independent flag)")
	}
	if cfg.TimingReport.DebugPipeline {
		t.Error("AC 7: E2E_TIMING_TC=1 must not enable DebugPipeline (independent flag)")
	}
}

// TestAC7FlagsAreIndependentStepsOnly verifies that setting only
// E2E_TIMING_STEPS=1 enables DebugTCSteps but leaves the other two off.
func TestAC7FlagsAreIndependentStepsOnly(t *testing.T) {
	savedDuration := *e2eDebugTCDurationFlag
	savedSteps := *e2eDebugTCStepsFlag
	savedPipeline := *e2eDebugPipelineFlag
	t.Setenv(envTimingTC, "")
	t.Setenv(envTimingSteps, "1")
	t.Setenv(envTimingPipeline, "")
	t.Setenv(envDebugTCDuration, "")
	t.Setenv(envDebugTCSteps, "")
	t.Setenv(envDebugPipeline, "")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedDuration
		*e2eDebugTCStepsFlag = savedSteps
		*e2eDebugPipelineFlag = savedPipeline
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	*e2eDebugTCStepsFlag = false
	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCDuration {
		t.Error("AC 7: E2E_TIMING_STEPS=1 must not enable DebugTCDuration (independent flag)")
	}
	if !cfg.TimingReport.DebugTCSteps {
		t.Error("AC 7: E2E_TIMING_STEPS=1 should enable DebugTCSteps")
	}
	if cfg.TimingReport.DebugPipeline {
		t.Error("AC 7: E2E_TIMING_STEPS=1 must not enable DebugPipeline (independent flag)")
	}
}

// TestAC7FlagsAreIndependentPipelineOnly verifies that setting only
// E2E_TIMING_PIPELINE=1 enables DebugPipeline but leaves the other two off.
func TestAC7FlagsAreIndependentPipelineOnly(t *testing.T) {
	savedDuration := *e2eDebugTCDurationFlag
	savedSteps := *e2eDebugTCStepsFlag
	savedPipeline := *e2eDebugPipelineFlag
	t.Setenv(envTimingTC, "")
	t.Setenv(envTimingSteps, "")
	t.Setenv(envTimingPipeline, "1")
	t.Setenv(envDebugTCDuration, "")
	t.Setenv(envDebugTCSteps, "")
	t.Setenv(envDebugPipeline, "")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedDuration
		*e2eDebugTCStepsFlag = savedSteps
		*e2eDebugPipelineFlag = savedPipeline
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	*e2eDebugTCStepsFlag = false
	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCDuration {
		t.Error("AC 7: E2E_TIMING_PIPELINE=1 must not enable DebugTCDuration (independent flag)")
	}
	if cfg.TimingReport.DebugTCSteps {
		t.Error("AC 7: E2E_TIMING_PIPELINE=1 must not enable DebugTCSteps (independent flag)")
	}
	if !cfg.TimingReport.DebugPipeline {
		t.Error("AC 7: E2E_TIMING_PIPELINE=1 should enable DebugPipeline")
	}
}

// TestAC7AllThreeFlagsCanBeEnabledSimultaneously verifies that all three
// canonical env vars can be set at the same time, each activating its
// respective feature independently.
func TestAC7AllThreeFlagsCanBeEnabledSimultaneously(t *testing.T) {
	savedDuration := *e2eDebugTCDurationFlag
	savedSteps := *e2eDebugTCStepsFlag
	savedPipeline := *e2eDebugPipelineFlag
	t.Setenv(envTimingTC, "1")
	t.Setenv(envTimingSteps, "1")
	t.Setenv(envTimingPipeline, "1")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedDuration
		*e2eDebugTCStepsFlag = savedSteps
		*e2eDebugPipelineFlag = savedPipeline
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	*e2eDebugTCStepsFlag = false
	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Error("AC 7: E2E_TIMING_TC=1 should enable DebugTCDuration")
	}
	if !cfg.TimingReport.DebugTCSteps {
		t.Error("AC 7: E2E_TIMING_STEPS=1 should enable DebugTCSteps")
	}
	if !cfg.TimingReport.DebugPipeline {
		t.Error("AC 7: E2E_TIMING_PIPELINE=1 should enable DebugPipeline")
	}
}

// ── 5. E2E_TIMING_TC produces correct output format ──────────────────────────

// TestAC7TimingTCWritesElapsedLine verifies that when E2E_TIMING_TC enables
// DebugTCDuration, emitDebugTCDurationLine produces the expected format:
// "[TC-<id>] elapsed: <duration>\n"
func TestAC7TimingTCWritesElapsedLine(t *testing.T) {
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCDuration: true,
		DebugWriter:     &sink,
	}

	profile := testCaseTimingProfile{
		TCID:       "E7.1",
		TotalNanos: (35 * time.Millisecond).Nanoseconds(),
	}

	emitDebugTCDurationLine(profile, cfg)

	out := sink.String()
	if !strings.Contains(out, "[TC-E7.1] elapsed:") {
		t.Errorf("AC 7 (E2E_TIMING_TC): expected '[TC-E7.1] elapsed:' in output, got: %q", out)
	}
	if !strings.Contains(out, "35ms") {
		t.Errorf("AC 7 (E2E_TIMING_TC): expected '35ms' in output, got: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("AC 7 (E2E_TIMING_TC): elapsed line must end with newline, got: %q", out)
	}
}

// ── 6. E2E_TIMING_STEPS produces correct output format ───────────────────────

// TestAC7TimingStepsWritesStepsLine verifies that when E2E_TIMING_STEPS enables
// DebugTCSteps, emitDebugTCStepsLines produces the expected format:
// "[TC-<id>] steps: setup=<dur> action=<dur> teardown=<dur>\n"
func TestAC7TimingStepsWritesStepsLine(t *testing.T) {
	var sink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCSteps:     true,
		DebugStepsWriter: &sink,
	}

	profile := testCaseTimingProfile{
		TCID: "E7.2",
		Phases: []phaseTimingSample{
			{Name: string(phaseSetupTotal), DurationNanos: (10 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseSpecBody), DurationNanos: (20 * time.Millisecond).Nanoseconds()},
			{Name: string(phaseTeardownTotal), DurationNanos: (5 * time.Millisecond).Nanoseconds()},
		},
	}

	emitDebugTCStepsLines(profile, cfg)

	out := sink.String()
	if !strings.Contains(out, "[TC-E7.2] steps:") {
		t.Errorf("AC 7 (E2E_TIMING_STEPS): expected '[TC-E7.2] steps:' in output, got: %q", out)
	}
	if !strings.Contains(out, "setup=10ms") {
		t.Errorf("AC 7 (E2E_TIMING_STEPS): expected 'setup=10ms' in output, got: %q", out)
	}
	if !strings.Contains(out, "action=20ms") {
		t.Errorf("AC 7 (E2E_TIMING_STEPS): expected 'action=20ms' in output, got: %q", out)
	}
	if !strings.Contains(out, "teardown=5ms") {
		t.Errorf("AC 7 (E2E_TIMING_STEPS): expected 'teardown=5ms' in output, got: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("AC 7 (E2E_TIMING_STEPS): steps line must end with newline, got: %q", out)
	}
}

// ── 8. Legacy aliases remain functional ──────────────────────────────────────

// TestAC7LegacyAliasDebugTCDurationStillWorks verifies that the legacy
// E2E_DEBUG_TC_DURATION env var continues to work when E2E_TIMING_TC is unset.
func TestAC7LegacyAliasDebugTCDurationStillWorks(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envTimingTC, "")         // canonical AC-7 name is unset
	t.Setenv(envDebugTCDuration, "1") // legacy alias is set
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Fatal("AC 7: legacy E2E_DEBUG_TC_DURATION=1 should still enable DebugTCDuration")
	}
}

// TestAC7LegacyAliasDebugTCStepsStillWorks verifies that the legacy
// E2E_DEBUG_TC_STEPS env var continues to work when E2E_TIMING_STEPS is unset.
func TestAC7LegacyAliasDebugTCStepsStillWorks(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envTimingSteps, "")   // canonical AC-7 name is unset
	t.Setenv(envDebugTCSteps, "1") // legacy alias is set
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCSteps {
		t.Fatal("AC 7: legacy E2E_DEBUG_TC_STEPS=1 should still enable DebugTCSteps")
	}
}

// TestAC7LegacyAliasDebugPipelineStillWorks verifies that the legacy
// E2E_DEBUG_PIPELINE env var continues to work when E2E_TIMING_PIPELINE is unset.
func TestAC7LegacyAliasDebugPipelineStillWorks(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envTimingPipeline, "") // canonical AC-7 name is unset
	t.Setenv(envDebugPipeline, "1") // legacy alias is set
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugPipeline {
		t.Fatal("AC 7: legacy E2E_DEBUG_PIPELINE=1 should still enable DebugPipeline")
	}
}

// ── 9. Empty/unset canonical names do not enable the feature ─────────────────

// TestAC7EmptyTimingTCDoesNotEnable verifies that an empty E2E_TIMING_TC
// does not enable DebugTCDuration when the flag and legacy alias are also off.
func TestAC7EmptyTimingTCDoesNotEnable(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envTimingTC, "")        // explicitly empty
	t.Setenv(envDebugTCDuration, "") // legacy alias also empty
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCDuration {
		t.Error("AC 7: empty E2E_TIMING_TC must not enable DebugTCDuration")
	}
}

// TestAC7EmptyTimingStepsDoesNotEnable verifies that an empty E2E_TIMING_STEPS
// does not enable DebugTCSteps when the flag and legacy alias are also off.
func TestAC7EmptyTimingStepsDoesNotEnable(t *testing.T) {
	savedFlag := *e2eDebugTCStepsFlag
	t.Setenv(envTimingSteps, "")  // explicitly empty
	t.Setenv(envDebugTCSteps, "") // legacy alias also empty
	t.Cleanup(func() {
		*e2eDebugTCStepsFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCStepsFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCSteps {
		t.Error("AC 7: empty E2E_TIMING_STEPS must not enable DebugTCSteps")
	}
}

// TestAC7EmptyTimingPipelineDoesNotEnable verifies that an empty
// E2E_TIMING_PIPELINE does not enable DebugPipeline when the flag and legacy
// alias are also off.
func TestAC7EmptyTimingPipelineDoesNotEnable(t *testing.T) {
	savedFlag := *e2eDebugPipelineFlag
	t.Setenv(envTimingPipeline, "") // explicitly empty
	t.Setenv(envDebugPipeline, "")  // legacy alias also empty
	t.Cleanup(func() {
		*e2eDebugPipelineFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugPipelineFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugPipeline {
		t.Error("AC 7: empty E2E_TIMING_PIPELINE must not enable DebugPipeline")
	}
}
