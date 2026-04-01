//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// lvm_pvc_pod_mount_e2e_test.go — E2E tests for LVM-backed PVC provisioning
// and Pod volume mount in internal-agent (DaemonSet) mode.
//
// # Sub-AC 10b: LVM PVC creation and Pod mount
//
// This file exercises the full storage lifecycle against the LVM backend:
//
//  1. LVM PVC provisioning — a PillarTarget (NodeRef) + PillarPool (LVM thin or
//     linear) + PillarProtocol (NVMe-oF TCP) + PillarBinding CR stack is created.
//     The generated StorageClass is used to provision a PVC; the test verifies
//     the PVC becomes Bound and the PV properties are correct.
//
//  2. LVM mount/unmount lifecycle — a Pod is scheduled on the compute-worker
//     node (NVMe-oF initiator side) and the test verifies it reaches the Running
//     phase (NodeStage + NodePublish via NVMe-oF TCP succeeded).  Pod deletion
//     verifies the unmount path, and PVC deletion verifies DeleteVolume.
//
// # Key differences from the ZFS mount test
//
//   - Pool type: LVM thin-provisioned LV (lvcreate -V --thinpool) when
//     PILLAR_E2E_LVM_THIN_POOL is set; linear LV (lvcreate -L) otherwise.
//   - Device path: /dev/<vg>/<lv> (LVM symlink created by libdevmapper) vs
//     /dev/zvol/<pool>/<vol> for ZFS.
//   - No zvol bridge goroutine: LVM device-mapper entries appear in /dev/mapper
//     which is bind-mounted with Bidirectional propagation in kind-config.yaml,
//     so device nodes and symlinks are visible inside the Kind storage-worker
//     container without any additional mknod bridge.
//   - NVMe device bridge goroutine: still required (NVMe block devices created
//     by `nvme connect` on the Docker HOST are not automatically visible inside
//     the Kind compute-worker container — only /dev/nvme-fabrics is bind-mounted
//     there).
//
// # Prerequisites
//
//   - PILLAR_E2E_LVM_VG must be set (done by TestMain.setupLVMVG on success).
//   - PILLAR_E2E_LVM_THIN_POOL may be set for thin provisioning; if absent, the
//     test uses linear LV provisioning.
//   - The LVM VG (and optional thin pool) must exist on the storage-worker node.
//   - NVMe-oF TCP kernel modules (nvmet, nvmet_tcp, nvme_tcp, nvme_fabrics)
//     must be loaded on the relevant Kind nodes.
//
// # Sequential vs parallel with ZFS tests
//
// ZFS and LVM tests share the same Kind cluster, storage node, and agent
// DaemonSet pod.  They use different storage backends (distinct ZFS pool vs LVM
// VG) but share the NVMe-oF protocol stack on the same node.  Running them in
// parallel risks NVMe-oF port/configfs conflicts.  Ginkgo's default sequential
// execution within a single suite guarantees ordering without any explicit
// serialisation — ZFS and LVM spec groups simply execute one after the other.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Environment variable helpers
// ─────────────────────────────────────────────────────────────────────────────

// lvmVGName returns the LVM Volume Group name from the PILLAR_E2E_LVM_VG
// environment variable.  Returns "" when unset; callers should Skip in that
// case.  Exported from env (not testEnv.LVMVGName) so it is safe to call from
// both spec-registration guards and BeforeAll closures.
func lvmVGName() string {
	return os.Getenv("PILLAR_E2E_LVM_VG")
}

// lvmThinPoolName returns the LVM thin pool LV name from PILLAR_E2E_LVM_THIN_POOL.
// Returns "" when the variable is not set, indicating linear provisioning mode.
func lvmThinPoolName() string {
	return os.Getenv("PILLAR_E2E_LVM_THIN_POOL")
}

// lvmBuildPool creates a PillarPool CR for LVM, choosing thin or linear mode
// based on whether a thin pool name is available.
//
// Parameters:
//
//	name      — Kubernetes name of the PillarPool CR
//	targetRef — name of the PillarTarget this pool lives on
func lvmBuildPool(name, targetRef string) *v1alpha1.PillarPool {
	vg := lvmVGName()
	pool := lvmThinPoolName()
	if pool != "" {
		return framework.KindLVMThinPool(name, targetRef, vg, pool)
	}
	return framework.KindLVMLinearPool(name, targetRef, vg)
}

// ─────────────────────────────────────────────────────────────────────────────
// LVM PVC provisioning — Group 1
// ─────────────────────────────────────────────────────────────────────────────
//
// These specs verify that the full CR stack (PillarTarget → PillarPool [LVM] →
// PillarProtocol → PillarBinding) reconciles correctly when a real LVM VG is
// available on the storage-worker node, and that PVCs can be dynamically
// provisioned.
//
// Gated on PILLAR_E2E_LVM_VG.

