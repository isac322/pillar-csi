//go:build e2e

package e2e

// helm_bootstrap_e2e.go — Sub-AC 2: parallel-safe Helm chart bootstrap.
//
// Problem: When running Helm-related specs (e.g., with --label-filter=helm),
// the E27 Describe blocks each include a BeforeAll or It node that executes
// `helm install --wait --timeout 5m`. With N parallel Ginkgo workers assigned
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
// `helm install pillar-csi … --wait --timeout 5m`. If E2E_HELM_BOOTSTRAP=true
// is also set, node-1 pre-installs the same release and TC-E27.207 fails with
// "cannot re-use a name that is still in use". Use E2E_HELM_BOOTSTRAP only
// when running tests that REQUIRE a pre-installed chart (e.g., E10-cluster)
// but do NOT test the install process itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	// defaultHelmNamespace is the chart's namespace when no override is set.
	defaultHelmNamespace = "pillar-csi-system"

	// helmInstallTimeout is the maximum duration allowed for `helm install --wait`
	// inside bootstrapSuiteHelm. 5 minutes for chart deployment plus 2 minutes
	// headroom for slow Kind nodes.
	helmInstallTimeout = 7 * time.Minute

	// helmTeardownTimeout is the maximum duration for `helm uninstall --wait`
	// inside teardownSuiteHelm.
	helmTeardownTimeout = 3 * time.Minute
)

// resolveHelmNamespace returns the configured Helm release namespace, using
// the same environment/default contract for bootstrap and E2E component lookups.
func resolveHelmNamespace() string {
	namespace := strings.TrimSpace(os.Getenv(helmNamespaceEnvVar))
	if namespace == "" {
		return defaultHelmNamespace
	}
	return namespace
}

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
	// Native fixture ownership is serialized rather than reconstructed from
	// mutable process environment during cleanup.
	NativeSourceRoot  string `json:"nativeSourceRoot,omitempty"`
	NativeStorageNode string `json:"nativeStorageNode,omitempty"`
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
) (_ *helmBootstrapState, err error) {
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
	namespace := resolveHelmNamespace()

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
	helmArgs := []string{
		"--kubeconfig=" + clusterState.KubeconfigPath,
		"install", release, chartPath,
		"--namespace", namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "5m",
	}
	var nativeSourceRoot, nativeStorageNode string
	if strings.EqualFold(strings.TrimSpace(os.Getenv("E2E_NFS_E2E")), "true") {
		pool := strings.TrimSpace(os.Getenv(suiteZFSPoolEnvVar))
		parent := strings.TrimSpace(os.Getenv(suiteNFSParentDatasetEnvVar))
		if pool == "" || parent == "" {
			return nil, fmt.Errorf("[helm-bootstrap] E2E_NFS_E2E requires %s and %s",
				suiteZFSPoolEnvVar, suiteNFSParentDatasetEnvVar)
		}
		tag := envOrDefault(imageTagEnvVar, defaultE2EImageTag)
		for _, component := range []string{"controller", "agent", "node"} {
			helmArgs = append(helmArgs,
				"--set", component+".image.repository=pillar-csi/"+component,
				"--set", component+".image.tag="+tag,
				"--set", component+".image.pullPolicy=Never")
		}
		// E71 filesystem cases consume the explicit file CSI identity.  E37
		// keeps its legacy classes and identity; enabling the second route here
		// does not rewrite existing StorageClasses or PVs.
		sourceRoot := "/var/lib/pillar-csi/e71-sources"
		storageNode := strings.TrimSpace(os.Getenv(suiteBackendContainerEnvVar))
		if storageNode == "" {
			return nil, fmt.Errorf("[helm-bootstrap] filesystem lane requires %s", suiteBackendContainerEnvVar)
		}
		if _, err := kindContainerExec(ctx, storageNode, "mkdir", "-p", sourceRoot); err != nil {
			return nil, fmt.Errorf("[helm-bootstrap] create owned source allow-root: %w", err)
		}
		// Roll back only this owned native fixture on partial preparation or
		// install failure. Kernel loop devices outlive a deleted Kind node.
		defer func() {
			if err == nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), helmTeardownTimeout)
			defer cancel()
			if cleanupErr := CleanupFilesystemAdoptionNativeSources(cleanupCtx, storageNode, sourceRoot); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("[helm-bootstrap] native fixture rollback: %w", cleanupErr))
			}
		}()
		if err := PrepareFilesystemAdoptionNativeSources(ctx, storageNode, sourceRoot); err != nil {
			return nil, fmt.Errorf("[helm-bootstrap] prepare existing native source fixtures: %w", err)
		}
		nativeSourceRoot, nativeStorageNode = sourceRoot, storageNode
		if err := os.Setenv("PILLAR_E2E_FILESYSTEM_SOURCE_ROOT", sourceRoot); err != nil {
			return nil, fmt.Errorf("[helm-bootstrap] export filesystem source root: %w", err)
		}
		if err := os.Setenv("PILLAR_E2E_FILESYSTEM_PHYSICAL_ROOT", "/"); err != nil {
			return nil, fmt.Errorf("[helm-bootstrap] export filesystem physical root: %w", err)
		}
		helmArgs = append(helmArgs,
			"--set", "fileDriver.enabled=true",
			"--set", "fileDriver.nfs.enabled=true",
			"--set", "fileDriver.name=files.pillar-csi.bhyoo.com",
			"--set", "fileDriver.sourceHostRoot=/host",
			"--set", "fileDriver.proxyRoot=/var/lib/pillar-csi/agent/datasets",
			"--set", "agent.backends[0].zfs.pool="+pool,
			"--set", "agent.backends[0].zfs.volumeType=zvol",
			"--set", "agent.backends[0].zfs.parentDataset="+parent,
			"--set", "agent.backends[1].zfs.pool="+pool,
			"--set", "agent.backends[1].zfs.volumeType=dataset",
			"--set", "agent.backends[1].zfs.parentDataset="+parent,
			"--set", "agent.backends[2].directory.logicalPool=e71-files",
			"--set", "agent.backends[2].directory.hostRoot="+sourceRoot,
			"--set", "agent.backends[3].directory.logicalPool=e71-files-alias",
			"--set", "agent.backends[3].directory.hostRoot="+sourceRoot,
			"--set", "agent.tolerations[0].operator=Exists",
			"--set", "node.tolerations[0].operator=Exists",
		)
	}
	cmd := exec.CommandContext(ctx, "helm", helmArgs...)
	cmd.Stdout = io.MultiWriter(output, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(output, &stderrBuf)

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderrBuf.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return nil, fmt.Errorf("[helm-bootstrap] helm install %q: %w\nstderr: %s",
			release, err, errMsg)
	}

	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] release %q deployed successfully in namespace %q\n",
		release, namespace)

	return &helmBootstrapState{
		Installed:         true,
		Release:           release,
		Namespace:         namespace,
		ChartPath:         chartPath,
		NativeSourceRoot:  nativeSourceRoot,
		NativeStorageNode: nativeStorageNode,
	}, nil
}

