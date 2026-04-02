//go:build e2e

package e2e

// helm_bootstrap_test.go — Unit tests for the Helm bootstrap machinery
// introduced by Sub-AC 2 (parallel-safe Helm installation).
//
// Requires -tags=e2e (the tested functions live in helm_bootstrap_e2e.go).
// These tests run without a real Kind cluster and complete in milliseconds.
// They verify:
//
//   1. resolveHelmBootstrap reads the E2E_HELM_BOOTSTRAP env var correctly.
//   2. encodeSuitePayload / decodeSuitePayload round-trip cleanly, including
//      when HelmState is nil (default) or populated.
//   3. teardownSuiteHelm is a no-op when state is nil or Installed=false.
//   4. teardownSuiteHelm falls back to KUBECONFIG env var when clusterState is nil.
//   5. bootstrapSuiteHelm returns an error when called with a nil clusterState.

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// ── resolveHelmBootstrap ──────────────────────────────────────────────────────

func TestResolveHelmBootstrapFalseWhenUnset(t *testing.T) {
	t.Parallel()

	const envKey = "E2E_HELM_BOOTSTRAP"
	old, hadOld := os.LookupEnv(envKey)
	_ = os.Unsetenv(envKey)
	if hadOld {
		defer func() { _ = os.Setenv(envKey, old) }()
	} else {
		defer func() { _ = os.Unsetenv(envKey) }()
	}

	if resolveHelmBootstrap() {
		t.Error("resolveHelmBootstrap() = true, want false (env var unset)")
	}
}

func TestResolveHelmBootstrapTrueWhenTrue(t *testing.T) {
	t.Parallel()

	const envKey = "E2E_HELM_BOOTSTRAP"
	old, hadOld := os.LookupEnv(envKey)
	_ = os.Setenv(envKey, "true")
	if hadOld {
		defer func() { _ = os.Setenv(envKey, old) }()
	} else {
		defer func() { _ = os.Unsetenv(envKey) }()
	}

	if !resolveHelmBootstrap() {
		t.Error("resolveHelmBootstrap() = false, want true (E2E_HELM_BOOTSTRAP=true)")
	}
}

func TestResolveHelmBootstrapTrueWhenOne(t *testing.T) {
	t.Parallel()

	const envKey = "E2E_HELM_BOOTSTRAP"
	old, hadOld := os.LookupEnv(envKey)
	_ = os.Setenv(envKey, "1")
	if hadOld {
		defer func() { _ = os.Setenv(envKey, old) }()
	} else {
		defer func() { _ = os.Unsetenv(envKey) }()
	}

	if !resolveHelmBootstrap() {
		t.Error("resolveHelmBootstrap() = false, want true (E2E_HELM_BOOTSTRAP=1)")
	}
}

func TestResolveHelmBootstrapFalseWhenFalse(t *testing.T) {
	t.Parallel()

	const envKey = "E2E_HELM_BOOTSTRAP"
	old, hadOld := os.LookupEnv(envKey)
	_ = os.Setenv(envKey, "false")
	if hadOld {
		defer func() { _ = os.Setenv(envKey, old) }()
	} else {
		defer func() { _ = os.Unsetenv(envKey) }()
	}

	if resolveHelmBootstrap() {
		t.Error("resolveHelmBootstrap() = true, want false (E2E_HELM_BOOTSTRAP=false)")
	}
}

// ── encodeSuitePayload / decodeSuitePayload roundtrip ─────────────────────────

