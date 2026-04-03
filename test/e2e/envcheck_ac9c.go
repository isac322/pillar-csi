package e2e

// envcheck_ac9c.go — Sub-AC 9c: test-suite-level EnvCheck that asserts all
// four real storage backends are reachable and functional before any TC runs.
//
// # AC 9c Contract
//
// runAllBackendEnvChecks is called from the all-nodes phase of
// SynchronizedBeforeSuite (kind_bootstrap_e2e_test.go) on EVERY parallel
// Ginkgo worker. If any backend is absent or replaced by a stub/mock, the
// function returns a non-nil error that causes Ginkgo to Fail the entire suite
// before a single TC is scheduled.
//
// # Why "no stub/mock" enforcement?
//
// The project constraint is: ALL 416 TCs must run against real backends.
// Soft-skip is explicitly forbidden (AC 10). This function verifies reachability
// AND functionality — a backend that is present but non-functional (e.g. a pool
// in a degraded state, an LVM VG with a missing PV, a configfs directory that
// exists but has no TCP port) fails the same as a completely absent backend.
//
// # Four backend checks
//
//  1. ZFS  — "docker exec <container> zpool list -H -o name,health <pool>"
//             Pool must be ONLINE.
//
//  2. LVM  — "docker exec <container> vgs --noheadings -o vg_name,vg_attr <vg>"
//             VG must be writeable (vg_attr contains 'w').
//
//  3. NVMe-oF TCP — checks configfs at /sys/kernel/config/nvmet/subsystems/<NQN>
//             and /sys/kernel/config/nvmet/ports/1 inside the container.
//             The NQN and port must have been created by the fabric DaemonSet
//             (DeployFabricReadinessDaemonSet in framework/kind/fabric_daemonset.go).
//
//  4. iSCSI — "docker exec <container> tgtadm --lld iscsi --mode target --op show"
//             must succeed and contain the E2E target IQN (ISCSITargetIQN),
//             proving that tgtd is running and the target was created by the
//             fabric DaemonSet.
//
// # Stub/mock detection
//
// A stub backend would NOT create a real ZFS pool, LVM VG, configfs entry, or
// iSCSI target inside the container. Therefore, passing all four checks proves
// that the suite is backed by real kernel-level storage — not in-process fakes.
//
// # Output
//
// All check results (PASS and FAIL) are written to GinkgoWriter so they appear
// in the suite output regardless of verbosity level. This provides an audit
// trail that lets operators confirm which backends were verified before TCs ran.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	kindhelper "github.com/bhyoo/pillar-csi/test/e2e/framework/kind"
)

// ─── Result type ──────────────────────────────────────────────────────────────

// backendCheckResult holds the outcome of a single backend reachability check.
// Both passing and failing results carry a Details string for GinkgoWriter output.
type backendCheckResult struct {
	// Name is the human-readable backend identifier printed in check summaries.
	Name string

	// Err is non-nil when the backend is absent, non-functional, or its state
	// suggests a stub/mock is in use instead of a real backend.
	Err error

	// Details is a brief description of what was verified (or what failed).
	// Always non-empty — populated even on error to provide context.
	Details string
}

// ─── Entry point ──────────────────────────────────────────────────────────────

