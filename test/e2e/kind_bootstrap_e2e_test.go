//go:build e2e

package e2e

import (
	"context"
	"fmt"
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
	//  B. Direct ginkgo invocation (make test-e2e) — TestMain sees
	//     isGinkgoParallelWorker()=true and calls runWorker(), skipping
	//     bootstrapSuiteCluster.  We bootstrap the cluster here on node 1.
	state, envErr := kindBootstrapStateFromEnv()
	if envErr != nil {
		// Path B: cluster not yet created — bootstrap now on ginkgo node 1.
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

	payload, err := state.encode()
	Expect(err).NotTo(HaveOccurred())
	return payload
}, func(data []byte) {
	// All-nodes function: run on every parallel worker with the bytes
	// produced by the node-1 function above.
	state, err := decodeKindBootstrapState(data)
	Expect(err).NotTo(HaveOccurred())
	suiteKindCluster = state

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
var _ = SynchronizedAfterSuite(func() {}, func() {
	runner := execCommandRunner{Output: GinkgoWriter}
	deleteTimeout := 2 * time.Minute
	if suiteKindCluster != nil {
		deleteTimeout = suiteKindCluster.DeleteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()

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
})
