package e2e

// stage_timer_ac6_test.go — AC 6: make test-e2e completes within 2 minutes
// with per-stage time budgets.
//
// Acceptance criteria verified here:
//
//  1. Per-stage budget constants are defined and sum to the 120s total:
//       clusterImagesBudgetSeconds (60) + backendBudgetSeconds (15) +
//       testsBudgetSeconds (45) = 120
//
//  2. StageBudgetViolations returns nil when all stages are within budget.
//
//  3. StageBudgetViolations reports a violation when cluster+images > 60s.
//
//  4. StageBudgetViolations reports a violation when backend > 15s.
//
//  5. StageBudgetViolations reports a violation when tests > 45s.
//
//  6. StageBudgetViolations reports multiple violations simultaneously when
//     more than one stage exceeds its budget.
//
//  7. Emit output includes per-stage budget status lines ("cluster+images",
//     "backend", "tests") so the summary is self-contained.
//
//  8. Emit marks a stage budget as "EXCEEDED" in per-stage budget status when
//     the stage exceeds its limit.
//
//  9. Emit marks all stages as "OK" in per-stage budget status when all are
//     within their limits.
//
// 10. StageBudgetViolations is safe to call on a nil receiver.
//
// These tests run as plain Go unit tests (no Ginkgo suite) so they execute
// quickly via `go test -run TestAC6Stage ./test/e2e/`.

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// ── 1. Budget constants sum to 120s ──────────────────────────────────────────

// TestAC6StageBudgetConstantsSumTo120 verifies that the three per-stage budget
// constants sum exactly to the total 120-second pipeline budget, ensuring the
// sub-budgets are internally consistent with the overall target.
func TestAC6StageBudgetConstantsSumTo120(t *testing.T) {
	t.Parallel()
	total := clusterImagesBudgetSeconds + backendBudgetSeconds + testsBudgetSeconds
	if total != stageBudgetSeconds {
		t.Errorf(
			"sub-budgets sum = %ds, want %ds (= %ds cluster+images + %ds backend + %ds tests)",
			total, stageBudgetSeconds,
			clusterImagesBudgetSeconds, backendBudgetSeconds, testsBudgetSeconds,
		)
	}
}

// TestAC6ClusterImagesBudgetIs60s verifies the cluster+images sub-budget is
// exactly 60 seconds as required by AC 6.
func TestAC6ClusterImagesBudgetIs60s(t *testing.T) {
	t.Parallel()
	if clusterImagesBudgetSeconds != 60 {
		t.Errorf("clusterImagesBudgetSeconds = %d, want 60", clusterImagesBudgetSeconds)
	}
}

// TestAC6BackendBudgetIs15s verifies the backend provision sub-budget is
// exactly 15 seconds as required by AC 6.
func TestAC6BackendBudgetIs15s(t *testing.T) {
	t.Parallel()
	if backendBudgetSeconds != 15 {
		t.Errorf("backendBudgetSeconds = %d, want 15", backendBudgetSeconds)
	}
}

// TestAC6TestsBudgetIs45s verifies the tests sub-budget is exactly 45 seconds
// as required by AC 6.
func TestAC6TestsBudgetIs45s(t *testing.T) {
	t.Parallel()
	if testsBudgetSeconds != 45 {
		t.Errorf("testsBudgetSeconds = %d, want 45", testsBudgetSeconds)
	}
}

// ── 2. No violations within budget ───────────────────────────────────────────

// TestAC6StageBudgetViolationsNilWhenAllWithinBudget verifies that
// StageBudgetViolations returns nil (no violations) when every stage is within
// its per-stage budget: cluster+images ≤ 60s, backend ≤ 15s, tests ≤ 45s.
func TestAC6StageBudgetViolationsNilWhenAllWithinBudget(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 25 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},   // cluster+images = 55s ≤ 60s
		{Name: stageBackendSetup, Duration: 10 * time.Second}, // backend = 10s ≤ 15s
		{Name: stageTestExec, Duration: 30 * time.Second},     // tests = 30s ≤ 45s
	}

	violations := timer.StageBudgetViolations()
	if len(violations) != 0 {
		t.Errorf("StageBudgetViolations() = %v, want nil (all stages within budget)", violations)
	}
}

// ── 3. Cluster+images violation ──────────────────────────────────────────────

// TestAC6StageBudgetViolationsClusterImagesExceeded verifies that
// StageBudgetViolations reports a violation message when the combined
// cluster+images time (stageClusterCreate + stageImageBuild) exceeds 60s.
func TestAC6StageBudgetViolationsClusterImagesExceeded(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 35 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second}, // cluster+images = 65s > 60s
		{Name: stageBackendSetup, Duration: 5 * time.Second},
		{Name: stageTestExec, Duration: 20 * time.Second},
	}

	violations := timer.StageBudgetViolations()
	if len(violations) == 0 {
		t.Fatal("StageBudgetViolations() returned no violations; expected cluster+images violation")
	}
	found := false
	for _, v := range violations {
		if strings.Contains(v, "cluster+images") && strings.Contains(v, "exceeded") {
			found = true
		}
	}
	if !found {
		t.Errorf("StageBudgetViolations() = %v; missing cluster+images exceeded message", violations)
	}
}

