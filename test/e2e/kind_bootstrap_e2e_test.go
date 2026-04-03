//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// SynchronizedBeforeSuite reads the Kind cluster that TestMain already
// created (via bootstrapSuiteCluster) from the process environment.
//
// Design rationale:
//   - Cluster creation is owned by TestMain (suite_test.go) so that
//     DeleteCluster is guaranteed to run even if the test binary panics
//     (the deferred cleanup in runPrimary fires before os.Exit is reached).
//   - SynchronizedBeforeSuite only needs to surface the cluster reference to
//     ginkgo specs: it reads env vars, serialises the state as JSON on node 1,
//     and deserialises it on every parallel node.
var _ = SynchronizedBeforeSuite(func() []byte {
	// Node-1 function: executed once across the entire parallel run.
	//
	// Two invocation paths:
	//  A. go test entry point (TestMain runPrimary) — TestMain already called
	//     bootstrapSuiteCluster and exported KUBECONFIG / KIND_CLUSTER env vars
	//     before spawning ginkgo workers.  We read the state from env here.
	//  B. Direct ginkgo CLI invocation (NOT make test-e2e) — the test binary is
	//     launched directly by the ginkgo CLI, so TestMain sees
	//     isGinkgoParallelWorker()=true on node 1 and calls runWorker(), skipping
	//     bootstrapSuiteCluster.  We bootstrap the cluster here on node 1.
	//     NOTE: make test-e2e uses PATH A (go test → TestMain runPrimary).
	state, envErr := kindBootstrapStateFromEnv()
	if envErr != nil {
		// In PATH A (make test-e2e / go test → TestMain runPrimary), TestMain
		// already called bootstrapSuiteCluster before spawning ginkgo workers,
		// and exported KUBECONFIG / KIND_CLUSTER / suite-path env vars so every
		// worker process inherits them via reexecViaGinkgoCLI (which sets
		// PILLAR_E2E_REEXEC_GUARD=1).  If we are running inside that re-exec
		// guard and the env vars are missing or invalid, something went wrong in
		// the primary — fail fast with a clear message rather than attempting a
		// second cluster bootstrap from inside a worker.
		Expect(isReexecGuarded()).To(BeFalse(),
			"[AC4] SynchronizedBeforeSuite: running under PILLAR_E2E_REEXEC_GUARD "+
				"but kindBootstrapStateFromEnv failed — KUBECONFIG/KIND_CLUSTER env vars "+
				"must be exported by TestMain before spawning ginkgo workers: %v", envErr)

		// Path B: direct ginkgo invocation (not re-exec guarded) and cluster not
		// yet created — bootstrap now on ginkgo node 1.
		var bootstrapErr error
		state, bootstrapErr = bootstrapSuiteCluster(GinkgoWriter)
		Expect(bootstrapErr).NotTo(HaveOccurred(),
			"[AC4] SynchronizedBeforeSuite: cluster bootstrap failed — "+
				"check Kind / Docker availability: %v", bootstrapErr)
	}

	_, _ = fmt.Fprintf(GinkgoWriter,
		"[AC4] SynchronizedBeforeSuite: kind cluster %q available: "+
			"kubeconfig=%s context=%s\n",
		state.ClusterName, state.KubeconfigPath, state.KubeContext)

	// Sub-AC 2: conditional Helm bootstrap — node-1 installs the suite-level
	// Helm release when E2E_HELM_BOOTSTRAP=true.  Other workers are blocked in
	// SynchronizedBeforeSuite until this function returns the payload, so the
	// install is guaranteed to complete before any spec runs.
	//
	// Use E2E_HELM_BOOTSTRAP=true when running cluster-level tests that require
	// a pre-installed pillar-csi deployment (e.g. --label-filter=E10-cluster).
	// Do NOT set E2E_HELM_BOOTSTRAP when running E27 Helm tests
	// (--label-filter=helm) because TC-E27.207 itself tests `helm install`.
	var helmState *helmBootstrapState
	if resolveHelmBootstrap() {
		helmCtx, helmCancel := context.WithTimeout(context.Background(), helmInstallTimeout)
		defer helmCancel()

		var helmErr error
		helmState, helmErr = bootstrapSuiteHelm(helmCtx, state, GinkgoWriter)
		Expect(helmErr).NotTo(HaveOccurred(),
			"[helm-bootstrap] SynchronizedBeforeSuite: helm install failed — "+
				"set E2E_HELM_BOOTSTRAP=false to skip Helm pre-install: %v", helmErr)
	}

	// Encode both the cluster state and the (optional) Helm state into the
	// synchronizedSuitePayload that all workers will receive.
	encoded, err := encodeSuitePayload(synchronizedSuitePayload{
		KindState: state,
		HelmState: helmState,
	})
	Expect(err).NotTo(HaveOccurred())
	return encoded
}, func(data []byte) {
	// All-nodes function: run on every parallel worker with the bytes
	// produced by the node-1 function above.
	//
	// Sub-AC 2: decodes both the cluster state and the optional Helm state
	// so that specs can use SuiteKubeRestConfig() and suiteHelmBootstrap
	// without any additional synchronisation.
	suitePayload, err := decodeSuitePayload(data)
	Expect(err).NotTo(HaveOccurred())

	state := suitePayload.KindState
	suiteKindCluster = state
	suiteHelmBootstrap = suitePayload.HelmState

	if suiteHelmBootstrap != nil {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"[helm-bootstrap] node %d: suite-level release %q available in namespace %q\n",
			GinkgoParallelProcess(), suiteHelmBootstrap.Release, suiteHelmBootstrap.Namespace)
	}

	// AC4c: Build the shared rest.Config from the per-invocation kubeconfig
	// so every spec on this node can connect to the ephemeral Kind cluster
	// via SuiteKubeRestConfig() without repeating the clientcmd boilerplate.
	//
	// The kubeconfig file was written by bootstrapSuiteCluster and its path
	// was propagated to this worker via the KUBECONFIG environment variable
	// (set by exportEnvironment in the primary process).
	restCfg, err := buildClusterRestConfig(state.KubeconfigPath)
	Expect(err).NotTo(HaveOccurred(),
		"[AC4c] SynchronizedBeforeSuite: failed to build rest.Config from "+
			"kubeconfig=%s — check that bootstrapSuiteCluster exported KUBECONFIG correctly",
		state.KubeconfigPath)
	suiteRestConfig = restCfg

	_, _ = fmt.Fprintf(GinkgoWriter,
		"[AC4c] rest.Config ready: host=%s kubeconfig=%s\n",
		suiteRestConfig.Host, state.KubeconfigPath)

	// Pre-warm every in-process verifier on this Ginkgo node, mirroring the
	// behaviour of the non-e2e SynchronizedBeforeSuite
	// (tc_id_uniqueness_guard_suite_test.go).  Each verifier uses sync.Once
	// internally; warmUpLocalBackend eagerly triggers that initialisation so
	// backends are ready before specs run, amortising first-call overhead and
	// surfacing verifier failures at suite-setup time (fast-fail).
	warmUpLocalBackend(GinkgoParallelProcess())

	_, _ = fmt.Fprintf(GinkgoWriter,
		"[AC2b] node %d: in-process backends initialised (%d verifiers pre-warmed)\n",
		GinkgoParallelProcess(), len(allLocalVerifierNames))

	// ── AC9c: Assert all four real backends are reachable and functional ──────
	//
	// runAllBackendEnvChecks verifies that ZFS, LVM, NVMe-oF TCP, and iSCSI are
	// all provisioned and genuinely functional inside the Kind container. It
	// fails the suite immediately if ANY backend is absent or non-functional,
	// ensuring that no TC can silently run against a fake/stub/mock backend.
	//
	// The check runs on EVERY parallel worker node so that every node confirms
	// the shared backend environment before it begins executing specs. All four
	// checks are read-only (docker exec with zpool list, vgs, test -d, tgtadm
	// show) and complete in under 10 seconds even on loaded hosts.
	//
	// AC 10 policy: Soft-skip is DISABLED. A non-nil error from
	// runAllBackendEnvChecks causes an unconditional Fail, aborting this
	// worker's spec queue and the entire Ginkgo run.
	envCheckCtx, envCheckCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer envCheckCancel()

	nodeContainer := os.Getenv(suiteBackendContainerEnvVar)
	if nodeContainer == "" && state != nil {
		// Derive the container name from the cluster name when the env var was
		// not explicitly exported (e.g. direct ginkgo CLI invocation path B).
		nodeContainer = state.ClusterName + "-control-plane"
	}
	zfsPool := os.Getenv(suiteZFSPoolEnvVar)
	lvmVG := os.Getenv(suiteLVMVGEnvVar)

	_, _ = fmt.Fprintf(GinkgoWriter,
		"[AC9c] node %d: running backend env-check (container=%s zfsPool=%s lvmVG=%s)...\n",
		GinkgoParallelProcess(), nodeContainer, zfsPool, lvmVG)

	envCheckErr := runAllBackendEnvChecks(envCheckCtx, nodeContainer, zfsPool, lvmVG, GinkgoWriter)
	Expect(envCheckErr).NotTo(HaveOccurred(),
		"[AC9c] Backend env-check FAILED on Ginkgo node %d: "+
			"one or more real backends are absent or replaced by a stub/mock.\n"+
			"AC 10 policy: soft-skip is DISABLED — ALL four backends must be real.\n"+
			"See the error above for remediation steps.",
		GinkgoParallelProcess())

	_, _ = fmt.Fprintf(GinkgoWriter,
		"[AC9c] node %d: all four backends (ZFS, LVM, NVMe-oF, iSCSI) verified — "+
			"no fake/stub/mock backend detected\n",
		GinkgoParallelProcess())
})

