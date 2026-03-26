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
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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
	if err := buildAndLoadImages(); err != nil {
		return fmt.Errorf("docker images: %w", err)
	}

	// Start the external agent container when requested and no pre-existing
	// address has been supplied via EXTERNAL_AGENT_ADDR.
	if testEnv.LaunchExternalAgent && testEnv.ExternalAgentAddr == "" {
		if err := startExternalAgentContainer(); err != nil {
			return fmt.Errorf("external agent container: %w", err)
		}
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
func ensureKindCluster() error {
	// Delete any pre-existing cluster with the same name to guarantee a clean
	// slate and prevent state leakage between runs.
	existingClusters, _ := captureOutput("kind", "get", "clusters")
	if clusterExists(existingClusters, testEnv.ClusterName) {
		fmt.Fprintf(os.Stdout,
			"e2e setup: kind cluster %q already exists — deleting for a fresh start\n",
			testEnv.ClusterName)
		if err := runCmd("kind", "delete", "cluster",
			"--name", testEnv.ClusterName,
		); err != nil {
			return fmt.Errorf("kind delete existing cluster: %w", err)
		}
	}

	// Write the embedded kind-config.yaml to a temporary file.
	configFile, err := writeTempFile("kind-config-*.yaml", KindConfigYAML)
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

// buildAndLoadImages builds the controller, agent, and node Docker images and
// loads each one into every node of the Kind cluster.  Images are tagged with
// the full registry paths used in the Helm chart (ghcr.io/bhyoo/pillar-csi/*)
// so that pods with imagePullPolicy: Never find the correct image name in the
// Kind node's container-image cache.
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
		if err := runCmd("kind", "load", "docker-image", img.name,
			"--name", testEnv.ClusterName,
		); err != nil {
			return fmt.Errorf("kind load %s: %w", img.name, err)
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

	return runCmd("helm", args...)
}

// ─────────────────────────────────────────────────────────────────────────────
// Teardown
// ─────────────────────────────────────────────────────────────────────────────

// teardownE2E removes the Helm release, stops the external-agent container
// (if TestMain started one), and always deletes the Kind cluster.  All errors
// are logged to stderr but do not affect the exit code — that was already
// captured by the m.Run() call in TestMain.
func teardownE2E() {
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

	if testEnv.ClusterName != "" {
		fmt.Fprintf(os.Stdout, "e2e teardown: deleting kind cluster %q\n",
			testEnv.ClusterName)
		if err := runCmd("kind", "delete", "cluster",
			"--name", testEnv.ClusterName,
		); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: kind delete cluster: %v\n", err)
		}
	}

	// Remove the temporary kubeconfig written by writeKindKubeconfig.
	if testEnv.KubeconfigPath != "" {
		_ = os.Remove(testEnv.KubeconfigPath)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

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

// writeKindKubeconfig runs "kind get kubeconfig --name <cluster>", writes the
// output to a new temporary file, and returns its path.
func writeKindKubeconfig(clusterName string) (string, error) {
	raw, err := captureOutput("kind", "get", "kubeconfig", "--name", clusterName)
	if err != nil {
		return "", fmt.Errorf("kind get kubeconfig: %s: %w", raw, err)
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
	//   --mount tmpfs    → writable /tmp so --configfs-root=/tmp works without
	//                      needing kernel nvmet modules or real ZFS
	//   --user 65532     → matches the non-root USER in Dockerfile.agent
	out, err := captureOutput("docker", "run",
		"--detach",
		"--name", name,
		"--network", "kind",
		"-p", portMapping,
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