// ── 4. Backend violation ─────────────────────────────────────────────────────

// TestAC6StageBudgetViolationsBackendExceeded verifies that
// StageBudgetViolations reports a violation message when stageBackendSetup
// exceeds the 15-second backend sub-budget.
func TestAC6StageBudgetViolationsBackendExceeded(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},   // cluster+images = 50s ≤ 60s
		{Name: stageBackendSetup, Duration: 20 * time.Second}, // backend = 20s > 15s
		{Name: stageTestExec, Duration: 25 * time.Second},
	}

	violations := timer.StageBudgetViolations()
	if len(violations) == 0 {
		t.Fatal("StageBudgetViolations() returned no violations; expected backend violation")
	}
	found := false
	for _, v := range violations {
		if strings.Contains(v, "backend") && strings.Contains(v, "exceeded") {
			found = true
		}
	}
	if !found {
		t.Errorf("StageBudgetViolations() = %v; missing backend exceeded message", violations)
	}
}

// ── 5. Tests violation ───────────────────────────────────────────────────────

// TestAC6StageBudgetViolationsTestsExceeded verifies that
// StageBudgetViolations reports a violation message when stageTestExec exceeds
// the 45-second tests sub-budget.
func TestAC6StageBudgetViolationsTestsExceeded(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},   // cluster+images = 50s ≤ 60s
		{Name: stageBackendSetup, Duration: 10 * time.Second}, // backend = 10s ≤ 15s
		{Name: stageTestExec, Duration: 50 * time.Second},     // tests = 50s > 45s
	}

	violations := timer.StageBudgetViolations()
	if len(violations) == 0 {
		t.Fatal("StageBudgetViolations() returned no violations; expected tests violation")
	}
	found := false
	for _, v := range violations {
		if strings.Contains(v, "tests") && strings.Contains(v, "exceeded") {
			found = true
		}
	}
	if !found {
		t.Errorf("StageBudgetViolations() = %v; missing tests exceeded message", violations)
	}
}

// ── 6. Multiple simultaneous violations ──────────────────────────────────────

// TestAC6StageBudgetViolationsMultipleViolations verifies that
// StageBudgetViolations reports all simultaneous violations when more than one
// stage exceeds its budget — not just the first one encountered.
func TestAC6StageBudgetViolationsMultipleViolations(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 40 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},   // cluster+images = 70s > 60s
		{Name: stageBackendSetup, Duration: 25 * time.Second}, // backend = 25s > 15s
		{Name: stageTestExec, Duration: 50 * time.Second},     // tests = 50s > 45s
	}

	violations := timer.StageBudgetViolations()
	if len(violations) < 3 {
		t.Errorf("StageBudgetViolations() = %v; want ≥ 3 violations (cluster+images, backend, tests)",
			violations)
	}
}

// TestAC6StageBudgetViolationsExactlyOneBudgetViolated verifies that when
// exactly one stage violates its budget, StageBudgetViolations returns exactly
// one violation message (not counting unviolated stages).
func TestAC6StageBudgetViolationsExactlyOneBudgetViolated(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	// Only backend exceeds its budget; cluster+images and tests are within limits.
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 35 * time.Second},   // cluster+images = 55s ≤ 60s
		{Name: stageBackendSetup, Duration: 16 * time.Second}, // backend = 16s > 15s
		{Name: stageTestExec, Duration: 40 * time.Second},     // tests = 40s ≤ 45s
	}

	violations := timer.StageBudgetViolations()
	if len(violations) != 1 {
		t.Errorf("StageBudgetViolations() = %v (len=%d), want exactly 1 violation", violations, len(violations))
	}
}

// ── 7. Emit includes per-stage budget lines ───────────────────────────────────

// TestAC6EmitIncludesPerStageBudgetLines verifies that Emit writes per-stage
// budget status lines for "cluster+images", "backend", and "tests" so the
// summary output is self-contained for AC 6 verification.
func TestAC6EmitIncludesPerStageBudgetLines(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 25 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},
		{Name: stageBackendSetup, Duration: 10 * time.Second},
		{Name: stageTestExec, Duration: 30 * time.Second},
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	for _, label := range []string{"cluster+images", "backend", "tests"} {
		if !strings.Contains(output, label) {
			t.Errorf("Emit output missing per-stage budget line for %q:\n%s", label, output)
		}
	}
}

// ── 8. Emit marks exceeded stage budget ──────────────────────────────────────

// TestAC6EmitMarksStageBudgetExceeded verifies that Emit writes "EXCEEDED" in
// the per-stage budget status line when a stage is over its limit, making the
// violation clearly visible in CI output.
func TestAC6EmitMarksStageBudgetExceeded(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	// backend exceeds its 15s limit.
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},
		{Name: stageBackendSetup, Duration: 20 * time.Second}, // 20s > 15s
		{Name: stageTestExec, Duration: 25 * time.Second},
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	if !strings.Contains(output, "EXCEEDED") {
		t.Errorf("Emit output missing 'EXCEEDED' for over-budget backend stage:\n%s", output)
	}
}

