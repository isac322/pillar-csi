//go:build e2e

package e2e

// helm_bootstrap_e2e.go — Sub-AC 2: parallel-safe Helm chart bootstrap.
//
// Problem: When running Helm-related specs (e.g., with --label-filter=helm),
// the E27 Describe blocks each include a BeforeAll or It node that executes
// `helm install --wait --timeout 5m` (TC-E27.207 tests helm install itself).
// With N parallel Ginkgo workers assigned
// to different Ordered containers, multiple workers can attempt concurrent Helm
// installs that individually take 5 minutes. The suite-level --timeout=2m is
// then exceeded, causing:
//
//	"Ginkgo timed out waiting for all parallel procs to report back"
//
// Solution: Provide an opt-in SynchronizedBeforeSuite hook that installs the
// suite-level pillar-csi Helm release on Ginkgo node 1 BEFORE any spec runs.
// Other workers block on SynchronizedBeforeSuite until node 1 returns the JSON
// payload that includes the Helm install result. After SynchronizedBeforeSuite
// completes, every worker starts its spec partition with Helm already deployed.
//
// # Activation
//
//	E2E_HELM_BOOTSTRAP=true make test-e2e E2E_LABEL_FILTER=E10-cluster
//
// When E2E_HELM_BOOTSTRAP is NOT set (or set to "false"/"0"), bootstrapSuiteHelm
// is skipped entirely; the E27 Helm install tests manage their own lifecycle.
//
// # Release configuration
//
//	E2E_HELM_RELEASE   — release name   (default: "pillar-csi")
//	E2E_HELM_NAMESPACE — target namespace (default: "pillar-csi-system")
//
// Both are forwarded by the Makefile's E2E_COMMON_ENV block, so the default
// `make test-e2e` already sets them to the correct values.
//
// # DO NOT combine with E27 Helm install tests
//
// E27 Helm tests (--label-filter=helm) include TC-E27.207 which itself runs
// plain `helm install pillar-csi … --wait --timeout 5m` to test the install
// behaviour directly. The suite-level bootstrap here uses `helm upgrade --install`
// (idempotent). If E2E_HELM_BOOTSTRAP=true
// is also set, node-1 pre-installs the same release and TC-E27.207 fails with
// "cannot re-use a name that is still in use". Use E2E_HELM_BOOTSTRAP only
// when running tests that REQUIRE a pre-installed chart (e.g., E10-cluster)
// but do NOT test the install process itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// helmBootstrapEnvVar enables suite-level Helm pre-install when set to
	// "true" or "1". Controlled by E2E_HELM_BOOTSTRAP env var.
	helmBootstrapEnvVar = "E2E_HELM_BOOTSTRAP"

	// helmReleaseEnvVar overrides the suite-level release name.
	// The Makefile forwards E2E_HELM_RELEASE (default: "pillar-csi").
	helmReleaseEnvVar = "E2E_HELM_RELEASE"

	// helmNamespaceEnvVar overrides the suite-level namespace.
	// The Makefile forwards E2E_HELM_NAMESPACE (default: "pillar-csi-system").
	helmNamespaceEnvVar = "E2E_HELM_NAMESPACE"

	// helmInstallTimeout is the maximum duration allowed for `helm upgrade --install
	// --wait --timeout=120s` inside bootstrapSuiteHelm. 2 minutes for chart
	// deployment (helm --timeout=120s) plus headroom for slow Kind nodes.
	//
	// SSOT compliance: docs/testing/infra/HELM.md mandates
	// `helm upgrade --install ... --wait --timeout=120s` for idempotent installation.
	helmInstallTimeout = 7 * time.Minute

	// helmTeardownTimeout is the maximum duration for `helm uninstall --wait`
	// inside teardownSuiteHelm.
	helmTeardownTimeout = 3 * time.Minute
)

// helmBootstrapState holds the state of the suite-level Helm release that was
// pre-installed by SynchronizedBeforeSuite node-1. It is serialised to JSON
// and propagated to every parallel worker via synchronizedSuitePayload.
type helmBootstrapState struct {
	// Installed is true when bootstrapSuiteHelm deployed the chart successfully.
	Installed bool `json:"installed"`
	// Release is the Helm release name (e.g. "pillar-csi").
	Release string `json:"release"`
	// Namespace is the Kubernetes namespace of the release.
	Namespace string `json:"namespace"`
	// ChartPath is the absolute path of the Helm chart that was installed.
	ChartPath string `json:"chartPath"`
}

// synchronizedSuitePayload is the JSON blob produced by SynchronizedBeforeSuite
// node-1 and consumed by the all-nodes phase. It carries both the Kind cluster
// connection details (previously the only payload) and the optional Helm
// bootstrap state so that workers can connect to the cluster and know whether
// a suite-level Helm release was pre-installed.
type synchronizedSuitePayload struct {
	// KindState contains the cluster name, kubeconfig path, and related details.
	KindState *kindBootstrapState `json:"kindState"`
	// HelmState is set when E2E_HELM_BOOTSTRAP=true; nil otherwise.
	HelmState *helmBootstrapState `json:"helmState,omitempty"`
}

// encodeSuitePayload serialises a synchronizedSuitePayload to JSON.
func encodeSuitePayload(p synchronizedSuitePayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode suite payload: %w", err)
	}
	return data, nil
}

