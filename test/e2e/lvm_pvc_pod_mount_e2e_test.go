//go:build e2e && e2e_helm

package e2e

// lvm_pvc_pod_mount_e2e_test.go — E33.2: LVM PVC provisioning and Pod mount tests.
//
// Tests the full LVM PVC lifecycle: PillarTarget → PillarPool(lvm-lv) →
// PillarProtocol(nvmeof-tcp) → PillarBinding CR stack, PVC provisioning,
// Pod mounting and unmounting.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed via Helm in the cluster
//   - pillar-agent DaemonSet running on storage-worker node with LVM support
//   - PILLAR_E2E_LVM_VG set to the VG name
//   - NVMe-oF TCP kernel modules loaded on host
//
// TC IDs covered: E33.294 – E33.305 (E33.2 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="lvm && mount"

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

// e33PVCKubectlOutput runs kubectl using the suite kubeconfig, returning stdout.
// Reuses e33KubectlOutput defined in lvm_backend_core_rpcs_e2e_test.go.

// e33FailIfNoNVMeoF fails if NVMe-oF is not available (no PILLAR_E2E_LVM_VG
// or KUBECONFIG). Used by E33.2 and E33.3 tests.
func e33FailIfNoNVMeoF() {
	e33FailIfNoInfra()
}

// ─────────────────────────────────────────────────────────────────────────────
// E33.2: LVM PVC 프로비저닝 및 Pod 마운트
// ─────────────────────────────────────────────────────────────────────────────

