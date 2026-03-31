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

package e2e

// setup_test.go — TestMain-based e2e test lifecycle management.
//
// This file owns the full lifecycle of the e2e Kind cluster AND, when
// external-agent mode is requested, the Docker container that runs the
// out-of-cluster pillar-agent:
//
//  1. TestMain reads configuration from environment variables.
//  2. A multi-node Kind cluster is always freshly created from the embedded
//     kind-config.yaml located in testdata/ (any pre-existing cluster with the
//     same name is deleted first).
//  3. Pillar-CSI Docker images are built and loaded into the cluster.
//  4. When E2E_LAUNCH_EXTERNAL_AGENT=true, a Docker container running the
//     agent image is started on the Kind Docker network, port-mapped to the
//     host, and polled until the gRPC port is accepting connections.
//  5. The Helm chart is installed via `helm upgrade --install`.
//  6. All e2e test functions (including the Ginkgo suite in e2e_suite_test.go)
//     are executed via m.Run().
//  7. The Helm release is removed, the external-agent container (if any) is
//     stopped and removed, and the Kind cluster is always deleted.  Teardown
//     is unconditional: it runs via defer even when m.Run() or setup panics.
//
// Shared state is stored in the package-level testEnv variable so that
// individual test files can read cluster coordinates (kubeconfig path, image
// tag, Helm namespace, etc.) without re-parsing the environment.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Shared global state
// ─────────────────────────────────────────────────────────────────────────────

// E2EEnv holds all cluster and configuration state that is initialised once by
// TestMain and shared across every e2e spec.  Fields are exported so that
// helper packages (e.g. test/e2e/framework) can read them via the testEnv
// package variable below.
type E2EEnv struct {
	// ClusterName is the Kind cluster name. Sourced from KIND_CLUSTER env var;
	// defaults to "pillar-csi-e2e".
	ClusterName string

	// DockerHost is the Docker daemon endpoint used for all docker, kind, and
	// helm sub-processes spawned by the e2e lifecycle helpers.  Sourced from
	// DOCKER_HOST.  When empty, sub-processes use Docker's default behaviour
	// (local Unix socket).  Injected explicitly into every exec.Command env
	// only when non-empty.
	DockerHost string

	// KubeconfigPath is the absolute path to the kubeconfig file written by
	// "kind get kubeconfig".  KUBECONFIG is also set to this value so that
	// kubectl, Helm, and client-go all pick it up automatically.
	KubeconfigPath string

	// ImageTag is the Docker image tag applied to the controller, agent, and
	// node images.  Sourced from E2E_IMAGE_TAG; defaults to "e2e".
	ImageTag string

	// HelmRelease is the Helm release name.  Sourced from E2E_HELM_RELEASE;
	// defaults to "pillar-csi".
	HelmRelease string

	// HelmNamespace is the namespace into which the Helm chart is deployed.
	// Sourced from E2E_HELM_NAMESPACE; defaults to "pillar-csi-system".
	HelmNamespace string

	// ExternalAgentAddr is the host:port address of the out-of-cluster agent
	// when running the external-agent test suite.  When LaunchExternalAgent is
	// true this is populated automatically by startExternalAgentContainer.
	// May also be pre-set via EXTERNAL_AGENT_ADDR to point at an already-running
	// external agent without starting a new container.  Empty means "internal-agent mode".
	ExternalAgentAddr string

	// LaunchExternalAgent tells TestMain to start a Docker container running
	// the agent image before tests, and to stop/remove it afterwards.
	// Sourced from E2E_LAUNCH_EXTERNAL_AGENT; default: false.
	LaunchExternalAgent bool

	// ExternalAgentPort is the host-side TCP port used when LaunchExternalAgent
	// is true.  The container's internal port 9500 is mapped to this port on
	// 127.0.0.1 so the test process can dial the agent directly.
	// Sourced from E2E_EXTERNAL_AGENT_PORT; default: "9500".
	ExternalAgentPort string

	// ExternalAgentReadyTimeout is how long TestMain waits for the agent
	// container's gRPC port to become reachable before giving up.
	// Sourced from E2E_EXTERNAL_AGENT_READY_TIMEOUT in seconds; default: 60 s.
	ExternalAgentReadyTimeout time.Duration

	// ExternalAgentContainerName is the Docker container name of a pre-existing
	// external-agent container when using EXTERNAL_AGENT_ADDR to point at an
	// already-running agent.  When non-empty, NVMe-oF mount tests can docker-exec
	// into this container even without E2E_LAUNCH_EXTERNAL_AGENT=true.
	// Sourced from E2E_EXTERNAL_AGENT_CONTAINER_NAME; empty means derive from
	// the Kind cluster name ("<KIND_CLUSTER>-agent").
	ExternalAgentContainerName string

	// externalAgentContainerID is the Docker container ID created by
	// startExternalAgentContainer.  Empty when LaunchExternalAgent is false or
	// when the container could not be started.  Used by teardown to stop/remove.
	externalAgentContainerID string

	// ── ZFS loopback pool ─────────────────────────────────────────────────

	// ZFSPoolName is the ZFS pool name created on the remote host.
	// Sourced from E2E_ZFS_POOL; defaults to "e2e-pool".
	ZFSPoolName string

	// ZFSImagePath is the absolute path of the sparse backing image file
	// created on the remote host.
	// Sourced from E2E_ZFS_IMAGE_PATH; defaults to "/tmp/e2e-zfs.img".
	ZFSImagePath string

	// ZFSImageSize is the size string passed to truncate(1) when creating
	// the backing image (e.g. "2G", "512M").
	// Sourced from E2E_ZFS_IMAGE_SIZE; defaults to "2G".
	ZFSImageSize string

	// zfsLoopDev is the loop device path returned by CreateLoopbackZFSPool
	// (e.g. "/dev/loop5").  Stored so teardownZFSPool can pass it to
	// DestroyLoopbackZFSPool.  Empty until setupZFSPool succeeds.
	zfsLoopDev string

	// zfsHostExec is the privileged exec helper created by setupZFSPool.
	// teardownZFSPool uses it to destroy the pool and then calls Close on it.
	// Nil until setupZFSPool has successfully started the helper container.
	zfsHostExec *framework.DockerHostExec

	// ── LVM loopback VG ──────────────────────────────────────────────────────

	// LVMVGName is the LVM Volume Group name created inside the Kind storage
	// worker container.  Sourced from E2E_LVM_VG; defaults to "e2e-vg".
	LVMVGName string

	// LVMThinPoolName is the thin pool LV name within LVMVGName.
	// Sourced from E2E_LVM_THINPOOL; defaults to "e2e-thinpool".
	LVMThinPoolName string

	// LVMImagePath is the absolute path inside the Kind worker container for
	// the sparse loopback image backing the LVM VG.
	// Sourced from E2E_LVM_IMAGE_PATH; defaults to "/tmp/e2e-lvm.img".
	LVMImagePath string

	// LVMImageSize is the size string passed to truncate(1) when creating
	// the backing image (e.g. "4G").
	// Sourced from E2E_LVM_IMAGE_SIZE; defaults to "4G".
	LVMImageSize string

	// LVMStorageNode is the Kind worker node/container name where the LVM VG
	// was created.  Populated by setupLVMVG after successful setup.
	LVMStorageNode string

	// lvmVGReady is true when setupLVMVG completed successfully and the VG
	// is active.  When false, LVM tests should be skipped.
	lvmVGReady bool

	// lvmLoopDev is the loop device path returned by SetupKindLVMVG
	// (e.g. "/dev/loop6").  Stored so teardownLVMVG can pass it to
	// TeardownKindLVMVG.  Empty until setupLVMVG succeeds.
	lvmLoopDev string

	// lvmHostExec is the privileged exec helper created by setupLVMVG.
	// teardownLVMVG uses it to destroy the VG and then calls Close on it.
	// Nil until setupLVMVG has successfully started the helper container.
	lvmHostExec *framework.DockerHostExec
}

// testEnv is the single, shared E2EEnv populated by TestMain.  All e2e test
// files should read cluster coordinates from this variable rather than
// re-querying the environment.
var testEnv = &E2EEnv{}

// defaultClusterName is used when KIND_CLUSTER is not set.
const defaultClusterName = "pillar-csi-e2e"

// defaultDockerHost is empty — when DOCKER_HOST is not set in the calling
// environment, sub-processes inherit Docker's own default behaviour (typically
// the local Unix socket /var/run/docker.sock).  Set DOCKER_HOST explicitly
// when the daemon listens on a TCP socket or a remote host.
const defaultDockerHost = ""

// ─────────────────────────────────────────────────────────────────────────────
// TestMain — entry point
// ─────────────────────────────────────────────────────────────────────────────

// TestMain is the single entry point for the e2e test binary.  It controls the
// full cluster lifecycle so that every test function (including the Ginkgo
// suite defined in e2e_suite_test.go) runs inside a properly bootstrapped
// environment.
//
// The deferred os.Exit pattern below guarantees that teardownE2E always runs:
//
//	exitCode is set to 1 before any work begins.
//	The deferred closure captures the exitCode variable by reference.
//	m.Run() writes the actual test result into exitCode.
//	On return (normal or via panic+recover), the closure calls teardownE2E
//	followed by os.Exit(exitCode).
//
// This ensures correct propagation of test pass/fail status even when
// individual tests call t.Fatal or the process would otherwise exit 0 before
// cleanup finishes.
func TestMain(m *testing.M) {
	suiteStart := time.Now()
	exitCode := 1 // default to failure; m.Run() will overwrite on success

	// setupDuration and testsDuration are captured in the defer below so that
	// printTimingReport can include them in the final metrics summary.
	var setupDuration, testsDuration time.Duration

	defer func() {
		teardownStart := time.Now()
		teardownE2E()
		teardownDuration := time.Since(teardownStart)
		printTimingReport(suiteStart, setupDuration, testsDuration, teardownDuration, exitCode)
		os.Exit(exitCode)
	}()

	if err := initE2EEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: init env: %v\n", err)
		return // deferred teardown + os.Exit(1)
	}

	setupStart := time.Now()
	if err := setupE2E(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: setup: %v\n", err)
		setupDuration = time.Since(setupStart)
		return // deferred teardown + os.Exit(1)
	}
	setupDuration = time.Since(setupStart)

	testsStart := time.Now()
	exitCode = m.Run() // run all Test* functions (includes Ginkgo suite)
	testsDuration = time.Since(testsStart)
}

// ─────────────────────────────────────────────────────────────────────────────
// Environment initialisation
// ─────────────────────────────────────────────────────────────────────────────

