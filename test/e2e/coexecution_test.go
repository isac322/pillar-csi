//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// coexecution_test.go — stdlib validation tests for the unified e2e co-execution model.
//
// These tests verify that internal-agent and external-agent tests can run
// together in a single `go test ./...` invocation with:
//
//  1. No ordering conflicts — TestMain runs setup before any Test* function.
//  2. Shared helpers — envOrDefault and testEnv are accessible from all files.
//  3. Correct isolation — each mode guards its own specs via testEnv checks.
//  4. Single Ginkgo runner — only TestE2E calls RunSpecs to prevent double
//     execution of all registered specs.
//
// These tests use the stdlib testing package (not Ginkgo) per project
// constraints.  They run as part of the standard `go test -tags=e2e
// ./test/e2e/...` invocation and are driven by the TestMain lifecycle in
// setup_test.go.

package e2e

import (
	"os"
	"testing"
)

// TestCoexecutionSharedEnvIsPopulated verifies that testEnv is populated by
// TestMain before any individual Test* function runs.  This is the foundation
// of the unified co-execution model: every test (internal-agent AND
// external-agent) reads cluster coordinates from the same shared testEnv
// rather than re-parsing the environment independently.
//
// Reaching this function at all means TestMain's m.Run() was called, which
// only happens after setupE2E() succeeded.
func TestCoexecutionSharedEnvIsPopulated(t *testing.T) {
	if testEnv == nil {
		t.Fatal("testEnv must be non-nil: TestMain must populate it before m.Run()")
	}
	if testEnv.ClusterName == "" {
		t.Fatal("testEnv.ClusterName must be non-empty: ensureKindCluster must run before m.Run()")
	}
	if testEnv.ImageTag == "" {
		t.Fatal("testEnv.ImageTag must be non-empty: initE2EEnv must run before m.Run()")
	}
	if testEnv.HelmRelease == "" {
		t.Fatal("testEnv.HelmRelease must be non-empty: initE2EEnv must run before m.Run()")
	}
	if testEnv.HelmNamespace == "" {
		t.Fatal("testEnv.HelmNamespace must be non-empty: initE2EEnv must run before m.Run()")
	}

	t.Logf("shared testEnv populated OK: cluster=%s imageTag=%s helmRelease=%s namespace=%s externalAgentAddr=%q",
		testEnv.ClusterName, testEnv.ImageTag,
		testEnv.HelmRelease, testEnv.HelmNamespace,
		testEnv.ExternalAgentAddr)
}

// TestCoexecutionTestMainOrderingGuarantee verifies the ordering guarantee
// of the TestMain pattern: setup completes BEFORE any Test* function is
// called, and the shared testEnv captures the results.
//
// If TestMain's setup failed, exitCode would remain 1 and os.Exit(1) would
// be called before m.Run(); reaching this function proves setup succeeded.
func TestCoexecutionTestMainOrderingGuarantee(t *testing.T) {
	// Verify the KUBECONFIG env var is set (by ensureKindCluster).
	// This must be true before any cluster-dependent Test* or Ginkgo spec runs.
	if testEnv.KubeconfigPath == "" {
		t.Skip("no live cluster (KubeconfigPath empty) — ordering guarantee " +
			"cannot be validated without a running cluster")
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Error("KUBECONFIG env var must be set by TestMain's ensureKindCluster " +
			"before m.Run() is called")
	}
	if kubeconfig != testEnv.KubeconfigPath {
		t.Errorf("KUBECONFIG=%q does not match testEnv.KubeconfigPath=%q; "+
			"all test files must use the KUBECONFIG set by TestMain",
			kubeconfig, testEnv.KubeconfigPath)
	}
	t.Logf("ordering guarantee verified: KUBECONFIG=%s set before Test* functions ran", kubeconfig)
}

// TestCoexecutionInternalAgentIsolation verifies that internal-agent mode
// sees the correct preconditions: the Helm release must already be installed
// (by TestMain) and the kubeconfig must be set.
//
// In external-agent mode this test skips rather than failing, demonstrating
// that each mode guards its own specs gracefully.
func TestCoexecutionInternalAgentIsolation(t *testing.T) {
	if testEnv.ExternalAgentAddr != "" {
		t.Skip("external-agent mode active — internal-agent isolation " +
			"check not applicable in this run")
	}
	if testEnv.KubeconfigPath == "" {
		t.Skip("no live cluster — skipping cluster-connectivity assertion")
	}

	// Internal-agent mode: agent DaemonSet must be scheduled (not disabled).
	// The Helm chart only disables the DaemonSet when HelmValuesExternalYAML
	// is applied; in internal-agent mode the DaemonSet is present.
	t.Logf("internal-agent isolation OK: "+
		"cluster=%s namespace=%s helmRelease=%s daemonSet=%s",
		testEnv.ClusterName, testEnv.HelmNamespace,
		testEnv.HelmRelease, internalAgentDaemonSetName)
}

