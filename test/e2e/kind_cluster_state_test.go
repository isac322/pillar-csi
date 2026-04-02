package e2e

// kind_cluster_state_test.go — Package-level state for the per-invocation Kind
// cluster and the *rest.Config built from it.
//
// This file has NO build tags so its symbols are available in both the
// non-e2e (in-process) and e2e (Kind cluster) builds.  That lets:
//
//   - kind_kubeconfig_test.go (plain testing.T tests) call buildClusterRestConfig
//     and access suiteRestConfig without requiring -tags=e2e.
//
//   - cluster_kubeconfig_e2e_test.go (Ginkgo specs, //go:build e2e) assert that
//     the propagation chain works against a live Kind cluster.
//
// The SynchronizedBeforeSuite / SynchronizedAfterSuite that populate and nil-out
// these variables live in:
//
//   kind_bootstrap_e2e_test.go  (//go:build e2e)  — for Kind-cluster runs
//   tc_id_uniqueness_guard_suite_test.go (//go:build !e2e) — for in-process runs
//   suite_group_timing_test.go  (//go:build !e2e) — AfterSuite for in-process runs

import (
	"fmt"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// suiteKindCluster holds the cluster state for specs that need direct cluster
// access (e.g. to derive kubeconfig paths, cluster names, or timeouts).
// It is populated by SynchronizedBeforeSuite from the environment variables
// that TestMain.bootstrapSuiteCluster exported before tests started.
var suiteKindCluster *kindBootstrapState

// suiteRestConfig is the *rest.Config built from the per-invocation kubeconfig
// (suiteKindCluster.KubeconfigPath). It is constructed in the all-nodes phase
// of SynchronizedBeforeSuite so every Ginkgo spec — including those running on
// parallel worker nodes — receives a ready-to-use cluster connection config
// without rebuilding it from scratch.
//
// AC4c: This variable is the canonical bridge between the kubeconfig file that
// TestMain writes under /tmp (via bootstrapSuiteCluster → exportEnvironment)
// and the client-go / controller-runtime code paths that specs exercise against
// the live Kind cluster.
//
// It is reset to nil in SynchronizedAfterSuite so that any spec that
// accidentally holds a reference after suite teardown receives a clear signal
// (nil dereference) rather than stale connection state.
var suiteRestConfig *rest.Config

// SuiteKubeRestConfig returns the *rest.Config that was built from the
// per-invocation kubeconfig during SynchronizedBeforeSuite.
//
// All Ginkgo specs that need to create Kubernetes clients (e.g.
// controller-runtime client, typed client-go clients) should obtain their
// base config from this function rather than calling clientcmd independently.
// Using a shared config instance ensures:
//
//   - Every spec connects to the same ephemeral Kind cluster for this
//     go test invocation — no accidental cross-cluster contamination.
//   - The kubeconfig file path stays within /tmp (environment hygiene).
//   - Specs remain agnostic of the cluster name and kubeconfig path.
//
// Returns nil only when called outside a Ginkgo suite (e.g. from a plain
// `go test -run=TestXxx` function that does not go through TestE2E).
func SuiteKubeRestConfig() *rest.Config {
	return suiteRestConfig
}

// buildClusterRestConfig constructs a *rest.Config from the kubeconfig file at
// the given path.  It wraps clientcmd.BuildConfigFromFlags with an opinionated
// error message that surfaces the kubeconfig path for quick diagnosis.
//
// The resulting config connects to the cluster described by the current-context
// in the kubeconfig; for Kind clusters this is always "kind-<cluster-name>".
func buildClusterRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return nil, fmt.Errorf("[AC4c] kubeconfig path is empty — KUBECONFIG env var not set or cluster not bootstrapped")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("[AC4c] build rest.Config from kubeconfig %q: %w", kubeconfigPath, err)
	}
	return cfg, nil
}
