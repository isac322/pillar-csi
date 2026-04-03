//go:build e2e && e2e_helm

package e2e

// tc_e10_node_service_e2e_test.go — Sub-AC 3: Type B Kind-cluster E2E tests for
// E10 node service (E10.1 manager deployment verification).
//
// These specs require a running Kind cluster and are NOT labeled "default-profile"
// so they do not run in the standard 437-case in-process suite.  They run when:
//
//	go test -tags=e2e ./test/e2e/ -run TestE2E -- --label-filter=E10-cluster
//
// Prerequisite: pillar-csi must be installed via Helm before these tests run.
// The E27 Helm suite (tc_e27_helm_e2e_test.go) installs and tears down the chart.
// For standalone E10 testing, pre-install with:
//
//	helm install pillar-csi ./charts/pillar-csi -n pillar-csi-system --create-namespace
//
// TC IDs covered:
//
//	E10.68 — Manager controller pod running
//	E10.69 — Manager metrics service accessible
//	E10.70 — cert-manager integration (skipped when CERT_MANAGER_INSTALL_SKIP=true)

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// e10Namespace is the namespace where pillar-csi is deployed.
	e10Namespace = "pillar-csi-system"
	// e10ControllerLabel is the label selector for the controller deployment.
	e10ControllerLabel = "app.kubernetes.io/component=controller"
)

// e10KubectlOutput runs kubectl with the suite kubeconfig and returns stdout.
// It uses the KUBECONFIG env var set by bootstrapSuiteCluster/exportEnvironment.
func e10KubectlOutput(ctx context.Context, args ...string) (string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", fmt.Errorf("[TC-E10] KUBECONFIG not set — Kind cluster not bootstrapped")
	}

	cmdArgs := append([]string{"--kubeconfig=" + kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", cmdArgs...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %w\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

var _ = Describe("E10: 클러스터 레벨 E2E 테스트", Label("E10-cluster"), func() {

	// ─────────────────────────────────────────────────────────────────────────
	// E10.1 매니저 배포 검증
	// ─────────────────────────────────────────────────────────────────────────

	Describe("E10.1 매니저 배포 검증", func() {

		// ── TC-E10.68 ─────────────────────────────────────────────────────────
		// E10.68: Manager controller pod running.
		// TestE2E/Manager_컨트롤러_파드_실행_확인
		It("[TC-E10.68] Manager controller pod Running 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyControllerPodRunning := func(g Gomega) {
				out, err := e10KubectlOutput(ctx,
					"get", "pods",
					"-n", e10Namespace,
					"-l", e10ControllerLabel,
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E10.68] kubectl get pods for controller must succeed")

				var podList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							Phase             string `json:"phase"`
							ContainerStatuses []struct {
								Name         string `json:"name"`
								Ready        bool   `json:"ready"`
								RestartCount int    `json:"restartCount"`
							} `json:"containerStatuses"`
						} `json:"status"`
					} `json:"items"`
				}

				g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed(),
					"[TC-E10.68] controller pod list JSON must be parseable")
				g.Expect(podList.Items).NotTo(BeEmpty(),
					"[TC-E10.68] at least one controller pod must exist in %s", e10Namespace)

				for _, pod := range podList.Items {
					g.Expect(pod.Status.Phase).To(Equal("Running"),
						"[TC-E10.68] controller pod %s must be Running (got %s)",
						pod.Metadata.Name, pod.Status.Phase)
					for _, cs := range pod.Status.ContainerStatuses {
						g.Expect(cs.RestartCount).To(BeZero(),
							"[TC-E10.68] controller pod %s container %s must have 0 restarts",
							pod.Metadata.Name, cs.Name)
					}
				}
			}

			Eventually(verifyControllerPodRunning).
				WithContext(ctx).
				WithTimeout(90*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(),
					"[TC-E10.68] controller pod must reach Running state within 90s")
		})

		// ── TC-E10.69 ─────────────────────────────────────────────────────────
		// E10.69: Manager metrics service accessible.
		// TestE2E/매니저_메트릭스_서비스_접근_가능
		It("[TC-E10.69] Manager metrics service 접근 가능 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			// Verify the controller pods are running first.
			verifyMetricsAccessible := func(g Gomega) {
				// Check that the controller pod has a running metrics port (8080).
				out, err := e10KubectlOutput(ctx,
					"get", "pods",
					"-n", e10Namespace,
					"-l", e10ControllerLabel,
					"-o", "jsonpath={.items[0].status.phase}",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E10.69] kubectl get controller pod phase must succeed")
				g.Expect(out).To(Equal("Running"),
					"[TC-E10.69] controller pod must be Running before metrics check")

				// Verify the metrics port is declared in the controller container spec.
				containerPorts, err := e10KubectlOutput(ctx,
					"get", "pods",
					"-n", e10Namespace,
					"-l", e10ControllerLabel,
					"-o", "jsonpath={.items[0].spec.containers[0].ports[*].containerPort}",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E10.69] kubectl get container ports must succeed")
				// The controller pod exposes metrics on port 8080.
				g.Expect(containerPorts).To(ContainSubstring("8080"),
					"[TC-E10.69] controller pod must expose metrics on port 8080, got ports: %s",
					containerPorts)
			}

			Eventually(verifyMetricsAccessible).
				WithContext(ctx).
				WithTimeout(90*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(),
					"[TC-E10.69] metrics port must be accessible within 90s")
		})

		// ── TC-E10.70 ─────────────────────────────────────────────────────────
		// E10.70: cert-manager integration.
		// TestE2E/cert-manager_통합
		It("[TC-E10.70] cert-manager 통합 검증", Label("optional:cert-manager"), func() {
			// cert-manager is required for this test. If CERT_MANAGER_INSTALL_SKIP=true
			// is set, this test will fail — either install cert-manager or exclude it
			// from the run via --label-filter=!optional:cert-manager.
			if os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true" {
				Fail("[TC-E10.70] cert-manager integration cannot run: CERT_MANAGER_INSTALL_SKIP=true is set. " +
					"To run this test, unset CERT_MANAGER_INSTALL_SKIP or install cert-manager. " +
					"To exclude it from the suite, add --label-filter=!optional:cert-manager.")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			verifyCertManager := func(g Gomega) {
				// Check cert-manager namespace exists.
				out, err := e10KubectlOutput(ctx,
					"get", "namespace", "cert-manager",
					"-o", "jsonpath={.status.phase}",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E10.70] cert-manager namespace must exist")
				g.Expect(out).To(Equal("Active"),
					"[TC-E10.70] cert-manager namespace must be Active")

				// Check cert-manager pods are running.
				podOut, podErr := e10KubectlOutput(ctx,
					"get", "pods",
					"-n", "cert-manager",
					"-l", "app.kubernetes.io/instance=cert-manager",
					"-o", "jsonpath={.items[*].status.phase}",
				)
				g.Expect(podErr).NotTo(HaveOccurred(),
					"[TC-E10.70] cert-manager pods must be accessible")
				g.Expect(podOut).To(ContainSubstring("Running"),
					"[TC-E10.70] cert-manager pods must be Running, got: %s", podOut)
			}

			Eventually(verifyCertManager).
				WithContext(ctx).
				WithTimeout(2*time.Minute).
				WithPolling(10*time.Second).
				Should(Succeed(),
					"[TC-E10.70] cert-manager must be running within 2m")
		})
	})
})
