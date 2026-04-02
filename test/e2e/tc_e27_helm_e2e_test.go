//go:build e2e

package e2e

// tc_e27_helm_e2e_test.go — Type B Kind-cluster E2E tests for E27: Helm chart
// installation and release validation (TC-E27.207 through TC-E27.243).
//
// These specs require a running Kind cluster and are NOT labeled "default-profile"
// so they do not run in the standard 437-case in-process suite.  They run when:
//
//	go test -tags=e2e ./test/e2e/ -run TestE2E -- --label-filter=helm
//
// The suite installs pillar-csi via Helm in BeforeAll and uninstalls in AfterAll.
// Tests that require separate isolated installs (custom values, installCRDs=false,
// duplicate-install error, CSIDriver overrides) use unique release names in unique
// namespaces and are nested in separate Ordered Describe blocks with their own
// BeforeAll/AfterAll.
//
// TC IDs covered:
//
//	E27.207  — Helm basic install success
//	E27.208  — Helm release status text
//	E27.209  — Helm release status JSON
//	E27.210  — Helm release list text
//	E27.211  — Helm release list JSON
//	E27.212  — Controller Deployment running
//	E27.213  — Node DaemonSet running
//	E27.214  — Agent DaemonSet desiredNumberScheduled==0 (no storage labels)
//	E27.215  — ServiceAccount 3 types exist
//	E27.216  — CSIDriver registered
//	E27.217  — CRD 4 types bulk Established check
//	E27.217a–d — Individual CRD Established status
//	E27.217e–h — CRD metadata (group, version, scope, shortName)
//	E27.217i–m — kubectl api-resources shortNames
//	E27.217n–q — CRD OpenAPI v3 schema existence
//	E27.217r–u — CRD additionalPrinterColumns
//	E27.217v   — CRD resource-policy:keep annotation
//	E27.217w–z — CRD CRUD (create/get/delete sample objects)
//	E27.218  — Custom values override (replicaCount=2)
//	E27.219  — installCRDs=false mode
//	E27.220  — Duplicate install error
//	E27.221  — Helm upgrade success
//	E27.222  — Helm upgrade history
//	E27.223  — Helm uninstall success
//	E27.224  — CRD preserved after uninstall (resource-policy:keep)
//	E27.225  — All pods Running summary
//	E27.226  — Controller pod 5-container Ready check
//	E27.227  — Node pod 3-container + init-container check
//	E27.228  — Agent DaemonSet 0 pods (no storage labels)
//	E27.229  — Agent pod Running after storage label applied
//	E27.230  — Pod Ready condition within 5 minutes
//	E27.231  — No pod restarts in 5-minute observation (opt-in via E2E_STABILITY_CHECKS=true)
//	E27.232  — CSIDriver exists
//	E27.233  — CSIDriver JSON parseable
//	E27.234  — CSIDriver.spec.attachRequired==true
//	E27.235  — CSIDriver.spec.podInfoOnMount==true
//	E27.236  — CSIDriver.spec.fsGroupPolicy=="File"
//	E27.236a — CSIDriver.spec.fsGroupPolicy valid enum
//	E27.237  — CSIDriver.spec.volumeLifecycleModes contains Persistent
//	E27.237a — CSIDriver.spec.volumeLifecycleModes excludes Ephemeral
//	E27.238  — CSIDriver Helm labels (app.kubernetes.io/*)
//	E27.238a — CSIDriver managed-by: Helm label
//	E27.239  — CSIDriver meta.helm.sh annotations
//	E27.240  — CSIDriver.create=false — no CSIDriver created
//	E27.241  — CSIDriver.podInfoOnMount=false override
//	E27.242  — CSIDriver.fsGroupPolicy=None override
//	E27.243  — Helm upgrade changes CSIDriver spec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	e27Namespace = "pillar-csi-system"
	e27Release   = "pillar-csi"
	// csiDriverName is the expected CSI driver provisioner name.
	csiDriverName = "pillar-csi.bhyoo.com"
	// crdGroup is the CRD API group prefix used in CRD FQDNs.
	crdGroup = "pillar-csi.pillar-csi.bhyoo.com"
)