// initE2EEnv populates testEnv from environment variables with sensible
// defaults.  No external commands are run at this stage.
func initE2EEnv() error {
	testEnv.ClusterName = envOrDefault("KIND_CLUSTER", defaultClusterName)
	testEnv.DockerHost = os.Getenv("DOCKER_HOST") // empty = Docker default (local socket)
	testEnv.ImageTag = envOrDefault("E2E_IMAGE_TAG", "e2e")
	testEnv.HelmRelease = envOrDefault("E2E_HELM_RELEASE", "pillar-csi")
	testEnv.HelmNamespace = envOrDefault("E2E_HELM_NAMESPACE", "pillar-csi-system")

	// External agent: pre-existing address (mutually exclusive with
	// LaunchExternalAgent — if both are set, the pre-existing address wins).
	testEnv.ExternalAgentAddr = os.Getenv("EXTERNAL_AGENT_ADDR")

	// External agent Docker container lifecycle.
	testEnv.LaunchExternalAgent = os.Getenv("E2E_LAUNCH_EXTERNAL_AGENT") == "true"
	testEnv.ExternalAgentPort = envOrDefault("E2E_EXTERNAL_AGENT_PORT", "9500")

	// Optional explicit container name for a pre-existing external-agent
	// container.  When set, NVMe-oF mount tests can docker-exec into this
	// container even when E2E_LAUNCH_EXTERNAL_AGENT is not true.
	testEnv.ExternalAgentContainerName = os.Getenv("E2E_EXTERNAL_AGENT_CONTAINER_NAME")

	// Parse ready timeout (seconds → duration).
	if secs := os.Getenv("E2E_EXTERNAL_AGENT_READY_TIMEOUT"); secs != "" {
		d, err := time.ParseDuration(secs + "s")
		if err != nil {
			return fmt.Errorf("invalid E2E_EXTERNAL_AGENT_READY_TIMEOUT %q: %w", secs, err)
		}
		testEnv.ExternalAgentReadyTimeout = d
	} else {
		testEnv.ExternalAgentReadyTimeout = 60 * time.Second
	}

	// ZFS loopback pool.
	testEnv.ZFSPoolName = envOrDefault("E2E_ZFS_POOL", "e2e-pool")
	testEnv.ZFSImagePath = envOrDefault("E2E_ZFS_IMAGE_PATH", "/tmp/e2e-zfs.img")
	testEnv.ZFSImageSize = envOrDefault("E2E_ZFS_IMAGE_SIZE", "4G")

	// LVM loopback VG.
	testEnv.LVMVGName = envOrDefault("E2E_LVM_VG", "e2e-vg")
	testEnv.LVMThinPoolName = envOrDefault("E2E_LVM_THINPOOL", "e2e-thinpool")
	testEnv.LVMImagePath = envOrDefault("E2E_LVM_IMAGE_PATH", "/tmp/e2e-lvm.img")
	testEnv.LVMImageSize = envOrDefault("E2E_LVM_IMAGE_SIZE", "4G")

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Setup
// ─────────────────────────────────────────────────────────────────────────────

// setupE2E orchestrates full cluster bootstrap:
//
//  1. Always freshly create the Kind cluster (deleting any pre-existing one).
//  2. Build and load Docker images.
//  3. When E2E_LAUNCH_EXTERNAL_AGENT=true and EXTERNAL_AGENT_ADDR is not
//     already set, start the external agent Docker container on the Kind
//     network and wait for it to become ready.
//  4. Install the Helm chart (with external-agent overlay when applicable).
func setupE2E() error {
	defer logStep("setupE2E (total)")()

	// Fail fast if Docker is unreachable — all subsequent steps depend on it.
	if err := verifyDockerAccess(); err != nil {
		return fmt.Errorf("docker: %w", err)
	}

	// Verify /dev/nvme-fabrics is a character device BEFORE creating the Kind
	// cluster.  kind-config.yaml bind-mounts this device into the compute-worker
	// node.  If nvme_fabrics is not loaded, Docker creates an empty directory
	// instead — and NodeStageVolume fails with "open /dev/nvme-fabrics: is a
	// directory".  This check catches the problem before the slow cluster-create.
	if err := verifyHostNVMeFabrics(); err != nil {
		return fmt.Errorf("nvme-fabrics device: %w", err)
	}

	if err := ensureKindCluster(); err != nil {
		return fmt.Errorf("kind cluster: %w", err)
	}
	if err := ensureStorageNodeLabel(); err != nil {
		return fmt.Errorf("storage node label: %w", err)
	}
	if err := validateWorkerNodeMounts(); err != nil {
		return fmt.Errorf("worker node mounts: %w", err)
	}
	// ── Parallel: buildAndLoadImages + setupZFSPool + setupLVMVG ─────────
	//
	// All three operations are fully independent:
	//   - buildAndLoadImages builds and loads pillar-csi Docker images into
	//     Kind nodes using the local Docker daemon and Kind network.
	//   - setupZFSPool creates a loopback ZFS pool on the remote Docker host
	//     via a privileged helper container; it does not touch Kind nodes.
	//   - setupLVMVG creates a loopback LVM Volume Group inside the Kind
	//     storage-node container via docker exec, requiring no separate
	//     host-exec helper.
	//
	// Running them concurrently overlaps the image-build/load latency
	// (~60-90 s) with storage setup (~30-45 s each), eliminating the
	// sequential bottleneck without any ordering constraint between them.
	//
	// Error semantics: buildAndLoadImages and setupZFSPool send to
	// parallelErrCh on failure (fatal — both are required for the test suite).
	// setupLVMVG is non-fatal: LVM tests are gated on PILLAR_E2E_LVM_VG so a
	// failure only causes LVM specs to be skipped while ZFS specs continue.
	parallelSetupDone := logStep("  buildAndLoadImages + setupZFSPool + setupLVMVG (parallel)")
	// parallelErrCh capacity 2 — image-build and ZFS pool failures are fatal.
	parallelErrCh := make(chan error, 2)
	var parallelWg sync.WaitGroup

	parallelWg.Add(1)
	go func() {
		defer parallelWg.Done()
		if err := buildAndLoadImages(); err != nil {
			parallelErrCh <- fmt.Errorf("docker images: %w", err)
		}
	}()

	parallelWg.Add(1)
	go func() {
		defer parallelWg.Done()
		if err := setupZFSPool(); err != nil {
			parallelErrCh <- fmt.Errorf("zfs pool: %w", err)
		}
	}()

	parallelWg.Add(1)
	go func() {
		defer parallelWg.Done()
		if err := setupLVMVG(); err != nil {
			// Non-fatal: LVM tests are gated on PILLAR_E2E_LVM_VG.
			fmt.Fprintf(os.Stderr, "e2e setup: warning: lvm vg setup failed (LVM tests will be skipped): %v\n", err)
		}
	}()

	parallelWg.Wait()
	close(parallelErrCh)
	parallelSetupDone()
	for err := range parallelErrCh {
		if err != nil {
			return err
		}
	}

	// Export the ZFS pool name so that the functional e2e test suite
	// (internal_agent_functional_test.go) can gate ZFS-dependent spec groups
	// on its availability.  The pool was just created by setupZFSPool above;
	// setting PILLAR_E2E_ZFS_POOL enables the CR stack lifecycle, CSI
	// provisioning, and mount/unmount test groups.
	if testEnv.ZFSPoolName != "" {
		if err := os.Setenv("PILLAR_E2E_ZFS_POOL", testEnv.ZFSPoolName); err != nil {
			return fmt.Errorf("setenv PILLAR_E2E_ZFS_POOL: %w", err)
		}
	}

	// Export the LVM VG name so that LVM-dependent spec groups can gate on its
	// availability.  The VG was created by setupLVMVG above; setting
	// PILLAR_E2E_LVM_VG enables the LVM CR stack lifecycle and CSI provisioning
	// test groups.
	if testEnv.lvmVGReady {
		if err := os.Setenv("PILLAR_E2E_LVM_VG", testEnv.LVMVGName); err != nil {
			return fmt.Errorf("setenv PILLAR_E2E_LVM_VG: %w", err)
		}
		if testEnv.LVMThinPoolName != "" {
			if err := os.Setenv("PILLAR_E2E_LVM_THIN_POOL", testEnv.LVMThinPoolName); err != nil {
				return fmt.Errorf("setenv PILLAR_E2E_LVM_THIN_POOL: %w", err)
			}
		}
	}

	// Start the external agent container when requested and no pre-existing
	// address has been supplied via EXTERNAL_AGENT_ADDR.
	if testEnv.LaunchExternalAgent && testEnv.ExternalAgentAddr == "" {
		if err := startExternalAgentContainer(); err != nil {
			return fmt.Errorf("external agent container: %w", err)
		}
	}

	// Wait for the Kubernetes API server to be healthy before running Helm.
	//
	// Loading Docker images into kind node containers can spike memory
	// usage and cause the kube-apiserver to restart briefly.  We poll
	// "kubectl version" until it succeeds (or we time out) to ensure the
	// API is ready before the Helm install sends API requests.
	if err := waitForAPIServer(5 * time.Minute); err != nil {
		return fmt.Errorf("API server not ready before helm install: %w", err)
	}

	if err := installHelm(); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	return nil
}

// verifyDockerAccess runs `docker info` to confirm the Docker daemon is
// reachable.  Called at the very start of setupE2E so that permission errors,
// network failures, or missing DOCKER_HOST produce a clear, early message
// instead of a cryptic timeout deep in the cluster-creation flow.
func verifyDockerAccess() error {
	defer logStep("verifyDockerAccess")()
	endpoint := testEnv.DockerHost
	if endpoint == "" {
		endpoint = "(default local socket)"
	}
	fmt.Fprintf(os.Stdout, "e2e setup: verifying Docker daemon access [%s]\n", endpoint)

	out, err := captureOutput("docker", "info", "--format", "{{.ServerVersion}}")
	trimmed := strings.TrimSpace(out)
	if err != nil {
		return fmt.Errorf(
			"cannot reach Docker daemon [%s]: %s: %w\n"+
				"  Ensure the Docker daemon is running and accessible.\n"+
				"  Set DOCKER_HOST if the daemon listens on a non-default endpoint.",
			endpoint, trimmed, err)
	}
	// docker info can exit 0 even when the daemon is unreachable (e.g. the
	// CLI prints a connection error as output).  Detect this by checking
	// whether the output looks like a version string (digits and dots) rather
	// than an error message.
	if trimmed == "" || strings.Contains(trimmed, "dial tcp") || strings.Contains(trimmed, "Cannot connect") {
		return fmt.Errorf(
			"cannot reach Docker daemon [%s]: docker info returned: %s\n"+
				"  Ensure the Docker daemon is running and accessible.\n"+
				"  Set DOCKER_HOST if the daemon listens on a non-default endpoint.",
			endpoint, trimmed)
	}
	fmt.Fprintf(os.Stdout, "e2e setup: Docker daemon OK (server %s)\n", trimmed)
	return nil
}

// verifyHostNVMeFabrics checks that /dev/nvme-fabrics is a character device on
// the Docker host.  This MUST run before "kind create cluster" because
// kind-config.yaml bind-mounts /dev/nvme-fabrics into the compute-worker node.
// If the nvme_fabrics kernel module is not loaded, /dev/nvme-fabrics does not
// exist and Docker creates an empty directory at the mount-source path — making
// NVMe-oF connect fail with "open /dev/nvme-fabrics: is a directory".
func verifyHostNVMeFabrics() error {
	defer logStep("verifyHostNVMeFabrics")()
	// Use `docker run --rm --privileged` to check on the Docker host.
	out, err := captureOutput("docker", "run", "--rm",
		"--privileged", "--pid=host",
		framework.ImageDebianBookwormSlim,
		"nsenter", "-t", "1", "-m", "--",
		"test", "-c", "/dev/nvme-fabrics",
	)
	if err != nil {
		return fmt.Errorf(
			"/dev/nvme-fabrics is not a character device on the Docker host.\n"+
				"  The nvme_fabrics kernel module must be loaded BEFORE running e2e tests.\n"+
				"  Run: sudo modprobe nvme_fabrics nvme_tcp nvmet nvmet_tcp\n"+
				"  Output: %s", strings.TrimSpace(out))
	}
	fmt.Fprintf(os.Stdout, "e2e setup: /dev/nvme-fabrics verified on Docker host\n")
	return nil
}

// ensureKindCluster always creates a fresh Kind cluster.  Any pre-existing
// cluster with the same name is deleted first to guarantee a clean slate and
// prevent state leakage between runs.  On success testEnv.KubeconfigPath is
// populated and KUBECONFIG is exported.  The embedded KindConfigYAML (from
// testdata_embed.go) is written to a temporary file so Kind can read it via
// --config.
//
// # Why kind delete failure is fatal here (vs non-fatal in teardownE2E)
//
// In teardownE2E, a delete failure is logged but ignored because the cluster
// may genuinely not exist (setup never created it) and the CI system will
// start fresh on the next run.
//
// Here in setup, we are ABOUT to create a new cluster.  If the old cluster
// cannot be deleted we cannot guarantee a clean starting state: the new
// "create" would either fail (name collision) or succeed on top of stale
// state.  Either outcome violates the "always fresh" guarantee, so we
// return a hard error and abort setup entirely.
func ensureKindCluster() error {
	defer logStep("ensureKindCluster")()
	// Delete any pre-existing cluster with the same name to guarantee a clean
	// slate and prevent state leakage between runs.
	//
	// clusterExists() is used here to produce a clear log message and to avoid
	// an unconditional "kind delete" invocation that would always print a
	// "Deleting cluster ... (not found)" line even on first-ever runs.  If
	// ensureKindCluster were ever simplified to an unconditional delete
	// (treating not-found as success), clusterExists would become dead code and
	// should be removed at that time.
	existingClusters, _ := captureOutput("kind", "get", "clusters")
	if clusterExists(existingClusters, testEnv.ClusterName) {
		fmt.Fprintf(os.Stdout,
			"e2e setup: kind cluster %q already exists — deleting for a fresh start\n",
			testEnv.ClusterName)
		if err := runCmd("kind", "delete", "cluster",
			"--name", testEnv.ClusterName,
		); err != nil {
			// Fatal: we cannot proceed with a stale cluster.  See function
			// doc comment for the rationale.
			return fmt.Errorf("kind delete existing cluster: %w", err)
		}
	}

	// Build the Kind config, optionally appending kubeadmConfigPatches that
	// add the remote Docker daemon's IP to the API server certificate SANs.
	//
	// When Kind runs on a remote Docker daemon (e.g. tcp://192.168.1.100:2375),
	// the embedded kind-config.yaml sets apiServerAddress=0.0.0.0 so that the
	// API server port is reachable from outside the daemon host.  However, the
	// API server TLS certificate only includes 0.0.0.0 and internal IPs in its
	// SANs by default.  Adding the remote host's routable IP as a SAN allows
	// TLS verification to succeed when kubectl and client-go connect via that
	// IP.
	kindConfig := buildKindConfig()
	configFile, err := writeTempFile("kind-config-*.yaml", []byte(kindConfig))
	if err != nil {
		return fmt.Errorf("write kind config: %w", err)
	}
	defer os.Remove(configFile) //nolint:errcheck

	// Write kubeconfig to /tmp so Kind does NOT modify ~/.kube/config.
	kubeconfigFile, err := os.CreateTemp("", "kubeconfig-"+testEnv.ClusterName+"-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp kubeconfig: %w", err)
	}
	kubeconfigFile.Close() //nolint:errcheck
	kubeconfigPath := kubeconfigFile.Name()

	fmt.Fprintf(os.Stdout, "e2e setup: creating kind cluster %q\n", testEnv.ClusterName)
	if err := runCmd("kind", "create", "cluster",
		"--name", testEnv.ClusterName,
		"--config", configFile,
		"--kubeconfig", kubeconfigPath,
	); err != nil {
		os.Remove(kubeconfigPath) //nolint:errcheck
		return fmt.Errorf("kind create cluster: %w", err)
	}

	// When DOCKER_HOST points to a remote daemon, the kubeconfig contains
	// "server: https://127.0.0.1:<port>" which is unreachable from the local
	// machine.  Rewrite to the remote host IP.
	if remoteHost := dockerHostIP(testEnv.DockerHost); remoteHost != "" && remoteHost != "127.0.0.1" {
		raw, readErr := os.ReadFile(kubeconfigPath)
		if readErr == nil {
			patched := strings.ReplaceAll(string(raw), "https://127.0.0.1:", "https://"+remoteHost+":")
			patched = strings.ReplaceAll(patched, "https://0.0.0.0:", "https://"+remoteHost+":")
			_ = os.WriteFile(kubeconfigPath, []byte(patched), 0o600)
		}
	}

	testEnv.KubeconfigPath = kubeconfigPath
	if err := os.Setenv("KUBECONFIG", kubeconfigPath); err != nil {
		return fmt.Errorf("setenv KUBECONFIG: %w", err)
	}
	return nil
}