// ── 9. Emit marks all stages OK when within budget ───────────────────────────

// TestAC6EmitMarksAllStagesOKWhenWithinBudget verifies that Emit writes "OK"
// for every per-stage budget status line when all stages are within their
// respective limits — no false positives.
func TestAC6EmitMarksAllStagesOKWhenWithinBudget(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 35 * time.Second},   // cluster+images = 55s ≤ 60s
		{Name: stageBackendSetup, Duration: 10 * time.Second}, // 10s ≤ 15s
		{Name: stageTestExec, Duration: 30 * time.Second},     // 30s ≤ 45s
	}

	var buf bytes.Buffer
	timer.Emit(&buf)
	output := buf.String()

	if strings.Contains(output, "EXCEEDED") {
		t.Errorf("Emit output contains 'EXCEEDED' when all stages are within budget:\n%s", output)
	}
	// All three sub-budget lines should say OK.
	for _, label := range []string{"cluster+images", "backend", "tests"} {
		// Find the line for this label.
		found := false
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, label) && strings.Contains(line, "OK") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Emit output missing 'OK' on %q budget line:\n%s", label, output)
		}
	}
}

// ── 10. Nil receiver safety ──────────────────────────────────────────────────

// TestAC6StageBudgetViolationsNilReceiver verifies that StageBudgetViolations
// is safe to call on a nil *pipelineStageTimer (returns nil, no panic).
func TestAC6StageBudgetViolationsNilReceiver(t *testing.T) {
	t.Parallel()
	var timer *pipelineStageTimer
	violations := timer.StageBudgetViolations()
	if violations != nil {
		t.Errorf("nil receiver StageBudgetViolations() = %v, want nil", violations)
	}
}

// ── boundary cases ────────────────────────────────────────────────────────────

// TestAC6ClusterImagesBudgetExactlyAtLimit verifies the boundary condition:
// cluster+images exactly at 60s is NOT a violation (≤, not <).
func TestAC6ClusterImagesBudgetExactlyAtLimit(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 30 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second}, // exactly 60s
		{Name: stageBackendSetup, Duration: 5 * time.Second},
		{Name: stageTestExec, Duration: 25 * time.Second},
	}

	violations := timer.StageBudgetViolations()
	for _, v := range violations {
		if strings.Contains(v, "cluster+images") {
			t.Errorf("cluster+images at exactly 60s should NOT be a violation, got: %q", v)
		}
	}
}

// TestAC6BackendBudgetExactlyAtLimit verifies the boundary condition: backend
// exactly at 15s is NOT a violation.
func TestAC6BackendBudgetExactlyAtLimit(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},
		{Name: stageBackendSetup, Duration: 15 * time.Second}, // exactly 15s
		{Name: stageTestExec, Duration: 30 * time.Second},
	}

	violations := timer.StageBudgetViolations()
	for _, v := range violations {
		if strings.Contains(v, "backend") {
			t.Errorf("backend at exactly 15s should NOT be a violation, got: %q", v)
		}
	}
}

// TestAC6TestsBudgetExactlyAtLimit verifies the boundary condition: tests
// exactly at 45s is NOT a violation.
func TestAC6TestsBudgetExactlyAtLimit(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		{Name: stageClusterCreate, Duration: 20 * time.Second},
		{Name: stageImageBuild, Duration: 30 * time.Second},
		{Name: stageBackendSetup, Duration: 10 * time.Second},
		{Name: stageTestExec, Duration: 45 * time.Second}, // exactly 45s
	}

	violations := timer.StageBudgetViolations()
	for _, v := range violations {
		if strings.Contains(v, "tests") {
			t.Errorf("tests at exactly 45s should NOT be a violation, got: %q", v)
		}
	}
}

// TestAC6StageBudgetViolationsClusterOnlyExceedsBudget verifies that when only
// stageClusterCreate (without stageImageBuild) exceeds the 60s combined budget,
// StageBudgetViolations still reports it correctly because the combined total
// is what matters.
func TestAC6StageBudgetViolationsClusterOnlyExceedsBudget(t *testing.T) {
	t.Parallel()
	timer := newPipelineStageTimerEnabled()
	timer.stages = []stageMeasurement{
		// stageImageBuild absent; stageClusterCreate alone is 65s > 60s limit
		{Name: stageClusterCreate, Duration: 65 * time.Second},
		{Name: stageBackendSetup, Duration: 5 * time.Second},
		{Name: stageTestExec, Duration: 20 * time.Second},
	}

	violations := timer.StageBudgetViolations()
	if len(violations) == 0 {
		t.Fatal("StageBudgetViolations() returned no violations; expected cluster+images violation")
	}
	found := false
	for _, v := range violations {
		if strings.Contains(v, "cluster+images") {
			found = true
		}
	}
	if !found {
		t.Errorf("StageBudgetViolations() = %v; missing cluster+images violation message", violations)
	}
}
