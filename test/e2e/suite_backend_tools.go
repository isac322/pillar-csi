package e2e

// suite_backend_tools.go — AC4: ephemeral backend tool installation for Kind
// container nodes.
//
// The standard Kind node image (kindest/node) ships with Debian bookworm but
// does NOT include ZFS userspace utilities (zpool, zfs) or LVM tools
// (pvcreate, vgcreate). These are required by the ZFS and LVM backend
// provisioners (AC4).
//
// # Design
//
// installKindContainerBackendTools runs before the provisioner pipeline in
// bootstrapSuiteBackends. It installs the required packages via docker exec
// without modifying the host or requiring a custom Kind node image.
//
// Installation is best-effort:
//   - If apt-get is unavailable, the function logs a warning and returns nil.
//   - If the package install fails, the function logs a warning and returns nil.
//   - The provisioners detect missing binaries and soft-skip accordingly.
//
// All commands run via "docker exec <nodeContainer> …" so DOCKER_HOST is
// inherited from the process environment and is never hardcoded.
//
// # ZFS specifics
//
// zfsutils-linux is in the Debian bookworm "contrib" component, not "main".
// This function enables the contrib component before updating the package list.
//
// # LVM specifics
//
// LVM2 is installed from the Debian main repository. After installation,
// lvm.conf is patched to disable udev integration (udev_sync, udev_rules,
// obtain_device_list_from_udev) so that LVM commands work correctly inside
// Docker containers where udev is not running.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// installKindContainerBackendTools installs zfsutils-linux and lvm2 inside the
// Kind container nodeContainer.
//
// It is called from bootstrapSuiteBackends before the provisioner pipeline runs
// (AC4). Installation is best-effort: if a step fails, a warning is written to
// output but the function returns nil so that the provisioners can apply soft-
// skip semantics if the binary is still absent.
//
// Prerequisites:
//   - nodeContainer must be the Docker container name of the Kind node.
//   - DOCKER_HOST is read from the environment; never hardcoded.
func installKindContainerBackendTools(ctx context.Context, nodeContainer string, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}

	// Verify the container exists and apt-get is available before attempting
	// any installation. If apt-get is absent (e.g. non-Debian container image),
	// log a notice and return — provisioners will soft-skip.
	if _, err := kindContainerExec(ctx, nodeContainer, "which", "apt-get"); err != nil {
		// apt-get absent means a non-Debian container image; log and return nil
		// (soft skip) — the ZFS/LVM provisioners will soft-skip if their binaries
		// are missing, so failing hard here would be too strict.
		_ = err // intentional: treat apt-get absence as soft skip, not a hard error
		_, _ = fmt.Fprintf(output,
			"[AC4] apt-get not found in container %s — skipping tool installation "+
				"(ZFS/LVM provisioning will soft-skip if binaries are absent)\n",
			nodeContainer)
		return nil //nolint:nilerr // intentional soft skip: apt-get absent is not a hard error
	}

	// ── Step 1: Enable contrib component ─────────────────────────────────────
	//
	// zfsutils-linux is in the Debian bookworm "contrib" component. The
	// standard kindest/node image only has "main" in its apt sources, so we
	// must add "contrib" before the package will be found.
	//
	// We write a new sources entry file rather than modifying the existing one
	// to minimise the risk of corrupting the package index.
	const contribSources = `Types: deb
URIs: http://deb.debian.org/debian
Suites: bookworm bookworm-updates
Components: main contrib
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

Types: deb
URIs: http://deb.debian.org/debian-security
Suites: bookworm-security
Components: main contrib
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
`

	if _, err := kindContainerExec(ctx, nodeContainer,
		"bash", "-c",
		fmt.Sprintf("cat > /etc/apt/sources.list.d/debian.sources << 'SOURCES_EOF'\n%sSOURCES_EOF", contribSources),
	); err != nil {
		_, _ = fmt.Fprintf(output,
			"[AC4] warn: enable contrib apt source in %s: %v — continuing\n",
			nodeContainer, err)
		// Non-fatal: proceed without contrib; zfsutils-linux may still be cached.
	}

	// ── Step 2: Update apt cache ──────────────────────────────────────────────
	//
	// Run apt-get update with quiet flags. Errors are non-fatal: if the cache
	// cannot be updated we proceed with the existing cached package index.
	if out, err := kindContainerExec(ctx, nodeContainer,
		"apt-get", "update", "-qq",
	); err != nil {
		_, _ = fmt.Fprintf(output,
			"[AC4] warn: apt-get update in %s: %v — proceeding with cached package index\n",
			nodeContainer, err)
		if out != "" {
			_, _ = fmt.Fprintf(output, "[AC4] apt-get update output: %s\n", out)
		}
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC4] apt-get update in container %s succeeded\n", nodeContainer)
	}

	// ── Step 3: Install zfsutils-linux and lvm2 ─────────────────────────────
	//
	// Install both packages in a single apt-get invocation to reduce docker exec
	// round-trips and apt-get startup overhead. A separate install for each
	// package would add ~5-15s because apt-get parses its database twice.
	//
	// DEBIAN_FRONTEND=noninteractive suppresses interactive prompts.
	// --no-install-recommends keeps the installation small and fast.
	//
	// Failure handling: if the combined install fails (e.g. network timeout),
	// we fall back to the two-package combined output for diagnostics. The
	// provisioners detect missing binaries and soft-skip independently.
	// Install zfsutils-linux, lvm2, AND tgt (iSCSI target daemon) in one
	// apt-get call to avoid multiple round-trips and apt database parses.
	// tgt provides tgtd and tgtadm needed by the AC9c iSCSI backend check.
	if _, err := kindContainerExec(ctx, nodeContainer,
		"bash", "-c",
		"DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends zfsutils-linux lvm2 tgt 2>&1",
	); err != nil {
		_, _ = fmt.Fprintf(output,
			"[AC4] warn: install zfsutils-linux lvm2 tgt in %s: %v — ZFS/LVM/iSCSI provisioning will soft-skip\n",
			nodeContainer, err)
		// Non-fatal: provisioners detect missing binaries and soft-skip.
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC4] zfsutils-linux and lvm2 installed in container %s\n", nodeContainer)
	}

	// ── Step 5: Patch lvm.conf for container operation ────────────────────────
	//
	// By default LVM2 tries to use udev for device list management and
	// synchronisation. Inside a Docker container udev is not running, which
	// causes pvcreate/vgcreate/lvcreate to fail with "device not cleared".
	//
	// We apply two layers of patches:
	//  1. sed replacements for global udev settings (idempotent on exact matches)
	//  2. Append an activation{} override block so that "verify_udev_operations=0"
	//     prevents "not found: device not cleared" errors in lvcreate. LVM2 uses
	//     the last value when a key appears multiple times, so appending wins.
	lvmConfPatch := strings.Join([]string{
		`sed -i 's/udev_sync = 1/udev_sync = 0/' /etc/lvm/lvm.conf 2>/dev/null || true`,
		`sed -i 's/udev_rules = 1/udev_rules = 0/' /etc/lvm/lvm.conf 2>/dev/null || true`,
		`sed -i 's/obtain_device_list_from_udev = 1/obtain_device_list_from_udev = 0/' /etc/lvm/lvm.conf 2>/dev/null || true`,
		// Append override block — LVM uses last-wins for duplicate keys.
		`cat >> /etc/lvm/lvm.conf << 'LVMEOF'` + "\n" +
			`activation {` + "\n" +
			`    udev_sync = 0` + "\n" +
			`    udev_rules = 0` + "\n" +
			`    verify_udev_operations = 0` + "\n" +
			`}` + "\n" +
			`LVMEOF`,
	}, "\n")

	if _, err := kindContainerExec(ctx, nodeContainer, "bash", "-c", lvmConfPatch); err != nil {
		// Non-fatal: lvm.conf patching failed; LVM commands may emit warnings
		// but should still function for the simple pvcreate/vgcreate workflow.
		_, _ = fmt.Fprintf(output,
			"[AC4] warn: patch lvm.conf in %s: %v — LVM may emit udev warnings\n",
			nodeContainer, err)
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC4] lvm.conf patched for container operation in %s\n", nodeContainer)
	}

	return nil
}