// buildKindConfig returns the Kind cluster configuration YAML to be passed to
// "kind create cluster --config".  It starts from the embedded KindConfigYAML
// and appends a kubeadmConfigPatches block that adds the remote Docker daemon
// host's routable IP to the API server certificate SANs when the daemon is
// remote (i.e. not 127.0.0.1).
//
// Without this patch, the API server certificate only includes internal cluster
// addresses and 0.0.0.0 in its SANs.  kubectl, helm, and client-go all verify
// the server certificate by default; they would reject the connection with
// "x509: certificate is not valid for <remote-host>" when connecting via the
// remote host IP.  Adding the IP as a SAN makes TLS verification succeed.
func buildKindConfig() string {
	base := string(KindConfigYAML)

	remoteHost := dockerHostIP(testEnv.DockerHost)
	if remoteHost == "" || remoteHost == "127.0.0.1" {
		// Local daemon or unknown endpoint — no extra SAN needed.
		return base
	}

	// Append kubeadmConfigPatches to add the remote host IP as a SAN.
	// The patch targets ClusterConfiguration (applied to the control-plane
	// node).  We include both the remote IP and the standard aliases so that
	// connections from any interface continue to work.
	patch := fmt.Sprintf(`
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      certSANs:
        - %q
        - "localhost"
        - "127.0.0.1"
        - "0.0.0.0"
`, remoteHost)

	return base + patch
}

// ensureStorageNodeLabel labels the first worker node as a storage node so that
// the agent DaemonSet (nodeSelector: pillar-csi.bhyoo.com/storage-node=true) is
// scheduled.  The Kind cluster is always freshly created, so this step ensures
// the label is present on every run (Kind applies node labels only during
// "kind create cluster", which we always invoke).
func ensureStorageNodeLabel() error {
	defer logStep("ensureStorageNodeLabel")()
	fmt.Fprintf(os.Stdout, "e2e setup: ensuring storage-node label on first worker\n")
	// Find the first worker node (not control-plane).
	out, err := captureOutput("kubectl", "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("find worker node: %s: %w", strings.TrimSpace(out), err)
	}
	workerNode := strings.TrimSpace(out)
	if workerNode == "" {
		return fmt.Errorf("no worker node found in cluster %q", testEnv.ClusterName)
	}
	return runCmd("kubectl", "label", "node", workerNode,
		"pillar-csi.bhyoo.com/storage-node=true", "--overwrite")
}

// validateWorkerNodeMounts checks that the Kind worker node containers have
// the filesystem paths that were specified via extraMounts in kind-config.yaml
// accessible.
//
// Checks performed:
//
//   - Storage worker (labelled pillar-csi.bhyoo.com/storage-node=true):
//     /sys/kernel/config is a readable directory — proves that the configfs
//     bind-mount from the host is in place.  Without it the pillar-agent init
//     container cannot write NVMe-oF target configuration and the protocol
//     tests would hang indefinitely waiting for a subsystem that never appears.
//
// Note: /dev is intentionally NOT bind-mounted from the host into Kind nodes.
// Mounting the host /dev with Bidirectional propagation causes the Kind worker
// container init to fail with "open /dev/console: input/output error" because
// the host console device is not accessible from within the container context.
// Real ZFS zvol and NVMe device access is provided through the DockerHostExec
// framework helper, which runs privileged commands directly on the remote host.
//
// These checks run before the slow Helm install so that a mount mis-
// configuration surfaces as a clear error message rather than a cryptic
// timeout later in the suite.
func validateWorkerNodeMounts() error {
	defer logStep("validateWorkerNodeMounts")()
	fmt.Fprintf(os.Stdout, "e2e setup: validating worker node mount accessibility\n")

	// ── /sys/kernel/config on the storage worker ──────────────────────────
	//
	// The storage worker is the one with label
	// pillar-csi.bhyoo.com/storage-node=true.  ensureStorageNodeLabel()
	// guarantees this label is set before validateWorkerNodeMounts is called.
	storageNodeOut, err := captureOutput("kubectl", "get", "nodes",
		"-l", "pillar-csi.bhyoo.com/storage-node=true",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("find storage worker node: %s: %w",
			strings.TrimSpace(storageNodeOut), err)
	}
	storageNode := strings.TrimSpace(storageNodeOut)
	if storageNode == "" {
		return fmt.Errorf(
			"validateWorkerNodeMounts: no storage worker node "+
				"(label pillar-csi.bhyoo.com/storage-node=true) found in cluster %q",
			testEnv.ClusterName)
	}

	if err := kindNodePathExists(storageNode, "/sys/kernel/config"); err != nil {
		return fmt.Errorf(
			"storage worker %q: /sys/kernel/config not accessible "+
				"(configfs bind-mount missing — check kind-config.yaml extraMounts): %w",
			storageNode, err)
	}
	fmt.Fprintf(os.Stdout,
		"e2e setup: storage worker %q /sys/kernel/config is accessible\n", storageNode)

	return nil
}

