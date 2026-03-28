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

// internal_agent_test.go — Ginkgo specs for pillar-csi "internal agent" mode.
//
// In internal-agent mode the pillar-agent binary runs as a DaemonSet
// *inside* the Kind cluster, managed by the same Helm chart that deploys
// the controller and node components.
//
// # Running only internal-agent specs via label filter
//
//	make test-e2e E2E_TEST_ARGS="-run TestE2E -- --ginkgo.label-filter=internal-agent"
//
// # Architecture
//
// InternalAgentSuite holds all state shared by specs in this container.
// The cluster lifecycle (Kind, images, Helm install) is fully managed by
// TestMain in setup_test.go.  BeforeSuite here only connects to the
// already-running cluster and waits for the agent DaemonSet to be ready.
// AfterSuite is intentionally a no-op — Helm uninstall is handled by
// TestMain's teardown.
//
// There is NO separate TestInternalAgent entry point.  All Ginkgo specs
// (InternalAgent, ExternalAgent, and others) are run by the single TestE2E
// function in e2e_suite_test.go, which calls RunSpecs once.  Having multiple
// RunSpecs calls in one binary causes every spec to execute twice.

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─── Constants ───────────────────────────────────────────────────────────────

const (
	// internalAgentDaemonSetName is the name of the agent DaemonSet created
	// by the Helm chart.  Used to verify readiness after deployment.
	internalAgentDaemonSetName = "pillar-csi-agent"

	// defaultInternalAgentReadyTimeout is the maximum time to wait for all
	// agent DaemonSet pods to become Ready before aborting BeforeSuite.
	defaultInternalAgentReadyTimeout = 90 * time.Second
)

// ─── InternalAgentSuite ──────────────────────────────────────────────────────

// InternalAgentSuite holds the shared state for the pillar-csi internal-agent
// Ginkgo specs.
//
// The cluster lifecycle (Kind cluster, Docker images, Helm install) is fully
// managed by TestMain in setup_test.go.  This suite only:
//   - connects to the already-running cluster (via framework.SetupSuite)
//   - waits for the agent DaemonSet rollout to complete
//
// AfterSuite is intentionally a no-op.  Helm uninstall and cluster teardown
// are handled by TestMain's deferred teardownE2E().
//
// Typical lifecycle inside the Ordered Describe container below:
//
//	BeforeAll → s.BeforeSuite(ctx)   // cluster connect + DaemonSet readiness
//	It specs  → use s.Client, s.Namespace, etc.
//	AfterAll  → s.AfterSuite(ctx)   // no-op
type InternalAgentSuite struct {
	// Client is the controller-runtime client connected to the test cluster.
	// Populated by BeforeSuite; nil before that.
	Client client.Client

	// Namespace is the Kubernetes namespace where the Helm release is
	// installed.  Sourced from testEnv.HelmNamespace (set by TestMain).
	Namespace string

	// HelmRelease is the release name used to identify the deployment.
	// Sourced from testEnv.HelmRelease (set by TestMain).
	HelmRelease string

	// agentReadyTimeout caps BeforeSuite's wait for the agent DaemonSet.
	agentReadyTimeout time.Duration
}

// newInternalAgentSuite constructs an InternalAgentSuite using values already
// established by TestMain (testEnv).  The AGENT_READY_TIMEOUT env var may
// override the DaemonSet readiness wait duration.
func newInternalAgentSuite() *InternalAgentSuite {
	s := &InternalAgentSuite{
		// Consume the coordinates set by TestMain so both the suite and TestMain
		// agree on which namespace / release to look at.
		HelmRelease:       testEnv.HelmRelease,
		Namespace:         testEnv.HelmNamespace,
		agentReadyTimeout: defaultInternalAgentReadyTimeout,
	}
	if v := envOrDefault("AGENT_READY_TIMEOUT", ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			s.agentReadyTimeout = d
		}
	}
	return s
}