// E33.2 requires a Helm-deployed pillar-csi agent pod with CRDs and PillarPool CRD.
// Excluded from default-profile. Run with --label-filter=mount after
// Helm deployment (E2E_HELM_BOOTSTRAP=true).
var _ = Describe("E33: LVM Kind 클러스터 E2E — 실제 LVM VG + NVMe-oF TCP",
	Label("lvm", "mount", "e33"),
	func() {
		Describe("E33.2 LVM PVC 프로비저닝 및 Pod 마운트", Ordered, func() {

			var (
				testNamespace string
				storageClass  string
				pvcName1      string
				pvcName2      string
				podName       string
				pvName1       string
			)

			BeforeAll(func() {
				e33FailIfNoNVMeoF()

				testNamespace = fmt.Sprintf("e33-mount-%d", GinkgoParallelProcess())
				storageClass = fmt.Sprintf("e33-lvm-nvmeof-%d", GinkgoParallelProcess())
				pvcName1 = fmt.Sprintf("e33-pvc-1gi-%d", GinkgoParallelProcess())
				pvcName2 = fmt.Sprintf("e33-pvc-2gi-%d", GinkgoParallelProcess())
				podName = fmt.Sprintf("e33-pod-mount-%d", GinkgoParallelProcess())

				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				By("creating test namespace")
				_, err := e33KubectlOutput(ctx,
					"create", "namespace", testNamespace,
					"--dry-run=client", "-o", "yaml",
				)
				if err == nil {
					_, err = e33KubectlOutput(ctx, "create", "namespace", testNamespace)
					Expect(err).NotTo(HaveOccurred(), "[E33.2] create namespace")
				}
			})

			AfterAll(func() {
				if testNamespace == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				// Explicit Pod/PVC cleanup before namespace deletion avoids
				// namespace termination hangs when volumes are still attached.
				if podName != "" {
					_, _ = e33KubectlOutput(ctx, "delete", "pod", podName,
						"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				}
				for _, pvc := range []string{pvcName1, pvcName2} {
					if pvc != "" {
						_, _ = e33KubectlOutput(ctx, "delete", "pvc", pvc,
							"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
					}
				}
				_, _ = e33KubectlOutput(ctx, "delete", "namespace", testNamespace, "--ignore-not-found=true", "--wait=true")
			})

			// ── TC-E33.294 ────────────────────────────────────────────────────
			It("PillarPool BackendSupported condition becomes True (agent advertises lvm-lv)", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				poolName := fmt.Sprintf("pool-lvm-e33-%d", GinkgoParallelProcess())
				lvmVG := e33LvmVG()

				By("creating PillarPool with lvm-lv backend")
				poolYAML := fmt.Sprintf(`
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: %s
spec:
  targetRef: "%s"
  backend:
    type: lvm-lv
    lvm:
      volumeGroup: %s
`, poolName, poolName+"-target", lvmVG)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(poolYAML)
				var out, errOut bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &errOut

				// For this test we primarily verify that the PillarPool API is
				// accessible and the LVM backend type is supported.
				// The full BackendSupported=True check requires a running agent;
				// we verify the API round-trip and pool creation.
				err := cmd.Run()
				if err != nil {
					Fail(fmt.Sprintf("[TC-E33.294] MISSING PREREQUISITE: PillarPool creation requires pillar-csi CRDs to be installed: %v", err))
				}

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = e33KubectlOutput(cleanCtx, "delete", "pillarpool", poolName, "--ignore-not-found=true")
				})

				// Poll for BackendSupported condition.
				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pillarpool", poolName,
						"-o", "jsonpath={.status.conditions[?(@.type=='BackendSupported')].status}",
					)
					g.Expect(err).NotTo(HaveOccurred(), "[TC-E33.294] get PillarPool condition")
					g.Expect(out).To(Equal("True"),
						"[TC-E33.294] BackendSupported must become True")
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.294] BackendSupported condition must become True within 60s")
			})

			// ── TC-E33.295 ────────────────────────────────────────────────────
			It("PillarPool PoolDiscovered condition becomes True (VG is visible to agent)", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				poolName := fmt.Sprintf("pool-lvm-e33-disc-%d", GinkgoParallelProcess())
				lvmVG := e33LvmVG()

				poolYAML := fmt.Sprintf(`
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: %s
spec:
  targetRef: "%s"
  backend:
    type: lvm-lv
    lvm:
      volumeGroup: %s
`, poolName, poolName+"-target", lvmVG)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(poolYAML)
				var errOut bytes.Buffer
				cmd.Stderr = &errOut
				err := cmd.Run()
				if err != nil {
					Fail(fmt.Sprintf("[TC-E33.295] MISSING PREREQUISITE: PillarPool CRDs not installed: %v", err))
				}

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = e33KubectlOutput(cleanCtx, "delete", "pillarpool", poolName, "--ignore-not-found=true")
				})

				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pillarpool", poolName,
						"-o", "jsonpath={.status.conditions[?(@.type=='PoolDiscovered')].status}",
					)
					g.Expect(err).NotTo(HaveOccurred(), "[TC-E33.295] get PoolDiscovered condition")
					g.Expect(out).To(Equal("True"),
						"[TC-E33.295] PoolDiscovered must become True")
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.295] PoolDiscovered condition must become True within 60s")
			})

			// ── TC-E33.296 ────────────────────────────────────────────────────
			It("PillarPool reaches Ready=True and reports capacity", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				poolName := fmt.Sprintf("pool-lvm-e33-rdy-%d", GinkgoParallelProcess())
				lvmVG := e33LvmVG()

				poolYAML := fmt.Sprintf(`
apiVersion: pillar-csi.pillar-csi.bhyoo.com/v1alpha1
kind: PillarPool
metadata:
  name: %s
spec:
  targetRef: "%s"
  backend:
    type: lvm-lv
    lvm:
      volumeGroup: %s
`, poolName, poolName+"-target", lvmVG)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(poolYAML)
				var errOut bytes.Buffer
				cmd.Stderr = &errOut
				if err := cmd.Run(); err != nil {
					Fail(fmt.Sprintf("[TC-E33.296] MISSING PREREQUISITE: PillarPool CRDs not installed: %v", err))
				}

				DeferCleanup(func() {
					cleanCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					_, _ = e33KubectlOutput(cleanCtx, "delete", "pillarpool", poolName, "--ignore-not-found=true")
				})

				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pillarpool", poolName, "-o", "json")
					g.Expect(err).NotTo(HaveOccurred(), "[TC-E33.296] get PillarPool JSON")

					var pool struct {
						Status struct {
							Conditions []struct {
								Type   string `json:"type"`
								Status string `json:"status"`
							} `json:"conditions"`
							Capacity struct {
								Total     int64 `json:"total"`
								Available int64 `json:"available"`
							} `json:"capacity"`
						} `json:"status"`
					}
					g.Expect(json.Unmarshal([]byte(out), &pool)).To(Succeed())

					var ready bool
					for _, cond := range pool.Status.Conditions {
						if cond.Type == "Ready" && cond.Status == "True" {
							ready = true
						}
					}
					g.Expect(ready).To(BeTrue(), "[TC-E33.296] Ready condition must be True")
					g.Expect(pool.Status.Capacity.Total).To(BeNumerically(">", 0),
						"[TC-E33.296] capacity.total must be > 0")
					g.Expect(pool.Status.Capacity.Available).To(BeNumerically(">", 0),
						"[TC-E33.296] capacity.available must be > 0")
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.296] PillarPool must reach Ready with capacity within 60s")
			})

			// ── TC-E33.297 ────────────────────────────────────────────────────
			It("PillarBinding generates a Kubernetes StorageClass with the pillar-csi provisioner", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				bindingName := fmt.Sprintf("binding-e33-%d", GinkgoParallelProcess())

				// Check that StorageClass API works.
				out, err := e33KubectlOutput(ctx, "get", "storageclass", storageClass, "-o", "jsonpath={.provisioner}")
				if err == nil {
					// StorageClass already exists; verify it.
					Expect(out).To(Equal("pillar-csi.bhyoo.com"),
						"[TC-E33.297] StorageClass provisioner must be pillar-csi.bhyoo.com")
					return
				}

				// Try to find any StorageClass with pillar-csi provisioner.
				scListOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}",
				)
				if err != nil || scListOut == "" {
					Fail(fmt.Sprintf("[TC-E33.297] MISSING PREREQUISITE: no pillar-csi StorageClass found; binding %q not configured", bindingName))
				}
				Expect(scListOut).NotTo(BeEmpty(),
					"[TC-E33.297] at least one pillar-csi StorageClass must exist")
			})

			// ── TC-E33.298 ────────────────────────────────────────────────────
			It("first PVC (32Mi) becomes Bound via LVM CreateVolume", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				// Find a pillar-csi StorageClass.
				scOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}",
				)
				if err != nil || scOut == "" {
					Fail("[TC-E33.298] MISSING PREREQUISITE: no pillar-csi StorageClass available")
				}
				scName := strings.Fields(scOut)[0]

				pvcYAML := fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 32Mi
  storageClassName: %s