// kindNodePathExists runs `docker exec <container> test -e <path>` against a
// Kind node container on the remote Docker host.  It returns nil when the path
// exists (exit 0) or a descriptive error when the path is absent or the exec
// mechanism fails.
//
// captureOutput is used so that DOCKER_HOST is injected automatically and the
// error message contains any Docker output that helps diagnose the failure.
func kindNodePathExists(container, path string) error {
	out, err := captureOutput("docker", "exec", container, "test", "-e", path)
	if err != nil {
		return fmt.Errorf("docker exec %s test -e %s: %s: %w",
			container, path, strings.TrimSpace(out), err)
	}
	return nil
}

// thirdPartyImages delegates to the centralized registry in
// framework/images.go so that image versions are maintained in a single place.
//
// See framework.ThirdPartyImages for the authoritative list and rationale.
var thirdPartyImages = framework.ThirdPartyImages

// buildAndLoadImages builds the controller, agent, and node Docker images and
// loads each one into every node of the Kind cluster.  Images are tagged with
// the full registry paths used in the Helm chart (ghcr.io/bhyoo/pillar-csi/*)
// so that pods with imagePullPolicy: Never find the correct image name in the
// Kind node's container-image cache.
//
// After building pillar-csi images, it also pulls (if absent) and loads all
// third-party images (busybox init container, CSI sidecars) from thirdPartyImages
// so that no pod ever needs to pull from an external registry at runtime.
// This eliminates Docker Hub rate-limit failures (HTTP 429) on CI runners.
//
// Parallelism strategy:
//   - Phase 1: all 3 pillar-csi docker builds run concurrently (sync.WaitGroup + semaphore).
//   - Phase 2: all 3 kind loads run concurrently via framework.PullImages (after all builds succeed).
//   - Phase 3: all third-party pull+load pairs run concurrently via framework.PullImages.
//
// Phases 2 and 3 both use framework.PullImages, which provides bounded concurrency
// (≤ MaxPullConcurrency) and early-exit on first error via errgroup.  Images within
// each phase are order-independent: they share no state and can be loaded in any
// permutation, always yielding an identical final Kind node image cache.
//
// loadImageIntoKindNodes already parallelises across Kind nodes internally;
// the framework.PullImages call here parallelises across distinct images.
//
// Why not "kind load docker-image":
//
// "kind load docker-image" internally pipes the image tar through a
// "docker exec ... ctr images import -" call.  When the Docker daemon is
// remote (tcp://…), this streaming-over-exec mechanism is unreliable: the
// exec instance is created on the daemon and then the image data is piped
// over the same TCP connection.  Under load, the exec can disappear
// ("No such exec instance") or be SIGKILL-ed (exit 137) before the transfer
// completes.
//
// Instead, this function uses a file-based approach:
//  1. "docker save -o localTar image:tag" — downloads the image from the
//     remote daemon and writes it to a temporary file on the local machine.
//  2. "docker cp localTar node:/root/kind-image.tar" — uploads the local
//     tar file into each Kind node container via the Docker API's
//     file-copy endpoint, which is a complete HTTP transaction with no
//     interactive exec channel.
//  3. "docker exec node ctr images import /root/kind-image.tar" — imports
//     from the local file inside the container; no streaming required.
//  4. "docker exec node rm /root/kind-image.tar" — cleans up the tar.
//
// The extra round-trip (download then upload) costs bandwidth but eliminates
// the exec-streaming reliability problem.
func buildAndLoadImages() error {
	defer logStep("buildAndLoadImages (total)")()

	type imageSpec struct {
		dockerfile string
		name       string
	}
	images := []imageSpec{
		{"Dockerfile", "ghcr.io/bhyoo/pillar-csi/controller:" + testEnv.ImageTag},
		{"Dockerfile.agent", "ghcr.io/bhyoo/pillar-csi/agent:" + testEnv.ImageTag},
		{"Dockerfile.node", "ghcr.io/bhyoo/pillar-csi/node:" + testEnv.ImageTag},
	}

	// ── Phase 1: build all 3 pillar-csi images in parallel ────────────────
	//
	// A semaphore with capacity buildConcurrency (3) limits how many docker
	// build processes run simultaneously.  With exactly 3 images the semaphore
	// is acquired immediately by all goroutines, but the cap ensures the limit
	// is enforced if more images are added in the future.
	const buildConcurrency = 3
	buildSem := make(chan struct{}, buildConcurrency)
	buildAllDone := logStep("  docker build all images (parallel, max 3 concurrent)")
	var buildWg sync.WaitGroup
	buildErrCh := make(chan error, len(images))
	for _, img := range images {
		buildWg.Add(1)
		go func(img imageSpec) {
			defer buildWg.Done()
			buildSem <- struct{}{}        // acquire a build slot
			defer func() { <-buildSem }() // release on exit
			fmt.Fprintf(os.Stdout, "e2e setup: building image %s from %s\n", img.name, img.dockerfile)
			buildDone := logStep("  docker build " + img.dockerfile)
			err := runCmd("docker", "build", "-f", img.dockerfile, "-t", img.name, ".")
			buildDone()
			if err != nil {
				buildErrCh <- fmt.Errorf("docker build %s: %w", img.name, err)
			}
		}(img)
	}
	buildWg.Wait()
	close(buildErrCh)
	buildAllDone()
	for err := range buildErrCh {
		if err != nil {
			return err
		}
	}

	// ── Phase 2: load all 3 pillar-csi images into Kind in parallel ────────
	//
	// framework.PullImages provides bounded concurrency (≤ MaxPullConcurrency)
	// and early-exit error handling via errgroup.  Pillar-csi images are
	// already present in the Docker daemon (built in Phase 1), so fn only
	// loads — no docker pull step is needed here.
	//
	// Order-independence: the three images (controller, agent, node) are
	// independent artefacts that share no state.  Any permutation of the
	// three concurrent loads produces an identical Kind node image cache.
	// framework.PullImages makes this order-independence explicit: the fn
	// receives only the image name and performs a pure load with no
	// cross-image side-effects.
	loadAllDone := logStep(fmt.Sprintf("  kind load all images (parallel, max %d concurrent)", framework.MaxPullConcurrency))
	imageNames := make([]string, len(images))
	for i, img := range images {
		imageNames[i] = img.name
	}
	loadErr := framework.PullImages(context.Background(), imageNames,
		func(ctx context.Context, img string) error {
			fmt.Fprintf(os.Stdout, "e2e setup: loading image %s into kind cluster %s\n",
				img, testEnv.ClusterName)
			loadDone := logStep("  kind load " + img)
			err := loadImageIntoKindNodes(img)
			loadDone()
			if err != nil {
				return fmt.Errorf("load image %s into kind nodes: %w", img, err)
			}
			return nil
		},
	)
	loadAllDone()
	if loadErr != nil {
		return loadErr
	}

	// ── Phase 3: pull + load all third-party images in parallel ───────────
	//
	// framework.PullImages caps concurrency at maxPullConcurrency (6) using an
	// errgroup + buffered-channel semaphore.  This prevents Docker Hub
	// rate-limits (HTTP 429) and avoids saturating the CI network link with
	// unbounded simultaneous pulls.  Pull and load are sequential within each
	// image so we only load images that have been successfully pulled.
	tpAllDone := logStep(fmt.Sprintf("  3rd-party pull+load (parallel, max %d concurrent)", framework.MaxPullConcurrency))
	tpErr := framework.PullImages(context.Background(), thirdPartyImages,
		func(ctx context.Context, img string) error {
			fmt.Fprintf(os.Stdout, "e2e setup: pulling third-party image %s\n", img)
			pullDone := logStep("  docker pull " + img)
			if err := runCmd("docker", "pull", img); err != nil {
				// Non-fatal: log and continue.  If the image is already present in
				// the daemon cache the pull is a no-op and will succeed; if it fails
				// for an unrecoverable reason (e.g. network outage) the subsequent
				// loadImageIntoKindNodes call will also fail with a clear error.
				fmt.Fprintf(os.Stdout, "e2e setup: warning: docker pull %s: %v\n", img, err)
			}
			pullDone()

			fmt.Fprintf(os.Stdout, "e2e setup: loading third-party image %s into kind cluster %s\n",
				img, testEnv.ClusterName)
			loadDone := logStep("  kind load (3rd-party) " + img)
			err := loadImageIntoKindNodes(img)
			loadDone()
			if err != nil {
				return fmt.Errorf("load third-party image %s into kind nodes: %w", img, err)
			}
			return nil
		},
	)
	tpAllDone()
	if tpErr != nil {
		return tpErr
	}

	return nil
}

