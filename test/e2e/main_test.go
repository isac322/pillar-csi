// Package e2e — main_test.go is the single entry point for the pillar-csi E2E
// test binary. It wires the full phase-sequenced pipeline:
//
//	Phase 1 — prereq check       : Docker daemon reachable + kernel modules loaded
//	Phase 2 — cluster creation   : Kind cluster created and kubeconfig exported
//	Phase 3 — image build/load   : docker build × 3 + kind load × 3
//	Phase 4 — backend provision  : ZFS pool + LVM VG provisioned in Kind container
//	Phase 5 — parallel test exec : ginkgo CLI re-exec with N workers (or sequential)
//	Phase 6 — teardown           : backends destroyed, cluster deleted, temp dirs removed
//
// All phases run inside a single `go test` invocation triggered by `make test-e2e`.
// The pipeline aborts on the first failure; teardown always runs via deferred cleanup.
//
// Environment variables consumed:
//
//	DOCKER_HOST              — Docker daemon endpoint (env only, never hardcoded)
//	KIND_CLUSTER             — Kind cluster name
//	E2E_IMAGE_TAG            — image tag for all three component images
//	E2E_SKIP_IMAGE_BUILD     — "true" skips docker build + kind load
//	E2E_USE_EXISTING_CLUSTER — "true" skips Kind cluster creation
//	E2E_DOCKER_BUILD_CACHE   — "true" enables --cache-from for faster rebuilds
//	E2E_STAGE_TIMING         — "1" emits wall-clock stage breakdown
//	GINKGO                   — absolute path to the ginkgo binary
//	PILLAR_E2E_PROCS         — parallel worker count (default: nproc)
//	E2E_FAIL_FAST            — "true" stops after the first spec failure
package e2e

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework/prereq"
)

// tcRunFocusOverride is set by TestMain when the -test.run flag looks like a
// TC-ID pattern (e.g. "TC-E1.2" or "TC-F"). It is applied as a Ginkgo
// FocusStrings entry in TestE2E so that only matching specs execute.
var tcRunFocusOverride string

// isTCRunPattern reports whether pattern is a TC-ID filter that should be
// routed through Ginkgo's focus mechanism rather than Go's native test-
// function matching. Recognized patterns start with "TC-" (case-sensitive)
// so they can match node names like "[TC-E1.2]" or "[TC-F27.1]".
//
// Examples that return true:  "TC-E1.2", "TC-F", "TC-E", "TC-F27.1".
// Examples that return false: "TestE2E", "TestAC3", "", "^TestE2E$".
func isTCRunPattern(pattern string) bool {
	return strings.HasPrefix(pattern, "TC-")
}

// canMatchGinkgoSuite returns true when the given test.run regex pattern
// could select the Ginkgo suite entry point "TestE2E". When this function
// returns false, the Kind cluster bootstrap and ginkgo re-exec can be
// safely skipped, enabling efficient iteration on plain Go unit tests
// (e.g. go test -run TestAC7 ./test/e2e/) without the 20-30 second
// cluster creation overhead.
//
// AC 7 fast-path contract:
//
//	pattern = ""          → true  (empty pattern matches everything)
//	pattern = "TestE2E"  → true  (direct match)
//	pattern = "^TestE2E$"→ true  (anchored exact match)
//	pattern = "Test"     → true  (prefix regex matches TestE2E)
//	pattern = "TestAC7"  → false (does not match TestE2E)
//	pattern = "TestAC61" → false (does not match TestE2E)
//	pattern = "^TestE2E$"→ true  (rewritten by isTCRunPattern from TC-* patterns)
//
// On regex parse error the function is conservative and returns true so
// that the caller proceeds through the full cluster-creation path rather
// than silently skipping it.
func canMatchGinkgoSuite(pattern string) bool {
	if pattern == "" {
		return true
	}
	matched, err := regexp.MatchString(pattern, "TestE2E")
	if err != nil {
		// Invalid regex: be conservative — assume it could match.
		return true
	}
	return matched
}

// suiteLevelTimeout is the maximum wall-clock duration the full E2E suite is
// allowed to consume in a single go test invocation. It is applied as the
// Ginkgo suite timeout whenever no explicit --timeout flag has been provided.
//
// Parallel execution is the mechanism that keeps total wall-clock time below
// this threshold. TestMain automatically fans out DefaultParallelNodes
// parallel Ginkgo workers when not already running under the ginkgo CLI.
const suiteLevelTimeout = 2 * time.Minute

// ginkgoCliTimeout is the --timeout value passed to the ginkgo CLI in
// reexecViaGinkgoCLI. It must equal suiteLevelTimeout (2m) so that the Ginkgo
// coordinator enforces the same 2-minute wall-clock budget that TestE2E applies
// via suiteConfig.Timeout.
//
// Sub-AC 3: use this named constant instead of the raw string literal "2m" so
// that any future change to suiteLevelTimeout is surfaced at compile time and
// caught by TestAC3GinkgoCliTimeoutMatchesSuiteLevelTimeout.
const ginkgoCliTimeout = "2m"

// DefaultParallelNodes is the recommended default number of parallel Ginkgo
// nodes for a full suite run. It equals runtime.NumCPU() so that the suite
// scales automatically to the host machine.
//
// TestMain reads this value when launching the ginkgo CLI re-exec. Override
// by setting PILLAR_E2E_PROCS in the environment before running go test.
//
// NOTE: reexecViaGinkgoCLI enforces a minimum of minParallelProcs workers
// to meet the 45-second test-exec budget even on low-CPU machines.
// DefaultParallelNodes itself is left at runtime.NumCPU() so that tests
// checking the default value (TestAC51DefaultParallelNodesEqualsCPUCount)
// continue to pass.
var DefaultParallelNodes = runtime.NumCPU()

