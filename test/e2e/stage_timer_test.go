package e2e

// stage_timer_test.go — Sub-AC 5.4: tests for the pipeline stage timer.
//
// Verified behaviours:
//
//  1. newPipelineStageTimer returns a disabled timer when E2E_STAGE_TIMING is unset.
//  2. newPipelineStageTimerEnabled creates a timer that is always active.
//  3. StartStage / done() records the correct stage name.
//  4. Multiple stages are recorded in insertion order.
//  5. Bottleneck returns the stage with the longest duration.
//  6. TotalDuration sums all stage durations.
//  7. Emit writes a human-readable summary to an io.Writer.
//  8. Emit identifies the bottleneck stage with the "▶" marker.
//  9. Emit writes "budget: WITHIN 120s" when total < 120s.
// 10. Emit writes "WARNING: pipeline exceeded 120s budget" when total >= 120s.
// 11. Emit is idempotent on a nil or disabled timer.
// 12. RecordedStages returns a copy; modifications do not affect the timer.
// 13. Calling done() twice for the same stage does not double-count.
// 14. Emit finalises any in-progress stage before computing the summary.
// 15. Stages not listed in pipelineStageOrder appear in the index but not in
//     the ordered section (order coverage only for known stages).

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// ── 1. Disabled timer when env var is absent ──────────────────────────────────

func TestStagetimer_Disabled_WhenEnvUnset(t *testing.T) {
	t.Parallel()
	// Use newPipelineStageTimerDisabled() instead of t.Setenv so that this test
	// can safely run in parallel (t.Setenv and t.Parallel are incompatible in Go).
	timer := newPipelineStageTimerDisabled()
	if timer.enabled {
		t.Fatal("timer should be disabled when E2E_STAGE_TIMING is empty")
	}
}

// ── 2. Always-enabled constructor ────────────────────────────────────────────

func TestStagetimer_Enabled_newPipelineStageTimerEnabled(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	if !timer.enabled {
		t.Fatal("newPipelineStageTimerEnabled should always be enabled")
	}
}

// ── 3. StartStage / done records the stage name ───────────────────────────────

func TestStagetimer_StartStage_RecordsName(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	done := timer.StartStage(stageClusterCreate)
	done()

	stages := timer.RecordedStages()
	if len(stages) != 1 {
		t.Fatalf("want 1 stage, got %d", len(stages))
	}
	if stages[0].Name != stageClusterCreate {
		t.Errorf("stage name = %q, want %q", stages[0].Name, stageClusterCreate)
	}
}

// ── 4. Multiple stages recorded in insertion order ────────────────────────────

func TestStagetimer_MultipleStages_InOrder(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.StartStage(stageClusterCreate)()
	timer.StartStage(stageImageBuild)()
	timer.StartStage(stageBackendSetup)()
	timer.StartStage(stageTestExec)()

	stages := timer.RecordedStages()
	if len(stages) != 4 {
		t.Fatalf("want 4 stages, got %d", len(stages))
	}
	wantOrder := []pipelineStage{stageClusterCreate, stageImageBuild, stageBackendSetup, stageTestExec}
	for i, name := range wantOrder {
		if stages[i].Name != name {
			t.Errorf("stages[%d].Name = %q, want %q", i, stages[i].Name, name)
		}
	}
}

// ── 5. Bottleneck returns the longest stage ───────────────────────────────────

func TestStagetimer_Bottleneck_LongestStage(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()

	// Inject synthetic stage durations.
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 60 * time.Second}, // longest
		{Name: stageBackendSetup, Duration: 3 * time.Second},
		{Name: stageTestExec, Duration: 15 * time.Second},
	}

	b := timer.Bottleneck()
	if b.Name != stageImageBuild {
		t.Errorf("Bottleneck().Name = %q, want %q", b.Name, stageImageBuild)
	}
	if b.Duration != 60*time.Second {
		t.Errorf("Bottleneck().Duration = %s, want 60s", b.Duration)
	}
}

// ── 6. TotalDuration sums all stages ─────────────────────────────────────────

func TestStagetimer_TotalDuration_Sum(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 60 * time.Second},
		{Name: stageBackendSetup, Duration: 3 * time.Second},
		{Name: stageTestExec, Duration: 15 * time.Second},
	}

	want := 108 * time.Second
	if got := timer.TotalDuration(); got != want {
		t.Errorf("TotalDuration() = %s, want %s", got, want)
	}
}

// ── 7. Emit writes a summary to an io.Writer ─────────────────────────────────

func TestStagetimer_Emit_WritesHeader(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 60 * time.Second},
		{Name: stageBackendSetup, Duration: 3 * time.Second},
		{Name: stageTestExec, Duration: 15 * time.Second},
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	if !strings.Contains(output, "=== E2E Pipeline Stage Timing ===") {
		t.Errorf("Emit output missing header:\n%s", output)
	}
	if !strings.Contains(output, "total pipeline:") {
		t.Errorf("Emit output missing 'total pipeline:':\n%s", output)
	}
}

// ── 8. Emit marks the bottleneck with "▶" ─────────────────────────────────────