// teardownSuiteHelm uninstalls the suite-level Helm release that was
// pre-installed by bootstrapSuiteHelm.
//
// Called from SynchronizedAfterSuite primary phase (Ginkgo node-1 only).
// Errors are returned so uninstall or native loop cleanup failures fail the
// physical lane instead of being hidden by subsequent cluster deletion.
//
// clusterState is the Kind cluster state used to derive the kubeconfig path.
// Passing it explicitly avoids a dependency on test-file globals (suiteKindCluster).
func teardownSuiteHelm(ctx context.Context, state *helmBootstrapState, clusterState *kindBootstrapState, output io.Writer) error {
	if state == nil || !state.Installed {
		return nil
	}
	if output == nil {
		output = io.Discard
	}

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && clusterState != nil {
		kubeconfigPath = clusterState.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return fmt.Errorf("[helm-bootstrap] teardown: KUBECONFIG is unavailable")
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
		return fmt.Errorf("[helm-bootstrap] uninstall %q: %w", state.Release, err)
	}
	_, _ = fmt.Fprintf(output,
		"[helm-bootstrap] release %q uninstalled from namespace %q\n",
		state.Release, state.Namespace)
	if state.NativeSourceRoot != "" {
		if err := CleanupFilesystemAdoptionNativeSources(ctx,
			state.NativeStorageNode,
			state.NativeSourceRoot); err != nil {
			return fmt.Errorf("[helm-bootstrap] native fixture cleanup: %w", err)
		}
	}
	return nil
}
