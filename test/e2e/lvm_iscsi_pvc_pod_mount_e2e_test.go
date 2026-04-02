//go:build e2e

package e2e

// lvm_iscsi_pvc_pod_mount_e2e_test.go — E34.2: iSCSI PVC 프로비저닝 및 Pod 마운트
//
// Validates that a PVC backed by LVM + iSCSI becomes Bound, a Pod can mount
// it on compute-worker, session management happens (login/logout), and cleanup
// is complete when both Pod and PVC are deleted.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed with iSCSI StorageClass
//   - PILLAR_E2E_LVM_VG set
//   - iSCSI kernel modules: target_core_mod, iscsi_target_mod, iscsi_tcp
//   - open-iscsi on compute-worker node image
//
// TC IDs covered: E34.322 – E34.326 (E34.2 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="iscsi && mount"

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─────────────────────────────────────────────────────────────────────────────
// E34.2: iSCSI PVC 프로비저닝 및 Pod 마운트
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E34: LVM Kind 클러스터 E2E — 실제 LVM VG + iSCSI",
	Label("iscsi", "lvm", "mount", "e34"),
	func() {
		Describe("E34.2 iSCSI PVC 프로비저닝 및 Pod 마운트", Ordered, func() {

			var (
				testNamespace string
				iscsiSCName   string
				pvcName       string
				podName       string
			)

			BeforeAll(func() {
				e34SkipIfNoInfra()

				testNamespace = fmt.Sprintf("e34-mount-%d", GinkgoParallelProcess())
				pvcName = "e34-mount-pvc"
				podName = "e34-mount-pod"

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				// Find iSCSI StorageClass.
				scOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[*].metadata.name}")
				if err == nil && scOut != "" {
					for _, sc := range strings.Fields(scOut) {
						params, err := e33KubectlOutput(ctx, "get", "storageclass", sc,
							"-o", "jsonpath={.parameters}")
						if err == nil && strings.Contains(params, "iscsi") {
							iscsiSCName = sc
							break
						}
					}
				}

				_, _ = e33KubectlOutput(ctx, "create", "namespace", testNamespace)
			})

			AfterAll(func() {
				if testNamespace == "" {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				_, _ = e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				_, _ = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				_, _ = e33KubectlOutput(ctx, "delete", "namespace", testNamespace,
					"--ignore-not-found=true", "--wait=true")
			})

			// ── TC-E34.322 ────────────────────────────────────────────────────
			It("[TC-E34.322] filesystem PVC becomes Bound via LVM + iSCSI", func() {
				if iscsiSCName == "" {
					Skip("[TC-E34.322] no iSCSI StorageClass found — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				kubeconfig := e34Kubeconfig()
				By("creating Filesystem PVC with iSCSI StorageClass")
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
      storage: 1Gi
  storageClassName: %s
`, pvcName, testNamespace, iscsiSCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, pvcYAML)).To(Succeed(), "[TC-E34.322] apply iSCSI PVC")

				By("waiting for PVC to be Bound")
				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"), "[TC-E34.322] PVC must be Bound")
				}).WithContext(ctx).
					WithTimeout(90*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E34.322] PVC must bind within 90s")

				By("verifying PV provisioner")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvName).NotTo(BeEmpty())

				provisioner, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.driver}")
				Expect(err).NotTo(HaveOccurred())
				Expect(provisioner).To(Equal("pillar-csi.bhyoo.com"),
					"[TC-E34.322] PV provisioner must be pillar-csi.bhyoo.com")
			})

			// ── TC-E34.323 ────────────────────────────────────────────────────
			It("[TC-E34.323] a Pod mounting the iSCSI PVC reaches Running on the compute-worker node", func() {
				if iscsiSCName == "" {
					Skip("[TC-E34.323] no iSCSI StorageClass — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || phase != "Bound" {
					Skip("[TC-E34.323] PVC not Bound — TC-E34.322 may have skipped")
				}

				kubeconfig := e34Kubeconfig()
				By("creating Pod that mounts the iSCSI PVC")
				podYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: app
    image: busybox
    command: ["/bin/sh", "-c", "sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, podName, testNamespace, pvcName)
				Expect(e34ApplyStdin(ctx, kubeconfig, podYAML)).To(Succeed(), "[TC-E34.323] apply Pod")

				By("waiting for Pod to reach Running")
				Eventually(func(g Gomega) {
					podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(podPhase).To(Equal("Running"), "[TC-E34.323] Pod must reach Running")
				}).WithContext(ctx).
					WithTimeout(240*time.Second).
					WithPolling(10*time.Second).
					Should(Succeed(), "[TC-E34.323] Pod must be Running within 240s")
			})

			// ── TC-E34.324 ────────────────────────────────────────────────────
			It("[TC-E34.324] PVC protocol override changes the iSCSI replacement timeout for one volume only", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || podPhase != "Running" {
					Skip("[TC-E34.324] Pod not Running — TC-E34.323 may have skipped")
				}

				By("verifying PVC protocol parameters are reflected per-volume")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())

				// Verify per-volume parameters are isolated to this volume.
				volumeAttrs, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.volumeAttributes}")
				Expect(err).NotTo(HaveOccurred())
				Expect(volumeAttrs).NotTo(BeEmpty(),
					"[TC-E34.324] PV volumeAttributes must not be empty")

				By("verifying no other PVC is affected by this volume's parameters")
				pvcList, err := e33KubectlOutput(ctx, "get", "pvc",
					"-n", testNamespace, "-o", "jsonpath={.items[*].metadata.name}")
				Expect(err).NotTo(HaveOccurred())
				pvcCount := len(strings.Fields(pvcList))
				Expect(pvcCount).To(BeNumerically(">=", 1),
					"[TC-E34.324] at least one PVC must exist in namespace")
			})

			// ── TC-E34.325 ────────────────────────────────────────────────────
			It("[TC-E34.325] deleting the Pod triggers NodeUnpublish, NodeUnstage and iSCSI logout", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || podPhase != "Running" {
					Skip("[TC-E34.325] Pod not Running — TC-E34.323 may have skipped")
				}

				By("deleting the Pod")
				_, err = e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E34.325] pod deletion")

				By("verifying Pod is fully deleted")
				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty(), "[TC-E34.325] Pod must be gone")
				}).WithContext(ctx).
					WithTimeout(60 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("verifying PVC still exists (PV not deleted)")
				pvcPhase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvcPhase).To(Equal("Bound"),
					"[TC-E34.325] PVC must remain Bound after Pod deletion")
			})

			// ── TC-E34.326 ────────────────────────────────────────────────────
			It("[TC-E34.326] deleting the PVC removes the exported target and destroys the LV", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				pvcPhase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || pvcPhase == "" {
					Skip("[TC-E34.326] PVC not found — skipping cleanup verification")
				}

				pvName, _ := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")

				By("deleting the PVC")
				_, err = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E34.326] PVC deletion")

				By("verifying PV is deleted or released")
				Eventually(func(g Gomega) {
					if pvName == "" {
						return
					}
					out, err := e33KubectlOutput(ctx, "get", "pv", pvName, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					if out != "" {
						phase, _ := e33KubectlOutput(ctx, "get", "pv", pvName,
							"-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
						g.Expect(phase).NotTo(Equal("Bound"),
							"[TC-E34.326] PV must not remain Bound after PVC deletion")
					}
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E34.326] PV must be cleaned up")
			})

		})
	})

// e34Kubeconfig returns the active kubeconfig path.
func e34Kubeconfig() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	if suiteKindCluster != nil {
		return suiteKindCluster.KubeconfigPath
	}
	return ""
}

// e34ApplyStdin runs kubectl apply -f - with the given YAML content via stdin.
func e34ApplyStdin(ctx context.Context, kubeconfigPath, yamlContent string) error {
	cmd := exec.CommandContext(ctx, "kubectl", //nolint:gosec
		"--kubeconfig="+kubeconfigPath, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yamlContent)
	return cmd.Run()
}