var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}
	Describe("LVM PVC provisioning", Ordered, Label("internal-agent", "lvm"), func() {
		var (
			k8sClient   client.Client
			target      *v1alpha1.PillarTarget
			pool        *v1alpha1.PillarPool
			protocol    *v1alpha1.PillarProtocol
			binding     *v1alpha1.PillarBinding
			pvc         *corev1.PersistentVolumeClaim
			pvc2        *corev1.PersistentVolumeClaim
			testNS      *corev1.Namespace
			bindingName string
			vgName      string
		)

		BeforeAll(func(ctx context.Context) {
			reapplyStorageNodeLabel()
			vgName = lvmVGName()
			if vgName == "" {
				Skip("PILLAR_E2E_LVM_VG not set — skipping LVM PVC provisioning tests " +
					"(set to the LVM Volume Group name on the storage-worker node, e.g. 'e2e-vg')")
			}

			By("connecting to the Kind cluster")
			suite, err := framework.SetupSuite(framework.WithConnectTimeout(iatConnectTimeout))
			Expect(err).NotTo(HaveOccurred(),
				"LVM PVC provisioning: cluster connectivity check failed — "+
					"ensure KUBECONFIG is set and TestMain has bootstrapped the cluster")
			k8sClient = suite.Client

			storageNode := iatResolveStorageNode(ctx, k8sClient)
			Expect(storageNode).NotTo(BeEmpty(),
				"LVM PVC provisioning: no storage-worker node found "+
					"(expected label %s=true)", iatStorageNodeLabel)
			By(fmt.Sprintf("storage-worker node: %s  lvm-vg: %s  thin-pool: %q",
				storageNode, vgName, lvmThinPoolName()))

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := fmt.Sprintf("lvm-prov-target-%s", crSuffix)
			poolName := fmt.Sprintf("lvm-prov-pool-%s", crSuffix)
			protoName := fmt.Sprintf("lvm-prov-proto-%s", crSuffix)
			bindingName = fmt.Sprintf("lvm-prov-binding-%s", crSuffix)

			// Apply all four CRs with retry to handle transient REST-mapper cache
			// misses that can occur briefly after Helm install.
			Eventually(func(g Gomega) {
				t := framework.NewNodeRefPillarTarget(targetName, storageNode, nil)
				g.Expect(framework.Apply(ctx, k8sClient, t)).To(Succeed(),
					"apply PillarTarget %q (node %s)", targetName, storageNode)
				target = t

				p := lvmBuildPool(poolName, targetName)
				g.Expect(framework.Apply(ctx, k8sClient, p)).To(Succeed(),
					"apply PillarPool %q (lvm vg=%s)", poolName, vgName)
				pool = p

				proto := framework.KindNVMeOFTCPProtocol(protoName)
				g.Expect(framework.Apply(ctx, k8sClient, proto)).To(Succeed(),
					"apply PillarProtocol %q (nvmeof-tcp)", protoName)
				protocol = proto

				b := framework.NewSimplePillarBinding(bindingName, poolName, protoName)
				g.Expect(framework.Apply(ctx, k8sClient, b)).To(Succeed(),
					"apply PillarBinding %q", bindingName)
				binding = b
			}, 60*time.Second, 5*time.Second).Should(Succeed(),
				"LVM PVC provisioning CR stack: API server did not accept all CRs within 60 s")

			// Wait for binding (and generated StorageClass) to be Ready.
			By(fmt.Sprintf("waiting for PillarBinding %q to be Ready", bindingName))
			Expect(framework.WaitForReady(ctx, k8sClient, binding, iatConditionTimeout)).To(Succeed(),
				"PillarBinding must reach Ready=True before PVC provisioning can start")

			// Create an isolated test namespace.
			testNS, err = framework.CreateTestNamespace(ctx, k8sClient, "lvm-prov")
			Expect(err).NotTo(HaveOccurred(), "create test namespace for LVM provisioning specs")

			// Create first PVC (1 Gi).
			pvc = framework.NewPillarPVC("lvm-vol-1", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc)).To(Succeed(),
				"create first PVC against StorageClass %q", bindingName)

			// Create second PVC (2 Gi) for independent-provisioning check.
			pvc2 = framework.NewPillarPVC("lvm-vol-2", testNS.Name, bindingName,
				resource.MustParse("2Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc2)).To(Succeed(),
				"create second PVC against StorageClass %q", bindingName)

			// Register cleanup: PVCs → CRs → namespace.
			DeferCleanup(func(dctx context.Context) {
				By("LVM PVC provisioning: cleaning up PVCs, CRs, and namespace")
				for _, p := range []*corev1.PersistentVolumeClaim{pvc, pvc2} {
					if p == nil {
						continue
					}
					if err := framework.EnsurePVCAndPVGone(dctx, k8sClient, p, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: cleanup PVC %q/%q: %v\n", p.Namespace, p.Name, err)
					}
				}
				for _, obj := range []client.Object{binding, protocol, pool, target} {
					if obj == nil {
						continue
					}
					if err := framework.EnsureGone(dctx, k8sClient, obj, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				if testNS != nil {
					if err := framework.EnsureNamespaceGone(dctx, k8sClient, testNS.Name, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: cleanup namespace %q: %v\n", testNS.Name, err)
					}
				}
				suite.TeardownSuite()
			})
		})

		// ── PillarPool conditions ──────────────────────────────────────────────────

		It("PillarPool BackendSupported condition becomes True (agent advertises lvm-lv)", func(ctx context.Context) {
			err := framework.WaitForCondition(ctx, k8sClient, pool,
				"BackendSupported", metav1.ConditionTrue, iatConditionTimeout)
			Expect(err).NotTo(HaveOccurred(),
				"PillarPool BackendSupported must be True — the agent DaemonSet must "+
					"advertise the lvm-lv backend type in GetCapabilities")
		})

		It("PillarPool PoolDiscovered condition becomes True (VG is visible to agent)", func(ctx context.Context) {
			err := framework.WaitForCondition(ctx, k8sClient, pool,
				"PoolDiscovered", metav1.ConditionTrue, iatConditionTimeout)
			Expect(err).NotTo(HaveOccurred(),
				"PoolDiscovered must be True — LVM VG %q must exist on the storage-worker node "+
					"(check that setupLVMVG succeeded and the VG is visible via /dev/mapper inside "+
					"the Kind storage-worker container)", vgName)
		})

		It("PillarPool reaches Ready=True and reports capacity", func(ctx context.Context) {
			Expect(framework.WaitForReady(ctx, k8sClient, pool, iatConditionTimeout)).To(Succeed(),
				"PillarPool must reach Ready=True once all conditions are satisfied")

			capErr := framework.WaitForField(ctx, k8sClient, pool,
				func(p *v1alpha1.PillarPool) bool {
					return p.Status.Capacity != nil &&
						p.Status.Capacity.Total != nil &&
						!p.Status.Capacity.Total.IsZero()
				}, iatConditionTimeout)
			Expect(capErr).NotTo(HaveOccurred(),
				"PillarPool status.capacity.total must be non-zero once the LVM VG is discovered")
			By(fmt.Sprintf("pool %q capacity: total=%s", pool.Name, pool.Status.Capacity.Total.String()))
		})

		It("PillarBinding generates a Kubernetes StorageClass with the pillar-csi provisioner", func(ctx context.Context) {
			sc := &storagev1.StorageClass{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: bindingName}, sc)).To(Succeed(),
				"StorageClass %q must exist (created by the PillarBinding controller)", bindingName)
			Expect(sc.Provisioner).To(Equal(iatCSIProvisioner),
				"StorageClass must use the pillar-csi.bhyoo.com provisioner")
			By(fmt.Sprintf("StorageClass %q created with provisioner %s", sc.Name, sc.Provisioner))
		})

		// ── PVC provisioning ──────────────────────────────────────────────────────

		It("first PVC (1Gi) becomes Bound via LVM CreateVolume", func(ctx context.Context) {
			By(fmt.Sprintf("waiting for PVC %q/%q to be Bound (up to %s)",
				testNS.Name, pvc.Name, iatProvisioningTimeout))
			err := framework.WaitForPVCBound(ctx, k8sClient, pvc, iatProvisioningTimeout)
			Expect(err).NotTo(HaveOccurred(),
				"PVC must be Bound — the pillar-csi controller must have called the "+
					"in-cluster agent's CreateVolume RPC against LVM VG %q", vgName)
			By(fmt.Sprintf("PVC %q/%q is Bound to PV %q",
				testNS.Name, pvc.Name, pvc.Spec.VolumeName))
		})

		It("bound PV (first PVC) has capacity >= 1Gi", func(ctx context.Context) {
			pv, err := framework.GetBoundPV(ctx, k8sClient, pvc)
			Expect(err).NotTo(HaveOccurred(), "GetBoundPV must succeed after WaitForPVCBound")
			Expect(framework.AssertPVCapacity(pv, resource.MustParse("1Gi"))).To(Succeed(),
				"PV capacity must be >= 1Gi as requested")
		})

		It("bound PV (first PVC) references the correct StorageClass", func(ctx context.Context) {
			pv, err := framework.GetBoundPV(ctx, k8sClient, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(framework.AssertPVStorageClass(pv, bindingName)).To(Succeed(),
				"PV StorageClass must equal the PillarBinding name %q", bindingName)
		})

		It("bound PV (first PVC) uses the Delete reclaim policy", func(ctx context.Context) {
			pv, err := framework.GetBoundPV(ctx, k8sClient, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(framework.AssertPVReclaimPolicy(pv, corev1.PersistentVolumeReclaimDelete)).To(Succeed(),
				"default PillarBinding reclaim policy is Delete")
		})

		It("second PVC (2Gi) is independently provisioned and Bound", func(ctx context.Context) {
			By(fmt.Sprintf("waiting for second PVC %q/%q to be Bound", testNS.Name, pvc2.Name))
			Expect(framework.WaitForPVCBound(ctx, k8sClient, pvc2, iatProvisioningTimeout)).To(Succeed(),
				"second PVC must be provisioned independently (agent handles concurrent "+
					"lvcreate calls; LV names are unique per PVC)")
			By(fmt.Sprintf("second PVC %q/%q is Bound to PV %q",
				testNS.Name, pvc2.Name, pvc2.Spec.VolumeName))

			Expect(pvc.Spec.VolumeName).NotTo(Equal(pvc2.Spec.VolumeName),
				"each PVC must be backed by a distinct PV (LVM LV names are unique per volume ID)")
		})
	}) // end Describe("LVM PVC provisioning")
	return true
}()

// ─────────────────────────────────────────────────────────────────────────────
// LVM mount/unmount lifecycle — Group 2
// ─────────────────────────────────────────────────────────────────────────────
//
// These specs exercise the full NodeStage → NodePublish → NodeUnpublish →
// NodeUnstage → DeleteVolume path using the LVM backend.
//
// The test manually configures the real NVMe-oF TCP kernel target on the
// storage-worker after PVC binding so that the compute-worker can issue
// `nvme connect` during NodeStageVolume.
//
// # No LVM device-node bridge goroutine
//
// For ZFS, a bridge goroutine polls the Docker host for new zvol device nodes
// and creates them inside the Kind storage-worker container via mknod.  For
// LVM, this is NOT needed:
//   - /dev/mapper is bind-mounted with Bidirectional propagation, so
//     device-mapper entries created by lvcreate inside the Kind container are
//     immediately visible there.
//   - libdevmapper also creates /dev/<vg>/<lv> symlinks inside the container
//     as part of the logical volume activation step.
//
// # NVMe device-node bridge goroutine (still required)
//
// The NVMe block device created by `nvme connect` on the Docker HOST (e.g.
// /dev/nvme2n1) is NOT automatically visible inside the Kind compute-worker
// container.  The bridge goroutine polls the host for new nvmeXnY devices and
// creates their device nodes in the compute-worker container via mknod, exactly
// as the ZFS mount test does.
//
// Gated on PILLAR_E2E_LVM_VG.

var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}
	Describe("LVM mount/unmount lifecycle", Ordered, Label("internal-agent", "lvm", "mount"), func() {
		var (
			k8sClient       client.Client
			target          *v1alpha1.PillarTarget
			pool            *v1alpha1.PillarPool
			protocol        *v1alpha1.PillarProtocol
			binding         *v1alpha1.PillarBinding
			pvc             *corev1.PersistentVolumeClaim
			pod             *corev1.Pod
			testNS          *corev1.Namespace
			bindingName     string
			vgName          string
			storageNodeName string
			computeNodeName string
		)

		BeforeAll(func(ctx context.Context) {
			reapplyStorageNodeLabel()
			vgName = lvmVGName()
			if vgName == "" {
				Skip("PILLAR_E2E_LVM_VG not set — skipping LVM mount/unmount lifecycle tests")
			}

			By("connecting to the Kind cluster")
			suite, err := framework.SetupSuite(framework.WithConnectTimeout(iatConnectTimeout))
			Expect(err).NotTo(HaveOccurred(),
				"LVM mount lifecycle: cluster connectivity check failed")
			k8sClient = suite.Client

			storageNodeName = iatResolveStorageNode(ctx, k8sClient)
			Expect(storageNodeName).NotTo(BeEmpty(),
				"LVM mount lifecycle: no storage-worker node found "+
					"(expected label %s=true)", iatStorageNodeLabel)
			By(fmt.Sprintf("storage-worker: %s  lvm-vg: %s  thin-pool: %q",
				storageNodeName, vgName, lvmThinPoolName()))

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := fmt.Sprintf("lvm-mnt-target-%s", crSuffix)
			poolName := fmt.Sprintf("lvm-mnt-pool-%s", crSuffix)
			protoName := fmt.Sprintf("lvm-mnt-proto-%s", crSuffix)
			bindingName = fmt.Sprintf("lvm-mnt-binding-%s", crSuffix)

			// Apply CR stack.
			target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
			Expect(framework.Apply(ctx, k8sClient, target)).To(Succeed())

			pool = lvmBuildPool(poolName, targetName)
			Expect(framework.Apply(ctx, k8sClient, pool)).To(Succeed())

			protocol = framework.KindNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, k8sClient, protocol)).To(Succeed())

			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, k8sClient, binding)).To(Succeed())

			By(fmt.Sprintf("waiting for PillarBinding %q to be Ready", bindingName))
			Expect(framework.WaitForReady(ctx, k8sClient, binding, iatConditionTimeout)).To(Succeed(),
				"PillarBinding must be Ready before PVC provisioning")

			// Create isolated test namespace.
			testNS, err = framework.CreateTestNamespace(ctx, k8sClient, "lvm-mnt")
			Expect(err).NotTo(HaveOccurred())

			// Label the compute-worker node (non-storage worker) for test Pod scheduling.
			{
				By("labelling compute-worker node for NVMe-oF initiator test pod")
				nodeList := &corev1.NodeList{}
				Expect(k8sClient.List(ctx, nodeList)).To(Succeed())
				for i := range nodeList.Items {
					n := &nodeList.Items[i]
					if _, isCtrl := n.Labels["node-role.kubernetes.io/control-plane"]; isCtrl {
						continue
					}
					if n.Labels[iatStorageNodeLabel] == "true" {
						continue
					}
					computeNodeName = n.Name
					break
				}
				Expect(computeNodeName).NotTo(BeEmpty(),
					"a non-storage worker node must exist to run the NVMe-oF initiator test pod")
				var cn corev1.Node
				Expect(k8sClient.Get(ctx, client.ObjectKey{Name: computeNodeName}, &cn)).To(Succeed())
				if cn.Labels == nil {
					cn.Labels = make(map[string]string)
				}
				cn.Labels[iatComputeNodeLabel] = "true"
				Expect(k8sClient.Update(ctx, &cn)).To(Succeed())
				By(fmt.Sprintf("labelled compute-worker %q with %s=true",
					computeNodeName, iatComputeNodeLabel))
			}

			// Create a 1 Gi PVC and wait for it to be Bound.
			//
			// Unlike ZFS, there is no bridge goroutine needed for the LVM device node:
			// /dev/mapper is bind-mounted with Bidirectional propagation, so the
			// device-mapper entry created by lvcreate inside the Kind container is
			// immediately visible there.  libdevmapper also creates the /dev/<vg>/<lv>
			// symlink inside the container during LV activation.
			pvc = framework.NewPillarPVC("lvm-mnt-vol", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc)).To(Succeed())

			Expect(framework.WaitForPVCBound(ctx, k8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
				"PVC must be Bound before creating the NVMe-oF target and the test Pod; "+
					"verify the agent DaemonSet is running on %q and the LVM VG %q is "+
					"accessible via /dev/mapper inside the container",
				storageNodeName, vgName)
			By(fmt.Sprintf("PVC %q/%q is Bound to PV %q",
				testNS.Name, pvc.Name, pvc.Spec.VolumeName))

			// ── Ensure NVMe-oF target kernel modules are loaded ───────────────────
			By("ensuring nvmet kernel modules are loaded on storage-worker")
			modprobeOut, modprobeErr := captureOutput("docker", "exec", storageNodeName,
				"sh", "-c", "modprobe nvmet nvmet_tcp 2>/dev/null || true; test -d /sys/kernel/config/nvmet")
			Expect(modprobeErr).NotTo(HaveOccurred(),
				"modprobe nvmet/nvmet_tcp failed on %s: /sys/kernel/config/nvmet not found — "+
					"host kernel must have NVMe-oF target support "+
					"(CONFIG_NVME_TARGET=y or =m). Output: %s",
				storageNodeName, modprobeOut)
			By("nvmet modules loaded — /sys/kernel/config/nvmet exists on storage-worker")

			// ── Set up real NVMe-oF TCP kernel target ─────────────────────────────
			//
			// The pillar-agent runs with --configfs-root=/tmp (fake configfs) so it
			// never starts a real kernel NVMe-oF listener.  After the PVC is Bound we
			// read the PV volumeAttributes and configure the real nvmet kernel target
			// on the storage-worker so the compute-worker's NodeStageVolume can
			// `nvme connect` successfully.
			By("reading PV volumeAttributes for NVMe-oF target setup")
			nvmPV, pvErr := framework.GetBoundPV(ctx, k8sClient, pvc)
			Expect(pvErr).NotTo(HaveOccurred(), "GetBoundPV after PVC Bound")
			Expect(nvmPV.Spec.CSI).NotTo(BeNil(),
				"PV must have a CSI spec with volumeAttributes (target_id, port)")

			nvmNQN := nvmPV.Spec.CSI.VolumeAttributes["target_id"]
			nvmPort := nvmPV.Spec.CSI.VolumeAttributes["port"]
			Expect(nvmNQN).NotTo(BeEmpty(), "PV must have target_id volumeAttribute (NQN)")
			Expect(nvmPort).NotTo(BeEmpty(), "PV must have port volumeAttribute (TCP port)")

			// Derive the LVM device path from the volumeHandle.
			// volumeHandle format: "<target>/<proto>/<backend>/<agentVolumeID>"
			// agentVolumeID for LVM: "<vg>/<lv-name>"
			// → device path: "/dev/<vg>/<lv-name>"
			//
			// libdevmapper creates this symlink inside the Kind container when
			// lvcreate activates the logical volume, so the path should exist at
			// the time the NVMe-oF target is being configured.
			vhParts := strings.SplitN(nvmPV.Spec.CSI.VolumeHandle, "/", 4)
			Expect(len(vhParts)).To(Equal(4),
				"LVM volumeHandle must have 4 slash-separated parts: <target>/<proto>/<backend>/<agentVolID>")
			nvmDevPath := "/dev/" + vhParts[3]

			By(fmt.Sprintf("configuring NVMe-oF TCP target: nqn=%s port=%s dev=%s",
				nvmNQN, nvmPort, nvmDevPath))

			// Set up the nvmet target inside the storage-worker Kind container.
			// The /dev/<vg>/<lv> path is a symlink to /dev/mapper/<vg>-<lv> created
			// by libdevmapper inside the container during LV activation.  If for any
			// reason the symlink is not yet visible (transient udev race), the script
			// falls back to the canonical /dev/mapper path.
			// Derive the expected DM name: hyphens in vg/lv names are doubled in
			// /dev/mapper.  Use shell arithmetic to construct the fallback path.
			nvmSetupScript := fmt.Sprintf(`set -e
NVMET=/sys/kernel/config/nvmet
NQN='%s'
DEVPATH='%s'
TRSVCID='%s'
PORTID='%s'

# Resolve actual device: prefer the LVM symlink, fall back to /dev/mapper.
# libdevmapper creates /dev/<vg>/<lv> when lvcreate activates the LV inside
# this container.  If it is not yet present (transient race), derive the
# device-mapper path by replacing each hyphen with two hyphens in the VG and
# LV names (LVM convention for /dev/mapper entries).
if [ ! -b "$DEVPATH" ]; then
  # Extract <vg>/<lv> from the path like /dev/e2e-vg/some-lv-uuid.
  VG_LV="${DEVPATH#/dev/}"
  VG_NAME="${VG_LV%%/*}"
  LV_NAME="${VG_LV#*/}"
  DM_VG="$(printf '%%s' "$VG_NAME" | sed 's/-/--/g')"
  DM_LV="$(printf '%%s' "$LV_NAME" | sed 's/-/--/g')"
  DEVPATH="/dev/mapper/${DM_VG}-${DM_LV}"
fi

# Wait up to 30 s for the device node to be visible (LV activation is async).
_w=0
while [ $_w -lt 60 ] && ! [ -b "$DEVPATH" ]; do
  sleep 0.5
  _w=$((_w+1))
done
[ -b "$DEVPATH" ] || { echo "LVM device $DEVPATH not found after 30s" >&2; exit 1; }

mkdir -p "$NVMET/subsystems/$NQN"
echo 1 > "$NVMET/subsystems/$NQN/attr_allow_any_host"
mkdir -p "$NVMET/subsystems/$NQN/namespaces/1"
echo "$DEVPATH" > "$NVMET/subsystems/$NQN/namespaces/1/device_path"
echo 1 > "$NVMET/subsystems/$NQN/namespaces/1/enable"
# Recreate the port to ensure the TCP listener is active.  A stale port
# from a previous run may exist in configfs with a dead TCP socket.
if [ -d "$NVMET/ports/$PORTID" ]; then
  for sub in "$NVMET/ports/$PORTID/subsystems/"*; do
    [ -L "$sub" ] && rm -f "$sub"
  done
  rmdir "$NVMET/ports/$PORTID" 2>/dev/null || true
fi
mkdir -p "$NVMET/ports/$PORTID"
echo tcp   > "$NVMET/ports/$PORTID/addr_trtype"
echo ipv4  > "$NVMET/ports/$PORTID/addr_adrfam"
echo 0.0.0.0 > "$NVMET/ports/$PORTID/addr_traddr"
echo "$TRSVCID" > "$NVMET/ports/$PORTID/addr_trsvcid"
test -L "$NVMET/ports/$PORTID/subsystems/$NQN" || \
  ln -s "$NVMET/subsystems/$NQN" "$NVMET/ports/$PORTID/subsystems/$NQN"
`, nvmNQN, nvmDevPath, nvmPort, nvmPort)
			setupOut, setupErr := captureOutput("docker", "exec", storageNodeName,
				"sh", "-c", nvmSetupScript)
			Expect(setupErr).NotTo(HaveOccurred(),
				"NVMe-oF target setup failed (LVM backend): %s", setupOut)
			By(fmt.Sprintf("NVMe-oF TCP target listening: nqn=%s port=%s dev=%s",
				nvmNQN, nvmPort, nvmDevPath))

			// ── NVMe device-node bridge goroutine ─────────────────────────────────
			//
			// When the pillar-node plugin calls NodeStageVolume on the compute-worker
			// it writes to /dev/nvme-fabrics and the kernel creates the block device
			// (e.g. /dev/nvme2n1) on the Docker HOST devtmpfs.  That device is NOT
			// visible inside the Kind compute-worker container (only /dev/nvme-fabrics
			// is bind-mounted there).  This goroutine polls the Docker host for new
			// nvmeXnY block devices and creates their device nodes inside the
			// compute-worker container via mknod.  This mirrors the ZFS mount test.
			nvmBridgeCtx, nvmBridgeCancel := context.WithCancel(context.Background())
			go func() {
				knownNvmeDevs := make(map[string]bool)
				for {
					select {
					case <-nvmBridgeCtx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
					if testEnv.lvmHostExec == nil {
						continue
					}
					res, resErr := testEnv.lvmHostExec.ExecOnHost(nvmBridgeCtx,
						"ls /dev/nvme*n* 2>/dev/null || true")
					if resErr != nil {
						continue
					}
					for _, devPath := range strings.Fields(strings.TrimSpace(res.Stdout)) {
						devName := strings.TrimPrefix(devPath, "/dev/")
						if devName == "" || knownNvmeDevs[devName] {
							continue
						}
						statRes, _ := testEnv.lvmHostExec.ExecOnHost(nvmBridgeCtx,
							fmt.Sprintf("stat -c '%%t %%T' %s 2>/dev/null || true", devPath))
						parts := strings.Fields(strings.TrimSpace(statRes.Stdout))
						if len(parts) != 2 {
							continue
						}
						major, errMaj := strconv.ParseInt(parts[0], 16, 64)
						minor, errMin := strconv.ParseInt(parts[1], 16, 64)
						if errMaj != nil || errMin != nil {
							continue
						}
						knownNvmeDevs[devName] = true
						mknodScript := fmt.Sprintf(
							"[ -e /dev/%s ] || mknod /dev/%s b %d %d",
							devName, devName, major, minor)
						cmd := exec.CommandContext(nvmBridgeCtx,
							"docker", "exec", "--privileged", computeNodeName,
							"sh", "-c", mknodScript)
						cmd.Env = injectDockerHost(os.Environ())
						cmdOut, cmdErr := cmd.CombinedOutput()
						if cmdErr != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[nvme-bridge] mknod failed for %s: %v: %s\n",
								devName, cmdErr, cmdOut)
							delete(knownNvmeDevs, devName)
						} else {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[nvme-bridge] created /dev/%s (major=%d minor=%d) in %s\n",
								devName, major, minor, computeNodeName)
						}
					}
				}
			}()

			// NVMe-oF target teardown — registered FIRST, runs LAST (LIFO).
			capturedNQN := nvmNQN
			capturedPortID := nvmPort
			DeferCleanup(func(_ context.Context) {
				By("tearing down NVMe-oF TCP target configfs entries (LVM)")
				nvmCleanScript := fmt.Sprintf(`
NVMET=/sys/kernel/config/nvmet
NQN='%s'
PORTID='%s'
rm -f  "$NVMET/ports/$PORTID/subsystems/$NQN" 2>/dev/null || true
echo 0 > "$NVMET/subsystems/$NQN/namespaces/1/enable" 2>/dev/null || true
rmdir  "$NVMET/subsystems/$NQN/namespaces/1" 2>/dev/null || true
rmdir  "$NVMET/subsystems/$NQN" 2>/dev/null || true
rmdir  "$NVMET/ports/$PORTID" 2>/dev/null || true
`, capturedNQN, capturedPortID)
				_, _ = captureOutput("docker", "exec", storageNodeName,
					"sh", "-c", nvmCleanScript)
			})

			// NVMe initiator disconnect — registered second, runs second-to-last.
			capturedComputeNodeName := computeNodeName
			DeferCleanup(func(_ context.Context) {
				if capturedComputeNodeName == "" || capturedNQN == "" {
					return
				}
				By("disconnecting NVMe-oF initiator on compute-worker (safety cleanup)")
				disconnScript := fmt.Sprintf(
					"nvme disconnect -n '%s' 2>/dev/null || true", capturedNQN)
				_, _ = captureOutput("docker", "exec", capturedComputeNodeName,
					"sh", "-c", disconnScript)
			})

			// Pod → PVC → CRs → namespace cleanup + compute-node label removal.
			DeferCleanup(func(dctx context.Context) {
				By("LVM mount lifecycle: cleaning up Pod, PVC, CRs, and namespace")
				if pod != nil {
					_ = k8sClient.Delete(dctx, pod, client.GracePeriodSeconds(0))
					_ = framework.EnsureGone(dctx, k8sClient, pod, iatCleanupTimeout)
				}
				if pvc != nil {
					_ = framework.EnsurePVCAndPVGone(dctx, k8sClient, pvc, iatCleanupTimeout)
				}
				for _, obj := range []client.Object{binding, protocol, pool, target} {
					if err := framework.EnsureGone(dctx, k8sClient, obj, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				if testNS != nil {
					_ = framework.EnsureNamespaceGone(dctx, k8sClient, testNS.Name, iatCleanupTimeout)
				}
				if capturedComputeNodeName != "" {
					var cn corev1.Node
					if getErr := k8sClient.Get(dctx,
						client.ObjectKey{Name: capturedComputeNodeName}, &cn); getErr == nil {
						delete(cn.Labels, iatComputeNodeLabel)
						_ = k8sClient.Update(dctx, &cn)
					}
				}
				suite.TeardownSuite()
			})

			// Cancel the NVMe bridge goroutine — registered LAST, runs FIRST (LIFO).
			DeferCleanup(func(_ context.Context) {
				nvmBridgeCancel()
			})
		})

		// ── It: Pod mounts LVM-backed PVC via NVMe-oF TCP ─────────────────────────

		It("a Pod mounting the LVM PVC starts Running on the compute-worker node", func(ctx context.Context) {
			podName := fmt.Sprintf("lvm-mnt-pod-%d", time.Now().UnixMilli()%100000)
			pod = iatBuildTestPod(podName, testNS.Name, pvc.Name)

			By(fmt.Sprintf("creating Pod %q/%q that mounts LVM PVC %q",
				testNS.Name, podName, pvc.Name))
			Expect(k8sClient.Create(ctx, pod)).To(Succeed(),
				"create test Pod — triggers ControllerPublish + NodeStage + NodePublish")

			By(fmt.Sprintf("waiting for Pod %q to reach Running phase (up to %s)",
				podName, iatMountTimeout))

			// Collect pillar-node logs periodically to aid diagnosis.
			logCtx, logCancel := context.WithCancel(ctx)
			defer logCancel()
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-logCtx.Done():
						return
					case <-ticker.C:
						nodeLogsOut, _ := captureOutput(
							"kubectl", "logs",
							"-l", "app.kubernetes.io/component=node",
							"-n", testEnv.HelmNamespace,
							"-c", "node",
							"--tail=40",
							"--prefix",
						)
						_, _ = fmt.Fprintf(GinkgoWriter,
							"[node-logs] pillar-node (last 40 lines):\n%s\n", nodeLogsOut)
						ctrlLogsOut, _ := captureOutput(
							"kubectl", "logs",
							"-l", "app.kubernetes.io/component=controller",
							"-n", testEnv.HelmNamespace,
							"--all-containers",
							"--tail=40",
							"--prefix",
						)
						_, _ = fmt.Fprintf(GinkgoWriter,
							"[ctrl-logs] controller+sidecars (last 40 lines):\n%s\n", ctrlLogsOut)
						evOut, _ := captureOutput(
							"kubectl", "get", "events",
							"-n", testNS.Name,
							"--sort-by=.lastTimestamp",
							"--field-selector", "involvedObject.name="+podName,
						)
						_, _ = fmt.Fprintf(GinkgoWriter, "[pod-events]\n%s\n", evOut)
					}
				}
			}()

			Eventually(func(g Gomega) {
				current := &corev1.Pod{}
				g.Expect(k8sClient.Get(ctx,
					client.ObjectKey{Name: podName, Namespace: testNS.Name}, current)).To(Succeed())
				var condStrs []string
				for _, c := range current.Status.Conditions {
					condStrs = append(condStrs, fmt.Sprintf("%s=%s", c.Type, c.Status))
				}
				_, _ = fmt.Fprintf(GinkgoWriter,
					"[pod-wait] phase=%s node=%s conditions=[%s]\n",
					current.Status.Phase, current.Spec.NodeName,
					strings.Join(condStrs, ","))
				for _, cs := range current.Status.ContainerStatuses {
					if cs.State.Waiting != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"[pod-wait] container %q waiting: reason=%s msg=%s\n",
							cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
					}
				}
				g.Expect(current.Status.Phase).To(Equal(corev1.PodRunning),
					"Pod must reach Running after NVMe-oF connect + format + mount "+
						"(LVM backend); current phase: %s",
					current.Status.Phase)
			}, iatMountTimeout, 5*time.Second).Should(Succeed(),
				"Pod %q/%q did not reach Running — check pillar-node logs, "+
					"NVMe-oF module availability on compute-worker, and LVM device "+
					"accessibility on storage-worker (%s, vg=%s)",
				testNS.Name, podName, storageNodeName, vgName)

			By(fmt.Sprintf("Pod %q/%q is Running with LVM PVC %q mounted via NVMe-oF TCP",
				testNS.Name, podName, pvc.Name))
		})

		// ── It: Pod deletion triggers clean unmount ────────────────────────────────

		It("Pod deletion triggers NodeUnpublish + NodeUnstage + ControllerUnpublish", func(ctx context.Context) {
			Expect(pod).NotTo(BeNil(),
				"pod must have been created successfully in the previous spec")

			By(fmt.Sprintf("deleting Pod %q/%q", testNS.Name, pod.Name))
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())

			By("waiting for Pod to be fully removed")
			Expect(framework.EnsureGone(ctx, k8sClient, pod, iatCleanupTimeout)).To(Succeed(),
				"Pod must be fully removed before PVC deletion")

			pod = nil // prevent double-delete in DeferCleanup
			By("Pod deleted: NodeUnpublish + NodeUnstage + ControllerUnpublish completed")
		})

		// ── It: PVC deletion triggers DeleteVolume (LVM LV removed) ──────────────

		It("PVC deletion after Pod removal triggers DeleteVolume (LV destroyed on agent)", func(ctx context.Context) {
			Expect(pvc).NotTo(BeNil(), "pvc must exist to be deleted")

			By(fmt.Sprintf("deleting PVC %q/%q", testNS.Name, pvc.Name))
			Expect(framework.EnsurePVCGone(ctx, k8sClient, pvc, iatCleanupTimeout)).To(Succeed(),
				"PVC deletion must complete — triggers ControllerUnpublish + "+
					"DeleteVolume (lvm LV is removed via lvremove on the agent)")

			pvc = nil // prevent double-delete in DeferCleanup
			By("PVC deleted: LVM logical volume destroyed and PV reclaimed")
		})
	}) // end Describe("LVM mount/unmount lifecycle")
	return true
}()