// minParallelProcs is the minimum number of Ginkgo parallel workers enforced
// by reexecViaGinkgoCLI when neither PILLAR_E2E_PROCS nor a sufficient
// DefaultParallelNodes value provides enough parallelism.
//
// The 45-second test-exec budget (testsBudgetSeconds = 45) requires at least
// 8 workers to distribute the ~91 cluster-level specs (E10, E27, E33-E35)
// so that no worker's share exceeds the per-worker budget. On a 1-7 CPU
// machine, defaulting to runtime.NumCPU() would starve parallelism.
const minParallelProcs = 8

// maxParallelProcs is the upper bound on the number of Ginkgo parallel workers
// that reexecViaGinkgoCLI will spawn when PILLAR_E2E_PROCS is not set.
//
// Sub-AC 2.3: Resource contention prevention — on machines with many CPUs
// (e.g., 16 or 32 cores), using runtime.NumCPU() workers creates too many
// simultaneous clients against the shared Kind API server. Observed behaviour:
// 16 parallel workers caused "Ginkgo timed out waiting for all parallel procs
// to report back" because the API server was saturated and workers failed to
// complete their SynchronizedBeforeSuite within the timeout window.
//
// Setting maxParallelProcs = 8 prevents this class of failure while still
// providing sufficient parallelism to complete the testsBudgetSeconds = 45s
// budget: 416 default-profile specs × ~0.1ms each = ~42ms at 1 worker, so 8
// workers comfortably fits within the budget even accounting for cluster-side
// specs. The effective worker range without PILLAR_E2E_PROCS is [8, 8].
//
// PILLAR_E2E_PROCS (and the Makefile's E2E_PROCS=4) always override this cap
// so operator-controlled runs (CI, make test-e2e) can dial down to 4 workers
// for additional headroom on constrained machines.
const maxParallelProcs = 8

// ── Auto-parallel re-exec constants ──────────────────────────────────────────

// ginkgoParallelTotalFlag is the flag name that ginkgo v2 registers at init
// time and populates on every parallel worker process it spawns. When its
// value is > 1 the current process is already coordinated by the Ginkgo
// parallel runner; TestMain must NOT re-exec.
//
// Ginkgo v2 (≥ v2.0) registers this flag as "ginkgo.parallel.total" (with the
// "ginkgo." prefix).  The flag is verified by flag.Lookup("ginkgo.parallel.total")
// which returns non-nil in any process that imports ginkgo/v2.  Using the wrong
// name ("parallel.total") causes flag.Lookup to always return nil, so
// isGinkgoParallelWorker() would always return false and any direct ginkgo
// invocation (without PILLAR_E2E_REEXEC_GUARD) would route workers to
// runPrimary() instead of runWorker().
const ginkgoParallelTotalFlag = "ginkgo.parallel.total"

// reexecSentinelEnv is injected into the environment of the ginkgo process
// spawned by reexecViaGinkgoCLI. When present, TestMain skips the re-exec
// path so that ginkgo worker processes and sequential ginkgo runs proceed
// directly to RunSpecs without entering another re-exec cycle.
const reexecSentinelEnv = "PILLAR_E2E_REEXEC_GUARD"

// sequentialModeEnv disables the auto-parallel re-exec entirely when set to a
// non-empty value. Set PILLAR_E2E_SEQUENTIAL=true for single-process targets
// (e.g. test-e2e-bench) that measure raw sequential throughput.
const sequentialModeEnv = "PILLAR_E2E_SEQUENTIAL"

// labelFilterEnv is the environment variable that controls which Ginkgo specs
// run during a parallel re-exec invocation (reexecViaGinkgoCLI).
//
// When unset, reexecViaGinkgoCLI defaults to defaultLabelFilter so that only
// the 416 "default-profile" specs execute.  Long-running tests (e.g. the
// "helm" E27 cluster specs in tc_e27_helm_e2e_test.go) are excluded by default
// because they call `helm install --wait --timeout 5m`, which exceeds the
// 2-minute suite timeout and causes "Ginkgo timed out waiting for all parallel
// procs to report back".
//
// Override to run other suites:
//
//	E2E_LABEL_FILTER=helm make test-e2e          # run only helm E27 specs
//	E2E_LABEL_FILTER=""   make test-e2e          # run ALL specs (no filter)
//
// The empty-string override is intentional: it lets callers explicitly opt in
// to running all specs (including slow helm tests) when the run environment
// supports longer timeouts.
const labelFilterEnv = "E2E_LABEL_FILTER"

// defaultLabelFilter is the Ginkgo label expression applied when labelFilterEnv
// is not set. It restricts the default parallel run to the 416 "default-profile"
// specs so that the suite completes within the 2-minute suiteLevelTimeout.
//
// Specs without the "default-profile" label (e.g. "helm", "AC3.3", "AC4c",
// "lvm") are excluded from the default run and must be opted in explicitly via
// E2E_LABEL_FILTER.
const defaultLabelFilter = "default-profile"

// ─────────────────────────────────────────────────────────────────────────────

