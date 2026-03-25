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

// internal_agent_test.go — Ginkgo suite scaffolding for pillar-csi
// "internal agent" mode.
//
// In internal-agent mode the pillar-agent binary runs as a DaemonSet
// *inside* the Kind cluster, managed by the same Helm chart that deploys
// the controller and node components.
//
// # Running only internal-agent tests
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestInternalAgent \
//	    -- --ginkgo.label-filter=internal-agent --ginkgo.v
//
// # Running the full e2e suite (excluding internal-agent)
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestE2E \
//	    -- --ginkgo.label-filter='!internal-agent' --ginkgo.v
//
// # Architecture
//
// InternalAgentSuite holds all state shared by specs in this container.
// Suite-level setup (Helm chart install + agent DaemonSet readiness) runs
// in BeforeAll and teardown (Helm uninstall) runs in AfterAll.  The struct
// methods are named BeforeSuite / AfterSuite to make their role explicit even
// though they are invoked via Ginkgo's ordered-container lifecycle, not via
// the global BeforeSuite hook (which is reserved for the package-wide image
// build/load step in e2e_suite_test.go).

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─── Constants ───────────────────────────────────────────────────────────────

const (
	// internalAgentHelmRelease is the default Helm release name for the
	// internal-agent e2e tests.
	internalAgentHelmRelease = "pillar-csi"

	// internalAgentNamespace is the default Kubernetes namespace into which
	// the Helm chart is installed for internal-agent tests.
	internalAgentNamespace = "pillar-csi-system"

	// internalAgentDaemonSetName is the name of the agent DaemonSet created
	// by the Helm chart.  Used to verify readiness after deployment.
	internalAgentDaemonSetName = "pillar-csi-agent"

	// internalAgentHelmChart is the path to the Helm chart, relative to the
	// project root.  utils.Run sets cmd.Dir to the project root.
	internalAgentHelmChart = "charts/pillar-csi"

	// defaultInternalAgentReadyTimeout is the maximum time to wait for all
	// agent DaemonSet pods to become Ready before aborting BeforeSuite.
	defaultInternalAgentReadyTimeout = 3 * time.Minute
)

// ─── InternalAgentSuite ──────────────────────────────────────────────────────

// InternalAgentSuite holds the shared state for the pillar-csi internal-agent
// e2e test suite.
//
// Typical lifecycle inside an Ordered Describe container:
//
//	BeforeAll → s.BeforeSuite(ctx)   // helm install + DaemonSet readiness
//	It specs  → use s.Client, s.Namespace, etc.
//	AfterAll  → s.AfterSuite(ctx)   // helm uninstall
type InternalAgentSuite struct {
	// Client is the controller-runtime client connected to the test cluster.
	// Populated by BeforeSuite; nil before that.
	Client client.Client

	// Namespace is the Kubernetes namespace where the Helm release is
	// installed.  Defaults to internalAgentNamespace.
	Namespace string

	// HelmRelease is the name passed to `helm upgrade --install`.
	// Defaults to internalAgentHelmRelease.
	HelmRelease string

	// HelmValuesFile is the path to the e2e Helm values override file,
	// relative to the project root.  Defaults to hack/e2e-values.yaml.
	HelmValuesFile string

	// agentReadyTimeout caps BeforeSuite's wait for the agent DaemonSet.
	agentReadyTimeout time.Duration
}

// newInternalAgentSuite constructs an InternalAgentSuite, reading overrides
// from environment variables.
//
// Supported env vars:
//
//	HELM_RELEASE          Helm release name             (default: pillar-csi)
//	HELM_NAMESPACE        Namespace for the release     (default: pillar-csi-system)
//	HELM_VALUES           Path to values override file  (default: hack/e2e-values.yaml)
//	AGENT_READY_TIMEOUT   time.Duration string for the  (default: 3m)
//	                      agent DaemonSet readiness wait
func newInternalAgentSuite() *InternalAgentSuite {
	s := &InternalAgentSuite{
		HelmRelease:       envOrDefault("HELM_RELEASE", internalAgentHelmRelease),
		Namespace:         envOrDefault("HELM_NAMESPACE", internalAgentNamespace),
		HelmValuesFile:    envOrDefault("HELM_VALUES", "hack/e2e-values.yaml"),
		agentReadyTimeout: defaultInternalAgentReadyTimeout,
	}
	if v := os.Getenv("AGENT_READY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			s.agentReadyTimeout = d
		}
	}
	return s
}

