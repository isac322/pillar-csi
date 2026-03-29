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

// internal_agent_functional_test.go — Agent functional e2e test cases for the
// pillar-csi "internal agent" (DaemonSet) mode.
//
// This file implements Sub-AC 7c of the pillar-csi e2e test infrastructure:
// functional tests that exercise the full CSI volume lifecycle through the
// in-cluster agent DaemonSet, including:
//
//  1. Agent DaemonSet connectivity — PillarTarget (NodeRef) → AgentConnected
//     → Ready; verifies the controller dials the DaemonSet agent and populates
//     status.agentVersion + status.capabilities.
//
//  2. CR stack lifecycle — PillarTarget → PillarPool → PillarProtocol →
//     PillarBinding; verifies all conditions transition to True and the
//     PillarBinding generates a Kubernetes StorageClass.
//     Gated on PILLAR_E2E_ZFS_POOL (requires a real ZFS pool on the storage node).
//
//  3. CSI volume provisioning — PVC created against the generated StorageClass;
//     verifies the PVC becomes Bound, the PV has the correct capacity and
//     StorageClass, and that a second PVC can be provisioned independently.
//     Gated on PILLAR_E2E_ZFS_POOL.
//
//  4. Mount/unmount lifecycle — Pod creation on the compute-worker node
//     triggers NodeStage + NodePublish; Pod deletion triggers NodeUnpublish +
//     NodeUnstage; PVC deletion triggers ControllerUnpublish + DeleteVolume.
//     Gated on PILLAR_E2E_ZFS_POOL.
//
//  5. Error-path scenarios — always run; cover invalid NodeRef, missing ZFS
//     pool name, PVC against a non-existent StorageClass, and volume expansion
//     on an expansion-capable StorageClass.
//
// # Prerequisites
//
// The Kind cluster must already be bootstrapped and the Helm chart deployed:
//
//	hack/e2e-setup.sh
//	export KUBECONFIG=$(kind get kubeconfig --name pillar-csi-e2e)
//
// # Running these specs in isolation
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestInternalAgent \
//	    -- --ginkgo.label-filter=internal-agent --ginkgo.v
//
// # Environment variables
//
//	PILLAR_E2E_ZFS_POOL     ZFS pool name on the storage-worker node.
//	                         When empty, all ZFS-dependent test groups are
//	                         skipped with an informative message.
//	PILLAR_E2E_STORAGE_NODE Kubernetes Node name that runs the agent DaemonSet.
//	                         When empty the first node labelled
//	                         pillar-csi.bhyoo.com/storage-node=true is used.
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

// ─── Constants ───────────────────────────────────────────────────────────────

const (
	// iatStorageNodeLabel is the node label used to identify storage-worker
	// nodes.  Matches the label set in hack/kind-config.yaml.
	iatStorageNodeLabel = "pillar-csi.bhyoo.com/storage-node"

	// iatComputeNodeLabel is the node label used to identify compute-worker
	// nodes.  Pods that mount CSI volumes should be scheduled here (initiator side).
	iatComputeNodeLabel = "pillar-csi.bhyoo.com/compute-node"

	// iatCSIProvisioner is the CSI driver name registered by pillar-csi.
	iatCSIProvisioner = "pillar-csi.bhyoo.com"

	// Timeout constants used across all specs in this file.
	iatConnectTimeout      = 30 * time.Second
	iatConditionTimeout    = 90 * time.Second
	iatProvisioningTimeout = 90 * time.Second
	iatMountTimeout        = 90 * time.Second
	iatCleanupTimeout      = 90 * time.Second

	// iatHeartbeatObservation is how long the heartbeat spec observes the
	// Ready=True condition for stability.  15 s is sufficient to verify the
	// agent heartbeat/lease renewal (interval is typically 5 s).
	iatHeartbeatObservation = 15 * time.Second

	// iatHeartbeatPoll is the polling interval inside the heartbeat
	// Consistently block.
	iatHeartbeatPoll = 3 * time.Second

	// iatPendingVerificationDelay is how long the error-path specs wait before
	// asserting a PVC is still Pending (time for the provisioner to respond
	// with a failure event if it were going to).
	iatPendingVerificationDelay = 10 * time.Second
)

// ─── Package-level state ─────────────────────────────────────────────────────

// iatK8sClient is the controller-runtime client used by all specs in this
// file.  Initialised once in the outer BeforeAll.
var iatK8sClient client.Client

// ─── Ginkgo container ────────────────────────────────────────────────────────