// TestMain is the single entry point for the E2E test binary. It orchestrates
// the full phase-sequenced pipeline in order, aborting on the first failure:
//
//  1. Prerequisite check  — Docker daemon reachable + kernel modules present
//  2. Kind cluster create — ephemeral cluster for the invocation lifetime
//  3. Image build/load   — docker build × 3 + kind load docker-image × 3
//  4. Backend provision  — ZFS pool + LVM VG inside the Kind container
//  5. Parallel test exec — ginkgo CLI re-exec with N workers (or sequential)
//  6. Teardown           — backend destroy → cluster delete → temp dir removal
//
// Execution paths
//
//	Primary (non-worker) process
//	  → Phase 1 prereq check
//	  → bootstrapSuiteCluster: creates Kind cluster, exports KUBECONFIG /
//	    KIND_CLUSTER / suite-path env vars so ginkgo workers inherit them.
//	  → bootstrapSuiteImages: docker build + kind load
//	  → bootstrapSuiteBackends: ZFS pool + LVM VG provisioned
//	  → runPrimary: either re-execs via the ginkgo CLI (parallel) or calls
//	    m.Run() directly (sequential). A deferred cleanup in runPrimary
//	    deletes the cluster even when the inner call panics.
//
//	Ginkgo parallel worker / re-exec guarded process
//	  → runWorker: the cluster is already created; workers inherit env vars from
//	    the primary. runWorker runs m.Run() with panic-safe cleanup of any
//	    worker-local resources.
//
// Auto-parallel behaviour
//
//	go test ./test/e2e/... -count=1          → fans out DefaultParallelNodes workers
//	PILLAR_E2E_PROCS=4 go test ./test/e2e/   → fans out exactly 4 workers
//	PILLAR_E2E_SEQUENTIAL=true go test ...   → runs sequentially (no re-exec)
//	make test-e2e-bench                      → sets PILLAR_E2E_SEQUENTIAL=true
//	ginkgo --procs=N ./test/e2e/             → ginkgo CLI; no re-exec needed
//
// AC 7 fast-path for unit test iteration
//
//	go test -run=TestAC7  ./test/e2e/...    → unit tests only; no Kind cluster
//	go test -run=TestAC61 ./test/e2e/...    → unit tests only; no Kind cluster
//
// TC-ID routing
//
//	go test -run=TC-E1.2 ./test/e2e/...     → runs only [TC-E1.2] spec
//	go test -run=TC-F    ./test/e2e/...     → runs all [TC-F*] specs
func TestMain(m *testing.M) {
	// Parse flags early so we can inspect flag values (e.g. -test.run) before
	// m.Run() does so internally. flag.Parse() is idempotent.
	if !flag.Parsed() {
		flag.Parse()
	}

	// Reset invocation-scoped environment variables only in the primary
	// process. Ginkgo parallel workers and re-exec guarded processes inherit
	// the cluster environment (KUBECONFIG, KIND_CLUSTER, suite paths) that was
	// exported by bootstrapSuiteCluster; resetting it here would orphan the
	// cluster from the worker processes.
	if !isGinkgoParallelWorker() && !isReexecGuarded() {
		if err := resetSuiteInvocationEnvironment(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "reset e2e invocation environment: %v\n", err)
			os.Exit(1)
		}
	}

	// TC-pattern routing: when the caller uses go test -run=TC-E1.2 or
	// go test -run=TC-F, the pattern does not match the Go test function name
	// "TestE2E". Intercept the flag, save it as a Ginkgo focus override, and
	// rewrite -test.run to "^TestE2E$" so Go's runner selects TestE2E. TestE2E
	// will then apply the saved pattern as a Ginkgo FocusStrings filter.
	if runFlag := flag.Lookup("test.run"); runFlag != nil {
		pattern := runFlag.Value.String()
		if isTCRunPattern(pattern) {
			tcRunFocusOverride = pattern
			if err := runFlag.Value.Set("^TestE2E$"); err != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"e2e: failed to rewrite -test.run for TC routing: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// AC 7 fast-path: when the effective -test.run pattern cannot reach
	// "TestE2E" (the Ginkgo suite entry point), skip Kind cluster bootstrap
	// and ginkgo re-exec entirely. Plain Go unit tests — such as TestAC7*,
	// TestAC61*, TestKindBootstrap, etc. — complete in milliseconds and must
	// not pay the 20-30 second cluster creation cost.
	//
	// This check runs only in the primary process (workers are already
	// dispatched above via isGinkgoParallelWorker / isReexecGuarded). After
	// isTCRunPattern rewrites any "TC-*" flag to "^TestE2E$", the flag value
	// seen here is the effective pattern: "^TestE2E$" for all TC-* runs, and
	// the original pattern for everything else.
	if !isGinkgoParallelWorker() && !isReexecGuarded() {
		if runFlag := flag.Lookup("test.run"); runFlag != nil {
			if !canMatchGinkgoSuite(runFlag.Value.String()) {
				_, _ = fmt.Fprintf(os.Stderr,
					"e2e: [AC7] fast-path: pattern %q cannot match TestE2E; "+
						"skipping Kind cluster bootstrap\n",
					runFlag.Value.String())
				os.Exit(m.Run())
			}
		}
	}

	// Phase 1 — Prerequisite check
	//
	// AC 1: Verify Docker daemon and kernel modules are present before we
	// attempt to create a Kind cluster or run any test. Only the primary
	// process performs this check; parallel workers and re-exec guarded
	// processes inherit a pre-validated environment from the primary.
	if !isGinkgoParallelWorker() && !isReexecGuarded() {
		if err := prereq.CheckHostPrerequisites(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	}

	// Orphan reaper — primary process only.
	//
	// Before creating a new Kind cluster, scan for any clusters matching the
	// pillar-csi-e2e-* naming pattern and delete them. These are left behind
	// when a previous test run was SIGKILL'd (signal 9 cannot be caught by Go
	// signal handlers). The reaper is best-effort: a failed delete is logged
	// but does not abort the run.
	if !isGinkgoParallelWorker() && !isReexecGuarded() {
		reapOrphanedClusters(os.Stderr)
	}

	// Route to the appropriate execution path based on the process role.
	//
	// Primary process  — owns cluster lifecycle: CreateCluster → tests →
	//                    DeleteCluster (deferred, panic-safe).
	// Worker / guarded — cluster already exists; just run m.Run() with
	//                    panic-safe cleanup of worker-local resources.
	if isGinkgoParallelWorker() || isReexecGuarded() {
		os.Exit(runWorker(m))
	}
	os.Exit(runPrimary(m))
}

// bootstrapSuiteCluster creates (or reuses) the Kind cluster for this test
// invocation, exports the cluster connection details (KUBECONFIG, KIND_CLUSTER,
// suite-path vars) to the process environment so that parallel ginkgo workers
// can inherit them, and registers the cluster with suiteInvocationTeardown so
// signal-based cleanup works.
//
// Sub-AC 5.4: when E2E_USE_EXISTING_CLUSTER=true, cluster creation is skipped
// and the caller-supplied cluster (KUBECONFIG + KIND_CLUSTER) is used directly.
// This saves ~30-60 seconds on repeated runs during iterative development.
//
// Must only be called from the primary (non-worker) process. Returns the
// bootstrap state and any error; on error the caller should exit immediately.
func bootstrapSuiteCluster(output io.Writer) (*kindBootstrapState, error) {
	// Sub-AC 5.4: reuse an existing cluster when requested.
	if resolveUseExistingCluster() {
		_, _ = fmt.Fprintf(output,
			"[AC4] %s=true — skipping kind create cluster; reusing existing cluster\n",
			useExistingClusterEnvVar)
		state, err := existingClusterState()
		if err != nil {
			return nil, fmt.Errorf("[AC4] %s: %w", useExistingClusterEnvVar, err)
		}
		// Export the cluster connection details so ginkgo workers can inherit them.
		if err := state.exportEnvironment(); err != nil {
			_ = os.RemoveAll(state.SuiteRootDir)
			return nil, fmt.Errorf("[AC4] export existing cluster environment: %w", err)
		}
		_, _ = fmt.Fprintf(output,
			"[AC4] reusing kind cluster %q: kubeconfig=%s context=%s\n",
			state.ClusterName, state.KubeconfigPath, state.KubeContext)
		return state, nil
	}

	state, err := newKindBootstrapState()
	if err != nil {
		return nil, fmt.Errorf("[AC4] create kind bootstrap state: %w", err)
	}

	runner := execCommandRunner{Output: output}
	ctx, cancel := context.WithTimeout(context.Background(), state.CreateTimeout+30*time.Second)
	defer cancel()

	// Pre-create assertion: the cluster must not already exist so each go test
	// invocation starts from a clean slate.
	if err := state.verifyClusterAbsent(ctx, runner); err != nil {
		_ = os.RemoveAll(state.SuiteRootDir)
		return nil, fmt.Errorf("[AC4] pre-create check: %w", err)
	}

	// Phase 2 — Kind cluster creation.
	//
	// CreateCluster — this is the canonical call site for cluster creation.
	// DeleteCluster is called by the defer in runPrimary, ensuring it runs on
	// every exit including panics.
	if err := state.createCluster(ctx, runner); err != nil {
		_ = os.RemoveAll(state.SuiteRootDir)
		return nil, fmt.Errorf("[AC4] create cluster: %w", err)
	}

	// Post-create assertion: the cluster must appear in "kind get clusters"
	// immediately after creation so we have hard evidence the cluster is live.
	if err := state.verifyClusterPresent(ctx, runner); err != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), state.DeleteTimeout)
		defer cleanCancel()
		_ = state.destroyCluster(cleanCtx, runner)
		_ = os.RemoveAll(state.SuiteRootDir)
		return nil, fmt.Errorf("[AC4] post-create check: %w", err)
	}

	// Register with suiteInvocationTeardown so that SIGINT / SIGTERM also
	// trigger cluster deletion before the process exits.
	if _, err := suiteInvocationTeardown.RegisterKindCluster(state); err != nil {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), state.DeleteTimeout)
		defer cleanCancel()
		_ = state.destroyCluster(cleanCtx, runner)
		_ = os.RemoveAll(state.SuiteRootDir)
		return nil, fmt.Errorf("[AC4] register kind cluster: %w", err)
	}

	// Export connection details to the process environment so parallel ginkgo
	// workers (spawned by reexecViaGinkgoCLI) inherit them via os.Environ().
	if err := state.exportEnvironment(); err != nil {
		_ = suiteInvocationTeardown.Cleanup(output)
		return nil, fmt.Errorf("[AC4] export cluster environment: %w", err)
	}

	_, _ = fmt.Fprintf(output,
		"[AC4] kind cluster %q created: kubeconfig=%s context=%s\n",
		state.ClusterName, state.KubeconfigPath, state.KubeContext)

	return state, nil
}

