package e2e

// stage_timer.go — Sub-AC 5.4: wall-clock profiling for the outer make test-e2e
// pipeline.
//
// The stage timer records wall-clock duration for each of the four sequential
// stages that compose a full `make test-e2e` invocation:
//
//	cluster-create:  kind create cluster + kubeconfig export (~30-60s)
//	image-build:     docker build × 3 + kind load docker-image × 3 (~60-90s fresh)
//	backend-setup:   ZFS pool + LVM VG provisioning inside the Kind container (~2-5s)
//	test-exec:       ginkgo worker re-exec (or sequential m.Run()) (~15-45s)
//
// At the end of the pipeline, Emit writes a human-readable breakdown to stderr,
// identifies the bottleneck stage (the one that consumed the most wall-clock
// time), and warns when the total exceeds the 120-second budget.
//
// Activation:
//
//	E2E_STAGE_TIMING=1 (or any non-empty value) enables the summary.
//
// The stage timer complements the per-TC ProfileReport (profile_report.go).
// Use E2E_STAGE_TIMING together with the -e2e.profile flag to get both
// per-TC timing and per-stage pipeline profiling in a single run.
//
// Typical output:
//
//	=== E2E Pipeline Stage Timing ===
//	total pipeline: 95.2s
//	  cluster-create:    34.1s  (35.8%)
//	▶ image-build:       43.5s  (45.7%)
//	  backend-setup:      2.8s  ( 2.9%)
//	  test-exec:         14.8s  (15.5%)
//	bottleneck: image-build (43.5s, 45.7% of pipeline)
//	budget: WITHIN 120s (actual 95.2s)

import (
	"fmt"
	"io"
	"os"
	"time"
)

// pipelineStage is the name of one sequential stage in the make test-e2e pipeline.
type pipelineStage string

const (
	// stageClusterCreate covers `kind create cluster` through kubeconfig export.
	stageClusterCreate pipelineStage = "cluster-create"

	// stageImageBuild covers `docker build` × 3 + `kind load docker-image` × 3.
	stageImageBuild pipelineStage = "image-build"

	// stageBackendSetup covers ZFS pool + LVM VG provisioning inside the Kind
	// container.
	stageBackendSetup pipelineStage = "backend-setup"

	// stageTestExec covers the ginkgo CLI re-exec (or sequential m.Run()).
	stageTestExec pipelineStage = "test-exec"

	// envStageTiming enables stage timing output when set to a non-empty value.
	// Equivalent to passing --stage-timing on the command line. Set this in the
	// shell or via make: make test-e2e E2E_STAGE_TIMING=1
	envStageTiming = "E2E_STAGE_TIMING"

	// stageBudgetSeconds is the maximum allowed total pipeline wall-clock time
	// in seconds. Exceeding this threshold causes Emit to print a WARNING line.
	stageBudgetSeconds = 120

	// AC 6 per-stage time budgets (seconds).
	//
	// The three sub-budgets sum to the total 120s budget:
	//   clusterImagesBudgetSeconds + backendBudgetSeconds + testsBudgetSeconds
	//   = 60 + 15 + 45 = 120s
	//
	// clusterImagesBudgetSeconds covers stageClusterCreate + stageImageBuild
	// combined (the two sequential provisioning stages).
	clusterImagesBudgetSeconds = 60

	// backendBudgetSeconds covers stageBackendSetup (ZFS pool + LVM VG).
	backendBudgetSeconds = 15

	// testsBudgetSeconds covers stageTestExec (all Ginkgo workers).
	testsBudgetSeconds = 45
)

// pipelineStageOrder is the canonical display order for stage timing output.
// Stages are displayed in execution order even if they were recorded out of order.
var pipelineStageOrder = []pipelineStage{
	stageClusterCreate,
	stageImageBuild,
	stageBackendSetup,
	stageTestExec,
}

// stageMeasurement records the wall-clock duration of a single pipeline stage.
type stageMeasurement struct {
	Name     pipelineStage
	Duration time.Duration
}

