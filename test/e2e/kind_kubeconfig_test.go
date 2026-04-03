package e2e

// kind_kubeconfig_test.go — Unit tests for buildClusterRestConfig and
// SuiteKubeRestConfig (AC4c: per-invocation kubeconfig propagation).
//
// These tests exercise the helper functions in kind_bootstrap_e2e_test.go
// entirely in-process without a live Kind cluster.  They verify:
//
//   - buildClusterRestConfig returns a valid *rest.Config for a well-formed
//     kubeconfig file (path under /tmp, hygiene compliant).
//   - buildClusterRestConfig returns a clear error for an empty path or a
//     kubeconfig file that does not exist.
//   - SuiteKubeRestConfig returns whatever suiteRestConfig currently holds,
//     proving the accessor is wired to the correct package-level variable.

import (
	"os"
	"strings"
	"testing"
)

// minimalKubeconfig is a valid kubeconfig YAML that clientcmd can parse.
// It uses insecure-skip-tls-verify to avoid needing a real CA certificate
// while still exercising the full BuildConfigFromFlags code path.
const minimalKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    insecure-skip-tls-verify: true
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
preferences: {}
users:
- name: test-user
  user:
    token: test-token-ac4c
`

// TestBuildClusterRestConfigWithValidFile verifies that buildClusterRestConfig
// returns a non-nil *rest.Config when given a well-formed kubeconfig file.
// The returned config's Host must match the server URL in the kubeconfig.
func TestBuildClusterRestConfigWithValidFile(t *testing.T) {
	t.Parallel()

	kubeconfigPath := writeTmpKubeconfig(t, "ac4c-valid-", minimalKubeconfig)

	cfg, err := buildClusterRestConfig(kubeconfigPath)
	if err != nil {
		t.Fatalf("buildClusterRestConfig: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("buildClusterRestConfig returned nil config")
	}
	if cfg.Host == "" {
		t.Fatal("buildClusterRestConfig returned config with empty Host")
	}
	if !strings.Contains(cfg.Host, "127.0.0.1") {
		t.Fatalf("buildClusterRestConfig Host = %q, want string containing 127.0.0.1", cfg.Host)
	}
}

// TestBuildClusterRestConfigEmptyPath verifies that buildClusterRestConfig
// returns an error and a nil config when the kubeconfig path is empty.
func TestBuildClusterRestConfigEmptyPath(t *testing.T) {
	t.Parallel()

	cfg, err := buildClusterRestConfig("")
	if err == nil {
		t.Fatal("buildClusterRestConfig(empty path): expected error, got nil")
	}
	if cfg != nil {
		t.Fatalf("buildClusterRestConfig(empty path): expected nil config, got %#v", cfg)
	}
	// The error message must identify the root cause (AC4c marker).
	if !strings.Contains(err.Error(), "AC4c") {
		t.Fatalf("error %q does not contain AC4c marker", err.Error())
	}
}

// TestBuildClusterRestConfigWhitespacePath verifies that buildClusterRestConfig
// treats a whitespace-only path as empty and returns an error.
func TestBuildClusterRestConfigWhitespacePath(t *testing.T) {
	t.Parallel()

	cfg, err := buildClusterRestConfig("   ")
	if err == nil {
		t.Fatal("buildClusterRestConfig(whitespace path): expected error, got nil")
	}
	if cfg != nil {
		t.Fatalf("buildClusterRestConfig(whitespace path): expected nil config, got %#v", cfg)
	}
}

// TestBuildClusterRestConfigMissingFile verifies that buildClusterRestConfig
// returns an error when the kubeconfig path points to a file that does not
// exist, rather than silently succeeding with a default config.
func TestBuildClusterRestConfigMissingFile(t *testing.T) {
	t.Parallel()

	missingPath := os.TempDir() + "/pillar-csi-ac4c-does-not-exist-" + "xyz987.yaml"
	// Ensure it truly does not exist.
	_ = os.Remove(missingPath)
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("could not arrange missing file at %s: file exists and could not be removed", missingPath)
	}

	cfg, err := buildClusterRestConfig(missingPath)
	if err == nil {
		t.Fatal("buildClusterRestConfig(missing file): expected error, got nil")
	}
	if cfg != nil {
		t.Fatalf("buildClusterRestConfig(missing file): expected nil config, got %#v", cfg)
	}
}

// TestBuildClusterRestConfigKubeconfigUnderTmp verifies that the kubeconfig
// file written to /tmp satisfies the environment-hygiene constraint: the path
// returned by buildClusterRestConfig (and used by SuiteKubeRestConfig) stays
// strictly under os.TempDir().
func TestBuildClusterRestConfigKubeconfigUnderTmp(t *testing.T) {
	t.Parallel()

	kubeconfigPath := writeTmpKubeconfig(t, "ac4c-hygiene-", minimalKubeconfig)

	// Verify the path is under /tmp.
	if !strings.HasPrefix(kubeconfigPath, os.TempDir()) {
		t.Fatalf("kubeconfig path %q is not under %s (hygiene violation)", kubeconfigPath, os.TempDir())
	}

	// Verify buildClusterRestConfig works with this hygiene-compliant path.
	cfg, err := buildClusterRestConfig(kubeconfigPath)
	if err != nil {
		t.Fatalf("buildClusterRestConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("buildClusterRestConfig returned nil config")
	}
}

// TestSuiteKubeRestConfigReturnsCurrentVariable verifies that SuiteKubeRestConfig
// returns whatever suiteRestConfig currently holds.  This test overrides
// suiteRestConfig temporarily to confirm the accessor is wired to the correct
// package-level variable.
//
// Note: this test is NOT parallel because it mutates the package-level
// suiteRestConfig variable.
func TestSuiteKubeRestConfigReturnsCurrentVariable(t *testing.T) {
	// Snapshot and restore suiteRestConfig around this test.
	prev := suiteRestConfig
	t.Cleanup(func() { suiteRestConfig = prev })

	// Initially nil (outside a live suite).
	suiteRestConfig = nil
	if got := SuiteKubeRestConfig(); got != nil {
		t.Fatalf("SuiteKubeRestConfig() when nil = %#v, want nil", got)
	}

	// After the suite populates it, the accessor must reflect the new value.
	kubeconfigPath := writeTmpKubeconfig(t, "ac4c-accessor-", minimalKubeconfig)
	cfg, err := buildClusterRestConfig(kubeconfigPath)
	if err != nil {
		t.Fatalf("buildClusterRestConfig for accessor test: %v", err)
	}
	suiteRestConfig = cfg

	if got := SuiteKubeRestConfig(); got != cfg {
		t.Fatalf("SuiteKubeRestConfig() = %p, want %p", got, cfg)
	}
	if got := SuiteKubeRestConfig(); got == nil {
		t.Fatal("SuiteKubeRestConfig() returned nil after assignment")
	}
	if got := SuiteKubeRestConfig().Host; got == "" {
		t.Fatal("SuiteKubeRestConfig().Host is empty after assignment")
	}
}

// TestBuildClusterRestConfigHostMatchesKubeconfig verifies that the Host field
// in the returned rest.Config matches the server URL declared in the kubeconfig
// so that specs definitely connect to the intended cluster endpoint.
func TestBuildClusterRestConfigHostMatchesKubeconfig(t *testing.T) {
	t.Parallel()

	kubeconfigPath := writeTmpKubeconfig(t, "ac4c-host-", minimalKubeconfig)

	cfg, err := buildClusterRestConfig(kubeconfigPath)
	if err != nil {
		t.Fatalf("buildClusterRestConfig: %v", err)
	}

	// The minimal kubeconfig sets server: https://127.0.0.1:6443.
	// clientcmd normalises the URL; we only check the host:port portion.
	wantHost := "127.0.0.1:6443"
	if !strings.Contains(cfg.Host, wantHost) {
		t.Fatalf("rest.Config.Host = %q, want host containing %q", cfg.Host, wantHost)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// writeTmpKubeconfig writes content to a temporary kubeconfig file under
// os.TempDir(), registers its removal via t.Cleanup, and returns the path.
// The created file is always under /tmp, satisfying the environment-hygiene
// requirement (no side-effects outside /tmp).
func writeTmpKubeconfig(t *testing.T, prefix, content string) string {
	t.Helper()

	f, err := os.CreateTemp(os.TempDir(), prefix+"*.yaml")
	if err != nil {
		t.Fatalf("create temp kubeconfig: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("write temp kubeconfig: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp kubeconfig: %v", err)
	}

	return f.Name()
}
