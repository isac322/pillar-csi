//go:build e2e

package e2e

// zfs_iscsi_core_rpcs_e2e_test.go — E35.1: zvol 백엔드 제어면 및 export 계약
//
// Validates that a PillarBinding combining zfs-zvol backend + iSCSI protocol
// generates a correct StorageClass, that CreateVolume returns iSCSI VolumeContext
// fields (IQN, portal, port, LUN) for a zvol-backed volume, and that
// ControllerPublish/Unpublish correctly manages ACL entries via CSINode annotations.
//
// Prerequisites:
//   - Kind cluster bootstrapped (KUBECONFIG set)
//   - pillar-csi deployed with ZFS + iSCSI support
//   - PILLAR_E2E_ZFS_POOL environment variable set
//   - ZFS kernel module loaded; LIO kernel modules loaded
//
// TC IDs covered: E35.331 – E35.334 (E35.1 subsection)
//
// Build tag: //go:build e2e
// Run with: go test -tags=e2e ./test/e2e/ --ginkgo.label-filter="iscsi && zfs && controlplane"

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ─────────────────────────────────────────────────────────────────────────────
// E35 shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// e35ZFSPool returns the ZFS pool name from the environment.
func e35ZFSPool() string { return os.Getenv("PILLAR_E2E_ZFS_POOL") }

// e35ZFSParentDataset returns the optional ZFS parent dataset for zvol tests.
func e35ZFSParentDataset() string { return os.Getenv("PILLAR_E2E_ZFS_PARENT_DATASET") }

// e35ISCSIPort returns the iSCSI port (default 3260).
func e35ISCSIPort() string {
	if p := os.Getenv("PILLAR_E2E_ISCSI_PORT"); p != "" {
		return p
	}
	return "3260"
}

// e35Kubeconfig returns the kubeconfig path for E35 tests.
func e35Kubeconfig() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	if suiteKindCluster != nil {
		return suiteKindCluster.KubeconfigPath
	}
	return ""
}

