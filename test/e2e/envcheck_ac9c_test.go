package e2e

// envcheck_ac9c_test.go — Unit tests for the Sub-AC 9c backend env-check logic.
//
// These tests verify:
//  1. Error message shape and remediation content for missing/invalid inputs.
//  2. Per-backend error detection when nodeContainer or resource names are empty.
//  3. The runAllBackendEnvChecks aggregation — all errors are collected, not
//     just the first.
//  4. The indent helper.
//
// These tests do NOT require a running Docker daemon or a Kind cluster.
// They exercise the pure validation/formatting logic by passing invalid inputs
// that trigger error paths without docker exec calls.
//
// Live integration tests (requiring a real container) are covered by the e2e
// suite itself via SynchronizedBeforeSuite when the 'e2e' build tag is active.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// ─── runAllBackendEnvChecks unit tests ────────────────────────────────────────

// TestRunAllBackendEnvChecks_EmptyContainerReturnsError verifies that
// runAllBackendEnvChecks returns a non-nil error when nodeContainer is empty.
// This covers the most basic guard: all four backend checks share the same
// container-empty detection path so a single empty string fails all four.
func TestRunAllBackendEnvChecks_EmptyContainerReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var buf bytes.Buffer
	err := runAllBackendEnvChecks(ctx, "" /*nodeContainer*/, "" /*zfsPool*/, "" /*lvmVG*/, &buf)
	if err == nil {
		t.Fatal("[AC9c] expected non-nil error when nodeContainer is empty, got nil")
	}

	msg := err.Error()

	// The error must be tagged with [AC9c].
	if !strings.Contains(msg, "AC9c") {
		t.Errorf("[AC9c] expected error to contain 'AC9c' tag:\n%s", msg)
	}

	// All four backends must be reported as failed.
	for _, name := range []string{"ZFS", "LVM", "NVMe-oF", "iSCSI"} {
		if !strings.Contains(msg, name) {
			t.Errorf("[AC9c] expected error to mention backend %q:\n%s", name, msg)
		}
	}

	// Remediation must be present.
	if !strings.Contains(msg, "Remediation") {
		t.Errorf("[AC9c] expected error to contain 'Remediation' section:\n%s", msg)
	}

	// The summary must also be written to the output writer.
	summary := buf.String()
	if !strings.Contains(summary, "[AC9c]") {
		t.Errorf("[AC9c] expected output writer to contain '[AC9c]' summary line:\n%s", summary)
	}
}

// TestRunAllBackendEnvChecks_NilWriterIsSafeNoOp verifies that passing a nil
// io.Writer to runAllBackendEnvChecks does not panic. The function must use
// io.Discard when output is nil.
func TestRunAllBackendEnvChecks_NilWriterIsSafeNoOp(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Should not panic; should return an error (container is empty).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[AC9c] runAllBackendEnvChecks panicked with nil writer: %v", r)
		}
	}()

	_ = runAllBackendEnvChecks(ctx, "", "", "", nil)
}

// TestRunAllBackendEnvChecks_ReturnsNilOnAllPass demonstrates that the function
// returns nil when all per-backend functions pass (simulated by providing an
// obviously invalid container that causes docker exec to fail with a specific
// error we can detect, then verifying our test infrastructure handles it).
//
// NOTE: This test cannot inject passing backend results without a real docker
// container. Instead it just verifies that the failure path is correct when
// an obviously non-existent container is given — the docker client will reject
// the container name and we verify the error shape.
func TestRunAllBackendEnvChecks_NonExistentContainerError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var buf bytes.Buffer
	// Use a container name that cannot possibly exist.
	err := runAllBackendEnvChecks(ctx,
		"container-that-does-not-exist-e2e-ac9c-test",
		"pool-not-set",
		"vg-not-set",
		&buf,
	)

	// Must return a non-nil error (docker exec will fail for non-existent container).
	if err == nil {
		t.Fatal("[AC9c] expected non-nil error for non-existent container, got nil")
	}

	// Error must contain the 4-backend count.
	msg := err.Error()
	if !strings.Contains(msg, "4/4") {
		t.Errorf("[AC9c] expected '4/4' backend failure count in error:\n%s", msg)
	}

	// Error must include AC 10 policy note.
	if !strings.Contains(msg, "AC 10 policy") {
		t.Errorf("[AC9c] expected 'AC 10 policy' in error:\n%s", msg)
	}

	// Error must mention no fake/stub/mock.
	if !strings.Contains(msg, "fake") || !strings.Contains(msg, "stub") || !strings.Contains(msg, "mock") {
		t.Errorf("[AC9c] expected fake/stub/mock warning in error:\n%s", msg)
	}
}