// runAllBackendEnvChecks asserts that all four real storage backends (ZFS, LVM,
// NVMe-oF TCP, iSCSI) are reachable and functional inside the Kind container.
//
// Parameters:
//   - ctx           — timeout context (caller should use ≤60 s for fast-fail)
//   - nodeContainer — Docker container name of the Kind control-plane node
//   - zfsPool       — ZFS pool name from PILLAR_E2E_ZFS_POOL env var
//   - lvmVG         — LVM Volume Group name from PILLAR_E2E_LVM_VG env var
//   - output        — io.Writer for the check summary (pass GinkgoWriter or os.Stderr)
//
// Returns nil when all four backends pass. Returns a non-nil error listing ALL
// failing backends when any check fails. The caller (Ginkgo
// SynchronizedBeforeSuite) calls Expect(err).NotTo(HaveOccurred()) to abort
// the suite on any failure.
//
// All check results — pass AND fail — are written to output so that CI logs
// show the full backend verification summary regardless of verbosity level.
func runAllBackendEnvChecks(
	ctx context.Context,
	nodeContainer, zfsPool, lvmVG string,
	output io.Writer,
) error {
	if output == nil {
		output = io.Discard
	}

	results := []backendCheckResult{
		envCheckZFSBackend(ctx, nodeContainer, zfsPool),
		envCheckLVMBackend(ctx, nodeContainer, lvmVG),
		envCheckNVMeOFBackend(ctx, nodeContainer),
		envCheckISCSIBackend(ctx, nodeContainer),
	}

	// ── Write the combined result summary to the output writer ────────────────

	_, _ = fmt.Fprintln(output, "\n[AC9c] Backend env-check summary:")

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			_, _ = fmt.Fprintf(output, "  [FAIL] %-14s : %s\n", r.Name, r.Details)
		} else {
			_, _ = fmt.Fprintf(output, "  [OK]   %-14s : %s\n", r.Name, r.Details)
		}
	}

	if failed == 0 {
		_, _ = fmt.Fprintln(output, "\n[AC9c] All four backends verified — NO fake/stub/mock detected.")
		return nil
	}

	// ── Build the failure error ───────────────────────────────────────────────

	var sb strings.Builder
	fmt.Fprintf(&sb, "[AC9c] %d/%d backend(s) FAILED env check:\n\n", failed, len(results))
	for _, r := range results {
		if r.Err != nil {
			fmt.Fprintf(&sb, "  ✗ %s\n    Error  : %v\n    Details: %s\n\n",
				r.Name, r.Err, r.Details)
		}
	}

	sb.WriteString("Remediation:\n")
	sb.WriteString("  • bootstrapSuiteBackends must complete before SynchronizedBeforeSuite runs.\n")
	sb.WriteString("  • PILLAR_E2E_BACKEND_PROVISIONED must equal \"1\" in the worker environment.\n")
	sb.WriteString("  • For ZFS: 'docker exec <node> zpool list' must show the pool as ONLINE.\n")
	sb.WriteString("  • For LVM: 'docker exec <node> vgs' must show the VG as writable.\n")
	sb.WriteString("  • For NVMe-oF: DeployFabricReadinessDaemonSet must have run successfully;\n")
	sb.WriteString("      check '/sys/kernel/config/nvmet/subsystems/<NQN>' exists in the container.\n")
	sb.WriteString("  • For iSCSI: DeployFabricReadinessDaemonSet must have started tgtd;\n")
	sb.WriteString("      run 'docker exec <node> tgtadm --lld iscsi --mode target --op show'.\n")
	sb.WriteString("\nAC 10 policy: NO fake/stub/mock backends — every backend must be real.\n")
	sb.WriteString("              Soft-skip is DISABLED; fix ALL issues above.\n")

	return fmt.Errorf("%s", sb.String())
}

// ─── Per-backend checks ───────────────────────────────────────────────────────

// envCheckZFSBackend verifies that the ZFS pool is provisioned and ONLINE inside
// the Kind container. A pool in any state other than ONLINE (DEGRADED, FAULTED,
// OFFLINE, SUSPENDED, REMOVED) fails the check.
//
// The check uses "zpool list -H -o name,health <pool>" which outputs a single
// tab-separated line: "<poolname>\t<health>" (e.g. "pillar-e2e-zfs-abc12345\tONLINE").
// This is the authoritative indicator that ZFS pool I/O is fully functional.
func envCheckZFSBackend(ctx context.Context, nodeContainer, poolName string) backendCheckResult {
	r := backendCheckResult{Name: "ZFS"}

	if nodeContainer == "" {
		r.Details = "nodeContainer empty — PILLAR_E2E_BACKEND_CONTAINER not set"
		r.Err = fmt.Errorf("[AC9c/ZFS] %s", r.Details)
		return r
	}
	if poolName == "" {
		r.Details = "pool name empty — PILLAR_E2E_ZFS_POOL not set; " +
			"bootstrapSuiteBackends failed or 'zfs' kernel module not loaded"
		r.Err = fmt.Errorf("[AC9c/ZFS] %s", r.Details)
		return r
	}

	// ── Probe 1: zpool list ───────────────────────────────────────────────────
	//
	// "zpool list -H -o name,health <pool>" prints one tab-separated line:
	//   "<name>\t<health>"
	// Health values: ONLINE, DEGRADED, FAULTED, OFFLINE, SUSPENDED, REMOVED.
	// Only ONLINE means the pool is fully functional.
	out, err := containerExecForEnvCheck(ctx, nodeContainer,
		"zpool", "list", "-H", "-o", "name,health", poolName)
	if err != nil {
		r.Details = fmt.Sprintf("zpool list failed — pool %q may not exist in container %s",
			poolName, nodeContainer)
		r.Err = fmt.Errorf("[AC9c/ZFS] %s: %w\n"+
			"  Hint: verify with 'docker exec %s zpool list %s'",
			r.Details, err, nodeContainer, poolName)
		return r
	}

	fields := strings.Fields(out)
	if len(fields) < 2 {
		r.Details = fmt.Sprintf("unexpected zpool list output %q (expected '<name> <health>')", out)
		r.Err = fmt.Errorf("[AC9c/ZFS] %s", r.Details)
		return r
	}
	health := strings.ToUpper(fields[1])
	if health != "ONLINE" {
		r.Details = fmt.Sprintf("pool %q health=%q (want ONLINE) — real ZFS backend issue detected", poolName, health)
		r.Err = fmt.Errorf("[AC9c/ZFS] %s\n"+
			"  Hint: check 'docker exec %s zpool status %s' for degradation cause",
			r.Details, nodeContainer, poolName)
		return r
	}

	// ── Probe 2: zfs list ─────────────────────────────────────────────────────
	//
	// A stub backend would not create real ZFS datasets/zvols. A successful
	// "zfs list -H -r -o name <pool>" proves the pool has real backing storage.
	listOut, listErr := containerExecForEnvCheck(ctx, nodeContainer,
		"zfs", "list", "-H", "-r", "-o", "name", poolName)
	if listErr != nil {
		r.Details = fmt.Sprintf("pool %q ONLINE but 'zfs list' failed: %v", poolName, listErr)
		r.Err = fmt.Errorf("[AC9c/ZFS] %s", r.Details)
		return r
	}

	entries := len(strings.Split(strings.TrimSpace(listOut), "\n"))
	r.Details = fmt.Sprintf("pool %q ONLINE; zfs list returned %d dataset(s)", poolName, entries)
	return r
}

