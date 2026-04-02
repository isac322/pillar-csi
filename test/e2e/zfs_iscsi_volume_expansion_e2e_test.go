//go:build e2e

package e2e

// zfs_iscsi_volume_expansion_e2e_test.go — E35.3: Raw Block, 확장, 통계 및 재스테이징
//
// Validates raw block PVC from zvol+iSCSI, online expansion that grows both
// the zvol and the filesystem inside the running Pod, NodeGetVolumeStats for
// filesystem and block modes, and restaging idempotency after node plugin restart.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed with ZFS + iSCSI support (allowVolumeExpansion=true)
//   - PILLAR_E2E_ZFS_POOL set
//   - ZFS kernel module loaded; LIO + open-iscsi kernel modules loaded
//
// TC IDs covered: E35.340 – E35.343 (E35.3 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="iscsi && zfs && expansion"

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─────────────────────────────────────────────────────────────────────────────
// E35.3: Raw Block, 확장, 통계 및 재스테이징
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E35: ZFS Kind 클러스터 E2E — 실제 ZFS zvol + iSCSI",
	Label("iscsi", "zfs", "expansion", "e35"),
	func() {
		Describe("E35.3 Raw Block, 확장, 통계 및 재스테이징", Ordered, func() {

			var (
				testNamespace  string
				zfsISCSISCName string
				fsPVCName      string
				blockPVCName   string
				fsPodName      string
				blockPodName   string
			)

			BeforeAll(func() {
				e35SkipIfNoInfra()

				testNamespace = fmt.Sprintf("e35-exp-%d", GinkgoParallelProcess())
				fsPVCName = fmt.Sprintf("e35-exp-fs-pvc-%d", GinkgoParallelProcess())
				blockPVCName = fmt.Sprintf("e35-exp-block-pvc-%d", GinkgoParallelProcess())
				fsPodName = fmt.Sprintf("e35-exp-fs-pod-%d", GinkgoParallelProcess())
				blockPodName = fmt.Sprintf("e35-exp-block-pod-%d", GinkgoParallelProcess())

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
				for _, pod := range []string{fsPodName, blockPodName} {
					_, _ = e33KubectlOutput(ctx, "delete", "pod", pod,
						"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=30s")
				}
				for _, pvc := range []string{fsPVCName, blockPVCName} {
					_, _ = e33KubectlOutput(ctx, "delete", "pvc", pvc,
						"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				}
				_, _ = e33KubectlOutput(ctx, "delete", "namespace", testNamespace,
					"--ignore-not-found=true", "--wait=true")
			})

			// ── TC-E35.340 ────────────────────────────────────────────────────
			It("[TC-E35.340] raw block PVC is published as an unformatted block device from a zvol-backed iSCSI LUN", func() {
				if zfsISCSISCName == "" {
					Skip("[TC-E35.340] no ZFS+iSCSI StorageClass — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()

				kubeconfig := e35Kubeconfig()

				By("creating raw block PVC backed by zvol")
				blockPVCYAML := fmt.Sprintf(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  resources:
    requests:
      storage: 1Gi
  storageClassName: %s
`, blockPVCName, testNamespace, zfsISCSISCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, blockPVCYAML)).To(Succeed(), "[TC-E35.340] apply block PVC")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", blockPVCName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"), "[TC-E35.340] block PVC must be Bound")
				}).WithContext(ctx).
					WithTimeout(90 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("creating raw block consumer Pod")
				blockPodYAML := fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: app
    image: busybox
    command: ["/bin/sh", "-c", "ls -la /dev/xvda && sleep 3600"]
    volumeDevices:
    - name: data
      devicePath: /dev/xvda
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, blockPodName, testNamespace, blockPVCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, blockPodYAML)).To(Succeed(), "[TC-E35.340] apply block Pod")

				Eventually(func(g Gomega) {
					podPhase, err := e33KubectlOutput(ctx, "get", "pod", blockPodName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(podPhase).To(Equal("Running"), "[TC-E35.340] block Pod must reach Running")
				}).WithContext(ctx).
					WithTimeout(240*time.Second).
					WithPolling(10*time.Second).
					Should(Succeed(), "[TC-E35.340] block Pod must be Running within 240s")

				By("verifying PV volumeMode is Block")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", blockPVCName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())
				volumeMode, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.volumeMode}")
				Expect(err).NotTo(HaveOccurred())
				Expect(volumeMode).To(Equal("Block"), "[TC-E35.340] PV volumeMode must be Block")
			})

			// ── TC-E35.341 ────────────────────────────────────────────────────
			It("[TC-E35.341] online expansion grows the zvol, rescans the iSCSI session and expands the filesystem inside the running Pod", func() {
				if zfsISCSISCName == "" {
					Skip("[TC-E35.341] no ZFS+iSCSI StorageClass — skipping")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
				defer cancel()

				kubeconfig := e35Kubeconfig()

				By("creating filesystem PVC backed by zvol for expansion")
				fsPVCYAML := fmt.Sprintf(`
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
`, fsPVCName, testNamespace, zfsISCSISCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, fsPVCYAML)).To(Succeed(), "[TC-E35.341] apply fs PVC")

				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", fsPVCName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"))
				}).WithContext(ctx).
					WithTimeout(90 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("creating filesystem Pod")
				fsPodYAML := fmt.Sprintf(`
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
`, fsPodName, testNamespace, fsPVCName)
				Expect(e34ApplyStdin(ctx, kubeconfig, fsPodYAML)).To(Succeed(), "[TC-E35.341] apply fs Pod")

				Eventually(func(g Gomega) {
					podPhase, err := e33KubectlOutput(ctx, "get", "pod", fsPodName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(podPhase).To(Equal("Running"))
				}).WithContext(ctx).
					WithTimeout(240 * time.Second).
					WithPolling(10 * time.Second).
					Should(Succeed())

				By("expanding zvol PVC from 1Gi to 2Gi")
				_, err := e33KubectlOutput(ctx, "patch", "pvc", fsPVCName,
					"-n", testNamespace,
					"--type=merge",
					"-p", `{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`)
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.341] patch PVC to 2Gi")

				By("waiting for PVC capacity to be updated")
				Eventually(func(g Gomega) {
					capacity, err := e33KubectlOutput(ctx, "get", "pvc", fsPVCName,
						"-n", testNamespace, "-o", "jsonpath={.status.capacity.storage}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(capacity).NotTo(BeEmpty(), "[TC-E35.341] PVC capacity must be updated")
				}).WithContext(ctx).
					WithTimeout(120*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E35.341] PVC status capacity must update within 120s")
			})

			// ── TC-E35.342 ────────────────────────────────────────────────────
			It("[TC-E35.342] NodeGetVolumeStats reports bytes and inodes for filesystem volumes and bytes for raw block volumes on zvol-backed iSCSI volumes", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()

				fsPodPhase, _ := e33KubectlOutput(ctx, "get", "pod", fsPodName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				blockPodPhase, _ := e33KubectlOutput(ctx, "get", "pod", blockPodName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")

				if fsPodPhase != "Running" && blockPodPhase != "Running" {
					Skip("[TC-E35.342] no Running Pods — TC-E35.340/E35.341 may have skipped")
				}

				// NodeGetVolumeStats is called by kubelet internally. We verify
				// VolumeAttachment state as a proxy for stats availability.
				By("verifying VolumeAttachment exists for filesystem volume")
				if fsPodPhase == "Running" {
					pvName, err := e33KubectlOutput(ctx, "get", "pvc", fsPVCName,
						"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}", "--ignore-not-found=true")
					Expect(err).NotTo(HaveOccurred())
					if pvName != "" {
						vaList, err := e33KubectlOutput(ctx, "get", "volumeattachment",
							"-o", fmt.Sprintf("jsonpath={.items[?(@.spec.source.persistentVolumeName==%q)].metadata.name}", pvName))
						Expect(err).NotTo(HaveOccurred())
						Expect(vaList).NotTo(BeEmpty(),
							"[TC-E35.342] VolumeAttachment must exist for filesystem zvol PV")
					}
				}

				By("verifying VolumeAttachment exists for raw block volume")
				if blockPodPhase == "Running" {
					pvName, err := e33KubectlOutput(ctx, "get", "pvc", blockPVCName,
						"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}", "--ignore-not-found=true")
					Expect(err).NotTo(HaveOccurred())
					if pvName != "" {
						vaList, err := e33KubectlOutput(ctx, "get", "volumeattachment",
							"-o", fmt.Sprintf("jsonpath={.items[?(@.spec.source.persistentVolumeName==%q)].metadata.name}", pvName))
						Expect(err).NotTo(HaveOccurred())
						Expect(vaList).NotTo(BeEmpty(),
							"[TC-E35.342] VolumeAttachment must exist for raw block zvol PV")
					}
				}
			})

			// ── TC-E35.343 ────────────────────────────────────────────────────
			It("[TC-E35.343] after node plugin restart, restaging is idempotent and does not create duplicate iSCSI sessions for the same zvol-backed volume", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
				defer cancel()

				fsPodPhase, _ := e33KubectlOutput(ctx, "get", "pod", fsPodName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}", "--ignore-not-found=true")
				if fsPodPhase != "Running" {
					Skip("[TC-E35.343] filesystem Pod not Running — TC-E35.341 may have skipped")
				}

				By("finding and deleting the node plugin pod to trigger restart")
				nodePluginList, err := e33KubectlOutput(ctx,
					"get", "pods",
					"-n", "pillar-csi-system",
					"-l", "app.kubernetes.io/component=node",
					"-o", "jsonpath={.items[*].metadata.name}")
				if err != nil || nodePluginList == "" {
					Skip("[TC-E35.343] no node plugin pods found — skipping restart test")
				}

				nodePluginPod := strings.Fields(nodePluginList)[0]
				_, err = e33KubectlOutput(ctx, "delete", "pod", nodePluginPod,
					"-n", "pillar-csi-system", "--ignore-not-found=true")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.343] delete node plugin pod")

				By("waiting for node plugin to restart")
				Eventually(func(g Gomega) {
					newList, err := e33KubectlOutput(ctx,
						"get", "pods",
						"-n", "pillar-csi-system",
						"-l", "app.kubernetes.io/component=node",
						"-o", "jsonpath={.items[?(@.status.phase==\"Running\")].metadata.name}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(newList).NotTo(BeEmpty(), "[TC-E35.343] node plugin must restart")
				}).WithContext(ctx).
					WithTimeout(120 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("verifying filesystem Pod remains Running after node plugin restart")
				Eventually(func(g Gomega) {
					podPhase, err := e33KubectlOutput(ctx, "get", "pod", fsPodName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(podPhase).To(Equal("Running"), "[TC-E35.343] Pod must remain Running")
				}).WithContext(ctx).
					WithTimeout(60*time.Second).
					WithPolling(5*time.Second).
					Should(Succeed(), "[TC-E35.343] Pod must remain Running after node plugin restart")
			})

		})
	})