// e27KubectlOutput runs kubectl with the suite kubeconfig and returns trimmed stdout.
func e27KubectlOutput(ctx context.Context, args ...string) (string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", fmt.Errorf("[E27] KUBECONFIG not set — Kind cluster not bootstrapped")
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

// e27HelmOutput runs helm with the suite kubeconfig and returns (stdout, stderr, error).
func e27HelmOutput(ctx context.Context, args ...string) (string, string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", "", fmt.Errorf("[E27] KUBECONFIG not set — Kind cluster not bootstrapped")
	}
	cmdArgs := append([]string{"--kubeconfig=" + kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "helm", cmdArgs...) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// e27ChartPath returns the absolute path to the charts/pillar-csi directory.
// It is discovered at runtime relative to the module root.
func e27ChartPath() string {
	// Walk up from the test directory to the module root.
	testDir, err := filepath.Abs(filepath.Join(".", "..", ".."))
	if err != nil {
		return "./charts/pillar-csi"
	}
	return filepath.Join(testDir, "charts", "pillar-csi")
}

// e27SamplesDir returns the absolute path to config/samples/.
func e27SamplesDir() string {
	testDir, err := filepath.Abs(filepath.Join(".", "..", ".."))
	if err != nil {
		return "./config/samples"
	}
	return filepath.Join(testDir, "config", "samples")
}

// e27HelmInstall installs pillar-csi and waits up to 5 minutes.
func e27HelmInstall(ctx context.Context, release, namespace, chartPath string, extraArgs ...string) error {
	args := []string{
		"install", release, chartPath,
		"--namespace", namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "5m",
	}
	args = append(args, extraArgs...)
	_, stderr, err := e27HelmOutput(ctx, args...)
	if err != nil {
		return fmt.Errorf("helm install %s: %w\nstderr: %s", release, err, stderr)
	}
	return nil
}

// e27HelmUninstall uninstalls a Helm release; errors are ignored (best-effort cleanup).
func e27HelmUninstall(ctx context.Context, release, namespace string) {
	ctx2, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, _, _ = e27HelmOutput(ctx2, "uninstall", release, "--namespace", namespace, "--wait")
}

// e27KubectlDeleteNamespace deletes a namespace; error is ignored (best-effort).
func e27KubectlDeleteNamespace(ctx context.Context, namespace string) {
	ctx2, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	_, _ = e27KubectlOutput(ctx2, "delete", "namespace", namespace, "--ignore-not-found")
}

// ─────────────────────────────────────────────────────────────────────────────
// Main install lifecycle — E27.1 through E27.12 (207–243)
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E27: Helm 차트 설치 및 릴리스 검증", Label("helm", "E27-cluster"), Ordered, func() {

	var (
		installCtx    context.Context
		installCancel context.CancelFunc
		chartPath     string
	)

	BeforeAll(func() {
		installCtx, installCancel = context.WithTimeout(context.Background(), 10*time.Minute)
		DeferCleanup(installCancel)
		chartPath = e27ChartPath()
		GinkgoWriter.Printf("[E27] chart path: %s\n", chartPath)
	})

	// ── TC-E27.207 ───────────────────────────────────────────────────────────
	// E27.1 Helm 차트 기본값 설치 성공
	It("[TC-E27.207] Helm 차트 기본값 설치 성공", func() {
		stdout, _, err := e27HelmOutput(installCtx,
			"install", e27Release, chartPath,
			"--namespace", e27Namespace,
			"--create-namespace",
			"--wait",
			"--timeout", "5m",
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E27.207] helm install must succeed")
		Expect(stdout).To(ContainSubstring("STATUS: deployed"),
			"[TC-E27.207] stdout must contain STATUS: deployed")
		Expect(stdout).To(ContainSubstring("REVISION: 1"),
			"[TC-E27.207] stdout must contain REVISION: 1")
	})

	AfterAll(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		// Best-effort cleanup — do not fail the suite if already uninstalled.
		e27HelmUninstall(cleanCtx, e27Release, e27Namespace)
	})

	// ── TC-E27.208 / TC-E27.209 ──────────────────────────────────────────────
	// E27.2 Helm 릴리스 상태 검증
	Describe("E27.2 Helm 릴리스 상태 검증", func() {

		It("[TC-E27.208] Helm 릴리스 상태 텍스트 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"status", e27Release,
				"--namespace", e27Namespace,
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.208] helm status must succeed")
			Expect(stdout).To(ContainSubstring("NAME: "+e27Release),
				"[TC-E27.208] status must contain NAME: pillar-csi")
			Expect(stdout).To(ContainSubstring("NAMESPACE: "+e27Namespace),
				"[TC-E27.208] status must contain NAMESPACE: pillar-csi-system")
			Expect(stdout).To(ContainSubstring("STATUS: deployed"),
				"[TC-E27.208] status must contain STATUS: deployed")
			Expect(stdout).To(ContainSubstring("REVISION: 1"),
				"[TC-E27.208] status must contain REVISION: 1")
		})

		It("[TC-E27.209] Helm 릴리스 상태 JSON 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"status", e27Release,
				"--namespace", e27Namespace,
				"--output", "json",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.209] helm status --output json must succeed")

			var helmStatus struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				Version   int    `json:"version"`
				Info      struct {
					Status string `json:"status"`
				} `json:"info"`
			}
			Expect(json.Unmarshal([]byte(stdout), &helmStatus)).To(Succeed(),
				"[TC-E27.209] helm status JSON must be parseable")
			Expect(helmStatus.Info.Status).To(Equal("deployed"),
				"[TC-E27.209] .info.status must be deployed")
			Expect(helmStatus.Name).To(Equal(e27Release),
				"[TC-E27.209] .name must be pillar-csi")
			Expect(helmStatus.Namespace).To(Equal(e27Namespace),
				"[TC-E27.209] .namespace must be pillar-csi-system")
			Expect(helmStatus.Version).To(Equal(1),
				"[TC-E27.209] .version must be 1")
		})
	})

	// ── TC-E27.210 / TC-E27.211 ──────────────────────────────────────────────
	// E27.3 Helm 릴리스 목록 검증
	Describe("E27.3 Helm 릴리스 목록 검증", func() {

		It("[TC-E27.210] Helm 릴리스 목록 텍스트 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"list",
				"--namespace", e27Namespace,
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.210] helm list must succeed")
			Expect(stdout).To(ContainSubstring(e27Release),
				"[TC-E27.210] helm list must contain pillar-csi")
			Expect(stdout).To(ContainSubstring("deployed"),
				"[TC-E27.210] helm list STATUS must be deployed")
		})

		It("[TC-E27.211] Helm 릴리스 목록 JSON 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"list",
				"--namespace", e27Namespace,
				"--output", "json",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.211] helm list --output json must succeed")

			var releases []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Chart  string `json:"chart"`
			}
			Expect(json.Unmarshal([]byte(stdout), &releases)).To(Succeed(),
				"[TC-E27.211] helm list JSON must be parseable")
			Expect(releases).NotTo(BeEmpty(),
				"[TC-E27.211] helm list must contain at least one release")
			Expect(releases[0].Name).To(Equal(e27Release),
				"[TC-E27.211] first release .name must be pillar-csi")
			Expect(releases[0].Status).To(Equal("deployed"),
				"[TC-E27.211] first release .status must be deployed")
			Expect(releases[0].Chart).To(ContainSubstring("pillar-csi"),
				"[TC-E27.211] first release .chart must contain pillar-csi")
		})
	})

	// ── TC-E27.212 through TC-E27.216 ────────────────────────────────────────
	// E27.4 배포된 Kubernetes 리소스 정상 동작 검증
	Describe("E27.4 배포된 Kubernetes 리소스 검증", func() {

		It("[TC-E27.212] 컨트롤러 Deployment Running 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyControllerDeployment := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "deployment",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=controller",
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.212] kubectl get deployment must succeed")

				var depList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							AvailableReplicas int `json:"availableReplicas"`
						} `json:"status"`
					} `json:"items"`
				}
				g.Expect(json.Unmarshal([]byte(out), &depList)).To(Succeed())
				g.Expect(depList.Items).NotTo(BeEmpty(),
					"[TC-E27.212] controller Deployment must exist")
				g.Expect(depList.Items[0].Status.AvailableReplicas).To(BeNumerically(">=", 1),
					"[TC-E27.212] controller availableReplicas must be ≥1")
			}

			Eventually(verifyControllerDeployment).
				WithContext(ctx).
				WithTimeout(90*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(),
					"[TC-E27.212] controller Deployment must be Available within 90s")
		})

		It("[TC-E27.213] 노드 DaemonSet 배포 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyNodeDaemonSet := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "daemonset",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=node",
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.213] kubectl get daemonset must succeed")

				var dsList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							DesiredNumberScheduled int `json:"desiredNumberScheduled"`
							NumberReady            int `json:"numberReady"`
						} `json:"status"`
					} `json:"items"`
				}
				g.Expect(json.Unmarshal([]byte(out), &dsList)).To(Succeed())
				g.Expect(dsList.Items).NotTo(BeEmpty(),
					"[TC-E27.213] node DaemonSet must exist")
				ds := dsList.Items[0]
				g.Expect(ds.Status.NumberReady).To(Equal(ds.Status.DesiredNumberScheduled),
					"[TC-E27.213] node DaemonSet numberReady must equal desiredNumberScheduled")
			}

			Eventually(verifyNodeDaemonSet).
				WithContext(ctx).
				WithTimeout(90*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(),
					"[TC-E27.213] node DaemonSet must be ready within 90s")
		})

		It("[TC-E27.214] 에이전트 DaemonSet 스토리지 레이블 없는 환경 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "daemonset",
				"-n", e27Namespace,
				"-l", "app.kubernetes.io/component=agent",
				"-o", "jsonpath={.items[0].status.desiredNumberScheduled}",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.214] kubectl get agent daemonset must succeed")
			Expect(out).To(Equal("0"),
				"[TC-E27.214] agent desiredNumberScheduled must be 0 (no storage labels)")
		})

		It("[TC-E27.215] ServiceAccount 3종 존재 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "serviceaccount",
				"-n", e27Namespace,
				"-o", "name",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.215] kubectl get serviceaccounts must succeed")
			// Helm uses the fullname helper, which produces "<release>-<component>".
			// Accept names that contain the component keywords.
			Expect(out).To(ContainSubstring("controller"),
				"[TC-E27.215] controller ServiceAccount must exist")
			Expect(out).To(ContainSubstring("node"),
				"[TC-E27.215] node ServiceAccount must exist")
			Expect(out).To(ContainSubstring("agent"),
				"[TC-E27.215] agent ServiceAccount must exist")
		})

		It("[TC-E27.216] CSIDriver 등록 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "json",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.216] CSIDriver must be registered")

			var csidrv struct {
				Spec struct {
					AttachRequired       bool     `json:"attachRequired"`
					PodInfoOnMount       bool     `json:"podInfoOnMount"`
					FsGroupPolicy        string   `json:"fsGroupPolicy"`
					VolumeLifecycleModes []string `json:"volumeLifecycleModes"`
				} `json:"spec"`
			}
			Expect(json.Unmarshal([]byte(out), &csidrv)).To(Succeed())
			Expect(csidrv.Spec.AttachRequired).To(BeTrue(),
				"[TC-E27.216] attachRequired must be true")
			Expect(csidrv.Spec.PodInfoOnMount).To(BeTrue(),
				"[TC-E27.216] podInfoOnMount must be true")
			Expect(csidrv.Spec.FsGroupPolicy).To(Equal("File"),
				"[TC-E27.216] fsGroupPolicy must be File")
			Expect(csidrv.Spec.VolumeLifecycleModes).To(ContainElement("Persistent"),
				"[TC-E27.216] volumeLifecycleModes must contain Persistent")
		})
	})

	// ── TC-E27.217 through TC-E27.217z ───────────────────────────────────────
	// E27.5 CRD 등록 및 가용성 검증
	Describe("E27.5 CRD 등록 및 가용성 검증", func() {

		crdNames := []string{
			"pillartargets." + crdGroup,
			"pillarpools." + crdGroup,
			"pillarprotocols." + crdGroup,
			"pillarbindings." + crdGroup,
		}

		// E27.5.1 — CRD 4종 일괄
		It("[TC-E27.217] CRD 4종 설치 및 Established 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			verifyCRDs := func(g Gomega) {
				for _, crd := range crdNames {
					out, err := e27KubectlOutput(ctx,
						"get", "crd", crd,
						"-o", "jsonpath={.status.conditions[?(@.type==\"Established\")].status}",
					)
					g.Expect(err).NotTo(HaveOccurred(),
						"[TC-E27.217] CRD %s must exist", crd)
					g.Expect(out).To(Equal("True"),
						"[TC-E27.217] CRD %s must be Established=True", crd)
				}
			}

			Eventually(verifyCRDs).
				WithContext(ctx).
				WithTimeout(50*time.Second).
				WithPolling(5*time.Second).
				Should(Succeed(),
					"[TC-E27.217] all CRDs must be Established within 50s")
		})

		It("[TC-E27.217a] CRD Established PillarTarget", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx,
				"get", "crd", "pillartargets."+crdGroup,
				"-o", "jsonpath={.status.conditions[?(@.type==\"Established\")].status}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.217a] PillarTarget CRD must exist")
			Expect(out).To(Equal("True"), "[TC-E27.217a] PillarTarget CRD must be Established")
		})

		It("[TC-E27.217b] CRD Established PillarPool", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx,
				"get", "crd", "pillarpools."+crdGroup,
				"-o", "jsonpath={.status.conditions[?(@.type==\"Established\")].status}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.217b] PillarPool CRD must exist")
			Expect(out).To(Equal("True"), "[TC-E27.217b] PillarPool CRD must be Established")
		})

		It("[TC-E27.217c] CRD Established PillarProtocol", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx,
				"get", "crd", "pillarprotocols."+crdGroup,
				"-o", "jsonpath={.status.conditions[?(@.type==\"Established\")].status}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.217c] PillarProtocol CRD must exist")
			Expect(out).To(Equal("True"), "[TC-E27.217c] PillarProtocol CRD must be Established")
		})

		It("[TC-E27.217d] CRD Established PillarBinding", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx,
				"get", "crd", "pillarbindings."+crdGroup,
				"-o", "jsonpath={.status.conditions[?(@.type==\"Established\")].status}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.217d] PillarBinding CRD must exist")
			Expect(out).To(Equal("True"), "[TC-E27.217d] PillarBinding CRD must be Established")
		})

		// E27.5.2 — CRD metadata
		type crdMeta struct {
			tcID     string
			fqdn     string
			kind     string
			plural   string
			singular string
			short    string
			scope    string
		}
		crdMetas := []crdMeta{
			{"E27.217e", "pillartargets." + crdGroup, "PillarTarget", "pillartargets", "pillartarget", "pt", "Cluster"},
			{"E27.217f", "pillarpools." + crdGroup, "PillarPool", "pillarpools", "pillarpool", "pp", "Cluster"},
			{"E27.217g", "pillarprotocols." + crdGroup, "PillarProtocol", "pillarprotocols", "pillarprotocol", "ppr", "Cluster"},
			{"E27.217h", "pillarbindings." + crdGroup, "PillarBinding", "pillarbindings", "pillarbinding", "pb", "Cluster"},
		}

		for _, cm := range crdMetas {
			cm := cm // capture
			It(fmt.Sprintf("[TC-%s] CRD Metadata %s", cm.tcID, cm.kind), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				out, err := e27KubectlOutput(ctx, "get", "crd", cm.fqdn, "-o", "json")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-%s] CRD %s must exist", cm.tcID, cm.fqdn)

				var crd struct {
					Spec struct {
						Group string `json:"group"`
						Scope string `json:"scope"`
						Names struct {
							Kind       string   `json:"kind"`
							Plural     string   `json:"plural"`
							Singular   string   `json:"singular"`
							ShortNames []string `json:"shortNames"`
						} `json:"names"`
						Versions []struct {
							Name string `json:"name"`
						} `json:"versions"`
					} `json:"spec"`
				}
				Expect(json.Unmarshal([]byte(out), &crd)).To(Succeed())
				Expect(crd.Spec.Group).To(Equal(crdGroup),
					"[TC-%s] .spec.group must be %s", cm.tcID, crdGroup)
				Expect(crd.Spec.Names.Kind).To(Equal(cm.kind),
					"[TC-%s] .spec.names.kind must be %s", cm.tcID, cm.kind)
				Expect(crd.Spec.Names.Plural).To(Equal(cm.plural),
					"[TC-%s] .spec.names.plural must be %s", cm.tcID, cm.plural)
				Expect(crd.Spec.Names.Singular).To(Equal(cm.singular),
					"[TC-%s] .spec.names.singular must be %s", cm.tcID, cm.singular)
				Expect(crd.Spec.Names.ShortNames).To(ContainElement(cm.short),
					"[TC-%s] .spec.names.shortNames must contain %s", cm.tcID, cm.short)
				Expect(crd.Spec.Scope).To(Equal(cm.scope),
					"[TC-%s] .spec.scope must be %s", cm.tcID, cm.scope)
				Expect(crd.Spec.Versions).NotTo(BeEmpty())
				Expect(crd.Spec.Versions[0].Name).To(Equal("v1alpha1"),
					"[TC-%s] first version must be v1alpha1", cm.tcID)
			})
		}

		// E27.5.3 — kubectl api-resources
		It("[TC-E27.217i] API Resources 그룹 등록 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"api-resources",
				"--api-group="+crdGroup,
				"-o", "wide",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.217i] kubectl api-resources must succeed")
			for _, name := range []string{"pillartargets", "pillarpools", "pillarprotocols", "pillarbindings"} {
				Expect(out).To(ContainSubstring(name),
					"[TC-E27.217i] api-resources must list %s", name)
			}
			Expect(out).To(ContainSubstring("false"),
				"[TC-E27.217i] resources must be cluster-scoped (NAMESPACED=false)")
		})

		shortNameCases := []struct {
			tcID  string
			short string
			kind  string
		}{
			{"E27.217j", "pt", "PillarTarget"},
			{"E27.217k", "pp", "PillarPool"},
			{"E27.217l", "ppr", "PillarProtocol"},
			{"E27.217m", "pb", "PillarBinding"},
		}
		for _, sc := range shortNameCases {
			sc := sc
			It(fmt.Sprintf("[TC-%s] API Resources shortName %s 검증", sc.tcID, sc.short), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				out, err := e27KubectlOutput(ctx, "get", sc.short)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-%s] kubectl get %s must succeed (empty list is OK)", sc.tcID, sc.short)
				// Empty list or header-only is fine; we just need no "unknown resource type" error.
				_ = out
			})
		}

		// E27.5.4 — CRD OpenAPI v3 schema
		schemaTests := []struct {
			tcID   string
			fqdn   string
			kind   string
			fields []string
		}{
			{"E27.217n", "pillartargets." + crdGroup, "PillarTarget", []string{"external", "nodeRef"}},
			{"E27.217o", "pillarpools." + crdGroup, "PillarPool", []string{"backend", "targetRef"}},
			{"E27.217p", "pillarprotocols." + crdGroup, "PillarProtocol", []string{"type"}},
			{"E27.217q", "pillarbindings." + crdGroup, "PillarBinding", []string{"poolRef", "protocolRef"}},
		}

		for _, st := range schemaTests {
			st := st
			It(fmt.Sprintf("[TC-%s] CRD Schema %s", st.tcID, st.kind), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				out, err := e27KubectlOutput(ctx, "get", "crd", st.fqdn, "-o", "json")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-%s] CRD %s must exist", st.tcID, st.fqdn)

				var crd struct {
					Spec struct {
						Versions []struct {
							Name    string `json:"name"`
							Served  bool   `json:"served"`
							Storage bool   `json:"storage"`
							Schema  struct {
								OpenAPIV3Schema *json.RawMessage `json:"openAPIV3Schema"`
							} `json:"schema"`
						} `json:"versions"`
					} `json:"spec"`
				}
				Expect(json.Unmarshal([]byte(out), &crd)).To(Succeed())
				Expect(crd.Spec.Versions).NotTo(BeEmpty())
				v := crd.Spec.Versions[0]
				Expect(v.Served).To(BeTrue(),
					"[TC-%s] first version must be served", st.tcID)
				Expect(v.Storage).To(BeTrue(),
					"[TC-%s] first version must be storage", st.tcID)
				Expect(v.Schema.OpenAPIV3Schema).NotTo(BeNil(),
					"[TC-%s] openAPIV3Schema must exist", st.tcID)

				// Check that spec-level fields exist in the raw JSON.
				schemaStr := string(*v.Schema.OpenAPIV3Schema)
				for _, field := range st.fields {
					Expect(schemaStr).To(ContainSubstring(`"`+field+`"`),
						"[TC-%s] openAPIV3Schema must mention field %s", st.tcID, field)
				}
			})
		}

		// E27.5.5 — CRD additionalPrinterColumns
		printerColTests := []struct {
			tcID    string
			fqdn    string
			kind    string
			columns []string
		}{
			{"E27.217r", "pillartargets." + crdGroup, "PillarTarget", []string{"Address", "Agent", "Ready", "Age"}},
			{"E27.217s", "pillarpools." + crdGroup, "PillarPool", []string{"Target", "Backend", "Available", "Ready"}},
			{"E27.217t", "pillarprotocols." + crdGroup, "PillarProtocol", []string{"Type", "Bindings", "Ready"}},
			{"E27.217u", "pillarbindings." + crdGroup, "PillarBinding", []string{"Pool", "Protocol", "StorageClass", "Ready"}},
		}

		for _, pt := range printerColTests {
			pt := pt
			It(fmt.Sprintf("[TC-%s] CRD PrinterColumns %s", pt.tcID, pt.kind), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				out, err := e27KubectlOutput(ctx, "get", "crd", pt.fqdn, "-o", "json")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-%s] CRD %s must exist", pt.tcID, pt.fqdn)

				var crd struct {
					Spec struct {
						Versions []struct {
							AdditionalPrinterColumns []struct {
								Name string `json:"name"`
							} `json:"additionalPrinterColumns"`
						} `json:"versions"`
					} `json:"spec"`
				}
				Expect(json.Unmarshal([]byte(out), &crd)).To(Succeed())
				Expect(crd.Spec.Versions).NotTo(BeEmpty())

				var colNames []string
				for _, col := range crd.Spec.Versions[0].AdditionalPrinterColumns {
					colNames = append(colNames, col.Name)
				}
				for _, expected := range pt.columns {
					Expect(colNames).To(ContainElement(expected),
						"[TC-%s] printerColumns must contain %s", pt.tcID, expected)
				}
			})
		}

		// E27.5.6 — resource-policy: keep
		It("[TC-E27.217v] CRD resource-policy:keep 어노테이션 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			for _, crd := range crdNames {
				out, err := e27KubectlOutput(ctx,
					"get", "crd", crd,
					"-o", `jsonpath={.metadata.annotations.helm\.sh/resource-policy}`,
				)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217v] CRD %s must be accessible", crd)
				Expect(out).To(Equal("keep"),
					"[TC-E27.217v] CRD %s must have helm.sh/resource-policy: keep", crd)
			}
		})

		// E27.5.7 — CRD CRUD
		Describe("E27.5.7 CRD CRUD 기본 동작", Ordered, func() {
			samplesDir := e27SamplesDir()

			It("[TC-E27.217w] CRD CRUD PillarTarget 생성조회삭제", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				sampleFile := filepath.Join(samplesDir, "pillar-csi_v1alpha1_pillartarget.yaml")
				_, err := e27KubectlOutput(ctx, "apply", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217w] kubectl apply pillartarget must succeed")
				// Ensure cleanup even if assertions below fail.
				DeferCleanup(func() {
					cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cleanCancel()
					_, _ = e27KubectlOutput(cleanCtx, "delete", "-f", sampleFile, "--ignore-not-found=true")
				})

				out, err := e27KubectlOutput(ctx, "get", "pt")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217w] kubectl get pt must succeed")
				Expect(out).NotTo(BeEmpty(),
					"[TC-E27.217w] kubectl get pt must return at least one row")

				_, err = e27KubectlOutput(ctx, "delete", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217w] kubectl delete pillartarget must succeed")
			})

			It("[TC-E27.217x] CRD CRUD PillarPool 생성조회삭제", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				sampleFile := filepath.Join(samplesDir, "pillar-csi_v1alpha1_pillarpool.yaml")
				_, err := e27KubectlOutput(ctx, "apply", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217x] kubectl apply pillarpool must succeed")
				// Ensure cleanup even if assertions below fail.
				DeferCleanup(func() {
					cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cleanCancel()
					_, _ = e27KubectlOutput(cleanCtx, "delete", "-f", sampleFile, "--ignore-not-found=true")
				})

				out, err := e27KubectlOutput(ctx, "get", "pp")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217x] kubectl get pp must succeed")
				Expect(out).NotTo(BeEmpty(),
					"[TC-E27.217x] kubectl get pp must return at least one row")

				_, err = e27KubectlOutput(ctx, "delete", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217x] kubectl delete pillarpool must succeed")
			})

			It("[TC-E27.217y] CRD CRUD PillarProtocol 생성조회삭제", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				sampleFile := filepath.Join(samplesDir, "pillar-csi_v1alpha1_pillarprotocol.yaml")
				_, err := e27KubectlOutput(ctx, "apply", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217y] kubectl apply pillarprotocol must succeed")
				// Ensure cleanup even if assertions below fail.
				DeferCleanup(func() {
					cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cleanCancel()
					_, _ = e27KubectlOutput(cleanCtx, "delete", "-f", sampleFile, "--ignore-not-found=true")
				})

				out, err := e27KubectlOutput(ctx, "get", "ppr")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217y] kubectl get ppr must succeed")
				Expect(out).NotTo(BeEmpty(),
					"[TC-E27.217y] kubectl get ppr must return at least one row")

				_, err = e27KubectlOutput(ctx, "delete", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217y] kubectl delete pillarprotocol must succeed")
			})

			It("[TC-E27.217z] CRD CRUD PillarBinding 생성조회삭제", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				sampleFile := filepath.Join(samplesDir, "pillar-csi_v1alpha1_pillarbinding.yaml")
				_, err := e27KubectlOutput(ctx, "apply", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217z] kubectl apply pillarbinding must succeed")
				// Ensure cleanup even if assertions below fail.
				DeferCleanup(func() {
					cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cleanCancel()
					_, _ = e27KubectlOutput(cleanCtx, "delete", "-f", sampleFile, "--ignore-not-found=true")
				})

				out, err := e27KubectlOutput(ctx, "get", "pb")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217z] kubectl get pb must succeed")
				Expect(out).NotTo(BeEmpty(),
					"[TC-E27.217z] kubectl get pb must return at least one row")

				_, err = e27KubectlOutput(ctx, "delete", "-f", sampleFile)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.217z] kubectl delete pillarbinding must succeed")
			})
		})
	})

	// ── TC-E27.225 through TC-E27.231 ────────────────────────────────────────
	// E27.11 전체 파드 Running 상태 종합 검증
	Describe("E27.11 파드 Running 상태 종합 검증", func() {

		It("[TC-E27.225] 전체 파드 Running 상태 종합 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyAllPodsRunning := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "pods",
					"-n", e27Namespace,
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.225] kubectl get pods must succeed")

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
							Conditions []struct {
								Type   string `json:"type"`
								Status string `json:"status"`
							} `json:"conditions"`
						} `json:"status"`
					} `json:"items"`
				}
				g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
				// We expect at least controller + node pods.
				g.Expect(podList.Items).NotTo(BeEmpty(),
					"[TC-E27.225] there must be at least one pod in pillar-csi-system")

				for _, pod := range podList.Items {
					g.Expect(pod.Status.Phase).To(Equal("Running"),
						"[TC-E27.225] pod %s must be Running", pod.Metadata.Name)
					for _, cs := range pod.Status.ContainerStatuses {
						g.Expect(cs.Ready).To(BeTrue(),
							"[TC-E27.225] pod %s container %s must be ready",
							pod.Metadata.Name, cs.Name)
						g.Expect(cs.RestartCount).To(BeZero(),
							"[TC-E27.225] pod %s container %s must have 0 restarts",
							pod.Metadata.Name, cs.Name)
					}
				}
			}

			Eventually(verifyAllPodsRunning).
				WithContext(ctx).
				WithTimeout(90 * time.Second).
				WithPolling(5 * time.Second).
				Should(Succeed())
		})

		It("[TC-E27.226] 컨트롤러 파드 컨테이너 Ready 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyControllerContainers := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "pods",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=controller",
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							ContainerStatuses []struct {
								Name         string `json:"name"`
								Ready        bool   `json:"ready"`
								RestartCount int    `json:"restartCount"`
							} `json:"containerStatuses"`
						} `json:"status"`
					} `json:"items"`
				}
				g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
				g.Expect(podList.Items).NotTo(BeEmpty(),
					"[TC-E27.226] controller pod must exist")

				pod := podList.Items[0]
				for _, cs := range pod.Status.ContainerStatuses {
					g.Expect(cs.Ready).To(BeTrue(),
						"[TC-E27.226] controller pod container %s must be ready", cs.Name)
					g.Expect(cs.RestartCount).To(BeZero(),
						"[TC-E27.226] controller pod container %s must have 0 restarts", cs.Name)
				}
			}

			Eventually(verifyControllerContainers).
				WithContext(ctx).
				WithTimeout(90 * time.Second).
				WithPolling(5 * time.Second).
				Should(Succeed())
		})

		It("[TC-E27.227] 노드 파드 컨테이너 및 init 컨테이너 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			verifyNodePodContainers := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "pods",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=node",
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred())

				var podList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							InitContainerStatuses []struct {
								Name  string `json:"name"`
								State struct {
									Terminated *struct {
										ExitCode int `json:"exitCode"`
									} `json:"terminated"`
								} `json:"state"`
							} `json:"initContainerStatuses"`
							ContainerStatuses []struct {
								Name         string `json:"name"`
								Ready        bool   `json:"ready"`
								RestartCount int    `json:"restartCount"`
							} `json:"containerStatuses"`
						} `json:"status"`
					} `json:"items"`
				}
				g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
				g.Expect(podList.Items).NotTo(BeEmpty(),
					"[TC-E27.227] node pod must exist")

				for _, pod := range podList.Items {
					// init containers must have terminated with exit code 0
					for _, ic := range pod.Status.InitContainerStatuses {
						g.Expect(ic.State.Terminated).NotTo(BeNil(),
							"[TC-E27.227] init container %s must be terminated", ic.Name)
						g.Expect(ic.State.Terminated.ExitCode).To(BeZero(),
							"[TC-E27.227] init container %s exitCode must be 0", ic.Name)
					}
					for _, cs := range pod.Status.ContainerStatuses {
						g.Expect(cs.Ready).To(BeTrue(),
							"[TC-E27.227] node pod %s container %s must be ready",
							pod.Metadata.Name, cs.Name)
						g.Expect(cs.RestartCount).To(BeZero(),
							"[TC-E27.227] node pod %s container %s must have 0 restarts",
							pod.Metadata.Name, cs.Name)
					}
				}
			}

			Eventually(verifyNodePodContainers).
				WithContext(ctx).
				WithTimeout(90 * time.Second).
				WithPolling(5 * time.Second).
				Should(Succeed())
		})

		It("[TC-E27.228] 에이전트 DaemonSet 스토리지 레이블 없는 환경에서 파드 0개 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "pods",
				"-n", e27Namespace,
				"-l", "app.kubernetes.io/component=agent",
				"-o", "json",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.228] kubectl get agent pods must succeed")

			var podList struct {
				Items []interface{} `json:"items"`
			}
			Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
			Expect(podList.Items).To(BeEmpty(),
				"[TC-E27.228] agent pods must be 0 when no storage label on nodes")
		})

		It("[TC-E27.229] 에이전트 파드 스토리지 레이블 노드에서 Running 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			// Get the first worker node name.
			nodesOut, err := e27KubectlOutput(ctx, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.229] kubectl get nodes must succeed")
			nodeName := strings.TrimSpace(nodesOut)
			Expect(nodeName).NotTo(BeEmpty(), "[TC-E27.229] must find at least one node")

			// Apply storage label.
			_, err = e27KubectlOutput(ctx,
				"label", "node", nodeName,
				"pillar-csi.bhyoo.com/storage-node=true",
				"--overwrite",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.229] labeling node must succeed")

			// Remove label on test cleanup.
			DeferCleanup(func() {
				cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				_, _ = e27KubectlOutput(cleanCtx,
					"label", "node", nodeName,
					"pillar-csi.bhyoo.com/storage-node-",
				)
			})

			// Wait for agent pod to appear and be Running.
			verifyAgentPodRunning := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "pods",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=agent",
					"-o", "json",
				)
				g.Expect(err).NotTo(HaveOccurred())

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
				g.Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
				g.Expect(podList.Items).NotTo(BeEmpty(),
					"[TC-E27.229] agent pod must be scheduled on labeled node")
				for _, pod := range podList.Items {
					g.Expect(pod.Status.Phase).To(Equal("Running"),
						"[TC-E27.229] agent pod %s must be Running", pod.Metadata.Name)
					for _, cs := range pod.Status.ContainerStatuses {
						g.Expect(cs.Ready).To(BeTrue(),
							"[TC-E27.229] agent pod container %s must be ready", cs.Name)
					}
				}
			}

			Eventually(verifyAgentPodRunning).
				WithContext(ctx).
				WithTimeout(3*time.Minute).
				WithPolling(10*time.Second).
				Should(Succeed(),
					"[TC-E27.229] agent pod must be Running within 3m after label applied")
		})

		It("[TC-E27.230] 파드 Ready Condition Timeout 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "pods",
				"-n", e27Namespace,
				"-o", "json",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.230] kubectl get pods must succeed")

			var podList struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Status struct {
						Conditions []struct {
							Type               string `json:"type"`
							Status             string `json:"status"`
							LastTransitionTime string `json:"lastTransitionTime"`
						} `json:"conditions"`
					} `json:"status"`
				} `json:"items"`
			}
			Expect(json.Unmarshal([]byte(out), &podList)).To(Succeed())
			Expect(podList.Items).NotTo(BeEmpty())

			for _, pod := range podList.Items {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == "Ready" {
						Expect(cond.Status).To(Equal("True"),
							"[TC-E27.230] pod %s Ready condition must be True", pod.Metadata.Name)
					}
				}
			}
		})

		It("[TC-E27.231] 파드 재시작 없음 5분 관찰 (opt-in)", func() {
			if os.Getenv("E2E_STABILITY_CHECKS") != "true" {
				Skip("[TC-E27.231] skipped — set E2E_STABILITY_CHECKS=true to enable 5-min stability check")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()

			// Record initial restart counts.
			getRestarts := func() map[string]int {
				out, err := e27KubectlOutput(ctx,
					"get", "pods",
					"-n", e27Namespace,
					"-o", "json",
				)
				if err != nil {
					return nil
				}
				var podList struct {
					Items []struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Status struct {
							ContainerStatuses []struct {
								Name         string `json:"name"`
								RestartCount int    `json:"restartCount"`
							} `json:"containerStatuses"`
						} `json:"status"`
					} `json:"items"`
				}
				_ = json.Unmarshal([]byte(out), &podList)
				counts := make(map[string]int)
				for _, pod := range podList.Items {
					for _, cs := range pod.Status.ContainerStatuses {
						key := pod.Metadata.Name + "/" + cs.Name
						counts[key] = cs.RestartCount
					}
				}
				return counts
			}

			initial := getRestarts()
			Expect(initial).NotTo(BeEmpty(), "[TC-E27.231] must record initial restart counts")

			// Wait 5 minutes.
			select {
			case <-time.After(5 * time.Minute):
			case <-ctx.Done():
				Fail("[TC-E27.231] context cancelled before 5-minute observation complete")
			}

			final := getRestarts()
			for key, initCount := range initial {
				finalCount := final[key]
				Expect(finalCount).To(Equal(initCount),
					"[TC-E27.231] restart count for %s must not change (was %d, now %d)",
					key, initCount, finalCount)
			}
		})
	})

	// ── TC-E27.232 through TC-E27.239 ────────────────────────────────────────
	// E27.12 CSIDriver 객체 생성 및 설정 검증
	Describe("E27.12 CSIDriver 객체 검증", func() {

		It("[TC-E27.232] CSIDriver 존재 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx, "get", "csidriver", csiDriverName, "-o", "json")
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.232] CSIDriver must exist")

			var drv struct {
				Kind       string `json:"kind"`
				APIVersion string `json:"apiVersion"`
				Metadata   struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			Expect(json.Unmarshal([]byte(out), &drv)).To(Succeed())
			Expect(drv.Metadata.Name).To(Equal(csiDriverName),
				"[TC-E27.232] CSIDriver .metadata.name must be %s", csiDriverName)
			Expect(drv.Kind).To(Equal("CSIDriver"),
				"[TC-E27.232] .kind must be CSIDriver")
			Expect(drv.APIVersion).To(Equal("storage.k8s.io/v1"),
				"[TC-E27.232] .apiVersion must be storage.k8s.io/v1")
		})

		It("[TC-E27.233] CSIDriver JSON 파싱 가능", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx, "get", "csidriver", csiDriverName, "-o", "json")
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.233] CSIDriver must be gettable")

			var obj map[string]interface{}
			Expect(json.Unmarshal([]byte(out), &obj)).To(Succeed(),
				"[TC-E27.233] CSIDriver JSON must be parseable")
			Expect(obj).To(HaveKey("kind"), "[TC-E27.233] must have .kind")
			Expect(obj).To(HaveKey("apiVersion"), "[TC-E27.233] must have .apiVersion")
			Expect(obj).To(HaveKey("metadata"), "[TC-E27.233] must have .metadata")
			Expect(obj).To(HaveKey("spec"), "[TC-E27.233] must have .spec")
		})

		It("[TC-E27.234] CSIDriver.spec.attachRequired==true", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.attachRequired}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("true"),
				"[TC-E27.234] attachRequired must be true")
		})

		It("[TC-E27.235] CSIDriver.spec.podInfoOnMount==true", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.podInfoOnMount}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("true"),
				"[TC-E27.235] podInfoOnMount must be true")
		})

		It("[TC-E27.236] CSIDriver.spec.fsGroupPolicy==\"File\"", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.fsGroupPolicy}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("File"),
				"[TC-E27.236] fsGroupPolicy must be File")
		})

		It("[TC-E27.236a] CSIDriver.spec.fsGroupPolicy 유효값 범위 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.fsGroupPolicy}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect([]string{"None", "File", "ReadWriteOnceWithFSType"}).To(ContainElement(out),
				"[TC-E27.236a] fsGroupPolicy must be a valid enum value, got: %s", out)
		})

		It("[TC-E27.237] CSIDriver.spec.volumeLifecycleModes contains Persistent", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "json",
			)
			Expect(err).NotTo(HaveOccurred())

			var drv struct {
				Spec struct {
					VolumeLifecycleModes []string `json:"volumeLifecycleModes"`
				} `json:"spec"`
			}
			Expect(json.Unmarshal([]byte(out), &drv)).To(Succeed())
			Expect(drv.Spec.VolumeLifecycleModes).To(ContainElement("Persistent"),
				"[TC-E27.237] volumeLifecycleModes must contain Persistent")
		})

		It("[TC-E27.237a] CSIDriver.spec.volumeLifecycleModes excludes Ephemeral", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "json",
			)
			Expect(err).NotTo(HaveOccurred())

			var drv struct {
				Spec struct {
					VolumeLifecycleModes []string `json:"volumeLifecycleModes"`
				} `json:"spec"`
			}
			Expect(json.Unmarshal([]byte(out), &drv)).To(Succeed())
			Expect(drv.Spec.VolumeLifecycleModes).NotTo(ContainElement("Ephemeral"),
				"[TC-E27.237a] volumeLifecycleModes must NOT contain Ephemeral")
		})

		It("[TC-E27.238] CSIDriver Helm 레이블 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx, "get", "csidriver", csiDriverName, "-o", "json")
			Expect(err).NotTo(HaveOccurred())

			var drv struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			Expect(json.Unmarshal([]byte(out), &drv)).To(Succeed())
			labels := drv.Metadata.Labels
			Expect(labels).To(HaveKey("app.kubernetes.io/name"),
				"[TC-E27.238] must have app.kubernetes.io/name label")
			Expect(labels).To(HaveKey("app.kubernetes.io/instance"),
				"[TC-E27.238] must have app.kubernetes.io/instance label")
			Expect(labels["app.kubernetes.io/managed-by"]).To(Equal("Helm"),
				"[TC-E27.238] managed-by must be Helm")
			Expect(labels).To(HaveKey("helm.sh/chart"),
				"[TC-E27.238] must have helm.sh/chart label")
		})

		It("[TC-E27.238a] CSIDriver managed-by:Helm 레이블 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("Helm"),
				"[TC-E27.238a] managed-by must be Helm, got: %s", out)
		})

		It("[TC-E27.239] CSIDriver meta.helm.sh 어노테이션 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			out, err := e27KubectlOutput(ctx, "get", "csidriver", csiDriverName, "-o", "json")
			Expect(err).NotTo(HaveOccurred())

			var drv struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
			}
			Expect(json.Unmarshal([]byte(out), &drv)).To(Succeed())
			annots := drv.Metadata.Annotations
			Expect(annots["meta.helm.sh/release-name"]).To(Equal(e27Release),
				"[TC-E27.239] meta.helm.sh/release-name must be pillar-csi")
			Expect(annots["meta.helm.sh/release-namespace"]).To(Equal(e27Namespace),
				"[TC-E27.239] meta.helm.sh/release-namespace must be pillar-csi-system")
		})
	})

	// ── TC-E27.220 ───────────────────────────────────────────────────────────
	// E27.8 중복 설치 시도 오류 (must happen while the main release is installed)
	Describe("E27.8 중복 설치 시도 오류", func() {
		It("[TC-E27.220] 중복 설치 시도 오류 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, stderr, err := e27HelmOutput(ctx,
				"install", e27Release, chartPath,
				"--namespace", e27Namespace,
			)
			Expect(err).To(HaveOccurred(),
				"[TC-E27.220] duplicate helm install must fail")
			Expect(stderr).To(ContainSubstring("already exists"),
				`[TC-E27.220] stderr must mention "already exists", got: %s`, stderr)
		})
	})

	// ── TC-E27.221 / TC-E27.222 ──────────────────────────────────────────────
	// E27.9 Helm 업그레이드
	Describe("E27.9 Helm 차트 업그레이드", Ordered, func() {

		It("[TC-E27.221] Helm 차트 업그레이드 성공", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"upgrade", e27Release, chartPath,
				"--namespace", e27Namespace,
				"--wait",
				"--timeout", "5m",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.221] helm upgrade must succeed")
			Expect(stdout).To(ContainSubstring("STATUS: deployed"),
				"[TC-E27.221] status must be deployed")
			// After upgrade REVISION: 2 appears in helm status output.
			statusOut, _, _ := e27HelmOutput(ctx,
				"status", e27Release, "--namespace", e27Namespace,
			)
			Expect(statusOut).To(ContainSubstring("REVISION: 2"),
				"[TC-E27.221] REVISION must be 2 after upgrade")
		})

		It("[TC-E27.222] Helm 업그레이드 히스토리 검증", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"history", e27Release,
				"--namespace", e27Namespace,
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.222] helm history must succeed")
			Expect(stdout).To(ContainSubstring("superseded"),
				"[TC-E27.222] history must show superseded REVISION 1")
			Expect(stdout).To(ContainSubstring("deployed"),
				"[TC-E27.222] history must show deployed REVISION 2")
		})
	})

	// ── TC-E27.223 / TC-E27.224 ──────────────────────────────────────────────
	// E27.10 Helm 설치 해제 및 CRD 보존 (run last in the Ordered block)
	Describe("E27.10 Helm 설치 해제 및 리소스 정리", Ordered, func() {

		It("[TC-E27.223] Helm 설치 해제 성공", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			stdout, _, err := e27HelmOutput(ctx,
				"uninstall", e27Release,
				"--namespace", e27Namespace,
				"--wait",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.223] helm uninstall must succeed")
			Expect(stdout).To(ContainSubstring("uninstalled"),
				`[TC-E27.223] stdout must mention "uninstalled"`)

			// Verify Deployment is gone.
			verifyGone := func(g Gomega) {
				out, err := e27KubectlOutput(ctx,
					"get", "deployment",
					"-n", e27Namespace,
					"-l", "app.kubernetes.io/component=controller",
					"-o", "json",
				)
				if err != nil {
					// Namespace may be gone too — that's fine.
					return
				}
				var depList struct {
					Items []interface{} `json:"items"`
				}
				_ = json.Unmarshal([]byte(out), &depList)
				g.Expect(depList.Items).To(BeEmpty(),
					"[TC-E27.223] controller Deployment must be deleted after uninstall")
			}

			Eventually(verifyGone).
				WithContext(ctx).
				WithTimeout(90 * time.Second).
				WithPolling(5 * time.Second).
				Should(Succeed())
		})

		It("[TC-E27.224] 설치 해제 후 CRD 보존 검증 (resource-policy:keep)", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			for _, crd := range []string{
				"pillartargets." + crdGroup,
				"pillarpools." + crdGroup,
				"pillarprotocols." + crdGroup,
				"pillarbindings." + crdGroup,
			} {
				out, err := e27KubectlOutput(ctx,
					"get", "crd", crd,
					"-o", "jsonpath={.metadata.name}",
				)
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E27.224] CRD %s must still exist after helm uninstall", crd)
				Expect(out).To(ContainSubstring(crd[:10]),
					"[TC-E27.224] CRD %s must be preserved", crd)
			}
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// Isolated install tests — separate Ordered blocks with unique release names
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E27: Helm 커스텀 values 오버라이드 설치", Label("helm", "E27-cluster"), Ordered, func() {

	const (
		customRelease   = "pillar-csi-custom"
		customNamespace = "pillar-csi-custom"
	)

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeAll(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 12*time.Minute)
		DeferCleanup(cancel)
		err := e27HelmInstall(ctx, customRelease, customNamespace, e27ChartPath(),
			"--set", "controller.replicaCount=2",
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E27.218] BeforeAll: helm install with custom values must succeed")
	})

	AfterAll(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		e27HelmUninstall(cleanCtx, customRelease, customNamespace)
		e27KubectlDeleteNamespace(cleanCtx, customNamespace)
	})

	// ── TC-E27.218 ───────────────────────────────────────────────────────────
	It("[TC-E27.218] 커스텀 values 오버라이드 설치 검증", func() {
		verifyCustomValues := func(g Gomega) {
			out, err := e27KubectlOutput(ctx,
				"get", "deployment",
				"-n", customNamespace,
				"-l", "app.kubernetes.io/component=controller",
				"-o", "jsonpath={.items[0].spec.replicas}",
			)
			g.Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.218] kubectl get deployment must succeed")
			g.Expect(out).To(Equal("2"),
				"[TC-E27.218] controller.replicaCount override to 2 must be reflected in Deployment spec.replicas")
		}

		Eventually(verifyCustomValues).
			WithContext(ctx).
			WithTimeout(2 * time.Minute).
			WithPolling(5 * time.Second).
			Should(Succeed())
	})
})