// envCheckLVMBackend verifies that the LVM Volume Group is provisioned and
// writeable inside the Kind container.
//
// The check uses "vgs --noheadings -o vg_name,vg_attr <vg>" which outputs a
// line like "  pillar-e2e-lvm-abc12345   wz--n-". The vg_attr field is a
// six-character string: position 0 is the VG permissions ('w' = writable).
// A writeable VG proves the underlying PV (loop device) is accessible.
func envCheckLVMBackend(ctx context.Context, nodeContainer, vgName string) backendCheckResult {
	r := backendCheckResult{Name: "LVM"}

	if nodeContainer == "" {
		r.Details = "nodeContainer empty — PILLAR_E2E_BACKEND_CONTAINER not set"
		r.Err = fmt.Errorf("[AC9c/LVM] %s", r.Details)
		return r
	}
	if vgName == "" {
		r.Details = "VG name empty — PILLAR_E2E_LVM_VG not set; " +
			"bootstrapSuiteBackends failed or 'dm_thin_pool' kernel module not loaded"
		r.Err = fmt.Errorf("[AC9c/LVM] %s", r.Details)
		return r
	}

	// ── Probe: vgs ────────────────────────────────────────────────────────────
	//
	// "vgs --noheadings -o vg_name,vg_attr <vg>" outputs one line per VG:
	//   "  <name>   <attr>"
	// vg_attr position 0: 'w' = writeable, 'r' = read-only.
	out, err := containerExecForEnvCheck(ctx, nodeContainer,
		"vgs", "--noheadings", "-o", "vg_name,vg_attr", vgName)
	if err != nil {
		r.Details = fmt.Sprintf("vgs failed — VG %q may not exist in container %s",
			vgName, nodeContainer)
		r.Err = fmt.Errorf("[AC9c/LVM] %s: %w\n"+
			"  Hint: verify with 'docker exec %s vgs %s'",
			r.Details, err, nodeContainer, vgName)
		return r
	}

	fields := strings.Fields(out)
	if len(fields) < 2 {
		r.Details = fmt.Sprintf("unexpected vgs output %q (expected '<name> <attr>')", out)
		r.Err = fmt.Errorf("[AC9c/LVM] %s", r.Details)
		return r
	}
	attr := fields[1]
	if len(attr) == 0 || attr[0] != 'w' {
		r.Details = fmt.Sprintf("VG %q has vg_attr=%q (position 0 must be 'w' for writable)", vgName, attr)
		r.Err = fmt.Errorf("[AC9c/LVM] %s\n"+
			"  Hint: 'docker exec %s vgdisplay %s' for full VG status",
			r.Details, nodeContainer, vgName)
		return r
	}

	r.Details = fmt.Sprintf("VG %q writable (attr=%q)", vgName, attr)
	return r
}

