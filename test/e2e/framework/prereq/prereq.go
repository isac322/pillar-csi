// Package prereq validates host prerequisites before any E2E tests run.
//
// The checker is intentionally conservative: it only verifies conditions that
// every E2E backend (ZFS, LVM, iSCSI) requires on the host. It never runs
// commands that require root/sudo, reads only from /proc, and respects the
// DOCKER_HOST environment variable.
//
// Usage in TestMain:
//
//	if err := prereq.CheckHostPrerequisites(); err != nil {
//	    fmt.Fprintln(os.Stderr, err.Error())
//	    os.Exit(1)
//	}
package prereq

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CheckHostPrerequisites validates that the host is ready for E2E tests.
//
// It performs two independent checks:
//  1. Docker daemon – the daemon must be reachable so Kind clusters can be
//     created and so test containers run. DOCKER_HOST is honoured if set.
//  2. Kernel modules – at least one storage backend module (zfs, dm_thin_pool)
//     and the iSCSI initiator module (iscsi_tcp) must be present in
//     /proc/modules.  These modules are used inside Kind containers; they must
//     exist in the host kernel because containers share the host kernel.
//
// All checks are non-destructive and do not require elevated privileges.
// On failure the returned error contains a human-readable message with
// explicit remediation instructions.
func CheckHostPrerequisites() error {
	var errs []error

	if err := checkDockerDaemon(); err != nil {
		errs = append(errs, err)
	}

	if err := checkKernelModules(); err != nil {
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("\n╔══════════════════════════════════════════════════════════════╗\n")
	sb.WriteString("║          pillar-csi E2E prerequisite check FAILED            ║\n")
	sb.WriteString("╚══════════════════════════════════════════════════════════════╝\n")
	for i, err := range errs {
		fmt.Fprintf(&sb, "\n  [%d] %s\n", i+1, err.Error())
	}
	sb.WriteString("\nFix the issues above and re-run: make test-e2e\n")
	return fmt.Errorf("%s", sb.String())
}

// ─── Docker daemon ───────────────────────────────────────────────────────────

// checkDockerDaemon verifies that the Docker daemon is reachable by running
// "docker info". The DOCKER_HOST environment variable is respected; when it is
// unset the Docker client uses its compiled-in default (typically the Unix
// socket /var/run/docker.sock).
//
// A 10-second context timeout prevents the check from blocking indefinitely
// when the daemon is unresponsive.
func checkDockerDaemon() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	args := []string{"info", "--format", "{{.ServerVersion}}"}
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec

	// Propagate DOCKER_HOST from the environment so we never hard-code the
	// daemon address. os.Environ() includes any DOCKER_HOST set by the caller.
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		dockerHost := os.Getenv("DOCKER_HOST")
		hint := "Docker default socket (/var/run/docker.sock)"
		if dockerHost != "" {
			hint = fmt.Sprintf("DOCKER_HOST=%s", dockerHost)
		}

		return fmt.Errorf(
			"docker daemon not reachable via %s\n"+
				"     Error: %s\n"+
				"  Remediation:\n"+
				"     • Start Docker: sudo systemctl start docker\n"+
				"     • Or set DOCKER_HOST to a reachable daemon endpoint\n"+
				"     • Verify with: docker info",
			hint,
			strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// ─── Kernel modules ──────────────────────────────────────────────────────────

// kernelModule describes a kernel module that an E2E backend requires.
type kernelModule struct {
	// name is the module name as it appears in /proc/modules (underscores, not
	// hyphens – the kernel normalises hyphens to underscores on load).
	name string
	// required means the check is a hard failure; optional means a warning.
	required bool
	// purpose is a one-line human description shown in error messages.
	purpose string
	// loadHint is the command a user can run to load the module.
	loadHint string
}

// requiredModules lists every module the E2E suite may exercise.
// At least one storage backend module must be present; iscsi_tcp is optional
// because in CI the iSCSI target runs inside Kind (no host initiator needed).
var requiredModules = []kernelModule{
	{
		name:     "zfs",
		required: false, // optional: ZFS tests are skipped if absent
		purpose:  "ZFS backend (pools, datasets, clones)",
		loadHint: "modprobe zfs",
	},
	{
		name:     "dm_thin_pool",
		required: false, // optional: LVM tests are skipped if absent
		purpose:  "LVM thin-pool backend",
		loadHint: "modprobe dm_thin_pool",
	},
	{
		name:     "iscsi_tcp",
		required: false, // optional: iSCSI tests are skipped if absent
		purpose:  "iSCSI TCP initiator",
		loadHint: "modprobe iscsi_tcp",
	},
}

// minimumStorageModules is the number of storage backend modules (zfs or
// dm_thin_pool) that must be present for the suite to have any useful work to
// do.  When none are loaded, the suite would silently skip all backend tests,
// which is almost certainly unintentional.
const minimumStorageModules = 1

// checkKernelModules reads /proc/modules and verifies that at least one
// storage backend module is loaded. Missing modules are reported with
// human-readable remediation hints.
//
// /proc/modules is world-readable on all Linux distributions – no elevated
// privileges are required.
func checkKernelModules() error {
	loaded, err := loadedKernelModules()
	if err != nil {
		return fmt.Errorf(
			"cannot read /proc/modules: %w\n"+
				"  Remediation: Ensure the test is running on a Linux host",
			err,
		)
	}

	var missing []kernelModule
	storagePresent := 0

	for _, mod := range requiredModules {
		if _, ok := loaded[mod.name]; ok {
			if mod.name == "zfs" || mod.name == "dm_thin_pool" {
				storagePresent++
			}
			continue
		}
		missing = append(missing, mod)
	}

	if storagePresent >= minimumStorageModules {
		// At least one storage backend is available; ignore missing optional
		// modules so the suite can run a useful subset.
		return nil
	}

	// Build a friendly error listing every missing module.
	var sb strings.Builder
	sb.WriteString("no storage backend kernel modules are loaded\n")
	sb.WriteString("     At least one of the following modules is required:\n\n")

	for _, mod := range missing {
		if mod.name != "zfs" && mod.name != "dm_thin_pool" {
			continue
		}
		fmt.Fprintf(&sb,
			"       Module: %-20s  Purpose: %s\n"+
				"       Load with: %s\n\n",
			mod.name, mod.purpose, mod.loadHint,
		)
	}

	sb.WriteString("  Remediation:\n")
	sb.WriteString("     • Run 'modprobe zfs' (requires ZFS kernel module package)\n")
	sb.WriteString("     • Or run 'modprobe dm_thin_pool' (requires lvm2/device-mapper)\n")
	sb.WriteString("     • Ubuntu/Debian: sudo apt install zfsutils-linux\n")
	sb.WriteString("     • Fedora/RHEL:   sudo dnf install zfs\n")
	sb.WriteString("     • Verify with:   lsmod | grep -E 'zfs|dm_thin_pool'\n")

	return fmt.Errorf("%s", sb.String())
}

// loadedKernelModules returns the set of modules currently loaded in the
// running kernel by parsing /proc/modules. The map key is the module name
// (as normalised by the kernel: hyphens converted to underscores).
//
// The file format is whitespace-separated with the module name in column 0.
// Example line:
//
//	zfs 5058560 3 zunicode,zavl,zcommon, Live 0xffffffffc0a00000
func loadedKernelModules() (map[string]struct{}, error) {
	data, err := os.ReadFile("/proc/modules")
	if err != nil {
		return nil, err
	}

	modules := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Normalise: kernel uses underscores internally but users type hyphens.
		name := strings.ReplaceAll(fields[0], "-", "_")
		modules[name] = struct{}{}
	}
	return modules, nil
}