// loadImageIntoKindNodes loads a Docker image into all Kind cluster nodes
// using a file-based copy approach that is reliable over a remote TCP Docker
// daemon (unlike "kind load docker-image" which uses streaming exec).
//
// Steps:
//  1. Save the image to a local temporary tar file via "docker save -o".
//  2. For each Kind node: copy the tar into the container with "docker cp".
//  3. Import the tar inside the container with "ctr images import <file>".
//  4. Remove the tar from the container.
//  5. Remove the local temporary tar file.
//
// The on-node tar filename includes an image-derived slug so that multiple
// concurrent loadImageIntoKindNodes calls (one per image) do not collide on
// the same Kind node container.
func loadImageIntoKindNodes(imageName string) error {
	// Step 1 — Save image to a local tar file.
	localTar, err := os.CreateTemp("", "kind-image-*.tar")
	if err != nil {
		return fmt.Errorf("create temp tar: %w", err)
	}
	localTarPath := localTar.Name()
	localTar.Close()
	defer os.Remove(localTarPath) //nolint:errcheck

	if err := runCmd("docker", "save", imageName, "-o", localTarPath); err != nil {
		return fmt.Errorf("docker save %s: %w", imageName, err)
	}

	// Step 2 — List all Kind nodes using docker ps with the Kind cluster label.
	// We use docker ps rather than "kind get nodes" because the latter can
	// return non-empty output like "No kind nodes found for cluster..." when
	// it misidentifies the cluster state, leading to that message being
	// (incorrectly) treated as a container name.
	nodesOut, err := captureOutput("docker", "ps",
		"--filter", "label=io.x-k8s.kind.cluster="+testEnv.ClusterName,
		"--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("docker ps for kind nodes: %s: %w", strings.TrimSpace(nodesOut), err)
	}

	nodes := strings.Split(strings.TrimSpace(nodesOut), "\n")

	// Derive a safe filename slug from the image name so that concurrent calls
	// for different images do not overwrite each other's tar on the same node.
	// e.g. "ghcr.io/bhyoo/pillar-csi/controller:e2e" → "ghcr.io-bhyoo-pillar-csi-controller-e2e"
	safeImage := strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(imageName)

	// Load image into all Kind nodes in parallel.
	var wg sync.WaitGroup
	errCh := make(chan error, len(nodes))

	for _, rawNode := range nodes {
		node := strings.TrimSpace(rawNode)
		if node == "" {
			continue
		}
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			// Use /root/ instead of /tmp/: Kind node containers mount /tmp as
			// tmpfs, and docker cp silently fails to write into tmpfs paths when
			// the Docker daemon is remote.
			// Include safeImage in the name to avoid collisions across parallel
			// loadImageIntoKindNodes calls targeting the same node.
			nodeTar := "/root/kind-image-" + safeImage + "-" + node + ".tar"

			if err := runCmd("docker", "cp", localTarPath, node+":"+nodeTar); err != nil {
				errCh <- fmt.Errorf("docker cp to %s: %w", node, err)
				return
			}
			if err := runCmd("docker", "exec", node,
				"ctr", "--namespace=k8s.io", "images", "import",
				"--all-platforms", "--digests", "--snapshotter=overlayfs",
				nodeTar,
			); err != nil {
				errCh <- fmt.Errorf("ctr import on %s: %w", node, err)
				return
			}
			_ = runCmd("docker", "exec", node, "rm", nodeTar)
		}(node)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// installHelm installs (or upgrades) the pillar-csi Helm chart.
//
// The embedded HelmValuesYAML is always written to a temp file and passed via
// --values.  When ExternalAgentAddr is set, the HelmValuesExternalYAML overlay
// is written to a second temp file and appended as an additional --values flag
// so that the agent DaemonSet is effectively disabled.
//
// The actual image tag (testEnv.ImageTag) is forwarded via --set flags to
// ensure the chart references the exact images loaded into Kind — even when
// ImageTag differs from the hardcoded "e2e" default in the embedded YAML.
func installHelm() error {
	defer logStep("installHelm (total)")()

	valuesFile, err := writeTempFile("helm-values-*.yaml", HelmValuesYAML)
	if err != nil {
		return fmt.Errorf("write helm values: %w", err)
	}
	defer os.Remove(valuesFile) //nolint:errcheck

	fmt.Fprintf(os.Stdout, "e2e setup: installing helm chart %q in namespace %q\n",
		testEnv.HelmRelease, testEnv.HelmNamespace)

	args := []string{
		"upgrade", "--install",
		testEnv.HelmRelease, "./charts/pillar-csi",
		"--namespace", testEnv.HelmNamespace,
		"--create-namespace",
		"--values", valuesFile,
	}

	// When running in external-agent mode, overlay the external-agent values so
	// that the in-cluster agent DaemonSet is disabled (unmatchable nodeSelector).
	if testEnv.ExternalAgentAddr != "" {
		extValuesFile, err := writeTempFile("helm-values-ext-*.yaml", HelmValuesExternalYAML)
		if err != nil {
			return fmt.Errorf("write helm values external: %w", err)
		}
		defer os.Remove(extValuesFile) //nolint:errcheck
		args = append(args, "--values", extValuesFile)
	}

	// When the LVM VG was set up successfully, overlay the LVM values so that
	// the agent DaemonSet receives both --backend flags (ZFS + LVM) and the
	// DM_DISABLE_UDEV=1 environment variable.
	//
	// testEnv.lvmVGReady is true only when setupLVMVG succeeded; using it
	// as the gate avoids deploying the LVM overlay when the VG creation failed
	// or was skipped.
	if testEnv.lvmVGReady {
		lvmValuesFile, err := writeTempFile("helm-values-lvm-*.yaml", HelmValuesLVMYAML)
		if err != nil {
			return fmt.Errorf("write helm values lvm: %w", err)
		}
		defer os.Remove(lvmValuesFile) //nolint:errcheck
		args = append(args, "--values", lvmValuesFile)
		fmt.Fprintf(os.Stdout, "e2e setup: applying LVM helm values overlay (vg=%s, thinpool=%s)\n",
			testEnv.LVMVGName, testEnv.LVMThinPoolName)
	}

	// Override image tags via --set so the chart uses whatever tag TestMain
	// built and loaded into Kind, regardless of what the embedded YAML contains.
	args = append(args,
		"--set", "controller.image.tag="+testEnv.ImageTag,
		"--set", "agent.image.tag="+testEnv.ImageTag,
		"--set", "node.image.tag="+testEnv.ImageTag,
		"--wait",
		"--atomic",
		"--timeout", "5m",
	)

	helmDone := logStep("  helm upgrade --install")
	if err := runCmd("helm", args...); err != nil {
		helmDone()
		return err
	}
	helmDone()

	// Wait for the pillar-csi CRDs to be fully established in the API server.
	//
	// Helm's --wait flag ensures Deployment and DaemonSet pods are running but
	// does NOT guarantee that CustomResourceDefinition endpoints are registered
	// and ready for client discovery.  The controller-runtime REST mapper used
	// by e2e test clients builds its discovery cache right after Helm returns;
	// if the CRD REST endpoint is not yet live, the mapper returns "no matches
	// for kind" even though the CRD object exists in etcd.
	//
	// "kubectl wait --for=condition=established" blocks until the API server
	// has registered the REST endpoint, making subsequent client operations
	// reliable.  All five pillar-csi CRDs must be established before tests run.
	fmt.Fprintf(os.Stdout, "e2e setup: waiting for pillar-csi CRDs to be established\n")
	crdDone := logStep("  wait CRDs established")
	crdWaitErr := runCmd("kubectl", "wait",
		"--for=condition=established",
		"--timeout=60s",
		"crd/pillartargets.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarpools.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarprotocols.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarbindings.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarvolumes.pillar-csi.pillar-csi.bhyoo.com",
	)
	crdDone()
	if crdWaitErr != nil {
		return fmt.Errorf("wait for pillar-csi CRDs to be established: %w", crdWaitErr)
	}
	fmt.Fprintf(os.Stdout, "e2e setup: pillar-csi CRDs are established\n")

	// Wait for the pillar-csi API group to appear in the API server's discovery
	// endpoint.
	//
	// "kubectl wait --for=condition=established" only verifies the CRD status
	// condition, not the discovery endpoint.  The Kubernetes API server
	// refreshes its aggregated discovery document on a separate async cycle
	// (typically every 10-15 s after a CRD is installed).  If e2e test clients
	// query the discovery endpoint before this refresh, controller-runtime's
	// DynamicRESTMapper returns "no matches for kind" even though the CRD is
	// fully established.  This causes intermittent "apply PillarTarget: no
	// matches for kind" failures in Ginkgo BeforeAll blocks that run early when
	// the test randomisation seed orders them before the DaemonSet readiness
	// wait (which acts as an implicit synchronisation barrier).
	//
	// We poll "kubectl get pillartargets --all-namespaces" until kubectl no
	// longer returns "the server doesn't have a resource type 'pillartargets'"
	// (stale discovery) and instead returns either 0 or more resources (live
	// discovery).  A 60-second deadline is more than enough for the API server
	// to refresh its discovery document after CRD establishment.
	fmt.Fprintf(os.Stdout, "e2e setup: waiting for pillar-csi API group to appear in discovery\n")
	discDone := logStep("  wait API group discoverable")
	discoveryDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(discoveryDeadline) {
		out, _ := captureOutput("kubectl", "get",
			"pillartargets.pillar-csi.pillar-csi.bhyoo.com",
			"--all-namespaces",
		)
		if !strings.Contains(out, "doesn't have a resource type") {
			break // Discovery endpoint has been refreshed; API group is visible.
		}
		time.Sleep(2 * time.Second)
	}
	discDone()
	fmt.Fprintf(os.Stdout, "e2e setup: pillar-csi API group is discoverable\n")

	// Wait for the agent DaemonSet object to exist in the cluster.
	//
	// Helm --wait ensures deployment/daemonset pods are running before it exits,
	// but on rare occasions the Kubernetes API server's watch cache may not have
	// propagated the DaemonSet object to all watch consumers before the first
	// Ginkgo BeforeAll block runs.  Polling here closes that gap without coupling
	// test setup to an arbitrary sleep.
	//
	// Skip this wait when running in external-agent mode: the external-agent
	// overlay uses an unmatchable nodeSelector so DesiredNumberScheduled=0, and
	// a rollout-status wait would succeed immediately anyway.
	if testEnv.ExternalAgentAddr == "" {
		fmt.Fprintf(os.Stdout, "e2e setup: waiting for agent DaemonSet pillar-csi-agent to exist\n")
		dsDone := logStep("  wait agent DaemonSet exists")
		agentDSDeadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(agentDSDeadline) {
			out, err := captureOutput("kubectl", "get", "daemonset", "pillar-csi-agent",
				"--namespace", testEnv.HelmNamespace,
				"--ignore-not-found",
			)
			if err == nil && strings.Contains(out, "pillar-csi-agent") {
				break
			}
			fmt.Fprintf(os.Stdout, "e2e setup: agent DaemonSet not yet present, retrying...\n")
			time.Sleep(3 * time.Second)
		}
		dsDone()
		fmt.Fprintf(os.Stdout, "e2e setup: agent DaemonSet pillar-csi-agent is present\n")

		// Wait for the agent DaemonSet rollout to complete so that all test
		// specs that assert DaemonSet readiness start from a known-good state.
		fmt.Fprintf(os.Stdout, "e2e setup: waiting for agent DaemonSet rollout\n")
		rolloutDone := logStep("  wait agent DaemonSet rollout")
		if err := runCmd("kubectl", "rollout", "status",
			"daemonset/pillar-csi-agent",
			"--namespace", testEnv.HelmNamespace,
			"--timeout", "4m",
		); err != nil {
			fmt.Fprintf(os.Stderr,
				"e2e setup: WARNING: agent DaemonSet rollout did not complete: %v\n", err)
		}
		rolloutDone()
		fmt.Fprintf(os.Stdout, "e2e setup: agent DaemonSet rollout complete\n")
	}

	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Teardown
// ─────────────────────────────────────────────────────────────────────────────

// teardownE2E removes the Helm release, stops the external-agent container
// (if TestMain started one), and always deletes the Kind cluster.  All errors
// are logged to stderr but do not affect the exit code — that was already
// captured by the m.Run() call in TestMain.
//
// Cluster deletion is unconditional: even if testEnv.ClusterName is somehow
// empty (e.g. initE2EEnv was interrupted before setting it), teardownE2E falls
// back to defaultClusterName so that no cluster is ever left behind.  This
// hard guarantee prevents state leakage between runs regardless of how or when
// the process exits.
//
// All steps are best-effort: errors are logged but do not abort subsequent
// steps.  The only exception to the "best-effort" rule is the final cluster
// deletion, which runs unconditionally even after Helm or container failures.
func teardownE2E() {
	defer logStep("teardownE2E (total)")()

	if os.Getenv("E2E_SKIP_TEARDOWN") == "true" {
		fmt.Fprintf(os.Stdout, "e2e teardown: SKIPPED (E2E_SKIP_TEARDOWN=true) — cluster %q left running for debugging\n", testEnv.ClusterName)
		return
	}
	if testEnv.HelmRelease != "" {
		fmt.Fprintf(os.Stdout, "e2e teardown: uninstalling helm release %q\n",
			testEnv.HelmRelease)
		helmDone := logStep("  helm uninstall")
		if err := runCmd("helm", "uninstall", testEnv.HelmRelease,
			"--namespace", testEnv.HelmNamespace,
			"--ignore-not-found",
		); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: helm uninstall: %v\n", err)
		}
		helmDone()
	}

	// Stop and remove the external-agent container started by TestMain.
	stopExternalAgentContainer()

	// Destroy the loopback ZFS pool and release the DockerHostExec helper.
	teardownZFSPool()

	// Destroy the loopback LVM VG inside the Kind storage worker container.
	// Must run before the Kind cluster is deleted (LVM state lives inside the
	// worker container, not on the Docker host).
	teardownLVMVG()

	// Always delete the Kind cluster — unconditionally, with no guard.
	//
	// If testEnv.ClusterName is empty (initE2EEnv was interrupted before the
	// first envOrDefault call), fall back to defaultClusterName so we still
	// attempt to clean up.  This fallback is a safety net: in normal operation
	// ClusterName is always non-empty because initE2EEnv sets it on its very
	// first line.
	//
	// Errors here are non-fatal (logged and ignored) for two reasons:
	//  1. The cluster may never have been created (e.g. setup failed early).
	//  2. Subsequent CI runs will call ensureKindCluster() which checks for
	//     and deletes any pre-existing cluster — a missed teardown is thus
	//     corrected at the next setup.
	clusterName := testEnv.ClusterName
	if clusterName == "" {
		clusterName = defaultClusterName
	}
	fmt.Fprintf(os.Stdout, "e2e teardown: deleting kind cluster %q (unconditional)\n",
		clusterName)
	kindDone := logStep("  kind delete cluster")
	if err := runCmd("kind", "delete", "cluster",
		"--name", clusterName,
	); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: kind delete cluster: %v\n", err)
	}
	kindDone()

	// Remove the temporary kubeconfig written by writeKindKubeconfig.
	if testEnv.KubeconfigPath != "" {
		_ = os.Remove(testEnv.KubeconfigPath)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ZFS loopback pool lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// setupZFSPool creates a loopback-backed ZFS pool on the remote Docker host.
//
// It starts a privileged DockerHostExec helper container (stored in
// testEnv.zfsHostExec) and calls framework.CreateLoopbackZFSPool to:
//
//  1. Create a sparse image file of testEnv.ZFSImageSize at testEnv.ZFSImagePath.
//  2. Attach the image to a free loop device via losetup.
//  3. Create the ZFS pool named testEnv.ZFSPoolName on that loop device.
//
// The resulting loop device path is stored in testEnv.zfsLoopDev so that
// teardownZFSPool can pass it to DestroyLoopbackZFSPool.
//
// On error the function returns a descriptive message that TestMain prints to
// stderr before calling os.Exit(1).  Partial resources (loop device, image
// file) are cleaned up inside CreateLoopbackZFSPool via its internal deferred
// guards, so no additional cleanup is required here on failure.
func setupZFSPool() error {
	defer logStep("setupZFSPool")()
	ctx := context.Background()

	fmt.Fprintf(os.Stdout,
		"e2e setup: starting privileged host-exec container on %s\n",
		testEnv.DockerHost)
	h, err := framework.NewDockerHostExec(ctx, testEnv.DockerHost)
	if err != nil {
		return fmt.Errorf(
			"start host-exec container on %s: %w\n"+
				"  (ZFS pool %q cannot be created without a privileged exec helper)",
			testEnv.DockerHost, err, testEnv.ZFSPoolName)
	}
	// Store immediately so teardownZFSPool can close h even when subsequent
	// steps fail.
	testEnv.zfsHostExec = h

	fmt.Fprintf(os.Stdout,
		"e2e setup: creating ZFS pool %q on %s (image %s, size %s)\n",
		testEnv.ZFSPoolName, testEnv.DockerHost,
		testEnv.ZFSImagePath, testEnv.ZFSImageSize)
	loopDev, err := framework.CreateLoopbackZFSPool(ctx, h,
		testEnv.ZFSPoolName, testEnv.ZFSImagePath, testEnv.ZFSImageSize)
	if err != nil {
		return fmt.Errorf(
			"create loopback ZFS pool %q (image %s, size %s): %w\n"+
				"  Check that the ZFS kernel module is loaded on the remote host and that\n"+
				"  the remote Docker daemon at %s is reachable.",
			testEnv.ZFSPoolName, testEnv.ZFSImagePath, testEnv.ZFSImageSize, err,
			testEnv.DockerHost)
	}
	testEnv.zfsLoopDev = loopDev

	fmt.Fprintf(os.Stdout,
		"e2e setup: ZFS pool %q ready (loop device %s, image %s)\n",
		testEnv.ZFSPoolName, loopDev, testEnv.ZFSImagePath)
	return nil
}

// teardownZFSPool destroys the loopback ZFS pool created by setupZFSPool and
// closes the DockerHostExec helper container.
//
// All errors are logged to stderr but do not abort teardown — subsequent steps
// (Kind cluster deletion, kubeconfig removal) must still run even when pool
// destruction fails.  This matches the best-effort contract of teardownE2E.
//
// teardownZFSPool is a no-op when testEnv.zfsHostExec is nil (i.e. when
// setupZFSPool was never called or failed before allocating the helper).
func teardownZFSPool() {
	h := testEnv.zfsHostExec
	if h == nil {
		return
	}
	defer logStep("teardownZFSPool")()

	ctx := context.Background()

	fmt.Fprintf(os.Stdout,
		"e2e teardown: destroying ZFS pool %q (loop %s, image %s)\n",
		testEnv.ZFSPoolName, testEnv.zfsLoopDev, testEnv.ZFSImagePath)
	if err := framework.DestroyLoopbackZFSPool(ctx, h,
		testEnv.ZFSPoolName, testEnv.zfsLoopDev, testEnv.ZFSImagePath); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: destroy ZFS pool %q: %v\n",
			testEnv.ZFSPoolName, err)
	}

	if err := h.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: close host-exec container: %v\n", err)
	}
	testEnv.zfsHostExec = nil
}