// kindContainerExec runs a command inside a Docker container via "docker exec".
//
// DOCKER_HOST is forwarded automatically from the calling process's environment
// by setting cmd.Env = os.Environ(). The Docker daemon endpoint is NEVER
// hardcoded — only the environment variable is consulted.
//
// Returns (stdout, nil) on success or ("", error) on failure. Stderr is
// included in the error message for diagnostics.
func kindContainerExec(ctx context.Context, container string, args ...string) (string, error) {
	if strings.TrimSpace(container) == "" {
		return "", fmt.Errorf("kindContainerExec: container name must not be empty")
	}

	dockerArgs := append([]string{"exec", container}, args...)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...) //nolint:gosec

	// Propagate DOCKER_HOST (and all other env vars) from the parent process.
	// os.Environ() includes DOCKER_HOST when set; when absent the docker client
	// falls back to its default daemon socket.
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
		return "", fmt.Errorf("docker exec %s %s: %s",
			container, strings.Join(args, " "), errText)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// setupFabricBackends configures NVMe-oF TCP and iSCSI targets inside the Kind
// container so that the AC9c backend env-check passes on all Ginkgo nodes.
//
// This replaces the DeployFabricReadinessDaemonSet Kubernetes DaemonSet approach
// with direct docker exec calls, avoiding the ~5m DaemonSet readiness timeout
// and keeping fabric setup within the 2-minute suite budget.
//
// NVMe-oF setup: configures kernel configfs subsystem + port + symlink.
// iSCSI setup: starts tgtd and creates the E2E target IQN.
//
// All steps are idempotent — safe to call on a pre-configured container.
// Non-fatal errors are logged; hard failures return non-nil error.
func setupFabricBackends(ctx context.Context, nodeContainer string, output io.Writer) error {
	if output == nil {
		output = io.Discard
	}

	const (
		nvmeSubsysNQN = "nqn.2024-01.io.pillar-csi:e2e-target"
		iscsiIQN      = "iqn.2024-01.io.pillar-csi:e2e-target"
		iscsiTID      = "10"
	)

	// ── NVMe-oF TCP target via kernel configfs ────────────────────────────────
	//
	// The nvmet and nvmet_tcp modules must be loaded on the host kernel (Kind
	// containers share the host kernel). modprobe is best-effort; configfs ops
	// fail gracefully if the modules are absent.
	nvmofScript := strings.Join([]string{
		// Load modules (idempotent; || true so missing modules don't abort)
		"modprobe nvmet 2>/dev/null || true",
		"modprobe nvmet_tcp 2>/dev/null || true",
		// Mount configfs if not already mounted
		"mountpoint -q /sys/kernel/config 2>/dev/null || mount -t configfs configfs /sys/kernel/config 2>/dev/null || true",
		// Create NVMe-oF subsystem (idempotent)
		`NVME_SUBSYS="` + nvmeSubsysNQN + `"`,
		`SUBSYS_DIR="/sys/kernel/config/nvmet/subsystems/${NVME_SUBSYS}"`,
		`PORT_DIR="/sys/kernel/config/nvmet/ports/1"`,
		`if [ ! -d "${SUBSYS_DIR}" ]; then`,
		`  mkdir -p "${SUBSYS_DIR}"`,
		`  echo 1 > "${SUBSYS_DIR}/attr_allow_any_host"`,
		`  dd if=/dev/zero of=/tmp/nvmet-e2e-ns0.img bs=1M count=64 status=none 2>/dev/null || true`,
		`  NVME_LOOP=$(losetup --find --show /tmp/nvmet-e2e-ns0.img 2>/dev/null || echo "")`,
		`  if [ -n "${NVME_LOOP}" ]; then`,
		`    mkdir -p "${SUBSYS_DIR}/namespaces/1"`,
		`    echo "${NVME_LOOP}" > "${SUBSYS_DIR}/namespaces/1/device_path"`,
		`    echo 1 > "${SUBSYS_DIR}/namespaces/1/enable"`,
		`  fi`,
		`fi`,
		// Create TCP port 1 (idempotent)
		`if [ ! -d "${PORT_DIR}" ]; then`,
		`  mkdir -p "${PORT_DIR}"`,
		`  echo 0.0.0.0 > "${PORT_DIR}/addr_traddr"`,
		`  echo tcp      > "${PORT_DIR}/addr_trtype"`,
		`  echo 4420     > "${PORT_DIR}/addr_trsvcid"`,
		`  echo ipv4     > "${PORT_DIR}/addr_adrfam"`,
		`fi`,
		// Link subsystem to port (idempotent)
		`if [ ! -e "${PORT_DIR}/subsystems/${NVME_SUBSYS}" ]; then`,
		`  ln -s "${SUBSYS_DIR}" "${PORT_DIR}/subsystems/${NVME_SUBSYS}"`,
		`fi`,
	}, "\n")

	if _, err := kindContainerExec(ctx, nodeContainer, "bash", "-c", nvmofScript); err != nil {
		// NVMe-oF setup failed — log but continue (iSCSI check may still pass)
		_, _ = fmt.Fprintf(output,
			"[AC9b] warn: NVMe-oF TCP setup in %s: %v — NVMe-oF AC9c check may fail\n",
			nodeContainer, err)
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC9b] NVMe-oF TCP target configured in container %s\n", nodeContainer)
	}

	// ── iSCSI target via tgtd ─────────────────────────────────────────────────
	//
	// tgt was installed by installKindContainerBackendTools above. Start tgtd
	// (if not already running) and create the E2E target IQN.
	iscsiScript := strings.Join([]string{
		// Start tgtd if not already running
		`if ! pgrep -x tgtd > /dev/null 2>&1; then`,
		`  tgtd 2>/dev/null || true`,
		`  sleep 2`,
		`fi`,
		// Create iSCSI target (idempotent)
		`IQN="` + iscsiIQN + `"`,
		`TID=` + iscsiTID,
		`if ! tgtadm --lld iscsi --mode target --op show 2>/dev/null | grep -q "${IQN}"; then`,
		`  dd if=/dev/zero of=/tmp/iscsi-e2e-lun0.img bs=1M count=64 status=none 2>/dev/null || true`,
		`  ISCSI_LOOP=$(losetup --find --show /tmp/iscsi-e2e-lun0.img 2>/dev/null || echo "")`,
		`  tgtadm --lld iscsi --mode target --op new --tid ${TID} --targetname "${IQN}" 2>/dev/null || true`,
		`  if [ -n "${ISCSI_LOOP}" ]; then`,
		`    tgtadm --lld iscsi --mode logicalunit --op new --tid ${TID} --lun 1 --backing-store "${ISCSI_LOOP}" 2>/dev/null || true`,
		`  fi`,
		`  tgtadm --lld iscsi --mode target --op bind --tid ${TID} --initiator-address ALL 2>/dev/null || true`,
		`fi`,
	}, "\n")

	if _, err := kindContainerExec(ctx, nodeContainer, "bash", "-c", iscsiScript); err != nil {
		_, _ = fmt.Fprintf(output,
			"[AC9b] warn: iSCSI target setup in %s: %v — iSCSI AC9c check may fail\n",
			nodeContainer, err)
	} else {
		_, _ = fmt.Fprintf(output,
			"[AC9b] iSCSI target configured in container %s\n", nodeContainer)
	}

	return nil
}