// pipelineStageTimer is a minimal sequential stage profiler for the outer
// make test-e2e pipeline.  It is intentionally simple: each stage is started
// by StartStage, which returns a done() closure, and the final summary is
// written by Emit.
//
// All methods are safe to call on a nil receiver (no-ops).
//
// Usage:
//
//	timer := newPipelineStageTimer()
//
//	done := timer.StartStage(stageClusterCreate)
//	state, err := bootstrapSuiteCluster(...)
//	done()
//
//	done = timer.StartStage(stageImageBuild)
//	err = bootstrapSuiteImages(...)
//	done()
//
//	// ... more stages ...
//
//	defer timer.Emit(os.Stderr)
type pipelineStageTimer struct {
	enabled    bool
	stages     []stageMeasurement
	stageStart time.Time
	stageName  pipelineStage
}

// newPipelineStageTimer creates a stage timer. Timing output is enabled when
// E2E_STAGE_TIMING is set to a non-empty value.
func newPipelineStageTimer() *pipelineStageTimer {
	return &pipelineStageTimer{
		enabled: os.Getenv(envStageTiming) != "",
	}
}

// newPipelineStageTimerEnabled creates a stage timer with timing always enabled.
// Used by tests to verify the timer behaviour without setting an env var.
func newPipelineStageTimerEnabled() *pipelineStageTimer {
	return &pipelineStageTimer{enabled: true}
}

// newPipelineStageTimerDisabled creates a stage timer with timing always disabled.
// Used by tests to verify no-op behaviour without env var manipulation, allowing
// t.Parallel() to be used safely alongside other concurrent tests.
func newPipelineStageTimerDisabled() *pipelineStageTimer {
	return &pipelineStageTimer{enabled: false}
}

// StartStage records the start of a named pipeline stage and returns a done
// function that finalises the stage duration when called.  If a previous stage
// was started but not finished, it is finalised first.
//
// Typical usage:
//
//	done := timer.StartStage(stageImageBuild)
//	defer done() // or call explicitly before the next StartStage
func (t *pipelineStageTimer) StartStage(name pipelineStage) func() {
	if t == nil || !t.enabled {
		return func() {}
	}
	// Finalise any previously started stage.
	t.finishStage()
	t.stageName = name
	t.stageStart = time.Now()
	called := false
	return func() {
		if called {
			return
		}
		called = true
		t.finishStage()
	}
}

// finishStage appends a measurement for the current stage.
// It is idempotent: calling it when no stage is in progress is a no-op.
func (t *pipelineStageTimer) finishStage() {
	if t == nil || t.stageName == "" {
		return
	}
	elapsed := time.Since(t.stageStart)
	t.stages = append(t.stages, stageMeasurement{
		Name:     t.stageName,
		Duration: elapsed,
	})
	t.stageName = ""
}

// TotalDuration returns the sum of all recorded stage durations.
func (t *pipelineStageTimer) TotalDuration() time.Duration {
	if t == nil {
		return 0
	}
	var total time.Duration
	for _, s := range t.stages {
		total += s.Duration
	}
	return total
}

// Bottleneck returns the stage measurement with the longest wall-clock duration.
// When no stages have been recorded it returns a zero-value measurement.
func (t *pipelineStageTimer) Bottleneck() stageMeasurement {
	if t == nil || len(t.stages) == 0 {
		return stageMeasurement{}
	}
	best := t.stages[0]
	for _, s := range t.stages[1:] {
		if s.Duration > best.Duration {
			best = s
		}
	}
	return best
}

// RecordedStages returns a copy of the recorded stage measurements in
// insertion order.  Useful for tests.
func (t *pipelineStageTimer) RecordedStages() []stageMeasurement {
	if t == nil || len(t.stages) == 0 {
		return nil
	}
	out := make([]stageMeasurement, len(t.stages))
	copy(out, t.stages)
	return out
}

// StageBudgetViolations returns a list of human-readable per-stage budget
// violation messages for the AC 6 sub-budgets:
//
//	cluster+images ≤ 60s  (stageClusterCreate + stageImageBuild combined)
//	backend         ≤ 15s  (stageBackendSetup)
//	tests           ≤ 45s  (stageTestExec)
//
// Returns nil when all stages are within their budgets.
// Safe to call on a nil receiver (returns nil).
func (t *pipelineStageTimer) StageBudgetViolations() []string {
	if t == nil {
		return nil
	}
	stageMap := make(map[pipelineStage]time.Duration, len(t.stages))
	for _, s := range t.stages {
		stageMap[s.Name] += s.Duration
	}

	clusterImages := stageMap[stageClusterCreate] + stageMap[stageImageBuild]
	backend := stageMap[stageBackendSetup]
	tests := stageMap[stageTestExec]

	var violations []string
	limit := time.Duration(clusterImagesBudgetSeconds) * time.Second
	if clusterImages > limit {
		violations = append(violations, fmt.Sprintf(
			"cluster+images %s exceeded %ds budget", fmtDur(clusterImages), clusterImagesBudgetSeconds))
	}
	limit = time.Duration(backendBudgetSeconds) * time.Second
	if backend > limit {
		violations = append(violations, fmt.Sprintf(
			"backend %s exceeded %ds budget", fmtDur(backend), backendBudgetSeconds))
	}
	limit = time.Duration(testsBudgetSeconds) * time.Second
	if tests > limit {
		violations = append(violations, fmt.Sprintf(
			"tests %s exceeded %ds budget", fmtDur(tests), testsBudgetSeconds))
	}
	return violations
}