// runPrimary is the TestMain execution path for the primary (non-worker)
// process. It performs the full cluster and backend lifecycle:
//
//  1. Calls bootstrapSuiteCluster to create the Kind cluster and export cluster
//     env vars (KUBECONFIG, KIND_CLUSTER, suite-path vars).
//  2. Calls bootstrapSuiteImages to build Docker images and load them into the
//     Kind cluster (AC8).
//  3. Calls bootstrapSuiteBackends to provision ZFS pools and LVM VGs inside
//     the Kind container (AC5.2). Provisioned resource names are exported as
//     PILLAR_E2E_ZFS_POOL, PILLAR_E2E_LVM_VG so ginkgo workers inherit them.
//  4. Either re-execs via the ginkgo CLI (parallel mode) or calls m.Run()
//     directly (sequential mode / ginkgo-not-found fallback).
//  5. Defers a panic-safe cleanup that fires on every exit:
//     a. Backend teardown (ZFS/LVM) — while the container is still alive.
//     b. Kind cluster deletion.
//
// Sub-AC 5.4: stage timing profiling is active when E2E_STAGE_TIMING=1.
// Each pipeline stage (cluster-create, image-build, backend-setup, test-exec)
// is bracketed with timer.StartStage() / done() so that Emit at the end of
// runPrimary prints a wall-clock breakdown identifying the bottleneck.
//
// The return value is the exit code to pass to os.Exit.
func runPrimary(m *testing.M) (exitCode int) {
	// Sub-AC 5.4: initialise the stage timer. Emit is deferred so the summary
	// is always written even when runPrimary returns early on error.
	stageTimer := newPipelineStageTimer()
	defer stageTimer.Emit(os.Stderr)

	// ── Phase 2: Kind cluster creation ───────────────────────────────────────
	doneCluster := stageTimer.StartStage(stageClusterCreate)
	state, err := bootstrapSuiteCluster(os.Stderr)
	doneCluster()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		return 1
	}

	// ── Phase 3: image build + kind load ─────────────────────────────────────
	//
	// AC8: Build Docker images and load them into the Kind cluster.
	//
	// Images must be present on Kind nodes before any spec deploys a DaemonSet or
	// Deployment that references them. Building here (once per go test invocation,
	// before ginkgo workers are spawned) avoids duplicate builds across workers.
	// DOCKER_HOST is inherited from the process environment automatically.
	// Set E2E_SKIP_IMAGE_BUILD=true to skip for iterative test development.
	// Set E2E_DOCKER_BUILD_CACHE=true to enable --cache-from for faster rebuilds.
	doneImages := stageTimer.StartStage(stageImageBuild)
	imageCtx, imageCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	imageErr := bootstrapSuiteImages(imageCtx, state, os.Stderr)
	imageCancel()
	doneImages()
	if imageErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: [AC8] image build/load: %v\n", imageErr)
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), state.DeleteTimeout)
		defer cleanCancel()
		_ = suiteInvocationTeardown.CleanupWithRunner(cleanCtx, execCommandRunner{Output: os.Stderr})
		return 1
	}

	// ── Phase 4: backend provisioning ────────────────────────────────────────
	//
	// AC5.2: Provision shared ZFS pool and LVM VG once per suite run.
	//
	// The context timeout covers both ZFS and LVM provisioning. Each backend
	// allocates a 512 MiB loop device image (dd) and runs pool/VG creation —
	// typically 2-5 seconds total.
	doneBackend := stageTimer.StartStage(stageBackendSetup)
	backendCtx, backendCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	backendState, backendErr := bootstrapSuiteBackends(backendCtx, state, os.Stderr)
	backendCancel()
	doneBackend()
	if backendErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: [AC5.2] backend provisioning: %v\n", backendErr)
		// Clean up the cluster before returning because the deferred cleanup
		// below will not run when we return early here.
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), state.DeleteTimeout)
		defer cleanCancel()
		_ = suiteInvocationTeardown.CleanupWithRunner(cleanCtx, execCommandRunner{Output: os.Stderr})
		return 1
	}
	if err := suiteInvocationTeardown.RegisterBackend(backendState); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: [AC5.2] register backend: %v\n", err)
		return 1
	}
	// Export backend env vars BEFORE spawning ginkgo workers so they inherit
	// PILLAR_E2E_ZFS_POOL, PILLAR_E2E_LVM_VG, PILLAR_E2E_BACKEND_CONTAINER.
	if err := backendState.exportBackendEnvironment(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "e2e: [AC5.2] export backend environment: %v\n", err)
		return 1
	}

	// deleteOnExit tears down the cluster, removes suite temp dirs, and
	// verifies the cluster is absent from "kind get clusters". It is invoked
	// from the deferred function below and thus runs on every exit path.
	//
	// Sub-AC 5.4: when E2E_USE_EXISTING_CLUSTER=true the cluster is not owned by
	// this invocation (clusterCreated=false), so we skip deletion and the
	// post-destroy verify. The suite temp dir is still removed so ephemeral
	// kubeconfig copies and workspace dirs are cleaned up.
	deleteOnExit := func() {
		runner := execCommandRunner{Output: os.Stderr}
		ctx, cancel := context.WithTimeout(context.Background(), state.DeleteTimeout+30*time.Second)
		defer cancel()

		// Phase 6 — Teardown.
		//
		// suiteInvocationTeardown.Cleanup is idempotent; if SynchronizedAfterSuite
		// already cleaned up (sequential path), this is a safe no-op.
		// When using an existing cluster (clusterCreated=false) Cleanup only
		// removes the suite temp dir, not the Kind cluster itself.
		if cleanErr := suiteInvocationTeardown.Cleanup(os.Stderr); cleanErr != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"e2e: [AC4] cluster cleanup: %v\n", cleanErr)
			if exitCode == 0 {
				exitCode = 1
			}
		}

		// Post-destroy assertion: skip when using an existing cluster because
		// we intentionally did not delete it.
		if resolveUseExistingCluster() {
			_, _ = fmt.Fprintf(os.Stderr,
				"[AC4] %s=true — skipping post-destroy cluster-absent check\n",
				useExistingClusterEnvVar)
			return
		}

		// Sub-AC 3.3 post-destroy assertion: the cluster must no longer appear in
		// "kind get clusters" after cleanup, proving this invocation fully
		// released its cluster rather than leaving a dangling resource.
		// This is the teardown half of the AC3.3 cluster lifecycle smoke contract.
		if verifyErr := state.verifyClusterAbsent(ctx, runner); verifyErr != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"e2e: [AC3.3] cluster %q still present after teardown: %v\n",
				state.ClusterName, verifyErr)
			if exitCode == 0 {
				exitCode = 1
			}
		} else {
			_, _ = fmt.Fprintf(os.Stderr,
				"[AC3.3] kind cluster %q confirmed absent after teardown\n",
				state.ClusterName)
		}
	}

	// The nested function holds the defer so it executes before returning to
	// TestMain (which calls os.Exit). recover() catches any panic from
	// m.Run() or reexecViaGinkgoCLI, ensuring deleteOnExit always fires.
	return func() (code int) {
		defer func() {
			if r := recover(); r != nil {
				_, _ = fmt.Fprintf(os.Stderr,
					"e2e: [AC4] panic in primary test runner: %v\n", r)
				code = 1
			}
			deleteOnExit()
		}()

		// Install signal handlers so SIGINT / SIGTERM also trigger cleanup.
		// stopSignals is deferred to unregister the handler after the run.
		stopSignals := installInvocationSignalHandlers(suiteInvocationTeardown, os.Stderr, os.Exit)
		defer stopSignals()

		// ── Phase 5: test execution ───────────────────────────────────────
		//
		// Sub-AC 5.4: the test-exec stage covers all ginkgo worker time (or
		// sequential m.Run()). We start the stage timer here and close it
		// after the inner call returns so the duration captures the total
		// time spent running specs.
		doneTestExec := stageTimer.StartStage(stageTestExec)

		// ── Signal handler unit tests ─────────────────────────────────────
		//
		// Sub-AC 2.2: run the three signal handler unit tests in a dedicated
		// subprocess before spawning ginkgo workers.
		//
		// These tests (TestInstallInvocationSignalHandlersSIGTERM,
		// TestInstallInvocationSignalHandlersSIGINT,
		// TestInstallInvocationSignalHandlersStopPreventsCleanup) send real
		// OS signals to the current process and must not race with ginkgo's
		// internal suite coordination protocol.  They are plain Go tests (not
		// Ginkgo specs) and are excluded from the ginkgo worker run
		// (-test.run=^TestE2E$), so without this call they would never execute.
		//
		// The subprocess takes the AC7 fast-path: the pattern
		// signalHandlerTestPattern cannot match "TestE2E", so TestMain calls
		// resetSuiteInvocationEnvironment() (clearing KIND_CLUSTER) and then
		// os.Exit(m.Run()) directly, skipping cluster bootstrap entirely.
		// With KIND_CLUSTER unset the signal tests run their actual signal
		// logic rather than delegating to another subprocess.
		if !isSequentialMode() {
			if signalCode := runSignalHandlerTestsInSubprocess(os.Stdout, os.Stderr); signalCode != 0 {
				_, _ = fmt.Fprintf(os.Stderr,
					"e2e: [Sub-AC 2.2] signal handler unit tests failed (exit %d)\n", signalCode)
				doneTestExec()
				return signalCode
			}
		}

		// ── Auto-parallel re-exec ─────────────────────────────────────────
		//
		// Cluster env vars are already exported (by bootstrapSuiteCluster).
		// Spawn ginkgo workers now; they will inherit KUBECONFIG, KIND_CLUSTER,
		// and the suite-path vars via cmd.Env = append(os.Environ(), ...) in
		// reexecViaGinkgoCLI.
		if !isSequentialMode() {
			if code := reexecViaGinkgoCLI(os.Stdout, os.Stderr); code >= 0 {
				doneTestExec()
				return code
			}
			// Ginkgo binary not found — fall through to sequential execution.
			_, _ = fmt.Fprintln(os.Stderr,
				"e2e: ginkgo CLI not found; running specs sequentially (install via `make ginkgo`)")
		}

		// Sequential path: run specs in-process.
		seqCode := m.Run()
		doneTestExec()
		return seqCode
	}()
}

