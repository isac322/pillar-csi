//go:build e2e

package e2e

// smoke_test.go — Sub-AC 3 of AC 1: No-op smoke test that registers with the
// framework and asserts cluster reachability.
//
// This is the minimal "single passing TC" that verifies the entire E2E pipeline
// is wired correctly:
//
//	make test-e2e
//	  → prereq checks pass
//	  → Kind cluster created
//	  → image built/loaded
//	  → backend provisioned
//	  → THIS SPEC PASSES (cluster reachable)
//	  → teardown
//
// The spec is intentionally lightweight — it only checks that the Kubernetes
// API server is reachable, not that any pillar-csi workloads are deployed.
//
// Labels:
//   - "smoke" — selectable via --label-filter=smoke
//   - "AC1"   — selectable via --label-filter=AC1
//
// NOT labeled "default-profile" to avoid counting towards the 416-case budget.
//
// Run with:
//
//	go test -tags=e2e ./test/e2e/ -run TestE2E -- --label-filter=smoke

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var _ = Describe("Smoke: cluster reachability", Label("smoke", "AC1", "default-profile"), func() {

	// ── TC-SMOKE-1: framework wiring ─────────────────────────────────────────

	It("[TC-SMOKE-1] Kind cluster is bootstrapped and reachable", func() {
		// 1. Verify the suite-level Kind cluster state was populated by
		//    SynchronizedBeforeSuite (kind_bootstrap_e2e_test.go).
		Expect(suiteKindCluster).NotTo(BeNil(),
			"[TC-SMOKE-1] suiteKindCluster must be non-nil — "+
				"SynchronizedBeforeSuite should have bootstrapped the Kind cluster "+
				"before any spec runs")

		Expect(suiteKindCluster.ClusterName).NotTo(BeEmpty(),
			"[TC-SMOKE-1] suiteKindCluster.ClusterName must be set")

		Expect(suiteKindCluster.KubeconfigPath).NotTo(BeEmpty(),
			"[TC-SMOKE-1] suiteKindCluster.KubeconfigPath must be set")

		// 2. Verify the shared rest.Config was built from the kubeconfig.
		cfg := SuiteKubeRestConfig()
		Expect(cfg).NotTo(BeNil(),
			"[TC-SMOKE-1] SuiteKubeRestConfig() must return a non-nil *rest.Config — "+
				"SynchronizedBeforeSuite must have called buildClusterRestConfig")

		Expect(cfg.Host).NotTo(BeEmpty(),
			"[TC-SMOKE-1] rest.Config.Host must be non-empty — "+
				"the config must point to the Kind API server")

		// 3. Create a typed Kubernetes client and call the API server.
		//    Use a short timeout so the spec finishes well within the 2-minute
		//    suite budget even if the API server is temporarily slow.
		clientset, err := kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-SMOKE-1] kubernetes.NewForConfig: %v", err)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// List namespaces — the lightest API call that proves the server is up.
		nsList, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred(),
			"[TC-SMOKE-1] Namespaces().List: cluster API server unreachable at %s: %v",
			cfg.Host, err)

		// A freshly created Kind cluster always has at least the kube-system and
		// default namespaces.
		Expect(nsList.Items).NotTo(BeEmpty(),
			"[TC-SMOKE-1] expected at least one namespace in the Kind cluster, got none")

		// Log the reachable namespaces for quick diagnosis in CI output.
		names := make([]string, 0, len(nsList.Items))
		for _, ns := range nsList.Items {
			names = append(names, ns.Name)
		}
		_, _ = GinkgoWriter.Write([]byte(
			"[TC-SMOKE-1] cluster reachable — namespaces: " +
				joinStrings(names) + "\n",
		))
	})
})

// joinStrings joins a slice of strings with ", " for display in test output.
func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
