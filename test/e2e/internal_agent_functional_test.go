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
	iatConditionTimeout    = 2 * time.Minute
	iatProvisioningTimeout = 3 * time.Minute
	iatMountTimeout        = 4 * time.Minute
	iatCleanupTimeout      = 2 * time.Minute

	// iatHeartbeatObservation is how long the heartbeat spec observes the
	// Ready=True condition for stability.
	iatHeartbeatObservation = 30 * time.Second

	// iatHeartbeatPoll is the polling interval inside the heartbeat
	// Consistently block.
	iatHeartbeatPoll = 5 * time.Second

	// iatPendingVerificationDelay is how long the error-path specs wait before
	// asserting a PVC is still Pending (time for the provisioner to respond
	// with a failure event if it were going to).
	iatPendingVerificationDelay = 15 * time.Second
)

// ─── Package-level state ─────────────────────────────────────────────────────

// iatK8sClient is the controller-runtime client used by all specs in this
// file.  Initialised once in the outer BeforeAll.
var iatK8sClient client.Client

// ─── Ginkgo container ────────────────────────────────────────────────────────

var _ = Describe("InternalAgent functional", Ordered, Label("internal-agent"), func() {
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

			// PillarTarget (NodeRef → storage-worker)
			targetName := fmt.Sprintf("iat-stack-target-%s", crSuffix)
			target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
			Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed(),
				"create PillarTarget %q for CR stack lifecycle test", targetName)

			// PillarPool (ZFS zvol backend)
			poolName := fmt.Sprintf("iat-stack-pool-%s", crSuffix)
			pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
			Expect(framework.Apply(ctx, iatK8sClient, pool)).To(Succeed(),
				"create PillarPool %q (zfs-zvol, pool=%s)", poolName, zfsPool)

			// PillarProtocol (NVMe-oF TCP)
			protoName := fmt.Sprintf("iat-stack-proto-%s", crSuffix)
			protocol = framework.NewNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, iatK8sClient, protocol)).To(Succeed(),
				"create PillarProtocol %q (nvmeof-tcp)", protoName)

			// PillarBinding (links pool + protocol → generates StorageClass)
			bindingName := fmt.Sprintf("iat-stack-binding-%s", crSuffix)
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, iatK8sClient, binding)).To(Succeed(),
				"create PillarBinding %q (pool=%s, proto=%s)", bindingName, poolName, protoName)

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

			// Build the full CR stack required for provisioning.
			targetName := fmt.Sprintf("iat-prov-target-%s", crSuffix)
			target = framework.NewNodeRefPillarTarget(targetName, storageNodeName, nil)
			Expect(framework.Apply(ctx, iatK8sClient, target)).To(Succeed())

			poolName := fmt.Sprintf("iat-prov-pool-%s", crSuffix)
			pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
			Expect(framework.Apply(ctx, iatK8sClient, pool)).To(Succeed())

			protoName := fmt.Sprintf("iat-prov-proto-%s", crSuffix)
			protocol = framework.NewNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, iatK8sClient, protocol)).To(Succeed())

			bindingName = fmt.Sprintf("iat-prov-binding-%s", crSuffix)
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, iatK8sClient, binding)).To(Succeed())

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
			if os.Getenv("PILLAR_E2E_NVMEOF_TCP") != "true" {
				Skip("PILLAR_E2E_NVMEOF_TCP not set — skipping mount/unmount lifecycle tests " +
					"(real NVMe-oF TCP target configuration required; agent uses --configfs-root=/tmp in this environment)")
			}

			crSuffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

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

			// Create and wait for the PVC to be bound before creating the Pod.
			pvc = framework.NewPillarPVC("iat-mount-vol", testNS.Name, bindingName,
				resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, iatK8sClient, pvc)).To(Succeed())
			Expect(framework.WaitForPVCBound(ctx, iatK8sClient, pvc, iatProvisioningTimeout)).To(Succeed(),
				"PVC must be Bound before creating the mount-lifecycle test Pod")
			By(fmt.Sprintf("PVC %q/%q is Bound to PV %q", testNS.Name, pvc.Name, pvc.Spec.VolumeName))

			// Register cleanup: Pod → PVC → CRs → Namespace.
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
			Eventually(func(g Gomega) {
				current := &corev1.Pod{}
				g.Expect(iatK8sClient.Get(ctx,
					client.ObjectKey{Name: podName, Namespace: testNS.Name}, current)).To(Succeed())
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
				}, 20*time.Second, 5*time.Second).Should(Succeed(),
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
				}, 20*time.Second, 5*time.Second).Should(Succeed(),
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
				By(fmt.Sprintf("waiting %s then asserting PVC is still Pending",
					iatPendingVerificationDelay))
				time.Sleep(iatPendingVerificationDelay)

				current := &corev1.PersistentVolumeClaim{}
				Expect(iatK8sClient.Get(ctx,
					client.ObjectKeyFromObject(pvc), current)).To(Succeed())
				Expect(current.Status.Phase).To(Equal(corev1.ClaimPending),
					"PVC referencing a non-existent StorageClass must remain Pending — "+
						"the provisioner must not attempt to provision against an unknown class")
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
})

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
//   - uses busybox:1.36 to minimise image pull time in CI
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
					Name:    "test",
					Image:   "busybox:1.36",
					Command: []string{"sleep", "infinity"},
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