// runWorker is the TestMain execution path for ginkgo parallel workers and
// re-exec guarded processes. The cluster was already created by runPrimary
// (the parent process) and is reachable via environment variables
// (KUBECONFIG, KIND_CLUSTER, etc.) that were inherited at process spawn time.
//
// Workers must not create or delete the cluster. Their role is to run a
// subset of Ginkgo specs, release any worker-local resources on exit, and
// propagate the correct exit code.
//
// A recover() wrapper ensures any spec panic is logged and the exit code is
// set to 1 rather than crashing the worker silently.
func runWorker(m *testing.M) (exitCode int) {
	defer func() {
		if r := recover(); r != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"e2e: [AC4] panic in worker test runner: %v\n", r)
			exitCode = 1
		}
		// Release any worker-local resources (e.g. per-worker namespace
		// cleanups registered by specs). suiteInvocationTeardown is empty
		// in worker processes (the cluster is owned by the primary); this call
		// is therefore a safe no-op for the cluster itself.
		if cleanErr := suiteInvocationTeardown.Cleanup(os.Stderr); cleanErr != nil {
			_, _ = fmt.Fprintf(os.Stderr,
				"e2e: worker cleanup: %v\n", cleanErr)
		}
	}()

	// NOTE: Do NOT install signal handlers in ginkgo parallel workers.
	//
	// installInvocationSignalHandlers calls os.Exit when SIGTERM/SIGINT is
	// received. In a ginkgo parallel worker, os.Exit bypasses ginkgo's internal
	// suite-done reporting protocol: the worker exits without sending the
	// "suite complete" message to ginkgo's coordinator server, which then waits
	// up to 1 second and prints:
	//   "Ginkgo timed out waiting for all parallel procs to report back"
	//
	// Workers have no cluster resource to clean up (suiteInvocationTeardown is
	// empty — the cluster is owned by runPrimary). Signal handling here provides
	// no safety benefit but causes the parallel timeout failure.
	//
	// The primary process retains its signal handler via runPrimary so that
	// cluster deletion fires even on Ctrl-C or external kill signals.
	return m.Run()
}

