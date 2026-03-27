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

// pvc_provision_e2e_test.go — E2E test for PVC provisioning via pillar-csi.
//
// This test validates the complete CSI dynamic provisioning path by:
//
//  1. Creating the four pillar-csi CRs in dependency order:
//     PillarTarget → PillarPool → PillarProtocol → PillarBinding
//  2. Waiting for the PillarBinding controller to auto-create a StorageClass
//     (StorageClassCreated condition = True on the binding).
//  3. Creating a PVC that references the auto-created StorageClass in a
//     freshly created, isolated test namespace.
//  4. Waiting for the PVC to reach the Bound phase (up to 5 minutes).
//  5. Retrieving the bound PV and asserting its fields:
//     - Capacity ≥ requested size (1 Gi)
//     - StorageClass name equals the binding name
//     - ReclaimPolicy == Delete
//     - AccessModes includes ReadWriteOnce
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true (or EXTERNAL_AGENT_ADDR set) so that the
//     out-of-cluster agent is reachable.  The PillarTarget controller must be
//     able to dial the agent to reach AgentConnected=True.
//   - The Helm chart must be installed (TestMain handles this) so that the
//     CSI controller plugin is running and can call CreateVolume on the agent.
//   - The ZFS pool (E2E_ZFS_POOL, default "e2e-pool") must exist on the remote
//     Docker host (TestMain creates this in setupZFSPool).
//
// # Cleanup
//
// DeferCleanup handlers are registered before any state is created so that
// the PVC, namespace, and all CRs are always removed — even when an assertion
// fails.  Cleanup order (LIFO):
//
//	PVC deleted (EnsurePVCGone)
//	  → test namespace deleted (EnsureNamespaceGone)
//	    → CRs deleted in reverse dependency order (Binding→Protocol→Pool→Target)
//	      → suite client closed (TeardownSuite)
//
// Note: because PVs are cluster-scoped and owned by the provisioner, the PV
// is reclaimed automatically when the PVC is deleted (ReclaimPolicy=Delete).
package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// PVCProvision validates the full dynamic provisioning path from StorageClass
// to a bound PersistentVolume.
//
// The Describe is Ordered so that each step runs in declaration order and a
// failure in an earlier step (e.g. StorageClass not created) causes subsequent
// steps to be skipped rather than running against incomplete state.
var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("PVCProvision", Ordered, func() {

		var (
			suite *framework.Suite
			stack *framework.KindE2EStack
			ns    *corev1.Namespace
			pvc   *corev1.PersistentVolumeClaim
		)

		// ── BeforeAll: cluster client + CR stack setup ───────────────────────────
		BeforeAll(func(ctx SpecContext) {
			var err error
			suite, err = framework.SetupSuite(
				framework.WithConnectTimeout(30 * time.Second),
			)
			Expect(err).NotTo(HaveOccurred(),
				"connect to Kind cluster — KUBECONFIG must point at the e2e cluster "+
					"(TestMain sets KUBECONFIG before m.Run() is called)")

			// Build a uniquely-named CR stack so parallel test runs on the same
			// cluster do not collide on resource names.
			prefix := framework.UniqueName("pvc-prov")
			// Use the cluster-accessible agent address (reachable from inside Kind pods).
			// TestMain sets EXTERNAL_AGENT_CLUSTER_ADDRESS automatically when
			// E2E_LAUNCH_EXTERNAL_AGENT=true; fall back to ExternalAgentAddr when not set.
			agentAddr := extAgentClusterAddress()
			if agentAddr == "" {
				agentAddr = testEnv.ExternalAgentAddr
			}
			stack = framework.NewKindE2EStack(prefix, agentAddr, testEnv.ZFSPoolName)

			// Register CR cleanup BEFORE creating any resources so that all CRs
			// are removed even when an assertion below fails.  CRs are deleted in
			// reverse dependency order (innermost dependents first).
			DeferCleanup(func(dctx SpecContext) {
				if suite == nil {
					return
				}
				for _, obj := range stack.ReverseObjects() {
					if err := framework.EnsureGone(dctx, suite.Client, obj, 2*time.Minute); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"warning: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				suite.TeardownSuite()
			})
		})

		// ── Step 1: Apply the full CR stack ─────────────────────────────────────
		//
		// Apply each CR individually (in dependency order) so that reviewers can
		// see exactly which step failed.
		It("Step 1: applies all four pillar-csi CRs to the cluster", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarTarget %q (external agent at %s)",
				stack.Target.Name, testEnv.ExternalAgentAddr))
			Expect(framework.Apply(ctx, suite.Client, stack.Target)).To(Succeed(),
				"PillarTarget must be accepted by the API server")

			By(fmt.Sprintf("applying PillarPool %q (ZFS pool %q)",
				stack.Pool.Name, testEnv.ZFSPoolName))
			Expect(framework.Apply(ctx, suite.Client, stack.Pool)).To(Succeed(),
				"PillarPool with ZFS zvol backend must be accepted by the API server")

			By(fmt.Sprintf("applying PillarProtocol %q (NVMe-oF TCP port %d)",
				stack.Proto.Name, framework.KindNVMeOFPort))
			Expect(framework.Apply(ctx, suite.Client, stack.Proto)).To(Succeed(),
				"PillarProtocol with NVMe-oF TCP must be accepted by the API server")

			By(fmt.Sprintf("applying PillarBinding %q (pool=%q, protocol=%q)",
				stack.Binding.Name, stack.Pool.Name, stack.Proto.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Binding)).To(Succeed(),
				"PillarBinding wiring pool and protocol must be accepted by the API server")

			By("verifying all CRs have a non-zero creationTimestamp (API server accepted them)")
			for _, obj := range stack.Objects() {
				Expect(obj.GetCreationTimestamp()).NotTo(Equal(metav1.Time{}),
					"%T %q must have a non-zero creationTimestamp after Apply",
					obj, obj.GetName())
			}
		})

		// ── Step 2: Wait for the StorageClass to be auto-created ─────────────────
		//
		// The PillarBinding controller reconciler creates a Kubernetes StorageClass
		// once both PillarPool and PillarProtocol are in Ready state.  We wait for
		// StorageClassCreated=True on the binding — the controller's authoritative
		// signal that the StorageClass is available for PVC creation.
		//
		// The 5-minute timeout accommodates:
		//   - PillarTarget controller dialling the agent (gRPC dial + health check)
		//   - PillarPool controller polling the agent for pool discovery
		//   - PillarProtocol controller computing bindingCount / activeTargets
		//   - PillarBinding controller reconciling (periodic requeue every 15 s)
		It("Step 2: StorageClass is auto-created by the PillarBinding controller",
			func(ctx SpecContext) {
				By(fmt.Sprintf(
					"waiting for PillarBinding %q StorageClassCreated=True (up to 5 m)",
					stack.Binding.Name))

				waitBinding := &v1alpha1.PillarBinding{}
				waitBinding.Name = stack.Binding.Name

				Expect(framework.WaitForCondition(
					ctx, suite.Client, waitBinding,
					"StorageClassCreated", metav1.ConditionTrue, 5*time.Minute,
				)).To(Succeed(),
					"PillarBinding %q must reach StorageClassCreated=True within 5 m — "+
						"check that the controller pod is running in namespace %q and that "+
						"PillarTarget %q reached AgentConnected=True",
					stack.Binding.Name, testEnv.HelmNamespace, stack.Target.Name,
				)

				By(fmt.Sprintf(
					"verifying PillarBinding.Status.StorageClassName == %q",
					stack.Binding.Name))
				Expect(waitBinding.Status.StorageClassName).To(Equal(stack.Binding.Name),
					"PillarBinding.Status.StorageClassName must equal the binding's name "+
						"when no spec.storageClass.name override is set")
			})

		// ── Step 3: Create test namespace and PVC ─────────────────────────────────
		//
		// The PVC is created in a freshly allocated, isolated Namespace so that
		// cleanup is simple (deleting the Namespace removes all namespaced resources)
		// and concurrent test runs on the same cluster do not collide.
		//
		// The StorageClass name equals the PillarBinding name (auto-created in Step 2).
		It("Step 3: creates a test namespace and a PVC referencing the StorageClass",
			func(ctx SpecContext) {
				// Create an isolated namespace for this PVC.
				var err error
				ns, err = framework.CreateTestNamespace(ctx, suite.Client, "pvc-prov")
				Expect(err).NotTo(HaveOccurred(),
					"create test namespace with prefix 'pvc-prov'")
				By(fmt.Sprintf("created test namespace %q", ns.Name))

				// Register namespace cleanup BEFORE the PVC so that the namespace
				// (and everything inside it) is removed on test failure.
				DeferCleanup(func(dctx SpecContext) {
					By(fmt.Sprintf("cleanup: deleting test namespace %q", ns.Name))
					if err := framework.EnsureNamespaceGone(dctx, suite.Client, ns.Name, 3*time.Minute); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"warning: cleanup EnsureNamespaceGone %q: %v\n", ns.Name, err)
					}
				})

				// Build the PVC.  StorageClass name == binding name (per pillar-csi convention).
				// Request 1 Gi — small enough to be cheap on a loopback-backed ZFS pool
				// but large enough to exercise the zvol allocation path.
				storageClassName := stack.Binding.Name
				pvcCapacity := resource.MustParse("1Gi")
				pvc = framework.NewPillarPVC(
					framework.UniqueName("pvc"),
					ns.Name,
					storageClassName,
					pvcCapacity,
				)

				By(fmt.Sprintf("creating PVC %q/%q (storageClass=%q, size=%s)",
					pvc.Namespace, pvc.Name, storageClassName, pvcCapacity.String()))
				Expect(framework.CreatePVC(ctx, suite.Client, pvc)).To(Succeed(),
					"PVC %q/%q with StorageClass %q must be accepted by the API server — "+
						"verify that the StorageClass exists (Step 2 must have succeeded)",
					pvc.Namespace, pvc.Name, storageClassName,
				)
			})

		// ── Step 4: PVC reaches Bound phase ──────────────────────────────────────
		//
		// Wait for the CSI controller plugin (running in the Kind cluster via Helm)
		// to reconcile the PVC:
		//   a) The external-provisioner sidecar calls CreateVolume on the CSI controller.
		//   b) The CSI controller calls CreateVolume on the pillar-agent gRPC API.
		//   c) The agent creates the ZFS zvol and responds with the volume ID.
		//   d) The external-provisioner creates a PersistentVolume and binds it to
		//      the PVC.
		//
		// The 5-minute timeout covers all async provisioner steps.
		It("Step 4: PVC reaches the Bound phase within 5 minutes", func(ctx SpecContext) {
			By(fmt.Sprintf("waiting for PVC %q/%q to reach Bound phase (up to 5 m)",
				pvc.Namespace, pvc.Name))

			Expect(framework.WaitForPVCBound(ctx, suite.Client, pvc, 5*time.Minute)).To(Succeed(),
				"PVC %q/%q must reach the Bound phase within 5 m — verify that:\n"+
					"  1. The pillar-csi controller plugin is running in namespace %q\n"+
					"  2. The external-provisioner sidecar is healthy\n"+
					"  3. The agent at %s can call CreateVolume on ZFS pool %q\n"+
					"  4. The ZFS pool %q exists on the remote host",
				pvc.Namespace, pvc.Name,
				testEnv.HelmNamespace,
				testEnv.ExternalAgentAddr,
				testEnv.ZFSPoolName, testEnv.ZFSPoolName,
			)

			By(fmt.Sprintf("PVC %q/%q is Bound — bound to PV %q",
				pvc.Namespace, pvc.Name, pvc.Spec.VolumeName))
		})

		// ── Step 5: Verify the bound PV ──────────────────────────────────────────
		//
		// After the PVC is Bound, retrieve the PersistentVolume and assert that the
		// pillar-csi provisioner set all expected fields:
		//
		//   - Capacity ≥ 1 Gi   — the provisioner must honour the request size
		//   - StorageClass       — must match the PillarBinding name (auto-created SC)
		//   - ReclaimPolicy      — must be Delete (the default set by the binding)
		//   - AccessModes        — must include ReadWriteOnce (block device capability)
		It("Step 5: the bound PV has the expected capacity, StorageClass, reclaimPolicy, and access modes",
			func(ctx SpecContext) {
				pv, err := framework.GetBoundPV(ctx, suite.Client, pvc)
				Expect(err).NotTo(HaveOccurred(),
					"GetBoundPV must succeed — PVC %q/%q must be Bound (Step 4 must have passed)",
					pvc.Namespace, pvc.Name)

				By(fmt.Sprintf("fetched PV %q — verifying fields", pv.Name))

				// ── 5a: Capacity ───────────────────────────────────────────────────
				wantCapacity := resource.MustParse("1Gi")
				Expect(framework.AssertPVCapacity(pv, wantCapacity)).To(Succeed(),
					"PV %q must have capacity ≥ %s — the provisioner must honour the "+
						"PVC request size",
					pv.Name, wantCapacity.String(),
				)

				// ── 5b: StorageClass ───────────────────────────────────────────────
				// The StorageClass name on the PV must match the auto-created SC that
				// was generated by the PillarBinding controller.
				Expect(framework.AssertPVStorageClass(pv, stack.Binding.Name)).To(Succeed(),
					"PV %q StorageClass must be %q (the PillarBinding name) — the "+
						"provisioner must record the originating StorageClass",
					pv.Name, stack.Binding.Name,
				)

				// ── 5c: ReclaimPolicy ──────────────────────────────────────────────
				// The PillarBinding controller sets reclaimPolicy=Delete on the
				// auto-created StorageClass.  The external-provisioner sidecar copies
				// the StorageClass reclaimPolicy to the provisioned PV.
				Expect(framework.AssertPVReclaimPolicy(pv, corev1.PersistentVolumeReclaimDelete)).To(Succeed(),
					"PV %q reclaimPolicy must be Delete — the StorageClass was created "+
						"with reclaimPolicy=Delete (pillar-csi default for ZFS zvol block devices)",
					pv.Name,
				)

				// ── 5d: AccessModes ────────────────────────────────────────────────
				// ZFS zvol block devices support ReadWriteOnce (exclusive block access
				// from a single node).  The PVC was created with ReadWriteOnce, so the
				// bound PV must include that access mode.
				Expect(framework.AssertPVAccessModes(pv,
					[]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				)).To(Succeed(),
					"PV %q must include ReadWriteOnce — ZFS zvol block devices support "+
						"exclusive read/write access from one node at a time",
					pv.Name,
				)

				pvCapacity := pv.Spec.Capacity[corev1.ResourceStorage]
				By(fmt.Sprintf(
					"PV %q verified: capacity=%s storageClass=%q reclaimPolicy=%s accessModes=%v",
					pv.Name,
					pvCapacity.String(),
					pv.Spec.StorageClassName,
					pv.Spec.PersistentVolumeReclaimPolicy,
					pv.Spec.AccessModes,
				))
			})
	}) // end Describe("PVCProvision")
	return true
}()