var _ = Describe("E27: Helm installCRDs=false 설치 모드", Label("helm", "E27-cluster"), Ordered, func() {

	const (
		noCRDRelease   = "pillar-csi-nocrd"
		noCRDNamespace = "pillar-csi-nocrd"
	)

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeAll(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 12*time.Minute)
		DeferCleanup(cancel)

		// Pre-condition: CRDs must already exist (from the main E27 suite install or
		// pre-existing cluster state) so that installCRDs=false doesn't break the chart.
		// If CRDs are absent, this test suite skips gracefully.
		_, err := e27KubectlOutput(ctx,
			"get", "crd", "pillartargets."+crdGroup, "--ignore-not-found",
		)
		if err != nil {
			Skip("[TC-E27.219] CRDs must be pre-installed for installCRDs=false test")
		}

		installErr := e27HelmInstall(ctx, noCRDRelease, noCRDNamespace, e27ChartPath(),
			"--set", "installCRDs=false",
		)
		Expect(installErr).NotTo(HaveOccurred(),
			"[TC-E27.219] BeforeAll: helm install --set installCRDs=false must succeed")
	})

	AfterAll(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		e27HelmUninstall(cleanCtx, noCRDRelease, noCRDNamespace)
		e27KubectlDeleteNamespace(cleanCtx, noCRDNamespace)
	})

	// ── TC-E27.219 ───────────────────────────────────────────────────────────
	It("[TC-E27.219] installCRDs=false 모드에서 Deployment 존재 검증", func() {
		verifyDeployment := func(g Gomega) {
			out, err := e27KubectlOutput(ctx,
				"get", "deployment",
				"-n", noCRDNamespace,
				"-l", "app.kubernetes.io/component=controller",
				"-o", "json",
			)
			g.Expect(err).NotTo(HaveOccurred())

			var depList struct {
				Items []interface{} `json:"items"`
			}
			g.Expect(json.Unmarshal([]byte(out), &depList)).To(Succeed())
			g.Expect(depList.Items).NotTo(BeEmpty(),
				"[TC-E27.219] controller Deployment must exist even with installCRDs=false")
		}
		Eventually(verifyDeployment).
			WithContext(ctx).
			WithTimeout(2 * time.Minute).
			WithPolling(5 * time.Second).
			Should(Succeed())
	})
})

