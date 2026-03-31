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

// lvm_volume_expansion_e2e_test.go — E2E tests for LVM-backed PVC volume
// expansion with in-Pod filesystem resize verification.
//
// # Sub-AC 10c: LVM volume expansion
//
// This file implements a full end-to-end expansion test for the LVM backend,
// verifying that resizing a PVC triggers both the storage-layer resize (via
// agent.ExpandVolume) and the on-node filesystem resize (via node.NodeExpandVolume
// → resize2fs), and that the new capacity is visible inside a running Pod.
//
// # Test flow
//
//  1. Build the CR stack with AllowVolumeExpansion=true:
//     PillarTarget (NodeRef) → PillarPool (LVM thin or linear) →
//     PillarProtocol (NVMe-oF TCP) → PillarBinding.
//
//  2. Create a 1 Gi PVC against the generated StorageClass; wait for Bound.
//
//  3. Configure the real NVMe-oF TCP kernel target on the storage-worker so the
//     compute-worker can connect during NodeStageVolume.
//
//  4. Start the NVMe device-node bridge goroutine (reuse pattern from
//     lvm_pvc_pod_mount_e2e_test.go).
//
//  5. Create a test Pod that mounts the PVC at /data; wait for Running.
//
//  6. Capture the initial filesystem size via kubectl exec into the Pod
//     ("df --output=avail /data | tail -1").
//
//  7. Patch the PVC storage request from 1 Gi to 2 Gi; the CSI resizer sidecar
//     calls ControllerExpandVolume, which in turn calls agent.ExpandVolume.
//
//  8. Wait for:
//     a. PVC status.capacity.storage >= 2 Gi (controller-side expansion done).
//     b. PVC FileSystemResizePending condition absent or False (node-side resize done).
//
//  9. Verify the filesystem size inside the running Pod is >= 2 Gi via
//     kubectl exec ("df --output=avail /data | tail -1" >= 2 * 1024 * 1024 KB).
//
// # Key differences from the ZFS expansion test (internal_agent_functional_test.go)
//
//   - Full Pod lifecycle (create → Running → delete) rather than just checking
//     PVC spec.resources.requests.
//   - Filesystem resize verified inside the Pod via kubectl exec.
//   - Uses the LVM backend (lvm-lv type, VG from PILLAR_E2E_LVM_VG).
//   - Requires the NVMe-oF target kernel module stack (same as mount test).
//
// # Sequential vs parallel with ZFS tests
//
// ZFS and LVM tests share the same Kind cluster, storage node, and agent
// DaemonSet pod.  They use different storage backends (distinct ZFS pool vs LVM
// VG) but share the NVMe-oF protocol stack on the same node.  Running them in
// parallel risks NVMe-oF port/configfs conflicts.  Ginkgo's default sequential
// execution within a single suite guarantees ordering without any explicit
// serialisation.
//
// # Gated on PILLAR_E2E_LVM_VG
//
// When the environment variable is absent (no LVM VG provisioned by TestMain),
// the entire Describe block is skipped with an informative message.
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
// LVM volume expansion — Ginkgo spec container
// ─────────────────────────────────────────────────────────────────────────────

