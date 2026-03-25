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
// This file owns the full lifecycle of the e2e Kind cluster:
//
//  1. TestMain reads configuration from environment variables.
//  2. A multi-node Kind cluster is created (or adopted) from the embedded
//     kind-config.yaml located in testdata/.
//  3. Pillar-CSI Docker images are built and loaded into the cluster.
//  4. The Helm chart is installed via `helm upgrade --install`.
//  5. All e2e test functions (including the Ginkgo suite in e2e_suite_test.go)
//     are executed via m.Run().
//  6. The Helm release is removed and, when TestMain created the cluster, Kind
//     deletes it.  Teardown is unconditional: it runs via defer even when
//     m.Run() or setup panics.
//
// Shared state is stored in the package-level testEnv variable so that
// individual test files can read cluster coordinates (kubeconfig path, image
// tag, Helm namespace, etc.) without re-parsing the environment.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
	// when running the external-agent test suite.  Sourced from
	// EXTERNAL_AGENT_ADDR; empty string means "not running external-agent mode".
	ExternalAgentAddr string

	// clusterCreatedByUs records whether TestMain created the Kind cluster so
	// that teardown knows whether to delete it.  When adopting a pre-existing
	// cluster this is false and the cluster is left intact after the run.
	clusterCreatedByUs bool
}

// testEnv is the single, shared E2EEnv populated by TestMain.  All e2e test
// files should read cluster coordinates from this variable rather than
// re-querying the environment.
var testEnv = &E2EEnv{}

// defaultClusterName is used when KIND_CLUSTER is not set.
const defaultClusterName = "pillar-csi-e2e"

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
	testEnv.ImageTag = envOrDefault("E2E_IMAGE_TAG", "e2e")
	testEnv.HelmRelease = envOrDefault("E2E_HELM_RELEASE", "pillar-csi")
	testEnv.HelmNamespace = envOrDefault("E2E_HELM_NAMESPACE", "pillar-csi-system")
	testEnv.ExternalAgentAddr = os.Getenv("EXTERNAL_AGENT_ADDR")
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Setup
// ─────────────────────────────────────────────────────────────────────────────

// setupE2E orchestrates full cluster bootstrap:
//
//  1. Create (or adopt) the Kind cluster.
//  2. Build and load Docker images.
//  3. Install the Helm chart.
func setupE2E() error {
	if err := ensureKindCluster(); err != nil {
		return fmt.Errorf("kind cluster: %w", err)
	}
	if err := buildAndLoadImages(); err != nil {
		return fmt.Errorf("docker images: %w", err)
	}
	if err := installHelm(); err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	return nil
}

// ensureKindCluster creates the Kind cluster unless it already exists.  On
// success testEnv.KubeconfigPath is populated and KUBECONFIG is exported.
// The embedded KindConfigYAML (from testdata_embed.go) is written to a
// temporary file so Kind can read it via --config.
func ensureKindCluster() error {
	existingClusters, _ := captureOutput("kind", "get", "clusters")
	if clusterExists(existingClusters, testEnv.ClusterName) {
		fmt.Fprintf(os.Stdout, "e2e setup: kind cluster %q already exists — adopting\n",
			testEnv.ClusterName)
	} else {
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
		testEnv.clusterCreatedByUs = true
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

// buildAndLoadImages builds the controller, agent, and node Docker images and
// loads each one into every node of the Kind cluster.  Images are tagged with
// testEnv.ImageTag so Helm values files can reference them with
// imagePullPolicy: Never.
func buildAndLoadImages() error {
	type imageSpec struct {
		dockerfile string
		name       string
	}
	images := []imageSpec{
		{"Dockerfile", "pillar-csi-controller:" + testEnv.ImageTag},
		{"Dockerfile.agent", "pillar-csi-agent:" + testEnv.ImageTag},
		{"Dockerfile.node", "pillar-csi-node:" + testEnv.ImageTag},
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

// installHelm installs (or upgrades) the pillar-csi Helm chart.  The embedded
// HelmValuesYAML is written to a temporary file and passed via --values.
func installHelm() error {
	valuesFile, err := writeTempFile("helm-values-*.yaml", HelmValuesYAML)
	if err != nil {
		return fmt.Errorf("write helm values: %w", err)
	}
	defer os.Remove(valuesFile) //nolint:errcheck

	fmt.Fprintf(os.Stdout, "e2e setup: installing helm chart %q in namespace %q\n",
		testEnv.HelmRelease, testEnv.HelmNamespace)

	return runCmd("helm", "upgrade", "--install",
		testEnv.HelmRelease, "./charts/pillar-csi",
		"--namespace", testEnv.HelmNamespace,
		"--create-namespace",
		"--values", valuesFile,
		"--wait",
		"--timeout", "5m",
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Teardown
// ─────────────────────────────────────────────────────────────────────────────

// teardownE2E removes the Helm release and, when TestMain created the Kind
// cluster, deletes it.  All errors are logged to stderr but do not affect
// the exit code — that was already captured by the m.Run() call in TestMain.
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

	if testEnv.clusterCreatedByUs {
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
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = e2eProjectRoot()
	return cmd.Run()
}

// captureOutput is like runCmd but captures and returns combined output
// instead of streaming it.  Errors are returned alongside partial output.
func captureOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) //nolint:gosec
	cmd.Dir = e2eProjectRoot()
	out, err := cmd.CombinedOutput()
	return string(out), err
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

// e2eProjectRoot returns the repository root directory.  When go test is run
// from within test/e2e the working directory is under the repo root; this
// function strips the known suffix so that relative paths such as
// "./charts/pillar-csi" resolve correctly regardless of invocation directory.
func e2eProjectRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	for _, suffix := range []string{"/test/e2e", "/test"} {
		if strings.HasSuffix(wd, suffix) {
			return strings.TrimSuffix(wd, suffix)
		}
	}
	return wd
}