// SynchronizedAfterSuite is the belt-and-suspenders cleanup path for the
// sequential execution mode (PILLAR_E2E_SEQUENTIAL=true or ginkgo not found).
//
// In sequential mode, the test binary is also the primary process
// (suiteInvocationTeardown owns the cluster).  Calling CleanupWithRunner here
// deletes the cluster immediately after all specs finish and before
// TestMain.runPrimary.deleteOnExit runs.  deleteOnExit then finds the cluster
// already gone and confirms via verifyClusterAbsent — no double-delete error
// because suiteInvocationTeardown.takeKindCluster is atomic and idempotent.
//
// In parallel mode, this runs inside a ginkgo worker process whose
// suiteInvocationTeardown is empty (cluster is owned by the primary).
// CleanupWithRunner is therefore a safe no-op; the primary's deleteOnExit
// handles the actual deletion after reexecViaGinkgoCLI returns.
var _ = SynchronizedAfterSuite(
	// ── All-nodes phase ───────────────────────────────────────────────────────
	// Sub-AC 5.3: drain any in-flight background TC cleanup goroutines before
	// this worker exits. Each parallel worker has its own suiteAsyncCleanup
	// batch; draining here ensures no goroutines are left running when the
	// worker process ends. Cleanup errors are logged, not fatal.
	func() {
		if err := DrainPendingCleanups(30 * time.Second); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"[AC5.3] node %d: background TC cleanup errors (informational): %v\n",
				GinkgoParallelProcess(), err)
		}
	},
	// ── Primary phase ────────────────────────────────────────────────────────
	func() {
		runner := execCommandRunner{Output: GinkgoWriter}
		deleteTimeout := 2 * time.Minute
		if suiteKindCluster != nil {
			deleteTimeout = suiteKindCluster.DeleteTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()

		// Sub-AC 2: uninstall the suite-level Helm release (if pre-installed by
		// bootstrapSuiteHelm) before the Kind cluster is deleted. This keeps
		// Helm's release registry consistent and avoids "release already installed"
		// errors on the next run. Errors are non-fatal: the cluster deletion that
		// follows will clean up all Kubernetes resources regardless.
		if suiteHelmBootstrap != nil {
			helmCtx, helmCancel := context.WithTimeout(context.Background(), helmTeardownTimeout)
			defer helmCancel()
			teardownSuiteHelm(helmCtx, suiteHelmBootstrap, suiteKindCluster, GinkgoWriter)
			suiteHelmBootstrap = nil
		}

		// Idempotent: no-op in parallel workers (empty teardown) and safe to
		// call again from runPrimary.deleteOnExit.
		if err := suiteInvocationTeardown.CleanupWithRunner(ctx, runner); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"[AC4] SynchronizedAfterSuite: cluster cleanup: %v\n", err)
		}
		suiteKindCluster = nil
		// AC4c: nil out the shared rest.Config so any spec that accidentally
		// retains a reference after suite teardown panics immediately rather
		// than silently using stale cluster state.
		suiteRestConfig = nil
	},
)