var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}

	Describe("LVM volume expansion", Ordered, Label("internal-agent", "lvm", "expansion"), func() {
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
			vgName = lvmVGName()
			if vgName == "" {
				Skip("PILLAR_E2E_LVM_VG not set — skipping LVM volume expansion tests " +
					"(set to the LVM Volume Group name on the storage-worker node, e.g. 'e2e-vg')")
			}

			By("connecting to the Kind cluster")
			suite, err := framework.SetupSuite(framework.WithConnectTimeout(iatConnectTimeout))
			Expect(err).NotTo(HaveOccurred(),
				"LVM volume expansion: cluster connectivity check failed — "+
					"ensure KUBECONFIG is set and TestMain has bootstrapped the cluster")
			k8sClient = suite.Client

			// ── Resolve storage and compute worker nodes ──────────────────────────

			storageNodeName = iatResolveStorageNode(ctx, k8sClient)
			Expect(storageNodeName).NotTo(BeEmpty(),
				"LVM volume expansion: no storage-worker node found "+
					"(expected label %s=true)", iatStorageNodeLabel)
			By(fmt.Sprintf("storage-worker: %s  lvm-vg: %s  thin-pool: %q",
				storageNodeName, vgName, lvmThinPoolName()))

			// Identify and label the compute-worker (NVMe-oF initiator) node.
			{
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

			// ── Build CR stack with AllowVolumeExpansion=true ─────────────────────

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := fmt.Sprintf("lvm-exp-target-%s", crSuffix)
			poolName := fmt.Sprintf("lvm-exp-pool-%s", crSuffix)
			protoName := fmt.Sprintf("lvm-exp-proto-%s", crSuffix)
			bindingName = fmt.Sprintf("lvm-exp-binding-%s", crSuffix)

			// Apply all four CRs, retrying on transient REST-mapper cache misses.
			Eventually(func(g Gomega) {
				t := framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
				g.Expect(framework.Apply(ctx, k8sClient, t)).To(Succeed(),
					"apply PillarTarget %q (node %s)", targetName, storageNodeName)
				target = t

				p := lvmBuildPool(poolName, targetName)
				g.Expect(framework.Apply(ctx, k8sClient, p)).To(Succeed(),
					"apply PillarPool %q (lvm vg=%s)", poolName, vgName)
				pool = p

				proto := framework.KindNVMeOFTCPProtocol(protoName)
				g.Expect(framework.Apply(ctx, k8sClient, proto)).To(Succeed(),
					"apply PillarProtocol %q (nvmeof-tcp)", protoName)
				protocol = proto

				// PillarBinding with AllowVolumeExpansion=true so the StorageClass
				// permits PVC resize requests to reach ControllerExpandVolume.
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
					"apply PillarBinding %q (AllowVolumeExpansion=true)", bindingName)
				binding = b
			}, 60*time.Second, 5*time.Second).Should(Succeed(),
				"LVM expansion CR stack: API server did not accept all CRs within 60 s")

			// Wait for the binding (and its generated StorageClass) to be Ready.
			By(fmt.Sprintf("waiting for PillarBinding %q to be Ready", bindingName))
			Expect(framework.WaitForReady(ctx, k8sClient, binding, iatConditionTimeout)).To(Succeed(),
				"PillarBinding must reach Ready=True before PVC provisioning can start")

			// ── Create isolated test namespace and initial PVC ─────────────────────

			var nsErr error
			testNS, nsErr = framework.CreateTestNamespace(ctx, k8sClient, "lvm-exp")
			Expect(nsErr).NotTo(HaveOccurred(),
				"create test namespace for LVM expansion specs")

			pvc = framework.NewPillarPVC("lvm-exp-vol", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, k8sClient, pvc)).To(Succeed(),
				"create 1Gi PVC against StorageClass %q", bindingName)

			By(fmt.Sprintf("waiting for PVC %q/%q to become Bound (up to %s)",
				testNS.Name, pvc.Name, iatProvisioningTimeout))
			Expect(framework.WaitForPVCBound(ctx, k8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
				"PVC must be Bound before setting up NVMe-oF target and creating the test Pod; "+
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
			vhParts := strings.SplitN(nvmPV.Spec.CSI.VolumeHandle, "/", 4)
			Expect(len(vhParts)).To(Equal(4),
				"LVM volumeHandle must have 4 slash-separated parts: "+
					"<target>/<proto>/<backend>/<agentVolID>")
			nvmDevPath := "/dev/" + vhParts[3]

			By(fmt.Sprintf("configuring NVMe-oF TCP target: nqn=%s port=%s dev=%s",
				nvmNQN, nvmPort, nvmDevPath))

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
			setupOut, setupErr := captureOutput("docker", "exec", storageNodeName,
				"sh", "-c", nvmSetupScript)
			Expect(setupErr).NotTo(HaveOccurred(),
				"NVMe-oF target setup failed (LVM expansion): %s", setupOut)
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
			// compute-worker container via mknod.
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

			// ── DeferCleanup registrations (LIFO order) ───────────────────────────

			// Registered FIRST (runs LAST): cancel NVMe bridge goroutine.
			DeferCleanup(func(_ context.Context) {
				nvmBridgeCancel()
			})

			// Registered SECOND (runs SECOND-TO-LAST): NVMe initiator disconnect.
			capturedComputeNodeName := computeNodeName
			capturedNQN := nvmNQN
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

			// Registered THIRD (runs THIRD-TO-LAST): NVMe-oF target configfs cleanup.
			capturedPortID := nvmPort
			DeferCleanup(func(_ context.Context) {
				By("tearing down NVMe-oF TCP target configfs entries (LVM expansion)")
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

			// Registered LAST (runs FIRST): Pod → PVC → CRs → namespace + compute-node label.
			DeferCleanup(func(dctx context.Context) {
				By("LVM volume expansion: cleaning up Pod, PVC, CRs, and namespace")
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
							"WARNING: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				if testNS != nil {
					_ = framework.EnsureNamespaceGone(dctx, k8sClient, testNS.Name, iatCleanupTimeout)
				}
				// Remove compute-node label.
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
		})

		// ─────────────────────────────────────────────────────────────────────────
		// It: Pod reaches Running with 1 Gi LVM-backed PVC
		// ─────────────────────────────────────────────────────────────────────────

		It("Pod mounts 1Gi LVM PVC and reaches Running", func(ctx context.Context) {
			podName := fmt.Sprintf("lvm-exp-pod-%d", time.Now().UnixMilli()%100000)
			pod = iatBuildTestPod(podName, testNS.Name, pvc.Name)

			By(fmt.Sprintf("creating Pod %q/%q that mounts LVM PVC %q",
				testNS.Name, podName, pvc.Name))
			Expect(k8sClient.Create(ctx, pod)).To(Succeed(),
				"create test Pod — triggers ControllerPublish + NodeStage + NodePublish")

			By(fmt.Sprintf("waiting for Pod %q to reach Running (up to %s)", podName, iatMountTimeout))

			// Periodic log collection to aid diagnosis of slow starts.
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
				g.Expect(current.Status.Phase).To(Equal(corev1.PodRunning),
					"Pod must reach Running after NVMe-oF connect + format + mount "+
						"(LVM backend); current phase: %s", current.Status.Phase)
			}, iatMountTimeout, 5*time.Second).Should(Succeed(),
				"Pod %q/%q did not reach Running — check pillar-node logs, "+
					"NVMe-oF module availability on compute-worker, and LVM device "+
					"accessibility on storage-worker (%s, vg=%s)",
				testNS.Name, podName, storageNodeName, vgName)

			By(fmt.Sprintf("Pod %q/%q is Running with 1Gi LVM PVC mounted at /data",
				testNS.Name, podName))
		})

		// ─────────────────────────────────────────────────────────────────────────
		// It: Initial filesystem size is approximately 1 Gi
		// ─────────────────────────────────────────────────────────────────────────

		It("filesystem inside Pod reports approximately 1Gi capacity before expansion", func(ctx context.Context) {
			Expect(pod).NotTo(BeNil(),
				"pod must have been created and reached Running in the previous spec")

			By(fmt.Sprintf("exec into Pod %q/%q: df -k /data",
				testNS.Name, pod.Name))

			// Poll until the exec succeeds (pod may still be initialising the mount).
			var initialKiB int64
			Eventually(func(g Gomega) {
				// Use "df -k" (POSIX/BusyBox compatible) instead of
				// "df --output=size" (GNU coreutils only).
				out, execErr := captureOutput(
					"kubectl", "exec",
					"-n", testNS.Name, pod.Name,
					"--", "df", "-k", "/data",
				)
				g.Expect(execErr).NotTo(HaveOccurred(),
					"kubectl exec df /data failed: %s", out)
				kiB := parseDfSizeKiB(g, out)
				initialKiB = kiB
			}, iatMountTimeout, 5*time.Second).Should(Succeed(),
				"failed to read filesystem size inside Pod %q/%q",
				testNS.Name, pod.Name)

			// A 1 Gi formatted ext4 volume leaves ~0.94 Gi usable after journal.
			// Accept anything between 700 Mi and 1.2 Gi to be flexible.
			const (
				minExpectedKiB int64 = 700 * 1024  // 700 Mi
				maxExpectedKiB int64 = 1200 * 1024 // 1.2 Gi
			)
			Expect(initialKiB).To(BeNumerically(">=", minExpectedKiB),
				"initial filesystem size %d KiB is below expected minimum %d KiB (700 Mi) — "+
					"PVC may not have been formatted or the LVM LV is too small",
				initialKiB, minExpectedKiB)
			Expect(initialKiB).To(BeNumerically("<=", maxExpectedKiB),
				"initial filesystem size %d KiB exceeds expected maximum %d KiB (1.2 Gi) — "+
					"unexpected filesystem layout on LVM LV",
				initialKiB, maxExpectedKiB)

			By(fmt.Sprintf("initial filesystem size in Pod: %d KiB (~%.2f Gi)",
				initialKiB, float64(initialKiB)/(1024*1024)))
		})

		// ─────────────────────────────────────────────────────────────────────────
		// It: PVC resize from 1Gi to 2Gi is reflected in PVC status
		// ─────────────────────────────────────────────────────────────────────────

		It("PVC resize to 2Gi is reflected in PVC status capacity", func(ctx context.Context) {
			Expect(pvc).NotTo(BeNil(), "pvc must exist")

			By("fetching current PVC state for resource version")
			current := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), current)).To(Succeed())

			By("updating PVC storage request from 1Gi to 2Gi")
			current.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
			Expect(k8sClient.Update(ctx, current)).To(Succeed(),
				"update PVC storage request — triggers CSI ControllerExpandVolume via "+
					"the external-resizer sidecar; the agent then calls lvextend on the LVM LV")

			By(fmt.Sprintf("waiting for PVC status.capacity.storage >= 2Gi (up to %s)",
				iatProvisioningTimeout))
			Eventually(func(g Gomega) {
				updated := &corev1.PersistentVolumeClaim{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), updated)).To(Succeed())

				// PVC status.capacity is updated by the provisioner/controller after
				// ControllerExpandVolume succeeds.
				actualCap, ok := updated.Status.Capacity[corev1.ResourceStorage]
				g.Expect(ok).To(BeTrue(),
					"PVC status.capacity must contain a storage entry after expansion; "+
						"current status: %+v", updated.Status)

				wantCap := resource.MustParse("2Gi")
				_, _ = fmt.Fprintf(GinkgoWriter,
					"[expand-wait] PVC status.capacity.storage=%s (want >= %s)\n",
					actualCap.String(), wantCap.String())

				g.Expect(actualCap.Cmp(wantCap)).To(BeNumerically(">=", 0),
					"PVC status.capacity.storage must be >= 2Gi after expansion "+
						"(current: %s) — check the CSI resizer sidecar and agent ExpandVolume RPC",
					actualCap.String())
			}, 3*time.Minute, 5*time.Second).Should(Succeed(),
				"PVC status.capacity did not reflect 2Gi within 3m — "+
					"check the external-resizer sidecar, agent ExpandVolume RPC, "+
					"and kubelet NodeExpandVolume; "+
					"also verify lvextend ran successfully on VG %q", vgName)

			By("PVC status.capacity.storage reflects >= 2Gi: ControllerExpandVolume + agent.ExpandVolume succeeded")
		})

		// ─────────────────────────────────────────────────────────────────────────
		// It: Filesystem inside running Pod is resized to >= 2Gi
		// ─────────────────────────────────────────────────────────────────────────

		It("filesystem inside running Pod is resized to >= 2Gi after PVC expansion", func(ctx context.Context) {
			Expect(pod).NotTo(BeNil(),
				"pod must have been created and reached Running in earlier specs")
			Expect(pvc).NotTo(BeNil(), "pvc must exist")

			// After ControllerExpandVolume succeeds the kubelet detects the
			// FileSystemResizePending condition on the PVC and calls NodeExpandVolume,
			// which runs resize2fs on the mounted filesystem.  For online ext4 resize
			// the resize happens without pod restart.
			//
			// We poll df output inside the running pod.  The kubelet may take a few
			// seconds to trigger NodeExpandVolume after the PV annotation is set.

			By(fmt.Sprintf("waiting for filesystem in Pod %q/%q to report >= 2Gi (up to %s)",
				testNS.Name, pod.Name, iatProvisioningTimeout))

			const wantMinKiB int64 = 1900 * 1024 // ~1.85 Gi — generous lower bound for 2Gi ext4

			Eventually(func(g Gomega) {
				out, execErr := captureOutput(
					"kubectl", "exec",
					"-n", testNS.Name, pod.Name,
					"--", "df", "-k", "/data",
				)
				g.Expect(execErr).NotTo(HaveOccurred(),
					"kubectl exec df /data failed during expansion wait: %s", out)

				kiB := parseDfSizeKiB(g, out)

				_, _ = fmt.Fprintf(GinkgoWriter,
					"[resize-wait] filesystem size = %d KiB (want >= %d KiB)\n",
					kiB, wantMinKiB)

				g.Expect(kiB).To(BeNumerically(">=", wantMinKiB),
					"filesystem inside Pod must be >= %d KiB (~1.85 Gi) after resize; "+
						"current: %d KiB — NodeExpandVolume + resize2fs may not have run yet",
					wantMinKiB, kiB)
			}, 3*time.Minute, 5*time.Second).Should(Succeed(),
				"filesystem inside Pod %q/%q did not expand to >= 2Gi within 3m — "+
					"check the pillar-node NodeExpandVolume logs and verify that "+
					"resize2fs completed on the NVMe-oF block device",
				testNS.Name, pod.Name, iatProvisioningTimeout)

			By(fmt.Sprintf("filesystem inside Pod %q/%q is >= 2Gi: NodeExpandVolume + resize2fs succeeded",
				testNS.Name, pod.Name))
		})

		// ─────────────────────────────────────────────────────────────────────────
		// It: Pod deletion and PVC cleanup complete cleanly after expansion
		// ─────────────────────────────────────────────────────────────────────────

		It("Pod deletion and PVC deletion complete cleanly after expansion", func(ctx context.Context) {
			Expect(pod).NotTo(BeNil(),
				"pod must have been created successfully in earlier specs")
			Expect(pvc).NotTo(BeNil(), "pvc must exist")

			By(fmt.Sprintf("deleting Pod %q/%q", testNS.Name, pod.Name))
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())

			By("waiting for Pod to be fully removed")
			Expect(framework.EnsureGone(ctx, k8sClient, pod, iatCleanupTimeout)).To(Succeed(),
				"Pod must be fully removed (NodeUnpublish + NodeUnstage + ControllerUnpublish) "+
					"before PVC deletion")
			pod = nil // prevent double-delete in DeferCleanup

			By(fmt.Sprintf("deleting PVC %q/%q after expansion", testNS.Name, pvc.Name))
			Expect(framework.EnsurePVCGone(ctx, k8sClient, pvc, iatCleanupTimeout)).To(Succeed(),
				"PVC deletion must complete after expansion — triggers DeleteVolume "+
					"(lvremove on the expanded LVM LV)")
			pvc = nil // prevent double-delete in DeferCleanup

			By("expanded LVM LV destroyed via DeleteVolume; PV reclaimed")
		})
	}) // end Describe("LVM volume expansion")

	return true
}()

// parseDfSizeKiB extracts the total size in KiB from "df -k" output.
// BusyBox df -k output format:
//
//	Filesystem           1K-blocks      Used Available Use% Mounted on
//	/dev/nvme0n1           1038336     34716    987236   3% /data
//
// The function parses the second column (1K-blocks) of the data line.
func parseDfSizeKiB(g Gomega, dfOutput string) int64 {
	lines := strings.Split(strings.TrimSpace(dfOutput), "\n")
	g.Expect(len(lines)).To(BeNumerically(">=", 2),
		"df output must have at least header + data lines, got: %q", dfOutput)
	// Parse the last data line (skip header).
	fields := strings.Fields(lines[len(lines)-1])
	g.Expect(len(fields)).To(BeNumerically(">=", 4),
		"df data line must have at least 4 columns, got: %q", lines[len(lines)-1])
	kiB, parseErr := strconv.ParseInt(fields[1], 10, 64)
	g.Expect(parseErr).NotTo(HaveOccurred(),
		"parse df 1K-blocks column %q as int: %v", fields[1], parseErr)
	return kiB
}
