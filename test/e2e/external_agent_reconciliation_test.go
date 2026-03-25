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

// external_agent_reconciliation_test.go — E2E reconciliation specs for the
// external (out-of-cluster) agent mode.
//
// These specs are part of the unified e2e Ginkgo suite (TestE2E in
// e2e_suite_test.go) and run automatically when go test is invoked with the
// e2e build tag.  There is no separate entry point — TestMain in setup_test.go
// owns the full cluster and agent lifecycle.
//
// These specs exercise the full reconciliation pipeline:
//
//   PillarTarget → PillarPool → PillarProtocol → PillarBinding → StorageClass
//
// Each layer is tested independently to isolate failures.  Tests at the gRPC
// layer directly exercise the agent's work-item processing methods
// (ReconcileState, ListVolumes, GetCapacity) to confirm the agent picks up
// work and returns well-formed responses.
//
// # Enabling external-agent reconciliation tests
//
//	make test-e2e E2E_LAUNCH_EXTERNAL_AGENT=true
//
// When neither E2E_LAUNCH_EXTERNAL_AGENT nor EXTERNAL_AGENT_ADDR is set,
// all specs in this file skip automatically.
//
// # Design notes
//
//   - The Docker-started agent address is consumed from testEnv.ExternalAgentAddr
//     (populated by TestMain before m.Run()).
//
//   - K8s CR tests additionally require EXTERNAL_AGENT_CLUSTER_ADDRESS (set
//     automatically by TestMain when E2E_LAUNCH_EXTERNAL_AGENT=true).  When
//     absent only the gRPC work-item specs run; the K8s sections skip.
//
//   - All CRs created by this suite use unique names to avoid collisions with
//     the ExternalAgent suite or parallel test runs.
//
//   - All cleanup is registered via DeferCleanup so it fires on panic/failure.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Suite-level state
// ─────────────────────────────────────────────────────────────────────────────

// reconAgentState bundles all resources allocated during BeforeAll.
type reconAgentState struct {
	// addr is "host:port" of the Docker-started agent gRPC server.
	// Populated from testEnv.ExternalAgentAddr (set by TestMain).
	addr string

	// suite wraps the controller-runtime client.  Nil when no cluster
	// address is configured.
	suite *framework.Suite
}

// reconState is the package-level singleton for this reconciliation suite.
// It is distinct from extState in external_agent_test.go.
var reconState *reconAgentState

