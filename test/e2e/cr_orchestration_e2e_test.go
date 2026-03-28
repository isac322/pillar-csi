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

// cr_orchestration_e2e_test.go — E2E test for sequential CR creation and
// auto StorageClass generation.
//
// This test verifies the full Kubernetes controller reconciliation path by
// applying the four pillar-csi Custom Resources in dependency order and
// asserting that the PillarBinding controller automatically creates a
// Kubernetes StorageClass with the correct provisioner, parameters, reclaim
// policy, and volume binding mode.
//
// # CR dependency order
//
//	PillarTarget → PillarPool → PillarProtocol → PillarBinding → StorageClass
//
// The test applies each CR individually (in order) so that reviewers can see
// exactly which step failed when something goes wrong in CI.
//
// # What is verified
//
//  1. Each of the four CRs is accepted by the Kubernetes API server without
//     validation errors.
//  2. After the PillarBinding's StorageClassCreated condition becomes True
//     (the authoritative controller signal), a Kubernetes StorageClass exists
//     with the name equal to the binding's own name (no override set).
//  3. The StorageClass has the correct provisioner ("pillar-csi.bhyoo.com"),
//     reclaim policy (Delete), volume binding mode (Immediate), and
//     AllowVolumeExpansion=true (non-NFS default).
//  4. The StorageClass parameter map carries all expected keys that the CSI
//     controller/node plugin uses during provisioning:
//     - pillar-csi.bhyoo.com/pool        → binding.Spec.PoolRef
//     - pillar-csi.bhyoo.com/protocol    → binding.Spec.ProtocolRef
//     - pillar-csi.bhyoo.com/backend-type → "zfs-zvol"
//     - pillar-csi.bhyoo.com/protocol-type → "nvmeof-tcp"
//     - pillar-csi.bhyoo.com/target      → pool's targetRef (PillarTarget name)
//     - pillar-csi.bhyoo.com/zfs-pool    → ZFS pool name
//     - pillar-csi.bhyoo.com/nvmeof-port → "4421"
//     - pillar-csi.bhyoo.com/acl-enabled → "false"
//     - csi.storage.k8s.io/fstype        → "ext4"
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true (or EXTERNAL_AGENT_ADDR set) so that the
//     out-of-cluster agent is reachable.  The PillarTarget controller must be
//     able to dial the agent to reach AgentConnected=True, which is a
//     prerequisite for PillarPool becoming Ready, which in turn is required for
//     the PillarBinding controller to create the StorageClass.
//   - The Helm chart must be installed (TestMain handles this).
//
// # Cleanup
//
// A single DeferCleanup registered in BeforeAll deletes all four CRs in reverse
// dependency order (Binding → Protocol → Pool → Target).  The StorageClass is
// owned by the PillarBinding and is deleted by the controller when the binding
// is removed.
package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// CROrchestration tests the sequential CR creation path:
//
//	PillarTarget → PillarPool → PillarProtocol → PillarBinding → StorageClass
//
// The Describe is Ordered so that each step runs in declaration order and a
// failure in an earlier step causes subsequent steps to be skipped rather than
// running against a partially-constructed stack.
var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("CROrchestration", Ordered, func() {

		var (
			suite *framework.Suite
			stack *framework.KindE2EStack
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
			// cluster cannot collide on resource names.
			prefix := framework.UniqueName("cr-orch")
			// Use the cluster-accessible agent address (reachable from inside Kind pods).
			// TestMain sets EXTERNAL_AGENT_CLUSTER_ADDRESS automatically when
			// E2E_LAUNCH_EXTERNAL_AGENT=true; fall back to ExternalAgentAddr when not set.
			agentAddr := extAgentClusterAddress()
			if agentAddr == "" {
				agentAddr = testEnv.ExternalAgentAddr
			}
			stack = framework.NewKindE2EStack(prefix, agentAddr, testEnv.ZFSPoolName)

			// Register cleanup BEFORE creating any resources so that each CR is
			// removed even when an assertion below fails.  CRs are deleted in
			// reverse dependency order (innermost dependents first).
			DeferCleanup(func(dctx SpecContext) {
				if suite == nil {
					return
				}
				for _, obj := range stack.ReverseObjects() {
					if err := framework.EnsureGone(dctx, suite.Client, obj, 90*time.Second); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"warning: cleanup %T %q: %v\n", obj, obj.GetName(), err)
					}
				}
				suite.TeardownSuite()
			})
		})

		// ── Step 1: PillarTarget ─────────────────────────────────────────────────
		//
		// A PillarTarget with spec.external points the controller at the
		// out-of-cluster agent running in Docker.  The controller will dial the
		// agent and set AgentConnected=True once the gRPC health check passes.
		It("Step 1: applies PillarTarget to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarTarget %q (external agent at %s)",
				stack.Target.Name, testEnv.ExternalAgentAddr))
			Expect(framework.Apply(ctx, suite.Client, stack.Target)).To(Succeed(),
				"PillarTarget with spec.external must be accepted by the API server — "+
					"verify the CRD schema allows the address %q and port %d",
				stack.Target.Spec.External.Address,
				stack.Target.Spec.External.Port,
			)

			By("verifying PillarTarget is retrievable from the cluster")
			got := &v1alpha1.PillarTarget{}
			Expect(suite.Client.Get(ctx, framework.ObjectKey(stack.Target), got)).To(Succeed(),
				"PillarTarget %q must be readable back from the API server immediately after Apply",
				stack.Target.Name)
			Expect(got.Spec.External).NotTo(BeNil(),
				"spec.external must be present in the server-returned PillarTarget")
			Expect(got.Spec.External.Address).To(Equal(stack.Target.Spec.External.Address),
				"address must round-trip through the API server unchanged")
			Expect(got.Spec.External.Port).To(Equal(stack.Target.Spec.External.Port),
				"port must round-trip through the API server unchanged")
		})

		// ── Step 2: PillarPool ───────────────────────────────────────────────────
		//
		// A PillarPool backed by ZFS zvols on the loopback test pool.  The pool's
		// spec.targetRef must match the PillarTarget created in Step 1.
		It("Step 2: applies PillarPool to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarPool %q (ZFS pool %q, targetRef %q)",
				stack.Pool.Name, testEnv.ZFSPoolName, stack.Target.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Pool)).To(Succeed(),
				"PillarPool with ZFS zvol backend and Kind properties must be accepted — "+
					"verify the CRD schema allows the volblocksize, compression, and sync properties")

			By("verifying PillarPool is retrievable and field values are preserved")
			got := &v1alpha1.PillarPool{}
			Expect(suite.Client.Get(ctx, framework.ObjectKey(stack.Pool), got)).To(Succeed(),
				"PillarPool %q must be readable back from the API server immediately after Apply",
				stack.Pool.Name)
			Expect(got.Spec.TargetRef).To(Equal(stack.Target.Name),
				"spec.targetRef must reference the PillarTarget created in Step 1")
			Expect(got.Spec.Backend.Type).To(Equal(v1alpha1.BackendTypeZFSZvol),
				"backend.type must be zfs-zvol")
			Expect(got.Spec.Backend.ZFS).NotTo(BeNil(),
				"backend.zfs must be populated for a ZFS zvol pool")
			Expect(got.Spec.Backend.ZFS.Pool).To(Equal(testEnv.ZFSPoolName),
				"backend.zfs.pool must match the ZFS pool name passed to TestMain")
			Expect(got.Spec.Backend.ZFS.Properties["volblocksize"]).To(Equal("4096"),
				"volblocksize=4096 must be preserved through the API server round-trip")
			Expect(got.Spec.Backend.ZFS.Properties["compression"]).To(Equal("lz4"),
				"compression=lz4 must be preserved through the API server round-trip")
			Expect(got.Spec.Backend.ZFS.Properties["sync"]).To(Equal("disabled"),
				"sync=disabled must be preserved through the API server round-trip")
		})

		// ── Step 3: PillarProtocol ───────────────────────────────────────────────
		//
		// A PillarProtocol for NVMe-oF/TCP on port 4421 (Kind-safe port).
		// The protocol does not depend on PillarTarget or PillarPool — it can
		// be created in any order relative to those resources.
		It("Step 3: applies PillarProtocol to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarProtocol %q (NVMe-oF TCP port %d)",
				stack.Proto.Name, framework.KindNVMeOFPort))
			Expect(framework.Apply(ctx, suite.Client, stack.Proto)).To(Succeed(),
				"PillarProtocol with NVMe-oF TCP and port 4421 must be accepted — "+
					"verify the CRD schema allows port numbers and boolean ACL fields")

			By("verifying PillarProtocol is retrievable and field values are preserved")
			got := &v1alpha1.PillarProtocol{}
			Expect(suite.Client.Get(ctx, framework.ObjectKey(stack.Proto), got)).To(Succeed(),
				"PillarProtocol %q must be readable back from the API server immediately after Apply",
				stack.Proto.Name)
			Expect(got.Spec.Type).To(Equal(v1alpha1.ProtocolTypeNVMeOFTCP),
				"spec.type must be nvmeof-tcp")
			Expect(got.Spec.NVMeOFTCP).NotTo(BeNil(),
				"spec.nvmeofTcp must be populated for an NVMe-oF TCP protocol")
			Expect(got.Spec.NVMeOFTCP.Port).To(Equal(framework.KindNVMeOFPort),
				"port must be 4421 (Kind-safe port, avoids conflict with standard 4420)")
			Expect(got.Spec.NVMeOFTCP.ACL).To(BeFalse(),
				"ACL must be disabled — allow_any_host simplifies e2e testing")
			Expect(got.Spec.FSType).To(Equal("ext4"),
				"fsType must be ext4 for broad filesystem compatibility")
		})

		// ── Step 4: PillarBinding ────────────────────────────────────────────────
		//
		// A PillarBinding wires the ZFS zvol pool (Step 2) to the NVMe-oF TCP
		// protocol (Step 3).  When the controller reconciles this object it will:
		//   a) Check that PillarPool is Ready (requires PillarTarget to be Ready).
		//   b) Check that PillarProtocol is Ready.
		//   c) Verify backend/protocol compatibility.
		//   d) Create the Kubernetes StorageClass.
		//   e) Set StorageClassCreated=True and Ready=True on the binding's status.
		It("Step 4: applies PillarBinding to the cluster without error", func(ctx SpecContext) {
			By(fmt.Sprintf("applying PillarBinding %q (pool=%q, protocol=%q)",
				stack.Binding.Name, stack.Pool.Name, stack.Proto.Name))
			Expect(framework.Apply(ctx, suite.Client, stack.Binding)).To(Succeed(),
				"PillarBinding wiring pool and protocol must be accepted — "+
					"verify the CRD schema allows poolRef and protocolRef string fields")

			By("verifying PillarBinding is retrievable and field values are preserved")
			got := &v1alpha1.PillarBinding{}
			Expect(suite.Client.Get(ctx, framework.ObjectKey(stack.Binding), got)).To(Succeed(),
				"PillarBinding %q must be readable back from the API server immediately after Apply",
				stack.Binding.Name)
			Expect(got.Spec.PoolRef).To(Equal(stack.Pool.Name),
				"spec.poolRef must reference the PillarPool created in Step 2")
			Expect(got.Spec.ProtocolRef).To(Equal(stack.Proto.Name),
				"spec.protocolRef must reference the PillarProtocol created in Step 3")
		})

		// ── Step 5: StorageClass auto-creation ───────────────────────────────────
		//
		// This is the central assertion of the CROrchestration test.
		//
		// The PillarBinding controller reconciler creates a Kubernetes StorageClass
		// once both PillarPool and PillarProtocol are in Ready state.  We wait for
		// the binding's StorageClassCreated condition to become True — which is the
		// controller's authoritative signal that the StorageClass exists — and then
		// fetch and inspect the StorageClass directly.
		//
		// # Timeout
		//
		// The wait uses a 5-minute timeout to accommodate:
		//   - PillarTarget controller connecting to the agent (gRPC dial + health check)
		//   - PillarPool controller polling the agent for pool discovery
		//   - PillarProtocol controller computing bindingCount and activeTargets
		//   - PillarBinding controller reconciling (periodic requeue every 15 s)
		//
		// On a CI host with a responsive agent the whole chain typically completes
		// within 30–60 seconds.
		It("Step 5: StorageClass is auto-created by the PillarBinding controller with correct parameters",
			func(ctx SpecContext) {
				// The expected StorageClass name equals the binding's name because
				// no spec.storageClass.name override was set in KindNVMeOFBinding.
				expectedSCName := stack.Binding.Name

				// ── Wait for StorageClassCreated=True on the PillarBinding ────────────
				//
				// WaitForCondition re-fetches the binding on each poll cycle and
				// updates it in-place, so after the call returns successfully the
				// binding object carries the latest server state.
				By(fmt.Sprintf(
					"waiting for PillarBinding %q StorageClassCreated=True (up to 90 s)",
					stack.Binding.Name))

				// Use a fresh binding object with only the name set; WaitForCondition
				// will fill in the rest via repeated Get calls.
				waitBinding := &v1alpha1.PillarBinding{}
				waitBinding.Name = stack.Binding.Name

				Expect(framework.WaitForCondition(
					ctx, suite.Client, waitBinding,
					"StorageClassCreated", metav1.ConditionTrue, 90*time.Second,
				)).To(Succeed(),
					"PillarBinding %q must reach StorageClassCreated=True within 90 s — "+
						"check that the controller pod is running in namespace %q, that "+
						"PillarTarget %q reached AgentConnected=True, and that PillarPool "+
						"%q reached Ready=True",
					stack.Binding.Name, testEnv.HelmNamespace,
					stack.Target.Name, stack.Pool.Name,
				)

				// After WaitForCondition succeeds, waitBinding holds the latest status.
				By(fmt.Sprintf("verifying PillarBinding.Status.StorageClassName == %q",
					expectedSCName))
				Expect(waitBinding.Status.StorageClassName).To(Equal(expectedSCName),
					"PillarBinding.Status.StorageClassName must equal the binding's own name "+
						"when no spec.storageClass.name override is set")

				// ── Fetch the StorageClass ────────────────────────────────────────────
				By(fmt.Sprintf("fetching StorageClass %q from the API server", expectedSCName))
				sc := &storagev1.StorageClass{}
				Expect(suite.Client.Get(ctx,
					client.ObjectKey{Name: expectedSCName}, sc)).To(Succeed(),
					"StorageClass %q must exist after PillarBinding reaches StorageClassCreated=True",
					expectedSCName)

				// ── Assert StorageClass top-level fields ──────────────────────────────
				By("asserting StorageClass provisioner, reclaimPolicy, volumeBindingMode, and allowVolumeExpansion")

				Expect(sc.Provisioner).To(Equal("pillar-csi.bhyoo.com"),
					"StorageClass.provisioner must be the pillar-csi CSI driver name")

				Expect(sc.ReclaimPolicy).NotTo(BeNil(),
					"StorageClass.reclaimPolicy must be set")
				Expect(*sc.ReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete),
					"reclaimPolicy must be Delete (the default when spec.storageClass.reclaimPolicy "+
						"is not set on the PillarBinding)")

				Expect(sc.VolumeBindingMode).NotTo(BeNil(),
					"StorageClass.volumeBindingMode must be set")
				Expect(*sc.VolumeBindingMode).To(Equal(storagev1.VolumeBindingImmediate),
					"volumeBindingMode must be Immediate (the default when "+
						"spec.storageClass.volumeBindingMode is not set on the PillarBinding)")

				Expect(sc.AllowVolumeExpansion).NotTo(BeNil(),
					"StorageClass.allowVolumeExpansion must be set")
				Expect(*sc.AllowVolumeExpansion).To(BeTrue(),
					"allowVolumeExpansion must be true for ZFS zvol backend — "+
						"block backends support online expansion by default")

				// ── Assert StorageClass parameters ────────────────────────────────────
				//
				// The parameters encode all configuration that the CSI controller and
				// node plugins need during provisioning and attachment.  The key
				// convention is:  pillar-csi.bhyoo.com/<parameter-name>
				By("asserting StorageClass parameters carry all expected pillar-csi keys")

				params := sc.Parameters
				Expect(params).NotTo(BeNil(),
					"StorageClass.parameters must not be nil")

				// Pool and protocol references — used by the CSI controller to route
				// CreateVolume / ExportVolume calls to the correct agent.
				Expect(params["pillar-csi.bhyoo.com/pool"]).To(Equal(stack.Pool.Name),
					"parameter pool must equal the binding's spec.poolRef")
				Expect(params["pillar-csi.bhyoo.com/protocol"]).To(Equal(stack.Proto.Name),
					"parameter protocol must equal the binding's spec.protocolRef")

				// Backend and protocol type literals — allow the CSI plugin to dispatch
				// without needing to fetch the PillarPool / PillarProtocol at runtime.
				Expect(params["pillar-csi.bhyoo.com/backend-type"]).To(Equal("zfs-zvol"),
					"parameter backend-type must be 'zfs-zvol' for a ZFS zvol pool")
				Expect(params["pillar-csi.bhyoo.com/protocol-type"]).To(Equal("nvmeof-tcp"),
					"parameter protocol-type must be 'nvmeof-tcp' for an NVMe-oF TCP protocol")

				// Target reference — the PillarTarget that owns the agent this pool lives on.
				Expect(params["pillar-csi.bhyoo.com/target"]).To(Equal(stack.Target.Name),
					"parameter target must equal the pool's spec.targetRef (PillarTarget name)")

				// ZFS-specific parameters — pool name on the agent host.
				Expect(params["pillar-csi.bhyoo.com/zfs-pool"]).To(Equal(testEnv.ZFSPoolName),
					"parameter zfs-pool must equal the ZFS pool name on the agent host")

				// NVMe-oF TCP parameters — port and ACL toggle.
				Expect(params["pillar-csi.bhyoo.com/nvmeof-port"]).To(Equal("4421"),
					"parameter nvmeof-port must be '4421' (Kind-safe port from KindNVMeOFTCPProtocol)")
				Expect(params["pillar-csi.bhyoo.com/acl-enabled"]).To(Equal("false"),
					"parameter acl-enabled must be 'false' (ACL disabled in KindNVMeOFTCPProtocol)")

				// Filesystem type — used by the CSI node plugin during NodeStageVolume.
				Expect(params["csi.storage.k8s.io/fstype"]).To(Equal("ext4"),
					"parameter csi.storage.k8s.io/fstype must be 'ext4' (fsType set in "+
						"KindNVMeOFTCPProtocol)")

				By(fmt.Sprintf(
					"StorageClass %q verified: provisioner=%q reclaimPolicy=%s "+
						"volumeBindingMode=%s allowVolumeExpansion=%v pool=%q protocol=%q "+
						"target=%q zfsPool=%q nvmeofPort=%q",
					sc.Name,
					sc.Provisioner,
					*sc.ReclaimPolicy,
					*sc.VolumeBindingMode,
					*sc.AllowVolumeExpansion,
					params["pillar-csi.bhyoo.com/pool"],
					params["pillar-csi.bhyoo.com/protocol"],
					params["pillar-csi.bhyoo.com/target"],
					params["pillar-csi.bhyoo.com/zfs-pool"],
					params["pillar-csi.bhyoo.com/nvmeof-port"],
				))
			})
	}) // end Describe("CROrchestration")
	return true
}()