// envCheckNVMeOFBackend verifies that the NVMe-oF TCP target is configured in
// the kernel's configfs inside the Kind container.
//
// The fabric DaemonSet (DeployFabricReadinessDaemonSet) creates:
//   - /sys/kernel/config/nvmet/subsystems/<NVMeOFSubsystemNQN>/
//   - /sys/kernel/config/nvmet/ports/1/
//   - Symlink: /sys/kernel/config/nvmet/ports/1/subsystems/<NVMeOFSubsystemNQN>
//
// This function verifies all three are present, proving:
//  1. The nvmet kernel module is loaded (configfs base dir exists).
//  2. The subsystem was created (no stub would populate configfs).
//  3. The TCP port is configured (port 1 dir exists).
//  4. The subsystem is linked to the port (the symlink enables TCP connectivity).
func envCheckNVMeOFBackend(ctx context.Context, nodeContainer string) backendCheckResult {
	r := backendCheckResult{Name: "NVMe-oF TCP"}

	if nodeContainer == "" {
		r.Details = "nodeContainer empty — PILLAR_E2E_BACKEND_CONTAINER not set"
		r.Err = fmt.Errorf("[AC9c/NVMe-oF] %s", r.Details)
		return r
	}

	nvmetBase := "/sys/kernel/config/nvmet"
	nqn := kindhelper.NVMeOFSubsystemNQN
	subsysPath := nvmetBase + "/subsystems/" + nqn
	portPath := nvmetBase + "/ports/1"
	symlinkPath := portPath + "/subsystems/" + nqn

	// ── Probe 1: configfs base ────────────────────────────────────────────────
	//
	// /sys/kernel/config/nvmet/ exists only when the 'nvmet' kernel module is
	// loaded AND configfs is mounted. A stub cannot fake this.
	if _, err := containerExecForEnvCheck(ctx, nodeContainer,
		"test", "-d", nvmetBase); err != nil {
		r.Details = fmt.Sprintf("configfs nvmet base %s not found — "+
			"'nvmet' module may not be loaded or configfs not mounted", nvmetBase)
		r.Err = fmt.Errorf("[AC9c/NVMe-oF] %s\n"+
			"  Remediation: 'sudo modprobe nvmet nvmet_tcp' on the host\n"+
			"  Verify: 'docker exec %s ls %s'",
			r.Details, nodeContainer, nvmetBase)
		return r
	}

	// ── Probe 2: subsystem NQN directory ─────────────────────────────────────
	//
	// The fabric DaemonSet creates the subsystem directory. Its absence means
	// either the DaemonSet failed or a stub replaced the real target setup.
	if _, err := containerExecForEnvCheck(ctx, nodeContainer,
		"test", "-d", subsysPath); err != nil {
		r.Details = fmt.Sprintf("NVMe-oF subsystem %q not found at %s", nqn, subsysPath)
		r.Err = fmt.Errorf("[AC9c/NVMe-oF] %s\n"+
			"  Hint: DeployFabricReadinessDaemonSet may not have run or failed\n"+
			"  Verify: 'docker exec %s ls %s'",
			r.Details, nodeContainer, nvmetBase+"/subsystems/")
		return r
	}

	// ── Probe 3: TCP port directory ───────────────────────────────────────────
	//
	// Port 1 must exist to accept TCP connections on 0.0.0.0:4420.
	if _, err := containerExecForEnvCheck(ctx, nodeContainer,
		"test", "-d", portPath); err != nil {
		r.Details = fmt.Sprintf("NVMe-oF TCP port 1 not found at %s", portPath)
		r.Err = fmt.Errorf("[AC9c/NVMe-oF] %s\n"+
			"  Hint: fabric DaemonSet port configuration may have failed",
			r.Details)
		return r
	}

	// ── Probe 4: subsystem→port symlink ──────────────────────────────────────
	//
	// The symlink connects the subsystem to the port, enabling TCP connectivity.
	// Without this link, the NVMe-oF target is configured but not accessible.
	if _, err := containerExecForEnvCheck(ctx, nodeContainer,
		"test", "-e", symlinkPath); err != nil {
		r.Details = fmt.Sprintf("NVMe-oF subsystem→port symlink missing at %s", symlinkPath)
		r.Err = fmt.Errorf("[AC9c/NVMe-oF] %s\n"+
			"  Hint: subsystem is created but not linked to the TCP port; "+
			"fabric DaemonSet symlink step may have failed",
			r.Details)
		return r
	}

	r.Details = fmt.Sprintf("configfs: subsystem %q linked to port 1", nqn)
	return r
}