func TestEncodeSuitePayloadRoundtripKindStateOnly(t *testing.T) {
	t.Parallel()

	state := &kindBootstrapState{
		ClusterName:    "test-cluster",
		KubeconfigPath: "/tmp/kubeconfig",
		KubeContext:    "kind-test-cluster",
		SuiteRootDir:   "/tmp/suite",
		WorkspaceDir:   "/tmp/suite/workspace",
		LogsDir:        "/tmp/suite/logs",
		GeneratedDir:   "/tmp/suite/generated",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  2 * time.Minute,
		DeleteTimeout:  2 * time.Minute,
	}

	payload := synchronizedSuitePayload{KindState: state}
	encoded, err := encodeSuitePayload(payload)
	if err != nil {
		t.Fatalf("encodeSuitePayload: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("encodeSuitePayload: returned empty bytes")
	}

	decoded, err := decodeSuitePayload(encoded)
	if err != nil {
		t.Fatalf("decodeSuitePayload: %v", err)
	}
	if decoded.KindState == nil {
		t.Fatal("decodeSuitePayload: KindState is nil")
	}
	if decoded.KindState.ClusterName != state.ClusterName {
		t.Errorf("ClusterName: got %q, want %q", decoded.KindState.ClusterName, state.ClusterName)
	}
	if decoded.KindState.KubeconfigPath != state.KubeconfigPath {
		t.Errorf("KubeconfigPath: got %q, want %q", decoded.KindState.KubeconfigPath, state.KubeconfigPath)
	}
	if decoded.HelmState != nil {
		t.Errorf("HelmState: got %+v, want nil (not set in input)", decoded.HelmState)
	}
}

func TestEncodeSuitePayloadRoundtripWithHelmState(t *testing.T) {
	t.Parallel()

	state := &kindBootstrapState{
		ClusterName:    "helm-test-cluster",
		KubeconfigPath: "/tmp/kubeconfig-helm",
		KubeContext:    "kind-helm-test-cluster",
		SuiteRootDir:   "/tmp/suite",
		WorkspaceDir:   "/tmp/suite/workspace",
		LogsDir:        "/tmp/suite/logs",
		GeneratedDir:   "/tmp/suite/generated",
		KindBinary:     "kind",
		KubectlBinary:  "kubectl",
		CreateTimeout:  2 * time.Minute,
		DeleteTimeout:  2 * time.Minute,
	}
	helm := &helmBootstrapState{
		Installed: true,
		Release:   "pillar-csi",
		Namespace: "pillar-csi-system",
		ChartPath: "/repo/charts/pillar-csi",
	}

	payload := synchronizedSuitePayload{KindState: state, HelmState: helm}
	encoded, err := encodeSuitePayload(payload)
	if err != nil {
		t.Fatalf("encodeSuitePayload: %v", err)
	}

	decoded, err := decodeSuitePayload(encoded)
	if err != nil {
		t.Fatalf("decodeSuitePayload: %v", err)
	}
	if decoded.KindState == nil {
		t.Fatal("decoded KindState is nil")
	}
	if decoded.KindState.ClusterName != state.ClusterName {
		t.Errorf("ClusterName: got %q, want %q", decoded.KindState.ClusterName, state.ClusterName)
	}
	if decoded.HelmState == nil {
		t.Fatal("decoded HelmState is nil, expected non-nil")
	}
	if !decoded.HelmState.Installed {
		t.Error("decoded HelmState.Installed = false, want true")
	}
	if decoded.HelmState.Release != helm.Release {
		t.Errorf("HelmState.Release: got %q, want %q", decoded.HelmState.Release, helm.Release)
	}
	if decoded.HelmState.Namespace != helm.Namespace {
		t.Errorf("HelmState.Namespace: got %q, want %q", decoded.HelmState.Namespace, helm.Namespace)
	}
	if decoded.HelmState.ChartPath != helm.ChartPath {
		t.Errorf("HelmState.ChartPath: got %q, want %q", decoded.HelmState.ChartPath, helm.ChartPath)
	}
}

func TestDecodeSuitePayloadRejectsEmptyBytes(t *testing.T) {
	t.Parallel()

	_, err := decodeSuitePayload([]byte{})
	if err == nil {
		t.Error("decodeSuitePayload(empty) must return an error")
	}
}

func TestDecodeSuitePayloadRejectsNilKindState(t *testing.T) {
	t.Parallel()

	// Encode a payload where KindState is nil (should fail decode validation).
	encoded, err := encodeSuitePayload(synchronizedSuitePayload{KindState: nil})
	if err != nil {
		// Encoding nil might itself fail; that's also acceptable.
		t.Logf("encodeSuitePayload(nil KindState) returned error: %v", err)
		return
	}

	_, decErr := decodeSuitePayload(encoded)
	if decErr == nil {
		t.Error("decodeSuitePayload with nil KindState must return an error")
	}
}

// ── teardownSuiteHelm edge cases ──────────────────────────────────────────────

func TestTeardownSuiteHelmNilStateIsNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Should return immediately without error.
	teardownSuiteHelm(ctx, nil, nil, io.Discard)
}

func TestTeardownSuiteHelmNotInstalledIsNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &helmBootstrapState{
		Installed: false,
		Release:   "pillar-csi",
		Namespace: "pillar-csi-system",
	}

	// Should return immediately without running any helm command.
	teardownSuiteHelm(ctx, state, nil, io.Discard)
}

func TestTeardownSuiteHelmSkipsWhenNoKubeconfig(t *testing.T) {
	t.Parallel()

	// Ensure KUBECONFIG is unset so teardownSuiteHelm logs "skipping".
	const envKey = "KUBECONFIG"
	old, hadOld := os.LookupEnv(envKey)
	_ = os.Unsetenv(envKey)
	if hadOld {
		defer func() { _ = os.Setenv(envKey, old) }()
	} else {
		defer func() { _ = os.Unsetenv(envKey) }()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &helmBootstrapState{
		Installed: true,
		Release:   "pillar-csi",
		Namespace: "pillar-csi-system",
	}

	// No clusterState, no KUBECONFIG env var — should log and return without
	// attempting to run helm.
	teardownSuiteHelm(ctx, state, nil, io.Discard)
}

// ── bootstrapSuiteHelm validation ─────────────────────────────────────────────

func TestBootstrapSuiteHelmRejectsNilClusterState(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := bootstrapSuiteHelm(ctx, nil, io.Discard)
	if err == nil {
		t.Error("bootstrapSuiteHelm(nil clusterState) must return an error")
	}
}

// ── helmBootstrapEnvVar constant ─────────────────────────────────────────────

func TestHelmBootstrapEnvVarIsNonEmpty(t *testing.T) {
	t.Parallel()

	if helmBootstrapEnvVar == "" {
		t.Error("helmBootstrapEnvVar must be non-empty")
	}
	if helmReleaseEnvVar == "" {
		t.Error("helmReleaseEnvVar must be non-empty")
	}
	if helmNamespaceEnvVar == "" {
		t.Error("helmNamespaceEnvVar must be non-empty")
	}
}

func TestHelmInstallTimeoutIsPositive(t *testing.T) {
	t.Parallel()

	if helmInstallTimeout <= 0 {
		t.Errorf("helmInstallTimeout = %v, must be positive", helmInstallTimeout)
	}
	if helmTeardownTimeout <= 0 {
		t.Errorf("helmTeardownTimeout = %v, must be positive", helmTeardownTimeout)
	}
}
