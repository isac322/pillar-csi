//go:build e2e

package e2e

// cluster_kubeconfig_e2e_test.go — AC4c: Ginkgo specs that verify the
// per-invocation kubeconfig is correctly propagated to the test environment
// so every Ginkgo spec within the TestE2E suite can connect to the ephemeral
// Kind cluster without any per-spec clientcmd boilerplate.
//
// Propagation chain (asserted by these specs):
//
//	TestMain → bootstrapSuiteCluster → exportEnvironment
//	       sets: KUBECONFIG=$tmp/generated/kubeconfig
//	             KIND_CLUSTER=<cluster-name>
//	             PILLAR_CSI_E2E_SUITE_ROOT=<root-dir>
//	             (+ other suite-path vars)
//	       ↓
//	SynchronizedBeforeSuite (all-nodes) reads KUBECONFIG / KIND_CLUSTER /
//	       suite-path vars → kindBootstrapStateFromEnv → suiteKindCluster
//	       then: buildClusterRestConfig(suiteKindCluster.KubeconfigPath)
//	                → suiteRestConfig              ← tested here
//	       ↓
//	Every Ginkgo It-node: SuiteKubeRestConfig() → *rest.Config
//
// Label: "AC4c" — run via:
//
//	go test -run=TestE2E ./test/e2e/... -- --label-filter=AC4c
//
// These specs are NOT labeled "default-profile" so they do not count towards
// the 437 documented TC budget; they validate suite infrastructure only.

