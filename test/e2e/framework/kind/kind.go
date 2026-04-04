// Package kind provides a reusable helper for creating and deleting Kind
// (Kubernetes-in-Docker) clusters in E2E test suites. It uses the
// sigs.k8s.io/kind programmatic API so no external `kind` binary is required
// at call sites.
//
// All filesystem side-effects (kubeconfig files, temp directories) are kept
// strictly under /tmp to satisfy the environment-hygiene requirement that no
// files are created outside /tmp during the full test lifecycle.
//
// Usage:
//
//	cfg, err := kind.CreateCluster("pillar-csi-e2e-abc123")
//	if err != nil { ... }
//	defer kind.DeleteCluster("pillar-csi-e2e-abc123")
//
//	// cfg is the raw YAML kubeconfig; write it where needed or pass to
//	// client-go rest.InClusterConfig / clientcmd.RESTConfigFromKubeConfig.
package kind

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	kindcluster "sigs.k8s.io/kind/pkg/cluster"
)

const (
	// defaultWaitForReady is the maximum time CreateCluster waits for the
	// control-plane node to become Ready.  Two minutes is generous enough
	// for a laptop while keeping the overall suite comfortably under the
	// 120-second wall-clock budget when cluster creation is amortised across
	// the test run.
	defaultWaitForReady = 2 * time.Minute

	// kubeconfigDirPrefix is the prefix used for the per-cluster temp
	// directory created under /tmp.
	kubeconfigDirPrefix = "pillar-csi-kind-"
)

// CreateCluster creates a new Kind cluster with the given name, waits for it
// to be ready, and returns the raw YAML kubeconfig string.  The kubeconfig
// file is written under /tmp so it never touches the source tree.
//
// SSOT compliance: docs/testing/infra/KIND.md §2 (리소스 생성) mandates that
// the cluster name uses the "pillar-e2e-<suite>" naming pattern and that
// kubeconfig is stored under a temporary directory, never in the source tree.
//
// The caller is responsible for calling DeleteCluster when finished.  If
// CreateCluster returns an error the cluster (if partially created) is cleaned
// up internally; callers do not need to call DeleteCluster on error.
func CreateCluster(name string) (kubeconfig string, err error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("kind: cluster name must not be empty")
	}

	// Build a deterministic, /tmp-rooted path for the kubeconfig.
	kubeconfigPath, cleanup, err := tempKubeconfigPath(name)
	if err != nil {
		return "", err
	}

	provider := kindcluster.NewProvider()

	createErr := provider.Create(
		name,
		kindcluster.CreateWithKubeconfigPath(kubeconfigPath),
		kindcluster.CreateWithWaitForReady(defaultWaitForReady),
		// Suppress the usage/salutation banners that kind normally prints to
		// stdout; they pollute test output.
		kindcluster.CreateWithDisplayUsage(false),
		kindcluster.CreateWithDisplaySalutation(false),
	)
	if createErr != nil {
		// Best-effort cleanup: remove the temp directory and attempt to
		// delete the (possibly partially-created) cluster so the host is
		// left in a clean state.
		cleanup()
		_ = provider.Delete(name, kubeconfigPath)
		return "", fmt.Errorf("kind: create cluster %q: %w", name, createErr)
	}

	raw, readErr := os.ReadFile(kubeconfigPath)
	if readErr != nil {
		// Cluster was created but we cannot read the kubeconfig – clean up
		// to avoid leaving orphaned clusters.
		_ = provider.Delete(name, kubeconfigPath)
		cleanup()
		return "", fmt.Errorf("kind: read kubeconfig for %q: %w", name, readErr)
	}

	return string(raw), nil
}

// DeleteCluster deletes the Kind cluster with the given name and removes its
// associated /tmp kubeconfig directory.  It is idempotent: calling it on a
// cluster that no longer exists returns nil.
func DeleteCluster(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("kind: cluster name must not be empty")
	}

	provider := kindcluster.NewProvider()

	// Derive the same deterministic kubeconfig path used by CreateCluster.
	kubeconfigPath := kubeconfigPathForName(name)

	if err := provider.Delete(name, kubeconfigPath); err != nil {
		// kind returns an error if the cluster does not exist.  Treat that
		// as a success so that callers can call DeleteCluster idempotently
		// in defer statements without worrying about double-deletes.
		if isNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("kind: delete cluster %q: %w", name, err)
	}

	// Remove the temp directory created by CreateCluster.  Ignore errors
	// here; the directory may already be gone.
	_ = os.RemoveAll(kubeconfigDir(name))

	return nil
}

// KubeconfigPath returns the filesystem path where CreateCluster will write
// the kubeconfig for the named cluster.  Callers may use this to pass
// --kubeconfig to kubectl or to build a rest.Config via clientcmd.
func KubeconfigPath(name string) string {
	return kubeconfigPathForName(name)
}

// ReapOrphanClusters lists all existing Kind clusters whose names start with
// the given prefix and deletes each one. It is intended to be called at the
// start of TestMain — before a new cluster is created — to clean up clusters
// left behind by previous test runs that were SIGKILL'd (which cannot be
// caught by Go signal handlers).
//
// Errors from individual cluster deletions are collected and returned as a
// combined error so the caller sees the full picture. The function always
// attempts to delete every matching cluster regardless of intermediate errors.
//
// The function completes quickly (<5 s in the common case where no orphans
// exist) because provider.List() only queries Docker for container labels.
func ReapOrphanClusters(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("kind: reap orphan clusters: prefix must not be empty")
	}

	provider := kindcluster.NewProvider()

	clusters, err := provider.List()
	if err != nil {
		return fmt.Errorf("kind: reap orphan clusters: list clusters: %w", err)
	}

	var errs []string
	for _, name := range clusters {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// DeleteCluster is idempotent; ignore "not found" internally.
		if delErr := DeleteCluster(name); delErr != nil {
			errs = append(errs, fmt.Sprintf("delete %q: %v", name, delErr))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("kind: reap orphan clusters: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// kubeconfigDir returns the /tmp directory used for the given cluster's
// kubeconfig.  The path is deterministic given the cluster name so that
// DeleteCluster can reconstruct it without the caller passing it back.
func kubeconfigDir(name string) string {
	return filepath.Join(os.TempDir(), kubeconfigDirPrefix+name)
}

// kubeconfigPathForName returns the full path to the kubeconfig file for a
// cluster, derived deterministically from the cluster name.
func kubeconfigPathForName(name string) string {
	return filepath.Join(kubeconfigDir(name), "kubeconfig")
}

// tempKubeconfigPath creates the /tmp directory for the kubeconfig and
// returns (path, cleanup, error).  cleanup removes the directory; callers
// should invoke it only on error paths – on success the directory is removed
// by DeleteCluster.
func tempKubeconfigPath(name string) (path string, cleanup func(), err error) {
	dir := kubeconfigDir(name)
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return "", func() {}, fmt.Errorf("kind: create kubeconfig dir %q: %w", dir, mkErr)
	}
	p := filepath.Join(dir, "kubeconfig")
	return p, func() { _ = os.RemoveAll(dir) }, nil
}

// isNotFoundError returns true when err looks like a "cluster not found"
// error from kind.  The kind library does not export a typed sentinel so we
// resort to string matching – the error message has been stable across kind
// releases.
func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown cluster") ||
		strings.Contains(msg, "no such cluster") ||
		strings.Contains(msg, "cluster not found")
}