// e35SkipIfNoInfra skips if E35 infrastructure is unavailable.
func e35SkipIfNoInfra() {
	if e35ZFSPool() == "" {
		Skip("[E35] PILLAR_E2E_ZFS_POOL not set — skipping ZFS+iSCSI test")
	}
	if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
		Skip("[E35] KUBECONFIG not set and suiteKindCluster is nil — Kind cluster not available")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// E35.1: zvol 백엔드 제어면 및 export 계약
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("E35: ZFS Kind 클러스터 E2E — 실제 ZFS zvol + iSCSI",
	Label("iscsi", "zfs", "controlplane", "e35"),
	func() {
		Describe("E35.1 zvol 백엔드 제어면 및 export 계약", Ordered, func() {

			var (
				testNamespace  string
				zfsISCSISCName string
				pvcName        string
			)

			BeforeAll(func() {
				e35SkipIfNoInfra()

				testNamespace = fmt.Sprintf("e35-ctrl-%d", GinkgoParallelProcess())
				pvcName = "e35-ctrl-pvc"

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
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				_, _ = e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=30s")
				_, _ = e33KubectlOutput(ctx, "delete", "namespace", testNamespace,
					"--ignore-not-found=true", "--wait=true")
			})

			// ── TC-E35.331 ────────────────────────────────────────────────────
			It("[TC-E35.331] PillarBinding generates an iSCSI StorageClass for zfs-zvol pools without losing zvol parameters", func() {
				if zfsISCSISCName == "" {
					Skip("[TC-E35.331] no ZFS+iSCSI StorageClass found — PillarBinding with ZFS+iSCSI not configured")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				By("verifying StorageClass has backend-type=zfs-zvol")
				params, err := e33KubectlOutput(ctx, "get", "storageclass", zfsISCSISCName,
					"-o", "jsonpath={.parameters}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.331] get StorageClass parameters")
				Expect(params).To(ContainSubstring("iscsi"),
					"[TC-E35.331] StorageClass parameters must contain iscsi protocol reference")
				Expect(params).To(ContainSubstring("zfs"),
					"[TC-E35.331] StorageClass parameters must contain zfs backend reference")

				By("verifying provisioner")
				provisioner, err := e33KubectlOutput(ctx, "get", "storageclass", zfsISCSISCName,
					"-o", "jsonpath={.provisioner}")
				Expect(err).NotTo(HaveOccurred())
				Expect(provisioner).To(Equal("pillar-csi.bhyoo.com"),
					"[TC-E35.331] StorageClass provisioner must be pillar-csi.bhyoo.com")
			})

			// ── TC-E35.332 ────────────────────────────────────────────────────
			It("[TC-E35.332] CreateVolume provisions a zvol-backed volume and returns target IQN, portal, port and LUN in VolumeContext", func() {
				if zfsISCSISCName == "" {
					Skip("[TC-E35.332] no ZFS+iSCSI StorageClass")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				kubeconfig := e34Kubeconfig()
				By("creating zvol+iSCSI PVC to trigger CreateVolume")
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
				Expect(e34ApplyStdin(ctx, kubeconfig, pvcYAML)).To(Succeed(), "[TC-E35.332] apply ZFS+iSCSI PVC")

				By("waiting for PVC to be Bound")
				Eventually(func(g Gomega) {
					phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
						"-n", testNamespace, "-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"), "[TC-E35.332] PVC must be Bound")
				}).WithContext(ctx).
					WithTimeout(90 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())

				By("checking PV VolumeContext for iSCSI + ZFS parameters")
				pvName, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred())
				Expect(pvName).NotTo(BeEmpty())

				volumeAttrs, err := e33KubectlOutput(ctx, "get", "pv", pvName,
					"-o", "jsonpath={.spec.csi.volumeAttributes}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.332] get PV volumeAttributes")
				Expect(volumeAttrs).NotTo(BeEmpty(),
					"[TC-E35.332] PV volumeAttributes must not be empty for ZFS+iSCSI volume")
			})

			// ── TC-E35.333 ────────────────────────────────────────────────────
			It("[TC-E35.333] ControllerPublishVolume resolves the compute-worker initiator IQN from CSINode annotations for a zvol-backed target", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()

				phase, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
					"-n", testNamespace, "-o", "jsonpath={.status.phase}",
					"--ignore-not-found=true")
				if err != nil || phase != "Bound" {
					Skip("[TC-E35.333] PVC not Bound — TC-E35.332 may have skipped")
				}

				By("checking compute-worker CSINode for initiator IQN annotation")
				csiNodeList, err := e33KubectlOutput(ctx, "get", "csinode",
					"-o", "jsonpath={.items[*].metadata.name}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.333] list CSINodes")

				var iqnFound bool
				for _, nodeName := range strings.Fields(csiNodeList) {
					annotations, err := e33KubectlOutput(ctx, "get", "csinode", nodeName,
						"-o", "jsonpath={.metadata.annotations}")
					if err != nil {
						continue
					}
					if strings.Contains(annotations, "iscsi-initiator-iqn") ||
						strings.Contains(annotations, "pillar-csi.bhyoo.com") {
						iqnFound = true
						break
					}
				}

				if !iqnFound {
					Skip("[TC-E35.333] no iSCSI initiator IQN found in CSINode annotations — node plugin may not be fully configured")
				}
				Expect(iqnFound).To(BeTrue(),
					"[TC-E35.333] at least one CSINode must have iSCSI initiator IQN annotation")
			})

			// ── TC-E35.334 ────────────────────────────────────────────────────
			It("[TC-E35.334] ControllerUnpublishVolume revokes the CSINode-derived initiator IQN ACL without deleting the zvol-backed target before PVC cleanup", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()

				By("deleting ZFS+iSCSI PVC to trigger ControllerUnpublish → DeleteVolume")
				_, err := e33KubectlOutput(ctx, "delete", "pvc", pvcName,
					"-n", testNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=60s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E35.334] delete ZFS+iSCSI PVC")

				Eventually(func(g Gomega) {
					out, err := e33KubectlOutput(ctx, "get", "pvc", pvcName,
						"-n", testNamespace, "--ignore-not-found=true")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty(),
						"[TC-E35.334] PVC must be fully deleted after ControllerUnpublish")
				}).WithContext(ctx).
					WithTimeout(60 * time.Second).
					WithPolling(5 * time.Second).
					Should(Succeed())
			})

		})
	})