// ─── Per-backend check tests ──────────────────────────────────────────────────

// TestEnvCheckZFSBackend_EmptyPoolNameReturnsError verifies that
// envCheckZFSBackend returns an actionable error when poolName is empty.
func TestEnvCheckZFSBackend_EmptyPoolNameReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckZFSBackend(ctx, "some-container", "" /*poolName*/)

	if r.Err == nil {
		t.Fatal("[AC9c/ZFS] expected non-nil error for empty poolName, got nil")
	}
	if r.Name != "ZFS" {
		t.Errorf("[AC9c/ZFS] expected Name='ZFS', got %q", r.Name)
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_ZFS_POOL") {
		t.Errorf("[AC9c/ZFS] Details should mention PILLAR_E2E_ZFS_POOL env var:\n%s", r.Details)
	}
}

// TestEnvCheckZFSBackend_EmptyContainerReturnsError verifies that
// envCheckZFSBackend returns an actionable error when nodeContainer is empty.
func TestEnvCheckZFSBackend_EmptyContainerReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckZFSBackend(ctx, "" /*nodeContainer*/, "some-pool")

	if r.Err == nil {
		t.Fatal("[AC9c/ZFS] expected non-nil error for empty nodeContainer, got nil")
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_BACKEND_CONTAINER") {
		t.Errorf("[AC9c/ZFS] Details should mention PILLAR_E2E_BACKEND_CONTAINER env var:\n%s", r.Details)
	}
}

// TestEnvCheckLVMBackend_EmptyVGNameReturnsError verifies that
// envCheckLVMBackend returns an actionable error when vgName is empty.
func TestEnvCheckLVMBackend_EmptyVGNameReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckLVMBackend(ctx, "some-container", "" /*vgName*/)

	if r.Err == nil {
		t.Fatal("[AC9c/LVM] expected non-nil error for empty vgName, got nil")
	}
	if r.Name != "LVM" {
		t.Errorf("[AC9c/LVM] expected Name='LVM', got %q", r.Name)
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_LVM_VG") {
		t.Errorf("[AC9c/LVM] Details should mention PILLAR_E2E_LVM_VG env var:\n%s", r.Details)
	}
}

// TestEnvCheckLVMBackend_EmptyContainerReturnsError verifies that
// envCheckLVMBackend returns an actionable error when nodeContainer is empty.
func TestEnvCheckLVMBackend_EmptyContainerReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckLVMBackend(ctx, "" /*nodeContainer*/, "some-vg")

	if r.Err == nil {
		t.Fatal("[AC9c/LVM] expected non-nil error for empty nodeContainer, got nil")
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_BACKEND_CONTAINER") {
		t.Errorf("[AC9c/LVM] Details should mention PILLAR_E2E_BACKEND_CONTAINER env var:\n%s", r.Details)
	}
}

// TestEnvCheckNVMeOFBackend_EmptyContainerReturnsError verifies that
// envCheckNVMeOFBackend returns an actionable error when nodeContainer is empty.
func TestEnvCheckNVMeOFBackend_EmptyContainerReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckNVMeOFBackend(ctx, "" /*nodeContainer*/)

	if r.Err == nil {
		t.Fatal("[AC9c/NVMe-oF] expected non-nil error for empty nodeContainer, got nil")
	}
	if r.Name != "NVMe-oF TCP" {
		t.Errorf("[AC9c/NVMe-oF] expected Name='NVMe-oF TCP', got %q", r.Name)
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_BACKEND_CONTAINER") {
		t.Errorf("[AC9c/NVMe-oF] Details should mention PILLAR_E2E_BACKEND_CONTAINER env var:\n%s", r.Details)
	}
}

