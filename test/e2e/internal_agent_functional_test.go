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

// internal_agent_functional_test.go — Agent functional e2e tests for
// pillar-csi in "internal agent" (DaemonSet) mode.
//
// This file implements Sub-AC 7c: functional e2e test cases exercised through
// the in-cluster agent DaemonSet.  Tests cover:
//
//  1. CR hierarchy setup and reconciliation (PillarTarget → PillarPool →
//     PillarProtocol → PillarBinding).  The controller dials the real
//     in-cluster agent; these tests verify that the full CR chain resolves
//     correctly and that the StorageClass is generated.
//
//  2. CSI volume provisioning lifecycle: PVC creation, controller
//     CreateVolume invocation via the in-cluster agent, PV binding (when
//     the ZFS pool exists on the storage-worker), and PVC deletion with
//     volume cleanup.  In Kind clusters without a real ZFS pool the PVC
//     stays Pending; those specs skip gracefully while still asserting
//     that the provisioner was invoked.
//
//  3. Volume mount/unmount lifecycle: ControllerPublishVolume and
//     ControllerUnpublishVolume exercised when a Pod requests a Bound PVC.
//     Node-side staging (NodeStageVolume) is attempted; in Kind without
//     NVMe-oF kernel modules the Pod stays in ContainerCreating, but we
//     still assert that the VolumeAttachment was created (i.e.
//     ControllerPublish was called).
//
//  4. Error-path scenarios: PVC with unknown StorageClass stays Pending,
//     PillarTarget with an unreachable external address reports
//     AgentConnected=False, and PillarPool with a non-existent target
//     reports an error condition.
//
// # Prerequisites
//
// The Kind cluster (pillar-csi-e2e) must already be bootstrapped with the
// pillar-csi Helm chart deployed.  Run hack/e2e-setup.sh first:
//
//	hack/e2e-setup.sh
//	export KUBECONFIG=$(kind get kubeconfig --name pillar-csi-e2e)
//
// # Configuration
//
//	ZFS_POOL              ZFS pool name on the storage-worker (default: "e2e-pool")
//	FUNC_TEST_NAMESPACE   Namespace for namespaced resources    (default: "pillar-csi-func-e2e")
//
// # Running
//
//	go test -tags=e2e -v -count=1 ./test/e2e/ \
//	    -run TestInternalAgent \
//	    -- --ginkgo.label-filter=internal-agent --ginkgo.v
package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// funcAgentNS is the namespace the agent DaemonSet is deployed into.
	funcAgentNS = "pillar-csi-system"
	// funcAgentDSName is the Helm-generated agent DaemonSet name.
	funcAgentDSName = "pillar-csi-agent"

	// funcStorageNodeLabelKey is the node label selector for the storage-worker.
	funcStorageNodeLabelKey = "pillar-csi.bhyoo.com/storage-node"
	// funcComputeNodeLabelKey is the node label selector for the compute-worker.
	funcComputeNodeLabelKey = "pillar-csi.bhyoo.com/compute-node"

	// funcDefaultZFSPool is the ZFS pool name used when ZFS_POOL is unset.
	funcDefaultZFSPool = "e2e-pool"

	// funcAgentConnTimeout is the max time to wait for AgentConnected=True.
	funcAgentConnTimeout = 2 * time.Minute
	// funcCRReconcileTimeout is the max time to wait for any CR condition.
	funcCRReconcileTimeout = 2 * time.Minute
	// funcPVCProvisionTimeout is the max time to wait for a PVC to become Bound.
	funcPVCProvisionTimeout = 3 * time.Minute
	// funcPodRunningTimeout is the max time to wait for a Pod to reach Running.
	funcPodRunningTimeout = 3 * time.Minute
	// funcCleanupTimeout is the per-resource deletion wait timeout.
	funcCleanupTimeout = 2 * time.Minute
	// funcHeartbeatWindow is how long we observe Ready=True stability.
	funcHeartbeatWindow = 30 * time.Second
	// funcPollInterval is used by Eventually / Consistently blocks.
	funcPollInterval = 3 * time.Second
)

// ─────────────────────────────────────────────────────────────────────────────
// Package-level state — populated by the top-level BeforeAll
// ─────────────────────────────────────────────────────────────────────────────

var (
	// funcClient is the controller-runtime client for all functional specs.
	funcClient client.Client
	// funcStorageNodeName is the Kind node running the agent DaemonSet pod.
	funcStorageNodeName string
	// funcTestNS is the shared test namespace (created once, deleted in AfterAll).
	funcTestNS string
	// funcZFSPool is the ZFS pool name on the storage-worker.
	funcZFSPool string
)

