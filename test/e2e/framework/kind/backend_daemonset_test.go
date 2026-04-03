package kind_test

import (
	"os"
	"strings"
	"testing"

	kindhelper "github.com/bhyoo/pillar-csi/test/e2e/framework/kind"
)

// ─── CheckBackendKernelModules tests ──────────────────────────────────────────

// TestCheckBackendKernelModules_OnLinuxWithModules verifies that
// CheckBackendKernelModules returns nil when the required modules are present.
// This test is skipped on non-Linux hosts (no /proc/modules).
func TestCheckBackendKernelModules_OnLinuxWithModules(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err != nil {
		t.Skip("/proc/modules not found — not a Linux host; skipping module check test")
	}

	err := kindhelper.CheckBackendKernelModules()
	if err == nil {
		// All required modules are present — nothing more to verify.
		t.Log("CheckBackendKernelModules: all required modules present (zfs, dm_thin_pool)")
		return
	}

	// If the error is non-nil, it means one or more modules are missing.
	// This is NOT a test failure (the modules may legitimately be absent on the
	// test host), but we verify that the error message is actionable.
	msg := err.Error()

	// The error must contain the canonical header.
	if !strings.Contains(msg, "pillar-csi E2E backend kernel modules MISSING") {
		t.Errorf("CheckBackendKernelModules error missing expected header:\ngot:\n%s", msg)
	}

	// The error must mention the missing module name(s).
	if !strings.Contains(msg, "zfs") && !strings.Contains(msg, "dm_thin_pool") {
		t.Errorf("CheckBackendKernelModules error does not name any missing module:\ngot:\n%s", msg)
	}

	// The error must provide remediation: at least one modprobe command.
	if !strings.Contains(msg, "modprobe") {
		t.Errorf("CheckBackendKernelModules error missing modprobe remediation:\ngot:\n%s", msg)
	}

	// The error must mention that soft-skip is disabled.
	if !strings.Contains(msg, "Soft-skip is DISABLED") {
		t.Errorf("CheckBackendKernelModules error missing soft-skip-disabled notice:\ngot:\n%s", msg)
	}

	t.Logf("CheckBackendKernelModules returned (expected on machines without ZFS/LVM):\n%s", msg)
}

// TestCheckBackendKernelModules_ErrorMessageShape verifies that the error
// returned when modules are missing has the correct shape — even when we cannot
// guarantee the modules are absent.
//
// We test the error-formatting path directly by reading /proc/modules and
// confirming the module is absent before calling the function.
func TestCheckBackendKernelModules_ErrorMessageShape(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err != nil {
		t.Skip("/proc/modules not found — not a Linux host")
	}

	// Read the real /proc/modules to check whether the required modules
	// are actually present.  If both are loaded, skip — we cannot test the
	// error path without them being absent.
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		t.Fatalf("read /proc/modules: %v", err)
	}
	content := string(data)

	zfsLoaded := strings.Contains(content, "zfs ")
	lvmLoaded := strings.Contains(content, "dm_thin_pool ")

	if zfsLoaded && lvmLoaded {
		t.Skip("both zfs and dm_thin_pool modules are loaded — cannot test missing-module error path")
	}

	// At least one module is missing; calling CheckBackendKernelModules should
	// return a non-nil error.
	err = kindhelper.CheckBackendKernelModules()
	if err == nil {
		t.Fatal("expected CheckBackendKernelModules to return an error when a module is missing, got nil")
	}

	msg := err.Error()
	t.Logf("CheckBackendKernelModules error:\n%s", msg)

	// Verify error shape invariants.
	invariants := []struct {
		description string
		check       func(string) bool
	}{
		{"contains MISSING header", func(s string) bool {
			return strings.Contains(s, "pillar-csi E2E backend kernel modules MISSING")
		}},
		{"mentions Soft-skip is DISABLED", func(s string) bool {
			return strings.Contains(s, "Soft-skip is DISABLED")
		}},
		{"contains modprobe command", func(s string) bool {
			return strings.Contains(s, "modprobe")
		}},
		{"contains install hint", func(s string) bool {
			return strings.Contains(s, "apt install") || strings.Contains(s, "dnf install")
		}},
		{"mentions make test-e2e", func(s string) bool {
			return strings.Contains(s, "make test-e2e")
		}},
	}

	for _, inv := range invariants {
		if !inv.check(msg) {
			t.Errorf("error message missing: %s\ngot:\n%s", inv.description, msg)
		}
	}
}