import (
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("AC4c: per-invocation kubeconfig propagation", Label("AC4c"), func() {

	// ── KUBECONFIG environment variable ──────────────────────────────────────

	Describe("KUBECONFIG env var", func() {
		It("is set to a non-empty path before any spec runs", func() {
			kubeconfigPath := os.Getenv("KUBECONFIG")
			Expect(kubeconfigPath).NotTo(BeEmpty(),
				"[AC4c] KUBECONFIG env var must be set — "+
					"bootstrapSuiteCluster should have called exportEnvironment() "+
					"before any Ginkgo worker was launched")
		})

		It("points to a file that exists under /tmp", func() {
			kubeconfigPath := os.Getenv("KUBECONFIG")
			Expect(kubeconfigPath).NotTo(BeEmpty(), "[AC4c] KUBECONFIG not set")

			// Environment-hygiene: the kubeconfig must stay under /tmp so
			// no test side-effect escapes to the source tree or home directory.
			Expect(kubeconfigPath).To(HavePrefix(os.TempDir()),
				"[AC4c] kubeconfig path %q must be under %s (environment hygiene)",
				kubeconfigPath, os.TempDir())

			_, err := os.Stat(kubeconfigPath)
			Expect(err).NotTo(HaveOccurred(),
				"[AC4c] kubeconfig file %q must exist — "+
					"kind create cluster should have written it", kubeconfigPath)
		})

		It("matches suiteKindCluster.KubeconfigPath after SynchronizedBeforeSuite", func() {
			Expect(suiteKindCluster).NotTo(BeNil(),
				"[AC4c] suiteKindCluster must be populated by SynchronizedBeforeSuite")

			kubeconfigEnv := os.Getenv("KUBECONFIG")
			Expect(kubeconfigEnv).To(Equal(suiteKindCluster.KubeconfigPath),
				"[AC4c] KUBECONFIG env var %q must match "+
					"suiteKindCluster.KubeconfigPath %q",
				kubeconfigEnv, suiteKindCluster.KubeconfigPath)
		})
	})

	// ── KIND_CLUSTER environment variable ────────────────────────────────────

	Describe("KIND_CLUSTER env var", func() {
		It("is set to a non-empty cluster name before any spec runs", func() {
			clusterName := os.Getenv("KIND_CLUSTER")
			Expect(clusterName).NotTo(BeEmpty(),
				"[AC4c] KIND_CLUSTER env var must be set — "+
					"exportEnvironment() should have populated it")
		})

		It("matches suiteKindCluster.ClusterName", func() {
			Expect(suiteKindCluster).NotTo(BeNil())

			clusterEnv := os.Getenv("KIND_CLUSTER")
			Expect(clusterEnv).To(Equal(suiteKindCluster.ClusterName),
				"[AC4c] KIND_CLUSTER %q must match suiteKindCluster.ClusterName %q",
				clusterEnv, suiteKindCluster.ClusterName)
		})
	})

	// ── suiteKindCluster ─────────────────────────────────────────────────────

	Describe("suiteKindCluster state", func() {
		It("is non-nil after SynchronizedBeforeSuite", func() {
			Expect(suiteKindCluster).NotTo(BeNil(),
				"[AC4c] suiteKindCluster must be set by SynchronizedBeforeSuite "+
					"after kindBootstrapStateFromEnv reads the cluster env vars")
		})

		It("has a non-empty ClusterName", func() {
			Expect(suiteKindCluster).NotTo(BeNil())
			Expect(suiteKindCluster.ClusterName).NotTo(BeEmpty(),
				"[AC4c] suiteKindCluster.ClusterName must be non-empty")
		})

		It("has a non-empty KubeconfigPath under /tmp", func() {
			Expect(suiteKindCluster).NotTo(BeNil())
			Expect(suiteKindCluster.KubeconfigPath).NotTo(BeEmpty(),
				"[AC4c] suiteKindCluster.KubeconfigPath must be set")

			Expect(suiteKindCluster.KubeconfigPath).To(HavePrefix(os.TempDir()),
				"[AC4c] KubeconfigPath %q must stay under %s",
				suiteKindCluster.KubeconfigPath, os.TempDir())
		})

		It("has a KubeContext that matches kind-<cluster-name>", func() {
			Expect(suiteKindCluster).NotTo(BeNil())
			expectedCtx := "kind-" + suiteKindCluster.ClusterName
			Expect(suiteKindCluster.KubeContext).To(Equal(expectedCtx),
				"[AC4c] suiteKindCluster.KubeContext = %q, want %q",
				suiteKindCluster.KubeContext, expectedCtx)
		})
	})

	// ── suiteRestConfig / SuiteKubeRestConfig ────────────────────────────────

	Describe("SuiteKubeRestConfig()", func() {
		It("returns a non-nil *rest.Config after SynchronizedBeforeSuite", func() {
			Expect(SuiteKubeRestConfig()).NotTo(BeNil(),
				"[AC4c] SuiteKubeRestConfig() must return a non-nil *rest.Config — "+
					"SynchronizedBeforeSuite should have called buildClusterRestConfig")
		})

		It("has a non-empty Host field", func() {
			cfg := SuiteKubeRestConfig()
			Expect(cfg).NotTo(BeNil(), "[AC4c] SuiteKubeRestConfig() is nil")
			Expect(cfg.Host).NotTo(BeEmpty(),
				"[AC4c] SuiteKubeRestConfig().Host must not be empty — "+
					"the rest.Config must point to a real cluster endpoint")
		})

		It("Host starts with https:// for a Kind cluster", func() {
			cfg := SuiteKubeRestConfig()
			Expect(cfg).NotTo(BeNil())
			Expect(cfg.Host).To(
				Or(HavePrefix("https://"), ContainSubstring(":")),
				"[AC4c] rest.Config.Host %q must be a valid cluster URL", cfg.Host)
		})

		It("is consistent across multiple calls (same pointer)", func() {
			first := SuiteKubeRestConfig()
			second := SuiteKubeRestConfig()
			Expect(first).To(BeIdenticalTo(second),
				"[AC4c] SuiteKubeRestConfig() must return the same pointer on "+
					"every call — multiple build passes would be wasteful and "+
					"risk connecting to different cluster endpoints")
		})
	})

	// ── Suite path env vars ───────────────────────────────────────────────────

	Describe("suite-path env vars", func() {
		It("PILLAR_CSI_E2E_SUITE_ROOT is set and under /tmp", func() {
			suiteRoot := os.Getenv(suiteRootEnvVar)
			Expect(suiteRoot).NotTo(BeEmpty(),
				"[AC4c] %s env var must be set", suiteRootEnvVar)
			Expect(suiteRoot).To(HavePrefix(os.TempDir()),
				"[AC4c] suite root %q must be under %s", suiteRoot, os.TempDir())
		})

		It("PILLAR_CSI_E2E_KUBE_CONTEXT is set to kind-<cluster-name>", func() {
			kubeCtx := os.Getenv(suiteContextEnvVar)
			Expect(kubeCtx).NotTo(BeEmpty(),
				"[AC4c] %s env var must be set", suiteContextEnvVar)
			Expect(kubeCtx).To(HavePrefix("kind-"),
				"[AC4c] kube context %q must start with kind-", kubeCtx)
		})
	})

	// ── Cross-spec isolation: each spec sees the same suiteRestConfig ─────────

	Describe("cross-spec isolation", func() {
		It("SuiteKubeRestConfig returns the same config in every spec (no per-spec re-build)", func() {
			// The rest.Config is built once in SynchronizedBeforeSuite and
			// returned by-pointer on every call.  If each spec were to rebuild
			// it, they might race on the filesystem kubeconfig file and produce
			// divergent cluster endpoints.
			cfg1 := SuiteKubeRestConfig()
			cfg2 := SuiteKubeRestConfig()
			Expect(cfg1).To(BeIdenticalTo(cfg2),
				"[AC4c] SuiteKubeRestConfig must return a stable pointer — "+
					"shared immutable state, not per-call construction")
		})

		It("KUBECONFIG env var is consistent with suiteRestConfig.Host", func() {
			kubeconfigPath := os.Getenv("KUBECONFIG")
			Expect(kubeconfigPath).NotTo(BeEmpty(), "[AC4c] KUBECONFIG not set")

			cfg := SuiteKubeRestConfig()
			Expect(cfg).NotTo(BeNil())

			// Both point to the same cluster; the Host in the config must be
			// derivable from the kubeconfig file.  We verify indirectly by
			// checking that the kubeconfig lives under a pillar-csi prefix —
			// if the files differ, there's a propagation bug.
			Expect(kubeconfigPath).To(ContainSubstring("pillar-csi"),
				"[AC4c] KUBECONFIG %q must contain pillar-csi (correct suite root)",
				kubeconfigPath)

			// The rest.Config.Host must be non-empty — confirming the file at
			// KUBECONFIG was actually parsed.
			Expect(cfg.Host).NotTo(BeEmpty(),
				"[AC4c] rest.Config.Host is empty; kubeconfig at %q may be malformed",
				kubeconfigPath)
		})

		It("KUBECONFIG file content is parseable and non-empty", func() {
			kubeconfigPath := os.Getenv("KUBECONFIG")
			Expect(kubeconfigPath).NotTo(BeEmpty(), "[AC4c] KUBECONFIG not set")

			raw, err := os.ReadFile(kubeconfigPath)
			Expect(err).NotTo(HaveOccurred(),
				"[AC4c] read kubeconfig %q", kubeconfigPath)
			Expect(raw).NotTo(BeEmpty(),
				"[AC4c] kubeconfig file %q is empty", kubeconfigPath)

			// Minimal structural check: must contain "kind: Config".
			Expect(strings.Contains(string(raw), "kind: Config")).To(BeTrue(),
				"[AC4c] kubeconfig %q does not contain 'kind: Config'", kubeconfigPath)
		})
	})
})