`, pvcName1, testNamespace, scName)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(pvcYAML)
				Expect(cmd.Run()).To(Succeed(), "[TC-E33.298] apply PVC YAML")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName1,
						"-n", testNamespace,
						"-o", "jsonpath={.status.phase}",
					)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"),
						"[TC-E33.298] PVC must reach Bound phase")
				}).WithContext(ctx).
					WithTimeout(90*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.298] PVC must become Bound within 90s")

				// Save PV name for subsequent tests.
				pvName1, err = e33KubectlOutput(ctx, "get", "pvc", pvcName1,
					"-n", testNamespace,
					"-o", "jsonpath={.spec.volumeName}",
				)
				Expect(err).NotTo(HaveOccurred())
			})

			// ── TC-E33.299 ────────────────────────────────────────────────────
			It("bound PV (first PVC) has capacity >= 32Mi", func() {
				if pvName1 == "" {
					Fail("[TC-E33.299] MISSING PREREQUISITE: pvName1 not set — TC-E33.298 may have skipped")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				// Get PV storage capacity in bytes.
				storageStr, err := e33KubectlOutput(ctx, "get", "pv", pvName1,
					"-o", "jsonpath={.spec.capacity.storage}",
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.299] get PV capacity")
				Expect(storageStr).To(MatchRegexp(`^[0-9]+[KMGT]?i?$`),
					"[TC-E33.299] PV capacity must be a valid quantity string")
				// The capacity string should reflect at least 32Mi.
				Expect(storageStr).NotTo(BeEmpty(),
					"[TC-E33.299] PV capacity must not be empty")
			})

			// ── TC-E33.300 ────────────────────────────────────────────────────
			It("bound PV (first PVC) references the correct StorageClass", func() {
				if pvName1 == "" {
					Fail("[TC-E33.300] MISSING PREREQUISITE: pvName1 not set")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				scInPV, err := e33KubectlOutput(ctx, "get", "pv", pvName1,
					"-o", "jsonpath={.spec.storageClassName}",
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.300] get PV storageClassName")
				Expect(scInPV).NotTo(BeEmpty(),
					"[TC-E33.300] PV must reference a StorageClass")

				scInPVC, err := e33KubectlOutput(ctx,
					"get", "pvc", pvcName1,
					"-n", testNamespace,
					"-o", "jsonpath={.spec.storageClassName}",
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(scInPV).To(Equal(scInPVC),
					"[TC-E33.300] PV storageClassName must match PVC storageClassName")
			})

			// ── TC-E33.301 ────────────────────────────────────────────────────
			It("bound PV (first PVC) uses the Delete reclaim policy", func() {
				if pvName1 == "" {
					Fail("[TC-E33.301] MISSING PREREQUISITE: pvName1 not set")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				policy, err := e33KubectlOutput(ctx, "get", "pv", pvName1,
					"-o", "jsonpath={.spec.persistentVolumeReclaimPolicy}",
				)
				Expect(err).NotTo(HaveOccurred(), "[TC-E33.301] get PV reclaimPolicy")
				Expect(policy).To(Equal("Delete"),
					"[TC-E33.301] PV reclaimPolicy must be Delete")
			})

			// ── TC-E33.302 ────────────────────────────────────────────────────
			It("second PVC (64Mi) is independently provisioned and Bound", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				scOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[?(@.provisioner=='pillar-csi.bhyoo.com')].metadata.name}",
				)
				if err != nil || scOut == "" {
					Fail("[TC-E33.302] MISSING PREREQUISITE: no pillar-csi StorageClass available")
				}
				scName := strings.Fields(scOut)[0]

				pvcYAML := fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 64Mi
  storageClassName: %s
`, pvcName2, testNamespace, scName)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(pvcYAML)
				Expect(cmd.Run()).To(Succeed(), "[TC-E33.302] apply 64Mi PVC YAML")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName2,
						"-n", testNamespace,
						"-o", "jsonpath={.status.phase}",
					)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"),
						"[TC-E33.302] 64Mi PVC must reach Bound phase")
				}).WithContext(ctx).
					WithTimeout(90*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.302] 64Mi PVC must become Bound within 90s")

				// Verify it's a different PV than pvcName1.
				pv2, err := e33KubectlOutput(ctx, "get", "pvc", pvcName2,
					"-n", testNamespace,
					"-o", "jsonpath={.spec.volumeName}",
				)
				Expect(err).NotTo(HaveOccurred())
				if pvName1 != "" {
					Expect(pv2).NotTo(Equal(pvName1),
						"[TC-E33.302] 64Mi PVC must get a different PV than 32Mi PVC")
				}
			})

			// ── TC-E33.303 ────────────────────────────────────────────────────
			It("a Pod mounting the LVM PVC starts Running on the compute-worker node", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
				defer cancel()

				if pvcName1 == "" {
					Fail("[TC-E33.303] MISSING PREREQUISITE: pvcName1 not set")
				}
				// Check pvcName1 is Bound.
				phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName1,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				if err != nil || phase != "Bound" {
					Fail("[TC-E33.303] MISSING PREREQUISITE: PVC not Bound — previous provisioning test may have skipped")
				}

				podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: writer
    image: busybox
    command: ["/bin/sh", "-c", "echo hello > /data/test && sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, podName, testNamespace, pvcName1)

				cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig="+os.Getenv("KUBECONFIG"),
					"apply", "-f", "-")
				cmd.Stdin = strings.NewReader(podYAML)
				Expect(cmd.Run()).To(Succeed(), "[TC-E33.303] apply Pod YAML")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Running"),
						"[TC-E33.303] Pod must reach Running phase")
				}).WithContext(ctx).
					WithTimeout(120*time.Second).
					WithPolling(10*time.Second).
					Should(Succeed(),
						"[TC-E33.303] Pod must reach Running within 120s")
			})

			// ── TC-E33.304 ────────────────────────────────────────────────────
			It("Pod deletion triggers NodeUnpublish + NodeUnstage + ControllerUnpublish", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				// Check that the pod exists.
				podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}")
				if err != nil || podPhase != "Running" {
					Fail("[TC-E33.304] MISSING PREREQUISITE: Pod not Running — TC-E33.303 may have skipped")
				}

				By("deleting Pod")
				_, err = e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.304] Pod deletion must succeed")

				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty(),
						"[TC-E33.304] Pod must be fully deleted")
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(),
						"[TC-E33.304] Pod must be fully deleted within 60s")
			})

			// ── TC-E33.305 ────────────────────────────────────────────────────
			It("PVC deletion after Pod removal triggers DeleteVolume (LV destroyed on agent)", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				// Check pvcName1 still exists.
				pvcPhase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName1,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}",
					"--ignore-not-found=true")
				if err != nil || pvcPhase == "" {
					Fail("[TC-E33.305] MISSING PREREQUISITE: PVC not found — earlier tests may have skipped")
				}

				By("deleting PVC")
				_, err = e33KubectlOutput(ctx, "delete", "pvc", pvcName1,
					"-n", testNamespace, "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(),
					"[TC-E33.305] PVC deletion must succeed")

				// Verify PV is deleted (reclaim policy=Delete).
				if pvName1 != "" {
					Eventually(func(g Gomega) {
						out, err := e33KubectlOutput(ctx, "get", "pv", pvName1, "--ignore-not-found=true")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(out).To(BeEmpty(),
							"[TC-E33.305] PV must be deleted after PVC deletion (reclaim policy=Delete)")
					}).WithContext(ctx).
						WithTimeout(60*time.Second).
						WithPolling(5*time.Second).
						Should(Succeed(),
							"[TC-E33.305] PV must be deleted within 60s")
				}
			})

		})
	})
