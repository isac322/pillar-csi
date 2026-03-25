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

// external_agent_test.go — E2E tests for the external (out-of-cluster) agent mode.
//
// This file scaffolds a Ginkgo test suite that starts the pillar-agent binary
// directly on the test host (out-of-cluster), dials it over gRPC, and verifies
// that the pillar-csi controller can reach it through a PillarTarget CR whose
// Spec.External.Address points at the locally-bound port.
//
// # Cluster prerequisites
//
// The Kind cluster (pillar-csi-e2e) must already exist and have the pillar-csi
// Helm chart deployed.  Run hack/e2e-setup.sh first, then set KUBECONFIG:
//
//	export KUBECONFIG=$(kind get kubeconfig --name pillar-csi-e2e)
//
// The agent binary must be compiled before running:
//
//	make build     # produces bin/pillar-agent
//
// # Running the suite
//
//	go test -tags=e2e ./test/e2e/ -v -run TestExternalAgent
//
// Alternatively, use the e2e Makefile target which sets KUBECONFIG and builds
// the binary automatically before invoking go test.
//
// # Configuration
//
// All tunable parameters are read from environment variables so that the suite
// works both in CI (ubuntu-latest) and on a developer's macOS workstation:
//
//	EXTERNAL_AGENT_BINARY    path to compiled agent binary
//	                         (default: bin/pillar-agent relative to repo root)
//	EXTERNAL_AGENT_PORT      TCP port for the out-of-cluster agent to listen on
//	                         (default: 9501; use ≠ 9500 to avoid collision with
//	                         the Docker-based agent started by e2e-external-agent.sh)
//	EXTERNAL_AGENT_ZFS_POOL  ZFS pool name passed via --zfs-pool
//	                         (default: e2e-pool)
//	AGENT_READY_TIMEOUT      seconds to wait for the gRPC port to open
//	                         (default: 30)
//	KUBECONFIG               path to kubeconfig for the Kind cluster
//	                         (default: standard kubeconfig lookup order)
//
// # Design notes
//
//   - Agent lifecycle (start/stop) is managed with BeforeAll/AfterAll inside an
//     Ordered Describe block rather than at suite (BeforeSuite/AfterSuite) level.
//     This avoids conflicting with the global BeforeSuite in e2e_suite_test.go.
//
//   - The agent is started with --configfs-root pointing at a t.TempDir so that
//     NVMe-oF configfs path code is exercised without needing kernel nvmet modules.
//
//   - Kubeconfig for the Kind cluster is loaded via framework.SetupSuite which
//     honours the KUBECONFIG env var (set by kind get kubeconfig or e2e-setup.sh).
//
//   - All cleanup (process termination, temp dir removal) is registered with
//     DeferCleanup / AfterAll so that it runs even when a spec panics or fails.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
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
// Suite entry point
// ─────────────────────────────────────────────────────────────────────────────

// TestExternalAgent is the Ginkgo entry point for the external-agent e2e suite.
//
// Run with:
//
//	KUBECONFIG=$(kind get kubeconfig --name pillar-csi-e2e) \
//	  go test -tags=e2e ./test/e2e/ -v -run TestExternalAgent
func TestExternalAgent(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting ExternalAgent e2e suite\n")
	RunSpecs(t, "ExternalAgent E2E Suite")
}

// ─────────────────────────────────────────────────────────────────────────────
// Suite-level state (populated in BeforeAll, read by all specs)
// ─────────────────────────────────────────────────────────────────────────────

// externalAgentState bundles every resource allocated during BeforeAll so
// that AfterAll can release it all in one place.
type externalAgentState struct {
	// addr is the "host:port" of the locally-running agent gRPC server.
	addr string

	// proc is the running agent OS process.  Terminated in AfterAll.
	proc *os.Process

	// tmpDir is the base temporary directory for this suite run.
	// Contains the simulated configfs subtree.  Removed in AfterAll.
	tmpDir string

	// suite wraps the controller-runtime client connected to the Kind cluster.
	// Specs use suite.Client to create/delete Kubernetes CRs.
	suite *framework.Suite
}

// extState is the package-level singleton populated by BeforeAll.
var extState *externalAgentState

// ─────────────────────────────────────────────────────────────────────────────
// ExternalAgent Ginkgo suite
// ─────────────────────────────────────────────────────────────────────────────

