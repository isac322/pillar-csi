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

// external_agent_test.go — E2E specs for the external (out-of-cluster) agent mode.
//
// These specs are registered as part of the unified e2e Ginkgo suite (TestE2E
// in e2e_suite_test.go) and run automatically when go test is invoked with
// the e2e build tag.  There is no separate entry point — all lifecycle
// management (Kind cluster, Docker images, Helm install, external-agent
// Docker container) is performed by TestMain in setup_test.go.
//
// # Enabling external-agent tests
//
// Set E2E_LAUNCH_EXTERNAL_AGENT=true before running the suite to have TestMain
// start a Docker container running the agent image on the Kind network and
// expose it to both the test process (127.0.0.1:<port>) and to in-cluster
// pods via the "kind" Docker bridge:
//
//	make test-e2e E2E_LAUNCH_EXTERNAL_AGENT=true
//
// Alternatively, point the suite at an already-running agent by setting
// EXTERNAL_AGENT_ADDR=<host>:<port> and EXTERNAL_AGENT_CLUSTER_ADDRESS.
//
// When neither variable is set these specs skip automatically.
//
// # Design notes
//
//   - Agent lifecycle is owned by TestMain.  The Ginkgo specs here consume
//     testEnv.ExternalAgentAddr which is populated by startExternalAgentContainer
//     before m.Run() is called.
//
//   - Cluster connectivity uses framework.SetupSuite which honours the KUBECONFIG
//     env var.  KUBECONFIG is exported by TestMain's ensureKindCluster so no
//     additional setup is required in these specs.
//
//   - All cleanup (PillarTarget deletion, suite teardown) is registered with
//     DeferCleanup / AfterAll so that it runs even when a spec panics or fails.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Suite-level state (populated in BeforeAll, read by all specs)
// ─────────────────────────────────────────────────────────────────────────────

// externalAgentState bundles every resource allocated during BeforeAll so
// that AfterAll can release it all in one place.
type externalAgentState struct {
	// addr is the "host:port" of the Docker-started agent gRPC server.
	// Populated from testEnv.ExternalAgentAddr which is set by TestMain's
	// startExternalAgentContainer before m.Run() is called.
	addr string

	// suite wraps the controller-runtime client connected to the Kind cluster.
	// Specs use suite.Client to create/delete Kubernetes CRs.
	suite *framework.Suite
}

// extState is the package-level singleton populated by BeforeAll.
var extState *externalAgentState

// ─────────────────────────────────────────────────────────────────────────────
// ExternalAgent Ginkgo suite
// ─────────────────────────────────────────────────────────────────────────────

