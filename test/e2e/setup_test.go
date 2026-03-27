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
	// DOCKER_HOST; defaults to "tcp://localhost:2375".  Injected explicitly
	// into every exec.Command env so sub-processes use the correct daemon even
	// when the caller did not export DOCKER_HOST.
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

	// ExternalAgentZFSPool is the --zfs-pool flag passed to the agent container.
	// Sourced from E2E_EXTERNAL_AGENT_ZFS_POOL; default: "e2e-pool".
	ExternalAgentZFSPool string

	// ExternalAgentReadyTimeout is how long TestMain waits for the agent
	// container's gRPC port to become reachable before giving up.
	// Sourced from E2E_EXTERNAL_AGENT_READY_TIMEOUT in seconds; default: 60 s.
	ExternalAgentReadyTimeout time.Duration

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
}

// testEnv is the single, shared E2EEnv populated by TestMain.  All e2e test
// files should read cluster coordinates from this variable rather than
// re-querying the environment.
var testEnv = &E2EEnv{}

// defaultClusterName is used when KIND_CLUSTER is not set.
const defaultClusterName = "pillar-csi-e2e"

// defaultDockerHost is the Docker daemon endpoint injected into all
// docker/kind/helm sub-processes when DOCKER_HOST is not set in the
// calling environment.
const defaultDockerHost = "tcp://localhost:2375"

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
	exitCode := 1 // default to failure; m.Run() will overwrite on success

	defer func() {
		teardownE2E()
		os.Exit(exitCode)
	}()

	if err := initE2EEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: init env: %v\n", err)
		return // deferred teardown + os.Exit(1)
	}

	if err := setupE2E(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: setup: %v\n", err)
		return // deferred teardown + os.Exit(1)
	}

	exitCode = m.Run() // run all Test* functions (includes Ginkgo suite)
}

// ─────────────────────────────────────────────────────────────────────────────
// Environment initialisation
// ─────────────────────────────────────────────────────────────────────────────