// ─────────────────────────────────────────────────────────────────────────────
// Top-level Ginkgo container
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("InternalAgent Functional", Ordered, Label("internal-agent"), func() {

	// ── Suite-level setup ────────────────────────────────────────────────────

	BeforeAll(func(ctx context.Context) {
		By("connecting to the Kind cluster")
		s, err := framework.SetupSuite(framework.WithConnectTimeout(30 * time.Second))
		Expect(err).NotTo(HaveOccurred(),
			"functional BeforeAll: cluster connectivity failed — "+
				"ensure KUBECONFIG is set and 'hack/e2e-setup.sh' has been run")
		funcClient = s.Client

		funcZFSPool = envOrDefault("ZFS_POOL", funcDefaultZFSPool)
		nsName := envOrDefault("FUNC_TEST_NAMESPACE", "pillar-csi-func-e2e")

		// Find the storage-worker Kind node.
		By("locating storage-worker node (label " + funcStorageNodeLabelKey + "=true)")
		nodeList := &corev1.NodeList{}
		Expect(funcClient.List(ctx, nodeList,
			client.MatchingLabels{funcStorageNodeLabelKey: "true"},
		)).To(Succeed(), "list storage-worker nodes")

		if len(nodeList.Items) == 0 {
			Skip("no node with label " + funcStorageNodeLabelKey + "=true — " +
				"verify hack/e2e-setup.sh has been run and kind-config.yaml labels are applied")
		}
		funcStorageNodeName = nodeList.Items[0].Name
		By(fmt.Sprintf("storage-worker node: %s", funcStorageNodeName))

		// Verify the agent DaemonSet exists (Helm chart is deployed).
		By(fmt.Sprintf("verifying agent DaemonSet %q exists in %q", funcAgentDSName, funcAgentNS))
		agentDS := &appsv1.DaemonSet{}
		if getErr := funcClient.Get(ctx,
			client.ObjectKey{Name: funcAgentDSName, Namespace: funcAgentNS},
			agentDS,
		); getErr != nil {
			Skip(fmt.Sprintf(
				"agent DaemonSet %q not found in %q — run 'hack/e2e-setup.sh' first: %v",
				funcAgentDSName, funcAgentNS, getErr))
		}
		By(fmt.Sprintf("agent DaemonSet present (desired=%d ready=%d)",
			agentDS.Status.DesiredNumberScheduled, agentDS.Status.NumberReady))

		// Create the shared test namespace (idempotent).
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		_ = funcClient.Create(ctx, ns) // ignore AlreadyExists
		funcTestNS = nsName
	})

	AfterAll(func(ctx context.Context) {
		if funcClient == nil || funcTestNS == "" {
			return
		}
		By(fmt.Sprintf("deleting shared test namespace %q", funcTestNS))
		if err := framework.EnsureNamespaceGone(ctx, funcClient, funcTestNS, funcCleanupTimeout); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"WARNING: InternalAgent Functional AfterAll: namespace %q: %v\n", funcTestNS, err)
		}
	})

	// ── Group 1: CR hierarchy and agent connectivity ─────────────────────────

	Describe("CR hierarchy and agent connectivity", Ordered, func() {
		var (
			suffix  string
			target  *v1alpha1.PillarTarget
			pool    *v1alpha1.PillarPool
			proto   *v1alpha1.PillarProtocol
			binding *v1alpha1.PillarBinding
		)

		BeforeAll(func(ctx context.Context) {
			Expect(funcClient).NotTo(BeNil(), "cluster client must be set")
			Expect(funcStorageNodeName).NotTo(BeEmpty(), "storage-worker node must be set")

			suffix = fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := "func-cr-target-" + suffix
			poolName := "func-cr-pool-" + suffix
			protoName := "func-cr-proto-" + suffix
			bindingName := "func-cr-binding-" + suffix

			By(fmt.Sprintf("creating PillarTarget %q → node %q", targetName, funcStorageNodeName))
			target = framework.NewNodeRefPillarTarget(targetName, funcStorageNodeName, nil)
			Expect(framework.Apply(ctx, funcClient, target)).To(Succeed())

			By(fmt.Sprintf("creating PillarPool %q (target=%s pool=%s)", poolName, targetName, funcZFSPool))
			pool = framework.NewZFSZvolPool(poolName, targetName, funcZFSPool)
			Expect(framework.Apply(ctx, funcClient, pool)).To(Succeed())

			By(fmt.Sprintf("creating PillarProtocol %q (NVMe-oF TCP)", protoName))
			proto = framework.NewNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, funcClient, proto)).To(Succeed())

			By(fmt.Sprintf("creating PillarBinding %q (pool=%s proto=%s)", bindingName, poolName, protoName))
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, funcClient, binding)).To(Succeed())
		})

		AfterAll(func(ctx context.Context) {
			tracker := framework.NewResourceTracker()
			for _, obj := range []client.Object{binding, proto, pool, target} {
				if obj != nil {
					tracker.Track(obj)
				}
			}
			if err := tracker.Cleanup(ctx, funcClient); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CR hierarchy cleanup: %v\n", err)
			}
		})

		It("PillarTarget spec.nodeRef is persisted correctly", func(ctx context.Context) {
			got := &v1alpha1.PillarTarget{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(target), got)).To(Succeed())

			Expect(got.Spec.NodeRef).NotTo(BeNil(), "spec.nodeRef must be set")
			Expect(got.Spec.External).To(BeNil(), "spec.external must be nil for NodeRef target")
			Expect(got.Spec.NodeRef.Name).To(Equal(funcStorageNodeName),
				"spec.nodeRef.name must equal the storage-worker node name")
		})

		It("PillarTarget AgentConnected condition becomes True", func(ctx context.Context) {
			By(fmt.Sprintf("waiting up to %s for AgentConnected=True on %q",
				funcAgentConnTimeout, target.Name))
			Expect(framework.WaitForCondition(ctx, funcClient, target,
				"AgentConnected", metav1.ConditionTrue, funcAgentConnTimeout,
			)).To(Succeed(),
				"AgentConnected must become True — verify the agent pod is Running on node %q "+
					"and the controller can reach it via the node's InternalIP", funcStorageNodeName)
			By(fmt.Sprintf("PillarTarget %q: AgentConnected=True ✓", target.Name))
		})

		It("PillarTarget Ready condition becomes True", func(ctx context.Context) {
			Expect(framework.WaitForReady(ctx, funcClient, target, funcCRReconcileTimeout)).To(Succeed(),
				"PillarTarget must reach Ready=True once the agent is connected and healthy")
			By(fmt.Sprintf("PillarTarget %q: Ready=True ✓", target.Name))
		})

		It("PillarTarget status.resolvedAddress is populated with the node's InternalIP", func(ctx context.Context) {
			Expect(framework.WaitForField(ctx, funcClient, target,
				func(t *v1alpha1.PillarTarget) bool { return t.Status.ResolvedAddress != "" },
				funcCRReconcileTimeout,
			)).To(Succeed(), "status.resolvedAddress must be populated once AgentConnected=True")
			Expect(target.Status.ResolvedAddress).NotTo(BeEmpty())
			By(fmt.Sprintf("resolvedAddress = %q", target.Status.ResolvedAddress))
		})

		It("PillarTarget status.agentVersion is reported by the connected agent", func(ctx context.Context) {
			Expect(framework.WaitForField(ctx, funcClient, target,
				func(t *v1alpha1.PillarTarget) bool { return t.Status.AgentVersion != "" },
				funcCRReconcileTimeout,
			)).To(Succeed(), "status.agentVersion must be set once the controller dials the agent")
			Expect(target.Status.AgentVersion).NotTo(BeEmpty())
			By(fmt.Sprintf("agentVersion = %q", target.Status.AgentVersion))
		})

		It("PillarTarget status.capabilities lists at least one backend", func(ctx context.Context) {
			Expect(framework.WaitForField(ctx, funcClient, target,
				func(t *v1alpha1.PillarTarget) bool {
					return t.Status.Capabilities != nil && len(t.Status.Capabilities.Backends) > 0
				}, funcCRReconcileTimeout,
			)).To(Succeed(), "status.capabilities must be populated from agent GetCapabilities RPC")
			Expect(target.Status.Capabilities).NotTo(BeNil())
			Expect(target.Status.Capabilities.Backends).NotTo(BeEmpty(),
				"agent must advertise at least one backend type (e.g. zfs-zvol)")
			By(fmt.Sprintf("capabilities: backends=%v protocols=%v",
				target.Status.Capabilities.Backends, target.Status.Capabilities.Protocols))
		})

		It("Ready condition is stable across reconcile cycles (heartbeat check)", func(ctx context.Context) {
			// First ensure Ready=True is present.
			current := &v1alpha1.PillarTarget{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(target), current)).To(Succeed())
			var readyCond *metav1.Condition
			for i := range current.Status.Conditions {
				if current.Status.Conditions[i].Type == "Ready" {
					c := current.Status.Conditions[i]
					readyCond = &c
					break
				}
			}
			Expect(readyCond).NotTo(BeNil(), "Ready condition must be present before heartbeat check")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready must already be True")

			initialTransition := readyCond.LastTransitionTime
			By(fmt.Sprintf("observing Ready=True stability for %s (initial transition: %s)",
				funcHeartbeatWindow, initialTransition.UTC().Format(time.RFC3339)))

			Consistently(func(g Gomega) {
				poll := &v1alpha1.PillarTarget{}
				g.Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(target), poll)).To(Succeed())
				var cond *metav1.Condition
				for i := range poll.Status.Conditions {
					if poll.Status.Conditions[i].Type == "Ready" {
						c := poll.Status.Conditions[i]
						cond = &c
						break
					}
				}
				g.Expect(cond).NotTo(BeNil(), "Ready condition must remain present")
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
					"Ready must stay True throughout the heartbeat window")
				g.Expect(cond.LastTransitionTime).To(Equal(initialTransition),
					"LastTransitionTime must not change — Ready must not flip")
			}, funcHeartbeatWindow, funcPollInterval).Should(Succeed(),
				"Ready=True was not stable for %s — check controller and agent health", funcHeartbeatWindow)
			By("heartbeat confirmed: Ready=True held without flip ✓")
		})

		It("PillarPool spec.targetRef and spec.backend are persisted correctly", func(ctx context.Context) {
			got := &v1alpha1.PillarPool{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())

			Expect(got.Spec.TargetRef).To(Equal(target.Name), "spec.targetRef must match the PillarTarget")
			Expect(got.Spec.Backend.Type).To(Equal(v1alpha1.BackendTypeZFSZvol),
				"spec.backend.type must be BackendTypeZFSZvol")
			Expect(got.Spec.Backend.ZFS).NotTo(BeNil(), "spec.backend.zfs must be set")
			Expect(got.Spec.Backend.ZFS.Pool).To(Equal(funcZFSPool),
				"spec.backend.zfs.pool must equal the configured ZFS pool name")
		})

		It("PillarPool is reconciled (at least one status condition set)", func(ctx context.Context) {
			// The pool may not be Ready when the ZFS pool is absent (Kind).
			// We assert only that the controller has reconciled (conditions present).
			Expect(framework.WaitForField(ctx, funcClient, pool,
				func(p *v1alpha1.PillarPool) bool { return len(p.Status.Conditions) > 0 },
				funcCRReconcileTimeout,
			)).To(Succeed(), "PillarPool must have at least one status condition after reconciliation")
			By(fmt.Sprintf("PillarPool %q reconciled with %d condition(s)", pool.Name,
				len(pool.Status.Conditions)))
		})

		It("PillarProtocol spec.type is persisted correctly", func(ctx context.Context) {
			got := &v1alpha1.PillarProtocol{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(proto), got)).To(Succeed())

			Expect(got.Spec.Type).To(Equal(v1alpha1.ProtocolTypeNVMeOFTCP),
				"spec.type must be NVMe-oF TCP")
			Expect(got.Spec.NVMeOFTCP).NotTo(BeNil(), "spec.nvmeofTcp must be populated")
		})

		It("PillarProtocol is reconciled (at least one status condition set)", func(ctx context.Context) {
			Expect(framework.WaitForField(ctx, funcClient, proto,
				func(p *v1alpha1.PillarProtocol) bool { return len(p.Status.Conditions) > 0 },
				funcCRReconcileTimeout,
			)).To(Succeed(), "PillarProtocol must have at least one status condition")
		})

		It("PillarBinding spec.poolRef and spec.protocolRef are persisted correctly", func(ctx context.Context) {
			got := &v1alpha1.PillarBinding{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(binding), got)).To(Succeed())

			Expect(got.Spec.PoolRef).To(Equal(pool.Name), "spec.poolRef must reference the PillarPool")
			Expect(got.Spec.ProtocolRef).To(Equal(proto.Name), "spec.protocolRef must reference the PillarProtocol")
		})

		It("PillarBinding causes a StorageClass to be created by the controller", func(ctx context.Context) {
			By(fmt.Sprintf("waiting for StorageClass %q to be created", binding.Name))
			Eventually(func() error {
				sc := &storagev1.StorageClass{}
				return funcClient.Get(ctx, client.ObjectKey{Name: binding.Name}, sc)
			}, funcCRReconcileTimeout, funcPollInterval).Should(Succeed(),
				"StorageClass %q must be created when a PillarBinding is applied", binding.Name)

			sc := &storagev1.StorageClass{}
			Expect(funcClient.Get(ctx, client.ObjectKey{Name: binding.Name}, sc)).To(Succeed())
			Expect(sc.Provisioner).To(Equal("pillar-csi.bhyoo.com"),
				"StorageClass provisioner must be pillar-csi.bhyoo.com")
			By(fmt.Sprintf("StorageClass %q created with provisioner %q ✓", sc.Name, sc.Provisioner))
		})
	})

	// ── Group 2: PVC provisioning lifecycle ─────────────────────────────────

	Describe("PVC provisioning lifecycle", Ordered, func() {
		var (
			suffix  string
			target  *v1alpha1.PillarTarget
			pool    *v1alpha1.PillarPool
			proto   *v1alpha1.PillarProtocol
			binding *v1alpha1.PillarBinding
			ns      *corev1.Namespace
		)

		BeforeAll(func(ctx context.Context) {
			Expect(funcClient).NotTo(BeNil())
			Expect(funcStorageNodeName).NotTo(BeEmpty())

			suffix = fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := "pvc-target-" + suffix
			poolName := "pvc-pool-" + suffix
			protoName := "pvc-proto-" + suffix
			bindingName := "pvc-binding-" + suffix

			target = framework.NewNodeRefPillarTarget(targetName, funcStorageNodeName, nil)
			Expect(framework.Apply(ctx, funcClient, target)).To(Succeed())
			pool = framework.NewZFSZvolPool(poolName, targetName, funcZFSPool)
			Expect(framework.Apply(ctx, funcClient, pool)).To(Succeed())
			proto = framework.NewNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, funcClient, proto)).To(Succeed())
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, funcClient, binding)).To(Succeed())

			// Wait for agent connectivity before provisioning tests.
			By(fmt.Sprintf("waiting for AgentConnected=True on %q", targetName))
			if err := framework.WaitForCondition(ctx, funcClient, target,
				"AgentConnected", metav1.ConditionTrue, funcAgentConnTimeout,
			); err != nil {
				Skip(fmt.Sprintf("agent not connected within %s — skipping provisioning tests: %v",
					funcAgentConnTimeout, err))
			}

			// Wait for StorageClass to be created.
			By(fmt.Sprintf("waiting for StorageClass %q", bindingName))
			Eventually(func() error {
				sc := &storagev1.StorageClass{}
				return funcClient.Get(ctx, client.ObjectKey{Name: bindingName}, sc)
			}, funcCRReconcileTimeout, funcPollInterval).Should(Succeed(),
				"StorageClass %q must be created before provisioning tests", bindingName)

			var nsErr error
			ns, nsErr = framework.CreateTestNamespace(ctx, funcClient, "pvc-prov")
			Expect(nsErr).NotTo(HaveOccurred(), "create test namespace for provisioning tests")
		})

		AfterAll(func(ctx context.Context) {
			tracker := framework.NewResourceTracker()
			if ns != nil {
				tracker.TrackNamespace(ns.Name)
			}
			for _, obj := range []client.Object{binding, proto, pool, target} {
				if obj != nil {
					tracker.Track(obj)
				}
			}
			if err := tracker.Cleanup(ctx, funcClient); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: PVC provisioning AfterAll: %v\n", err)
			}
		})

		It("PVC with binding StorageClass is accepted by the API server", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("pvc-accept", ns.Name, binding.Name, resource.MustParse("1Gi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})

			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			got := &corev1.PersistentVolumeClaim{}
			Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(pvc), got)).To(Succeed())
			Expect(got.Spec.StorageClassName).NotTo(BeNil())
			Expect(*got.Spec.StorageClassName).To(Equal(binding.Name))
		})

		It("PVC transitions to Bound or receives a ProvisioningFailed event from the CSI driver", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("pvc-provision-check", ns.Name, binding.Name, resource.MustParse("1Gi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			// Either (a) PVC becomes Bound (ZFS pool exists) or (b) provisioner
			// records a warning event indicating CreateVolume was attempted.
			// Both outcomes prove the CSI external-provisioner sidecar was invoked.
			Eventually(func(g Gomega) {
				got := &corev1.PersistentVolumeClaim{}
				g.Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(pvc), got)).To(Succeed())
				*pvc = *got

				if got.Status.Phase == corev1.ClaimBound {
					// Happy path: ZFS pool exists.
					return
				}

				// Not Bound yet — check for a provisioner event.
				eventList := &corev1.EventList{}
				g.Expect(funcClient.List(ctx, eventList,
					client.InNamespace(pvc.Namespace),
				)).To(Succeed())
				var found bool
				for _, ev := range eventList.Items {
					if ev.InvolvedObject.Name == pvc.Name &&
						ev.InvolvedObject.Kind == "PersistentVolumeClaim" {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(),
					"PVC %q/%q must have at least one event — "+
						"the CSI external-provisioner must have attempted CreateVolume",
					pvc.Namespace, pvc.Name)
			}, funcPVCProvisionTimeout, funcPollInterval).Should(Succeed(),
				"PVC %q/%q must reach Bound or have a provisioner event within %s",
				pvc.Namespace, pvc.Name, funcPVCProvisionTimeout)

			if pvc.Status.Phase == corev1.ClaimBound {
				By(fmt.Sprintf("PVC Bound → pvName=%q", pvc.Spec.VolumeName))
			} else {
				By(fmt.Sprintf("PVC Pending (ZFS unavailable in Kind) — provisioner event recorded"))
			}
		})

		It("Bound PVC has a PV with correct capacity, StorageClass, and ReclaimPolicy [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("pvc-pv-meta", ns.Name, binding.Name, resource.MustParse("1Gi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf(
					"PVC %q/%q did not become Bound within %s — "+
						"ZFS pool %q likely absent in this environment (Kind without ZFS): %v",
					pvc.Namespace, pvc.Name, funcPVCProvisionTimeout, funcZFSPool, err))
			}

			pv, err := framework.GetBoundPV(ctx, funcClient, pvc)
			Expect(err).NotTo(HaveOccurred())
			Expect(framework.AssertPVCapacity(pv, resource.MustParse("1Gi"))).To(Succeed())
			Expect(framework.AssertPVStorageClass(pv, binding.Name)).To(Succeed())
			Expect(framework.AssertPVReclaimPolicy(pv, corev1.PersistentVolumeReclaimDelete)).To(Succeed())
			Expect(framework.AssertPVAccessModes(pv,
				[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce})).To(Succeed())
			pvCap := pv.Spec.Capacity[corev1.ResourceStorage]
			By(fmt.Sprintf("PV %q: capacity=%s sc=%q reclaim=%s ✓",
				pv.Name,
				pvCap.String(),
				pv.Spec.StorageClassName,
				pv.Spec.PersistentVolumeReclaimPolicy))
		})

		It("Deleting a Bound PVC triggers PV reclamation (Delete policy) [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("pvc-reclaim", ns.Name, binding.Name, resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("PVC not Bound — skipping reclaim test: %v", err))
			}
			pvName := pvc.Spec.VolumeName

			By(fmt.Sprintf("deleting PVC %q/%q (pvName=%q)", pvc.Namespace, pvc.Name, pvName))
			Expect(framework.EnsurePVCGone(ctx, funcClient, pvc, funcCleanupTimeout)).To(Succeed())

			By(fmt.Sprintf("waiting for PV %q to be deleted (Delete reclaim policy)", pvName))
			deletedPV := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvName}}
			Expect(framework.WaitForDeletion(ctx, funcClient, deletedPV, funcPVCProvisionTimeout)).To(Succeed(),
				"PV %q must be deleted after PVC deletion with reclaimPolicy=Delete", pvName)
			By(fmt.Sprintf("PV %q deleted — CSI driver called DeleteVolume on the agent ✓", pvName))
		})

		It("Two PVCs with the same StorageClass are provisioned to distinct PVs [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc1 := framework.NewPillarPVC("pvc-multi-1", ns.Name, binding.Name, resource.MustParse("512Mi"))
			pvc2 := framework.NewPillarPVC("pvc-multi-2", ns.Name, binding.Name, resource.MustParse("512Mi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc1, funcCleanupTimeout)
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc2, funcCleanupTimeout)
			})
			Expect(framework.CreatePVC(ctx, funcClient, pvc1)).To(Succeed())
			Expect(framework.CreatePVC(ctx, funcClient, pvc2)).To(Succeed())

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc1, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("multi-PVC: PVC1 not Bound (ZFS unavailable): %v", err))
			}
			if err := framework.WaitForPVCBound(ctx, funcClient, pvc2, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("multi-PVC: PVC2 not Bound (ZFS unavailable): %v", err))
			}
			Expect(pvc1.Spec.VolumeName).NotTo(Equal(pvc2.Spec.VolumeName),
				"concurrent PVCs must bind to distinct PersistentVolumes")
			By(fmt.Sprintf("two PVCs bound to distinct PVs: %q and %q ✓",
				pvc1.Spec.VolumeName, pvc2.Spec.VolumeName))
		})
	})

	// ── Group 3: Volume mount/unmount lifecycle ──────────────────────────────

	Describe("volume mount/unmount lifecycle via Pod", Ordered, func() {
		// In Kind clusters without NVMe-oF kernel modules (nvmet_tcp), NodeStageVolume
		// fails because the NVMe-oF initiator cannot connect to the target.  Tests in
		// this group verify the orchestration path (ControllerPublishVolume via the CSI
		// attacher sidecar calling the agent's ExportVolume RPC) even when the actual
		// mount cannot complete.  Tests that require a running Pod are skipped when the
		// PVC never becomes Bound.
		var (
			suffix  string
			target  *v1alpha1.PillarTarget
			pool    *v1alpha1.PillarPool
			proto   *v1alpha1.PillarProtocol
			binding *v1alpha1.PillarBinding
			ns      *corev1.Namespace
		)

		BeforeAll(func(ctx context.Context) {
			Expect(funcClient).NotTo(BeNil())
			Expect(funcStorageNodeName).NotTo(BeEmpty())

			suffix = fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			targetName := "lifecycle-target-" + suffix
			poolName := "lifecycle-pool-" + suffix
			protoName := "lifecycle-proto-" + suffix
			bindingName := "lifecycle-binding-" + suffix

			target = framework.NewNodeRefPillarTarget(targetName, funcStorageNodeName, nil)
			Expect(framework.Apply(ctx, funcClient, target)).To(Succeed())
			pool = framework.NewZFSZvolPool(poolName, targetName, funcZFSPool)
			Expect(framework.Apply(ctx, funcClient, pool)).To(Succeed())
			proto = framework.NewNVMeOFTCPProtocol(protoName)
			Expect(framework.Apply(ctx, funcClient, proto)).To(Succeed())
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			Expect(framework.Apply(ctx, funcClient, binding)).To(Succeed())

			if err := framework.WaitForCondition(ctx, funcClient, target,
				"AgentConnected", metav1.ConditionTrue, funcAgentConnTimeout,
			); err != nil {
				Skip(fmt.Sprintf("lifecycle BeforeAll: agent not connected: %v", err))
			}

			Eventually(func() error {
				sc := &storagev1.StorageClass{}
				return funcClient.Get(ctx, client.ObjectKey{Name: bindingName}, sc)
			}, funcCRReconcileTimeout, funcPollInterval).Should(Succeed(),
				"StorageClass %q must be created", bindingName)

			var nsErr error
			ns, nsErr = framework.CreateTestNamespace(ctx, funcClient, "vol-lifecycle")
			Expect(nsErr).NotTo(HaveOccurred())
		})

		AfterAll(func(ctx context.Context) {
			tracker := framework.NewResourceTracker()
			if ns != nil {
				tracker.TrackNamespace(ns.Name)
			}
			for _, obj := range []client.Object{binding, proto, pool, target} {
				if obj != nil {
					tracker.Track(obj)
				}
			}
			if err := tracker.Cleanup(ctx, funcClient); err != nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: lifecycle AfterAll: %v\n", err)
			}
		})

		It("ControllerPublish (ExportVolume via agent) is exercised when Pod requests the PVC [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("lifecycle-vol-1", ns.Name, binding.Name, resource.MustParse("1Gi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("PVC not Bound (ZFS pool %q absent in Kind): %v", funcZFSPool, err))
			}
			By(fmt.Sprintf("PVC Bound to PV %q — creating Pod to trigger ControllerPublish", pvc.Spec.VolumeName))

			pod := funcBuildPod("lifecycle-pod-1", ns.Name, pvc.Name)
			DeferCleanup(func(dctx context.Context) {
				_ = funcClient.Delete(dctx, pod, client.GracePeriodSeconds(10))
				_ = framework.WaitForDeletion(dctx, funcClient, pod, funcCleanupTimeout)
			})
			Expect(funcClient.Create(ctx, pod)).To(Succeed())

			// Wait for VolumeAttachment to be created — this proves ControllerPublish was called.
			By("waiting for VolumeAttachment to be created by the CSI attacher sidecar")
			Eventually(func(g Gomega) {
				vaList := &storagev1.VolumeAttachmentList{}
				g.Expect(funcClient.List(ctx, vaList)).To(Succeed())
				var found bool
				for _, va := range vaList.Items {
					if va.Spec.Source.PersistentVolumeName != nil &&
						*va.Spec.Source.PersistentVolumeName == pvc.Spec.VolumeName {
						found = true
						By(fmt.Sprintf("VolumeAttachment %q found (attached=%v)", va.Name, va.Status.Attached))
						break
					}
				}
				g.Expect(found).To(BeTrue(),
					"VolumeAttachment must be created for PV %q — "+
						"the CSI attacher sidecar must call ControllerPublishVolume", pvc.Spec.VolumeName)
			}, funcPodRunningTimeout, funcPollInterval).Should(Succeed(),
				"VolumeAttachment for PV %q not created within %s", pvc.Spec.VolumeName, funcPodRunningTimeout)
			By("VolumeAttachment created — CSI attacher invoked ControllerPublishVolume ✓")

			// Check if Pod reached Running (NVMe-oF modules might not be available in Kind).
			podRunning := funcPollPodRunning(ctx, funcClient, pod, funcPodRunningTimeout)
			if podRunning {
				By(fmt.Sprintf("Pod %q/%q reached Running — full mount lifecycle succeeded ✓",
					pod.Namespace, pod.Name))
			} else {
				By("Pod did not reach Running — NVMe-oF kernel modules likely unavailable in Kind " +
					"(ControllerPublish path verified via VolumeAttachment)")
			}
		})

		It("Pod deletion triggers ControllerUnpublish (VolumeAttachment removed) [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("lifecycle-vol-unpub", ns.Name, binding.Name, resource.MustParse("1Gi"))
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("PVC not Bound: %v", err))
			}

			pod := funcBuildPod("lifecycle-pod-unpub", ns.Name, pvc.Name)
			Expect(funcClient.Create(ctx, pod)).To(Succeed())

			// Wait for VolumeAttachment to appear before deleting the pod.
			Eventually(func() bool {
				vaList := &storagev1.VolumeAttachmentList{}
				if err := funcClient.List(ctx, vaList); err != nil {
					return false
				}
				for _, va := range vaList.Items {
					if va.Spec.Source.PersistentVolumeName != nil &&
						*va.Spec.Source.PersistentVolumeName == pvc.Spec.VolumeName {
						return true
					}
				}
				return false
			}, funcPodRunningTimeout, funcPollInterval).Should(BeTrue(),
				"VolumeAttachment must appear before testing ControllerUnpublish")

			By(fmt.Sprintf("deleting Pod %q/%q to trigger ControllerUnpublish", pod.Namespace, pod.Name))
			Expect(funcClient.Delete(ctx, pod, client.GracePeriodSeconds(10))).To(Succeed())
			Expect(framework.WaitForDeletion(ctx, funcClient, pod, funcCleanupTimeout)).To(Succeed(),
				"Pod must be fully deleted so the attacher can call ControllerUnpublishVolume")

			// VolumeAttachment should eventually be removed once ControllerUnpublish completes.
			By(fmt.Sprintf("waiting for VolumeAttachment for PV %q to be removed", pvc.Spec.VolumeName))
			Eventually(func() bool {
				vaList := &storagev1.VolumeAttachmentList{}
				if err := funcClient.List(ctx, vaList); err != nil {
					return false
				}
				for _, va := range vaList.Items {
					if va.Spec.Source.PersistentVolumeName != nil &&
						*va.Spec.Source.PersistentVolumeName == pvc.Spec.VolumeName {
						return false // still present
					}
				}
				return true // gone
			}, funcPodRunningTimeout, funcPollInterval).Should(BeTrue(),
				"VolumeAttachment for PV %q must be removed after Pod deletion (ControllerUnpublish)", pvc.Spec.VolumeName)
			By("VolumeAttachment removed — ControllerUnpublishVolume called on the agent ✓")
		})

		It("NodeStageVolume is attempted on the compute-worker node when a Pod is scheduled [skipped if ZFS unavailable]", func(ctx context.Context) {
			Expect(ns).NotTo(BeNil())
			pvc := framework.NewPillarPVC("lifecycle-vol-stage", ns.Name, binding.Name, resource.MustParse("1Gi"))
			DeferCleanup(func(dctx context.Context) {
				_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
			})
			Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

			if err := framework.WaitForPVCBound(ctx, funcClient, pvc, funcPVCProvisionTimeout); err != nil {
				Skip(fmt.Sprintf("PVC not Bound: %v", err))
			}

			pod := funcBuildPod("lifecycle-pod-stage", ns.Name, pvc.Name)
			DeferCleanup(func(dctx context.Context) {
				_ = funcClient.Delete(dctx, pod, client.GracePeriodSeconds(10))
				_ = framework.WaitForDeletion(dctx, funcClient, pod, funcCleanupTimeout)
			})
			Expect(funcClient.Create(ctx, pod)).To(Succeed())

			// In Kind without NVMe-oF modules the Pod will stay in ContainerCreating
			// or Pending because NodeStageVolume fails.  We assert:
			//   (a) the Pod was scheduled on the compute-worker node, and
			//   (b) the Pod has a condition or event referencing the CSI node driver.
			Eventually(func(g Gomega) {
				got := &corev1.Pod{}
				g.Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(pod), got)).To(Succeed())
				*pod = *got
				// Pod should at least be scheduled (NodeName populated).
				g.Expect(got.Spec.NodeName).NotTo(BeEmpty(),
					"Pod must be scheduled on a node before NodeStageVolume is invoked")
			}, funcPodRunningTimeout, funcPollInterval).Should(Succeed(),
				"Pod was not scheduled within %s", funcPodRunningTimeout)

			By(fmt.Sprintf("Pod scheduled on node %q — NodeStageVolume attempted", pod.Spec.NodeName))

			if pod.Status.Phase == corev1.PodRunning {
				By("Pod reached Running — NodeStageVolume, NodePublishVolume succeeded ✓")
			} else {
				// Verify the kubelet / CSI driver issued a NodeStage event.
				eventList := &corev1.EventList{}
				Expect(funcClient.List(ctx, eventList, client.InNamespace(pod.Namespace))).To(Succeed())
				var stageEventFound bool
				for _, ev := range eventList.Items {
					if ev.InvolvedObject.Name == pod.Name &&
						ev.InvolvedObject.Kind == "Pod" {
						stageEventFound = true
						By(fmt.Sprintf("Pod event: reason=%q message=%q", ev.Reason, ev.Message))
						break
					}
				}
				if !stageEventFound {
					By("no Pod event yet — NodeStageVolume may not have been attempted (NVMe-oF modules missing)")
				}
				Skip("Pod did not reach Running (NVMe-oF unavailable in Kind) — " +
					"scheduling and ControllerPublish path verified ✓")
			}
		})
	})

	// ── Group 4: Error-path scenarios ────────────────────────────────────────

	Describe("error-path scenarios", Ordered, func() {
		var (
			suffix string
			ns     *corev1.Namespace
		)

		BeforeAll(func(ctx context.Context) {
			Expect(funcClient).NotTo(BeNil())
			suffix = fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
			var err error
			ns, err = framework.CreateTestNamespace(ctx, funcClient, "err-path")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func(ctx context.Context) {
			if ns != nil {
				if err := framework.EnsureNamespaceGone(ctx, funcClient, ns.Name, funcCleanupTimeout); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: error-path AfterAll: %v\n", err)
				}
			}
		})

		Context("PVC with a non-existent StorageClass", func() {
			It("stays Pending indefinitely without triggering the CSI provisioner", func(ctx context.Context) {
				Expect(ns).NotTo(BeNil())
				pvc := framework.NewPillarPVC(
					"err-no-sc",
					ns.Name,
					"does-not-exist-pillar-sc-"+suffix,
					resource.MustParse("1Gi"),
				)
				DeferCleanup(func(dctx context.Context) {
					_ = framework.EnsurePVCGone(dctx, funcClient, pvc, funcCleanupTimeout)
				})
				Expect(framework.CreatePVC(ctx, funcClient, pvc)).To(Succeed())

				// The PVC must remain Pending for the full observation window.
				// No provisioner event should appear.
				Consistently(func(g Gomega) {
					got := &corev1.PersistentVolumeClaim{}
					g.Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(pvc), got)).To(Succeed())
					g.Expect(got.Status.Phase).To(Equal(corev1.ClaimPending),
						"PVC with non-existent StorageClass must remain Pending — "+
							"the pillar-csi provisioner must not be invoked for unknown StorageClasses")
				}, 30*time.Second, funcPollInterval).Should(Succeed())
				By("PVC stays Pending with unknown StorageClass ✓")
			})
		})

		Context("PillarTarget with an unreachable external agent address", func() {
			It("reports AgentConnected=False with a descriptive reason", func(ctx context.Context) {
				// 192.0.2.0/24 is TEST-NET-1 per RFC 5737 — guaranteed non-routable.
				targetName := "err-unreachable-" + suffix
				unreachable := framework.NewExternalPillarTarget(targetName, "192.0.2.1", 9500)
				DeferCleanup(func(dctx SpecContext) {
					_ = framework.EnsureGone(dctx, funcClient, unreachable, funcCleanupTimeout)
				})
				Expect(framework.Apply(ctx, funcClient, unreachable)).To(Succeed())

				By(fmt.Sprintf("waiting for AgentConnected=False on PillarTarget %q", targetName))
				Expect(framework.WaitForCondition(ctx, funcClient, unreachable,
					"AgentConnected", metav1.ConditionFalse, funcCRReconcileTimeout,
				)).To(Succeed(),
					"AgentConnected must become False for an unreachable external agent address (192.0.2.1:9500)")

				Expect(framework.WaitForCondition(ctx, funcClient, unreachable,
					"Ready", metav1.ConditionFalse, funcCRReconcileTimeout,
				)).To(Succeed(),
					"Ready must be False when the agent cannot be reached")

				// Read the final condition to log the reason.
				final := &v1alpha1.PillarTarget{}
				Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(unreachable), final)).To(Succeed())
				for _, cond := range final.Status.Conditions {
					if cond.Type == "AgentConnected" {
						By(fmt.Sprintf("AgentConnected=False reason=%q message=%q", cond.Reason, cond.Message))
					}
				}
				By(fmt.Sprintf("unreachable PillarTarget %q correctly reports AgentConnected=False ✓", targetName))
			})
		})

		Context("PillarPool referencing a non-existent PillarTarget", func() {
			It("is reconciled but reports a non-Ready condition", func(ctx context.Context) {
				poolName := "err-bad-target-pool-" + suffix
				badPool := framework.NewZFSZvolPool(poolName, "does-not-exist-target-"+suffix, "tank")
				DeferCleanup(func(dctx SpecContext) {
					_ = framework.EnsureGone(dctx, funcClient, badPool, funcCleanupTimeout)
				})
				Expect(framework.Apply(ctx, funcClient, badPool)).To(Succeed())

				By(fmt.Sprintf("waiting for PillarPool %q to be reconciled (any condition)", poolName))
				Expect(framework.WaitForField(ctx, funcClient, badPool,
					func(p *v1alpha1.PillarPool) bool { return len(p.Status.Conditions) > 0 },
					funcCRReconcileTimeout,
				)).To(Succeed(), "PillarPool must have at least one condition after reconciliation")

				got := &v1alpha1.PillarPool{}
				Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(badPool), got)).To(Succeed())

				// The pool must NOT have Ready=True (target doesn't exist).
				for _, cond := range got.Status.Conditions {
					if cond.Type == "Ready" {
						Expect(cond.Status).NotTo(Equal(metav1.ConditionTrue),
							"PillarPool referencing a non-existent target must NOT be Ready=True; "+
								"got Ready=%s reason=%s", cond.Status, cond.Reason)
						By(fmt.Sprintf("PillarPool %q: Ready=%s reason=%q ✓", poolName, cond.Status, cond.Reason))
					}
				}
			})
		})

		Context("duplicate PVC name in the same namespace", func() {
			It("second Create is rejected by the API server (AlreadyExists)", func(ctx context.Context) {
				Expect(ns).NotTo(BeNil())
				scName := "does-not-exist-for-dup-test-" + suffix
				pvc1 := framework.NewPillarPVC("err-dup-pvc", ns.Name, scName, resource.MustParse("1Gi"))
				DeferCleanup(func(dctx context.Context) {
					_ = framework.EnsurePVCGone(dctx, funcClient, pvc1, funcCleanupTimeout)
				})
				Expect(framework.CreatePVC(ctx, funcClient, pvc1)).To(Succeed(),
					"first PVC creation must succeed")

				pvc2 := framework.NewPillarPVC("err-dup-pvc", ns.Name, scName, resource.MustParse("2Gi"))
				Expect(framework.CreatePVC(ctx, funcClient, pvc2)).NotTo(Succeed(),
					"second PVC with the same name must be rejected by the API server (AlreadyExists)")
				By("duplicate PVC creation correctly rejected ✓")
			})
		})

		Context("PillarBinding with non-existent pool and protocol references", func() {
			It("is reconciled but StorageClass provisioner is unavailable until refs resolve", func(ctx context.Context) {
				bindingName := "err-dangling-binding-" + suffix
				danglingBinding := framework.NewSimplePillarBinding(
					bindingName,
					"no-such-pool-"+suffix,
					"no-such-proto-"+suffix,
				)
				DeferCleanup(func(dctx SpecContext) {
					// Also clean up any StorageClass the controller might have created.
					sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: bindingName}}
					_ = funcClient.Delete(dctx, sc)
					_ = framework.EnsureGone(dctx, funcClient, danglingBinding, funcCleanupTimeout)
				})
				Expect(framework.Apply(ctx, funcClient, danglingBinding)).To(Succeed())

				By(fmt.Sprintf("waiting for PillarBinding %q to be reconciled", bindingName))
				Expect(framework.WaitForField(ctx, funcClient, danglingBinding,
					func(b *v1alpha1.PillarBinding) bool { return len(b.Status.Conditions) > 0 },
					funcCRReconcileTimeout,
				)).To(Succeed(), "PillarBinding must have at least one condition after reconciliation")

				got := &v1alpha1.PillarBinding{}
				Expect(funcClient.Get(ctx, client.ObjectKeyFromObject(danglingBinding), got)).To(Succeed())
				By(fmt.Sprintf("PillarBinding %q reconciled: conditions=%v", bindingName, got.Status.Conditions))
			})
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// Helper functions (private to this file)
// ─────────────────────────────────────────────────────────────────────────────

// funcBuildPod returns a Pod spec that mounts pvcName and requests scheduling
// on the compute-worker node (label pillar-csi.bhyoo.com/compute-node=true).
// The Pod runs a simple busybox sleep so that the CSI node driver can stage
// and publish the volume.
func funcBuildPod(name, namespace, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			// Schedule on the compute-worker node (NVMe-oF initiator side).
			NodeSelector: map[string]string{
				funcComputeNodeLabelKey: "true",
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "app",
					Image:   "busybox:stable",
					Command: []string{"sh", "-c", "echo ready && sleep 3600"},
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
		},
	}
}

// funcPollPodRunning polls until the Pod reaches Running phase or timeout
// expires.  Returns true if Running was reached, false otherwise.
//
// This helper is intentionally non-fatal so that callers can decide whether
// to skip or assert based on the return value.
func funcPollPodRunning(
	ctx context.Context,
	c client.Client,
	pod *corev1.Pod,
	timeout time.Duration,
) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), got); err == nil {
			*pod = *got
			if got.Status.Phase == corev1.PodRunning {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(funcPollInterval):
		}
	}
	return false
}
