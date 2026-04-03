package kind_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	kindhelper "github.com/bhyoo/pillar-csi/test/e2e/framework/kind"
)

// ─── CheckFabricKernelModules tests ───────────────────────────────────────────

// TestCheckFabricKernelModules_OnLinuxHost verifies that CheckFabricKernelModules
// returns nil when the required fabric modules (nvmet, nvmet_tcp) are present,
// or returns an actionable error message when they are absent.
//
// This test is informational on hosts where the modules are absent; the
// Sub-AC 9b contract is that CheckFabricKernelModules returns a non-nil error
// (not a skip) when modules are missing.
func TestCheckFabricKernelModules_OnLinuxHost(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err != nil {
		t.Skip("/proc/modules not found — not a Linux host; skipping fabric module check test")
	}

	err := kindhelper.CheckFabricKernelModules()
	if err == nil {
		t.Log("CheckFabricKernelModules: all required fabric modules present (nvmet, nvmet_tcp)")
		return
	}

	// Modules are missing: verify the error message is actionable.
	msg := err.Error()

	// The error must contain the canonical fabric-module header.
	if !strings.Contains(msg, "pillar-csi E2E fabric kernel modules MISSING") {
		t.Errorf("CheckFabricKernelModules error missing expected header:\ngot:\n%s", msg)
	}

	// The error must mention the missing module name(s).
	hasFabricModule := strings.Contains(msg, "nvmet") || strings.Contains(msg, "nvmet_tcp")
	if !hasFabricModule {
		t.Errorf("CheckFabricKernelModules error does not name any missing fabric module:\ngot:\n%s", msg)
	}

	// The error must provide remediation: at least one modprobe command.
	if !strings.Contains(msg, "modprobe") {
		t.Errorf("CheckFabricKernelModules error missing modprobe remediation:\ngot:\n%s", msg)
	}

	// The error must mention that soft-skip is disabled.
	if !strings.Contains(msg, "Soft-skip is DISABLED") {
		t.Errorf("CheckFabricKernelModules error missing soft-skip-disabled notice:\ngot:\n%s", msg)
	}

	t.Logf("CheckFabricKernelModules returned (expected on machines without nvmet):\n%s", msg)
}

// TestCheckFabricKernelModules_ErrorMessageShape verifies the shape of the error
// returned when fabric modules are absent.  This test directly inspects
// /proc/modules and calls the function only when at least one module is absent.
func TestCheckFabricKernelModules_ErrorMessageShape(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err != nil {
		t.Skip("/proc/modules not found — not a Linux host")
	}

	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		t.Fatalf("read /proc/modules: %v", err)
	}
	content := string(data)

	nvmetLoaded := strings.Contains(content, "nvmet ")
	nvmetTCPLoaded := strings.Contains(content, "nvmet_tcp ")

	if nvmetLoaded && nvmetTCPLoaded {
		t.Skip("both nvmet and nvmet_tcp modules are loaded — cannot test missing-module error path")
	}

	// At least one fabric module is missing; error must be non-nil.
	err = kindhelper.CheckFabricKernelModules()
	if err == nil {
		t.Fatal("expected CheckFabricKernelModules to return an error when a fabric module is missing, got nil")
	}

	msg := err.Error()
	t.Logf("CheckFabricKernelModules error:\n%s", msg)

	// Verify error shape invariants.
	invariants := []struct {
		description string
		check       func(string) bool
	}{
		{"contains fabric MISSING header", func(s string) bool {
			return strings.Contains(s, "pillar-csi E2E fabric kernel modules MISSING")
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
		{"mentions nvmet module", func(s string) bool {
			return strings.Contains(s, "nvmet")
		}},
	}

	for _, inv := range invariants {
		if !inv.check(msg) {
			t.Errorf("error message missing: %s\ngot:\n%s", inv.description, msg)
		}
	}
}

// TestCheckFabricKernelModules_NonLinux verifies that on non-Linux hosts
// CheckFabricKernelModules returns an informative error.
func TestCheckFabricKernelModules_NonLinux(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat("/proc/modules"); err == nil {
		t.Skip("/proc/modules exists — this is a Linux host; skipping non-Linux path test")
	}

	err := kindhelper.CheckFabricKernelModules()
	if err == nil {
		t.Fatal("expected an error on non-Linux host (no /proc/modules), got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "/proc/modules") && !strings.Contains(msg, "Linux") {
		t.Errorf("non-Linux error message should mention /proc/modules or Linux:\ngot:\n%s", msg)
	}
}

// ─── Fabric DaemonSet constants tests ─────────────────────────────────────────

// TestFabricDaemonSetConstants verifies that the exported constants have the
// expected values so that callers can hard-code them in label selectors and
// kubectl commands without fear of silent mismatches.
func TestFabricDaemonSetConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "FabricDaemonSetName",
			got:  kindhelper.FabricDaemonSetName,
			want: "pillar-csi-fabric-readiness",
		},
		{
			name: "FabricDaemonSetNamespace",
			got:  kindhelper.FabricDaemonSetNamespace,
			want: "kube-system",
		},
		{
			name: "NVMeOFSubsystemNQN",
			got:  kindhelper.NVMeOFSubsystemNQN,
			want: "nqn.2024-01.io.pillar-csi:e2e-target",
		},
		{
			name: "NVMeOFTCPPort",
			got:  kindhelper.NVMeOFTCPPort,
			want: "4420",
		},
		{
			name: "ISCSITargetIQN",
			got:  kindhelper.ISCSITargetIQN,
			want: "iqn.2024-01.io.pillar-csi:e2e-target",
		},
		{
			name: "ISCSITargetTID",
			got:  kindhelper.ISCSITargetTID,
			want: "10",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("%s = %q; want %q", tc.name, tc.got, tc.want)
			}
		})
	}

	if kindhelper.FabricDaemonSetReadyTimeout <= 0 {
		t.Errorf("FabricDaemonSetReadyTimeout must be positive, got %v",
			kindhelper.FabricDaemonSetReadyTimeout)
	}
}