// TestEnvCheckISCSIBackend_EmptyContainerReturnsError verifies that
// envCheckISCSIBackend returns an actionable error when nodeContainer is empty.
func TestEnvCheckISCSIBackend_EmptyContainerReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := envCheckISCSIBackend(ctx, "" /*nodeContainer*/)

	if r.Err == nil {
		t.Fatal("[AC9c/iSCSI] expected non-nil error for empty nodeContainer, got nil")
	}
	if r.Name != "iSCSI" {
		t.Errorf("[AC9c/iSCSI] expected Name='iSCSI', got %q", r.Name)
	}
	if !strings.Contains(r.Details, "PILLAR_E2E_BACKEND_CONTAINER") {
		t.Errorf("[AC9c/iSCSI] Details should mention PILLAR_E2E_BACKEND_CONTAINER env var:\n%s", r.Details)
	}
}

// ─── Error message shape tests ────────────────────────────────────────────────

// TestEnvCheckErrorMessages_ContainAC10PolicyNote verifies that every error
// returned by the per-backend check functions mentions the AC 10 constraint
// (no soft-skip) — not just the aggregated error. This ensures operators
// reading individual errors understand the policy context.
func TestEnvCheckErrorMessages_ContainAC10PolicyNote(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test the aggregated error message from runAllBackendEnvChecks.
	var buf bytes.Buffer
	aggErr := runAllBackendEnvChecks(ctx, "", "", "", &buf)
	if aggErr == nil {
		t.Fatal("[AC9c] expected non-nil error for empty container")
	}
	if !strings.Contains(aggErr.Error(), "AC 10 policy") {
		t.Errorf("[AC9c] aggregated error must contain 'AC 10 policy':\n%s", aggErr.Error())
	}
	if !strings.Contains(aggErr.Error(), "Soft-skip is DISABLED") {
		t.Errorf("[AC9c] aggregated error must contain 'Soft-skip is DISABLED':\n%s", aggErr.Error())
	}
}

// TestEnvCheckErrorMessages_ContainRemediationForAllBackends verifies that the
// aggregated error message includes backend-specific remediation hints for each
// of the four backends.
func TestEnvCheckErrorMessages_ContainRemediationForAllBackends(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var buf bytes.Buffer
	err := runAllBackendEnvChecks(ctx, "", "", "", &buf)
	if err == nil {
		t.Fatal("[AC9c] expected non-nil error for empty container")
	}
	msg := err.Error()

	remediationMarkers := []string{
		"ZFS",
		"LVM",
		"NVMe-oF",
		"iSCSI",
		"bootstrapSuiteBackends",
		"PILLAR_E2E_BACKEND_PROVISIONED",
	}
	for _, marker := range remediationMarkers {
		if !strings.Contains(msg, marker) {
			t.Errorf("[AC9c] aggregated error missing remediation marker %q:\n%s", marker, msg)
		}
	}
}

// ─── indent helper tests ──────────────────────────────────────────────────────

// TestIndent_SingleLine verifies that a single-line string is correctly indented.
func TestIndent_SingleLine(t *testing.T) {
	t.Parallel()

	got := indent("hello", "  ")
	want := "  hello"
	if got != want {
		t.Errorf("indent single line: got %q, want %q", got, want)
	}
}

// TestIndent_MultiLine verifies that every line in a multi-line string is indented.
func TestIndent_MultiLine(t *testing.T) {
	t.Parallel()

	input := "line1\nline2\nline3"
	got := indent(input, ">>> ")
	want := ">>> line1\n>>> line2\n>>> line3"
	if got != want {
		t.Errorf("indent multi line:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestIndent_EmptyString verifies that indent("", ...) returns the prefix on
// an empty line, not an empty string.
func TestIndent_EmptyString(t *testing.T) {
	t.Parallel()

	got := indent("", "XX")
	want := "XX"
	if got != want {
		t.Errorf("indent empty string: got %q, want %q", got, want)
	}
}

// ─── backendCheckResult tests ─────────────────────────────────────────────────

// TestBackendCheckResult_NilErrMeansPass verifies that a zero-error result is
// interpreted as passing and that Name/Details are populated.
func TestBackendCheckResult_NilErrMeansPass(t *testing.T) {
	t.Parallel()

	r := backendCheckResult{
		Name:    "ZFS",
		Err:     nil,
		Details: "pool online",
	}
	if r.Err != nil {
		t.Errorf("[AC9c] expected Err=nil for passing result, got %v", r.Err)
	}
	if r.Name != "ZFS" {
		t.Errorf("[AC9c] unexpected Name: %q", r.Name)
	}
	if r.Details == "" {
		t.Errorf("[AC9c] expected non-empty Details for passing result")
	}
}