var _ = Describe("E27: CSIDriver 설정 오버라이드 테스트", Label("helm", "E27-cluster"), Ordered, func() {

	// TC-E27.240: csiDriver.create=false
	Describe("TC-E27.240: CSIDriver create=false", Ordered, func() {
		const (
			noDrvRelease   = "pillar-csi-nocsidrv"
			noDrvNamespace = "pillar-csi-nocsidrv"
		)
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)

		BeforeAll(func() {
			ctx, cancel = context.WithTimeout(context.Background(), 12*time.Minute)
			DeferCleanup(cancel)
			err := e27HelmInstall(ctx, noDrvRelease, noDrvNamespace, e27ChartPath(),
				"--set", "csiDriver.create=false",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.240] BeforeAll: helm install --set csiDriver.create=false must succeed")
		})

		AfterAll(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			e27HelmUninstall(cleanCtx, noDrvRelease, noDrvNamespace)
			e27KubectlDeleteNamespace(cleanCtx, noDrvNamespace)
		})

		It("[TC-E27.240] csiDriver.create=false 시 CSIDriver 미생성 검증", func() {
			ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			out, _ := e27KubectlOutput(ctx2,
				"get", "csidriver", csiDriverName,
				"--ignore-not-found",
				"-o", "name",
			)
			Expect(out).To(BeEmpty(),
				"[TC-E27.240] CSIDriver must NOT exist when csiDriver.create=false")
		})
	})

	// TC-E27.241: csiDriver.podInfoOnMount=false
	Describe("TC-E27.241: CSIDriver podInfoOnMount=false override", Ordered, func() {
		const (
			customPimRelease   = "pillar-csi-custom-pim"
			customPimNamespace = "pillar-csi-custom-pim"
		)
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)

		BeforeAll(func() {
			ctx, cancel = context.WithTimeout(context.Background(), 12*time.Minute)
			DeferCleanup(cancel)
			err := e27HelmInstall(ctx, customPimRelease, customPimNamespace, e27ChartPath(),
				"--set", "csiDriver.podInfoOnMount=false",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.241] BeforeAll: helm install --set csiDriver.podInfoOnMount=false must succeed")
		})

		AfterAll(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			e27HelmUninstall(cleanCtx, customPimRelease, customPimNamespace)
			e27KubectlDeleteNamespace(cleanCtx, customPimNamespace)
		})

		It("[TC-E27.241] CSIDriver.podInfoOnMount=false オーバーライド검증", func() {
			ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx2,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.podInfoOnMount}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.241] CSIDriver must exist")
			Expect(out).To(Equal("false"),
				"[TC-E27.241] podInfoOnMount must be false when overridden")
		})
	})

	// TC-E27.242: csiDriver.fsGroupPolicy=None
	Describe("TC-E27.242: CSIDriver fsGroupPolicy=None override", Ordered, func() {
		const (
			noFsgRelease   = "pillar-csi-nofsg"
			noFsgNamespace = "pillar-csi-nofsg"
		)
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)

		BeforeAll(func() {
			ctx, cancel = context.WithTimeout(context.Background(), 12*time.Minute)
			DeferCleanup(cancel)
			err := e27HelmInstall(ctx, noFsgRelease, noFsgNamespace, e27ChartPath(),
				"--set", "csiDriver.fsGroupPolicy=None",
			)
			Expect(err).NotTo(HaveOccurred(),
				"[TC-E27.242] BeforeAll: helm install --set csiDriver.fsGroupPolicy=None must succeed")
		})

		AfterAll(func() {
			cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			e27HelmUninstall(cleanCtx, noFsgRelease, noFsgNamespace)
			e27KubectlDeleteNamespace(cleanCtx, noFsgNamespace)
		})

		It("[TC-E27.242] CSIDriver.fsGroupPolicy=None 오버라이드 검증", func() {
			ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := e27KubectlOutput(ctx2,
				"get", "csidriver", csiDriverName,
				"-o", "jsonpath={.spec.fsGroupPolicy}",
			)
			Expect(err).NotTo(HaveOccurred(), "[TC-E27.242] CSIDriver must exist")
			Expect(out).To(Equal("None"),
				"[TC-E27.242] fsGroupPolicy must be None when overridden")
		})
	})
})