var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ExternalAgent", Ordered, func() {
	// ── BeforeAll: connect to Docker-started agent + cluster ────────────────
	//
	// BeforeAll runs once before the first spec in this Ordered Describe block.
	// It guards against the suite running in internal-agent mode (where
	// testEnv.ExternalAgentAddr is empty) and wires specs to the Docker
	// container started by TestMain.
	BeforeAll(func(ctx SpecContext) {
		extState = &externalAgentState{}

		// Consume the Docker-started agent address from TestMain.
		extState.addr = testEnv.ExternalAgentAddr
		By(fmt.Sprintf("external agent addr (from TestMain): %s", extState.addr))

		// Connect to the Kind cluster.  KUBECONFIG is already exported by
		// TestMain's ensureKindCluster so framework.SetupSuite picks it up.
		var err error
		extState.suite, err = framework.SetupSuite(
			framework.WithConnectTimeout(30 * time.Second),
		)
		Expect(err).NotTo(HaveOccurred(),
			"connect to Kind cluster — KUBECONFIG must be set by TestMain")
		By("connected to Kind cluster")
	})

	// ── AfterAll: disconnect from cluster ───────────────────────────────────
	AfterAll(func() {
		if extState != nil && extState.suite != nil {
			extState.suite.TeardownSuite()
		}
	})

	// ────────────────────────────────────────────────────────────────────────
	// Specs
	// ────────────────────────────────────────────────────────────────────────

	// Basic connectivity: verify the out-of-cluster agent responds to RPCs.

	It("responds to GetCapabilities with a valid agent version", func(ctx SpecContext) {
		conn := extAgentDial(extState.addr)
		DeferCleanup(conn.Close)

		c := agentv1.NewAgentServiceClient(conn)
		resp, err := c.GetCapabilities(ctx, &agentv1.GetCapabilitiesRequest{})
		Expect(err).NotTo(HaveOccurred(), "GetCapabilities RPC to external agent")
		Expect(resp.GetAgentVersion()).NotTo(BeEmpty(),
			"agent version must be a non-empty string")
		By(fmt.Sprintf("agent version: %s", resp.GetAgentVersion()))
	})

	It("responds to HealthCheck with a timestamp and subsystem list", func(ctx SpecContext) {
		conn := extAgentDial(extState.addr)
		DeferCleanup(conn.Close)

		c := agentv1.NewAgentServiceClient(conn)
		resp, err := c.HealthCheck(ctx, &agentv1.HealthCheckRequest{})
		Expect(err).NotTo(HaveOccurred(), "HealthCheck RPC to external agent")
		Expect(resp.GetCheckedAt()).NotTo(BeNil(),
			"HealthCheck response must include a CheckedAt timestamp")
		Expect(len(resp.GetSubsystems())).To(BeNumerically(">", 0),
			"HealthCheck must report at least one subsystem")
		By(fmt.Sprintf("health check: %d subsystem(s) reported", len(resp.GetSubsystems())))
	})

	It("GetCapacity returns NotFound for an unknown pool", func(ctx SpecContext) {
		conn := extAgentDial(extState.addr)
		DeferCleanup(conn.Close)

		c := agentv1.NewAgentServiceClient(conn)
		_, err := c.GetCapacity(ctx, &agentv1.GetCapacityRequest{
			PoolName: "no-such-pool",
		})
		Expect(err).To(HaveOccurred(), "GetCapacity with unknown pool must fail")
		By("unknown pool correctly returned an error")
	})

	It("the Kind cluster is reachable and CRDs are installed", func(ctx SpecContext) {
		Expect(extState.suite).NotTo(BeNil(), "suite must be set up in BeforeAll")
		Expect(extState.suite.Client).NotTo(BeNil(), "Kubernetes client must be initialised")
		By("Kind cluster connectivity verified via framework.SetupSuite")
	})

	// ─── PillarTarget registration tests ──────────────────────────────────────
	//
	// These specs create a PillarTarget CR that points the pillar-csi controller
	// at the running external agent, then verify that the controller:
	//
	//   • persists spec.external.address and spec.external.port correctly
	//   • transitions the AgentConnected condition to True (agent registered)
	//   • populates status.resolvedAddress, status.agentVersion, and
	//     status.capabilities after connecting
	//   • maintains the Ready condition across multiple reconcile cycles
	//     (heartbeat / lease stability)
	//
	// Prerequisite: the pillar-csi controller must be running inside the Kind
	// cluster and the agent must be reachable from within the Kind nodes.
	//
	// Set EXTERNAL_AGENT_CLUSTER_ADDRESS=<host>:<port> to the address reachable
	// from within Kind pods (e.g. the Docker bridge gateway IP).
	// TestMain sets this automatically when E2E_LAUNCH_EXTERNAL_AGENT=true.
	// If the variable is empty these specs are skipped.
	Context("PillarTarget registration", Ordered, func() {
		var (
			target      *v1alpha1.PillarTarget
			targetName  string
			clusterHost string
			clusterPort int32
		)

		// BeforeAll runs once, before the first spec in this Ordered Context.
		// It guards against missing prerequisites and creates the PillarTarget CR.
		BeforeAll(func(ctx SpecContext) {
			// Guard: require the outer BeforeAll to have initialised the suite.
			Expect(extState).NotTo(BeNil(), "outer BeforeAll must have initialised extState")
			Expect(extState.suite).NotTo(BeNil(), "outer BeforeAll must have connected to the cluster")

			// Fail when the cluster-accessible address is not provided.
			clusterAddr := extAgentClusterAddress()
			Expect(clusterAddr).NotTo(BeEmpty(), "EXTERNAL_AGENT_CLUSTER_ADDRESS must be set — TestMain sets this automatically when E2E_LAUNCH_EXTERNAL_AGENT=true")

			var ok bool
			clusterHost, clusterPort, ok = extAgentClusterAddrParts(clusterAddr)
			Expect(ok).To(BeTrue(),
				"EXTERNAL_AGENT_CLUSTER_ADDRESS must be in host:port format, got: %q", clusterAddr)

			// Use a millisecond-based suffix so parallel runs don't collide.
			targetName = fmt.Sprintf("ext-agent-reg-%d", time.Now().UnixMilli()%100000)
			target = framework.NewExternalPillarTarget(targetName, clusterHost, clusterPort)

			By(fmt.Sprintf("creating PillarTarget %q → %s:%d", targetName, clusterHost, clusterPort))
			Expect(framework.Apply(ctx, extState.suite.Client, target)).To(Succeed(),
				"apply PillarTarget CR to the Kind cluster")

			// Register cleanup now that the CR exists.  DeferCleanup in a
			// BeforeAll fires after this Ordered Context's last spec or AfterAll.
			// Ginkgo injects a fresh SpecContext for the cleanup closure.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleaning up PillarTarget %q", targetName))
				if err := framework.EnsureGone(dctx, extState.suite.Client, target, 2*time.Minute); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup PillarTarget %q: %v\n", targetName, err)
				}
			})
		})

		// ── spec 1: spec fields are persisted correctly ──────────────────────

		It("persists spec.external.address and spec.external.port", func(ctx SpecContext) {
			got := &v1alpha1.PillarTarget{}
			Expect(extState.suite.Client.Get(ctx,
				client.ObjectKey{Name: targetName}, got)).To(Succeed(),
				"PillarTarget %q must exist in the cluster", targetName)

			Expect(got.Spec.External).NotTo(BeNil(),
				"spec.external must be populated for an external agent target")
			Expect(got.Spec.NodeRef).To(BeNil(),
				"spec.nodeRef must be nil when spec.external is used (discriminated union)")
			Expect(got.Spec.External.Address).To(Equal(clusterHost),
				"spec.external.address must match the configured host exactly")
			Expect(got.Spec.External.Port).To(Equal(clusterPort),
				"spec.external.port must match the configured port exactly")

			By(fmt.Sprintf("spec.external validated: address=%s port=%d",
				got.Spec.External.Address, got.Spec.External.Port))
		})

		// ── spec 2: controller dials agent → AgentConnected=True ────────────

		It("controller transitions AgentConnected condition to True", func(ctx SpecContext) {
			By(fmt.Sprintf("waiting for AgentConnected=True on PillarTarget %q (up to 2 min)", targetName))
			err := framework.WaitForCondition(ctx, extState.suite.Client, target,
				"AgentConnected", metav1.ConditionTrue, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"AgentConnected must become True — verify the controller is running and "+
					"can reach %s:%d from inside the cluster", clusterHost, clusterPort)

			By("AgentConnected=True: controller successfully dialled the external agent")
		})

		// ── spec 3: status.resolvedAddress ──────────────────────────────────

		It("status.resolvedAddress is populated after agent connection", func(ctx SpecContext) {
			// WaitForField re-fetches the object on each poll; target is updated
			// in-place so the final value is available after the wait.
			err := framework.WaitForField(ctx, extState.suite.Client, target,
				func(t *v1alpha1.PillarTarget) bool {
					return t.Status.ResolvedAddress != ""
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"status.resolvedAddress must be populated once AgentConnected=True")
			Expect(target.Status.ResolvedAddress).To(Equal(clusterHost),
				"resolvedAddress must match the configured external agent address")

			By(fmt.Sprintf("status.resolvedAddress = %q", target.Status.ResolvedAddress))
		})

		// ── spec 4: status.agentVersion ─────────────────────────────────────

		It("status.agentVersion is reported by the connected agent", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, extState.suite.Client, target,
				func(t *v1alpha1.PillarTarget) bool {
					return t.Status.AgentVersion != ""
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"status.agentVersion must be set once the controller connects to the agent")
			Expect(target.Status.AgentVersion).NotTo(BeEmpty(),
				"agentVersion must be a non-empty string returned by GetCapabilities RPC")

			By(fmt.Sprintf("status.agentVersion = %q", target.Status.AgentVersion))
		})

		// ── spec 5: status.capabilities ─────────────────────────────────────

		It("status.capabilities lists at least one backend", func(ctx SpecContext) {
			err := framework.WaitForField(ctx, extState.suite.Client, target,
				func(t *v1alpha1.PillarTarget) bool {
					return t.Status.Capabilities != nil &&
						len(t.Status.Capabilities.Backends) > 0
				}, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"status.capabilities must be populated from the agent's GetCapabilities response")

			Expect(target.Status.Capabilities).NotTo(BeNil(),
				"capabilities struct must be non-nil once the agent is connected")
			Expect(target.Status.Capabilities.Backends).NotTo(BeEmpty(),
				"agent must advertise at least one backend type (e.g. zfs-zvol, lvm-lv)")

			By(fmt.Sprintf("status.capabilities: backends=%v protocols=%v",
				target.Status.Capabilities.Backends,
				target.Status.Capabilities.Protocols))
		})

		// ── spec 6: Ready=True ───────────────────────────────────────────────

		It("Ready condition becomes True", func(ctx SpecContext) {
			By(fmt.Sprintf("waiting for Ready=True on PillarTarget %q", targetName))
			err := framework.WaitForReady(ctx, extState.suite.Client, target, 2*time.Minute)
			Expect(err).NotTo(HaveOccurred(),
				"PillarTarget must reach Ready=True once the agent is connected and healthy")
			By("PillarTarget is Ready=True")
		})

		// ── spec 7: heartbeat / lease stability ─────────────────────────────
		//
		// This spec verifies that the controller continuously re-contacts the
		// agent (heartbeat) and does not allow the Ready condition to lapse.
		//
		// Design: observe Ready=True for 30 s, polling every 5 s.  We also
		// record the condition's LastTransitionTime up front and assert it never
		// changes — a changed transition time would indicate the condition flipped
		// to False (agent unreachable) and then back to True.

		It("Ready condition is maintained across reconcile cycles (heartbeat)", func(ctx SpecContext) {
			// Read the current state to obtain the initial LastTransitionTime.
			fresh := &v1alpha1.PillarTarget{}
			Expect(extState.suite.Client.Get(ctx,
				client.ObjectKey{Name: targetName}, fresh)).To(Succeed(),
				"re-read PillarTarget to obtain baseline condition state")

			var readyCond *metav1.Condition
			for i := range fresh.Status.Conditions {
				if fresh.Status.Conditions[i].Type == "Ready" {
					c := fresh.Status.Conditions[i]
					readyCond = &c
					break
				}
			}
			Expect(readyCond).NotTo(BeNil(),
				"Ready condition must be present before the heartbeat observation window")
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue),
				"Ready must already be True before beginning the heartbeat check")

			initialTransition := readyCond.LastTransitionTime
			By(fmt.Sprintf(
				"Ready=True since %s — observing stability for 30 s (polling every 5 s)",
				initialTransition.UTC().Format(time.RFC3339)))

			// Consistently asserts the predicate holds for the full duration.
			// Each iteration re-fetches the object so we see real API-server state.
			Consistently(func(g Gomega) {
				current := &v1alpha1.PillarTarget{}
				g.Expect(extState.suite.Client.Get(ctx,
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
					"Ready condition must remain True throughout the 30 s observation window "+
						"(agent heartbeat/lease must be maintained)")
				g.Expect(cond.LastTransitionTime).To(Equal(initialTransition),
					"Ready condition must not flip during observation — "+
						"a changed LastTransitionTime indicates the heartbeat was interrupted")
			}, 30*time.Second, 5*time.Second).Should(Succeed(),
				"Ready=True stability check failed: agent heartbeat/lease not maintained")

			By("heartbeat confirmed: Ready=True held for 30 s without condition flip")
		})
	})
	}) // end Describe("ExternalAgent")
	return true
}()