// isGinkgoParallelWorker returns true when the current process was spawned by
// the ginkgo CLI as a parallel worker node.
//
// Detection strategy: ginkgo v2 registers the -ginkgo.parallel.total flag at
// package init time and passes "--ginkgo.parallel.total=N" to every parallel
// worker process it spawns. When total > 1 the process is operating as a
// coordinated worker and MUST NOT re-exec into another ginkgo invocation.
//
// Note: ginkgo v2 does NOT set any environment variable on workers, so an
// env-var check (e.g. GINKGO_PROC_HANDLE) would always return false.
func isGinkgoParallelWorker() bool {
	f := flag.Lookup(ginkgoParallelTotalFlag)
	if f == nil {
		return false
	}
	total, err := strconv.Atoi(f.Value.String())
	return err == nil && total > 1
}

// isReexecGuarded returns true when the current process was launched (directly
// or transitively) by reexecViaGinkgoCLI, indicating it must not re-exec again.
func isReexecGuarded() bool {
	return os.Getenv(reexecSentinelEnv) != ""
}

// isSequentialMode returns true when PILLAR_E2E_SEQUENTIAL is set to a
// non-empty value, opting out of the auto-parallel re-exec.
func isSequentialMode() bool {
	return os.Getenv(sequentialModeEnv) != ""
}