// BeforeSuite connects to the test cluster, installs (or upgrades) the
// pillar-csi Helm chart with the e2e values override, and waits for the
// agent DaemonSet rollout to complete.
//
// Any error causes Ginkgo to fail the suite immediately.
//
// Called from BeforeAll in the InternalAgent Ordered Describe container.
func (s *InternalAgentSuite) BeforeSuite(ctx context.Context) {
	By("connecting to the test cluster")
	fw, err := framework.SetupSuite()
	Expect(err).NotTo(HaveOccurred(), "internal-agent BeforeSuite: cluster connectivity check failed")
	s.Client = fw.Client

	By("deploying the pillar-csi Helm chart (controller + node + agent DaemonSet)")
	helmArgs := []string{
		"upgrade", "--install",
		s.HelmRelease,
		internalAgentHelmChart,
		"--namespace", s.Namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "5m",
		"-f", s.HelmValuesFile,
	}
	helmCmd := exec.CommandContext(ctx, "helm", helmArgs...)
	helmCmd.Stdout = GinkgoWriter
	helmCmd.Stderr = GinkgoWriter
	Expect(helmCmd.Run()).To(Succeed(),
		"internal-agent BeforeSuite: helm upgrade --install failed "+
			"(release=%s, namespace=%s, values=%s)",
		s.HelmRelease, s.Namespace, s.HelmValuesFile)

	By("waiting for the agent DaemonSet rollout to complete")
	rolloutCmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		"daemonset/"+internalAgentDaemonSetName,
		"--namespace", s.Namespace,
		"--timeout", s.agentReadyTimeout.String(),
	)
	rolloutCmd.Stdout = GinkgoWriter
	rolloutCmd.Stderr = GinkgoWriter
	Expect(rolloutCmd.Run()).To(Succeed(),
		"internal-agent BeforeSuite: agent DaemonSet %q not ready within %s",
		internalAgentDaemonSetName, s.agentReadyTimeout)
}

// AfterSuite uninstalls the pillar-csi Helm release, removing the agent
// DaemonSet and all other chart-managed resources from the cluster.
//
// Teardown errors are printed as warnings rather than failing the suite so
// that subsequent runs start with a clean state even when a prior run left
// the environment partially deployed.
//
// Called from AfterAll in the InternalAgent Ordered Describe container.
func (s *InternalAgentSuite) AfterSuite(ctx context.Context) {
	if s.HelmRelease == "" {
		return
	}
	By("uninstalling the pillar-csi Helm chart (internal agent DaemonSet mode)")
	cmd := exec.CommandContext(ctx, "helm", "uninstall", s.HelmRelease,
		"--namespace", s.Namespace,
		"--wait",
		"--timeout", "3m",
	)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"WARNING: internal-agent AfterSuite: helm uninstall failed "+
				"(release may already be absent): %v\n", err)
	}
}

// ─── Ginkgo entry point ──────────────────────────────────────────────────────

// TestInternalAgent is the Ginkgo entry point for the internal-agent test
// suite.  It can be invoked in isolation to run only internal-agent tests:
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestInternalAgent \
//	    -- --ginkgo.label-filter=internal-agent --ginkgo.v
//
// Note: the package-level BeforeSuite (in e2e_suite_test.go) is shared
// across all Ginkgo runners in this package; it handles image build/load
// tasks that must complete before any cluster-dependent spec can run.
func TestInternalAgent(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting pillar-csi internal-agent integration test suite\n")
	RunSpecs(t, "Internal Agent Suite")
}

// ─── Suite instance ──────────────────────────────────────────────────────────

// internalAgentSuite is the package-level singleton shared by all specs in
// the InternalAgent Describe container below.  It is initialised in BeforeAll
// and safe to read in any It / BeforeEach block inside that container.
var internalAgentSuite *InternalAgentSuite

// ─── Ginkgo container ────────────────────────────────────────────────────────

var _ = Describe("InternalAgent", Ordered, Label("internal-agent"), func() {
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

	// ── Scaffolding placeholder ───────────────────────────────────────────

	// Real specs live in dedicated *_test.go files alongside this one
	// (e.g. internal_agent_pvc_test.go, internal_agent_lifecycle_test.go).
	// Those files reference the package-level internalAgentSuite variable.
	It("has the agent DaemonSet deployed and Running", func() {
		Skip("scaffolding placeholder — real specs live in dedicated spec files")
	})
})

// ─── Helpers ─────────────────────────────────────────────────────────────────

// envOrDefault returns the value of env var key, or defaultValue when the
// variable is unset or empty.
func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