// ─── DeployFabricReadinessDaemonSet input validation tests ───────────────────

// TestDeployFabricReadinessDaemonSet_EmptyKubeconfigReturnsError verifies that
// DeployFabricReadinessDaemonSet returns an error immediately when the
// kubeconfigPath is empty, without attempting to call kubectl.
func TestDeployFabricReadinessDaemonSet_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.DeployFabricReadinessDaemonSet(t.Context(), "", "kubectl")
	if err == nil {
		t.Error("DeployFabricReadinessDaemonSet(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestDeployFabricReadinessDaemonSet_EmptyKubeconfigDefaultsBinary verifies that
// an empty kubectlBinary still returns the kubeconfig error (not a binary error),
// confirming that the kubeconfigPath check precedes the binary defaulting.
func TestDeployFabricReadinessDaemonSet_EmptyKubeconfigDefaultsBinary(t *testing.T) {
	t.Parallel()

	// Both empty — the kubeconfig check should fire first.
	err := kindhelper.DeployFabricReadinessDaemonSet(t.Context(), "", "")
	if err == nil {
		t.Error("DeployFabricReadinessDaemonSet(\"\", \"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("expected kubeconfigPath error first, got: %v", err)
	}
}

// ─── WaitForFabricDaemonSetReady input validation tests ──────────────────────

// TestWaitForFabricDaemonSetReady_EmptyKubeconfigReturnsError verifies that
// WaitForFabricDaemonSetReady returns an error immediately when kubeconfigPath
// is empty.
func TestWaitForFabricDaemonSetReady_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.WaitForFabricDaemonSetReady(t.Context(), "", "kubectl", 0)
	if err == nil {
		t.Error("WaitForFabricDaemonSetReady(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ─── RemoveFabricReadinessDaemonSet input validation tests ───────────────────

// TestRemoveFabricReadinessDaemonSet_EmptyKubeconfigReturnsError verifies that
// RemoveFabricReadinessDaemonSet returns an error when kubeconfigPath is empty.
func TestRemoveFabricReadinessDaemonSet_EmptyKubeconfigReturnsError(t *testing.T) {
	t.Parallel()

	err := kindhelper.RemoveFabricReadinessDaemonSet(t.Context(), "", "kubectl")
	if err == nil {
		t.Error("RemoveFabricReadinessDaemonSet(\"\") should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "kubeconfigPath must not be empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// ─── NQN / IQN format tests ───────────────────────────────────────────────────

// TestFabricNQNAndIQNFormats verifies that the exported NQN and IQN constants
// follow the correct format specifications:
//   - NQN must start with "nqn."
//   - IQN must start with "iqn."
func TestFabricNQNAndIQNFormats(t *testing.T) {
	t.Parallel()

	if !strings.HasPrefix(kindhelper.NVMeOFSubsystemNQN, "nqn.") {
		t.Errorf("NVMeOFSubsystemNQN %q does not start with 'nqn.'", kindhelper.NVMeOFSubsystemNQN)
	}

	if !strings.HasPrefix(kindhelper.ISCSITargetIQN, "iqn.") {
		t.Errorf("ISCSITargetIQN %q does not start with 'iqn.'", kindhelper.ISCSITargetIQN)
	}
}

// TestFabricNVMeOFPortIsNumeric verifies that NVMeOFTCPPort is a valid
// numeric port string in the range [1, 65535].
func TestFabricNVMeOFPortIsNumeric(t *testing.T) {
	t.Parallel()

	var port int
	if _, err := fmt.Sscanf(kindhelper.NVMeOFTCPPort, "%d", &port); err != nil {
		t.Errorf("NVMeOFTCPPort %q is not a valid integer: %v", kindhelper.NVMeOFTCPPort, err)
		return
	}
	if port < 1 || port > 65535 {
		t.Errorf("NVMeOFTCPPort %d is outside valid TCP port range [1, 65535]", port)
	}
}
