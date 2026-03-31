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

// external_agent_zfs_functional_test.go — ZFS functional e2e tests for the
// external (out-of-cluster) agent mode.
//
// This file extends the external-agent e2e test suite with ZFS-dependent
// functional tests that mirror the groups in internal_agent_functional_test.go
// but use an external PillarTarget (spec.external) instead of a NodeRef target:
//
//  1. ExternalAgentZFSExpansion — volume expansion via ControllerExpandVolume.
//     Creates a complete CR stack (Target → Pool → Protocol → Binding with
//     AllowVolumeExpansion), provisions a 1 GiB PVC, then expands it to 2 GiB
//     and verifies that the resizer sidecar delivered the ExpandVolume RPC to
//     the agent.
//
//  2. ExternalAgentZFSMount — full mount/unmount lifecycle.
//     Creates a CR stack, provisions a PVC, manually configures a real kernel
//     NVMe-oF TCP target inside the external agent container (which shares the
//     host kernel), schedules a Pod on a compute-worker node, and verifies that
//     it reaches the Running phase (NodeStage + NodePublish succeeded).
//     Cleanup exercises NodeUnpublish + NodeUnstage + DeleteVolume.
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true — TestMain starts the privileged agent
//     container; docker exec into it is used for the NVMe-oF target setup.
//     Alternatively, set E2E_EXTERNAL_AGENT_CONTAINER_NAME to the name of a
//     pre-existing privileged agent container (used together with EXTERNAL_AGENT_ADDR).
//   - The ZFS pool (PILLAR_E2E_ZFS_POOL) must be set by TestMain.setupZFSPool.
//   - For ExternalAgentZFSMount: the Kind cluster must have a compute-worker
//     node with nvme_tcp and nvme_fabrics kernel modules loaded, and a
//     storage-worker-labelled node for PillarTarget selection.
//
// # Mode guards
//
// All specs in this file are conditionally registered — they appear in the
// Ginkgo tree ONLY when isExternalAgentMode() is true, keeping the skip count
// at zero in other configurations.  PILLAR_E2E_ZFS_POOL is set by TestMain
// after init, so it is NOT checked at registration time; BeforeAll guards
// check testEnv.zfsHostExec instead.
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// ExternalAgentZFSExpansion — volume expansion in external-agent mode
// ─────────────────────────────────────────────────────────────────────────────