var _ = Describe("ExternalAgent", Ordered, func() {
	// ── BeforeAll: cluster connect + agent process lifecycle ────────────────
	//
	// BeforeAll runs once before the first spec in this Ordered Describe block.
	// It is roughly equivalent to a BeforeSuite scoped to these specs only,
	// which avoids conflicts with the global BeforeSuite in e2e_suite_test.go.
	BeforeAll(func(ctx SpecContext) {
		extState = &externalAgentState{}

		// 1. Resolve the agent binary path from env or derive from repo root.
		agentBin := extAgentResolveBinary()
		By(fmt.Sprintf("agent binary: %s", agentBin))
		Expect(agentBin).NotTo(BeEmpty(), "EXTERNAL_AGENT_BINARY or default bin/pillar-agent must exist")

		// Verify binary exists and is executable before attempting to start.
		info, err := os.Stat(agentBin)
		Expect(err).NotTo(HaveOccurred(),
			"agent binary not found — build first with 'make build' or set EXTERNAL_AGENT_BINARY")
		Expect(info.Mode()&0o111).NotTo(BeZero(),
			"agent binary is not executable: %s", agentBin)

		// 2. Create a temporary directory tree for the run.
		//    <tmpDir>/configfs  →  agent --configfs-root (simulated nvmet fs)
		extState.tmpDir, err = os.MkdirTemp("", "pillar-csi-e2e-ext-agent-*")
		Expect(err).NotTo(HaveOccurred(), "create temporary suite directory")
		configfsRoot := filepath.Join(extState.tmpDir, "configfs")
		Expect(os.MkdirAll(configfsRoot, 0o750)).To(Succeed(), "create configfs simulation dir")
		By(fmt.Sprintf("configfs root: %s", configfsRoot))

		// 3. Determine listen address and ZFS pool name from env / defaults.
		port := extAgentPort()
		extState.addr = net.JoinHostPort("127.0.0.1", port)
		pool := extAgentZFSPool()
		By(fmt.Sprintf("external agent addr: %s  pool: %s", extState.addr, pool))

		// 4. Spawn the agent binary as a background process.
		extState.proc, err = extAgentStart(agentBin, extState.addr, pool, configfsRoot)
		Expect(err).NotTo(HaveOccurred(), "spawn external agent process")
		By(fmt.Sprintf("agent process started (pid %d)", extState.proc.Pid))

		// Register process cleanup with DeferCleanup so it fires even if a
		// later BeforeAll step fails (e.g. cluster connectivity).
		DeferCleanup(extAgentStop, extState)

		// 5. Wait for the agent's gRPC port to accept connections.
		readyTimeout := extAgentReadyTimeout()
		By(fmt.Sprintf("waiting up to %s for agent on %s", readyTimeout, extState.addr))
		Eventually(func() error {
			return extAgentProbe(extState.addr)
		}, readyTimeout, 500*time.Millisecond).Should(Succeed(),
			"agent gRPC port did not become ready within %s", readyTimeout)
		By(fmt.Sprintf("agent is ready at %s", extState.addr))

		// 6. Connect to the Kind cluster via KUBECONFIG (honours env var set by
		//    'kind get kubeconfig' or hack/e2e-setup.sh).
		extState.suite, err = framework.SetupSuite(
			framework.WithConnectTimeout(30 * time.Second),
		)
		Expect(err).NotTo(HaveOccurred(),
			"connect to Kind cluster — ensure KUBECONFIG is set and cluster is running")
		By("connected to Kind cluster")
	})

	// ── AfterAll: stop agent + remove temp dir ──────────────────────────────
	//
	// AfterAll runs once after the last spec in this Ordered Describe block.
	// DeferCleanup registered in BeforeAll also fires here, but AfterAll
	// provides an explicit label for clarity in verbose test output.
	AfterAll(func() {
		if extState != nil && extState.suite != nil {
			extState.suite.TeardownSuite()
		}
		// extAgentStop is already registered via DeferCleanup; it handles
		// process termination and tmpDir removal.
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
	// cluster (deployed by hack/e2e-setup.sh), and the agent must be reachable
	// from within the Kind nodes.
	//
	// Set EXTERNAL_AGENT_CLUSTER_ADDRESS=<host>:<port> to the address that is
	// reachable from within Kind pods (e.g. the Docker bridge gateway IP).
	// If the variable is empty these specs are skipped so that the suite remains
	// usable without a full cluster or when running direct gRPC tests only.
	//
	// Typical setup with the Docker-based agent helper:
	//
	//	AGENT_IP=$(hack/e2e-external-agent.sh | tail -1)
	//	export EXTERNAL_AGENT_CLUSTER_ADDRESS="${AGENT_IP}:9500"
	//	go test -tags=e2e ./test/e2e/ -v -run TestExternalAgent
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
			if extState == nil || extState.suite == nil {
				Skip("outer BeforeAll did not complete successfully — " +
					"skipping PillarTarget registration tests")
			}

			// Skip gracefully when the cluster-accessible address is not provided.
			clusterAddr := extAgentClusterAddress()
			if clusterAddr == "" {
				Skip("EXTERNAL_AGENT_CLUSTER_ADDRESS not set — " +
					"skipping PillarTarget registration tests " +
					"(set to <host>:<port> reachable from inside the Kind cluster)")
			}

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
})

// ─────────────────────────────────────────────────────────────────────────────
// Configuration helpers (environment variable resolution)
// ─────────────────────────────────────────────────────────────────────────────

// extAgentResolveBinary returns the path to the agent binary.
//
// Resolution order:
//  1. EXTERNAL_AGENT_BINARY env var (absolute or relative to CWD)
//  2. bin/pillar-agent two directories above the package (repo root)
//
// go test sets the working directory to the package directory (test/e2e/),
// so "../../bin/pillar-agent" resolves to <repo-root>/bin/pillar-agent.
func extAgentResolveBinary() string {
	if v := os.Getenv("EXTERNAL_AGENT_BINARY"); v != "" {
		return v
	}
	// Derive from the package working directory: test/e2e/ → ../../bin/
	rel := filepath.Join("..", "..", "bin", "pillar-agent")
	if abs, err := filepath.Abs(rel); err == nil {
		return abs
	}
	return rel
}

// extAgentPort returns the TCP port for the out-of-cluster agent.
// Reads EXTERNAL_AGENT_PORT (default: "9501").
// Default differs from the Docker-based agent port (9500) to allow both to run
// simultaneously during development.
func extAgentPort() string {
	if v := os.Getenv("EXTERNAL_AGENT_PORT"); v != "" {
		return v
	}
	return "9501"
}

// extAgentZFSPool returns the ZFS pool name passed to the agent via --zfs-pool.
// Reads EXTERNAL_AGENT_ZFS_POOL (default: "e2e-pool").
func extAgentZFSPool() string {
	if v := os.Getenv("EXTERNAL_AGENT_ZFS_POOL"); v != "" {
		return v
	}
	return "e2e-pool"
}

// extAgentReadyTimeout returns the maximum duration to wait for the agent's
// gRPC port to become live.
// Reads AGENT_READY_TIMEOUT in seconds (default: 30 s).
func extAgentReadyTimeout() time.Duration {
	if v := os.Getenv("AGENT_READY_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 30 * time.Second
}

// ─────────────────────────────────────────────────────────────────────────────
// Process management helpers
// ─────────────────────────────────────────────────────────────────────────────

// extAgentStart launches the agent binary as a background OS process and
// returns the *os.Process so the caller can terminate it later.
//
// The agent is started with:
//
//	<binary> --listen-address=<addr> --zfs-pool=<pool> --configfs-root=<dir>
//
// Agent stdout and stderr are forwarded to GinkgoWriter so that log output
// appears alongside test output and is captured in CI failure reports.
func extAgentStart(binary, addr, pool, configfsRoot string) (*os.Process, error) {
	cmd := exec.Command(binary,
		fmt.Sprintf("--listen-address=%s", addr),
		fmt.Sprintf("--zfs-pool=%s", pool),
		fmt.Sprintf("--configfs-root=%s", configfsRoot),
	)
	// Inherit test's GinkgoWriter for visibility during test runs.
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec agent %q: %w", binary, err)
	}
	return cmd.Process, nil
}

// extAgentStop terminates the agent process and removes the temporary
// directory.  It is registered via DeferCleanup in BeforeAll so it runs
// regardless of whether the suite passes or fails.
//
// Shutdown sequence:
//  1. Send os.Interrupt (SIGINT) for a graceful shutdown.
//  2. Wait up to 10 s for the process to exit.
//  3. Force-kill (SIGKILL) if the process is still alive after 10 s.
func extAgentStop(state *externalAgentState) {
	if state == nil {
		return
	}

	if state.proc != nil {
		pid := state.proc.Pid
		By(fmt.Sprintf("sending SIGINT to external agent (pid %d)", pid))

		// Graceful shutdown — ignore errors; process may have already exited.
		_ = state.proc.Signal(os.Interrupt)

		done := make(chan struct{})
		go func() {
			_, _ = state.proc.Wait()
			close(done)
		}()

		select {
		case <-done:
			By(fmt.Sprintf("external agent (pid %d) exited cleanly", pid))
		case <-time.After(10 * time.Second):
			By(fmt.Sprintf("external agent (pid %d) did not exit in 10 s — force-killing", pid))
			_ = state.proc.Kill()
			<-done
		}
		state.proc = nil
	}

	if state.tmpDir != "" {
		By(fmt.Sprintf("removing external agent temp dir: %s", state.tmpDir))
		_ = os.RemoveAll(state.tmpDir)
		state.tmpDir = ""
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// gRPC helpers
// ─────────────────────────────────────────────────────────────────────────────

// extAgentProbe attempts a plain TCP connection to addr and immediately closes
// it.  Returns nil when the port is open, an error otherwise.
//
// Used by the Eventually readiness loop to detect when the agent's gRPC
// listener is accepting connections.  A raw TCP probe is lighter than a full
// gRPC dial and works without requiring a gRPC handshake.
func extAgentProbe(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("tcp probe %s: %w", addr, err)
	}
	_ = conn.Close()
	return nil
}

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
// bridge network (e.g. 172.18.0.1:9501) or the container IP when the agent
// was started with hack/e2e-external-agent.sh.
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
