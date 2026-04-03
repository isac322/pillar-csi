// Package prereq validates host prerequisites before any E2E tests run.
//
// AC 10 contract: every missing prerequisite (kernel module, binary tool, or
// Docker daemon) causes an IMMEDIATE FAIL with human-readable remediation
// instructions.  Soft-skipping is explicitly forbidden: the caller MUST call
// os.Exit(1) on any non-nil return from CheckHostPrerequisites.
//
// Design principles:
//   - Non-destructive: reads only /proc/modules and PATH; never modifies state
//   - No sudo required: all checks run as a non-root user
//   - DOCKER_HOST is always honoured; no hard-coded daemon paths
//   - ALL checks are hard failures — there are no optional/skippable checks
//   - "Never soft-skip" is a compile-time guarantee: no t.Skip or GinkgoSkip
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

// CheckHostPrerequisites validates that the host is fully ready for the
// pillar-csi E2E suite.
//
// Checks performed (all hard failures — no soft-skipping):
//  1. Docker daemon  — daemon reachable via DOCKER_HOST or default socket
//  2. Kernel modules — zfs, dm_thin_pool, nvme_tcp, nvmet, nvmet_tcp must ALL be loaded
//  3. Binary tools   — kind, helm, zfs, zpool, lvcreate, vgcreate
//
// Every missing item is a hard failure: the returned error lists all missing
// items with per-item remediation commands.
//
// AC 10 contract: this function never silently omits a check or degrades to a
// "partial" run.  All default-profile TCs (388 catalog + 7 E33 standalone + 9 other = 404)
// require the listed modules and tools; a missing item causes an immediate FAIL.
//
// Note: iscsi_tcp and iscsiadm are NOT checked here because iSCSI initiator
// functionality runs inside Kind container worker nodes (not on the host).
// The nvme binary is NOT checked here because F27-F31 NVMe-oF host-connect
// tests are not in the default profile and perform their own prereq checks.
func CheckHostPrerequisites() error {
	var errs []error

	if err := checkDockerDaemon(); err != nil {
		errs = append(errs, err)
	}

	if err := checkKernelModules(); err != nil {
		errs = append(errs, err)
	}

	if err := checkBinaries(); err != nil {
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
	sb.WriteString("\nFix ALL issues above and re-run: make test-e2e\n")
	sb.WriteString("\nAC 10: soft-skipping is DISABLED — every prerequisite must be\n")
	sb.WriteString("       present or the suite will not start.\n")
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
//
// AC 10: a non-reachable Docker daemon is a hard FAIL — no skip allowed.
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
			"docker daemon not reachable via %s: "+
				"error: %s; "+
				"remediation: start Docker (sudo systemctl start docker), "+
				"set DOCKER_HOST=unix:///var/run/docker.sock, "+
				"verify with docker info, "+
				"see https://docs.docker.com/engine/install/; "+
				"AC 10: Docker daemon is required (no soft-skip)",
			hint,
			strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// ─── Kernel modules ──────────────────────────────────────────────────────────

// kernelModule describes a kernel module required by the E2E suite.
// Every entry in requiredModules is a hard requirement — there are no optional
// modules. Missing ANY module causes a FAIL with remediation instructions.
type kernelModule struct {
	// name is the module name as it appears in /proc/modules (underscores, not
	// hyphens – the kernel normalises hyphens to underscores on load).
	name string
	// purpose is a one-line human description shown in error messages.
	purpose string
	// loadHint is the modprobe command a user can run to load the module.
	loadHint string
	// installHints are OS-specific package installation commands.
	installHints []string
}

// requiredModules lists every kernel module the E2E suite exercises.
//
// AC 10 policy: ALL modules are hard requirements.  The previous soft-skip
// semantics (required: false) have been removed because:
//
//  1. All 404 TCs must run locally by default with no capability gating.
//  2. "Never soft-skip" is a hard AC 10 constraint: missing modules cause
//     an immediate FAIL with clear remediation, not a silent SKIP.
//  3. The suite uses real ZFS, real LVM, real iSCSI, and real NVMe-oF TCP —
//     none of these can be emulated without the corresponding kernel module.
var requiredModules = []kernelModule{
	{
		name:     "zfs",
		purpose:  "ZFS backend — pools, datasets, and zvol block devices",
		loadHint: "sudo modprobe zfs",
		installHints: []string{
			"Ubuntu/Debian:  sudo apt install zfsutils-linux",
			"Fedora/RHEL:    sudo dnf install zfs  (after adding ZFS repo)",
			"Arch Linux:     sudo pacman -S zfs-dkms",
		},
	},
	{
		name:     "dm_thin_pool",
		purpose:  "LVM thin-pool backend — logical volume provisioning via device-mapper",
		loadHint: "sudo modprobe dm_thin_pool",
		installHints: []string{
			"Ubuntu/Debian:  sudo apt install lvm2 thin-provisioning-tools",
			"Fedora/RHEL:    sudo dnf install lvm2",
			"Arch Linux:     sudo pacman -S lvm2",
			"Note: dm_thin_pool is part of device-mapper-libs on all distros",
		},
	},
	{
		name:     "nvme_tcp",
		purpose:  "NVMe-oF TCP initiator — NVMe block devices over TCP/IP fabric (host/initiator side)",
		loadHint: "sudo modprobe nvme_tcp",
		installHints: []string{
			"Ubuntu/Debian:  sudo apt install nvme-cli linux-modules-extra-$(uname -r)",
			"Fedora/RHEL:    sudo dnf install nvme-cli",
			"Requirement:    kernel ≥ 5.0 with CONFIG_NVME_TCP=m",
		},
	},
	{
		name:     "nvmet",
		purpose:  "NVMe-oF target core — configfs interface at /sys/kernel/config/nvmet (Sub-AC 9b)",
		loadHint: "sudo modprobe nvmet",
		installHints: []string{
			"Ubuntu/Debian:  sudo apt install linux-modules-extra-$(uname -r)",
			"Fedora/RHEL:    sudo dnf install kernel-modules-extra",
			"Requirement:    kernel ≥ 4.19 with CONFIG_NVME_TARGET=m",
			"Verify:         ls /sys/kernel/config/nvmet/ after modprobe nvmet",
		},
	},
	{
		name:     "nvmet_tcp",
		purpose:  "NVMe-oF target TCP transport — accepts NVMe-oF TCP connections (Sub-AC 9b)",
		loadHint: "sudo modprobe nvmet_tcp",
		installHints: []string{
			"Ubuntu/Debian:  sudo apt install linux-modules-extra-$(uname -r)",
			"Fedora/RHEL:    sudo dnf install kernel-modules-extra",
			"Requirement:    kernel ≥ 5.0 with CONFIG_NVME_TARGET_TCP=m",
			"Note: load nvmet before nvmet_tcp",
		},
	},
}

// checkKernelModules reads /proc/modules and verifies that ALL required kernel
// modules are loaded.
//
// AC 10 contract: every missing module is a hard FAIL. Returns a single error
// that aggregates all missing modules with per-module remediation instructions.
//
// /proc/modules is world-readable on all Linux distributions — no elevated
// privileges are required.
func checkKernelModules() error {
	loaded, err := loadedKernelModules()
	if err != nil {
		return fmt.Errorf(
			"cannot read /proc/modules: %w\n"+
				"  Remediation: Ensure the test is running on a Linux host with /proc mounted\n"+
				"  AC 10: kernel module check is required — cannot proceed without /proc/modules",
			err,
		)
	}
	return checkKernelModulesFromSet(loaded)
}

// checkKernelModulesFromSet verifies that ALL required kernel modules appear
// in the provided set.  This function is separated from checkKernelModules so
// that unit tests can supply a synthetic module set without touching /proc.
//
// AC 10 contract: every missing module is a hard FAIL. Returns a single error
// listing all missing modules with per-module remediation instructions.
func checkKernelModulesFromSet(loaded map[string]struct{}) error {
	var missing []kernelModule
	for _, mod := range requiredModules {
		// Normalise hyphens to underscores: the kernel stores module names with
		// underscores regardless of how the user types them.
		target := strings.ReplaceAll(mod.name, "-", "_")
		if _, ok := loaded[target]; !ok {
			missing = append(missing, mod)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	// Build a human-readable error for every missing module.
	// AC 10: ALL listed modules are required — no soft-skip.
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d required kernel module(s) are not loaded:\n\n", len(missing))

	for _, mod := range missing {
		fmt.Fprintf(&sb, "   ✗ %-20s  %s\n", mod.name, mod.purpose)
		fmt.Fprintf(&sb, "       Load:    %s\n", mod.loadHint)
		if len(mod.installHints) > 0 {
			sb.WriteString("       Install:\n")
			for _, hint := range mod.installHints {
				fmt.Fprintf(&sb, "         • %s\n", hint)
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("  Remediation:\n")
	sb.WriteString("     • Load each missing module with the modprobe command shown above.\n")
	sb.WriteString("     • Install the corresponding kernel package for your distribution.\n")
	sb.WriteString("  Verify: lsmod | grep -E 'zfs|dm_thin|iscsi|nvme'\n")
	sb.WriteString("  AC 10: soft-skip is DISABLED — every module must be present or the suite FAILs.\n")

	return fmt.Errorf("%s", sb.String())
}

// loadedKernelModules returns the set of modules currently loaded in the
// running kernel by parsing /proc/modules. The map key is the normalised
// module name (hyphens converted to underscores, matching the kernel's own
// internal representation).
//
// Example /proc/modules line:
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

// ─── Binary tool checks ───────────────────────────────────────────────────────

// binaryCheck describes an external binary the E2E suite requires.
// Every entry in requiredBinaries is a hard requirement.
type binaryCheck struct {
	// binary is the executable name (looked up via PATH).
	binary string
	// purpose is a one-line human description.
	purpose string
	// installHints are OS-specific installation commands.
	installHints []string
}

// requiredBinaries lists every external CLI tool the E2E suite invokes.
//
// AC 10 policy: ALL binaries are hard requirements.  Missing ANY binary
// causes a FAIL with explicit installation instructions.  No capability gating
// or soft-skip is permitted.
var requiredBinaries = []binaryCheck{
	{
		binary:  "kind",
		purpose: "Kind — Kubernetes-in-Docker cluster management",
		installHints: []string{
			"go install sigs.k8s.io/kind@latest",
			"Or: curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64 && chmod +x /usr/local/bin/kind",
			"Docs: https://kind.sigs.k8s.io/docs/user/quick-start/#installation",
		},
	},
	{
		binary:  "helm",
		purpose: "Helm — Kubernetes package manager (chart installation and lifecycle)",
		installHints: []string{
			"curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash",
			"Or: https://helm.sh/docs/intro/install/",
		},
	},
	{
		binary:  "zfs",
		purpose: "ZFS userland tools — pool and dataset administration",
		installHints: []string{
			"Ubuntu/Debian: sudo apt install zfsutils-linux",
			"Fedora/RHEL:   sudo dnf install zfs  (after adding ZFS repo)",
		},
	},
	{
		binary:  "zpool",
		purpose: "ZFS pool management (part of zfsutils-linux)",
		installHints: []string{
			"Ubuntu/Debian: sudo apt install zfsutils-linux",
			"Note: zpool is bundled with zfsutils-linux alongside the zfs binary",
		},
	},
	{
		binary:  "lvcreate",
		purpose: "LVM — logical volume creation (lvm2 package)",
		installHints: []string{
			"Ubuntu/Debian: sudo apt install lvm2",
			"Fedora/RHEL:   sudo dnf install lvm2",
		},
	},
	{
		binary:  "vgcreate",
		purpose: "LVM — volume group creation (lvm2 package)",
		installHints: []string{
			"Ubuntu/Debian: sudo apt install lvm2",
			"Note: vgcreate is bundled with lvm2 alongside lvcreate and pvcreate",
		},
	},
}

// checkBinaries verifies that every required external binary is present in
// PATH.  Missing binaries are aggregated and returned as a single error with
// per-binary remediation instructions.
//
// AC 10 contract: returns a non-nil error if ANY binary is absent from PATH;
// never silently permits the suite to continue in a degraded state.
func checkBinaries() error {
	return checkBinariesWithLookup(exec.LookPath)
}

// checkBinariesWithLookup verifies required binaries using the provided PATH
// lookup function.  This indirection lets unit tests supply a synthetic lookup
// without modifying the real PATH.
//
// AC 10 contract: returns a non-nil error if ANY binary is absent; never
// silently permits the suite to continue in a degraded state.
func checkBinariesWithLookup(lookup func(string) (string, error)) error {
	var missing []binaryCheck
	for _, b := range requiredBinaries {
		if _, err := lookup(b.binary); err != nil {
			missing = append(missing, b)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d required binary tool(s) not found in PATH:\n\n", len(missing))
	for _, b := range missing {
		fmt.Fprintf(&sb, "   ✗ %-14s  %s\n", b.binary, b.purpose)
		if len(b.installHints) > 0 {
			sb.WriteString("       Install:\n")
			for _, hint := range b.installHints {
				fmt.Fprintf(&sb, "         • %s\n", hint)
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString("  Verify: which kind helm zfs zpool lvcreate vgcreate\n")
	sb.WriteString("  AC 10: ALL required tools must be in PATH — no soft-skip allowed.\n")
	return fmt.Errorf("%s", sb.String())
}