// BeforeSuite connects to the already-running test cluster (bootstrapped by
// TestMain) and waits for the agent DaemonSet rollout to complete.
//
// Note: Helm install is NOT performed here.  TestMain already ran
// installHelm() before calling m.Run(), so the DaemonSet exists by the time
// this function is called.  Re-installing here would conflict with TestMain
// and reference values files that no longer exist.
//
// Any error causes Ginkgo to fail the suite immediately.
//
// Called from BeforeAll in the InternalAgent Ordered Describe container.
func (s *InternalAgentSuite) BeforeSuite(ctx context.Context) {
	By("connecting to the test cluster (bootstrapped by TestMain)")
	fw, err := framework.SetupSuite()
	Expect(err).NotTo(HaveOccurred(),
		"internal-agent BeforeSuite: cluster connectivity check failed; "+
			"KUBECONFIG=%s", testEnv.KubeconfigPath)
	s.Client = fw.Client

	By("waiting for the agent DaemonSet rollout to complete")
	rolloutCmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		"daemonset/"+internalAgentDaemonSetName,
		"--namespace", s.Namespace,
		"--timeout", s.agentReadyTimeout.String(),
	)
	rolloutCmd.Stdout = GinkgoWriter
	rolloutCmd.Stderr = GinkgoWriter
	Expect(rolloutCmd.Run()).To(Succeed(),
		"internal-agent BeforeSuite: agent DaemonSet %q not ready within %s "+
			"(Helm release=%s, namespace=%s)",
		internalAgentDaemonSetName, s.agentReadyTimeout,
		s.HelmRelease, s.Namespace)
}

// AfterSuite is intentionally a no-op.
//
// Helm uninstall and Kind cluster deletion are handled by TestMain's deferred
// teardownE2E() which runs after m.Run() returns.  Performing Helm uninstall
// here would:
//   - Conflict with TestMain's teardown (double-uninstall → spurious errors).
//   - Run BEFORE other spec containers in the same suite have finished,
//     disrupting any shared cluster state they rely on.
//
// Called from AfterAll in the InternalAgent Ordered Describe container.
func (s *InternalAgentSuite) AfterSuite(_ context.Context) {
	// Intentional no-op.  See doc comment above.
	_, _ = fmt.Fprintf(GinkgoWriter,
		"internal-agent AfterSuite: no-op (Helm teardown handled by TestMain)\n")
}

// ─── Suite instance ──────────────────────────────────────────────────────────

// internalAgentSuite is the package-level singleton shared by all specs in
// the InternalAgent Describe container below.  It is initialised in BeforeAll
// and safe to read in any It / BeforeEach block inside that container.
var internalAgentSuite *InternalAgentSuite

// ─── Ginkgo container ────────────────────────────────────────────────────────

// InternalAgent specs are only registered in internal-agent mode (no external
// agent container).  Conditional registration avoids "S" (skipped) marks in
// the Ginkgo summary when the suite is run in external-agent mode.
var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}
	Describe("InternalAgent", Ordered, Label("internal-agent"), func() {
		// BeforeAll deploys the full pillar-csi stack (including agent DaemonSet)
		// exactly once before any spec in this container runs.
		BeforeAll(func(ctx context.Context) {
			internalAgentSuite = newInternalAgentSuite()
			internalAgentSuite.BeforeSuite(ctx)
		})

		// AfterAll tears down the Helm release once after all specs in this
		// container have completed (pass or fail).
		AfterAll(func(ctx context.Context) {
			if internalAgentSuite != nil {
				internalAgentSuite.AfterSuite(ctx)
			}
		})

		// ── DaemonSet readiness check ─────────────────────────────────────────

		// Verify that BeforeAll successfully connected to the cluster and that
		// the InternalAgentSuite was initialised.  Real functional specs live in
		// dedicated *_test.go files alongside this one (e.g.
		// internal_agent_functional_test.go, internal_agent_daemonset_test.go).
		It("has the agent DaemonSet deployed and Running", func() {
			Expect(internalAgentSuite).NotTo(BeNil(),
				"InternalAgentSuite must be initialised by BeforeAll")
			Expect(internalAgentSuite.Client).NotTo(BeNil(),
				"Kubernetes client must be connected to the test cluster")
		})
	}) // end Describe("InternalAgent")
	return true
}()

// Note: envOrDefault is defined in setup_test.go and is accessible to all
// files in this package.