// initE2EEnv populates testEnv from environment variables with sensible
// defaults.  No external commands are run at this stage.
func initE2EEnv() error {
	testEnv.ClusterName = envOrDefault("KIND_CLUSTER", defaultClusterName)
	testEnv.DockerHost = envOrDefault("DOCKER_HOST", defaultDockerHost)
	testEnv.ImageTag = envOrDefault("E2E_IMAGE_TAG", "e2e")
	testEnv.HelmRelease = envOrDefault("E2E_HELM_RELEASE", "pillar-csi")
	testEnv.HelmNamespace = envOrDefault("E2E_HELM_NAMESPACE", "pillar-csi-system")

	// External agent: pre-existing address (mutually exclusive with
	// LaunchExternalAgent — if both are set, the pre-existing address wins).
	testEnv.ExternalAgentAddr = os.Getenv("EXTERNAL_AGENT_ADDR")

	// External agent Docker container lifecycle.
	testEnv.LaunchExternalAgent = os.Getenv("E2E_LAUNCH_EXTERNAL_AGENT") == "true"
	testEnv.ExternalAgentPort = envOrDefault("E2E_EXTERNAL_AGENT_PORT", "9500")
	testEnv.ExternalAgentZFSPool = envOrDefault("E2E_EXTERNAL_AGENT_ZFS_POOL", "e2e-pool")

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
	if err := ensureKindCluster(); err != nil {
		return fmt.Errorf("kind cluster: %w", err)
	}
	if err := ensureStorageNodeLabel(); err != nil {
		return fmt.Errorf("storage node label: %w", err)
	}
	if err := ensureComputeNodeLabel(); err != nil {
		return fmt.Errorf("compute node label: %w", err)
	}
	if err := validateWorkerNodeMounts(); err != nil {
		return fmt.Errorf("worker node mounts: %w", err)
	}
	if err := buildAndLoadImages(); err != nil {
		return fmt.Errorf("docker images: %w", err)
	}
	if err := preloadSidecarImages(); err != nil {
		return fmt.Errorf("sidecar images: %w", err)
	}
	if err := setupZFSPool(); err != nil {
		return fmt.Errorf("zfs pool: %w", err)
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
	if err := waitForAPIServer(2 * time.Minute); err != nil {
		return fmt.Errorf("API server not ready before helm install: %w", err)
	}

	if err := installHelm(); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
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
	// When Kind runs on a remote Docker daemon (e.g. tcp://10.111.0.1:2375),
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

	fmt.Fprintf(os.Stdout, "e2e setup: creating kind cluster %q\n", testEnv.ClusterName)
	if err := runCmd("kind", "create", "cluster",
		"--name", testEnv.ClusterName,
		"--config", configFile,
	); err != nil {
		return fmt.Errorf("kind create cluster: %w", err)
	}

	// Capture kubeconfig and point KUBECONFIG at it.
	kubeconfigPath, err := writeKindKubeconfig(testEnv.ClusterName)
	if err != nil {
		return fmt.Errorf("get kubeconfig: %w", err)
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
// "x509: certificate is not valid for 10.111.0.1" when connecting via the
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

// ensureComputeNodeLabel labels the second worker node as a compute node so
// that e2e test Pods that mount CSI volumes (NVMe-oF initiator side) are
// scheduled on a dedicated compute worker.  The Kind cluster is always freshly
// created; this step ensures the label is present on every run.
//
// The kind-config.yaml already sets the label via its `labels:` block, but
// this function provides a belt-and-suspenders guarantee: if the label was
// removed after cluster creation, or if the kind-config.yaml diverges from
// the running cluster, the label is re-applied here.
//
// The "second worker" is identified as the first node that has neither the
// control-plane role nor the storage-node label.  This mirrors the Kind
// topology in kind-config.yaml (control-plane, storage-worker, compute-worker).
func ensureComputeNodeLabel() error {
	fmt.Fprintf(os.Stdout, "e2e setup: ensuring compute-node label on second worker\n")
	// Find the compute worker: a node that is not the control-plane and does
	// not already have the storage-node label.
	out, err := captureOutput("kubectl", "get", "nodes",
		"-l", "!node-role.kubernetes.io/control-plane,!pillar-csi.bhyoo.com/storage-node",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("find compute worker node: %s: %w", strings.TrimSpace(out), err)
	}
	computeNode := strings.TrimSpace(out)
	if computeNode == "" {
		return fmt.Errorf("no compute worker node found in cluster %q "+
			"(expected a worker without pillar-csi.bhyoo.com/storage-node label)",
			testEnv.ClusterName)
	}
	return runCmd("kubectl", "label", "node", computeNode,
		"pillar-csi.bhyoo.com/compute-node=true", "--overwrite")
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

// buildAndLoadImages builds the controller, agent, and node Docker images and
// loads each one into every node of the Kind cluster.  Images are tagged with
// the full registry paths used in the Helm chart (ghcr.io/bhyoo/pillar-csi/*)
// so that pods with imagePullPolicy: Never find the correct image name in the
// Kind node's container-image cache.
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
//  2. "docker cp localTar node:/tmp/kind-image.tar" — uploads the local
//     tar file into each Kind node container via the Docker API's
//     file-copy endpoint, which is a complete HTTP transaction with no
//     interactive exec channel.
//  3. "docker exec node ctr images import /tmp/kind-image.tar" — imports
//     from the local file inside the container; no streaming required.
//  4. "docker exec node rm /tmp/kind-image.tar" — cleans up the tar.
//
// The extra round-trip (download then upload) costs bandwidth but eliminates
// the exec-streaming reliability problem.
func buildAndLoadImages() error {
	type imageSpec struct {
		dockerfile string
		name       string
	}
	images := []imageSpec{
		{"Dockerfile", "ghcr.io/bhyoo/pillar-csi/controller:" + testEnv.ImageTag},
		{"Dockerfile.agent", "ghcr.io/bhyoo/pillar-csi/agent:" + testEnv.ImageTag},
		{"Dockerfile.node", "ghcr.io/bhyoo/pillar-csi/node:" + testEnv.ImageTag},
	}

	for _, img := range images {
		fmt.Fprintf(os.Stdout, "e2e setup: building image %s from %s\n", img.name, img.dockerfile)
		if err := runCmd("docker", "build",
			"-f", img.dockerfile,
			"-t", img.name,
			".",
		); err != nil {
			return fmt.Errorf("docker build %s: %w", img.name, err)
		}

		fmt.Fprintf(os.Stdout, "e2e setup: loading image %s into kind cluster %s\n",
			img.name, testEnv.ClusterName)
		if err := loadImageIntoKindNodes(img.name); err != nil {
			return fmt.Errorf("load image %s into kind nodes: %w", img.name, err)
		}
	}
	return nil
}

// preloadSidecarImages pulls all CSI sidecar images into the local Docker
// daemon and then loads them into every Kind WORKER node via the same
// file-copy mechanism used by loadImageIntoKindNodes.
//
// Rationale: Kind nodes run an internal containerd that has no access to the
// local Docker daemon image cache.  On a freshly-created cluster every image
// must be pulled from the registry, which can take minutes for CSI sidecar
// images from registry.k8s.io on slow or rate-limited networks.  By
// preloading these images the Helm --wait phase starts with all required
// images already present in containerd, allowing pods to start within seconds
// and keeping the 5-minute Helm timeout comfortable.
//
// Only worker nodes are targeted (not the control-plane) because:
//   - Kubernetes taints the control-plane with NoSchedule so workload pods
//     (controller deployment, DaemonSets) are only scheduled on workers.
//   - The control-plane node runs kube-apiserver/etcd/scheduler and may have
//     insufficient free memory for large image imports (exit 137 / OOM).
//
// The image list must stay in sync with the versions declared in
// charts/pillar-csi/values.yaml.  The global imagePullPolicy is
// "IfNotPresent" in the e2e helm-values.yaml so preloaded images are used
// and the registry is not contacted during pod start-up.
func preloadSidecarImages() error {
	sidecarImages := []string{
		"registry.k8s.io/sig-storage/csi-provisioner:v5.2.0",
		"registry.k8s.io/sig-storage/csi-attacher:v4.8.1",
		"registry.k8s.io/sig-storage/csi-resizer:v1.13.2",
		"registry.k8s.io/sig-storage/livenessprobe:v2.15.0",
		"registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0",
		"busybox:1.36",
	}

	// Discover worker nodes only.  Kind labels each node container with
	// io.x-k8s.kind.role=worker or =control-plane; filtering by "worker"
	// prevents the expensive import step from running on the control-plane.
	nodesOut, err := captureOutput("docker", "ps",
		"--filter", "label=io.x-k8s.kind.cluster="+testEnv.ClusterName,
		"--filter", "label=io.x-k8s.kind.role=worker",
		"--format", "{{.Names}}")
	if err != nil {
		return fmt.Errorf("docker ps for worker nodes: %s: %w", strings.TrimSpace(nodesOut), err)
	}

	var workerNodes []string
	for _, node := range strings.Split(strings.TrimSpace(nodesOut), "\n") {
		node = strings.TrimSpace(node)
		if node != "" {
			workerNodes = append(workerNodes, node)
		}
	}
	if len(workerNodes) == 0 {
		return fmt.Errorf("no worker nodes found in cluster %q", testEnv.ClusterName)
	}

	for _, img := range sidecarImages {
		fmt.Fprintf(os.Stdout,
			"e2e setup: preloading sidecar image %s into %d worker nodes\n",
			img, len(workerNodes))

		// Pull into the local Docker daemon (no-op when already present).
		if err := runCmd("docker", "pull", img); err != nil {
			return fmt.Errorf("docker pull %s: %w", img, err)
		}

		// Save the image to a local tar file then copy+import it into each
		// worker node (the same approach used by loadImageIntoKindNodes).
		localTar, err := os.CreateTemp("", "kind-sidecar-*.tar")
		if err != nil {
			return fmt.Errorf("create temp tar for %s: %w", img, err)
		}
		localTarPath := localTar.Name()
		localTar.Close()

		if err := runCmd("docker", "save", img, "-o", localTarPath); err != nil {
			_ = os.Remove(localTarPath)
			return fmt.Errorf("docker save %s: %w", img, err)
		}

		for _, node := range workerNodes {
			const nodeTar = "/root/kind-sidecar.tar"

			fmt.Fprintf(os.Stdout, "e2e setup: copying sidecar image tar to node %s\n", node)
			if err := runCmd("docker", "cp", localTarPath, node+":"+nodeTar); err != nil {
				_ = os.Remove(localTarPath)
				return fmt.Errorf("docker cp sidecar image to %s: %w", node, err)
			}

			fmt.Fprintf(os.Stdout, "e2e setup: importing sidecar image on node %s\n", node)
			if err := runCmd("docker", "exec", node,
				"ctr", "--namespace=k8s.io", "images", "import",
				"--all-platforms", "--digests", "--snapshotter=overlayfs",
				nodeTar,
			); err != nil {
				_ = os.Remove(localTarPath)
				return fmt.Errorf("ctr import sidecar image on %s: %w", node, err)
			}

			_ = runCmd("docker", "exec", node, "rm", nodeTar) //nolint:errcheck
		}

		_ = os.Remove(localTarPath)
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

	for _, node := range strings.Split(strings.TrimSpace(nodesOut), "\n") {
		node = strings.TrimSpace(node)
		if node == "" {
			continue
		}

		// Use /root/ instead of /tmp/: Kind node containers mount /tmp as
		// tmpfs, and docker cp silently fails to write into tmpfs paths when
		// the Docker daemon is remote (returns exit 0 but no file appears).
		// /root/ is backed by the container's overlay filesystem and accepts
		// docker cp writes reliably.
		const nodeTar = "/root/kind-image.tar"

		// Step 3 — Copy tar into the node container.
		fmt.Fprintf(os.Stdout, "e2e setup: copying image tar to node %s\n", node)
		if err := runCmd("docker", "cp", localTarPath, node+":"+nodeTar); err != nil {
			return fmt.Errorf("docker cp to %s: %w", node, err)
		}

		// Verify the file landed in the container.
		if err := runCmd("docker", "exec", node, "test", "-f", nodeTar); err != nil {
			return fmt.Errorf(
				"file %s missing in %s immediately after docker cp "+
					"(docker cp may not support remote daemon file upload): %w",
				nodeTar, node, err)
		}

		// Step 4 — Import from the file (no exec-streaming needed).
		fmt.Fprintf(os.Stdout, "e2e setup: importing image on node %s\n", node)
		if err := runCmd("docker", "exec", node,
			"ctr", "--namespace=k8s.io", "images", "import",
			"--all-platforms", "--digests", "--snapshotter=overlayfs",
			nodeTar,
		); err != nil {
			return fmt.Errorf("ctr import on %s: %w", node, err)
		}

		// Step 5 — Remove tar from the container.
		_ = runCmd("docker", "exec", node, "rm", nodeTar) //nolint:errcheck
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

	// Override image tags via --set so the chart uses whatever tag TestMain
	// built and loaded into Kind, regardless of what the embedded YAML contains.
	args = append(args,
		"--set", "controller.image.tag="+testEnv.ImageTag,
		"--set", "agent.image.tag="+testEnv.ImageTag,
		"--set", "node.image.tag="+testEnv.ImageTag,
		"--wait",
		"--timeout", "5m",
	)

	if err := runCmd("helm", args...); err != nil {
		return err
	}

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
	crdWaitErr := runCmd("kubectl", "wait",
		"--for=condition=established",
		"--timeout=60s",
		"crd/pillartargets.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarpools.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarprotocols.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarbindings.pillar-csi.pillar-csi.bhyoo.com",
		"crd/pillarvolumes.pillar-csi.pillar-csi.bhyoo.com",
	)
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
		fmt.Fprintf(os.Stdout, "e2e setup: agent DaemonSet pillar-csi-agent is present\n")

		// Wait for the agent DaemonSet rollout to complete so that all test
		// specs that assert DaemonSet readiness start from a known-good state.
		fmt.Fprintf(os.Stdout, "e2e setup: waiting for agent DaemonSet rollout\n")
		if err := runCmd("kubectl", "rollout", "status",
			"daemonset/pillar-csi-agent",
			"--namespace", testEnv.HelmNamespace,
			"--timeout", "4m",
		); err != nil {
			fmt.Fprintf(os.Stderr,
				"e2e setup: WARNING: agent DaemonSet rollout did not complete: %v\n", err)
		}
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
	if os.Getenv("E2E_SKIP_TEARDOWN") == "true" {
		fmt.Fprintf(os.Stdout, "e2e teardown: SKIPPED (E2E_SKIP_TEARDOWN=true) — cluster %q left running for debugging\n", testEnv.ClusterName)
		return
	}
	if testEnv.HelmRelease != "" {
		fmt.Fprintf(os.Stdout, "e2e teardown: uninstalling helm release %q\n",
			testEnv.HelmRelease)
		if err := runCmd("helm", "uninstall", testEnv.HelmRelease,
			"--namespace", testEnv.HelmNamespace,
			"--ignore-not-found",
		); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: helm uninstall: %v\n", err)
		}
	}

	// Stop and remove the external-agent container started by TestMain.
	stopExternalAgentContainer()

	// Destroy the loopback ZFS pool and release the DockerHostExec helper.
	teardownZFSPool()

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
	if err := runCmd("kind", "delete", "cluster",
		"--name", clusterName,
	); err != nil {
		fmt.Fprintf(os.Stderr, "e2e teardown: kind delete cluster: %v\n", err)
	}

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
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// waitForAPIServer polls "kubectl version" until the Kubernetes API server
// responds successfully, or until the given deadline is exceeded.
//
// Loading Docker images into kind nodes can temporarily spike memory and cause
// the kube-apiserver to restart.  Calling waitForAPIServer before running
// Helm guarantees that the API is reachable before Helm sends API requests.
func waitForAPIServer(timeout time.Duration) error {
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
// stored in testEnv.DockerHost (defaulting to defaultDockerHost when empty).
// Any existing DOCKER_HOST entry in env is replaced so the sub-process always
// uses the configured daemon endpoint.
func injectDockerHost(env []string) []string {
	host := testEnv.DockerHost
	if host == "" {
		host = defaultDockerHost
	}
	const key = "DOCKER_HOST="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key) {
			out = append(out, e)
		}
	}
	return append(out, key+host)
}

// writeKindKubeconfig runs "kind get kubeconfig --name <cluster>", rewrites
// the API server address to point at the remote Docker host (instead of
// 127.0.0.1 which only works on the Docker daemon host), and writes the result
// to a new temporary file.
//
// When Kind runs on a remote Docker daemon (e.g. tcp://10.111.0.1:2375), the
// kubeconfig it emits contains "server: https://127.0.0.1:<port>".  That
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
// URL such as "tcp://10.111.0.1:2375".  Returns an empty string when the
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

// externalAgentContainerName returns the Docker container name derived from
// the Kind cluster name so that multiple concurrent CI runs on the same host
// (using distinct KIND_CLUSTER values) do not collide.
func externalAgentContainerName() string {
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
	out, err := captureOutput("docker", "run",
		"--detach",
		"--name", name,
		"--network", "kind",
		"-p", portMapping,
		"--privileged",
		"--user=root",
		"--mount", "type=tmpfs,destination=/tmp",
		image,
		"--listen-address=0.0.0.0:9500",
		"--zfs-pool="+testEnv.ExternalAgentZFSPool,
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