// Emit writes the stage timing summary to output and returns whether the
// pipeline finished within the 120-second budget.
//
// When the timer is disabled, output is nil, or no stages have been recorded,
// Emit is a no-op and returns true.
//
// Emit finalises any in-progress stage before computing the summary.
func (t *pipelineStageTimer) Emit(output io.Writer) (withinBudget bool) {
	if t == nil || !t.enabled || output == nil {
		return true
	}
	// Finalise any stage that was started but not explicitly closed.
	t.finishStage()
	if len(t.stages) == 0 {
		return true
	}

	// Build a name→total-duration index (accumulate in case a stage was
	// recorded more than once, e.g. in retry scenarios).
	stageMap := make(map[pipelineStage]time.Duration, len(t.stages))
	for _, s := range t.stages {
		stageMap[s.Name] += s.Duration
	}

	total := t.TotalDuration()
	bottleneck := t.Bottleneck()

	_, _ = fmt.Fprintln(output, "=== E2E Pipeline Stage Timing ===")
	_, _ = fmt.Fprintf(output, "total pipeline: %s\n", fmtDur(total))

	for _, name := range pipelineStageOrder {
		dur := stageMap[name]
		var pct float64
		if total > 0 {
			pct = float64(dur) / float64(total) * 100
		}
		marker := "  "
		if name == bottleneck.Name {
			marker = "▶ "
		}
		_, _ = fmt.Fprintf(output, "%s%-18s %8s  (%4.1f%%)\n",
			marker, string(name)+":", fmtDur(dur), pct)
	}

	if bottleneck.Name != "" {
		var pct float64
		if total > 0 {
			pct = float64(bottleneck.Duration) / float64(total) * 100
		}
		_, _ = fmt.Fprintf(output,
			"bottleneck: %s (%s, %.1f%% of pipeline)\n",
			bottleneck.Name, fmtDur(bottleneck.Duration), pct)
	}

	withinBudget = total <= stageBudgetSeconds*time.Second
	if withinBudget {
		_, _ = fmt.Fprintf(output, "budget: WITHIN 120s (actual %s)\n", fmtDur(total))
	} else {
		_, _ = fmt.Fprintf(output,
			"WARNING: pipeline exceeded 120s budget (actual %s)\n", fmtDur(total))
	}

	// AC 6 per-stage budget reporting: emit per-stage budget status so that the
	// caller can identify which sub-budget was violated in addition to the total.
	// stageMap was already built above; reuse it directly.
	clusterImages := stageMap[stageClusterCreate] + stageMap[stageImageBuild]
	backend := stageMap[stageBackendSetup]
	tests := stageMap[stageTestExec]

	budgetOK := func(actual time.Duration, limitSec int) string {
		limit := time.Duration(limitSec) * time.Second
		if actual <= limit {
			return fmt.Sprintf("OK (%s ≤ %ds)", fmtDur(actual), limitSec)
		}
		return fmt.Sprintf("EXCEEDED (%s > %ds)", fmtDur(actual), limitSec)
	}
	_, _ = fmt.Fprintf(output, "  cluster+images: %s\n", budgetOK(clusterImages, clusterImagesBudgetSeconds))
	_, _ = fmt.Fprintf(output, "  backend:        %s\n", budgetOK(backend, backendBudgetSeconds))
	_, _ = fmt.Fprintf(output, "  tests:          %s\n", budgetOK(tests, testsBudgetSeconds))

	return withinBudget
}

// fmtDur formats a duration to one decimal place (e.g. "43.5s" or "1.2ms").
func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	default:
		return d.String()
	}
}