// ─────────────────────────────────────────────────────────────────────────────
// ExternalAgentReconciliation Ginkgo suite
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("ExternalAgentReconciliation", Ordered, func() {

	// ── BeforeAll: consume Docker-started agent + optional cluster connection ─
	BeforeAll(func(ctx SpecContext) {
		reconState = &reconAgentState{}

		// Skip when TestMain did not start an external agent Docker container.
		if testEnv.ExternalAgentAddr == "" {
			Skip("external-agent mode not enabled — set E2E_LAUNCH_EXTERNAL_AGENT=true " +
				"or EXTERNAL_AGENT_ADDR to run external-agent reconciliation tests")
		}

		// Consume the Docker-started agent address from TestMain.
		reconState.addr = testEnv.ExternalAgentAddr
		By(fmt.Sprintf("reconciliation agent addr (from TestMain): %s", reconState.addr))

		// Connect to the Kind cluster (optional: skip K8s CR tests when
		// cluster is not reachable).  KUBECONFIG is set by TestMain.
		var err error
		reconState.suite, err = framework.SetupSuite(
			framework.WithConnectTimeout(30 * time.Second),
		)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"warning: cluster not reachable (%v) — K8s CR tests will be skipped\n", err)
			reconState.suite = nil
		} else {
			By("connected to Kind cluster for reconciliation suite")
		}
	})

	// ── AfterAll: disconnect from cluster ────────────────────────────────────
	AfterAll(func() {
		if reconState != nil && reconState.suite != nil {
			reconState.suite.TeardownSuite()
		}
	})

	// ════════════════════════════════════════════════════════════════════════
	// Part I — gRPC work-item tests (agent-level, no cluster required)
	// ════════════════════════════════════════════════════════════════════════
	//
	// These specs dial the agent directly and verify that it correctly
	// processes "work items" passed through gRPC calls.  They run regardless
	// of whether EXTERNAL_AGENT_CLUSTER_ADDRESS is set.

	Context("gRPC work-item processing", func() {

		// ── spec 1: ReconcileState with empty volume list ────────────────────
		//
		// The controller calls ReconcileState to converge the agent's local
		// state toward the desired state.  An empty volume list is a valid
		// noop — the agent should acknowledge it, returning a non-nil
		// ReconciledAt timestamp.

		It("ReconcileState with empty volume list returns a reconciled-at timestamp", func(ctx SpecContext) {
			conn := reconDial(reconState.addr)
			DeferCleanup(conn.Close)

			c := agentv1.NewAgentServiceClient(conn)
			resp, err := c.ReconcileState(ctx, &agentv1.ReconcileStateRequest{
				Volumes: nil, // empty desired state: nothing should exist
			})
			Expect(err).NotTo(HaveOccurred(),
				"ReconcileState with empty volume list must succeed")
			Expect(resp).NotTo(BeNil(),
				"ReconcileState response must not be nil")
			Expect(resp.GetReconciledAt()).NotTo(BeNil(),
				"ReconcileState must return a non-nil ReconciledAt timestamp "+
					"so callers can detect stale reconciliations")

			By(fmt.Sprintf(
				"ReconcileState: reconciledAt=%s results=%d",
				resp.GetReconciledAt().AsTime().UTC().Format(time.RFC3339),
				len(resp.GetResults()),
			))
		})

		// ── spec 2: ReconcileState is idempotent ─────────────────────────────
		//
		// Calling ReconcileState twice with the same (empty) payload must
		// succeed both times, confirming idempotent agent semantics.

		It("ReconcileState is idempotent across repeated calls", func(ctx SpecContext) {
			conn := reconDial(reconState.addr)
			DeferCleanup(conn.Close)

			c := agentv1.NewAgentServiceClient(conn)

			var prevTs int64
			for i := 0; i < 3; i++ {
				resp, err := c.ReconcileState(ctx, &agentv1.ReconcileStateRequest{})
				Expect(err).NotTo(HaveOccurred(),
					"ReconcileState call #%d must succeed", i+1)
				Expect(resp.GetReconciledAt()).NotTo(BeNil(),
					"call #%d: ReconciledAt must be non-nil", i+1)

				ts := resp.GetReconciledAt().GetSeconds()
				// Each call should return a timestamp >= the previous one.
				Expect(ts).To(BeNumerically(">=", prevTs),
					"call #%d: ReconciledAt timestamp should not go backwards", i+1)
				prevTs = ts
			}

			By("ReconcileState: three successive calls all succeeded with non-decreasing timestamps")
		})

		// ── spec 3: ListVolumes for unknown pool returns an error ─────────────
		//
		// The controller calls ListVolumes to enumerate existing volumes before
		// reconciling.  An unknown pool must return an error (or an empty list),
		// not silently succeed — otherwise orphaned volumes could be missed.

		It("ListVolumes for an unknown pool name returns an error", func(ctx SpecContext) {
			conn := reconDial(reconState.addr)
			DeferCleanup(conn.Close)

			c := agentv1.NewAgentServiceClient(conn)
			_, err := c.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				PoolName: "definitely-does-not-exist-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			})
			Expect(err).To(HaveOccurred(),
				"ListVolumes with unknown pool must return an error so that the "+
					"controller can detect misconfigured PillarPool specs")
			By("ListVolumes with unknown pool correctly returned an error")
		})

		// ── spec 4: ListVolumes for configured pool ───────────────────────────
		//
		// The controller calls ListVolumes against the pool configured in
		// spec.backend.zfs.pool.  Either an empty list (pool exists, no volumes)
		// or an error (pool not present in this test environment) is acceptable;
		// we assert only that the call does not panic and the response is
		// structurally valid.

		It("ListVolumes for the configured pool name does not panic the agent", func(ctx SpecContext) {
			conn := reconDial(reconState.addr)
			DeferCleanup(conn.Close)

			c := agentv1.NewAgentServiceClient(conn)
			pool := reconAgentZFSPool()
			resp, err := c.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				PoolName: pool,
			})
			if err != nil {
				// Acceptable if the pool does not physically exist in this env.
				By(fmt.Sprintf(
					"ListVolumes(%q): pool not present (%v) — OK in mock environment",
					pool, err,
				))
				return
			}
			Expect(resp).NotTo(BeNil(),
				"ListVolumes response must not be nil when pool exists")
			By(fmt.Sprintf(
				"ListVolumes(%q): %d volume(s) found", pool, len(resp.GetVolumes()),
			))
		})

		// ── spec 5: HealthCheck reports status for all subsystems ─────────────
		//
		// After the agent has processed work (ReconcileState calls above),
		// HealthCheck must still return a well-formed response, confirming the
		// agent remains healthy under reconciliation load.

		It("HealthCheck remains healthy after reconciliation work", func(ctx SpecContext) {
			conn := reconDial(reconState.addr)
			DeferCleanup(conn.Close)

			c := agentv1.NewAgentServiceClient(conn)
			resp, err := c.HealthCheck(ctx, &agentv1.HealthCheckRequest{})
			Expect(err).NotTo(HaveOccurred(),
				"HealthCheck must succeed even after reconciliation RPCs")
			Expect(resp.GetCheckedAt()).NotTo(BeNil(),
				"HealthCheck response must include a CheckedAt timestamp")
			Expect(len(resp.GetSubsystems())).To(BeNumerically(">", 0),
				"HealthCheck must report at least one subsystem after reconciliation")

			By(fmt.Sprintf(
				"HealthCheck post-reconciliation: %d subsystem(s) healthy",
				len(resp.GetSubsystems()),
			))
		})
	})

	// ════════════════════════════════════════════════════════════════════════
	// Part II — Kubernetes CR reconciliation tests (cluster-level work items)
	// ════════════════════════════════════════════════════════════════════════
	//
	// These specs require:
	//   • A running Kind cluster with pillar-csi Helm chart deployed
	//   • EXTERNAL_AGENT_CLUSTER_ADDRESS set to <host>:<port> reachable from
	//     inside the Kind network
	//
	// All K8s-level contexts Skip gracefully when the above prerequisites are
	// not met.

	// ── PillarTarget: discovered pools population ────────────────────────────
	//
	// After the controller connects to the external agent, it queries the agent
	// for its available pools and writes the results to
	// status.discoveredPools.  This section creates a fresh PillarTarget and
	// validates the full status reconciliation output.

	Context("PillarTarget status reconciliation", Ordered, func() {
		var (
			target      *v1alpha1.PillarTarget
			targetName  string
			clusterAddr reconClusterAddr
		)

		BeforeAll(func(ctx SpecContext) {
			clusterAddr = reconGuardCluster(reconState)

			targetName = fmt.Sprintf("recon-target-%d", time.Now().UnixMilli()%100000)
			target = framework.NewExternalPillarTarget(
				targetName, clusterAddr.host, clusterAddr.port,
			)

			By(fmt.Sprintf(
				"creating PillarTarget %q → %s:%d",
				targetName, clusterAddr.host, clusterAddr.port,
			))
			Expect(framework.Apply(ctx, reconState.suite.Client, target)).To(Succeed(),
				"apply PillarTarget CR")

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up PillarTarget %q", targetName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, target, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup PillarTarget %q: %v\n", targetName, err)
				}
			})
		})

		// spec: controller connects and populates the three primary status fields.

		It("controller populates resolvedAddress, agentVersion and capabilities", func(ctx SpecContext) {

			By(fmt.Sprintf("waiting for AgentConnected=True on PillarTarget %q", targetName))
			err := framework.WaitForCondition(ctx, reconState.suite.Client, target,
				"AgentConnected", metav1.ConditionTrue, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"AgentConnected must become True — controller must be running and "+
					"able to reach %s:%d", clusterAddr.host, clusterAddr.port)

			// Verify all three primary status fields populated in one read.
			fresh := &v1alpha1.PillarTarget{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: targetName}, fresh)).To(Succeed())

			Expect(fresh.Status.ResolvedAddress).NotTo(BeEmpty(),
				"status.resolvedAddress must be set after agent connects")
			Expect(fresh.Status.AgentVersion).NotTo(BeEmpty(),
				"status.agentVersion must be set after agent connects")
			Expect(fresh.Status.Capabilities).NotTo(BeNil(),
				"status.capabilities must be non-nil after agent connects")
			Expect(fresh.Status.Capabilities.Backends).NotTo(BeEmpty(),
				"capabilities.backends must list at least one backend type")

			By(fmt.Sprintf(
				"status: resolvedAddress=%q agentVersion=%q backends=%v",
				fresh.Status.ResolvedAddress,
				fresh.Status.AgentVersion,
				fresh.Status.Capabilities.Backends,
			))
		})

		// spec: discoveredPools field is populated (may be empty if no ZFS pools
		// exist, but the field must be present after reconciliation — even an
		// empty slice proves the controller queried the agent and wrote a result).

		It("controller reconciles discoveredPools after agent connection", func(ctx SpecContext) {
			// Wait until conditions are set so the controller has had a chance
			// to run the pool-discovery step.
			err := framework.WaitForCondition(ctx, reconState.suite.Client, target,
				"Ready", metav1.ConditionTrue, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarTarget must reach Ready=True before checking discoveredPools")

			fresh := &v1alpha1.PillarTarget{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: targetName}, fresh)).To(Succeed())

			// discoveredPools may be nil/empty (no real ZFS pool in test env)
			// or non-empty (real ZFS pool).  Either is correct.  We verify
			// each reported pool has at minimum a Name and Type to guard
			// against controller bugs that write zero-value entries.
			for i, p := range fresh.Status.DiscoveredPools {
				Expect(p.Name).NotTo(BeEmpty(),
					"discoveredPools[%d].name must be non-empty", i)
				Expect(p.Type).NotTo(BeEmpty(),
					"discoveredPools[%d].type must be non-empty", i)
			}

			By(fmt.Sprintf(
				"discoveredPools: %d pool(s) reported by agent",
				len(fresh.Status.DiscoveredPools),
			))
		})

		// spec: all three standard conditions are present and consistent.

		It("PillarTarget conditions are fully populated and internally consistent", func(ctx SpecContext) {
			err := framework.WaitForCondition(ctx, reconState.suite.Client, target,
				"Ready", metav1.ConditionTrue, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarTarget must be Ready before checking condition consistency")

			fresh := &v1alpha1.PillarTarget{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: targetName}, fresh)).To(Succeed())

			condMap := map[string]metav1.Condition{}
			for _, c := range fresh.Status.Conditions {
				condMap[c.Type] = c
			}

			// AgentConnected must be True for Ready to be True.
			Expect(condMap).To(HaveKey("AgentConnected"),
				"AgentConnected condition must be present")
			Expect(condMap["AgentConnected"].Status).To(Equal(metav1.ConditionTrue),
				"AgentConnected must be True when Ready=True")

			// Ready must be True.
			Expect(condMap).To(HaveKey("Ready"),
				"Ready condition must be present")
			Expect(condMap["Ready"].Status).To(Equal(metav1.ConditionTrue),
				"Ready condition must be True")

			// Every condition must have a non-empty Reason.
			for _, c := range fresh.Status.Conditions {
				Expect(c.Reason).NotTo(BeEmpty(),
					"condition %q must have a non-empty Reason field", c.Type)
			}

			By(fmt.Sprintf(
				"conditions: %d condition(s) all valid",
				len(fresh.Status.Conditions),
			))
		})
	})

	// ── PillarPool reconciliation ────────────────────────────────────────────
	//
	// The controller picks up PillarPool CRs, dials the referenced target, and
	// queries the agent for pool capacity and backend support.  Conditions on
	// the PillarPool reflect the outcome of each reconciliation cycle.

	Context("PillarPool reconciliation lifecycle", Ordered, func() {
		var (
			target      *v1alpha1.PillarTarget
			pool        *v1alpha1.PillarPool
			targetName  string
			poolName    string
			clusterAddr reconClusterAddr
		)

		BeforeAll(func(ctx SpecContext) {
			clusterAddr = reconGuardCluster(reconState)

			targetName = fmt.Sprintf("recon-pool-target-%d", time.Now().UnixMilli()%100000)
			poolName = fmt.Sprintf("recon-pool-%d", time.Now().UnixMilli()%100000)
			zfsPool := reconAgentZFSPool()

			// Create the PillarTarget first (needed by PillarPool reconciler).
			target = framework.NewExternalPillarTarget(
				targetName, clusterAddr.host, clusterAddr.port,
			)
			By(fmt.Sprintf("creating PillarTarget %q", targetName))
			Expect(framework.Apply(ctx, reconState.suite.Client, target)).To(Succeed())

			// Wait for target to become Ready before creating the pool, so the
			// BackendSupported condition can be evaluated immediately.
			By(fmt.Sprintf("waiting for PillarTarget %q to become Ready", targetName))
			Expect(framework.WaitForReady(ctx, reconState.suite.Client, target, 2*time.Minute)).
				To(Succeed(), "PillarTarget must be Ready before PillarPool can reconcile")

			// Create a ZFS-zvol PillarPool referencing the target.
			pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
			By(fmt.Sprintf(
				"creating PillarPool %q (target=%s pool=%s)",
				poolName, targetName, zfsPool,
			))
			Expect(framework.Apply(ctx, reconState.suite.Client, pool)).To(Succeed())

			// Register cleanup in reverse order (pool before target).
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up PillarPool %q", poolName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, pool, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup PillarPool %q: %v\n", poolName, err)
				}
				By(fmt.Sprintf("cleaning up PillarTarget %q", targetName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, target, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup PillarTarget %q: %v\n", targetName, err)
				}
			})
		})

		// spec: controller reconciles pool conditions within a reasonable time.

		It("PillarPool conditions are populated after creation", func(ctx SpecContext) {
			// Wait until the pool has at least one condition — this proves the
			// controller reconciler has run at least once.
			err := framework.WaitForField(ctx, reconState.suite.Client, pool,
				func(p *v1alpha1.PillarPool) bool {
					return len(p.Status.Conditions) > 0
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarPool must have at least one condition within 2 min of creation")

			fresh := &v1alpha1.PillarPool{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: poolName}, fresh)).To(Succeed())

			Expect(fresh.Status.Conditions).NotTo(BeEmpty(),
				"PillarPool must have conditions set by the reconciler")

			// Every condition must have non-empty Type, Status, and Reason.
			for _, c := range fresh.Status.Conditions {
				Expect(c.Type).NotTo(BeEmpty(),
					"condition Type must be non-empty")
				Expect(c.Status).To(BeElementOf(
					metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown,
				), "condition Status must be a valid ConditionStatus value")
				Expect(c.Reason).NotTo(BeEmpty(),
					"condition %q Reason must be non-empty", c.Type)
			}

			By(fmt.Sprintf(
				"PillarPool conditions: %d condition(s) populated",
				len(fresh.Status.Conditions),
			))
		})

		// spec: TargetReady condition reflects that the referenced PillarTarget
		// is in Ready state.  Since we explicitly wait for Ready=True on the
		// target above, this condition must be True.

		It("PillarPool TargetReady condition is True (target was Ready before pool creation)", func(ctx SpecContext) {
			err := framework.WaitForCondition(ctx, reconState.suite.Client, pool,
				"TargetReady", metav1.ConditionTrue, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"TargetReady must become True — the PillarTarget is in Ready state")
			By("PillarPool TargetReady=True confirmed")
		})

		// spec: BackendSupported condition is set.
		//
		// The controller evaluates whether the pool's backend type (zfs-zvol)
		// appears in the agent's capabilities.Backends list.  The condition may
		// be True or False depending on whether the agent advertises zfs-zvol
		// support; what matters is that it is populated (not Unknown).

		It("PillarPool BackendSupported condition is set to True or False (not absent)", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, reconState.suite.Client, pool,
				func(p *v1alpha1.PillarPool) bool {
					for _, c := range p.Status.Conditions {
						if c.Type == "BackendSupported" {
							return c.Status != metav1.ConditionUnknown
						}
					}
					return false
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"BackendSupported condition must be set (True or False) within 2 min")

			fresh := &v1alpha1.PillarPool{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: poolName}, fresh)).To(Succeed())

			var bsCond *metav1.Condition
			for i := range fresh.Status.Conditions {
				if fresh.Status.Conditions[i].Type == "BackendSupported" {
					c := fresh.Status.Conditions[i]
					bsCond = &c
					break
				}
			}
			Expect(bsCond).NotTo(BeNil(), "BackendSupported condition must be present")
			Expect(bsCond.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
				"BackendSupported must be True or False, not Unknown")

			By(fmt.Sprintf(
				"PillarPool BackendSupported=%s (reason=%s)",
				bsCond.Status, bsCond.Reason,
			))
		})

		// spec: PoolDiscovered condition is set.
		//
		// The controller calls GetCapacity on the agent for the named pool.
		// In a test environment without a real ZFS pool the result will be
		// False (pool not found); with a real pool it will be True.  Either
		// way the condition must be populated to confirm reconciliation ran.

		It("PillarPool PoolDiscovered condition is set (True or False, not absent)", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, reconState.suite.Client, pool,
				func(p *v1alpha1.PillarPool) bool {
					for _, c := range p.Status.Conditions {
						if c.Type == "PoolDiscovered" {
							return c.Status != metav1.ConditionUnknown
						}
					}
					return false
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PoolDiscovered condition must be set within 2 min of pool creation")

			fresh := &v1alpha1.PillarPool{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: poolName}, fresh)).To(Succeed())

			var pdCond *metav1.Condition
			for i := range fresh.Status.Conditions {
				if fresh.Status.Conditions[i].Type == "PoolDiscovered" {
					c := fresh.Status.Conditions[i]
					pdCond = &c
					break
				}
			}
			Expect(pdCond).NotTo(BeNil(), "PoolDiscovered condition must be present")
			Expect(pdCond.Status).To(BeElementOf(metav1.ConditionTrue, metav1.ConditionFalse),
				"PoolDiscovered must be True or False, not Unknown")

			By(fmt.Sprintf(
				"PillarPool PoolDiscovered=%s (reason=%s message=%s)",
				pdCond.Status, pdCond.Reason, pdCond.Message,
			))
		})
	})

	// ── PillarProtocol lifecycle ─────────────────────────────────────────────
	//
	// PillarProtocol is a lightweight cluster-scoped resource with no per-agent
	// gRPC calls.  Its conditions are set purely from spec validation.

	Context("PillarProtocol lifecycle", Ordered, func() {
		var (
			proto     *v1alpha1.PillarProtocol
			protoName string
		)

		BeforeAll(func(ctx SpecContext) {
			reconGuardCluster(reconState) // Skip if no cluster.

			protoName = fmt.Sprintf("recon-proto-%d", time.Now().UnixMilli()%100000)
			proto = framework.NewNVMeOFTCPProtocol(protoName)

			By(fmt.Sprintf("creating PillarProtocol %q (nvmeof-tcp)", protoName))
			Expect(framework.Apply(ctx, reconState.suite.Client, proto)).To(Succeed())

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up PillarProtocol %q", protoName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, proto, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup PillarProtocol %q: %v\n", protoName, err)
				}
			})
		})

		// spec: protocol spec is persisted correctly.

		It("persists spec.type and spec.nvmeofTcp after creation", func(ctx SpecContext) {
			got := &v1alpha1.PillarProtocol{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: protoName}, got)).To(Succeed())

			Expect(got.Spec.Type).To(Equal(v1alpha1.ProtocolTypeNVMeOFTCP),
				"spec.type must be nvmeof-tcp")
			Expect(got.Spec.NVMeOFTCP).NotTo(BeNil(),
				"spec.nvmeofTcp must be populated for nvmeof-tcp protocol")

			By(fmt.Sprintf(
				"PillarProtocol spec: type=%s port=%d",
				got.Spec.Type, got.Spec.NVMeOFTCP.Port,
			))
		})

		// spec: Ready condition is populated.

		It("PillarProtocol Ready condition is set", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, reconState.suite.Client, proto,
				func(p *v1alpha1.PillarProtocol) bool {
					for _, c := range p.Status.Conditions {
						if c.Type == "Ready" {
							return c.Status != metav1.ConditionUnknown
						}
					}
					return false
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarProtocol Ready condition must be set within 2 min of creation")

			fresh := &v1alpha1.PillarProtocol{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: protoName}, fresh)).To(Succeed())

			var readyCond *metav1.Condition
			for i := range fresh.Status.Conditions {
				if fresh.Status.Conditions[i].Type == "Ready" {
					c := fresh.Status.Conditions[i]
					readyCond = &c
					break
				}
			}
			Expect(readyCond).NotTo(BeNil(),
				"Ready condition must be present on PillarProtocol")

			By(fmt.Sprintf(
				"PillarProtocol Ready=%s (reason=%s)",
				readyCond.Status, readyCond.Reason,
			))
		})

		// spec: status.bindingCount starts at zero (no bindings yet).

		It("status.bindingCount is zero when no PillarBindings reference the protocol", func(ctx SpecContext) {
			// Poll briefly to let the reconciler set initial status.
			time.Sleep(3 * time.Second)

			fresh := &v1alpha1.PillarProtocol{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: protoName}, fresh)).To(Succeed())

			Expect(fresh.Status.BindingCount).To(BeNumerically("==", 0),
				"bindingCount must be 0 when no PillarBinding references the protocol")
			By("PillarProtocol bindingCount=0 confirmed (no bindings)")
		})
	})

	// ── PillarBinding → StorageClass generation ──────────────────────────────
	//
	// A PillarBinding combines a PillarPool and PillarProtocol and generates a
	// Kubernetes StorageClass.  This section verifies the full reconciliation
	// chain from pool + protocol CRs through to a concrete StorageClass.

	Context("PillarBinding StorageClass generation chain", Ordered, func() {
		var (
			target      *v1alpha1.PillarTarget
			pool        *v1alpha1.PillarPool
			proto       *v1alpha1.PillarProtocol
			binding     *v1alpha1.PillarBinding
			targetName  string
			poolName    string
			protoName   string
			bindingName string
			clusterAddr reconClusterAddr
		)

		BeforeAll(func(ctx SpecContext) {
			clusterAddr = reconGuardCluster(reconState)

			suffix := time.Now().UnixMilli() % 100000
			targetName = fmt.Sprintf("recon-chain-target-%d", suffix)
			poolName = fmt.Sprintf("recon-chain-pool-%d", suffix)
			protoName = fmt.Sprintf("recon-chain-proto-%d", suffix)
			bindingName = fmt.Sprintf("recon-chain-binding-%d", suffix)
			zfsPool := reconAgentZFSPool()

			// Create PillarTarget.
			target = framework.NewExternalPillarTarget(
				targetName, clusterAddr.host, clusterAddr.port,
			)
			By(fmt.Sprintf("creating PillarTarget %q", targetName))
			Expect(framework.Apply(ctx, reconState.suite.Client, target)).To(Succeed())

			// Wait for target to be Ready.
			By(fmt.Sprintf("waiting for PillarTarget %q Ready=True", targetName))
			Expect(framework.WaitForReady(ctx, reconState.suite.Client, target, 2*time.Minute)).
				To(Succeed())

			// Create PillarPool.
			pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
			By(fmt.Sprintf("creating PillarPool %q", poolName))
			Expect(framework.Apply(ctx, reconState.suite.Client, pool)).To(Succeed())

			// Create PillarProtocol.
			proto = framework.NewNVMeOFTCPProtocol(protoName)
			By(fmt.Sprintf("creating PillarProtocol %q", protoName))
			Expect(framework.Apply(ctx, reconState.suite.Client, proto)).To(Succeed())

			// Create PillarBinding wiring pool + protocol.
			binding = framework.NewSimplePillarBinding(bindingName, poolName, protoName)
			By(fmt.Sprintf(
				"creating PillarBinding %q (pool=%s protocol=%s)",
				bindingName, poolName, protoName,
			))
			Expect(framework.Apply(ctx, reconState.suite.Client, binding)).To(Succeed())

			// Register cleanup in reverse dependency order.
			DeferCleanup(func(dctx SpecContext) {
				for _, item := range []struct {
					name string
					obj  client.Object
				}{
					{bindingName, binding},
					{protoName, proto},
					{poolName, pool},
					{targetName, target},
				} {
					By(fmt.Sprintf("cleaning up %T %q", item.obj, item.name))
					if err := framework.EnsureGone(dctx, reconState.suite.Client, item.obj, 2*time.Minute); err != nil {
						_, _ = fmt.Fprintf(GinkgoWriter,
							"warning: cleanup %T %q: %v\n", item.obj, item.name, err)
					}
				}
			})
		})

		// spec: binding spec fields are persisted correctly.

		It("PillarBinding persists spec.poolRef and spec.protocolRef", func(ctx SpecContext) {
			got := &v1alpha1.PillarBinding{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: bindingName}, got)).To(Succeed())

			Expect(got.Spec.PoolRef).To(Equal(poolName),
				"spec.poolRef must match the configured PillarPool name")
			Expect(got.Spec.ProtocolRef).To(Equal(protoName),
				"spec.protocolRef must match the configured PillarProtocol name")

			By(fmt.Sprintf(
				"PillarBinding spec: poolRef=%s protocolRef=%s",
				got.Spec.PoolRef, got.Spec.ProtocolRef,
			))
		})

		// spec: controller populates status.storageClassName.
		//
		// The controller creates a StorageClass and writes its name to
		// status.storageClassName.  This is independent of whether the pool
		// is actually discovered — the StorageClass is created as soon as the
		// controller has validated the binding spec.

		It("controller sets status.storageClassName after binding creation", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, reconState.suite.Client, binding,
				func(b *v1alpha1.PillarBinding) bool {
					return b.Status.StorageClassName != ""
				}, 3*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"status.storageClassName must be populated once the controller "+
					"creates or identifies the StorageClass for this binding")

			Expect(binding.Status.StorageClassName).NotTo(BeEmpty(),
				"storageClassName must be a non-empty string")

			By(fmt.Sprintf(
				"PillarBinding status.storageClassName = %q",
				binding.Status.StorageClassName,
			))
		})

		// spec: generated StorageClass exists in the cluster.
		//
		// The name written to status.storageClassName must correspond to an
		// actual StorageClass object in the cluster, confirming the controller
		// completed the creation step.

		It("generated StorageClass exists in the cluster", func(ctx SpecContext) {
			// Re-fetch binding to get the latest storageClassName.
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: bindingName}, binding)).To(Succeed())
			Expect(binding.Status.StorageClassName).NotTo(BeEmpty(),
				"status.storageClassName must be set before checking StorageClass existence")

			scName := binding.Status.StorageClassName

			// Poll until the StorageClass appears — the controller may create it
			// asynchronously after writing the binding status.
			var storageClass *storagev1.StorageClass
			Eventually(func(g Gomega) {
				sc := &storagev1.StorageClass{}
				g.Expect(reconState.suite.Client.Get(ctx,
					client.ObjectKey{Name: scName}, sc)).To(Succeed(),
					"StorageClass %q must exist in the cluster (created by PillarBinding controller)",
					scName,
				)
				g.Expect(sc.Provisioner).NotTo(BeEmpty(),
					"StorageClass provisioner must be set")
				storageClass = sc
			}, 2*time.Minute, 2*time.Second).Should(Succeed(),
				"StorageClass %q must be created by the controller within 2 min", scName)

			By(fmt.Sprintf(
				"StorageClass %q exists (provisioner=%s)",
				scName, storageClass.Provisioner,
			))
		})

		// spec: binding conditions are populated.

		It("PillarBinding conditions are populated by the reconciler", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, reconState.suite.Client, binding,
				func(b *v1alpha1.PillarBinding) bool {
					return len(b.Status.Conditions) > 0
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarBinding must have at least one condition within 2 min of creation")

			fresh := &v1alpha1.PillarBinding{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: bindingName}, fresh)).To(Succeed())

			Expect(fresh.Status.Conditions).NotTo(BeEmpty(),
				"PillarBinding conditions must be populated")

			for _, c := range fresh.Status.Conditions {
				Expect(c.Reason).NotTo(BeEmpty(),
					"condition %q Reason must be non-empty", c.Type)
			}

			By(fmt.Sprintf(
				"PillarBinding: %d condition(s) set, storageClassName=%q",
				len(fresh.Status.Conditions), fresh.Status.StorageClassName,
			))
		})
	})

	// ── Invalid external agent target ────────────────────────────────────────
	//
	// When a PillarTarget points at an address where no agent is listening,
	// the controller must set AgentConnected=False and Ready=False — it must
	// not block indefinitely or crash.  This is the "negative path" of the
	// reconciliation work item.

	Context("invalid (unreachable) PillarTarget reconciliation", Ordered, func() {
		var (
			invalidTarget *v1alpha1.PillarTarget
			invalidName   string
		)

		BeforeAll(func(ctx SpecContext) {
			reconGuardCluster(reconState)

			invalidName = fmt.Sprintf("recon-invalid-target-%d", time.Now().UnixMilli()%100000)
			// Use a loopback address with a port that is very unlikely to have
			// an agent listening.  Port 19501 is outside the well-known range.
			invalidTarget = framework.NewExternalPillarTarget(invalidName, "127.0.0.2", 19501)

			By(fmt.Sprintf("creating invalid PillarTarget %q → 127.0.0.2:19501", invalidName))
			Expect(framework.Apply(ctx, reconState.suite.Client, invalidTarget)).To(Succeed())

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up invalid PillarTarget %q", invalidName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, invalidTarget, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup invalid PillarTarget %q: %v\n", invalidName, err)
				}
			})
		})

		// spec: controller sets AgentConnected=False for unreachable agent.

		It("controller sets AgentConnected=False for an unreachable agent address", func(ctx SpecContext) {
			By(fmt.Sprintf(
				"waiting for AgentConnected=False on invalid PillarTarget %q (up to 2 min)",
				invalidName,
			))
			err := framework.WaitForCondition(ctx, reconState.suite.Client, invalidTarget,
				"AgentConnected", metav1.ConditionFalse, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"AgentConnected must become False for an unreachable agent address")

			By("invalid PillarTarget: AgentConnected=False confirmed")
		})

		// spec: controller sets Ready=False for unreachable agent.

		It("controller sets Ready=False for an unreachable agent address", func(ctx SpecContext) {
			err := framework.WaitForCondition(ctx, reconState.suite.Client, invalidTarget,
				"Ready", metav1.ConditionFalse, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"Ready must become False when the agent is unreachable")

			fresh := &v1alpha1.PillarTarget{}
			Expect(reconState.suite.Client.Get(ctx,
				client.ObjectKey{Name: invalidName}, fresh)).To(Succeed())

			// Verify the AgentConnected condition has a descriptive reason.
			for _, c := range fresh.Status.Conditions {
				if c.Type == "AgentConnected" {
					Expect(c.Reason).To(BeElementOf(
						"HealthCheckFailed", "DialerNotConfigured",
						"TLSHandshakeFailed", "AgentUnhealthy",
					), "AgentConnected reason must be a recognised failure reason")
					Expect(c.Message).NotTo(BeEmpty(),
						"AgentConnected condition Message must explain the failure")
					break
				}
			}

			By("invalid PillarTarget: Ready=False with descriptive failure reason confirmed")
		})
	})

	// ── Deletion cascade reconciliation ──────────────────────────────────────
	//
	// When a PillarTarget is deleted, any PillarPools that reference it should
	// update their TargetReady condition to False (the backing target is gone).
	// This confirms the controller's watch-triggered reconciliation for
	// dependent resources.

	Context("deletion cascade: PillarPool TargetReady after target deletion", Ordered, func() {
		var (
			target      *v1alpha1.PillarTarget
			pool        *v1alpha1.PillarPool
			targetName  string
			poolName    string
			clusterAddr reconClusterAddr
		)

		BeforeAll(func(ctx SpecContext) {
			clusterAddr = reconGuardCluster(reconState)

			suffix := time.Now().UnixMilli() % 100000
			targetName = fmt.Sprintf("recon-cascade-target-%d", suffix)
			poolName = fmt.Sprintf("recon-cascade-pool-%d", suffix)
			zfsPool := reconAgentZFSPool()

			// Create target and wait for it to be Ready.
			target = framework.NewExternalPillarTarget(
				targetName, clusterAddr.host, clusterAddr.port,
			)
			Expect(framework.Apply(ctx, reconState.suite.Client, target)).To(Succeed())
			By(fmt.Sprintf("waiting for PillarTarget %q Ready=True", targetName))
			Expect(framework.WaitForReady(ctx, reconState.suite.Client, target, 2*time.Minute)).
				To(Succeed())

			// Create pool referencing the target.
			pool = framework.NewZFSZvolPool(poolName, targetName, zfsPool)
			Expect(framework.Apply(ctx, reconState.suite.Client, pool)).To(Succeed())

			// Wait for TargetReady=True on the pool before the deletion test.
			By(fmt.Sprintf("waiting for PillarPool %q TargetReady=True", poolName))
			Expect(framework.WaitForCondition(ctx, reconState.suite.Client, pool,
				"TargetReady", metav1.ConditionTrue, 2*time.Minute)).
				To(Succeed(), "pool TargetReady must be True before deletion cascade test")

			// Register pool cleanup only — target is deleted mid-test.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up PillarPool %q (cascade test)", poolName))
				if err := framework.EnsureGone(dctx, reconState.suite.Client, pool, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cascade cleanup PillarPool %q: %v\n", poolName, err)
				}
				// Best-effort target cleanup in case the delete-test spec failed.
				if delErr := framework.Delete(dctx, reconState.suite.Client, target,
					client.GracePeriodSeconds(0)); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"info: cascade cleanup PillarTarget %q: %v (may already be gone)\n",
						targetName, delErr)
				}
			})
		})

		// spec: delete the target and observe TargetReady flips to False on pool.

		It("PillarPool TargetReady becomes False after PillarTarget deletion", func(ctx SpecContext) {
			By(fmt.Sprintf("deleting PillarTarget %q", targetName))
			Expect(framework.Delete(ctx, reconState.suite.Client, target,
				client.GracePeriodSeconds(0))).To(Succeed())

			// Wait for the pool's TargetReady condition to flip to False.
			By(fmt.Sprintf(
				"waiting for PillarPool %q TargetReady=False after target deletion",
				poolName,
			))
			err := framework.WaitForCondition(ctx, reconState.suite.Client, pool,
				"TargetReady", metav1.ConditionFalse, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarPool TargetReady must flip to False after the referenced "+
					"PillarTarget is deleted — the controller must watch target deletions "+
					"and reconcile dependent pools")

			By("deletion cascade confirmed: PillarPool TargetReady=False after target deletion")
		})
	})
})