// decodeSuitePayload deserialises the JSON produced by encodeSuitePayload.
func decodeSuitePayload(data []byte) (synchronizedSuitePayload, error) {
	var p synchronizedSuitePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("decode suite payload: %w", err)
	}
	if p.KindState == nil {
		return p, fmt.Errorf("decode suite payload: kindState is nil")
	}
	return p, nil
}

// suiteHelmBootstrap is the suite-level Helm state received from the
// SynchronizedBeforeSuite node-1 payload and stored by every worker.
// It is nil when E2E_HELM_BOOTSTRAP is not set (the default).
var suiteHelmBootstrap *helmBootstrapState

// resolveHelmBootstrap returns true when E2E_HELM_BOOTSTRAP is "true" or "1".
func resolveHelmBootstrap() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(helmBootstrapEnvVar)))
	return v == "true" || v == "1"
}

// bootstrapSuiteHelm installs the pillar-csi Helm chart into the Kind cluster
// described by clusterState.
//
// This function is called from SynchronizedBeforeSuite node-1 — it runs exactly
// once across the whole parallel suite run. All other workers are blocked in
// SynchronizedBeforeSuite until node-1 returns the payload that includes the
// Helm install result. This serialisation is provided by Ginkgo's
// SynchronizedBeforeSuite protocol and requires no additional locking.
//
// Environment variables:
//
//	E2E_HELM_RELEASE   — Helm release name   (default: "pillar-csi")
//	E2E_HELM_NAMESPACE — target namespace     (default: "pillar-csi-system")
func bootstrapSuiteHelm(
	ctx context.Context,
	clusterState *kindBootstrapState,
	output io.Writer,
) (*helmBootstrapState, error) {
	if clusterState == nil {
		return nil, fmt.Errorf("[helm-bootstrap] cluster state is nil")
	}
	if output == nil {
		output = io.Discard
	}

	release := strings.TrimSpace(os.Getenv(helmReleaseEnvVar))
	if release == "" {
		release = "pillar-csi"
	}
	namespace := strings.TrimSpace(os.Getenv(helmNamespaceEnvVar))
	if namespace == "" {
		namespace = "pillar-csi-system"
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, fmt.Errorf("[helm-bootstrap] locate repo root: %w", err)
	}
	chartPath := filepath.Join(repoRoot, "charts", "pillar-csi")
	if _, statErr := os.Stat(chartPath); statErr != nil {
		return nil, fmt.Errorf("[helm-bootstrap] chart path %q: %w", chartPath, statErr)
	}

	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] node-1: installing release %q in namespace %q (chart: %s)\n",
		release, namespace, chartPath)

	var stdoutBuf, stderrBuf bytes.Buffer
	// SSOT compliance: docs/testing/infra/HELM.md §2 mandates
	//   `helm upgrade --install` for idempotency (not plain `helm install`)
	//   `--wait --timeout=120s` for Pod readiness gating.
	// Plain `helm install` is intentionally reserved for TC-E27.207 which tests
	// the install behavior itself; the suite-level bootstrap always uses
	// `helm upgrade --install` so it is idempotent across re-runs.
	cmd := exec.CommandContext(ctx, "helm", //nolint:gosec
		"--kubeconfig="+clusterState.KubeconfigPath,
		"upgrade", "--install", release, chartPath,
		"--namespace", namespace,
		"--create-namespace",
		"--wait",
		"--timeout=120s",
	)
	cmd.Stdout = io.MultiWriter(output, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(output, &stderrBuf)

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, fmt.Errorf("[helm-bootstrap] helm upgrade --install %q: %w\nstderr: %s",
			release, err, errMsg)
	}

	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] release %q deployed successfully in namespace %q\n",
		release, namespace)

	return &helmBootstrapState{
		Installed: true,
		Release:   release,
		Namespace: namespace,
		ChartPath: chartPath,
	}, nil
}

// teardownSuiteHelm uninstalls the suite-level Helm release that was
// pre-installed by bootstrapSuiteHelm.
//
// Called from SynchronizedAfterSuite primary phase (Ginkgo node-1 only),
// before the Kind cluster is deleted. Errors are logged but not returned
// because the Kind cluster — and all its resources — will be deleted
// immediately after this call regardless.
//
// clusterState is the Kind cluster state used to derive the kubeconfig path.
// Passing it explicitly avoids a dependency on test-file globals (suiteKindCluster).
func teardownSuiteHelm(ctx context.Context, state *helmBootstrapState, clusterState *kindBootstrapState, output io.Writer) {
	if state == nil || !state.Installed {
		return
	}
	if output == nil {
		output = io.Discard
	}

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && clusterState != nil {
		kubeconfigPath = clusterState.KubeconfigPath
	}
	if kubeconfigPath == "" {
		_, _ = fmt.Fprintf(output,
			"[helm-bootstrap] teardown: KUBECONFIG not set — skipping helm uninstall\n")
		return
	}

	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] uninstalling release %q from namespace %q\n",
		state.Release, state.Namespace)

	cmd := exec.CommandContext(ctx, "helm", //nolint:gosec
		"--kubeconfig="+kubeconfigPath,
		"uninstall", state.Release,
		"--namespace", state.Namespace,
		"--wait",
	)
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Run(); err != nil {
		_, _ = fmt.Fprintf(output,
			"[helm-bootstrap] uninstall %q failed (non-fatal — cluster will be deleted): %v\n",
			state.Release, err)
		return
	}
	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] release %q uninstalled from namespace %q\n",
		state.Release, state.Namespace)
}