// ─────────────────────────────────────────────────────────────────────────────
// LVM volume group lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// setupLVMVG creates a loopback-backed LVM volume group (and thin pool) inside
// the Kind storage worker container.
//
// It uses docker exec to run a bash script that:
//  1. Installs lvm2 in the Kind worker container (if not already present).
//  2. Loads the dm_thin_pool kernel module (best-effort).
//  3. Creates a sparse loopback image, PV, VG, and thin pool.
//
// On success, testEnv.LVMStorageNode is populated with the worker container
// name and testEnv.lvmVGReady is set to true.
//
// On error the function logs to stderr and returns a non-nil error.  The
// caller (the parallel setup goroutine in setupE2E) treats LVM failures as
// non-fatal: LVM tests are skipped but ZFS tests continue.
func setupLVMVG() error {
	defer logStep("setupLVMVG")()

	// Find the storage worker node name.
	storageNodeOut, err := captureOutput("kubectl", "get", "nodes",
		"-l", "pillar-csi.bhyoo.com/storage-node=true",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("find storage worker node: %s: %w", strings.TrimSpace(storageNodeOut), err)
	}
	storageNode := strings.TrimSpace(storageNodeOut)
	if storageNode == "" {
		return fmt.Errorf("no storage worker node (label pillar-csi.bhyoo.com/storage-node=true) found")
	}
	testEnv.LVMStorageNode = storageNode

	fmt.Fprintf(os.Stdout,
		"e2e setup: creating LVM VG %q in Kind worker %q (image %s, size %s)\n",
		testEnv.LVMVGName, storageNode, testEnv.LVMImagePath, testEnv.LVMImageSize)

	if err := framework.SetupLoopbackLVMVG(
		context.Background(),
		testEnv.DockerHost,
		storageNode,
		testEnv.LVMVGName,
		testEnv.LVMThinPoolName,
		testEnv.LVMImagePath,
		testEnv.LVMImageSize,
	); err != nil {
		return fmt.Errorf(
			"create loopback LVM VG %q in Kind worker %q: %w\n"+
				"  Check that the Kind worker container is running and reachable.\n"+
				"  Check that apt-get can download lvm2 packages (network access required).",
			testEnv.LVMVGName, storageNode, err)
	}

	// Create a privileged DockerHostExec for the storage worker container so
	// that LVM e2e tests can run commands on the host (e.g. inspect device
	// nodes, check NVMe block devices).  This mirrors setupZFSPool's pattern.
	lvmH, hErr := framework.NewDockerHostExec(context.Background(), testEnv.DockerHost)
	if hErr != nil {
		fmt.Fprintf(os.Stderr,
			"e2e setup: warning: could not create LVM host-exec helper: %v\n", hErr)
		// Non-fatal: core LVM tests still run; only host-exec-dependent tests skip.
	} else {
		testEnv.lvmHostExec = lvmH
	}

	testEnv.lvmVGReady = true
	fmt.Fprintf(os.Stdout,
		"e2e setup: LVM VG %q ready in Kind worker %q (image %s)\n",
		testEnv.LVMVGName, storageNode, testEnv.LVMImagePath)
	return nil
}