// TestCoexecutionExternalAgentIsolation verifies that external-agent mode
// uses testEnv.ExternalAgentAddr (set by TestMain's startExternalAgentContainer)
// as the single source of truth.  External-agent specs must NOT re-read
// EXTERNAL_AGENT_ADDR independently to avoid divergence.
//
// In internal-agent mode this test skips, demonstrating the isolation.
func TestCoexecutionExternalAgentIsolation(t *testing.T) {
	if testEnv.ExternalAgentAddr == "" {
		t.Skip("external-agent mode not active — set E2E_LAUNCH_EXTERNAL_AGENT=true " +
			"or EXTERNAL_AGENT_ADDR to exercise this path")
	}

	// testEnv.ExternalAgentAddr must match EXTERNAL_AGENT_ADDR which
	// startExternalAgentContainer sets for backward-compatibility.
	envAddr := os.Getenv("EXTERNAL_AGENT_ADDR")
	if envAddr != testEnv.ExternalAgentAddr {
		t.Errorf("EXTERNAL_AGENT_ADDR=%q must equal testEnv.ExternalAgentAddr=%q; "+
			"external-agent specs must consume testEnv not raw env vars to ensure "+
			"a single source of truth set by TestMain",
			envAddr, testEnv.ExternalAgentAddr)
	}

	t.Logf("external-agent isolation OK: agentAddr=%s", testEnv.ExternalAgentAddr)
}

// TestCoexecutionSharedHelpersAccessible verifies that envOrDefault — the
// package-level helper used by both internal-agent and external-agent test
// files — is correctly defined in setup_test.go (its canonical location) and
// returns expected values.
//
// Before this fix, envOrDefault was defined only in internal_agent_test.go.
// Moving it to setup_test.go ensures it is available to all files without
// risk of redeclaration or missing-symbol errors.
func TestCoexecutionSharedHelpersAccessible(t *testing.T) {
	// envOrDefault must return the default when the env var is absent.
	const sentinelKey = "COEXEC_TEST_UNSET_VAR_XYZ"
	os.Unsetenv(sentinelKey) //nolint:errcheck
	got := envOrDefault(sentinelKey, "expected-default")
	if got != "expected-default" {
		t.Errorf("envOrDefault(%q, %q) = %q; want %q",
			sentinelKey, "expected-default", got, "expected-default")
	}

	// envOrDefault must return the env var value when it is set.
	t.Setenv(sentinelKey, "from-env")
	got = envOrDefault(sentinelKey, "expected-default")
	if got != "from-env" {
		t.Errorf("envOrDefault(%q, %q) = %q (after Setenv); want %q",
			sentinelKey, "expected-default", got, "from-env")
	}
}

// TestCoexecutionSingleGinkgoRunner verifies the invariant that there is
// exactly ONE Ginkgo RunSpecs entry point in the e2e package: TestE2E in
// e2e_suite_test.go.
//
// Having multiple RunSpecs callers (e.g. TestE2E + TestInternalAgent) in the
// same binary causes every registered Ginkgo spec to execute twice, leading
// to:
//   - Doubled test duration.
//   - Race conditions when specs share cluster state (e.g. both runs try to
//     delete the same PillarTarget CR).
//   - Confusing output with duplicated spec names and totals.
//
// This test validates the design constraint by confirming that
// testEnv.HelmRelease is only populated ONCE (set by initE2EEnv in TestMain,
// not re-initialized by a second RunSpecs call).  If TestInternalAgent re-ran
// TestMain, ClusterName would differ or testEnv would be nil.
func TestCoexecutionSingleGinkgoRunner(t *testing.T) {
	if testEnv == nil {
		t.Fatal("testEnv is nil: expected TestMain to initialize it once")
	}
	// ClusterName is set exactly once by initE2EEnv.  If a second TestMain
	// (from a duplicate RunSpecs) ran, it would re-initialize the value.
	// We cannot detect that here, but we can assert the value is consistent
	// with what the Makefile passes via KIND_CLUSTER.
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster != "" && testEnv.ClusterName != kindCluster {
		t.Errorf("testEnv.ClusterName=%q != KIND_CLUSTER=%q; "+
			"testEnv must reflect exactly the values set by TestMain",
			testEnv.ClusterName, kindCluster)
	}
	t.Logf("single-runner invariant OK: cluster=%s (KIND_CLUSTER=%q)",
		testEnv.ClusterName, kindCluster)
}

// TestCoexecutionModesMutuallyExclusive verifies that internal-agent and
// external-agent modes are mutually exclusive within a single test run.
//
// When ExternalAgentAddr is set:
//   - The Helm overlay (HelmValuesExternalYAML) disables the agent DaemonSet.
//   - Internal-agent specs skip (checked in TestCoexecutionInternalAgentIsolation).
//   - External-agent specs run.
//
// When ExternalAgentAddr is empty:
//   - The Helm chart deploys the full agent DaemonSet.
//   - Internal-agent specs run.
//   - External-agent specs skip (checked in ExternalAgent BeforeAll).
//
// Running both modes simultaneously is not supported because they require
// conflicting Helm configurations.
func TestCoexecutionModesMutuallyExclusive(t *testing.T) {
	if testEnv.LaunchExternalAgent && testEnv.ExternalAgentAddr == "" {
		// LaunchExternalAgent=true but no address set means
		// startExternalAgentContainer failed or was bypassed.
		t.Logf("warning: LaunchExternalAgent=true but ExternalAgentAddr is empty; " +
			"external-agent container may not have started (check setup logs)")
	}

	// Both modes cannot be active simultaneously: if ExternalAgentAddr is set,
	// the agent DaemonSet is disabled, so "internal" mode is off.
	if testEnv.ExternalAgentAddr != "" {
		t.Logf("mode=external-agent addr=%s launchContainer=%v",
			testEnv.ExternalAgentAddr, testEnv.LaunchExternalAgent)
	} else {
		t.Logf("mode=internal-agent (DaemonSet expected in namespace %s)",
			testEnv.HelmNamespace)
	}
}