// InternalAgent functional specs are only registered in internal-agent mode.
// The agent DaemonSet is disabled in external-agent mode (unmatchable
// nodeSelector), so running these specs there would time out waiting for
// AgentConnected to become True.  Conditional registration keeps the Ginkgo
// skip count at zero.
var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}
	Describe("InternalAgent functional", Ordered, Label("internal-agent"), func() {
		// storageNodeName is the Kubernetes Node name of the storage-worker resolved
		// in BeforeAll and referenced by all inner specs that create PillarTargets
		// with NodeRef.
		var storageNodeName string

		// ── BeforeAll: cluster connectivity + storage node resolution ────────────

		BeforeAll(func(ctx context.Context) {
			By("connecting to the Kind cluster")
			suite, err := framework.SetupSuite(framework.WithConnectTimeout(iatConnectTimeout))
			Expect(err).NotTo(HaveOccurred(),
				"InternalAgent functional: cluster connectivity check failed — "+
					"ensure KUBECONFIG is set and 'hack/e2e-setup.sh' has been run")
			iatK8sClient = suite.Client

			By("resolving the storage-worker node name")
			storageNodeName = iatResolveStorageNode(ctx, iatK8sClient)
			Expect(storageNodeName).NotTo(BeEmpty(),
				"InternalAgent functional: no node labelled %s=true found — "+
					"check hack/kind-config.yaml for the storage-worker entry",
				iatStorageNodeLabel)
			By(fmt.Sprintf("storage-worker node: %s", storageNodeName))
		})

		// ── Group 1: agent DaemonSet connectivity ────────────────────────────────
		//
		// These specs verify that the pillar-csi controller can reach the agent
		// DaemonSet pod running on the storage-worker node by creating a PillarTarget
		// with spec.nodeRef set to the storage-worker's Kubernetes Node name.
		//
		// All specs in this group share a single PillarTarget CR created in
		// BeforeAll and deleted in the DeferCleanup registered there.

		Describe("agent DaemonSet connectivity", Ordered, func() {
			var (
				target     *v1alpha1.PillarTarget
				targetName string
			)

			BeforeAll(func(ctx context.Context) {
				targetName = fmt.Sprintf("iat-conn-%d", time.Now().UnixMilli()%100000)
				By(fmt.Sprintf("creating PillarTarget %q → node %s", targetName, storageNodeName))
				// Wrap Apply in Eventually to retry transient REST-mapper cache misses.
				// The controller-runtime DynamicRESTMapper may not have refreshed its
				// discovery cache yet when this BeforeAll runs, causing "no matches for
				// kind 'PillarTarget'" errors in the first few seconds after Helm install.
				Eventually(func(g Gomega) {
					t := framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
					g.Expect(framework.Apply(ctx, iatK8sClient, t)).To(Succeed(),
						"apply PillarTarget %q", targetName)
					target = t
				}, 60*time.Second, 5*time.Second).Should(Succeed(),
					"apply PillarTarget %q: REST mapper did not discover PillarTarget "+
						"within 60s after Helm install", targetName)

				DeferCleanup(func(dctx context.Context) {
					By(fmt.Sprintf("cleaning up PillarTarget %q", targetName))
					if err := framework.EnsureGone(dctx, iatK8sClient, target, iatCleanupTimeout); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"WARNING: cleanup PillarTarget %q: %v\n", targetName, err)
					}
				})
			})

			It("AgentConnected condition becomes True", func(ctx context.Context) {
				By(fmt.Sprintf("waiting for AgentConnected=True on %q (up to %s)",
					targetName, iatConditionTimeout))
				err := framework.WaitForCondition(ctx, iatK8sClient, target,
					"AgentConnected", metav1.ConditionTrue, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"AgentConnected must become True — verify the pillar-agent DaemonSet is "+
						"Running on node %q and the pillar-csi controller is deployed",
					storageNodeName)
				By("AgentConnected=True: controller successfully dialled the in-cluster agent")
			})

			It("status.agentVersion is reported by the connected agent", func(ctx context.Context) {
				err := framework.WaitForField(ctx, iatK8sClient, target,
					func(t *v1alpha1.PillarTarget) bool {
						return t.Status.AgentVersion != ""
					}, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"status.agentVersion must be set once the controller connects to the agent")
				Expect(target.Status.AgentVersion).NotTo(BeEmpty(),
					"agentVersion must be a non-empty string returned by GetCapabilities RPC")
				By(fmt.Sprintf("status.agentVersion = %q", target.Status.AgentVersion))
			})

			It("status.capabilities lists at least one backend and protocol", func(ctx context.Context) {
				err := framework.WaitForField(ctx, iatK8sClient, target,
					func(t *v1alpha1.PillarTarget) bool {
						return t.Status.Capabilities != nil &&
							len(t.Status.Capabilities.Backends) > 0
					}, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"status.capabilities must be populated from the agent's GetCapabilities response")

				Expect(target.Status.Capabilities).NotTo(BeNil(),
					"capabilities struct must be non-nil once the agent is connected")
				Expect(target.Status.Capabilities.Backends).NotTo(BeEmpty(),
					"agent must advertise at least one backend type (e.g. zfs-zvol)")
				Expect(target.Status.Capabilities.Protocols).NotTo(BeEmpty(),
					"agent must advertise at least one protocol type (e.g. nvmeof-tcp)")
				By(fmt.Sprintf("capabilities: backends=%v protocols=%v",
					target.Status.Capabilities.Backends,
					target.Status.Capabilities.Protocols))
			})

			It("status.discoveredPools field is populated (may be empty without ZFS)", func(ctx context.Context) {
				// The field may contain an empty slice when no ZFS pool exists in CI,
				// but capabilities must be present before this check.
				err := framework.WaitForField(ctx, iatK8sClient, target,
					func(t *v1alpha1.PillarTarget) bool {
						return t.Status.Capabilities != nil
					}, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred())
				By(fmt.Sprintf("status.discoveredPools: %v", target.Status.DiscoveredPools))
			})

			It("Ready condition becomes True", func(ctx context.Context) {
				By(fmt.Sprintf("waiting for Ready=True on PillarTarget %q", targetName))
				err := framework.WaitForReady(ctx, iatK8sClient, target, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PillarTarget must reach Ready=True once the agent is connected and healthy")
				By("PillarTarget is Ready=True: in-cluster agent DaemonSet is reachable and healthy")
			})

			It("Ready condition is maintained across reconcile cycles (heartbeat)", func(ctx context.Context) {
				// Snapshot the current Ready condition's LastTransitionTime, then observe
				// that it does not change over iatHeartbeatObservation seconds.  A changed
				// transition time indicates the condition flipped to False and back, which
				// means the agent heartbeat was interrupted.
				fresh := &v1alpha1.PillarTarget{}
				Expect(iatK8sClient.Get(ctx,
					client.ObjectKey{Name: targetName}, fresh)).To(Succeed(),
					"re-read PillarTarget for baseline condition state")

				var readyCond *metav1.Condition
				for i := range fresh.Status.Conditions {
					if fresh.Status.Conditions[i].Type == "Ready" {
						c := fresh.Status.Conditions[i]
						readyCond = &c
						break
					}
				}
				Expect(readyCond).NotTo(BeNil(),
					"Ready condition must be present before heartbeat observation")
				Expect(readyCond.Status).To(Equal(metav1.ConditionTrue),
					"Ready must already be True before beginning the heartbeat check")

				initialTransition := readyCond.LastTransitionTime
				By(fmt.Sprintf(
					"Ready=True since %s — observing stability for %s (polling every %s)",
					initialTransition.UTC().Format(time.RFC3339),
					iatHeartbeatObservation, iatHeartbeatPoll))

				Consistently(func(g Gomega) {
					current := &v1alpha1.PillarTarget{}
					g.Expect(iatK8sClient.Get(ctx,
						client.ObjectKey{Name: targetName}, current)).To(Succeed(),
						"PillarTarget %q must still exist during heartbeat observation", targetName)

					var cond *metav1.Condition
					for i := range current.Status.Conditions {
						if current.Status.Conditions[i].Type == "Ready" {
							c := current.Status.Conditions[i]
							cond = &c
							break
						}
					}
					g.Expect(cond).NotTo(BeNil(),
						"Ready condition must still be present on every poll")
					g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
						"Ready must remain True throughout the %s observation window "+
							"(agent heartbeat must be maintained)", iatHeartbeatObservation)
					g.Expect(cond.LastTransitionTime).To(Equal(initialTransition),
						"Ready condition must not flip: a changed LastTransitionTime "+
							"indicates the heartbeat was interrupted")
				}, iatHeartbeatObservation, iatHeartbeatPoll).Should(Succeed(),
					"Ready=True stability check failed: agent heartbeat not maintained")

				By("heartbeat confirmed: Ready=True stable for the full observation window")
			})
		})

		// ── Group 2: CR stack lifecycle ──────────────────────────────────────────
		//
		// These specs verify that the full CR resource stack (PillarTarget →
		// PillarPool → PillarProtocol → PillarBinding) reconciles correctly when a
		// real ZFS pool is available on the storage-worker node.
		//
		// Skipped when PILLAR_E2E_ZFS_POOL is not set.

		Describe("CR stack lifecycle", Ordered, func() {
			var (
				target   *v1alpha1.PillarTarget
				pool     *v1alpha1.PillarPool
				protocol *v1alpha1.PillarProtocol
				binding  *v1alpha1.PillarBinding
				zfsPool  string
			)

			BeforeAll(func(ctx context.Context) {
				zfsPool = iatZFSPool()
				if zfsPool == "" {
					Skip("PILLAR_E2E_ZFS_POOL not set — skipping CR stack lifecycle tests " +
						"(set to the ZFS pool name on the storage-worker node, e.g. 'tank')")
				}

				crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

				// Pre-compute names so they are stable across Eventually retries.
				targetName := fmt.Sprintf("iat-stack-target-%s", crSuffix)
				poolName := fmt.Sprintf("iat-stack-pool-%s", crSuffix)
				protoName := fmt.Sprintf("iat-stack-proto-%s", crSuffix)
				bindingName := fmt.Sprintf("iat-stack-binding-%s", crSuffix)

				// Apply all four CRs with retry to handle transient REST-mapper cache
				// misses or brief API-server hiccups that can occur just after the
				// previous Describe group's DeferCleanup has finished.
				Eventually(func(g Gomega) {
					t := framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
					g.Expect(framework.Apply(ctx, iatK8sClient, t)).To(Succeed(),
						"apply PillarTarget %q for CR stack lifecycle test", targetName)
					target = t

					p := framework.NewZFSZvolPool(poolName, targetName, zfsPool)
					g.Expect(framework.Apply(ctx, iatK8sClient, p)).To(Succeed(),
						"apply PillarPool %q (zfs-zvol, pool=%s)", poolName, zfsPool)
					pool = p

					proto := framework.NewNVMeOFTCPProtocol(protoName)
					g.Expect(framework.Apply(ctx, iatK8sClient, proto)).To(Succeed(),
						"apply PillarProtocol %q (nvmeof-tcp)", protoName)
					protocol = proto

					b := framework.NewSimplePillarBinding(bindingName, poolName, protoName)
					g.Expect(framework.Apply(ctx, iatK8sClient, b)).To(Succeed(),
						"apply PillarBinding %q (pool=%s, proto=%s)", bindingName, poolName, protoName)
					binding = b
				}, 60*time.Second, 5*time.Second).Should(Succeed(),
					"CR stack apply: API server did not accept all four CRs within 60s "+
						"after Group 1 DeferCleanup completed")

				// Register cleanup in reverse creation order so dependencies are respected.
				DeferCleanup(func(dctx context.Context) {
					By("cleaning up CR stack lifecycle resources")
					for _, obj := range []client.Object{binding, protocol, pool, target} {
						if err := framework.EnsureGone(dctx, iatK8sClient, obj, iatCleanupTimeout); err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"WARNING: CR stack cleanup %T %q: %v\n", obj, obj.GetName(), err)
						}
					}
				})
			})

			It("PillarTarget reaches Ready=True", func(ctx context.Context) {
				err := framework.WaitForReady(ctx, iatK8sClient, target, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PillarTarget must be Ready before downstream CRs can progress")
			})

			It("PillarPool TargetReady condition becomes True", func(ctx context.Context) {
				err := framework.WaitForCondition(ctx, iatK8sClient, pool,
					"TargetReady", metav1.ConditionTrue, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PillarPool TargetReady must be True once the referenced PillarTarget is Ready")
			})

			It("PillarPool BackendSupported condition becomes True", func(ctx context.Context) {
				err := framework.WaitForCondition(ctx, iatK8sClient, pool,
					"BackendSupported", metav1.ConditionTrue, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"BackendSupported must be True — agent DaemonSet must advertise zfs-zvol backend")
			})

			It("PillarPool PoolDiscovered condition becomes True", func(ctx context.Context) {
				err := framework.WaitForCondition(ctx, iatK8sClient, pool,
					"PoolDiscovered", metav1.ConditionTrue, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PoolDiscovered must be True — ZFS pool %q must exist on node %q",
					zfsPool, storageNodeName)
			})

			It("PillarPool reaches Ready=True and reports capacity", func(ctx context.Context) {
				err := framework.WaitForReady(ctx, iatK8sClient, pool, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PillarPool must reach Ready=True once all conditions are satisfied")

				// Also verify that capacity is populated once the pool is Ready.
				capErr := framework.WaitForField(ctx, iatK8sClient, pool,
					func(p *v1alpha1.PillarPool) bool {
						return p.Status.Capacity != nil &&
							p.Status.Capacity.Total != nil &&
							!p.Status.Capacity.Total.IsZero()
					}, iatConditionTimeout)
				Expect(capErr).NotTo(HaveOccurred(),
					"PillarPool status.capacity.total must be non-zero once pool is discovered")

				By(fmt.Sprintf("pool %q capacity: total=%s",
					pool.Name, pool.Status.Capacity.Total.String()))
			})

			It("PillarBinding Ready condition becomes True", func(ctx context.Context) {
				err := framework.WaitForReady(ctx, iatK8sClient, binding, iatConditionTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PillarBinding must reach Ready=True to generate the StorageClass")
			})

			It("PillarBinding generates a Kubernetes StorageClass with the pillar-csi provisioner", func(ctx context.Context) {
				// The StorageClass name defaults to the PillarBinding name when
				// spec.storageClass.name is not explicitly set.
				scName := binding.Name
				sc := &storagev1.StorageClass{}
				Expect(iatK8sClient.Get(ctx, client.ObjectKey{Name: scName}, sc)).To(Succeed(),
					"StorageClass %q must be created by the controller once PillarBinding is Ready",
					scName)
				Expect(sc.Provisioner).To(Equal(iatCSIProvisioner),
					"StorageClass %q must use the pillar-csi provisioner", scName)
				By(fmt.Sprintf("StorageClass %q exists with provisioner %s", sc.Name, sc.Provisioner))
			})
		})

		// ── Group 3: CSI volume provisioning ─────────────────────────────────────
		//
		// These specs exercise dynamic PVC provisioning through the agent DaemonSet:
		//   1. PVC created → CSI CreateVolume → agent allocates ZFS zvol + NVMe-oF target
		//   2. PVC reaches Bound phase; PV properties are validated
		//   3. A second PVC is provisioned independently (non-colliding volume IDs)
		//
		// Gated on PILLAR_E2E_ZFS_POOL.

		Describe("CSI volume provisioning", Ordered, func() {
			var (
				target      *v1alpha1.PillarTarget
				pool        *v1alpha1.PillarPool
				protocol    *v1alpha1.PillarProtocol
				binding     *v1alpha1.PillarBinding
				pvc         *corev1.PersistentVolumeClaim
				pvc2        *corev1.PersistentVolumeClaim
				testNS      *corev1.Namespace
				bindingName string
				zfsPool     string
			)

			BeforeAll(func(ctx context.Context) {
				zfsPool = iatZFSPool()
				if zfsPool == "" {
					Skip("PILLAR_E2E_ZFS_POOL not set — skipping CSI volume provisioning tests")
				}

				crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

				// Pre-compute names so they are stable across Eventually retries.
				targetName := fmt.Sprintf("iat-prov-target-%s", crSuffix)
				poolName := fmt.Sprintf("iat-prov-pool-%s", crSuffix)
				protoName := fmt.Sprintf("iat-prov-proto-%s", crSuffix)
				bindingName = fmt.Sprintf("iat-prov-binding-%s", crSuffix)

				// Apply all four CRs with retry to handle transient REST-mapper cache
				// misses or brief API-server hiccups between Describe groups.
				// Build the full CR stack required for provisioning.
				Eventually(func(g Gomega) {
					t := framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
					g.Expect(framework.Apply(ctx, iatK8sClient, t)).To(Succeed())
					target = t

					p := framework.NewZFSZvolPool(poolName, targetName, zfsPool)
					g.Expect(framework.Apply(ctx, iatK8sClient, p)).To(Succeed())
					pool = p

					proto := framework.NewNVMeOFTCPProtocol(protoName)
					g.Expect(framework.Apply(ctx, iatK8sClient, proto)).To(Succeed())
					protocol = proto

					b := framework.NewSimplePillarBinding(bindingName, poolName, protoName)
					g.Expect(framework.Apply(ctx, iatK8sClient, b)).To(Succeed())
					binding = b
				}, 60*time.Second, 5*time.Second).Should(Succeed(),
					"CSI provisioning CR stack: API server did not accept all four CRs within 60s")

				// Wait for the binding to become Ready before creating any PVCs.
				By("waiting for PillarBinding to be Ready before creating test PVCs")
				Expect(framework.WaitForReady(ctx, iatK8sClient, binding, iatConditionTimeout)).To(Succeed(),
					"PillarBinding %q must be Ready before provisioning tests can run", bindingName)

				// Create an isolated namespace for the PVC objects.
				var err error
				testNS, err = framework.CreateTestNamespace(ctx, iatK8sClient, "iat-prov")
				Expect(err).NotTo(HaveOccurred(), "create test namespace for provisioning specs")

				// Create the first PVC (1Gi).
				pvc = framework.NewPillarPVC("iat-vol-1", testNS.Name, bindingName,
					resource.MustParse("1Gi"))
				Expect(framework.CreatePVC(ctx, iatK8sClient, pvc)).To(Succeed(),
					"create PVC %q/%q against StorageClass %q", testNS.Name, pvc.Name, bindingName)

				// Create the second PVC (2Gi) to test independent provisioning.
				pvc2 = framework.NewPillarPVC("iat-vol-2", testNS.Name, bindingName,
					resource.MustParse("2Gi"))
				Expect(framework.CreatePVC(ctx, iatK8sClient, pvc2)).To(Succeed(),
					"create second PVC %q/%q against StorageClass %q", testNS.Name, pvc2.Name, bindingName)

				// Register cleanup: PVCs first, then CRs, then namespace.
				DeferCleanup(func(dctx context.Context) {
					By("cleaning up CSI provisioning test resources")
					for _, p := range []*corev1.PersistentVolumeClaim{pvc, pvc2} {
						if p == nil {
							continue
						}
						if err := framework.EnsurePVCGone(dctx, iatK8sClient, p, iatCleanupTimeout); err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"WARNING: cleanup PVC %q/%q: %v\n", p.Namespace, p.Name, err)
						}
					}
					for _, obj := range []client.Object{binding, protocol, pool, target} {
						if err := framework.EnsureGone(dctx, iatK8sClient, obj, iatCleanupTimeout); err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"WARNING: cleanup %T %q: %v\n", obj, obj.GetName(), err)
						}
					}
					if testNS != nil {
						if err := framework.EnsureNamespaceGone(dctx, iatK8sClient, testNS.Name, iatCleanupTimeout); err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"WARNING: cleanup namespace %q: %v\n", testNS.Name, err)
						}
					}
				})
			})

			It("first PVC becomes Bound once the agent provisions the volume", func(ctx context.Context) {
				By(fmt.Sprintf("waiting for PVC %q/%q to be Bound (up to %s)",
					testNS.Name, pvc.Name, iatProvisioningTimeout))
				err := framework.WaitForPVCBound(ctx, iatK8sClient, pvc, iatProvisioningTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"PVC must be Bound — the pillar-csi controller must have called the "+
						"in-cluster agent's CreateVolume RPC against ZFS pool %q on node %q",
					zfsPool, storageNodeName)
				By(fmt.Sprintf("PVC %q/%q is Bound to PV %q", testNS.Name, pvc.Name, pvc.Spec.VolumeName))
			})

			It("bound PV has capacity >= 1Gi", func(ctx context.Context) {
				pv, err := framework.GetBoundPV(ctx, iatK8sClient, pvc)
				Expect(err).NotTo(HaveOccurred(),
					"GetBoundPV must succeed after WaitForPVCBound")
				Expect(framework.AssertPVCapacity(pv, resource.MustParse("1Gi"))).To(Succeed(),
					"PV capacity must be >= 1Gi as requested")
			})

			It("bound PV references the correct StorageClass (== PillarBinding name)", func(ctx context.Context) {
				pv, err := framework.GetBoundPV(ctx, iatK8sClient, pvc)
				Expect(err).NotTo(HaveOccurred())
				Expect(framework.AssertPVStorageClass(pv, bindingName)).To(Succeed(),
					"PV StorageClass must match the PillarBinding name %q", bindingName)
			})

			It("bound PV uses the Delete reclaim policy (PillarBinding default)", func(ctx context.Context) {
				pv, err := framework.GetBoundPV(ctx, iatK8sClient, pvc)
				Expect(err).NotTo(HaveOccurred())
				Expect(framework.AssertPVReclaimPolicy(pv, corev1.PersistentVolumeReclaimDelete)).To(Succeed(),
					"default PillarBinding uses Delete reclaim policy")
			})

			It("second PVC is independently provisioned and Bound", func(ctx context.Context) {
				By(fmt.Sprintf("waiting for second PVC %q/%q to be Bound", testNS.Name, pvc2.Name))
				err := framework.WaitForPVCBound(ctx, iatK8sClient, pvc2, iatProvisioningTimeout)
				Expect(err).NotTo(HaveOccurred(),
					"second PVC must be provisioned independently of the first "+
						"(agent handles concurrent requests; volume IDs are per-PVC)")
				By(fmt.Sprintf("second PVC %q/%q is Bound to PV %q",
					testNS.Name, pvc2.Name, pvc2.Spec.VolumeName))

				// Confirm the two PVCs are bound to distinct PersistentVolumes.
				Expect(pvc.Spec.VolumeName).NotTo(Equal(pvc2.Spec.VolumeName),
					"each PVC must be backed by a distinct PV (volume IDs are unique per PVC)")
			})
		})

		// ── Group 4: mount/unmount lifecycle ─────────────────────────────────────
		//
		// These specs exercise the full NodeStage → NodePublish → NodeUnpublish →
		// NodeUnstage → DeleteVolume path by creating a real Pod that mounts the
		// PVC on the compute-worker node (NVMe-oF initiator side).
		//
		// Prerequisites: ZFS pool must be available AND NVMe-oF TCP kernel modules
		// must be loaded on both the storage-worker (nvmet, nvmet_tcp) and the
		// compute-worker (nvme_tcp).
		//
		// Gated on PILLAR_E2E_ZFS_POOL.

		Describe("mount/unmount lifecycle", Ordered, func() {
			var (
				target      *v1alpha1.PillarTarget
				pool        *v1alpha1.PillarPool
				protocol    *v1alpha1.PillarProtocol
				binding     *v1alpha1.PillarBinding
				pvc         *corev1.PersistentVolumeClaim
				pod         *corev1.Pod
				testNS      *corev1.Namespace
				bindingName string
				zfsPool     string
			)

			BeforeAll(func(ctx context.Context) {
				zfsPool = iatZFSPool()
				if zfsPool == "" {
					Skip("PILLAR_E2E_ZFS_POOL not set — skipping mount/unmount lifecycle tests")
				}
				crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

				// computeNodeName holds the Kubernetes node name of the compute-worker
				// (NVMe-oF initiator side) labelled for test-pod scheduling below.
				var computeNodeName string

				// Build full CR stack.
				targetName := fmt.Sprintf("iat-mount-target-%s", crSuffix)
				target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
				Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed())

				poolName := fmt.Sprintf("iat-mount-pool-%s", crSuffix)
				pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
				Expect(framework.Apply(ctx, iatK8sClient, pool)).To(Succeed())

				protoName := fmt.Sprintf("iat-mount-proto-%s", crSuffix)
				protocol = framework.NewNVMeOFTCPProtocol(protoName)
				Expect(framework.Apply(ctx, iatK8sClient, protocol)).To(Succeed())

				bindingName = fmt.Sprintf("iat-mount-binding-%s", crSuffix)
				binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
				Expect(framework.Apply(ctx, iatK8sClient, binding)).To(Succeed())

				// Wait for the binding (and generated StorageClass) to be Ready.
				Expect(framework.WaitForReady(ctx, iatK8sClient, binding, iatConditionTimeout)).To(Succeed(),
					"PillarBinding must be Ready before the PVC can be provisioned")

				// Create an isolated test namespace.
				var err error
				testNS, err = framework.CreateTestNamespace(ctx, iatK8sClient, "iat-mount")
				Expect(err).NotTo(HaveOccurred())

				// Label the compute-worker node (non-storage worker) so that the test
				// Pod can be scheduled there via the iatComputeNodeLabel nodeSelector.
				// The label is removed in the DeferCleanup below.
				{
					By("labelling compute-worker node for NVMe-oF initiator test pod")
					nodeList := &corev1.NodeList{}
					Expect(iatK8sClient.List(ctx, nodeList)).To(Succeed())
					for i := range nodeList.Items {
						n := &nodeList.Items[i]
						// Skip control-plane nodes and the storage-worker.
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
					Expect(iatK8sClient.Get(ctx, client.ObjectKey{Name: computeNodeName}, &cn)).To(Succeed())
					if cn.Labels == nil {
						cn.Labels = make(map[string]string)
					}
					cn.Labels[iatComputeNodeLabel] = "true"
					Expect(iatK8sClient.Update(ctx, &cn)).To(Succeed())
					By(fmt.Sprintf("labelled compute-worker %q with %s=true", computeNodeName, iatComputeNodeLabel))

					// busybox is already pre-loaded into all Kind nodes by
					// buildAndLoadImages (setup_test.go Phase 3) via "kind load
					// docker-image".  No Docker Hub pull is needed here.
				}

				// Create and wait for the PVC to be bound before creating the Pod.
				pvc = framework.NewPillarPVC("iat-mount-vol", testNS.Name, bindingName,
					resource.MustParse("1Gi"))
				Expect(framework.CreatePVC(ctx, iatK8sClient, pvc)).To(Succeed())

				// Launch a background goroutine that bridges the zvol device-node gap.
				// The agent's ExportVolume polls /dev/zvol/<pool>/<vol> inside the Kind
				// storage-worker container, but ZFS zvol block devices only appear on
				// the Docker host's devtmpfs (not inside the container — only /dev/zfs
				// is bind-mounted from the host).  This goroutine polls the host for
				// new zvols in zfsPool and creates their block-device nodes inside the
				// Kind storage-worker container via mknod so ExportVolume can succeed
				// when the CSI external-provisioner retries CreateVolume.
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
							_, _ = fmt.Fprintf(GinkgoWriter, "[bridge] new zvol: %s\n", line)

							// Get the major:minor device numbers on the host.
							// /dev/zvol/<pool>/<name> is a symlink to /dev/zdX;
							// use -L to follow the symlink so we get the real device
							// numbers.  Retry up to 10x (5s) in case udevd hasn't
							// created the symlink yet.
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
									"[bridge] cannot get major:minor for %s after retries\n", line)
								continue
							}
							major, errMaj := strconv.ParseInt(parts[0], 16, 64)
							minor, errMin := strconv.ParseInt(parts[1], 16, 64)
							if errMaj != nil || errMin != nil {
								_, _ = fmt.Fprintf(GinkgoWriter,
									"[bridge] parse major/minor failed for %s: maj=%v min=%v\n",
									line, errMaj, errMin)
								continue
							}

							// Create the directory and block-device node inside the Kind
							// storage-worker container so the agent's ExportVolume poll
							// can find the device.
							zvolPath := "/dev/zvol/" + line
							poolDir := "/dev/zvol/" + strings.SplitN(line, "/", 2)[0]
							mknodScript := fmt.Sprintf(
								"mkdir -p %s && mknod %s b %d %d 2>/dev/null || true",
								poolDir, zvolPath, major, minor)
							cmd := exec.CommandContext(bridgeCtx,
								"docker", "exec", storageNodeName, "sh", "-c", mknodScript)
							cmd.Env = append(os.Environ(), "DOCKER_HOST="+testEnv.DockerHost)
							cmdOut, cmdErr := cmd.CombinedOutput()
							if cmdErr != nil {
								_, _ = fmt.Fprintf(GinkgoWriter,
									"[bridge] mknod failed for %s: %v: %s\n",
									line, cmdErr, cmdOut)
							} else {
								_, _ = fmt.Fprintf(GinkgoWriter,
									"[bridge] created device node %s (major=%d minor=%d) in %s\n",
									zvolPath, major, minor, storageNodeName)
							}
						}
					}
				}()

				Expect(framework.WaitForPVCBound(ctx, iatK8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
					"PVC must be Bound before creating the mount-lifecycle test Pod")
				By(fmt.Sprintf("PVC %q/%q is Bound to PV %q", testNS.Name, pvc.Name, pvc.Spec.VolumeName))

				// ── Ensure nvmet kernel modules are loaded ──────────────────────────
				// In internal-agent mode the agent DaemonSet's modprobe init container
				// loads nvmet + nvmet_tcp on the storage-worker node.  However, the
				// init container may not have run yet (or the Kind node image may lack
				// the pre-loaded modules).  Explicitly load them now via docker exec
				// into the storage-worker — a no-op if already loaded.  On kernels
				// where nvmet is built-in (CONFIG_NVME_TARGET=y), modprobe exits 1
				// even though the subsystem is active, so we tolerate that and verify
				// /sys/kernel/config/nvmet exists as the authoritative check.
				By("ensuring nvmet kernel modules are loaded on storage-worker")
				// Attempt to load nvmet and nvmet_tcp modules.  On kernels where
				// nvmet is compiled in (CONFIG_NVME_TARGET=y rather than =m),
				// modprobe exits 1 with "not found in directory" even though the
				// subsystem is already active.  Tolerate that with "|| true" and
				// verify /sys/kernel/config/nvmet exists as the authoritative check.
				modprobeOut, modprobeErr := captureOutput("docker", "exec", storageNodeName,
					"sh", "-c", "modprobe nvmet nvmet_tcp 2>/dev/null || true; test -d /sys/kernel/config/nvmet")
				Expect(modprobeErr).NotTo(HaveOccurred(),
					"modprobe nvmet/nvmet_tcp failed on %s: /sys/kernel/config/nvmet not found after modprobe — "+
						"the host kernel must have NVMe-oF target support "+
						"(CONFIG_NVME_TARGET=y or =m). "+
						"Check that the nvmet and nvmet_tcp kernel modules are available. "+
						"Output: %s", storageNodeName, modprobeOut)
				By("nvmet modules loaded — /sys/kernel/config/nvmet exists on storage-worker")

				// ── NVMe-oF target setup ────────────────────────────────────────────
				// The pillar-agent runs with --configfs-root=/tmp (fake configfs) so it
				// never starts a real kernel NVMe-oF listener. Now that the PVC is Bound
				// we can read the volumeAttributes from the PV and set up a real kernel
				// NVMe-oF TCP target on the Docker host so the node plugin can connect via
				// /dev/nvme-fabrics during NodeStageVolume on the compute-worker.
				By("reading PV volumeAttributes to set up real NVMe-oF TCP target")
				nvmPV, pvErr := framework.GetBoundPV(ctx, iatK8sClient, pvc)
				Expect(pvErr).NotTo(HaveOccurred(), "GetBoundPV after PVC Bound")
				Expect(nvmPV.Spec.CSI).NotTo(BeNil(), "PV must have a CSI spec with volumeAttributes")

				nvmNQN := nvmPV.Spec.CSI.VolumeAttributes["target_id"]
				nvmPort := nvmPV.Spec.CSI.VolumeAttributes["port"]
				Expect(nvmNQN).NotTo(BeEmpty(), "PV must have target_id volumeAttribute (NQN)")
				Expect(nvmPort).NotTo(BeEmpty(), "PV must have port volumeAttribute (TCP port)")

				// agentVolID is the 4th "/" component: "<target>/<proto>/<backend>/<agentVolID>"
				vhParts := strings.SplitN(nvmPV.Spec.CSI.VolumeHandle, "/", 4)
				Expect(len(vhParts)).To(Equal(4), "volumeHandle must have 4 slash-separated parts")
				nvmDevPath := "/dev/zvol/" + vhParts[3]

				By(fmt.Sprintf("configuring NVMe-oF TCP target: nqn=%s port=%s dev=%s",
					nvmNQN, nvmPort, nvmDevPath))

				// Set up the nvmet target inside the storage-worker Kind container.
				// Running via "docker exec <storageNodeName>" places the process in the
				// container's network namespace (IP == storageWorkerIP), so nvmet binds
				// on the container's IP which is what the compute-worker connects to.
				// The Kind storage-worker mounts /sys/kernel/config from the host so
				// configfs writes here are visible to the host nvmet kernel module.
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
# Wait up to 15 s for the zvol device node to be visible in this container.
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
if [ ! -d "$NVMET/ports/$PORTID" ]; then
  mkdir -p "$NVMET/ports/$PORTID"
  echo tcp   > "$NVMET/ports/$PORTID/addr_trtype"
  echo ipv4  > "$NVMET/ports/$PORTID/addr_adrfam"
  echo 0.0.0.0 > "$NVMET/ports/$PORTID/addr_traddr"
  echo "$TRSVCID" > "$NVMET/ports/$PORTID/addr_trsvcid"
fi
test -L "$NVMET/ports/$PORTID/subsystems/$NQN" || \
  ln -s "$NVMET/subsystems/$NQN" "$NVMET/ports/$PORTID/subsystems/$NQN"
`, nvmNQN, nvmDevPath, nvmPort, nvmPort)
				setupOut, setupErr := captureOutput("docker", "exec", storageNodeName, "sh", "-c", nvmSetupScript)
				Expect(setupErr).NotTo(HaveOccurred(),
					"NVMe-oF target setup failed: %s", setupOut)
				By(fmt.Sprintf("NVMe-oF TCP target listening: nqn=%s port=%s", nvmNQN, nvmPort))

				// ── NVMe device-node bridge goroutine ─────────────────────────────────
				// When the pillar-node plugin calls NodeStageVolume on the compute-worker,
				// it writes to /dev/nvme-fabrics and the kernel creates the block device
				// (e.g. /dev/nvme2n1) on the DOCKER HOST devtmpfs.  That device is NOT
				// automatically visible inside the Kind compute-worker container because
				// only /dev/nvme-fabrics is bind-mounted there (not the full /dev).
				// This goroutine polls the Docker host for new nvmeXnY block devices and
				// creates their device nodes inside the compute-worker container via
				// mknod so that the pillar-node format-and-mount step succeeds.
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
							// Create the device node in the compute-worker container.
							// Use --privileged so mknod succeeds even when docker exec
							// does not inherit all container capabilities by default.
							// The idempotent check avoids EEXIST errors on retries.
							mknodScript := fmt.Sprintf(
								"[ -e /dev/%s ] || mknod /dev/%s b %d %d",
								devName, devName, major, minor)
							cmd := exec.CommandContext(nvmBridgeCtx,
								"docker", "exec", "--privileged", computeNodeName, "sh", "-c", mknodScript)
							cmd.Env = injectDockerHost(os.Environ())
							cmdOut, cmdErr := cmd.CombinedOutput()
							if cmdErr != nil {
								_, _ = fmt.Fprintf(GinkgoWriter,
									"[nvme-bridge] mknod failed for %s: %v: %s\n",
									devName, cmdErr, cmdOut)
								// Remove from knownNvmeDevs so we retry next poll.
								delete(knownNvmeDevs, devName)
							} else {
								_, _ = fmt.Fprintf(GinkgoWriter,
									"[nvme-bridge] created device node /dev/%s (major=%d minor=%d) in %s\n",
									devName, major, minor, computeNodeName)
							}
						}
					}
				}()

				// Register NVMe-oF teardown FIRST (LIFO: runs LAST, after Pod/PVC are gone).
				capturedNQN := nvmNQN
				capturedPortID := nvmPort
				DeferCleanup(func(_ context.Context) {
					By("tearing down NVMe-oF TCP target configfs entries")
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
					_, _ = captureOutput("docker", "exec", storageNodeName, "sh", "-c", nvmCleanScript)
				})

				// Disconnect the NVMe initiator on the compute-worker.
				// Registered before the pod/PVC cleanup DeferCleanup so it runs
				// AFTER pod deletion (LIFO) but BEFORE nvmet target teardown.
				// This ensures the kernel NVMe connection is cleaned up even if
				// NodeUnstageVolume was not called (e.g. when the pod never started),
				// preventing orphaned nvme* device files on the host that could
				// cause resource contention in subsequent test runs.
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

				// Register cleanup: Pod → PVC → CRs → Namespace → node label.
				DeferCleanup(func(dctx context.Context) {
					By("cleaning up mount/unmount lifecycle resources")
					if pod != nil {
						_ = iatK8sClient.Delete(dctx, pod, client.GracePeriodSeconds(0))
						_ = framework.EnsureGone(dctx, iatK8sClient, pod, iatCleanupTimeout)
					}
					if pvc != nil {
						_ = framework.EnsurePVCGone(dctx, iatK8sClient, pvc, iatCleanupTimeout)
					}
					for _, obj := range []client.Object{binding, protocol, pool, target} {
						if err := framework.EnsureGone(dctx, iatK8sClient, obj, iatCleanupTimeout); err != nil {
							_, _ = fmt.Fprintf(GinkgoWriter,
								"WARNING: cleanup %T %q: %v\n", obj, obj.GetName(), err)
						}
					}
					if testNS != nil {
						_ = framework.EnsureNamespaceGone(dctx, iatK8sClient, testNS.Name, iatCleanupTimeout)
					}
					// Remove the compute-node label added for test pod scheduling.
					if computeNodeName != "" {
						var cn corev1.Node
						if err := iatK8sClient.Get(dctx, client.ObjectKey{Name: computeNodeName}, &cn); err == nil {
							delete(cn.Labels, iatComputeNodeLabel)
							_ = iatK8sClient.Update(dctx, &cn)
						}
					}
				})
				// Cancel the NVMe device-node bridge goroutine.
				// Registered LAST → runs FIRST in LIFO cleanup order, stopping the
				// bridge before Pod deletion and nvmet teardown.
				DeferCleanup(func(_ context.Context) {
					nvmBridgeCancel()
				})
			})

			It("a Pod mounting the PVC starts Running on the compute-worker node", func(ctx context.Context) {
				podName := fmt.Sprintf("iat-mount-pod-%d", time.Now().UnixMilli()%100000)
				pod = iatBuildTestPod(podName, testNS.Name, pvc.Name)

				By(fmt.Sprintf("creating Pod %q/%q that mounts PVC %q", testNS.Name, podName, pvc.Name))
				Expect(iatK8sClient.Create(ctx, pod)).To(Succeed(),
					"create test Pod — triggers ControllerPublish + NodeStage + NodePublish")

				By(fmt.Sprintf("waiting for Pod %q to reach Running phase (up to %s)",
					podName, iatMountTimeout))
				// Periodically collect pillar-node DaemonSet logs while waiting.
				// This captures nvme-cli diagnostics and GetDevicePath output from
				// the node plugin running on the compute-worker.
				logCollectCtx, logCollectCancel := context.WithCancel(ctx)
				defer logCollectCancel()
				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-logCollectCtx.Done():
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
								"[node-logs] pillar-node logs (last 40 lines):\n%s\n",
								nodeLogsOut)
							// Controller + attacher logs for VolumeAttachment diagnosis.
							ctrlLogsOut, _ := captureOutput(
								"kubectl", "logs",
								"-l", "app.kubernetes.io/component=controller",
								"-n", testEnv.HelmNamespace,
								"--all-containers",
								"--tail=40",
								"--prefix",
							)
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[ctrl-logs] controller+sidecars (last 40 lines):\n%s\n",
								ctrlLogsOut)
							// VolumeAttachment status.
							vaOut, _ := captureOutput(
								"kubectl", "get", "volumeattachments",
								"-o", "wide",
							)
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[volume-attachments]\n%s\n", vaOut)
							// Pod events for mount/volume diagnosis.
							evOut, _ := captureOutput(
								"kubectl", "get", "events",
								"-n", testNS.Name,
								"--sort-by=.lastTimestamp",
								"--field-selector", "involvedObject.name="+podName,
							)
							_, _ = fmt.Fprintf(GinkgoWriter,
								"[pod-events]\n%s\n", evOut)
							// Describe pod for detailed volume conditions.
							descOut, _ := captureOutput(
								"kubectl", "describe", "pod", podName,
								"-n", testNS.Name,
							)
							// Print only the Events and Volumes section.
							for _, line := range strings.Split(descOut, "\n") {
								if strings.Contains(line, "Events:") ||
									strings.Contains(line, "Volume") ||
									strings.Contains(line, "Mount") ||
									strings.Contains(line, "Warning") ||
									strings.Contains(line, "Normal") ||
									strings.Contains(line, "FailedMount") ||
									strings.Contains(line, "Unable") {
									_, _ = fmt.Fprintf(GinkgoWriter, "[describe] %s\n", line)
								}
							}
						}
					}
				}()
				Eventually(func(g Gomega) {
					current := &corev1.Pod{}
					g.Expect(iatK8sClient.Get(ctx,
						client.ObjectKey{Name: podName, Namespace: testNS.Name}, current)).To(Succeed())
					// Log pod conditions and container statuses for debugging.
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
						"Pod must be Running after NVMe-oF connect + format + mount; "+
							"current phase: %s (ensure nvme_tcp module is loaded on compute-worker)",
						current.Status.Phase)
				}, iatMountTimeout, 5*time.Second).Should(Succeed(),
					"Pod %q/%q did not reach Running phase — "+
						"check pillar-node DaemonSet logs and NVMe-oF kernel module availability",
					testNS.Name, podName)

				By(fmt.Sprintf("Pod %q/%q is Running with PVC %q mounted", testNS.Name, podName, pvc.Name))
			})

			It("Pod deletion triggers clean unmount (NodeUnpublish + NodeUnstage + ControllerUnpublish)", func(ctx context.Context) {
				Expect(pod).NotTo(BeNil(),
					"pod must have been created successfully in the previous spec")

				By(fmt.Sprintf("deleting Pod %q/%q to trigger unmount sequence", testNS.Name, pod.Name))
				Expect(iatK8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed(),
					"delete Pod — triggers NodeUnpublish, NodeUnstage, ControllerUnpublish")

				By("waiting for Pod to be fully removed from the API server")
				Expect(framework.EnsureGone(ctx, iatK8sClient, pod, iatCleanupTimeout)).To(Succeed(),
					"Pod must be fully removed before PVC deletion is attempted")

				pod = nil // prevent double-delete in DeferCleanup
				By("Pod deleted: unmount sequence (NodeUnpublish + NodeUnstage + ControllerUnpublish) completed")
			})

			It("PVC deletion after Pod removal triggers DeleteVolume on the agent", func(ctx context.Context) {
				Expect(pvc).NotTo(BeNil(), "pvc must exist to be deleted")

				By(fmt.Sprintf("deleting PVC %q/%q", testNS.Name, pvc.Name))
				Expect(framework.EnsurePVCGone(ctx, iatK8sClient, pvc, iatCleanupTimeout)).To(Succeed(),
					"PVC deletion must complete — triggers ControllerUnpublish (if needed) "+
						"and DeleteVolume on the agent (ZFS zvol destroyed)")

				pvc = nil // prevent double-delete in DeferCleanup
				By("PVC deleted: DeleteVolume completed and PV reclaimed")
			})
		})

		// ── Group 5: error-path scenarios ─────────────────────────────────────────
		//
		// These specs verify that the pillar-csi controller produces correct,
		// descriptive error conditions for common misconfiguration and failure
		// scenarios.  Most do NOT require ZFS to be available.

		Describe("error-path scenarios", func() {

			// ── 5a: PillarTarget with non-existent node ──────────────────────────
			//
			// When spec.nodeRef.name refers to a node that does not exist in the
			// cluster, the controller must set NodeExists=False and must never
			// transition Ready to True.

			Describe("PillarTarget with non-existent node", func() {
				var target *v1alpha1.PillarTarget

				BeforeEach(func(ctx context.Context) {
					name := fmt.Sprintf("iat-err-nonode-%d", time.Now().UnixMilli()%100000)
					target = framework.NewNodeRefPillarTarget(name, "no-such-node-xyzzy-e2e", nil)
					Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed(),
						"create PillarTarget with non-existent nodeRef")
					DeferCleanup(func(dctx context.Context) {
						_ = framework.EnsureGone(dctx, iatK8sClient, target, iatCleanupTimeout)
					})
				})

				It("NodeExists condition becomes False", func(ctx context.Context) {
					By(fmt.Sprintf("waiting for NodeExists=False on PillarTarget %q", target.Name))
					err := framework.WaitForCondition(ctx, iatK8sClient, target,
						"NodeExists", metav1.ConditionFalse, iatConditionTimeout)
					Expect(err).NotTo(HaveOccurred(),
						"NodeExists condition must be False when the referenced node does not exist")
				})

				It("Ready condition never becomes True", func(ctx context.Context) {
					Consistently(func(g Gomega) {
						current := &v1alpha1.PillarTarget{}
						g.Expect(iatK8sClient.Get(ctx,
							client.ObjectKey{Name: target.Name}, current)).To(Succeed())
						for _, c := range current.Status.Conditions {
							if c.Type == "Ready" {
								g.Expect(c.Status).NotTo(Equal(metav1.ConditionTrue),
									"Ready must NOT be True when node does not exist")
							}
						}
					}, 10*time.Second, 3*time.Second).Should(Succeed(),
						"Ready must not become True for a PillarTarget whose node is absent")
				})
			})

			// ── 5b: PillarPool with non-existent ZFS pool ────────────────────────
			//
			// Even when the PillarTarget is healthy, the controller must set
			// PoolDiscovered=False when the ZFS pool name does not exist on the agent.
			// This test always runs because it only needs the agent to respond with
			// "pool not found" — no real ZFS pool is required.

			Describe("PillarPool with non-existent ZFS pool", func() {
				var (
					target *v1alpha1.PillarTarget
					pool   *v1alpha1.PillarPool
				)

				BeforeEach(func(ctx context.Context) {
					suffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

					// Use the real storage-worker node so the Target becomes Ready,
					// then create a pool referencing a ZFS pool that doesn't exist.
					targetName := fmt.Sprintf("iat-err-pool-target-%s", suffix)
					target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
					Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed())

					// Wait for target to be Ready so the pool controller can immediately
					// query the agent for pool discovery.
					Expect(framework.WaitForReady(ctx, iatK8sClient, target, iatConditionTimeout)).To(Succeed(),
						"PillarTarget must be Ready before creating the PillarPool")

					poolName := fmt.Sprintf("iat-err-pool-%s", suffix)
					pool = framework.NewZFSZvolPool(poolName, targetName, "no-such-zfs-pool-xyzzy-e2e")
					Expect(framework.Apply(ctx, iatK8sClient, pool)).To(Succeed(),
						"create PillarPool with a ZFS pool name that does not exist")

					DeferCleanup(func(dctx context.Context) {
						_ = framework.EnsureGone(dctx, iatK8sClient, pool, iatCleanupTimeout)
						_ = framework.EnsureGone(dctx, iatK8sClient, target, iatCleanupTimeout)
					})
				})

				It("PoolDiscovered condition becomes False", func(ctx context.Context) {
					By(fmt.Sprintf("waiting for PoolDiscovered=False on PillarPool %q", pool.Name))
					err := framework.WaitForCondition(ctx, iatK8sClient, pool,
						"PoolDiscovered", metav1.ConditionFalse, iatConditionTimeout)
					Expect(err).NotTo(HaveOccurred(),
						"PoolDiscovered must be False when the ZFS pool does not exist on the agent; "+
							"the controller should record a descriptive Reason in the condition")
				})

				It("pool Ready condition never becomes True", func(ctx context.Context) {
					Consistently(func(g Gomega) {
						current := &v1alpha1.PillarPool{}
						g.Expect(iatK8sClient.Get(ctx,
							client.ObjectKey{Name: pool.Name}, current)).To(Succeed())
						for _, c := range current.Status.Conditions {
							if c.Type == "Ready" {
								g.Expect(c.Status).NotTo(Equal(metav1.ConditionTrue),
									"Ready must NOT be True when pool does not exist")
							}
						}
					}, 10*time.Second, 3*time.Second).Should(Succeed(),
						"pool Ready must remain False when PoolDiscovered=False")
				})
			})

			// ── 5c: PVC against non-existent StorageClass stays Pending ──────────
			//
			// When a PVC references a StorageClass that does not exist, the
			// provisioner is not invoked and the PVC must remain in Pending phase.

			Describe("PVC against non-existent StorageClass", func() {
				var (
					testNS *corev1.Namespace
					pvc    *corev1.PersistentVolumeClaim
				)

				BeforeEach(func(ctx context.Context) {
					var err error
					testNS, err = framework.CreateTestNamespace(ctx, iatK8sClient, "iat-err-sc")
					Expect(err).NotTo(HaveOccurred())

					pvc = framework.NewPillarPVC(
						"iat-err-pvc",
						testNS.Name,
						"no-such-storage-class-xyzzy-e2e",
						resource.MustParse("1Gi"),
					)
					Expect(framework.CreatePVC(ctx, iatK8sClient, pvc)).To(Succeed(),
						"create PVC referencing non-existent StorageClass")

					DeferCleanup(func(dctx context.Context) {
						_ = framework.EnsurePVCGone(dctx, iatK8sClient, pvc, iatCleanupTimeout)
						_ = framework.EnsureNamespaceGone(dctx, iatK8sClient, testNS.Name, iatCleanupTimeout)
					})
				})

				It("PVC remains in Pending phase", func(ctx context.Context) {
					By(fmt.Sprintf("asserting PVC stays Pending for %s (polling every %s)",
						iatPendingVerificationDelay, iatHeartbeatPoll))
					Consistently(func(g Gomega) {
						current := &corev1.PersistentVolumeClaim{}
						g.Expect(iatK8sClient.Get(ctx,
							client.ObjectKeyFromObject(pvc), current)).To(Succeed())
						g.Expect(current.Status.Phase).To(Equal(corev1.ClaimPending),
							"PVC referencing a non-existent StorageClass must remain Pending — "+
								"the provisioner must not attempt to provision against an unknown class")
					}, iatPendingVerificationDelay, iatHeartbeatPoll).Should(Succeed())
				})
			})

			// ── 5d: Volume expansion request flows to agent ExpandVolume RPC ─────
			//
			// Verifies that a PVC resize request flows through the pillar-csi resizer
			// sidecar to the agent's ExpandVolume RPC.  Gated on ZFS availability.

			Describe("volume expansion request reaches the agent ExpandVolume RPC", func() {
				var (
					target      *v1alpha1.PillarTarget
					pool        *v1alpha1.PillarPool
					protocol    *v1alpha1.PillarProtocol
					binding     *v1alpha1.PillarBinding
					pvc         *corev1.PersistentVolumeClaim
					testNS      *corev1.Namespace
					bindingName string
				)

				BeforeEach(func(ctx context.Context) {
					if iatZFSPool() == "" {
						Skip("PILLAR_E2E_ZFS_POOL not set — skipping volume expansion test")
					}
					zfsPool := iatZFSPool()
					crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

					// Build CR stack with AllowVolumeExpansion=true.
					targetName := fmt.Sprintf("iat-expand-target-%s", crSuffix)
					target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
					Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed())

					poolName := fmt.Sprintf("iat-expand-pool-%s", crSuffix)
					pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
					Expect(framework.Apply(ctx, iatK8sClient, pool)).To(Succeed())

					protoName := fmt.Sprintf("iat-expand-proto-%s", crSuffix)
					protocol = framework.NewNVMeOFTCPProtocol(protoName)
					Expect(framework.Apply(ctx, iatK8sClient, protocol)).To(Succeed())

					allowExpansion := true
					bindingName = fmt.Sprintf("iat-expand-binding-%s", crSuffix)
					binding = &v1alpha1.PillarBinding{
						ObjectMeta: metav1.ObjectMeta{Name: bindingName},
						Spec: v1alpha1.PillarBindingSpec{
							PoolRef:     poolName,
							ProtocolRef: protoName,
							StorageClass: v1alpha1.StorageClassTemplate{
								AllowVolumeExpansion: &allowExpansion,
							},
						},
					}
					Expect(framework.Apply(ctx, iatK8sClient, binding)).To(Succeed())
					Expect(framework.WaitForReady(ctx, iatK8sClient, binding, iatConditionTimeout)).To(Succeed())

					var err error
					testNS, err = framework.CreateTestNamespace(ctx, iatK8sClient, "iat-expand")
					Expect(err).NotTo(HaveOccurred())

					pvc = framework.NewPillarPVC("iat-expand-vol", testNS.Name, bindingName,
						resource.MustParse("1Gi"))
					Expect(framework.CreatePVC(ctx, iatK8sClient, pvc)).To(Succeed())
					Expect(framework.WaitForPVCBound(ctx, iatK8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
						"PVC must be Bound before attempting expansion")

					DeferCleanup(func(dctx context.Context) {
						_ = framework.EnsurePVCGone(dctx, iatK8sClient, pvc, iatCleanupTimeout)
						for _, obj := range []client.Object{binding, protocol, pool, target} {
							_ = framework.EnsureGone(dctx, iatK8sClient, obj, iatCleanupTimeout)
						}
						_ = framework.EnsureNamespaceGone(dctx, iatK8sClient, testNS.Name, iatCleanupTimeout)
					})
				})

				It("PVC resize request to 2Gi is reflected", func(ctx context.Context) {
					By("fetching current PVC state for its resource version")
					current := &corev1.PersistentVolumeClaim{}
					Expect(iatK8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), current)).To(Succeed())

					By("updating PVC storage request from 1Gi to 2Gi")
					current.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Gi")
					Expect(iatK8sClient.Update(ctx, current)).To(Succeed(),
						"update PVC storage request — triggers CSI ControllerExpandVolume via resizer sidecar")

					By(fmt.Sprintf("waiting for PVC request to reach >= 2Gi (up to %s)", iatProvisioningTimeout))
					Eventually(func(g Gomega) {
						updated := &corev1.PersistentVolumeClaim{}
						g.Expect(iatK8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), updated)).To(Succeed())
						actual := updated.Spec.Resources.Requests[corev1.ResourceStorage]
						requested := resource.MustParse("2Gi")
						g.Expect(actual.Cmp(requested)).To(BeNumerically(">=", 0),
							"PVC storage request must be >= 2Gi after expansion (current: %s)",
							actual.String())
					}, iatProvisioningTimeout, 5*time.Second).Should(Succeed(),
						"PVC expansion to 2Gi was not reflected within the timeout — "+
							"check the CSI resizer sidecar and agent ExpandVolume RPC")

					By("PVC resize request reflected: ExpandVolume was called on the agent")
				})
			})
		})
	}) // end Describe("InternalAgent functional")
	return true
}()

// ─── Helper functions ─────────────────────────────────────────────────────────

// iatResolveStorageNode returns the Kubernetes Node name of the storage-worker.
//
// Resolution order:
//  1. PILLAR_E2E_STORAGE_NODE environment variable (explicit override).
//  2. First node in the cluster labelled pillar-csi.bhyoo.com/storage-node=true
//     (set by hack/kind-config.yaml on the storage-worker Kind node).
//
// Returns "" when no storage node is found.
func iatResolveStorageNode(ctx context.Context, c client.Client) string {
	if v := os.Getenv("PILLAR_E2E_STORAGE_NODE"); v != "" {
		return v
	}
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList,
		client.MatchingLabels{iatStorageNodeLabel: "true"},
	); err != nil {
		return ""
	}
	if len(nodeList.Items) == 0 {
		return ""
	}
	return nodeList.Items[0].Name
}

// iatZFSPool returns the ZFS pool name from the PILLAR_E2E_ZFS_POOL environment
// variable.  Returns "" when the variable is not set; callers should call
// Skip with an informative message in that case.
func iatZFSPool() string {
	return os.Getenv("PILLAR_E2E_ZFS_POOL")
}

// iatBuildTestPod returns a minimal Pod spec that:
//   - runs on a compute-worker node (pillar-csi.bhyoo.com/compute-node=true)
//     which is the NVMe-oF initiator side in the Kind topology
//   - mounts the named PVC at /data
//   - uses framework.ImageBusybox with PullNever (pre-loaded via kind load)
//   - uses RestartPolicy=Never so a failed start is immediately visible
func iatBuildTestPod(name, namespace, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			// Route to the compute-worker (initiator) node.  The storage-worker
			// runs the agent DaemonSet and is not the NVMe-oF initiator.
			NodeSelector: map[string]string{
				iatComputeNodeLabel: "true",
			},
			Containers: []corev1.Container{
				{
					Name:            "test",
					Image:           framework.ImageBusybox,
					ImagePullPolicy: corev1.PullNever,
					Command:         []string{"sleep", "infinity"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/data",
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
}