// teardownLVMVG destroys the loopback LVM VG created by setupLVMVG inside the
// Kind storage worker container.
//
// All errors are logged to stderr but do not abort teardown — subsequent steps
// (ZFS teardown, Kind cluster deletion) must still run.  This matches the
// best-effort contract of teardownE2E.
//
// teardownLVMVG is a no-op when lvmVGReady is false (setupLVMVG was never
// called or failed before creating the VG).
func teardownLVMVG() {
	if !testEnv.lvmVGReady {
		return
	}
	defer logStep("teardownLVMVG")()

	ctx := context.Background()

	fmt.Fprintf(os.Stdout,
		"e2e teardown: destroying LVM VG %q in Kind worker %q (image %s)\n",
		testEnv.LVMVGName, testEnv.LVMStorageNode, testEnv.LVMImagePath)
	if err := framework.TeardownLoopbackLVMVG(ctx,
		testEnv.DockerHost,
		testEnv.LVMStorageNode,
		testEnv.LVMVGName,
		testEnv.LVMImagePath,
	); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: destroy LVM VG %q: %v\n",
			testEnv.LVMVGName, err)
	}
	testEnv.lvmVGReady = false

	// Close the privileged host-exec helper if it was created.
	if testEnv.lvmHostExec != nil {
		if err := testEnv.lvmHostExec.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: close LVM host-exec container: %v\n", err)
		}
		testEnv.lvmHostExec = nil
	}
}

// getStorageNodeContainerName returns the Docker container name of the Kind
// storage-node (the first node labelled pillar-csi.bhyoo.com/storage-node=true).
//
// In Kind, the Kubernetes node name IS the Docker container name (e.g.
// "pillar-csi-e2e-worker").  We use kubectl to look up the label and derive
// the container name from the node name.
func getStorageNodeContainerName() (string, error) {
	out, err := captureOutput("kubectl", "get", "nodes",
		"-l", "pillar-csi.bhyoo.com/storage-node=true",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", fmt.Errorf("find storage worker node: %s: %w",
			strings.TrimSpace(out), err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf(
			"no node with label pillar-csi.bhyoo.com/storage-node=true found "+
				"in cluster %q", testEnv.ClusterName)
	}
	return name, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// waitForAPIServer polls "kubectl version" until the Kubernetes API server
// responds successfully, or until the given deadline is exceeded.
//
// Loading Docker images into kind nodes can temporarily spike memory and cause
// the kube-apiserver to restart.  Calling waitForAPIServer before running
// Helm guarantees that the API is reachable before Helm sends API requests.
func waitForAPIServer(timeout time.Duration) error {
	defer logStep("waitForAPIServer")()
	deadline := time.Now().Add(timeout)
	fmt.Fprintf(os.Stdout, "e2e setup: waiting up to %s for Kubernetes API server to be ready\n",
		timeout)
	for time.Now().Before(deadline) {
		out, err := captureOutput("kubectl", "version", "--output=json")
		if err == nil && strings.Contains(out, "serverVersion") {
			fmt.Fprintf(os.Stdout, "e2e setup: Kubernetes API server is ready\n")
			return nil
		}
		fmt.Fprintf(os.Stdout, "e2e setup: API server not ready yet, retrying in 3s...\n")
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("API server did not become ready within %s", timeout)
}

// runCmd executes name with args in the project root, streaming stdout and
// stderr to the process outputs.  It returns a non-nil error when the command
// exits non-zero.
//
// DOCKER_HOST is explicitly injected into the child-process environment (using
// injectDockerHost) so that docker, kind, and helm sub-commands always reach
// the correct Docker daemon endpoint, regardless of whether the caller
// exported DOCKER_HOST in their shell.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = e2eProjectRoot()
	cmd.Env = injectDockerHost(os.Environ())
	return cmd.Run()
}

// captureOutput is like runCmd but captures and returns combined output
// instead of streaming it.  Errors are returned alongside partial output.
//
// DOCKER_HOST is explicitly injected into the child-process environment via
// injectDockerHost for the same reason described on runCmd.
func captureOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec
	cmd.Dir = e2eProjectRoot()
	cmd.Env = injectDockerHost(os.Environ())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// injectDockerHost returns a copy of env with DOCKER_HOST set to the value
// stored in testEnv.DockerHost.  When DockerHost is empty, existing
// DOCKER_HOST entries are stripped so that sub-processes use Docker's default
// behaviour (local Unix socket).
func injectDockerHost(env []string) []string {
	const key = "DOCKER_HOST="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key) {
			out = append(out, e)
		}
	}
	if testEnv.DockerHost != "" {
		out = append(out, key+testEnv.DockerHost)
	}
	return out
}

// writeKindKubeconfig runs "kind get kubeconfig --name <cluster>", rewrites
// the API server address to point at the remote Docker host (instead of
// 127.0.0.1 which only works on the Docker daemon host), and writes the result
// to a new temporary file.
//
// When Kind runs on a remote Docker daemon (e.g. tcp://192.168.1.100:2375),
// the kubeconfig it emits contains "server: https://127.0.0.1:<port>".  That
// loopback address is on the remote machine, not on the local machine where
// the test binary runs.  Replacing 127.0.0.1 with the remote host's IP makes
// kubectl and client-go connect to the correct API server endpoint.
func writeKindKubeconfig(clusterName string) (string, error) {
	raw, err := captureOutput("kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return "", fmt.Errorf("kind get kubeconfig: %s: %w", raw, err)
	}

	// When running against a remote Docker daemon, the Kind API server may
	// be bound to 0.0.0.0 (all interfaces) or 127.0.0.1 (loopback only).
	// Either way, the kubeconfig "server:" field needs to reference the
	// actual routable IP of the remote Docker daemon host so that kubectl,
	// helm, and client-go running locally can reach the API server.
	if remoteHost := dockerHostIP(testEnv.DockerHost); remoteHost != "" && remoteHost != "127.0.0.1" {
		raw = strings.ReplaceAll(raw, "https://127.0.0.1:", "https://"+remoteHost+":")
		raw = strings.ReplaceAll(raw, "https://0.0.0.0:", "https://"+remoteHost+":")
	}

	f, err := os.CreateTemp("", "kubeconfig-"+clusterName+"-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp kubeconfig: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.WriteString(raw); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write kubeconfig: %w", err)
	}
	return f.Name(), nil
}

// dockerHostIP extracts the IP address portion from a Docker daemon endpoint
// URL such as "tcp://192.168.1.100:2375".  Returns an empty string when the
// endpoint is not a TCP URL or when parsing fails.
func dockerHostIP(dockerHost string) string {
	if dockerHost == "" {
		return ""
	}
	// Strip the scheme prefix (e.g. "tcp://").
	const tcpPrefix = "tcp://"
	if !strings.HasPrefix(dockerHost, tcpPrefix) {
		return ""
	}
	hostPort := strings.TrimPrefix(dockerHost, tcpPrefix)
	// Split off the port; net.SplitHostPort handles IPv6 addresses too.
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	return h
}

// writeTempFile writes data to a new temporary file whose name matches pattern
// and returns its absolute path.  The caller is responsible for removing the
// file when it is no longer needed.
func writeTempFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file %q: %w", pattern, err)
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.Write(data); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write temp file %q: %w", pattern, err)
	}
	return f.Name(), nil
}

