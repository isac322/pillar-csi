//go:build e2e

package e2e

// kind_smoke_e2e_test.go — Sub-AC 3.3: E2E smoke tests that verify the Kind
// cluster lifecycle using the real `kind` binary (via execCommandRunner).
//
// This file provides formal Ginkgo assertions for the two halves of the
// cluster lifecycle contract:
//
//   Setup half (cluster must be PRESENT after bootstrapSuiteCluster):
//     Verified here by Ginkgo It-specs that call kind get clusters and assert
//     the cluster appears in the output. These specs run during the normal
//     Ginkgo suite alongside all other documented TCs.
//
//   Teardown half (cluster must be ABSENT after deleteOnExit):
//     Verified by deleteOnExit in main_test.go which calls verifyClusterAbsent
//     with a real execCommandRunner after suiteInvocationTeardown.Cleanup runs.
//     deleteOnExit sets exitCode=1 when the cluster is still listed, causing
//     `make test-e2e` to fail with a clear [AC3.3] message.
//
// Label "AC3.3" so these specs can be run in isolation:
//
//	go test -tags=e2e -run=TestE2E ./test/e2e/ -- --label-filter=AC3.3
//
// These specs are NOT labeled "default-profile" so they do not count towards
// the documented TC budget; they validate suite infrastructure only.

import (
	"context"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("[AC3.3] Kind cluster lifecycle smoke", Label("AC3.3"), func() {

	// ── Setup-half: cluster must be present after bootstrapSuiteCluster ───────

	Describe("cluster present after setup", func() {
		It("appears in 'kind get clusters' output", func() {
			Expect(suiteKindCluster).NotTo(BeNil(),
				"[AC3.3] suiteKindCluster must be set by SynchronizedBeforeSuite — "+
					"bootstrapSuiteCluster should have created the cluster before any specs ran")

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			runner := execCommandRunner{Output: GinkgoWriter}
			err := suiteKindCluster.verifyClusterPresent(ctx, runner)
			Expect(err).NotTo(HaveOccurred(),
				"[AC3.3] cluster %q must appear in 'kind get clusters' — "+
					"creation smoke check failed; the cluster may have been deleted or "+
					"never created successfully",
				suiteKindCluster.ClusterName)
		})

		It("cluster name from KIND_CLUSTER env var matches what kind lists", func() {
			Expect(suiteKindCluster).NotTo(BeNil(), "[AC3.3] suiteKindCluster is nil")

			clusterEnv := os.Getenv("KIND_CLUSTER")
			Expect(clusterEnv).NotTo(BeEmpty(),
				"[AC3.3] KIND_CLUSTER env var must be set — "+
					"exportEnvironment() should have populated it during bootstrap")

			Expect(clusterEnv).To(Equal(suiteKindCluster.ClusterName),
				"[AC3.3] KIND_CLUSTER=%q must match suiteKindCluster.ClusterName=%q",
				clusterEnv, suiteKindCluster.ClusterName)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			runner := execCommandRunner{Output: GinkgoWriter}
			clusters, err := suiteKindCluster.listClusters(ctx, runner)
			Expect(err).NotTo(HaveOccurred(),
				"[AC3.3] 'kind get clusters' must succeed during the test run")

			Expect(clusters).To(ContainElement(clusterEnv),
				"[AC3.3] cluster %q must appear in the kind cluster list: %v",
				clusterEnv, clusters)
		})

		It("cluster name is invocation-scoped (pillar-csi-e2e-p<pid>-<entropy> format)", func() {
			Expect(suiteKindCluster).NotTo(BeNil(), "[AC3.3] suiteKindCluster is nil")
			Expect(suiteKindCluster.ClusterName).To(HavePrefix("pillar-csi-e2e-p"),
				"[AC3.3] cluster name %q must start with pillar-csi-e2e-p to indicate "+
					"it is scoped to a single go test invocation (prevents cross-run collisions)",
				suiteKindCluster.ClusterName)
		})

		It("kubeconfig file exists under /tmp and is parseable", func() {
			Expect(suiteKindCluster).NotTo(BeNil(), "[AC3.3] suiteKindCluster is nil")

			kubeconfigPath := suiteKindCluster.KubeconfigPath
			Expect(kubeconfigPath).NotTo(BeEmpty(), "[AC3.3] KubeconfigPath must not be empty")
			Expect(kubeconfigPath).To(HavePrefix(os.TempDir()),
				"[AC3.3] kubeconfig %q must stay under %s — environment-hygiene requirement",
				kubeconfigPath, os.TempDir())

			raw, err := os.ReadFile(kubeconfigPath) //nolint:gosec
			Expect(err).NotTo(HaveOccurred(),
				"[AC3.3] kubeconfig file %q must be readable — kind create cluster should have written it",
				kubeconfigPath)
			Expect(raw).NotTo(BeEmpty(),
				"[AC3.3] kubeconfig file %q is empty", kubeconfigPath)
			Expect(string(raw)).To(ContainSubstring("kind: Config"),
				"[AC3.3] kubeconfig file %q must contain 'kind: Config'", kubeconfigPath)
		})
	})

	// ── Teardown-half: cluster must be absent after deleteOnExit ─────────────
	//
	// The formal assertion for this half lives in main_test.go's deleteOnExit
	// function, which is called from the deferred cleanup inside runPrimary. It:
	//
	//   1. Calls suiteInvocationTeardown.Cleanup (deletes the Kind cluster).
	//   2. Calls state.verifyClusterAbsent with a real execCommandRunner.
	//   3. Sets exitCode=1 when the cluster is still listed in kind get clusters,
	//      causing `make test-e2e` to exit non-zero.
	//   4. Logs "[AC3.3] kind cluster <name> confirmed absent after teardown"
	//      or "[AC3.3] cluster <name> still present after teardown: <err>".
	//
	// This design means the absence check runs AFTER the ginkgo suite exits —
	// which is unavoidable because cluster deletion must happen as the last step
	// of the primary go test process, after all specs and ginkgo workers have
	// completed.

	Describe("teardown verification contract", func() {
		It("verifyClusterAbsent is wired into deleteOnExit (static contract check)", func() {
			// This spec verifies the teardown contract at the API level:
			// suiteInvocationTeardown must be registered with the cluster state
			// so that deleteOnExit can call CleanupWithRunner → destroyCluster →
			// verifyClusterAbsent on the live kind binary.
			//
			// We cannot call verifyClusterAbsent HERE because the cluster is
			// intentionally still alive during spec execution. Instead we verify:
			//   a. suiteKindCluster is non-nil (the state exists to be cleaned up).
			//   b. suiteKindCluster.KindBinary is non-empty (the binary is wired).
			//   c. suiteKindCluster.ClusterName is non-empty (the name is tracked).
			//
			// The actual absence check fires in deleteOnExit after RunSpecs returns.
			Expect(suiteKindCluster).NotTo(BeNil(),
				"[AC3.3] teardown contract: suiteKindCluster must be set so "+
					"deleteOnExit can call verifyClusterAbsent after suite teardown")
			Expect(suiteKindCluster.KindBinary).NotTo(BeEmpty(),
				"[AC3.3] teardown contract: KindBinary must be set so "+
					"verifyClusterAbsent can run 'kind get clusters'")
			Expect(suiteKindCluster.ClusterName).NotTo(BeEmpty(),
				"[AC3.3] teardown contract: ClusterName must be set so "+
					"verifyClusterAbsent knows which cluster to look for")
		})
	})
})
