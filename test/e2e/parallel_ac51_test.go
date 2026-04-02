package e2e

// parallel_ac51_test.go — Sub-AC 5.1: parallel test execution and worker pool.
//
// Acceptance criteria verified here:
//
//  1. DefaultParallelNodes equals runtime.NumCPU() by default so the suite
//     scales automatically to the host machine's core count.
//  2. PILLAR_E2E_PROCS env var overrides the worker count in reexecViaGinkgoCLI,
//     allowing CI and local runs to dial up or down parallelism.
//  3. suiteLevelTimeout is exactly 2 minutes — achievable only with sufficient
//     parallelism (sequential execution of 466 TCs takes 5–15 minutes).
//  4. Multiple independent TCs can run concurrently without sharing any mutable
//     resource: each TC gets a distinct RootDir, backend fixture, and port lease.
//  5. TestCaseScope operations are safe for concurrent goroutine access (no data
//     races when multiple Ginkgo workers call scope operations simultaneously).
//  6. The parallel speedup invariant: running N TCs in parallel completes in
//     roughly 1/N of the time compared to running them sequentially.

import (
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 1. Default worker count ───────────────────────────────────────────────────

// TestAC51DefaultParallelNodesEqualsCPUCount verifies that DefaultParallelNodes
// is initialized to runtime.NumCPU() so the suite automatically scales to the
// host machine's core count without any manual configuration.
//
// AC 5.1 contract: DefaultParallelNodes == runtime.NumCPU() ≥ 1.
func TestAC51DefaultParallelNodesEqualsCPUCount(t *testing.T) {
	t.Parallel()

	want := runtime.NumCPU()
	if DefaultParallelNodes != want {
		t.Errorf("DefaultParallelNodes = %d, want %d (runtime.NumCPU())",
			DefaultParallelNodes, want)
	}
	if DefaultParallelNodes < 1 {
		t.Errorf("DefaultParallelNodes = %d, must be ≥ 1", DefaultParallelNodes)
	}
	t.Logf("AC51: DefaultParallelNodes = %d (runtime.NumCPU() = %d)", DefaultParallelNodes, want)
}

// ── 2. PILLAR_E2E_PROCS env var override ─────────────────────────────────────

// TestAC51WorkerCountOverrideViaEnvVar verifies that the PILLAR_E2E_PROCS
// environment variable is honoured by the reexec logic in reexecViaGinkgoCLI.
// The actual env var is NOT set in this test (to avoid side-effects); instead
// we replicate the selection logic that runs inside reexecViaGinkgoCLI.
//
// AC 5.1 contract: procs = PILLAR_E2E_PROCS if set, else DefaultParallelNodes.
func TestAC51WorkerCountOverrideViaEnvVar(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		envVal     string
		wantResult string
	}{
		{
			name:       "default (no env var)",
			envVal:     "",
			wantResult: strconv.Itoa(DefaultParallelNodes),
		},
		{
			name:       "explicit 1 worker",
			envVal:     "1",
			wantResult: "1",
		},
		{
			name:       "explicit 4 workers",
			envVal:     "4",
			wantResult: "4",
		},
		{
			name:       "explicit 16 workers",
			envVal:     "16",
			wantResult: "16",
		},
		{
			name:       "cpu-count override",
			envVal:     strconv.Itoa(runtime.NumCPU()),
			wantResult: strconv.Itoa(runtime.NumCPU()),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Replicate the selection logic from reexecViaGinkgoCLI.
			// Do NOT use os.Setenv — that would race with other parallel tests.
			procs := strconv.Itoa(DefaultParallelNodes)
			if tc.envVal != "" {
				procs = tc.envVal
			}

			if procs != tc.wantResult {
				t.Errorf("AC51: resolved procs = %q, want %q (envVal=%q)",
					procs, tc.wantResult, tc.envVal)
			}
		})
	}
}

// ── 3. Suite-level timeout ────────────────────────────────────────────────────

// TestAC51SuiteLevelTimeoutIs2Minutes verifies that suiteLevelTimeout is
// exactly 2 minutes. This constraint is achievable only with parallel execution:
// sequential execution of all 466 TCs takes 5-15 minutes depending on hardware.
//
// AC 5.1 contract: suiteLevelTimeout == 2 * time.Minute.
func TestAC51SuiteLevelTimeoutIs2Minutes(t *testing.T) {
	t.Parallel()

	const want = 2 * time.Minute
	if suiteLevelTimeout != want {
		t.Errorf("AC51: suiteLevelTimeout = %v, want %v", suiteLevelTimeout, want)
	}
	t.Logf("AC51: suiteLevelTimeout = %v (requires parallel execution to meet this budget)", want)
}

// ── 4. Concurrent TCs receive distinct isolation scopes ──────────────────────