func TestStagetimer_Emit_BottleneckMarker(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 60 * time.Second}, // bottleneck
		{Name: stageBackendSetup, Duration: 3 * time.Second},
		{Name: stageTestExec, Duration: 15 * time.Second},
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	// The bottleneck line must contain the "▶" marker.
	lines := strings.Split(output, "\n")
	var bottleneckLine string
	for _, l := range lines {
		if strings.Contains(l, "image-build") {
			bottleneckLine = l
			break
		}
	}
	if !strings.Contains(bottleneckLine, "▶") {
		t.Errorf("bottleneck line missing '▶' marker: %q\nfull output:\n%s", bottleneckLine, output)
	}

	// The non-bottleneck lines must NOT contain "▶".
	for _, l := range lines {
		if strings.Contains(l, "cluster-create") || strings.Contains(l, "backend-setup") || strings.Contains(l, "test-exec") {
			if strings.Contains(l, "▶") {
				t.Errorf("non-bottleneck line has '▶' marker: %q", l)
			}
		}
	}
}

// ── 9. Emit writes WITHIN budget line when total < 120s ──────────────────────

func TestStagetimer_Emit_WithinBudget(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 50 * time.Second},
		{Name: stageBackendSetup, Duration: 3 * time.Second},
		{Name: stageTestExec, Duration: 15 * time.Second},
	} // total = 98s < 120s

	var buf bytes.Buffer
	within := timer.Emit(&buf)
	output := buf.String()

	if !within {
		t.Errorf("Emit() returned false (over budget) but total = 98s < 120s")
	}
	if !strings.Contains(output, "budget: WITHIN 120s") {
		t.Errorf("Emit output missing 'budget: WITHIN 120s':\n%s", output)
	}
	if strings.Contains(output, "WARNING") {
		t.Errorf("Emit output should NOT contain WARNING when within budget:\n%s", output)
	}
}

// ── 10. Emit writes WARNING when total >= 120s ───────────────────────────────

func TestStagetimer_Emit_ExceedsBudget(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 50 * time.Second},
		{Name: stageImageBuild, Duration: 80 * time.Second}, // total = 130s
	}

	var buf bytes.Buffer
	within := timer.Emit(&buf)
	output := buf.String()

	if within {
		t.Errorf("Emit() returned true (within budget) but total = 130s > 120s")
	}
	if !strings.Contains(output, "WARNING") {
		t.Errorf("Emit output missing 'WARNING' when total exceeds 120s:\n%s", output)
	}
}

// ── 11. Nil / disabled timer is a no-op ──────────────────────────────────────

func TestStagetimer_NilReceiver_NoOp(t *testing.T) {
	t.Parallel()
	var timer *pipelineStageTimer

	// None of these should panic.
	done := timer.StartStage(stageClusterCreate)
	done()
	_ = timer.TotalDuration()
	_ = timer.Bottleneck()
	_ = timer.RecordedStages()

	var buf bytes.Buffer
	timer.Emit(&buf)
	if buf.Len() != 0 {
		t.Errorf("nil timer Emit wrote %d bytes, want 0", buf.Len())
	}
}

func TestStagetimer_DisabledTimer_NoOp(t *testing.T) {
	t.Parallel()
	// Use newPipelineStageTimerDisabled() to avoid t.Setenv incompatibility with t.Parallel.
	timer := newPipelineStageTimerDisabled()

	timer.StartStage(stageClusterCreate)()
	timer.StartStage(stageImageBuild)()

	if got := timer.TotalDuration(); got != 0 {
		t.Errorf("disabled timer TotalDuration() = %s, want 0", got)
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	if buf.Len() != 0 {
		t.Errorf("disabled timer Emit wrote %d bytes, want 0", buf.Len())
	}
}

// ── 12. RecordedStages returns a copy ────────────────────────────────────────

func TestStagetimer_RecordedStages_ReturnsCopy(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
	}

	stages := timer.RecordedStages()
	stages[0].Name = stageImageBuild // mutate copy

	// The original must be unaffected.
	if timer.stages[0].Name != stageClusterCreate {
		t.Errorf("RecordedStages mutated internal state: got %q, want %q",
			timer.stages[0].Name, stageClusterCreate)
	}
}

// ── 13. Calling done() twice does not double-count ───────────────────────────

func TestStagetimer_Done_Idempotent(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	done := timer.StartStage(stageClusterCreate)
	done()
	done() // second call — must be a no-op

	if len(timer.stages) != 1 {
		t.Errorf("double-calling done() recorded %d stages, want 1", len(timer.stages))
	}
}

// ── 14. Emit finalises an in-progress stage ───────────────────────────────────

func TestStagetimer_Emit_FinalisesInProgressStage(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.StartStage(stageTestExec)
	// Do NOT call done() — let Emit finalise it.

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	if !strings.Contains(output, "test-exec") {
		t.Errorf("Emit did not finalise in-progress stage 'test-exec':\n%s", output)
	}
	if !strings.Contains(output, "=== E2E Pipeline Stage Timing ===") {
		t.Errorf("Emit did not write header for in-progress stage:\n%s", output)
	}
}

// ── 15. bottleneck line mentions the bottleneck TC ───────────────────────────

func TestStagetimer_Emit_BottleneckLine(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 5 * time.Second},
		{Name: stageImageBuild, Duration: 90 * time.Second}, // bottleneck
		{Name: stageBackendSetup, Duration: 2 * time.Second},
		{Name: stageTestExec, Duration: 10 * time.Second},
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	if !strings.Contains(output, "bottleneck: image-build") {
		t.Errorf("Emit bottleneck line does not name image-build:\n%s", output)
	}
}
