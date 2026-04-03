package e2e

// kind_existing_cluster_test.go — Sub-AC 5.4: tests for USE_EXISTING_CLUSTER.
//
// Verified behaviours:
//
//  1. resolveUseExistingCluster returns false when E2E_USE_EXISTING_CLUSTER is unset.
//  2. resolveUseExistingCluster returns true for "true", "1", "TRUE", "True".
//  3. resolveUseExistingCluster returns false for "false", "0", "", "yes".
//  4. existingClusterState returns an error when KUBECONFIG is not set.
//  5. existingClusterState returns an error when KIND_CLUSTER is not set.
//  6. existingClusterState returns an error when KUBECONFIG points to a non-existent file.
//  7. existingClusterState succeeds when KUBECONFIG and KIND_CLUSTER are valid.
//  8. existingClusterState sets clusterCreated=false (does not own the cluster).
//  9. existingClusterState defaults kubeContext to "kind-<cluster-name>" when
//     PILLAR_CSI_E2E_KUBE_CONTEXT is unset.
// 10. existingClusterState honours PILLAR_CSI_E2E_KUBE_CONTEXT when set.
// 11. copyFile copies file content correctly.
// 12. copyFile returns an error when src does not exist.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── 1. resolveUseExistingCluster returns false when env is unset ──────────────

func TestUseExistingCluster_DisabledByDefault(t *testing.T) {
	t.Setenv(useExistingClusterEnvVar, "")
	if resolveUseExistingCluster() {
		t.Fatal("resolveUseExistingCluster should be false when env is unset")
	}
}

// ── 2. resolveUseExistingCluster returns true for truthy values ───────────────

func TestUseExistingCluster_TruthyValues(t *testing.T) {
	for _, v := range []string{"true", "1", "TRUE", "True", "TRUE "} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(useExistingClusterEnvVar, v)
			if !resolveUseExistingCluster() {
				t.Errorf("resolveUseExistingCluster should be true for %q", v)
			}
		})
	}
}

// ── 3. resolveUseExistingCluster returns false for falsy values ───────────────

func TestUseExistingCluster_FalsyValues(t *testing.T) {
	for _, v := range []string{"false", "0", "", "yes", "no"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(useExistingClusterEnvVar, v)
			if resolveUseExistingCluster() {
				t.Errorf("resolveUseExistingCluster should be false for %q", v)
			}
		})
	}
}

// ── 4. existingClusterState error when KUBECONFIG unset ──────────────────────

func TestExistingClusterState_MissingKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("KIND_CLUSTER", "test-cluster")
	t.Setenv(suiteContextEnvVar, "kind-test-cluster")

	_, err := existingClusterState()
	if err == nil {
		t.Fatal("expected error when KUBECONFIG is unset, got nil")
	}
	if !strings.Contains(err.Error(), "KUBECONFIG") {
		t.Errorf("error should mention KUBECONFIG: %v", err)
	}
}

// ── 5. existingClusterState error when KIND_CLUSTER unset ────────────────────

func TestExistingClusterState_MissingKindCluster(t *testing.T) {
	// Write a real temp kubeconfig so the KUBECONFIG check passes.
	kubeconfig := writeKubeconfigFixture(t)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KIND_CLUSTER", "")

	_, err := existingClusterState()
	if err == nil {
		t.Fatal("expected error when KIND_CLUSTER is unset, got nil")
	}
	if !strings.Contains(err.Error(), "KIND_CLUSTER") {
		t.Errorf("error should mention KIND_CLUSTER: %v", err)
	}
}

// ── 6. existingClusterState error when KUBECONFIG file does not exist ─────────

func TestExistingClusterState_KubeconfigNotExist(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent/path/to/kubeconfig")
	t.Setenv("KIND_CLUSTER", "test-cluster")

	_, err := existingClusterState()
	if err == nil {
		t.Fatal("expected error when KUBECONFIG path does not exist, got nil")
	}
}

// ── 7. existingClusterState succeeds with valid env ───────────────────────────