// TestCheckBackendKernelModules_NonLinux verifies that on non-Linux hosts (where
// /proc/modules is absent) the function returns an informative error rather
// than panicking or returning nil.
func TestCheckBackendKernelModules_NonLinux(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err == nil {
		t.Skip("/proc/modules exists — this is a Linux host; skipping non-Linux path test")
	}

	err := kindhelper.CheckBackendKernelModules()
	if err == nil {
		t.Fatal("expected an error on non-Linux host (no /proc/modules), got nil")
	}

	msg := err.Error()
	// The error should mention /proc/modules or Linux, not just a generic message.
	if !strings.Contains(msg, "/proc/modules") && !strings.Contains(msg, "Linux") {
		t.Errorf("non-Linux error message should mention /proc/modules or Linux:\ngot:\n%s", msg)
	}
}

// ─── DaemonSet manifest rendering tests ───────────────────────────────────────

// TestBackendDaemonSetConstants verifies that the exported constants for
// the DaemonSet have the expected values so that callers can hard-code them
// in label selectors and kubectl commands without fear of silent mismatches.
func TestBackendDaemonSetConstants(t *testing.T) {
	t.Parallel()

	if kindhelper.BackendDaemonSetName != "pillar-csi-backend-readiness" {
		t.Errorf("BackendDaemonSetName = %q; want %q",
			kindhelper.BackendDaemonSetName, "pillar-csi-backend-readiness")
	}
	if kindhelper.BackendDaemonSetNamespace != "kube-system" {
		t.Errorf("BackendDaemonSetNamespace = %q; want %q",
			kindhelper.BackendDaemonSetNamespace, "kube-system")
	}
	if kindhelper.BackendDaemonSetReadyTimeout <= 0 {
		t.Errorf("BackendDaemonSetReadyTimeout must be positive, got %v",
			kindhelper.BackendDaemonSetReadyTimeout)
	}
}

// ─── DeployBackendReadinessDaemonSet input validation tests ───────────────────

// TestDeployBackendReadinessDaemonSet_EmptyKubeconfigReturnsError verifies that
// DeployBackendReadinessDaemonSet returns an error immediately when the
// kubeconfigPath is empty, without attempting to call kubectl.
func TestDeployBackendReadinessDaemonSet_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.DeployBackendReadinessDaemonSet(t.Context(), "", "kubectl")
	if err == nil {
		t.Error("DeployBackendReadinessDaemonSet(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ─── WaitForBackendDaemonSetReady input validation tests ──────────────────────

// TestWaitForBackendDaemonSetReady_EmptyKubeconfigReturnsError verifies that
// WaitForBackendDaemonSetReady returns an error immediately when kubeconfigPath
// is empty.
func TestWaitForBackendDaemonSetReady_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.WaitForBackendDaemonSetReady(t.Context(), "", "kubectl", 0)
	if err == nil {
		t.Error("WaitForBackendDaemonSetReady(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ─── RemoveBackendReadinessDaemonSet input validation tests ───────────────────

// TestRemoveBackendReadinessDaemonSet_EmptyKubeconfigReturnsError verifies that
// RemoveBackendReadinessDaemonSet returns an error when kubeconfigPath is empty.
func TestRemoveBackendReadinessDaemonSet_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.RemoveBackendReadinessDaemonSet(t.Context(), "", "kubectl")
	if err == nil {
		t.Error("RemoveBackendReadinessDaemonSet(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}