// clusterExists reports whether clusterName appears as a full line in the
// whitespace-trimmed output of "kind get clusters".
//
// It is currently used by ensureKindCluster() to decide whether to log
// "already exists — deleting for a fresh start" before the unconditional
// setup-time delete.  If ensureKindCluster() is ever simplified to always
// call "kind delete cluster" without checking first (treating not-found as
// success), clusterExists becomes dead code and should be removed at that
// time.
func clusterExists(output, clusterName string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// External agent Docker container lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// externalAgentContainerName returns the Docker container name used for
// docker-exec calls into the external agent container.
//
// Resolution order:
//  1. E2E_EXTERNAL_AGENT_CONTAINER_NAME env var — explicit override, used when
//     pointing at a pre-existing agent container whose name is known.
//  2. Derived from the Kind cluster name ("<KIND_CLUSTER>-agent") — the default
//     when E2E_LAUNCH_EXTERNAL_AGENT=true and TestMain started the container.
//
// Reading the env var directly (rather than testEnv.ExternalAgentContainerName)
// makes this safe to call from both init-time registration guards and from
// BeforeAll/It closures that run after TestMain has populated testEnv.
func externalAgentContainerName() string {
	if name := os.Getenv("E2E_EXTERNAL_AGENT_CONTAINER_NAME"); name != "" {
		return name
	}
	return testEnv.ClusterName + "-agent"
}

// externalAgentImageRef returns the fully-qualified Docker image reference for
// the agent image that was built and loaded into Kind by buildAndLoadImages.
func externalAgentImageRef() string {
	return "ghcr.io/bhyoo/pillar-csi/agent:" + testEnv.ImageTag
}

// startExternalAgentContainer starts a Docker container that runs the
// pillar-agent gRPC server out-of-cluster.  The container is attached to the
// "kind" Docker network so that pods inside the Kind cluster can reach it via
// its container IP.  The agent's internal port 9500 is also mapped to
// 127.0.0.1:<ExternalAgentPort> on the host so that the test process can dial
// it directly for gRPC smoke tests.
//
// After the container is started this function:
//
//  1. Retrieves the container's IP on the "kind" Docker network.
//  2. Sets testEnv.ExternalAgentAddr to "127.0.0.1:<hostPort>" (host-dialable).
//  3. Sets EXTERNAL_AGENT_CLUSTER_ADDRESS to "<kindNetworkIP>:9500" so that
//     existing tests reading that env var receive a cluster-reachable address.
//  4. Polls the host-mapped port via TCP until the gRPC listener is ready or
//     ExternalAgentReadyTimeout elapses.
//
// On error all resources allocated so far are left for stopExternalAgentContainer
// to clean up via testEnv.externalAgentContainerID (set on first success).
func startExternalAgentContainer() error {
	defer logStep("startExternalAgentContainer")()
	name := externalAgentContainerName()
	image := externalAgentImageRef()
	hostAddr := net.JoinHostPort("127.0.0.1", testEnv.ExternalAgentPort)
	portMapping := "127.0.0.1:" + testEnv.ExternalAgentPort + ":9500"

	// Remove any leftover container from a previous interrupted run.
	// Ignore errors: the container may not exist.
	_ = exec.Command("docker", "rm", "-f", name).Run() //nolint:gosec

	fmt.Fprintf(os.Stdout,
		"e2e setup: starting external agent container %q (image %s, host port %s)\n",
		name, image, testEnv.ExternalAgentPort)

	// Run the agent container:
	//   --network kind   → reachable from Kind nodes (same Docker bridge)
	//   -p ...           → host-mapped port for test-process gRPC probes
	//   --privileged     → CAP_SYS_ADMIN + all host devices: required for
	//                      real ZFS zvol creation (ioctl on /dev/zfs) and
	//                      NVMe-oF configfs writes (/sys/kernel/config).
	//                      Without this flag, 'zfs create' inside the container
	//                      fails with EPERM because /dev/zfs is inaccessible.
	//   --user=root      → override Dockerfile.agent's USER 65532; ZFS ioctl
	//                      checks UID 0 in addition to CAP_SYS_ADMIN on some
	//                      kernel/ZFS version combinations.
	//   --mount tmpfs    → writable /tmp so --configfs-root=/tmp works without
	//                      needing kernel nvmet modules
	//   -v /sys/kernel/config:/sys/kernel/config
	//                    → expose the host configfs inside the container so the
	//                      e2e ExternalAgentZFSMount test can write real NVMe-oF
	//                      target entries (nvmet is either built-in or already
	//                      loaded on the host).  Without this bind-mount the
	//                      container sees a private (empty) configfs and
	//                      "/sys/kernel/config/nvmet" would not exist.
	//                      The agent itself uses --configfs-root=/tmp and never
	//                      touches /sys/kernel/config, so there is no conflict.
	out, err := captureOutput("docker", "run",
		"--detach",
		"--name", name,
		"--network", "kind",
		"-p", portMapping,
		"--privileged",
		"--user=root",
		"--mount", "type=tmpfs,destination=/tmp",
		"-v", "/sys/kernel/config:/sys/kernel/config",
		image,
		"--listen-address=0.0.0.0:9500",
		"--backend=type=zfs-zvol,pool="+testEnv.ZFSPoolName,
		"--configfs-root=/tmp",
	)
	if err != nil {
		return fmt.Errorf("docker run agent container: %s: %w", strings.TrimSpace(out), err)
	}
	// out is the container ID (full 64-char SHA).
	testEnv.externalAgentContainerID = strings.TrimSpace(out)
	fmt.Fprintf(os.Stdout,
		"e2e setup: external agent container started (id %.12s)\n",
		testEnv.externalAgentContainerID)

	// Retrieve the container's IP on the "kind" network so we can tell the
	// in-cluster controller how to reach the agent.
	kindIP, err := dockerContainerNetworkIP(testEnv.externalAgentContainerID, "kind")
	if err != nil {
		return fmt.Errorf("inspect agent container IP on 'kind' network: %w", err)
	}
	clusterAddr := net.JoinHostPort(kindIP, "9500")
	fmt.Fprintf(os.Stdout,
		"e2e setup: external agent cluster address: %s  host address: %s\n",
		clusterAddr, hostAddr)

	// Populate shared state and environment so both TestMain-managed setup and
	// individual test specs can discover the agent's addresses.
	testEnv.ExternalAgentAddr = hostAddr
	if err := os.Setenv("EXTERNAL_AGENT_ADDR", hostAddr); err != nil {
		return fmt.Errorf("setenv EXTERNAL_AGENT_ADDR: %w", err)
	}
	if err := os.Setenv("EXTERNAL_AGENT_CLUSTER_ADDRESS", clusterAddr); err != nil {
		return fmt.Errorf("setenv EXTERNAL_AGENT_CLUSTER_ADDRESS: %w", err)
	}

	// Poll the host-mapped port until the gRPC TCP listener is accepting
	// connections or until ExternalAgentReadyTimeout elapses.
	fmt.Fprintf(os.Stdout,
		"e2e setup: waiting up to %s for external agent on %s\n",
		testEnv.ExternalAgentReadyTimeout, hostAddr)
	if err := pollAgentReady(hostAddr, testEnv.ExternalAgentReadyTimeout); err != nil {
		return fmt.Errorf("external agent did not become ready: %w", err)
	}
	fmt.Fprintf(os.Stdout, "e2e setup: external agent is ready at %s\n", hostAddr)
	return nil
}

// stopExternalAgentContainer stops and removes the Docker container started by
// startExternalAgentContainer.  It is a no-op when no container was started.
// All errors are logged to stderr but do not propagate — teardown must be
// best-effort so that subsequent cleanup steps (kind delete cluster) still run.
func stopExternalAgentContainer() {
	id := testEnv.externalAgentContainerID
	if id == "" {
		return
	}
	defer logStep("stopExternalAgentContainer")()

	fmt.Fprintf(os.Stdout, "e2e teardown: stopping external agent container (id %.12s)\n", id)

	// Graceful stop with a 10-second timeout, then force-remove.
	if err := runCmd("docker", "stop", "--time=10", id); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: docker stop %s: %v\n", id[:12], err)
	}
	if err := runCmd("docker", "rm", "--force", id); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: docker rm %s: %v\n", id[:12], err)
	}

	testEnv.externalAgentContainerID = ""
}

// dockerContainerNetworkIP inspects a running container and returns its IP
// address on the named Docker network.  It runs:
//
//	docker inspect <id> --format '{{.NetworkSettings.Networks.<network>.IPAddress}}'
func dockerContainerNetworkIP(containerID, network string) (string, error) {
	format := "{{.NetworkSettings.Networks." + network + ".IPAddress}}"
	out, err := captureOutput("docker", "inspect", containerID, "--format", format)
	if err != nil {
		return "", fmt.Errorf("docker inspect: %s: %w", strings.TrimSpace(out), err)
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("container %s has no IP on network %q (is the container running?)",
			containerID[:12], network)
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("docker inspect returned unexpected value %q for container %s on network %q",
			ip, containerID[:12], network)
	}
	return ip, nil
}

// pollAgentReady dials addr over TCP every 500 ms until the connection
// succeeds or deadline is reached.  A successful TCP connection indicates that
// the gRPC server is accepting requests (the server calls net.Listen before
// grpc.Serve, so an open port means the listener is up).
func pollAgentReady(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tcp probe %s: last error: %w", addr, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Misc helpers
// ─────────────────────────────────────────────────────────────────────────────

// e2eProjectRoot returns the repository root directory by walking up from the
// current working directory until a go.mod file is found.  This works
// regardless of where `go test` is invoked from (project root, test/e2e/,
// or via `make -C`).
func e2eProjectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir || parent == "" {
			// Reached filesystem root without finding go.mod; fall back to cwd.
			return wd
		}
		dir = parent
	}
}

// envOrDefault returns the value of env var key, or defaultValue when the
// variable is unset or empty.  This is the single authoritative definition
// used by all e2e test files in this package (internal_agent_test.go,
// external_agent_test.go, coexecution_test.go, etc.) so that there is no
// duplication.
func envOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// logStep logs that a named setup/teardown step has started and returns a
// function that logs how long it took.  Intended for use as:
//
//	defer logStep("myStep")()
//
// in setup and teardown functions so that every exit path (normal return,
// early return on error, panic+recover) records the elapsed wall-clock time.
// Output is prefixed with "e2e timing:" so it is easy to grep out of verbose
// test logs to identify bottlenecks.
func logStep(name string) func() {
	start := time.Now()
	fmt.Fprintf(os.Stdout, "e2e timing: %-44s  [start]\n", name)
	return func() {
		fmt.Fprintf(os.Stdout, "e2e timing: %-44s  %.1fs\n", name, time.Since(start).Seconds())
	}
}

// printTimingReport prints a structured timing summary at the end of a
// TestMain run.  It is called from the deferred closure in TestMain so it
// always executes regardless of whether setup, tests, or teardown failed.
//
// Output uses the same "e2e timing:" prefix as logStep so the full timing
// profile (individual steps + summary) can be extracted from any log stream
// with:
//
//	grep "^e2e timing:" <log>
//
// Baseline measurements (DOCKER_HOST=tcp://localhost:2375, 2026-03-28):
//
//	internal-agent  setup ≈ 240s  tests ≈ 205s  teardown ≈ 30s  total ≈ 475s
//	external-agent  setup ≈ 130s  tests ≈  35s  teardown ≈ 20s  total ≈ 185s
func printTimingReport(suiteStart time.Time, setup, tests, teardown time.Duration, exitCode int) {
	total := time.Since(suiteStart)
	mode := "internal-agent"
	if isExternalAgentMode() {
		mode = "external-agent"
	}
	status := "PASS"
	if exitCode != 0 {
		status = "FAIL"
	}
	sep := "e2e timing: " + strings.Repeat("─", 46)
	fmt.Printf("\n")
	fmt.Printf("e2e timing: %s\n", strings.Repeat("━", 46))
	fmt.Printf("e2e timing:  E2E TIMING METRICS REPORT\n")
	fmt.Printf("%s\n", sep)
	fmt.Printf("e2e timing:  mode     %-37s\n", mode)
	fmt.Printf("e2e timing:  status   %-37s\n", status)
	fmt.Printf("%s\n", sep)
	fmt.Printf("e2e timing:  setup    %34.1fs\n", setup.Seconds())
	fmt.Printf("e2e timing:  tests    %34.1fs\n", tests.Seconds())
	fmt.Printf("e2e timing:  teardown %34.1fs\n", teardown.Seconds())
	fmt.Printf("%s\n", sep)
	fmt.Printf("e2e timing:  total    %34.1fs\n", total.Seconds())
	fmt.Printf("e2e timing: %s\n", strings.Repeat("━", 46))
	fmt.Printf("\n")
}

// isExternalAgentMode reports whether the test binary was invoked in
// external-agent mode.  The determination is made by inspecting environment
// variables that are set BEFORE the test binary starts:
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true  →  TestMain will start an agent container.
//   - EXTERNAL_AGENT_ADDR=<host:port> →  A pre-existing agent is already running.
//
// This function is called at package init() time (before TestMain runs) to
// decide which Ginkgo spec containers to register.  Specs registered only in
// the matching mode are never "skipped" — they simply do not exist in the
// Ginkgo tree for the other mode, keeping the skip count at zero.
func isExternalAgentMode() bool {
	return os.Getenv("E2E_LAUNCH_EXTERNAL_AGENT") == "true" ||
		os.Getenv("EXTERNAL_AGENT_ADDR") != ""
}