// ─────────────────────────────────────────────────────────────────────────────
// Configuration helpers (environment variable resolution)
// ─────────────────────────────────────────────────────────────────────────────

// extAgentZFSPool returns the ZFS pool name passed to the agent via --zfs-pool.
// Reads EXTERNAL_AGENT_ZFS_POOL (default: "e2e-pool").
func extAgentZFSPool() string {
	if v := os.Getenv("EXTERNAL_AGENT_ZFS_POOL"); v != "" {
		return v
	}
	return "e2e-pool"
}

// ─────────────────────────────────────────────────────────────────────────────
// gRPC helpers
// ─────────────────────────────────────────────────────────────────────────────

// extAgentDial opens a plaintext gRPC connection to the external agent at addr
// and returns it.  The connection is not closed by this function — callers
// should register conn.Close with DeferCleanup.
//
// The function fails the current spec immediately (via Expect) if the dial
// cannot be established, so callers do not need to check the error themselves.
func extAgentDial(addr string) *grpc.ClientConn {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck // DialContext is still widely used; NewClient lacks per-call ctx
		ctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	Expect(err).NotTo(HaveOccurred(),
		"gRPC dial to external agent at %s failed — is the agent running?", addr)
	return conn
}

// ─────────────────────────────────────────────────────────────────────────────
// PillarTarget registration helpers
// ─────────────────────────────────────────────────────────────────────────────

// extAgentClusterAddress returns the address (host:port) at which the external
// agent is reachable from within the Kind cluster pods.
//
// This is distinct from extState.addr, which is always 127.0.0.1:<port>
// (host-local).  The controller runs inside the Kind cluster and therefore
// needs a routable address — typically the host machine's IP on the Docker
// bridge network (e.g. 172.18.0.1:9500) or the container IP.
//
// TestMain sets EXTERNAL_AGENT_CLUSTER_ADDRESS automatically when
// E2E_LAUNCH_EXTERNAL_AGENT=true via startExternalAgentContainer.
//
// Reads EXTERNAL_AGENT_CLUSTER_ADDRESS (default: "").
// When empty, PillarTarget registration tests are skipped.
func extAgentClusterAddress() string {
	return os.Getenv("EXTERNAL_AGENT_CLUSTER_ADDRESS")
}

// extAgentClusterAddrParts splits a "host:port" address string into its
// constituent host and int32 port.  Returns ("", 0, false) for any parse or
// range error so callers can produce an actionable Expect failure message.
func extAgentClusterAddrParts(addr string) (host string, port int32, ok bool) {
	h, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, false
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil || portInt < 1 || portInt > 65535 {
		return "", 0, false
	}
	return h, int32(portInt), true
}
