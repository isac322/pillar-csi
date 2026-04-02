package kind_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	kindhelper "github.com/bhyoo/pillar-csi/test/e2e/framework/kind"
)

// TestKubeconfigPath verifies that KubeconfigPath returns a path that is
// rooted under /tmp and contains the cluster name.
func TestKubeconfigPath(t *testing.T) {
	t.Parallel()

	name := "test-cluster-abc"
	path := kindhelper.KubeconfigPath(name)

	tmpDir := os.TempDir()
	if !strings.HasPrefix(path, tmpDir) {
		t.Errorf("KubeconfigPath(%q) = %q; want a path under %q", name, path, tmpDir)
	}
	if !strings.Contains(path, name) {
		t.Errorf("KubeconfigPath(%q) = %q; expected it to contain the cluster name", name, path)
	}
	if filepath.Base(path) != "kubeconfig" {
		t.Errorf("KubeconfigPath(%q) = %q; expected base name to be %q", name, path, "kubeconfig")
	}
}

// TestKubeconfigPathDeterministic verifies that two calls with the same name
// return the same path (no random suffix).
func TestKubeconfigPathDeterministic(t *testing.T) {
	t.Parallel()

	name := "deterministic-cluster"
	p1 := kindhelper.KubeconfigPath(name)
	p2 := kindhelper.KubeconfigPath(name)
	if p1 != p2 {
		t.Errorf("KubeconfigPath is not deterministic: %q != %q", p1, p2)
	}
}

// TestKubeconfigPathDistinct verifies that different cluster names produce
// different kubeconfig paths (no accidental sharing).
func TestKubeconfigPathDistinct(t *testing.T) {
	t.Parallel()

	p1 := kindhelper.KubeconfigPath("cluster-alpha")
	p2 := kindhelper.KubeconfigPath("cluster-beta")
	if p1 == p2 {
		t.Errorf("KubeconfigPath returned the same path for different cluster names: %q", p1)
	}
}

// TestCreateClusterEmptyNameErrors verifies that an empty cluster name is
// rejected before any Docker/Kind calls are made.
func TestCreateClusterEmptyNameErrors(t *testing.T) {
	t.Parallel()

	_, err := kindhelper.CreateCluster("")
	if err == nil {
		t.Error("CreateCluster(\"\") should have returned an error, got nil")
	}
}

// TestDeleteClusterEmptyNameErrors verifies that an empty cluster name is
// rejected before any Docker/Kind calls are made.
func TestDeleteClusterEmptyNameErrors(t *testing.T) {
	t.Parallel()

	err := kindhelper.DeleteCluster("")
	if err == nil {
		t.Error("DeleteCluster(\"\") should have returned an error, got nil")
	}
}