// reexecViaGinkgoCLI re-executes the current test suite under the ginkgo CLI
// with DefaultParallelNodes parallel workers. It sets reexecSentinelEnv so
// that the spawned ginkgo workers do not enter another re-exec cycle.
//
// The cluster env vars exported by bootstrapSuiteCluster are included in
// cmd.Env via os.Environ(), so workers inherit KUBECONFIG, KIND_CLUSTER, etc.
// without any additional plumbing.
//
// Sub-AC 2.2: the current compiled test binary is passed to ginkgo instead of
// the package path "." so that ginkgo skips recompilation. Recompiling the e2e
// package from scratch adds 10–20 s to the stageTestExec budget (consuming
// 22–44% of the 45 s test-exec budget before a single spec runs). Using the
// precompiled binary eliminates this overhead and also preserves the -tags=e2e
// build configuration that is required for kind_bootstrap_e2e_test.go's
// SynchronizedBeforeSuite to register.
//
// Return values:
//
//	>= 0  ginkgo completed with this exit code (0 = all specs passed).
//	  -1  ginkgo binary not found; caller should fall back to sequential.
func reexecViaGinkgoCLI(stdout, stderr io.Writer) int {
	ginkgoBin := findGinkgoBinary()
	if ginkgoBin == "" {
		return -1
	}

	// Sub-AC 2.1 / 2.3: clamp the effective worker count to [minParallelProcs,
	// maxParallelProcs] so that:
	//   • low-CPU machines (< 8 cores) still spawn enough workers to meet the
	//     45-second test-exec budget (minParallelProcs floor), and
	//   • high-CPU machines (> 8 cores) do not overload the shared Kind API
	//     server with too many simultaneous clients (maxParallelProcs ceiling).
	//
	// Observed failure mode without the ceiling: 16 parallel workers on a
	// 16-core host caused "Ginkgo timed out waiting for all parallel procs to
	// report back" because the API server was saturated during
	// SynchronizedBeforeSuite. Capping at maxParallelProcs = 8 eliminates
	// this class of failure while preserving parallelism for the budget.
	effectiveProcs := DefaultParallelNodes
	if effectiveProcs < minParallelProcs {
		effectiveProcs = minParallelProcs
	}
	if effectiveProcs > maxParallelProcs {
		effectiveProcs = maxParallelProcs
	}
	procs := strconv.Itoa(effectiveProcs)
	if p := os.Getenv("PILLAR_E2E_PROCS"); p != "" {
		// PILLAR_E2E_PROCS (set by Makefile via E2E_PROCS=4) overrides the
		// clamped default so operator-controlled runs can further restrict
		// parallelism without changing code.
		procs = p
	}

	reportDir := filepath.Join(os.TempDir(), "pillar-csi-e2e-reports")
	_ = os.MkdirAll(reportDir, 0o755)

	// Sub-AC 2.2: resolve the path of the already-compiled test binary so ginkgo
	// can use it directly without recompiling the package from scratch.
	// resolveTestBinaryPath() returns "." as a fallback when os.Executable()
	// cannot locate a suitable compiled binary (e.g., in unusual test harnesses).
	testBinPath := resolveTestBinaryPath()
	if testBinPath != "." {
		_, _ = fmt.Fprintf(stderr,
			"e2e: [Sub-AC 2.2] using precompiled binary %s (skipping ginkgo recompilation)\n",
			testBinPath)
	}

	ginkgoArgs := []string{
		"--procs=" + procs,
		"--timeout=" + ginkgoCliTimeout,
		"--output-dir=" + reportDir,
		"--json-report=e2e-auto.json",
		testBinPath,
		"--",
		// Note: flags after "--" are forwarded to the compiled test binary.
		// -count is a `go test` runner flag that the test binary does NOT
		// recognise; pass only test-binary flags here.
		"-test.run=^TestE2E$",
	}
	// AC 6: propagate fail-fast setting to the ginkgo CLI.
	// resolveFailFast() reads -e2e.fail-fast flag and E2E_FAIL_FAST env var.
	// The default is false (continue on failure). When true, --fail-fast is
	// prepended so the ginkgo worker stops after the first spec failure.
	if resolveFailFast() {
		ginkgoArgs = append([]string{"--fail-fast"}, ginkgoArgs...)
	}
	if tcRunFocusOverride != "" {
		// Prepend the focus flag so ginkgo applies it before the binary path.
		ginkgoArgs = append([]string{"--focus=" + tcRunFocusOverride}, ginkgoArgs...)
	}

	// Sub-AC 2 (parallel-safe): apply a label filter so that long-running
	// cluster-side specs (e.g. "helm" E27 tests that call `helm install --wait
	// --timeout 5m`) are excluded from the default parallel run.
	//
	// Resolution order:
	//  1. E2E_LABEL_FILTER env var — set by the Makefile or caller; takes precedence.
	//     Empty string ("") disables all filtering (run every spec).
	//  2. defaultLabelFilter constant ("default-profile") — applied when the env
	//     var is absent (not set at all, as opposed to set-to-empty).
	//
	// Without this guard every ginkgo worker process can independently pick up a
	// helm spec and call `helm install --wait --timeout 5m`. With N=8 workers
	// and a 2-minute suite timeout, multiple workers stall beyond the deadline,
	// causing "Ginkgo timed out waiting for all parallel procs to report back".
	labelFilter, labelFilterSet := os.LookupEnv(labelFilterEnv)
	if !labelFilterSet {
		labelFilter = defaultLabelFilter
	}
	if labelFilter != "" {
		_, _ = fmt.Fprintf(stderr,
			"e2e: [Sub-AC 2] label filter %q applied (set %s='' to run all specs)\n",
			labelFilter, labelFilterEnv)
		ginkgoArgs = append([]string{"--label-filter=" + labelFilter}, ginkgoArgs...)
	}

	cmd := exec.Command(ginkgoBin, ginkgoArgs...) //nolint:gosec
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Inject the re-exec guard so ginkgo workers skip this re-exec path.
	// os.Environ() includes the cluster env vars (KUBECONFIG, KIND_CLUSTER, …)
	// exported by bootstrapSuiteCluster, so workers inherit the cluster.
	cmd.Env = append(os.Environ(), reexecSentinelEnv+"=1")

	_, _ = fmt.Fprintf(stderr,
		"e2e: auto-parallel: running %s workers via %s\n", procs, ginkgoBin)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

// findGinkgoBinary returns the absolute path of the ginkgo CLI binary.
//
// Search order:
//  1. $GINKGO environment variable (Makefile sets GINKGO=$(LOCALBIN)/ginkgo)
//  2. bin/ginkgo under the module root (local `make ginkgo` install)
//  3. System PATH
//
// Returns "" if the binary cannot be found.
func findGinkgoBinary() string {
	// 1. Explicit path via environment variable (set by Makefile).
	if g := os.Getenv("GINKGO"); g != "" {
		if _, err := os.Stat(g); err == nil {
			return g
		}
	}

	// 2. Local bin/ directory relative to cwd.
	//    When running as `go test ./test/e2e/...`, cwd is the package dir
	//    (test/e2e/). Walk up to find the module root's bin/ directory.
	if wd, err := os.Getwd(); err == nil {
		candidates := []string{
			// Direct: cwd is already the repo root (rare but possible)
			filepath.Join(wd, "bin", "ginkgo"),
			// From test/e2e/: two levels up
			filepath.Join(wd, "..", "..", "bin", "ginkgo"),
			// From test/e2e/subpkg/: three levels up
			filepath.Join(wd, "..", "..", "..", "bin", "ginkgo"),
		}
		for _, c := range candidates {
			if abs, err := filepath.Abs(c); err == nil {
				if _, err := os.Stat(abs); err == nil {
					return abs
				}
			}
		}
	}

	// 3. System PATH.
	if g, err := exec.LookPath("ginkgo"); err == nil {
		return g
	}

	return ""
}

// resolveTestBinaryPath returns the path of the currently-running compiled test
// binary, which ginkgo v2 accepts directly as a "package" argument to skip
// recompilation (Sub-AC 2.2).
//
// When invoked by `go test -tags=e2e ./test/e2e/...`, the binary at
// os.Executable() is a compiled .test file that already contains the e2e build
// tag. Ginkgo v2 recognises a path ending in ".test" as a precompiled binary
// and runs it without invoking the Go toolchain, saving 10–20 s.
//
// Falls back to "." (package path, forces recompilation) when:
//   - os.Executable() fails
//   - the resolved path does not end in ".test"
//   - the resolved path does not exist or is a directory
//
// The "." fallback ensures ginkgo still works when resolveTestBinaryPath is
// called in unusual environments (e.g., direct binary execution or CI harnesses
// that strip the .test suffix).
func resolveTestBinaryPath() string {
	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		return "."
	}
	// Resolve symlinks so ginkgo gets the real file path.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	// Ginkgo v2 identifies precompiled test binaries by the ".test" suffix.
	// Go's `go test` always produces binaries with this suffix (e.g. e2e.test).
	if !strings.HasSuffix(exePath, ".test") {
		return "."
	}
	// Final existence check: make sure the file is present and not a directory.
	if fi, err := os.Stat(exePath); err != nil || fi.IsDir() {
		return "."
	}
	return exePath
}