// envCheckISCSIBackend verifies that the iSCSI target daemon (tgtd) is running
// inside the Kind container and the E2E target IQN is present.
//
// The fabric DaemonSet (DeployFabricReadinessDaemonSet) installs tgt and starts
// tgtd with the E2E target (ISCSITargetIQN). This function checks:
//  1. tgtadm binary is present (tgt package installed).
//  2. tgtd responds to tgtadm queries (daemon is running).
//  3. The E2E target IQN appears in tgtadm output (target was created, not stubbed).
func envCheckISCSIBackend(ctx context.Context, nodeContainer string) backendCheckResult {
	r := backendCheckResult{Name: "iSCSI"}

	if nodeContainer == "" {
		r.Details = "nodeContainer empty — PILLAR_E2E_BACKEND_CONTAINER not set"
		r.Err = fmt.Errorf("[AC9c/iSCSI] %s", r.Details)
		return r
	}

	iqn := kindhelper.ISCSITargetIQN

	// ── Probe 1: tgtadm present ───────────────────────────────────────────────
	//
	// tgtadm is installed by the fabric DaemonSet. Its absence means the
	// DaemonSet did not run or the installation step failed.
	if _, err := containerExecForEnvCheck(ctx, nodeContainer,
		"which", "tgtadm"); err != nil {
		r.Details = fmt.Sprintf("tgtadm not found in container %s — tgt package not installed", nodeContainer)
		r.Err = fmt.Errorf("[AC9c/iSCSI] %s\n"+
			"  Hint: DeployFabricReadinessDaemonSet installs 'tgt'; "+
			"verify DaemonSet completed\n"+
			"  Verify: 'docker exec %s which tgtadm'",
			r.Details, nodeContainer)
		return r
	}

	// ── Probe 2: tgtd running + E2E target present ────────────────────────────
	//
	// "tgtadm --lld iscsi --mode target --op show" lists all iSCSI targets.
	// It exits non-zero if tgtd is not running.
	// A real backend created by the fabric DaemonSet will contain the E2E IQN;
	// a stub cannot fake tgtd output (tgtadm communicates with the actual daemon
	// via a Unix socket inside the container).
	out, err := containerExecForEnvCheck(ctx, nodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "target", "--op", "show")
	if err != nil {
		r.Details = fmt.Sprintf("tgtadm failed in container %s — tgtd may not be running", nodeContainer)
		r.Err = fmt.Errorf("[AC9c/iSCSI] %s: %w\n"+
			"  Hint: verify with 'docker exec %s tgtadm --lld iscsi --mode target --op show'",
			r.Details, err, nodeContainer)
		return r
	}

	if !strings.Contains(out, iqn) {
		r.Details = fmt.Sprintf("E2E iSCSI target %q not found in tgtadm output", iqn)
		r.Err = fmt.Errorf("[AC9c/iSCSI] %s\n"+
			"  tgtadm output:\n%s\n"+
			"  Hint: fabric DaemonSet may not have created the E2E target\n"+
			"  Verify: 'docker exec %s tgtadm --lld iscsi --mode target --op show | grep %s'",
			r.Details, indent(out, "    "), nodeContainer, iqn)
		return r
	}

	// Count target entries for the audit log.
	targetCount := strings.Count(out, "Target ")
	r.Details = fmt.Sprintf("tgtd reachable; E2E target %q present (%d total target(s))",
		iqn, targetCount)
	return r
}

// ─── Helper: container exec ───────────────────────────────────────────────────

// containerExecForEnvCheck runs a read-only command inside a Docker container
// via "docker exec" for env-check purposes.
//
// DOCKER_HOST is forwarded automatically from the calling process's environment
// (cmd.Env = os.Environ()) — never hardcoded.
//
// This function is intentionally separate from the provisioner-level
// kindContainerExec so that env-check failures are clearly tagged [AC9c] and
// not confused with provisioner errors.
//
// Returns (stdout, nil) on success, or ("", error) on non-zero exit.
func containerExecForEnvCheck(ctx context.Context, container string, args ...string) (string, error) {
	if strings.TrimSpace(container) == "" {
		return "", fmt.Errorf("containerExecForEnvCheck: container name must not be empty")
	}

	dockerArgs := append([]string{"exec", container}, args...)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...) //nolint:gosec

	// Propagate DOCKER_HOST from the parent environment. os.Environ() includes
	// DOCKER_HOST when set; otherwise docker falls back to its default socket.
	// The endpoint is NEVER hardcoded here.
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = strings.TrimSpace(stdout.String())
		}
		if errText == "" {
			errText = err.Error()
		}
		return "", fmt.Errorf("%s", errText)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ─── Helper: formatting ───────────────────────────────────────────────────────

// indent prepends a fixed prefix to every line in s.
// Used to indent multi-line command output inside error messages.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