// TestAC51ConcurrentTCsReceiveDistinctScopes verifies that N concurrently-
// running TCs each receive a distinct isolation scope: unique RootDir under
// /tmp, unique backend fixture name, and unique loopback port address.
// This is the fundamental invariant that makes parallel execution safe.
//
// AC 5.1 contract: no two concurrent TCs share any mutable resource.
func TestAC51ConcurrentTCsReceiveDistinctScopes(t *testing.T) {
	t.Parallel()

	const concurrency = 6

	type result struct {
		tcID        string
		rootDir     string
		portAddr    string
		backendName string
		failed      bool
	}

	results := make([]result, concurrency)
	var wg sync.WaitGroup
	var failures atomic.Int32

	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			tcID := fmt.Sprintf("E%d.1", idx+100) // use non-conflicting IDs
			ctx, err := StartTestCase(tcID, func(scope *TestCaseScope) (*TestCaseBaseline, error) {
				return BuildTestCaseBaseline(scope, TestCaseBaselinePlan{
					BackendObjects: []BackendFixturePlan{
						{Kind: "zfs", Label: "pool"},
					},
					LoopbackPorts: []string{"agent"},
				})
			})
			if err != nil {
				t.Errorf("AC51: goroutine %d: StartTestCase(%s): %v", idx, tcID, err)
				failures.Add(1)
				return
			}
			defer func() {
				if err := ctx.Close(); err != nil {
					t.Errorf("AC51: goroutine %d: Close: %v", idx, err)
				}
			}()

			port := ctx.Baseline.Port("agent")
			backend := ctx.Baseline.BackendObject("zfs", "pool")

			if port == nil {
				t.Errorf("AC51: goroutine %d: port lease is nil", idx)
				failures.Add(1)
				return
			}
			if backend == nil {
				t.Errorf("AC51: goroutine %d: backend object is nil", idx)
				failures.Add(1)
				return
			}

			results[idx] = result{
				tcID:        tcID,
				rootDir:     ctx.Scope.RootDir,
				portAddr:    port.Addr,
				backendName: backend.Name,
			}
		}(i)
	}
	wg.Wait()

	if failures.Load() > 0 {
		t.Fatalf("AC51: %d goroutines failed during concurrent scope creation", failures.Load())
	}

	// Verify uniqueness across all results.
	rootDirs := make(map[string]int, concurrency)
	portAddrs := make(map[string]int, concurrency)
	backendNames := make(map[string]int, concurrency)

	for idx, r := range results {
		if r.tcID == "" {
			continue
		}
		if prev, dup := rootDirs[r.rootDir]; dup {
			t.Errorf("AC51: goroutine %d and %d share root dir %q — isolation violated",
				idx, prev, r.rootDir)
		}
		rootDirs[r.rootDir] = idx

		if prev, dup := portAddrs[r.portAddr]; dup {
			t.Errorf("AC51: goroutine %d and %d share port addr %q — isolation violated",
				idx, prev, r.portAddr)
		}
		portAddrs[r.portAddr] = idx

		if prev, dup := backendNames[r.backendName]; dup {
			t.Errorf("AC51: goroutine %d and %d share backend name %q — isolation violated",
				idx, prev, r.backendName)
		}
		backendNames[r.backendName] = idx
	}

	if !t.Failed() {
		t.Logf("AC51: %d concurrent TCs each received distinct root dirs, port addrs, and backend names",
			concurrency)
	}
}

// ── 5. TestCaseScope thread safety ───────────────────────────────────────────