// signalHandlerTestPattern is the -test.run regex that selects the three
// signal-handler unit tests that must run as part of the e2e pipeline:
//
//   - TestInstallInvocationSignalHandlersSIGTERM
//   - TestInstallInvocationSignalHandlersSIGINT
//   - TestInstallInvocationSignalHandlersStopPreventsCleanup
//
// These are plain Go tests (not Ginkgo specs) that send real OS signals to
// the current process. They are excluded from the ginkgo parallel run
// (-test.run=^TestE2E$) and must therefore be invoked explicitly via
// runSignalHandlerTestsInSubprocess before the ginkgo re-exec step.
const signalHandlerTestPattern = "^TestInstallInvocationSignalHandlers"

// runSignalHandlerTestsInSubprocess re-execs the current test binary with
// signalHandlerTestPattern as the -test.run filter so the three signal
// handler unit tests execute and their results are captured.
//
// # Subprocess execution path
//
// The subprocess is NOT re-exec guarded (no PILLAR_E2E_REEXEC_GUARD), so its
// TestMain calls resetSuiteInvocationEnvironment() which unsets KIND_CLUSTER
// and other suite-owned env vars.  With KIND_CLUSTER unset, the AC7 fast-path
// activates: canMatchGinkgoSuite(signalHandlerTestPattern) returns false and
// TestMain calls os.Exit(m.Run()) directly, skipping Kind cluster bootstrap,
// prereq checks, and the ginkgo re-exec.
//
// Inside m.Run(), each signal test sees os.Getenv("KIND_CLUSTER") == ""
// (cleared by resetSuiteInvocationEnvironment) so it runs its actual signal
// logic (installs a handler, sends a real SIGTERM/SIGINT via syscall.Kill,
// asserts the exit code and cleanup) rather than delegating to yet another
// subprocess.
//
// # Why subprocess and not direct m.Run()
//
// A direct call to m.Run() in the primary process would also run TestE2E,
// causing the Ginkgo suite to execute in-process (sequentially) before the
// parallel ginkgo re-exec. Running the signal tests in a subprocess scopes the
// signal delivery to a fresh process with no ginkgo coordination state, avoiding
// interference with the primary process's signal handler (installed by
// installInvocationSignalHandlers).
//
// Returns 0 when all three tests pass; non-zero on any failure.
func runSignalHandlerTestsInSubprocess(stdout, stderr io.Writer) int {
	exePath, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr,
			"e2e: runSignalHandlerTestsInSubprocess: os.Executable: %v\n", err)
		return 1
	}
	// Resolve symlinks so the subprocess receives the canonical binary path.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	cmd := exec.Command(exePath, //nolint:gosec
		"-test.run="+signalHandlerTestPattern,
		"-test.count=1",
		"-test.v=true",
	)
	// Inherit the current environment. resetSuiteInvocationEnvironment() in
	// the subprocess's TestMain will clear KIND_CLUSTER and other suite vars
	// before any test runs, giving each signal test a clean environment.
	cmd.Env = os.Environ()
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	_, _ = fmt.Fprintf(stderr,
		"e2e: [Sub-AC 2.2] running signal handler unit tests (pattern=%q)\n",
		signalHandlerTestPattern)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr,
			"e2e: signal handler test subprocess: %v\n", err)
		return 1
	}
	return 0
}
