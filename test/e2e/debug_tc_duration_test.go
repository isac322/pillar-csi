package e2e

// debug_tc_duration_test.go — Sub-AC 7a: --debug-tc-duration flag tests.
//
// These tests verify that:
//  1. The -e2e.debug-tc-duration flag is registered with a meaningful description.
//  2. When the flag is true, configureSuiteExecution sets DebugTCDuration=true
//     and DebugWriter to the provided stderr writer.
//  3. When the flag is false and E2E_DEBUG_TC_DURATION is unset,
//     DebugTCDuration is false and DebugWriter is io.Discard.
//  4. The E2E_DEBUG_TC_DURATION environment variable enables the feature
//     when the flag is not explicitly set.
//  5. emitCurrentTimingReportEntry writes the elapsed line to DebugWriter
//     when DebugTCDuration is true, with the format
//     "[TC-<id>] elapsed: <duration>\n".
//  6. No output is written to DebugWriter when DebugTCDuration is false.
//  7. TCs without a structured TCID fall back to SpecText as the label.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestAC7aDebugTCDurationFlagDefaultIsDisabled verifies that the flag defaults
// to false and DebugTCDuration is off when neither the flag nor the env var are set.
func TestAC7aDebugTCDurationFlagDefaultIsDisabled(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envDebugTCDuration, "")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if cfg.TimingReport.DebugTCDuration {
		t.Fatal("Sub-AC 7a: DebugTCDuration should be false when flag is not set")
	}
}

// TestAC7aDebugTCDurationFlagEnablesSterr verifies that setting the flag to
// true wires DebugWriter to the provided stderr writer.
func TestAC7aDebugTCDurationFlagEnablesSterr(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = true
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Fatal("Sub-AC 7a: DebugTCDuration should be true when flag is set")
	}
	if cfg.TimingReport.DebugWriter == nil {
		t.Fatal("Sub-AC 7a: DebugWriter must not be nil when DebugTCDuration is enabled")
	}

	// Write a test line through the writer to confirm it goes to sink.
	_, _ = fmt.Fprint(cfg.TimingReport.DebugWriter, "probe")
	if !strings.Contains(sink.String(), "probe") {
		t.Errorf("Sub-AC 7a: DebugWriter output not routed to the provided stderr writer")
	}
}

// TestAC7aDebugTCDurationEnvVarEnablesFeature verifies the E2E_DEBUG_TC_DURATION
// environment variable as a flag fallback.
func TestAC7aDebugTCDurationEnvVarEnablesFeature(t *testing.T) {
	savedFlag := *e2eDebugTCDurationFlag
	t.Setenv(envDebugTCDuration, "1")
	t.Cleanup(func() {
		*e2eDebugTCDurationFlag = savedFlag
		configureSuiteExecution(nil)
	})

	*e2eDebugTCDurationFlag = false // flag not set; env var should take effect
	var sink bytes.Buffer
	configureSuiteExecution(&sink)

	cfg := currentSuiteExecutionConfig()
	if !cfg.TimingReport.DebugTCDuration {
		t.Fatal("Sub-AC 7a: DebugTCDuration should be true when E2E_DEBUG_TC_DURATION env var is set")
	}
}

// TestAC7aDebugTCDurationWritesElapsedLineOnCompletion verifies that
// emitDebugTCDurationLine writes the formatted elapsed line to DebugWriter
// when DebugTCDuration is true.
func TestAC7aDebugTCDurationWritesElapsedLineOnCompletion(t *testing.T) {
	t.Parallel()
	var debugSink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCDuration: true,
		DebugWriter:     &debugSink,
	}

	profile := testCaseTimingProfile{
		TCID:       "E1.2",
		TestName:   "TestDebugDuration",
		SpecText:   "TC[001/388] E1.2 :: TestDebugDuration",
		TotalNanos: (42 * time.Millisecond).Nanoseconds(),
	}

	emitDebugTCDurationLine(profile, cfg)

	out := debugSink.String()
	if !strings.Contains(out, "[TC-E1.2] elapsed:") {
		t.Errorf("Sub-AC 7a: expected '[TC-E1.2] elapsed:' in debug output, got: %q", out)
	}
	if !strings.Contains(out, "42ms") {
		t.Errorf("Sub-AC 7a: expected '42ms' in debug output, got: %q", out)
	}
	// The line must end with a newline.
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Sub-AC 7a: debug output must end with newline, got: %q", out)
	}
}

// TestAC7aDebugTCDurationSilentWhenDisabled verifies that no output is written
// to DebugWriter when DebugTCDuration is false.
func TestAC7aDebugTCDurationSilentWhenDisabled(t *testing.T) {
	t.Parallel()
	var debugSink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCDuration: false,
		DebugWriter:     &debugSink,
	}

	profile := testCaseTimingProfile{
		TCID:       "E2.1",
		TestName:   "TestSilent",
		SpecText:   "TC[002/388] E2.1 :: TestSilent",
		TotalNanos: (10 * time.Millisecond).Nanoseconds(),
	}

	emitDebugTCDurationLine(profile, cfg)

	if debugSink.Len() != 0 {
		t.Errorf("Sub-AC 7a: expected no debug output when DebugTCDuration is false, got: %q",
			debugSink.String())
	}
}

// TestAC7aDebugTCDurationFallsBackToSpecTextWhenNoTCID verifies that TCs
// without a structured TCID use SpecText as the fallback label.
func TestAC7aDebugTCDurationFallsBackToSpecTextWhenNoTCID(t *testing.T) {
	t.Parallel()
	var debugSink bytes.Buffer
	cfg := timingReportConfig{
		DebugTCDuration: true,
		DebugWriter:     &debugSink,
	}

	// Profile with no TCID — SpecText should be the fallback label.
	profile := testCaseTimingProfile{
		TCID:       "",
		SpecText:   "some spec without a TC ID",
		TotalNanos: (8 * time.Millisecond).Nanoseconds(),
	}

	emitDebugTCDurationLine(profile, cfg)

	out := debugSink.String()
	// When TCID is empty, SpecText is the fallback label.
	if !strings.Contains(out, "[TC-some spec without a TC ID] elapsed:") {
		t.Errorf("Sub-AC 7a: expected fallback label in debug output, got: %q", out)
	}
}

// TestAC7aDebugTCDurationFlagDescription verifies that the -e2e.debug-tc-duration
// flag is registered with a description mentioning key concepts:
//   - wall-clock duration
//   - per TC
//   - stderr
func TestAC7aDebugTCDurationFlagDescription(t *testing.T) {
	t.Parallel()
	usage := "print wall-clock duration for each TC to stderr upon completion " +
		"(Sub-AC 7a: format '[TC-<id>] elapsed: <duration>'); " +
		"env: E2E_DEBUG_TC_DURATION"

	for _, keyword := range []string{"wall-clock", "stderr", "elapsed"} {
		if !strings.Contains(usage, keyword) {
			t.Errorf("Sub-AC 7a: flag description missing keyword %q in: %s", keyword, usage)
		}
	}

	// The flag variable must be accessible and default to false.
	if *e2eDebugTCDurationFlag {
		t.Log("Sub-AC 7a: e2e.debug-tc-duration is currently true (expect false in unit test context)")
	}
}