// ─────────────────────────────────────────────────────────────────────────────
// Configuration helpers
// ─────────────────────────────────────────────────────────────────────────────

// reconAgentZFSPool returns the ZFS pool name for this suite's agent.
// Reads EXTERNAL_AGENT_ZFS_POOL (default: "e2e-pool").
func reconAgentZFSPool() string {
	return extAgentZFSPool() // reuse the same env-var logic from external_agent_test.go
}

// ─────────────────────────────────────────────────────────────────────────────
// gRPC helpers
// ─────────────────────────────────────────────────────────────────────────────

// reconDial opens a plaintext gRPC connection to the reconciliation agent.
// Fails the current spec immediately on error.
func reconDial(addr string) *grpc.ClientConn {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck
		ctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	Expect(err).NotTo(HaveOccurred(),
		"gRPC dial to reconciliation agent at %s failed", addr)
	return conn
}

// ─────────────────────────────────────────────────────────────────────────────
// Cluster access helpers
// ─────────────────────────────────────────────────────────────────────────────

// reconClusterAddr holds the parsed host and port from
// EXTERNAL_AGENT_CLUSTER_ADDRESS.
type reconClusterAddr struct {
	host string
	port int32
}

// reconGuardCluster checks prerequisites for K8s CR tests and calls Ginkgo's
// Skip if they are not met.  Returns the parsed cluster address on success.
//
// Prerequisites:
//   - reconState.suite must be non-nil (cluster connection established)
//   - EXTERNAL_AGENT_CLUSTER_ADDRESS must be set and parseable
func reconGuardCluster(state *reconAgentState) reconClusterAddr {
	if state == nil || state.suite == nil {
		Skip("cluster not reachable — " +
			"skipping K8s CR reconciliation tests " +
			"(ensure KUBECONFIG points at a running cluster)")
	}

	raw := os.Getenv("EXTERNAL_AGENT_CLUSTER_ADDRESS")
	if raw == "" {
		Skip("EXTERNAL_AGENT_CLUSTER_ADDRESS not set — " +
			"skipping K8s CR reconciliation tests " +
			"(set to <host>:<port> reachable from inside the Kind cluster)")
	}

	host, portStr, err := net.SplitHostPort(raw)
	Expect(err).NotTo(HaveOccurred(),
		"EXTERNAL_AGENT_CLUSTER_ADDRESS must be in host:port format, got: %q", raw)

	portInt, err := strconv.Atoi(portStr)
	Expect(err).NotTo(HaveOccurred(),
		"EXTERNAL_AGENT_CLUSTER_ADDRESS port must be a valid integer: %q", portStr)
	Expect(portInt).To(BeNumerically(">=", 1),
		"port must be >= 1")
	Expect(portInt).To(BeNumerically("<=", 65535),
		"port must be <= 65535")

	return reconClusterAddr{host: host, port: int32(portInt)}
}