func TestExistingClusterState_Success(t *testing.T) {
	kubeconfig := writeKubeconfigFixture(t)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KIND_CLUSTER", "pillar-csi-e2e")
	t.Setenv(suiteContextEnvVar, "kind-pillar-csi-e2e")

	state, err := existingClusterState()
	if err != nil {
		t.Fatalf("existingClusterState: %v", err)
	}
	t.Cleanup(func() {
		if state != nil {
			_ = os.RemoveAll(state.SuiteRootDir)
		}
	})

	if state.ClusterName != "pillar-csi-e2e" {
		t.Errorf("ClusterName = %q, want %q", state.ClusterName, "pillar-csi-e2e")
	}
	if state.KubeContext != "kind-pillar-csi-e2e" {
		t.Errorf("KubeContext = %q, want %q", state.KubeContext, "kind-pillar-csi-e2e")
	}
}

// ── 8. existingClusterState sets clusterCreated=false ────────────────────────

func TestExistingClusterState_ClusterCreatedFalse(t *testing.T) {
	kubeconfig := writeKubeconfigFixture(t)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KIND_CLUSTER", "test-cluster")
	t.Setenv(suiteContextEnvVar, "kind-test-cluster")

	state, err := existingClusterState()
	if err != nil {
		t.Fatalf("existingClusterState: %v", err)
	}
	t.Cleanup(func() {
		if state != nil {
			_ = os.RemoveAll(state.SuiteRootDir)
		}
	})

	// clusterCreated=false means this invocation does not own the cluster and
	// must not delete it on teardown.
	if state.clusterCreated {
		t.Error("existingClusterState: clusterCreated should be false (invocation does not own the cluster)")
	}
}

// ── 9. kubeContext defaults to "kind-<cluster-name>" ─────────────────────────

func TestExistingClusterState_DefaultKubeContext(t *testing.T) {
	kubeconfig := writeKubeconfigFixture(t)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KIND_CLUSTER", "my-cluster")
	t.Setenv(suiteContextEnvVar, "") // unset

	state, err := existingClusterState()
	if err != nil {
		t.Fatalf("existingClusterState: %v", err)
	}
	t.Cleanup(func() {
		if state != nil {
			_ = os.RemoveAll(state.SuiteRootDir)
		}
	})

	wantContext := "kind-my-cluster"
	if state.KubeContext != wantContext {
		t.Errorf("KubeContext = %q, want %q (default kind-<name>)", state.KubeContext, wantContext)
	}
}

// ── 10. PILLAR_CSI_E2E_KUBE_CONTEXT overrides default ────────────────────────

func TestExistingClusterState_CustomKubeContext(t *testing.T) {
	kubeconfig := writeKubeconfigFixture(t)
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KIND_CLUSTER", "my-cluster")
	t.Setenv(suiteContextEnvVar, "custom-context-name")

	state, err := existingClusterState()
	if err != nil {
		t.Fatalf("existingClusterState: %v", err)
	}
	t.Cleanup(func() {
		if state != nil {
			_ = os.RemoveAll(state.SuiteRootDir)
		}
	})

	if state.KubeContext != "custom-context-name" {
		t.Errorf("KubeContext = %q, want %q", state.KubeContext, "custom-context-name")
	}
}

// ── 11. copyFile copies content ───────────────────────────────────────────────

func TestCopyFile_CopiesContent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.txt")
	dst := filepath.Join(dir, "dest.txt")

	want := []byte("hello world\n")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("copied content = %q, want %q", string(got), string(want))
	}
}

// ── 12. copyFile error when src does not exist ────────────────────────────────

func TestCopyFile_SourceNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dst := filepath.Join(dir, "dest.txt")

	err := copyFile("/nonexistent/src.txt", dst)
	if err == nil {
		t.Fatal("copyFile: expected error when src does not exist, got nil")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// writeKubeconfigFixture writes a minimal kubeconfig file to a temp directory
// and returns its path. The content is syntactically valid YAML but intentionally
// minimal (no real cluster, not for actual kubectl use).
func writeKubeconfigFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	content := `apiVersion: v1
clusters: []
contexts: []
current-context: ""
kind: Config
preferences: {}
users: []
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig fixture: %v", err)
	}
	return path
}