// The volume expansion test creates a CR stack using the external agent,
// provisions a 1 GiB PVC, and verifies that a resize request to 2 GiB is
// reflected after the CSI resizer sidecar calls ControllerExpandVolume.
//
// No NVMe-oF initiator-side steps are needed: ControllerExpandVolume only
// calls agent.ExpandVolume (backend resize) and does not involve NodeStageVolume
// or `nvme connect`.
var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ExternalAgentZFSExpansion", Ordered, Label("external-agent", "zfs"), func() {
		var (
			k8sClient   client.Client
			target      *v1alpha1.PillarTarget
			pool        *v1alpha1.PillarPool
			protocol    *v1alpha1.PillarProtocol
			binding     *v1alpha1.PillarBinding
			pvc         *corev1.PersistentVolumeClaim
			testNS      *corev1.Namespace
			bindingName string
		)

		// ── BeforeAll: cluster client + CR stack + PVC ───────────────────────────

		BeforeAll(func(ctx SpecContext) {
			// Guard: ZFS host exec helper must be available (set by setupZFSPool in TestMain).
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ExternalAgentZFSExpansion: ZFS host-exec helper must be available — "+
					"setupZFSPool() must have succeeded")

			var err error
			suite, err := framework.SetupSuite(
				framework.WithConnectTimeout(iatConnectTimeout),
			)
			Expect(err).NotTo(HaveOccurred(),
				"ExternalAgentZFSExpansion: connect to Kind cluster — "+
					"KUBECONFIG must be set by TestMain")
			k8sClient = suite.Client

			zfsPool := iatZFSPool()
			agentAddr := extAgentClusterAddress()
			if agentAddr == "" {
				agentAddr = testEnv.ExternalAgentAddr
			}

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := fmt.Sprintf("ea-exp-target-%s", crSuffix)
			poolName := fmt.Sprintf("ea-exp-pool-%s", crSuffix)
			protoName := fmt.Sprintf("ea-exp-proto-%s", crSuffix)
			bindingName = fmt.Sprintf("ea-exp-binding-%s", crSuffix)

			// Build and apply the full CR stack with AllowVolumeExpansion=true.
			// Use Eventually to handle transient REST-mapper cache misses on a
			// freshly bootstrapped Kind cluster.
			Eventually(func(g Gomega) {
				t := framework.KindExternalTarget(targetName, agentAddr)
				g.Expect(framework.Apply(ctx, k8sClient, t)).To(Succeed(),
					"apply PillarTarget %q (external, addr=%s)", targetName, agentAddr)
				target = t

				p := framework.KindZFSZvolPool(poolName, targetName, zfsPool)
				g.Expect(framework.Apply(ctx, k8sClient, p)).To(Succeed(),
					"apply PillarPool %q (zfs-zvol, pool=%s)", poolName, zfsPool)
				pool = p

				proto := framework.KindNVMeOFTCPProtocol(protoName)
				g.Expect(framework.Apply(ctx, k8sClient, proto)).To(Succeed(),
					"apply PillarProtocol %q (nvmeof-tcp)", protoName)
				protocol = proto

				allowExpansion := true
				b := &v1alpha1.PillarBinding{
					ObjectMeta: metav1.ObjectMeta{Name: bindingName},
					Spec: v1alpha1.PillarBindingSpec{
						PoolRef:     poolName,
						ProtocolRef: protoName,
						StorageClass: v1alpha1.StorageClassTemplate{
							AllowVolumeExpansion: &allowExpansion,
						},
					},
				}
				g.Expect(framework.Apply(ctx, k8sClient, b)).To(Succeed(),
					"apply PillarBinding %q (allowVolumeExpansion=true)", bindingName)
				binding = b
			}, 60*time.Second, 5*time.Second).Should(Succeed(),
				"ExternalAgentZFSExpansion: API server did not accept all four CRs within 60 s")

			By(fmt.Sprintf("waiting for PillarBinding %q to be Ready", bindingName))
			Expect(framework.WaitForReady(ctx, k8sClient, binding, iatConditionTimeout)).To(Succeed(),
				"PillarBinding must be Ready (StorageClass created) before the PVC can be provisioned")

			// Create an isolated test namespace + a 1 GiB PVC.
			testNS, err = framework.CreateTestNamespace(ctx, k8sClient, "ea-exp")
			Expect(err).NotTo(HaveOccurred(), "create test namespace for expansion specs")

			pvc = framework.NewPillarPVC("ea-exp-vol", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc)).To(Succeed(),
				"create PVC %q/%q against StorageClass %q", testNS.Name, pvc.Name, bindingName)

			Expect(framework.WaitForPVCBound(ctx, k8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
				"PVC must be Bound before the expansion test can run — verify that the "+
					"external agent is reachable and the ZFS pool %q is available", zfsPool)
			By(fmt.Sprintf("PVC %q/%q is Bound — ready for expansion test", testNS.Name, pvc.Name))

			// Register cleanup as AfterAll (DeferCleanup in BeforeAll) so that
			// cleanup runs after ALL specs complete, not after each spec.
			DeferCleanup(func(dctx SpecContext) {
				By("ExternalAgentZFSExpansion: cleaning up PVC, CRs, and namespace")
				if pvc != nil {
					_ = framework.EnsurePVCAndPVGone(dctx, k8sClient, pvc, iatCleanupTimeout)
				}
				for _, obj := range []client.Object{binding, protocol, pool, target} {
					if obj == nil {
						continue
					}
					if err := framework.EnsureGone(dctx, k8sClient, obj, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: ExternalAgentZFSExpansion cleanup %T %q: %v\n",
							obj, obj.GetName(), err)
					}
				}
				if testNS != nil {
					_ = framework.EnsureNamespaceGone(dctx, k8sClient, testNS.Name, iatCleanupTimeout)
				}
				suite.TeardownSuite()
			})
		})

		// ── It: expand PVC from 1 GiB to 2 GiB ──────────────────────────────────
		//
		// The CSI external-resizer sidecar detects the capacity delta and calls
		// ControllerExpandVolume on the pillar-csi controller plugin, which in
		// turn calls agent.ExpandVolume.  We verify that the expansion is
		// acknowledged (PVC spec.resources.requests reflects >= 2 GiB).

		It("PVC resize request to 2Gi is reflected after ControllerExpandVolume", func(ctx SpecContext) {
			By("fetching current PVC state for its resource version")
			current := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), current)).To(Succeed(),
				"PVC %q/%q must exist", testNS.Name, pvc.Name)

			By("updating PVC storage request from 1Gi to 2Gi")
			current.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
			Expect(k8sClient.Update(ctx, current)).To(Succeed(),
				"update PVC storage request to 2Gi — "+
					"triggers CSI ControllerExpandVolume via the external-resizer sidecar")

			By(fmt.Sprintf("waiting for PVC %q/%q storage request >= 2Gi (up to %s)",
				testNS.Name, pvc.Name, iatProvisioningTimeout))
			Eventually(func(g Gomega) {
				updated := &corev1.PersistentVolumeClaim{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), updated)).To(Succeed(),
					"PVC %q/%q must still exist during expansion poll", testNS.Name, pvc.Name)
				actual := updated.Spec.Resources.Requests[corev1.ResourceStorage]
				requested := resource.MustParse("2Gi")
				g.Expect(actual.Cmp(requested)).To(BeNumerically(">=", 0),
					"PVC storage request must be >= 2Gi after expansion (current: %s)",
					actual.String())
			}, iatProvisioningTimeout, 5*time.Second).Should(Succeed(),
				"PVC %q/%q expansion to 2Gi was not reflected within the timeout — "+
					"check the external-resizer sidecar logs and agent ExpandVolume RPC; "+
					"agent addr: %s, ZFS pool: %q",
				testNS.Name, pvc.Name, testEnv.ExternalAgentAddr, iatZFSPool())

			By(fmt.Sprintf("PVC %q/%q expansion confirmed: ExpandVolume was called on the external agent",
				testNS.Name, pvc.Name))
		})
	}) // end Describe("ExternalAgentZFSExpansion")
	return true
}()

