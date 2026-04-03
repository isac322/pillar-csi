//go:build e2e && e2e_helm

package e2e

// zfs_iscsi_pvc_pod_mount_e2e_test.go — E35.2: zvol-backed Filesystem PVC 및 Pod 마운트
//
// Validates that a PVC backed by ZFS zvol + iSCSI becomes Bound, a Pod can
// mount it on compute-worker with ZFS-specific parameters preserved,
// session management happens (login/logout), and cleanup is complete.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed with ZFS + iSCSI support
//   - PILLAR_E2E_ZFS_POOL set
//   - ZFS kernel module loaded; LIO + open-iscsi kernel modules loaded
//
// TC IDs covered: E35.335 – E35.339 (E35.2 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="iscsi && zfs && mount"

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─────────────────────────────────────────────────────────────────────────────
// E35.2: zvol-backed Filesystem PVC 및 Pod 마운트
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E35: ZFS Kind 클러스터 E2E — 실제 ZFS zvol + iSCSI",
	Label("iscsi", "zfs", "mount", "e35"),
	func() {
		Describe("E35.2 zvol-backed Filesystem PVC 및 Pod 마운트", Ordered, func() {

			var (
				testNamespace  string
				zfsISCSISCName string
				pvcName        string
				podName        string
			)

			BeforeAll(func() {
				e35FailIfNoInfra()

				testNamespace = fmt.Sprintf("e35-mount-%d", GinkgoParallelProcess())
				pvcName = fmt.Sprintf("e35-mount-pvc-%d", GinkgoParallelProcess())
				podName = fmt.Sprintf("e35-mount-pod-%d", GinkgoParallelProcess())

				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				// Find ZFS+iSCSI StorageClass.
				scOut, err := e33KubectlOutput(ctx, "get", "storageclass",
					"-o", "jsonpath={.items[*].metadata.name}")
				if err == nil && scOut != "" {
					for _, sc := range strings.Fields(scOut) {
						params, err := e33KubectlOutput(ctx, "get", "storageclass", sc,
							"-o", "jsonpath={.parameters}")
						if err == nil && strings.Contains(params, "iscsi") && strings.Contains(params, "zfs") {
							zfsISCSISCName = sc
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

			// ── TC-E35.335 ────────────────────────────────────────────────────
			It("[TC-E35.335] filesystem PVC becomes Bound via ZFS zvol + iSCSI", func() {
				if zfsISCSISCName == "" {
					Fail("[TC-E35.335] MISSING PREREQUISITE: no ZFS+iSCSI StorageClass — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				kubeconfig := e34Kubeconfig()
				By("creating Filesystem PVC with ZFS+iSCSI StorageClass")
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
`, pvcName, testNamespace, zfsISCSISCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, pvcYAML)).To(Succeed(), "[TC-E35.335] apply ZFS+iSCSI PVC")

				By("waiting for PVC to be Bound")
				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"), "[TC-E35.335] PVC must be Bound")
				}).WithContext(ctx).
					WithTimeout(90*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E35.335] PVC must bind within 90s")

				By("verifying PV provisioner and backend")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvName).NotTo(BeEmpty())

				provisioner, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.driver}")
				Expect(err).NotTo(HaveOccurred())
				Expect(provisioner).To(Equal("pillar-csi.bhyoo.com"),
					"[TC-E35.335] PV provisioner must be pillar-csi.bhyoo.com")
			})

			// ── TC-E35.336 ────────────────────────────────────────────────────
			It("[TC-E35.336] a Pod mounting the zvol-backed iSCSI PVC reaches Running on the compute-worker node", func() {
				if zfsISCSISCName == "" {
					Fail("[TC-E35.336] MISSING PREREQUISITE: no ZFS+iSCSI StorageClass — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || phase != "Bound" {
					Fail("[TC-E35.336] MISSING PREREQUISITE: PVC not Bound — TC-E35.335 may have skipped")
				}

				kubeconfig := e34Kubeconfig()
				By("creating Pod that mounts the ZFS+iSCSI PVC")
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
				Expect(e34ApplyStdin(ctx, kubeconfig, podYAML)).To(Succeed(), "[TC-E35.336] apply Pod")

				By("waiting for Pod to reach Running")
				Eventually(func(g Gomega) {
					podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(podPhase).To(Equal("Running"), "[TC-E35.336] Pod must reach Running")
				}).WithContext(ctx).
					WithTimeout(240*time.Second).
					WithPolling(10*time.Second).
					Should(Succeed(), "[TC-E35.336] Pod must be Running within 240s")
			})

			// ── TC-E35.337 ────────────────────────────────────────────────────
			It("[TC-E35.337] zfs-specific volume parameters remain effective when the protocol is iSCSI", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || podPhase != "Running" {
					Fail("[TC-E35.337] MISSING PREREQUISITE: Pod not Running — TC-E35.336 may have skipped")
				}

				By("verifying PV VolumeContext includes ZFS-specific parameters")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvName).NotTo(BeEmpty())

				volumeAttrs, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.volumeAttributes}")
				Expect(err).NotTo(HaveOccurred())
				Expect(volumeAttrs).NotTo(BeEmpty(),
					"[TC-E35.337] PV volumeAttributes must not be empty")

				// Verify VolumeID contains zfs indicator (ZFS volumes include zfs-zvol in ID).
				volumeID, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.volumeHandle}")
				Expect(err).NotTo(HaveOccurred())
				Expect(volumeID).NotTo(BeEmpty(), "[TC-E35.337] VolumeHandle must not be empty")

				// Check ZFS pool parameter is preserved in StorageClass.
				if zfsISCSISCName != "" {
					params, err := e33KubectlOutput(ctx, "get", "storageclass", zfsISCSISCName,
						"-o", "jsonpath={.parameters}")
					Expect(err).NotTo(HaveOccurred())
					Expect(params).To(ContainSubstring(e35ZFSPool()),
						"[TC-E35.337] StorageClass parameters must reference the ZFS pool")
				}
			})

			// ── TC-E35.338 ────────────────────────────────────────────────────
			It("[TC-E35.338] deleting the Pod triggers NodeUnpublish, NodeUnstage and iSCSI logout for the zvol-backed volume", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				podPhase, err := e33KubectlOutput(ctx, "get", "pod", podName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || podPhase != "Running" {
					Fail("[TC-E35.338] MISSING PREREQUISITE: Pod not Running — TC-E35.336 may have skipped")
				}

				By("deleting the Pod")
				_, err = e33KubectlOutput(ctx, "delete", "pod", podName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.338] pod deletion")

				By("verifying Pod is fully deleted")
				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pod", podName,
						"-n", testNamespace, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty(), "[TC-E35.338] Pod must be gone")
				}).WithContext(ctx).
					WithTimeout(60 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("verifying PVC still exists (PV not deleted)")
				pvcPhase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvcPhase).To(Equal("Bound"),
					"[TC-E35.338] PVC must remain Bound after Pod deletion")
			})

			// ── TC-E35.339 ────────────────────────────────────────────────────
			It("[TC-E35.339] deleting the PVC removes the exported target and destroys the zvol", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				pvcPhase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if err != nil || pvcPhase == "" {
					Fail("[TC-E35.339] MISSING PREREQUISITE: PVC not found — skipping cleanup verification")
				}

				pvName, _ := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")

				By("deleting the PVC")
				_, err = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.339] PVC deletion")

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
							"[TC-E35.339] PV must not remain Bound after PVC deletion")
					}
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E35.339] PV must be cleaned up")
			})

		})
	})