var _ = Describe("E27: Helm upgrade CSIDriver spec 변경 반영", Label("helm", "E27-cluster"), Ordered, func() {

	const (
		upgRelease   = "pillar-csi-upg"
		upgNamespace = "pillar-csi-upg"
	)

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)

	BeforeAll(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Minute)
		DeferCleanup(cancel)
		// Initial install with defaults.
		err := e27HelmInstall(ctx, upgRelease, upgNamespace, e27ChartPath())
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E27.243] BeforeAll: initial helm install must succeed")
	})

	AfterAll(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		e27HelmUninstall(cleanCtx, upgRelease, upgNamespace)
		e27KubectlDeleteNamespace(cleanCtx, upgNamespace)
	})

	It("[TC-E27.243] Helm upgrade CSIDriver spec 변경 반영 검증", func() {
		// Upgrade with podInfoOnMount=false.
		_, _, err := e27HelmOutput(ctx,
			"upgrade", upgRelease, e27ChartPath(),
			"--namespace", upgNamespace,
			"--set", "csiDriver.podInfoOnMount=false",
			"--wait",
			"--timeout", "5m",
		)
		Expect(err).NotTo(HaveOccurred(),
			"[TC-E27.243] helm upgrade --set csiDriver.podInfoOnMount=false must succeed")

		// Verify the CSIDriver spec was patched.
		out, err := e27KubectlOutput(ctx,
			"get", "csidriver", csiDriverName,
			"-o", "jsonpath={.spec.podInfoOnMount}",
		)
		Expect(err).NotTo(HaveOccurred(), "[TC-E27.243] CSIDriver must exist after upgrade")
		Expect(out).To(Equal("false"),
			"[TC-E27.243] podInfoOnMount must be false after helm upgrade")

		// Verify REVISION: 2.
		statusOut, _, _ := e27HelmOutput(ctx,
			"status", upgRelease, "--namespace", upgNamespace,
		)
		Expect(statusOut).To(ContainSubstring("REVISION: 2"),
			"[TC-E27.243] REVISION must be 2 after upgrade")
	})
})