// ─────────────────────────────────────────────────────────────────────────────
// ExternalAgentZFSMount — mount/unmount lifecycle in external-agent mode
// ─────────────────────────────────────────────────────────────────────────────

// The mount lifecycle test exercises the full NodeStage → NodePublish →
// NodeUnpublish → NodeUnstage → DeleteVolume path using the external agent.
//
// The pillar-agent container is started with --configfs-root=/tmp (fake
// configfs) so its ExportVolume RPC writes synthetic entries but does NOT
// create a real kernel NVMe-oF listener.  This test manually configures the
// real NVMe-oF TCP target by running the nvmet setup script via:
//
//	docker exec <externalAgentContainerName> sh -c <script>
//
// Running inside the external agent container's network namespace means the
// nvmet kernel module binds the TCP listener to the container's IP on the
// "kind" Docker bridge — the same IP stored in the PV's "address"
// volumeAttribute — so the compute-worker's NodeStageVolume `nvme connect`
// succeeds.
//
// # Skip conditions
//
// The test is registered only when ALL of the following hold at binary start:
//
//   - isExternalAgentMode() is true (EXTERNAL_AGENT_ADDR or E2E_LAUNCH_EXTERNAL_AGENT)
//   - docker-exec is possible: either E2E_LAUNCH_EXTERNAL_AGENT=true (TestMain
//     will start the agent container and its name is known) OR
//     E2E_EXTERNAL_AGENT_CONTAINER_NAME is set (pre-existing container name)
//
// At runtime (BeforeAll) a guard checks that testEnv.zfsHostExec is non-nil
// (the ZFS host-exec helper started successfully in setupZFSPool).  The ZFS
// pool name (PILLAR_E2E_ZFS_POOL) is set by TestMain after init, so it cannot
// be checked at registration time — the BeforeAll guard handles that case.
var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	// docker exec into the external agent container requires knowing the
	// container name.  Accept either:
	//   - E2E_LAUNCH_EXTERNAL_AGENT=true  → TestMain will start the container
	//     using the derived name "<KIND_CLUSTER>-agent".
	//   - E2E_EXTERNAL_AGENT_CONTAINER_NAME set → caller provides the name of
	//     a pre-existing agent container reachable via EXTERNAL_AGENT_ADDR.
	canDockerExec := os.Getenv("E2E_LAUNCH_EXTERNAL_AGENT") == "true" ||
		os.Getenv("E2E_EXTERNAL_AGENT_CONTAINER_NAME") != ""
	if !canDockerExec {
		return false
	}
	Describe("ExternalAgentZFSMount", Ordered, Label("external-agent", "zfs", "mount"), func() {
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
			computeNodeName string
		)

		// ── BeforeAll: cluster client + CR stack + PVC + NVMe-oF target ─────────

		BeforeAll(func(ctx SpecContext) {
			// Guard: ZFS host exec helper must be available for the bridge goroutine.
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ExternalAgentZFSMount: ZFS host-exec helper must be available — "+
					"setupZFSPool() must have succeeded")

			var err error
			suite, err := framework.SetupSuite(
				framework.WithConnectTimeout(iatConnectTimeout),
			)
			Expect(err).NotTo(HaveOccurred(),
				"ExternalAgentZFSMount: connect to Kind cluster — "+
					"KUBECONFIG must be set by TestMain")
			k8sClient = suite.Client

			zfsPool := iatZFSPool()
			agentAddr := extAgentClusterAddress()
			if agentAddr == "" {
				agentAddr = testEnv.ExternalAgentAddr
			}
			agentContainerName := externalAgentContainerName()

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := fmt.Sprintf("ea-mnt-target-%s", crSuffix)
			poolName := fmt.Sprintf("ea-mnt-pool-%s", crSuffix)
			protoName := fmt.Sprintf("ea-mnt-proto-%s", crSuffix)
			bindingName = fmt.Sprintf("ea-mnt-binding-%s", crSuffix)

			// Apply all four CRs.
			target = framework.KindExternalTarget(targetName, agentAddr)
			Expect(framework.Apply(ctx, k8sClient, target)).To(Succeed())

			pool = framework.KindZFSZvolPool(poolName, targetName, zfsPool)
			Expect(framework.Apply(ctx, k8sClient, pool)).To(Succeed())

			protocol = framework.KindNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, k8sClient, protocol)).To(Succeed())

			binding = framework.KindNVMeOFBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, k8sClient, binding)).To(Succeed())

			By(fmt.Sprintf("waiting for PillarBinding %q to be Ready", bindingName))
			Expect(framework.WaitForReady(ctx, k8sClient, binding, iatConditionTimeout)).To(Succeed(),
				"PillarBinding must be Ready before PVC provisioning")

			// Create isolated test namespace.
			testNS, err = framework.CreateTestNamespace(ctx, k8sClient, "ea-mnt")
			Expect(err).NotTo(HaveOccurred())

			// Label a compute-worker node for the test Pod's nodeSelector.
			// Skip control-plane and storage-worker nodes.
			{
				By("labelling compute-worker node for NVMe-oF initiator test pod")
				nodeList := &corev1.NodeList{}
				Expect(k8sClient.List(ctx, nodeList)).To(Succeed())
				for i := range nodeList.Items {
					n := &nodeList.Items[i]
					if _, ctrl := n.Labels["node-role.kubernetes.io/control-plane"]; ctrl {
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
				By(fmt.Sprintf("labelled compute-worker %q with %s=true", computeNodeName, iatComputeNodeLabel))

				// busybox is already pre-loaded into all Kind nodes by
				// buildAndLoadImages (setup_test.go Phase 3) via "kind load
				// docker-image".  No Docker Hub pull is needed here.
			}

			// Create and wait for the PVC to be Bound before setting up NVMe-oF.
			pvc = framework.NewPillarPVC("ea-mnt-vol", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc)).To(Succeed())

			// Bridge goroutine: propagate new ZFS zvol device nodes from the Docker
			// host into the external agent container.
			//
			// ZFS zvol block devices (e.g. /dev/zd*) appear on the Docker HOST's
			// devtmpfs but may not be visible inside the external agent container.
			// This goroutine polls the host and creates the device node inside the
			// external agent container via mknod so that the NVMe-oF nvmet setup
			// script can verify the device exists.
			bridgeCtx, bridgeCancel := context.WithCancel(ctx)
			defer bridgeCancel()
			go func() {
				knownZvols := make(map[string]bool)
				for {
					select {
					case <-bridgeCtx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
					if testEnv.zfsHostExec == nil {
						continue
					}
					res, resErr := testEnv.zfsHostExec.ExecOnHost(bridgeCtx,
						"zfs list -H -t volume -o name 2>/dev/null || true")
					if resErr != nil {
						continue
					}
					for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
						if line == "" || knownZvols[line] {
							continue
						}
						if !strings.HasPrefix(line, zfsPool+"/") {
							continue
						}
						knownZvols[line] = true
						_, _ = fmt.Fprintf(GinkgoWriter, "[ea-zvol-bridge] new zvol: %s\n", line)

						// Get the major:minor device numbers on the host.
						var parts []string
						for retry := 0; retry < 10; retry++ {
							statRes, _ := testEnv.zfsHostExec.ExecOnHost(bridgeCtx,
								fmt.Sprintf(
									"stat -L -c '%%t %%T' /dev/zvol/%s 2>/dev/null || true",
									line))
							pp := strings.Fields(strings.TrimSpace(statRes.Stdout))
							if len(pp) == 2 && (pp[0] != "0" || pp[1] != "0") {
								parts = pp
								break
							}
							select {
							case <-bridgeCtx.Done():
								return
							case <-time.After(500 * time.Millisecond):
							}
						}
						if len(parts) != 2 {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-zvol-bridge] cannot get major:minor for %s after retries\n", line)
							continue
						}
						major, errMaj := strconv.ParseInt(parts[0], 16, 64)
						minor, errMin := strconv.ParseInt(parts[1], 16, 64)
						if errMaj != nil || errMin != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-zvol-bridge] parse major/minor failed for %s: maj=%v min=%v\n",
								line, errMaj, errMin)
							continue
						}

						// Create the directory and block-device node inside the external
						// agent container so the nvmet setup script can find the device.
						zvolPath := "/dev/zvol/" + line
						poolDir := "/dev/zvol/" + strings.SplitN(line, "/", 2)[0]
						mknodScript := fmt.Sprintf(
							"mkdir -p %s && mknod %s b %d %d 2>/dev/null || true",
							poolDir, zvolPath, major, minor)
						// The agent image has /bin/busybox but no /bin/sh symlink.
						// Use "/bin/busybox sh" so mknod runs in the agent container's
						// full namespace set (including its network namespace and
						// devtmpfs) where the device node must appear.
						cmd := exec.CommandContext(bridgeCtx,
							"docker", "exec", agentContainerName,
							"/bin/busybox", "sh", "-c", mknodScript)
						cmd.Env = injectDockerHost(os.Environ())
						cmdOut, cmdErr := cmd.CombinedOutput()
						if cmdErr != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-zvol-bridge] mknod failed for %s: %v: %s\n",
								line, cmdErr, cmdOut)
						} else {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-zvol-bridge] created device node %s (major=%d minor=%d) in %s\n",
								zvolPath, major, minor, agentContainerName)
						}
					}
				}
			}()

			Expect(framework.WaitForPVCBound(ctx, k8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
				"PVC must be Bound before creating the mount-lifecycle test Pod")
			By(fmt.Sprintf("PVC %q/%q is Bound to PV %q", testNS.Name, pvc.Name, pvc.Spec.VolumeName))

			// ── NVMe-oF target setup ─────────────────────────────────────────────
			//
			// The external pillar-agent uses --configfs-root=/tmp (fake configfs) so
			// it never creates a real kernel NVMe-oF listener.  We read the PV's
			// volumeAttributes and set up the real kernel NVMe-oF TCP target by
			// running the nvmet setup script inside the external agent container:
			//
			//   docker exec <agentContainer> sh -c <nvmSetupScript>
			//
			// Running inside the container's network namespace means the kernel
			// nvmet module binds the TCP listener to the container's IP on the
			// "kind" Docker bridge — the same IP recorded in the PV's "address"
			// volumeAttribute — so the compute-worker's `nvme connect` succeeds.
			By("reading PV volumeAttributes to set up real NVMe-oF TCP target")
			nvmPV, pvErr := framework.GetBoundPV(ctx, k8sClient, pvc)
			Expect(pvErr).NotTo(HaveOccurred(), "GetBoundPV after PVC Bound")
			Expect(nvmPV.Spec.CSI).NotTo(BeNil(), "PV must have a CSI spec with volumeAttributes")

			nvmNQN := nvmPV.Spec.CSI.VolumeAttributes["target_id"]
			nvmPort := nvmPV.Spec.CSI.VolumeAttributes["port"]
			Expect(nvmNQN).NotTo(BeEmpty(), "PV must have target_id volumeAttribute (NQN)")
			Expect(nvmPort).NotTo(BeEmpty(), "PV must have port volumeAttribute (TCP port)")

			// Extract the agent volume ID: 4th slash-separated component of volumeHandle.
			// VolumeHandle format: "<target>/<proto>/<backend>/<agentVolID>"
			vhParts := strings.SplitN(nvmPV.Spec.CSI.VolumeHandle, "/", 4)
			Expect(len(vhParts)).To(Equal(4), "volumeHandle must have 4 slash-separated parts")
			nvmDevPath := "/dev/zvol/" + vhParts[3]

			By(fmt.Sprintf("configuring NVMe-oF TCP target: nqn=%s port=%s dev=%s",
				nvmNQN, nvmPort, nvmDevPath))

			// ── Ensure nvmet kernel modules are loaded ──────────────────────────
			// In external-agent mode the agent DaemonSet is disabled, so its
			// modprobe init container (which loads nvmet + nvmet_tcp on the host)
			// never runs.  Without these modules /sys/kernel/config/nvmet does not
			// exist and the setup script below fails.  Load them now via the
			// privileged zfsHostExec helper before configuring the target.
			// Attempt to load nvmet and nvmet_tcp modules.  On systems where
			// nvmet is compiled into the kernel (CONFIG_NVME_TARGET=y rather than
			// =m), modprobe exits 1 with "not found in directory" even though the
			// module is already active.  Use "|| true" so the command always exits 0,
			// and instead verify that /sys/kernel/config/nvmet exists as the
			// authoritative check.
			By("ensuring nvmet kernel module is active on Docker host")
			checkRes, checkErr := testEnv.zfsHostExec.ExecOnHost(ctx,
				"modprobe nvmet nvmet_tcp 2>/dev/null || true; test -d /sys/kernel/config/nvmet")
			Expect(checkErr).NotTo(HaveOccurred(),
				"ExternalAgentZFSMount: nvmet check via zfsHostExec failed")
			Expect(checkRes.ExitCode).To(BeZero(),
				"ExternalAgentZFSMount: /sys/kernel/config/nvmet not found on Docker host\n"+
					"Ensure the host kernel has NVMe-oF target support "+
					"(CONFIG_NVME_TARGET=y or =m, and /sys/kernel/config/nvmet exists).")
			By("nvmet active — /sys/kernel/config/nvmet exists on Docker host")

			// Set up the nvmet target inside the external agent container.
			// The container is --privileged so it can write to /sys/kernel/config.
			// Running via "docker exec <agentContainer>" places the process in the
			// container's network namespace (IP == external agent's kind bridge IP),
			// so nvmet binds the TCP listener on that IP — matching the PV "address".
			nvmSetupScript := fmt.Sprintf(`set -e
NVMET=/sys/kernel/config/nvmet
NQN='%s'
DEVPATH='%s'
TRSVCID='%s'
PORTID='%s'
mkdir -p "$NVMET/subsystems/$NQN"
echo 1 > "$NVMET/subsystems/$NQN/attr_allow_any_host"
mkdir -p "$NVMET/subsystems/$NQN/namespaces/1"
echo "$DEVPATH" > "$NVMET/subsystems/$NQN/namespaces/1/device_path"
# Wait up to 15 s for the zvol device node to be visible inside this container.
# The bridge goroutine in the test process polls the host every 500 ms and
# mknods the zvol block device here; there is a small race between PVC Bound
# and the first bridge poll cycle completing.
_w=0
while [ $_w -lt 30 ] && ! [ -b "$DEVPATH" ]; do
  sleep 0.5
  _w=$((_w+1))
done
[ -b "$DEVPATH" ] || { echo "device $DEVPATH not found after 15s" >&2; exit 1; }
echo 1 > "$NVMET/subsystems/$NQN/namespaces/1/enable"
# Recreate the port to ensure the TCP listener is active.
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
			// The agent image removes /bin/sh but retains /bin/busybox.  Run the
			// NVMe-oF target setup script via "docker exec … /bin/busybox sh -c"
			// so it executes in the agent container's full namespace set:
			//   - agent network namespace: nvmet TCP listener binds to the
			//     container's Kind bridge IP (matching the PV address attribute)
			//   - agent mount namespace: /sys/kernel/config is the real configfs
			//     (privileged containers get a read-write bind of /sys from host)
			setupOut, setupErr := captureOutput("docker", "exec", agentContainerName,
				"/bin/busybox", "sh", "-c", nvmSetupScript)
			Expect(setupErr).NotTo(HaveOccurred(),
				"NVMe-oF target setup in external agent container %q failed: %s\n"+
					"Ensure the agent container was started with --privileged and that\n"+
					"the kernel modules nvmet and nvmet_tcp are loaded on the Docker host.",
				agentContainerName, setupOut)
			By(fmt.Sprintf("NVMe-oF TCP target listening: nqn=%s port=%s", nvmNQN, nvmPort))

			// ── NVMe device-node bridge goroutine ────────────────────────────────
			// When NodeStageVolume on the compute-worker calls `nvme connect`, the
			// kernel creates NVMe block devices (e.g. /dev/nvme2n1) on the Docker
			// HOST devtmpfs.  That device is NOT automatically visible inside the
			// Kind compute-worker container.  This goroutine polls the host for new
			// nvmeXnY block devices and creates their nodes inside the compute-worker
			// container via mknod so that the pillar-node format-and-mount step
			// succeeds.
			nvmBridgeCtx, nvmBridgeCancel := context.WithCancel(context.Background())
			go func() {
				knownNvmeDevs := make(map[string]bool)
				for {
					select {
					case <-nvmBridgeCtx.Done():
						return
					case <-time.After(500 * time.Millisecond):
					}
					if testEnv.zfsHostExec == nil {
						continue
					}
					res, resErr := testEnv.zfsHostExec.ExecOnHost(nvmBridgeCtx,
						"ls /dev/nvme*n* 2>/dev/null || true")
					if resErr != nil {
						continue
					}
					for _, devPath := range strings.Fields(strings.TrimSpace(res.Stdout)) {
						devName := strings.TrimPrefix(devPath, "/dev/")
						if devName == "" || knownNvmeDevs[devName] {
							continue
						}
						statRes, _ := testEnv.zfsHostExec.ExecOnHost(nvmBridgeCtx,
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
							"docker", "exec", "--privileged", computeNodeName, "sh", "-c", mknodScript)
						cmd.Env = injectDockerHost(os.Environ())
						cmdOut, cmdErr := cmd.CombinedOutput()
						if cmdErr != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-nvme-bridge] mknod failed for %s: %v: %s\n",
								devName, cmdErr, cmdOut)
							delete(knownNvmeDevs, devName)
						} else {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ea-nvme-bridge] created device node /dev/%s (major=%d minor=%d) in %s\n",
								devName, major, minor, computeNodeName)
						}
					}
				}
			}()

			// Register NVMe-oF teardown FIRST (LIFO → runs LAST, after Pod/PVC gone).
			capturedNQN := nvmNQN
			capturedPortID := nvmPort
			capturedAgentContainer := agentContainerName // capture for DeferCleanup
			DeferCleanup(func(_ SpecContext) {
				By("ExternalAgentZFSMount: tearing down NVMe-oF TCP target configfs entries")
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
				_, _ = captureOutput("docker", "exec", capturedAgentContainer,
					"/bin/busybox", "sh", "-c", nvmCleanScript)
			})

			// Disconnect the NVMe initiator on the compute-worker (safety cleanup).
			// Registered before Pod/PVC cleanup (LIFO: runs after Pod deletion).
			capturedComputeNodeName := computeNodeName
			DeferCleanup(func(_ SpecContext) {
				if capturedComputeNodeName == "" || capturedNQN == "" {
					return
				}
				By("ExternalAgentZFSMount: disconnecting NVMe-oF initiator on compute-worker")
				disconnScript := fmt.Sprintf(
					"nvme disconnect -n '%s' 2>/dev/null || true", capturedNQN)
				_, _ = captureOutput("docker", "exec", capturedComputeNodeName,
					"sh", "-c", disconnScript)
			})

			// Register Pod → PVC → CRs → Namespace → node label cleanup.
			// Registered BEFORE bridge cancel (LIFO: runs AFTER bridge cancel).
			DeferCleanup(func(dctx SpecContext) {
				By("ExternalAgentZFSMount: cleaning up Pod, PVC, CRs, namespace, and node label")
				if pod != nil {
					_ = k8sClient.Delete(dctx, pod, client.GracePeriodSeconds(0))
					_ = framework.EnsureGone(dctx, k8sClient, pod, iatCleanupTimeout)
				}
				if pvc != nil {
					_ = framework.EnsurePVCAndPVGone(dctx, k8sClient, pvc, iatCleanupTimeout)
				}
				for _, obj := range []client.Object{binding, protocol, pool, target} {
					if obj == nil {
						continue
					}
					if err := framework.EnsureGone(dctx, k8sClient, obj, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: ExternalAgentZFSMount cleanup %T %q: %v\n",
							obj, obj.GetName(), err)
					}
				}
				if testNS != nil {
					_ = framework.EnsureNamespaceGone(dctx, k8sClient, testNS.Name, iatCleanupTimeout)
				}
				// Remove the compute-node label added for test pod scheduling.
				if computeNodeName != "" {
					var cn corev1.Node
					if err := k8sClient.Get(dctx, client.ObjectKey{Name: computeNodeName}, &cn); err == nil {
						delete(cn.Labels, iatComputeNodeLabel)
						_ = k8sClient.Update(dctx, &cn)
					}
				}
				suite.TeardownSuite()
			})

			// Cancel the NVMe device-node bridge goroutine.
			// Registered LAST → runs FIRST in LIFO cleanup order, stopping the
			// bridge before Pod deletion and nvmet teardown.
			DeferCleanup(func(_ SpecContext) {
				nvmBridgeCancel()
			})
		})

		// ── It: Pod mounts the PVC on the compute-worker ────────────────────────

		It("a Pod mounting the PVC starts Running on the compute-worker node", func(ctx SpecContext) {
			podName := fmt.Sprintf("ea-mnt-pod-%d", time.Now().UnixMilli()%100000)
			pod = iatBuildTestPod(podName, testNS.Name, pvc.Name)

			By(fmt.Sprintf("creating Pod %q/%q that mounts PVC %q", testNS.Name, podName, pvc.Name))
			Expect(k8sClient.Create(ctx, pod)).To(Succeed(),
				"create test Pod — triggers ControllerPublish + NodeStage + NodePublish")

			By(fmt.Sprintf("waiting for Pod %q/%q to reach Running phase (up to %s)",
				testNS.Name, podName, iatMountTimeout))
			Eventually(func(g Gomega) {
				current := &corev1.Pod{}
				g.Expect(k8sClient.Get(ctx,
					client.ObjectKey{Namespace: testNS.Name, Name: podName}, current)).To(Succeed())
				g.Expect(current.Status.Phase).To(Equal(corev1.PodRunning),
					"Pod must be Running after NVMe-oF connect + format + mount; "+
						"current phase: %s (ensure nvme_tcp and nvme_fabrics modules are "+
						"loaded on compute-worker %q)",
					current.Status.Phase, computeNodeName)
			}, iatMountTimeout, 5*time.Second).Should(Succeed(),
				"Pod %q/%q did not reach Running phase — "+
					"check pillar-node DaemonSet logs and NVMe-oF kernel module availability",
				testNS.Name, podName)

			By(fmt.Sprintf("Pod %q/%q is Running with PVC %q mounted", testNS.Name, podName, pvc.Name))
		})

		// ── It: Pod deletion triggers clean unmount ──────────────────────────────

		It("Pod deletion triggers clean unmount (NodeUnpublish + NodeUnstage + ControllerUnpublish)", func(ctx SpecContext) {
			Expect(pod).NotTo(BeNil(),
				"pod must have been created successfully in the previous spec")

			By(fmt.Sprintf("deleting Pod %q/%q to trigger unmount sequence", testNS.Name, pod.Name))
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed(),
				"delete Pod — triggers NodeUnpublish, NodeUnstage, ControllerUnpublish")

			By("waiting for Pod to be fully removed from the API server")
			Expect(framework.EnsureGone(ctx, k8sClient, pod, iatCleanupTimeout)).To(Succeed(),
				"Pod must be fully removed before PVC deletion is attempted")

			pod = nil // prevent double-delete in DeferCleanup
			By("Pod deleted: unmount sequence (NodeUnpublish + NodeUnstage + ControllerUnpublish) completed")
		})

		// ── It: PVC deletion triggers DeleteVolume on the agent ─────────────────

		It("PVC deletion after Pod removal triggers DeleteVolume on the agent", func(ctx SpecContext) {
			Expect(pvc).NotTo(BeNil(), "pvc must exist to be deleted")

			By(fmt.Sprintf("deleting PVC %q/%q", testNS.Name, pvc.Name))
			Expect(framework.EnsurePVCGone(ctx, k8sClient, pvc, iatCleanupTimeout)).To(Succeed(),
				"PVC deletion must complete — triggers ControllerUnpublish (if needed) "+
					"and DeleteVolume on the external agent (ZFS zvol destroyed)")

			pvc = nil // prevent double-delete in DeferCleanup
			By("PVC deleted: DeleteVolume completed and PV reclaimed")
		})
	}) // end Describe("ExternalAgentZFSMount")
	return true
}()