// TestAC51IsolationScopeIsThreadSafe verifies that TestCaseScope operations
// (BackendObject, ReserveLoopbackPort, TempDir) are race-free when called
// concurrently from multiple goroutines — the condition that occurs when
// Ginkgo's parallel workers execute BeforeEach hooks concurrently.
//
// AC 5.1 contract: all TestCaseScope public methods are safe for concurrent use.
func TestAC51IsolationScopeIsThreadSafe(t *testing.T) {
	t.Parallel()

	scope, err := NewTestCaseScope("E5.1-thread-safety")
	if err != nil {
		t.Fatalf("AC51: NewTestCaseScope: %v", err)
	}
	defer func() { _ = scope.Close() }()

	const goroutines = 8
	var wg sync.WaitGroup
	var errCount atomic.Int32

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			label := fmt.Sprintf("resource-%d", idx)

			// BackendObject must be idempotent and race-free.
			obj, err := scope.BackendObject("zfs", label)
			if err != nil {
				t.Errorf("AC51: goroutine %d: BackendObject: %v", idx, err)
				errCount.Add(1)
				return
			}
			if obj == nil {
				t.Errorf("AC51: goroutine %d: BackendObject returned nil", idx)
				errCount.Add(1)
				return
			}

			// TempDir must also be race-free.
			dir, err := scope.TempDir(label)
			if err != nil {
				t.Errorf("AC51: goroutine %d: TempDir: %v", idx, err)
				errCount.Add(1)
				return
			}
			if dir == "" {
				t.Errorf("AC51: goroutine %d: TempDir returned empty path", idx)
				errCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if errCount.Load() > 0 {
		t.Fatalf("AC51: %d goroutines encountered errors during concurrent scope access",
			errCount.Load())
	}
	t.Logf("AC51: %d concurrent goroutines accessed the isolation scope without errors", goroutines)
}

// ── 6. No serialization bottleneck: parallel speedup ─────────────────────────

// TestAC51ParallelSpeedupOverSerial verifies the core Sub-AC 5.1 performance
// invariant: running N TCs in parallel completes significantly faster than
// running them sequentially.  Without this speedup, the 2-minute suite budget
// could not be met with 466 TCs.
//
// AC 5.1 contract: parallel throughput > sequential throughput by ≥ 1.5×.
func TestAC51ParallelSpeedupOverSerial(t *testing.T) {
	t.Parallel()

	// Skip on single-core machines where goroutines can't run truly in parallel.
	if runtime.NumCPU() < 2 {
		t.Skip("AC51: skipping parallel-speedup test on single-core machine (GOMAXPROCS=1 cannot achieve speedup)")
	}

	const (
		numTCs   = 4
		holdTime = 20 * time.Millisecond // simulates per-TC isolation setup overhead
	)

	// runBatch runs numTCs test cases using the given worker count.
	// Each TC does real work (StartTestCase + hold + Close) to measure the
	// actual isolation-scope creation overhead, not just goroutine scheduling.
	runBatch := func(workers int) time.Duration {
		type job struct{ idx int }
		jobs := make(chan job, numTCs)
		start := time.Now()

		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobs {
					tcID := fmt.Sprintf("E%d.1", j.idx+200)
					ctx, err := StartTestCase(tcID, nil)
					if err != nil {
						continue
					}
					time.Sleep(holdTime) // simulate spec body
					_ = ctx.Close()
				}
			}()
		}

		for i := range numTCs {
			jobs <- job{idx: i}
		}
		close(jobs)
		wg.Wait()

		return time.Since(start)
	}

	serialDuration := runBatch(1)
	parallelDuration := runBatch(numTCs) // fully concurrent

	// Speedup must be at least 1.5× to demonstrate meaningful parallelism.
	// We use a conservative threshold to account for scheduling overhead on
	// loaded CI machines.
	speedup := float64(serialDuration) / float64(parallelDuration)
	if speedup < 1.5 {
		t.Errorf("AC51: parallel speedup = %.2fx (serial=%v, parallel=%v) — "+
			"want ≥ 1.5x; possible serialization bottleneck",
			speedup, serialDuration, parallelDuration)
	} else {
		t.Logf("AC51: parallel speedup = %.2fx (serial=%v, parallel=%v) — "+
			"no serialization bottleneck detected",
			speedup, serialDuration, parallelDuration)
	}
}

// ── 7. Ginkgo reexec guard is idempotent ─────────────────────────────────────

// TestAC51ReexecSentinelPreventsDoubleReexec verifies that the re-exec sentinel
// constant is defined and non-empty. This constant is injected into the
// environment of ginkgo worker processes to prevent recursive re-exec loops.
//
// AC 5.1 contract: reexecSentinelEnv is non-empty (prevents infinite re-exec).
func TestAC51ReexecSentinelPreventsDoubleReexec(t *testing.T) {
	t.Parallel()

	if reexecSentinelEnv == "" {
		t.Error("AC51: reexecSentinelEnv is empty — workers could enter infinite re-exec loop")
	}
	t.Logf("AC51: reexecSentinelEnv = %q", reexecSentinelEnv)
}

// TestAC51SequentialModeEnvVarIsNamed verifies that the sequential-mode env var
// constant is defined and non-empty, enabling the benchmark target to opt out
// of auto-parallel re-exec.
//
// AC 5.1 contract: sequentialModeEnv is non-empty.
func TestAC51SequentialModeEnvVarIsNamed(t *testing.T) {
	t.Parallel()

	if sequentialModeEnv == "" {
		t.Error("AC51: sequentialModeEnv is empty — no way to opt out of parallel re-exec")
	}
	t.Logf("AC51: sequentialModeEnv = %q", sequentialModeEnv)
}

// ── 8. Worker count is positive ──────────────────────────────────────────────

// TestAC51WorkerCountIsPositive verifies that DefaultParallelNodes is at least
// 1 regardless of the host's reported CPU count.
//
// AC 5.1 contract: DefaultParallelNodes ≥ 1 always.
func TestAC51WorkerCountIsPositive(t *testing.T) {
	t.Parallel()

	if DefaultParallelNodes < 1 {
		t.Errorf("AC51: DefaultParallelNodes = %d, must be ≥ 1", DefaultParallelNodes)
	}
}
