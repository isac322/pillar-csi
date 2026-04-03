// Package kind provides helpers for creating Kind clusters and deploying the
// privileged fabric-readiness DaemonSet that pre-installs and validates real
// NVMe-oF TCP and iSCSI targets on every Kind node.
//
// # Sub-AC 9b Design
//
// This file implements the fabric (network fabric) backend DaemonSet, which
// is the companion to the storage backend DaemonSet in backend_daemonset.go
// (Sub-AC 9a). Together they ensure that all four storage backends — ZFS, LVM,
// NVMe-oF TCP, and iSCSI — are fully operational before any TC runs.
//
// The DaemonSet approach is Kubernetes-native and scales across all Kind nodes:
//
//  1. CheckFabricKernelModules — validates that the host kernel has the required
//     modules loaded (nvmet, nvmet_tcp). FAILs immediately with an actionable
//     remediation message if any module is absent — never soft-skips.
//
//  2. DeployFabricReadinessDaemonSet — applies the privileged DaemonSet manifest
//     to the Kind cluster via kubectl. The DaemonSet:
//     • Runs one privileged pod per Kind node (tolerates all taints including
//     control-plane).
//     • Uses hostPID: true so the init container can nsenter into the Kind
//     node's mount namespace and run apt-get directly on the node's filesystem.
//     • Init container installs nvme-cli and tgt (tgtd) on the Kind node,
//     configures the NVMe-oF TCP target in kernel configfs, starts the tgtd
//     daemon, and creates the iSCSI target.
//     • Main container stays running with a readiness probe that validates both
//     NVMe-oF (configfs symlink check) and iSCSI (tgtadm target list) via
//     nsenter into the Kind node's mount namespace.
//
//  3. WaitForFabricDaemonSetReady — polls until all DaemonSet pods are Ready
//     or the deadline is reached. On timeout or pod failure, fetches pod events
//     and logs for a clear diagnosis message. FAILs — never soft-skips.
//
//  4. RemoveFabricReadinessDaemonSet — idempotent cleanup called on teardown.
//
// # NVMe-oF TCP Target Architecture
//
// The NVMe-oF TCP target is implemented entirely in the Linux kernel via the
// nvmet module, configured through the configfs virtual filesystem at
// /sys/kernel/config/nvmet. No user-space daemon is required.
//
// Since Kind nodes share the host kernel, configfs operations inside a Kind
// node affect the host kernel's NVMe target state. The init container:
//   - Mounts configfs at /sys/kernel/config if not already mounted.
//   - Creates a subsystem at nvmet/subsystems/NVMeOFSubsystemNQN.
//   - Creates a loop-device-backed namespace (namespace 1) under the subsystem.
//   - Creates an NVMe-oF TCP port at nvmet/ports/1, listening on 0.0.0.0:4420.
//   - Symlinks the subsystem into the port's subsystems directory.
//
// # iSCSI Target Architecture
//
// The iSCSI target uses tgtd (Linux SCSI Target Framework daemon), which runs
// as a user-space process inside the Kind node. The init container:
//   - Starts tgtd (daemon mode; child process is reparented to Kind node PID 1).
//   - Creates a loop-device-backed LUN (LUN 1) for the iSCSI target.
//   - Binds the target to ALL initiator addresses.
//
// # Why nsenter?
//
// Same rationale as Sub-AC 9a: using nsenter --mount=/proc/1/ns/mnt ensures
// that installed binaries and kernel state (configfs) are in the Kind node's
// filesystem, making them accessible to subsequent `docker exec <kind-node>`
// invocations in the test suite's fabric provisioning helpers.
//
// # Fail-fast contract
//
// This package enforces the project constraint: "When host prerequisites are
// missing, FAIL immediately with a clear message — never soft-skip."
//   - CheckFabricKernelModules returns a non-nil error (not a skip) when modules
//     are absent, including the exact modprobe / package-install commands.
//   - WaitForFabricDaemonSetReady returns a non-nil error (not a skip) when pods
//     do not become Ready within the deadline.
package kind

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	// FabricDaemonSetName is the Kubernetes name of the privileged fabric DaemonSet.
	FabricDaemonSetName = "pillar-csi-fabric-readiness"

	// FabricDaemonSetNamespace is the namespace where the DaemonSet is deployed.
	// kube-system is used because Kind clusters label it with
	// pod-security.kubernetes.io/enforce=privileged, allowing privileged pods.
	FabricDaemonSetNamespace = "kube-system"

	// FabricDaemonSetReadyTimeout is the default maximum time to wait for all
	// fabric DaemonSet pods to transition to Ready.  5 minutes is sufficient
	// even on cold-cache machines where apt-get must download tgt and nvme-cli
	// plus set up the NVMe-oF target and start tgtd.
	FabricDaemonSetReadyTimeout = 5 * time.Minute

	// fabricDaemonSetPollInterval is how often WaitForFabricDaemonSetReady
	// polls the DaemonSet status.
	fabricDaemonSetPollInterval = 5 * time.Second

	// NVMeOFSubsystemNQN is the NVMe Qualified Name for the E2E target subsystem
	// created in the kernel's configfs.  This NQN is stable across test runs and
	// must be unique on the test host; pillar-csi-e2e is a unique prefix.
	NVMeOFSubsystemNQN = "nqn.2024-01.io.pillar-csi:e2e-target"

	// NVMeOFTCPPort is the TCP port on which the E2E NVMe-oF TCP target listens.
	// Port 4420 is the IANA-assigned well-known port for NVMe-oF.
	NVMeOFTCPPort = "4420"

	// ISCSITargetIQN is the IQN for the E2E iSCSI target created by tgtd.
	ISCSITargetIQN = "iqn.2024-01.io.pillar-csi:e2e-target"

	// ISCSITargetTID is the fixed tgtadm target ID used for the E2E iSCSI target.
	// TID 10 is chosen to avoid collisions with dynamically allocated TIDs in
	// [1, 9] used by individual TCs that call iscsi.CreateTarget.
	ISCSITargetTID = "10"
)

// fabricReadinessDaemonSetTemplate is the DaemonSet manifest applied by
// DeployFabricReadinessDaemonSet.  It is a Go text/template with one field:
//   - .Namespace — the target namespace (FabricDaemonSetNamespace).
//
// Design notes:
//   - hostPID: true shares the Kind node's PID namespace with each container.
//     /proc/1/ns/mnt points to the mount namespace of the Kind node's init
//     process, enabling nsenter to access the Kind node's filesystem.
//   - securityContext.privileged: true is required for nsenter, configfs
//     operations, loop device access, and block device management.
//   - tolerations with operator: Exists schedules the pod on all nodes.
//   - The installer init container runs all fabric setup inside the Kind node's
//     mount namespace (nsenter --mount=/proc/1/ns/mnt) so that `docker exec
//     <kind-node>` invocations in the test suite find tgtd running and configfs
//     entries present.
//   - NVMe-oF target: configured via Linux kernel configfs (nvmet module).
//     No user-space daemon required — kernel state persists after init exits.
//   - iSCSI target: tgtd daemon is started with daemon-mode fork; the child
//     process is reparented to the Kind node's PID 1 and persists after the
//     init container exits.
//   - Readiness probe validates BOTH backends via nsenter:
//   - NVMe-oF: verifies that the configfs subsystem→port symlink exists,
//     confirming the TCP target is configured and the nvmet_tcp module is bound.
//   - iSCSI: runs tgtadm target show and greps for the E2E IQN, confirming
//     that tgtd is running and the target is accessible.
const fabricReadinessDaemonSetTemplate = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: pillar-csi-fabric-readiness
  namespace: {{ .Namespace }}
  labels:
    app.kubernetes.io/name: pillar-csi-fabric-readiness
    app.kubernetes.io/component: fabric-readiness
    app.kubernetes.io/managed-by: pillar-csi-e2e
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: pillar-csi-fabric-readiness
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/name: pillar-csi-fabric-readiness
        app.kubernetes.io/component: fabric-readiness
    spec:
      # Tolerate every taint — ensures one pod runs on the control-plane node
      # and on every worker node regardless of taints applied by the Kind config.
      tolerations:
      - operator: Exists

      # hostPID: true shares the Kind node's PID namespace with each container.
      # This makes /proc/1/ns/mnt point to the mount namespace of the Kind
      # node container's init process, allowing nsenter to enter that namespace
      # and run commands (apt-get, configfs ops, tgtd, tgtadm) on the node's
      # own filesystem.
      hostPID: true

      initContainers:
      # fabric-installer runs once per pod start and:
      #   1. Installs nvme-cli and tgt (tgtd) on the Kind node's filesystem.
      #   2. Configures the NVMe-oF TCP target in kernel configfs.
      #   3. Starts the tgtd daemon and creates the iSCSI target.
      # All steps run via nsenter --mount=/proc/1/ns/mnt so they operate on
      # the Kind node's filesystem, not the pod's overlay filesystem.
      - name: fabric-installer
        image: debian:bookworm-slim
        imagePullPolicy: IfNotPresent
        securityContext:
          privileged: true
          runAsUser: 0
        command:
        - /bin/sh
        - -c
        - |
          set -eu
          echo "[fabric-installer] Entering Kind node mount namespace via nsenter..."

          nsenter --mount=/proc/1/ns/mnt -- /bin/bash -c '
            set -e
            export DEBIAN_FRONTEND=noninteractive

            # ── Step 1: Install packages ────────────────────────────────────────
            echo "[fabric-installer] apt-get update..."
            apt-get update -qq

            echo "[fabric-installer] Installing nvme-cli and tgt..."
            apt-get install -y -q --no-install-recommends nvme-cli tgt

            echo "[fabric-installer] Packages installed."

            # ── Step 2: NVMe-oF TCP target setup via kernel configfs ────────────
            echo "[fabric-installer] Configuring NVMe-oF TCP target..."

            # Mount configfs at /sys/kernel/config if not already mounted.
            # mountpoint -q exits 0 if the path is a mount point.
            mountpoint -q /sys/kernel/config 2>/dev/null || \
              mount -t configfs configfs /sys/kernel/config

            # Load the NVMe target modules.  They are required on the host
            # kernel (shared with the Kind node); modprobe is idempotent.
            modprobe nvmet   2>/dev/null || true
            modprobe nvmet_tcp 2>/dev/null || true

            NVME_SUBSYS="nqn.2024-01.io.pillar-csi:e2e-target"
            NVME_SUBSYS_DIR="/sys/kernel/config/nvmet/subsystems/${NVME_SUBSYS}"
            NVME_PORT_DIR="/sys/kernel/config/nvmet/ports/1"

            # Create subsystem (idempotent).
            if [ ! -d "${NVME_SUBSYS_DIR}" ]; then
              echo "[fabric-installer] Creating NVMe-oF subsystem ${NVME_SUBSYS}..."
              mkdir -p "${NVME_SUBSYS_DIR}"
              echo 1 > "${NVME_SUBSYS_DIR}/attr_allow_any_host"

              # Back the namespace with a sparse loop-device image.
              dd if=/dev/zero of=/tmp/nvmet-e2e-ns0.img bs=1M count=64 status=none
              NVME_LOOP=$(losetup --find --show /tmp/nvmet-e2e-ns0.img)

              mkdir -p "${NVME_SUBSYS_DIR}/namespaces/1"
              echo "${NVME_LOOP}" > "${NVME_SUBSYS_DIR}/namespaces/1/device_path"
              echo 1 > "${NVME_SUBSYS_DIR}/namespaces/1/enable"
              echo "[fabric-installer] NVMe-oF namespace created on ${NVME_LOOP}."
            else
              echo "[fabric-installer] NVMe-oF subsystem already exists, skipping."
            fi

            # Create port 1 (TCP, 0.0.0.0:4420) — idempotent.
            if [ ! -d "${NVME_PORT_DIR}" ]; then
              echo "[fabric-installer] Creating NVMe-oF TCP port 4420..."
              mkdir -p "${NVME_PORT_DIR}"
              echo 0.0.0.0 > "${NVME_PORT_DIR}/addr_traddr"
              echo tcp      > "${NVME_PORT_DIR}/addr_trtype"
              echo 4420     > "${NVME_PORT_DIR}/addr_trsvcid"
              echo ipv4     > "${NVME_PORT_DIR}/addr_adrfam"
            else
              echo "[fabric-installer] NVMe-oF port 1 already exists, skipping."
            fi

            # Link subsystem to port — idempotent.
            if [ ! -e "${NVME_PORT_DIR}/subsystems/${NVME_SUBSYS}" ]; then
              echo "[fabric-installer] Linking subsystem to port..."
              ln -s "${NVME_SUBSYS_DIR}" \
                "${NVME_PORT_DIR}/subsystems/${NVME_SUBSYS}"
            fi

            echo "[fabric-installer] NVMe-oF TCP target configured."

            # ── Step 3: iSCSI target setup via tgtd ────────────────────────────
            echo "[fabric-installer] Starting tgtd iSCSI daemon..."

            # Start tgtd in daemon mode (forks; parent returns, child persists).
            # The child is reparented to Kind node PID 1 when this init container
            # exits, ensuring tgtd survives the container lifecycle.
            # Guard with PID file check to avoid double-start on pod restart.
            if [ -f /var/run/tgtd.pid ] && kill -0 $(cat /var/run/tgtd.pid 2>/dev/null) 2>/dev/null; then
              echo "[fabric-installer] tgtd already running (pid $(cat /var/run/tgtd.pid)), skipping."
            else
              tgtd
              # Allow tgtd to fully initialise before issuing tgtadm commands.
              sleep 3
              echo "[fabric-installer] tgtd started."
            fi

            ISCSI_IQN="iqn.2024-01.io.pillar-csi:e2e-target"
            ISCSI_TID=10

            # Create iSCSI target — idempotent.
            if tgtadm --lld iscsi --mode target --op show 2>/dev/null | grep -q "${ISCSI_IQN}"; then
              echo "[fabric-installer] iSCSI target already exists, skipping."
            else
              echo "[fabric-installer] Creating iSCSI target ${ISCSI_IQN}..."

              dd if=/dev/zero of=/tmp/iscsi-e2e-lun0.img bs=1M count=64 status=none
              ISCSI_LOOP=$(losetup --find --show /tmp/iscsi-e2e-lun0.img)

              tgtadm --lld iscsi --mode target --op new \
                --tid ${ISCSI_TID} --targetname "${ISCSI_IQN}"
              tgtadm --lld iscsi --mode logicalunit --op new \
                --tid ${ISCSI_TID} --lun 1 --backing-store "${ISCSI_LOOP}"
              tgtadm --lld iscsi --mode target --op bind \
                --tid ${ISCSI_TID} --initiator-address ALL

              echo "[fabric-installer] iSCSI target created on ${ISCSI_LOOP}."
            fi

            echo "[fabric-installer] All fabric backends configured successfully."
          '

      containers:
      # fabric-readiness is the main container.  It stays running so the
      # readiness probe can be evaluated continuously.  The pod transitions to
      # Ready only when both NVMe-oF TCP and iSCSI pass their readiness checks.
      - name: fabric-readiness
        image: debian:bookworm-slim
        imagePullPolicy: IfNotPresent
        securityContext:
          privileged: true
          runAsUser: 0
        command:
        - /bin/sh
        - -c
        - |
          echo "[fabric-readiness] Init complete. Keeping pod alive for readiness probing."
          exec sleep infinity
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - |
              # ── NVMe-oF readiness ─────────────────────────────────────────────
              # Check that the subsystem→port symlink exists in configfs.
              # This confirms that:
              #   a) configfs is mounted on the Kind node,
              #   b) the nvmet module loaded the subsystem configuration,
              #   c) the nvmet_tcp module bound the TCP port.
              NVME_LINK="/sys/kernel/config/nvmet/ports/1/subsystems/nqn.2024-01.io.pillar-csi:e2e-target"
              nsenter --mount=/proc/1/ns/mnt -- test -L "${NVME_LINK}" 2>/dev/null || {
                echo "[readiness] FAIL: NVMe-oF subsystem→port symlink absent: ${NVME_LINK}"
                echo "[readiness]   Check: nvmet and nvmet_tcp modules loaded?"
                exit 1
              }

              # Verify the TCP port is configured correctly.
              TRTYPE=$(nsenter --mount=/proc/1/ns/mnt -- \
                cat /sys/kernel/config/nvmet/ports/1/addr_trtype 2>/dev/null)
              if [ "${TRTYPE}" != "tcp" ]; then
                echo "[readiness] FAIL: NVMe-oF port trtype='${TRTYPE}', expected 'tcp'"
                exit 1
              fi

              # ── iSCSI readiness ───────────────────────────────────────────────
              # Verify tgtd is running and the E2E target is accessible.
              nsenter --mount=/proc/1/ns/mnt -- \
                tgtadm --lld iscsi --mode target --op show 2>/dev/null \
                | grep -q "iqn.2024-01.io.pillar-csi:e2e-target" || {
                echo "[readiness] FAIL: iSCSI target not found in tgtd — is tgtd running?"
                exit 1
              }

              echo "[readiness] PASS: NVMe-oF TCP and iSCSI targets are operational."
          # initialDelaySeconds accounts for apt-get download + installation +
          # NVMe-oF configfs setup + tgtd start + target creation time.
          # 30 s is conservative; on a warm cache this typically takes 10-20 s.
          initialDelaySeconds: 30
          # periodSeconds controls how often the probe runs after the initial delay.
          periodSeconds: 10
          # failureThreshold * periodSeconds = 270 s maximum probe wait after
          # initialDelaySeconds, giving a total readiness window of ~5 minutes.
          failureThreshold: 27
          successThreshold: 1
          timeoutSeconds: 15
`

// ─── Fabric kernel module prerequisite check ──────────────────────────────────

// FabricKernelModule describes a kernel module required by the fabric (NVMe-oF
// TCP or iSCSI) storage backends.
type FabricKernelModule struct {
	// Name is the module name as it appears in /proc/modules (underscores).
	Name string
	// Purpose is a human-readable description of the module's role.
	Purpose string
	// ModprobeCmd is the command to load the module.
	ModprobeCmd string
	// InstallHints lists OS-specific package install commands.
	InstallHints []string
}

// requiredFabricModules lists the kernel modules required for the NVMe-oF TCP
// and iSCSI target fabric backends.  All entries are treated as required: if
// any is absent, CheckFabricKernelModules returns a non-nil error.
var requiredFabricModules = []FabricKernelModule{
	{
		Name:        "nvmet",
		Purpose:     "NVMe target core — provides configfs interface at /sys/kernel/config/nvmet",
		ModprobeCmd: "modprobe nvmet",
		InstallHints: []string{
			"Ubuntu/Debian:  sudo apt install linux-modules-extra-$(uname -r)",
			"Fedora/RHEL:    sudo dnf install kernel-modules-extra",
			"Arch Linux:     sudo pacman -S linux-headers",
			"Requirement:    kernel ≥ 4.19 with CONFIG_NVME_TARGET=m",
			"Verify with:    ls /sys/kernel/config/nvmet/ after modprobe nvmet",
		},
	},
	{
		Name:        "nvmet_tcp",
		Purpose:     "NVMe target TCP transport — allows NVMe-oF TCP connections to the target",
		ModprobeCmd: "modprobe nvmet_tcp",
		InstallHints: []string{
			"Ubuntu/Debian:  sudo apt install linux-modules-extra-$(uname -r)",
			"Fedora/RHEL:    sudo dnf install kernel-modules-extra",
			"Requirement:    kernel ≥ 5.0 with CONFIG_NVME_TARGET_TCP=m",
			"Note: nvmet must be loaded before nvmet_tcp",
		},
	},
}

// CheckFabricKernelModules validates that every module in requiredFabricModules
// is loaded in the running kernel by reading /proc/modules.
//
// This function enforces the fail-fast policy: if any required module is absent,
// it returns a non-nil error with actionable remediation instructions.  The
// caller should print the error and exit non-zero rather than soft-skipping.
//
// The check is non-destructive and does not require elevated privileges.
// It completes in < 10 ms.
func CheckFabricKernelModules() error {
	loaded, err := readLoadedKernelModules()
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9b] cannot read /proc/modules: %w\n"+
				"  Ensure the test runs on a Linux host with a mounted /proc filesystem",
			err,
		)
	}

	var missing []FabricKernelModule
	for _, mod := range requiredFabricModules {
		// Normalise hyphens to underscores: the kernel stores module names with
		// underscores regardless of how the user typed them.
		target := strings.ReplaceAll(mod.Name, "-", "_")
		if _, ok := loaded[target]; !ok {
			missing = append(missing, mod)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return formatMissingFabricModulesError(missing)
}

// formatMissingFabricModulesError builds the human-readable error message for
// missing fabric kernel modules, including remediation instructions.
func formatMissingFabricModulesError(missing []FabricKernelModule) error {
	var sb strings.Builder
	sb.WriteString("\n╔══════════════════════════════════════════════════════════════╗\n")
	sb.WriteString("║    pillar-csi E2E fabric kernel modules MISSING              ║\n")
	sb.WriteString("╚══════════════════════════════════════════════════════════════╝\n")
	sb.WriteString("\n  All 421 test cases require real storage backends including\n")
	sb.WriteString("  NVMe-oF TCP and iSCSI fabric transports.\n")
	sb.WriteString("  Soft-skip is DISABLED — missing modules cause FAIL, not SKIP.\n")
	sb.WriteString("\n  Missing fabric modules:\n\n")

	for _, mod := range missing {
		fmt.Fprintf(&sb, "  ┌─ %s\n", mod.Name)
		fmt.Fprintf(&sb, "  │  Purpose:    %s\n", mod.Purpose)
		fmt.Fprintf(&sb, "  │  Load with:  %s\n", mod.ModprobeCmd)
		if len(mod.InstallHints) > 0 {
			sb.WriteString("  │  Install:\n")
			for _, hint := range mod.InstallHints {
				fmt.Fprintf(&sb, "  │    • %s\n", hint)
			}
		}
		sb.WriteString("  └─\n\n")
	}

	sb.WriteString("  After loading the modules, re-run:\n")
	sb.WriteString("    make test-e2e\n")
	sb.WriteString("  or:\n")
	sb.WriteString("    go test ./test/e2e/ -tags=e2e -v\n")
	sb.WriteString("\n  Verify current module status with:\n")
	sb.WriteString("    lsmod | grep -E 'nvmet|iscsi'\n")

	return fmt.Errorf("%s", sb.String())
}

// ─── DaemonSet deploy / wait / remove ─────────────────────────────────────────

// DeployFabricReadinessDaemonSet writes the fabric DaemonSet manifest to a
// temporary file under /tmp and applies it to the Kind cluster via:
//
//	kubectl apply -f /tmp/<manifest> --kubeconfig <kubeconfigPath>
//
// The manifest is deleted from /tmp after the apply call.
//
// Call CheckFabricKernelModules before this function to catch missing kernel
// modules early with a clear error message.
//
// kubectlBinary defaults to "kubectl" when empty.
func DeployFabricReadinessDaemonSet(ctx context.Context, kubeconfigPath, kubectlBinary string) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9b] DeployFabricReadinessDaemonSet: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}

	// Render the manifest template.
	manifest, err := renderFabricDaemonSetManifest(FabricDaemonSetNamespace)
	if err != nil {
		return fmt.Errorf("[Sub-AC 9b] render fabric DaemonSet manifest: %w", err)
	}

	// Write to a temp file under /tmp (never outside /tmp).
	// Use os.TempDir() explicitly rather than "" so the path is unambiguous.
	tmpFile, err := os.CreateTemp(os.TempDir(), "pillar-csi-fabric-ds-*.yaml")
	if err != nil {
		return fmt.Errorf("[Sub-AC 9b] create temp manifest file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(manifest); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("[Sub-AC 9b] write manifest to %s: %w", tmpFile.Name(), err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("[Sub-AC 9b] close temp manifest file: %w", err)
	}

	// Apply the manifest.
	out, err := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"apply", "-f", tmpFile.Name(),
	)
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9b] kubectl apply fabric DaemonSet: %w\n  output: %s",
			err, out,
		)
	}
	return nil
}

// WaitForFabricDaemonSetReady polls the fabric DaemonSet status until all
// desired pods are Ready or the deadline is reached.
//
// On timeout, the function fetches pod events and logs to build a diagnostic
// message and returns a descriptive error.  The error includes:
//   - The number of pods that became Ready vs the desired count.
//   - Recent pod events (for CrashLoopBackOff / ImagePullBackOff diagnosis).
//   - Reminders to check kernel modules and package availability.
//
// The function never returns nil when any pod failed to become Ready.
// kubectlBinary defaults to "kubectl" when empty.
func WaitForFabricDaemonSetReady(
	ctx context.Context,
	kubeconfigPath, kubectlBinary string,
	timeout time.Duration,
) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9b] WaitForFabricDaemonSetReady: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}
	if timeout <= 0 {
		timeout = FabricDaemonSetReadyTimeout
	}

	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(fabricDaemonSetPollInterval)
	defer tick.Stop()

	for {
		// Check readiness.
		ready, desired, err := fabricDaemonSetReadiness(ctx, kubeconfigPath, kubectlBinary)
		if err == nil && ready >= desired && desired > 0 {
			return nil // all pods Ready
		}

		// Check deadline.
		if time.Now().After(deadline) {
			return buildFabricReadinessTimeoutError(ctx, kubeconfigPath, kubectlBinary, ready, desired, timeout)
		}

		// Wait for next tick or context cancellation.
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"[Sub-AC 9b] WaitForFabricDaemonSetReady: context cancelled "+
					"after %s (ready=%d desired=%d): %w",
				timeout, ready, desired, ctx.Err(),
			)
		case <-tick.C:
		}
	}
}

// RemoveFabricReadinessDaemonSet deletes the fabric-readiness DaemonSet from
// the Kind cluster.  It is idempotent: calling it when the DaemonSet does not
// exist returns nil.
//
// Note: this function removes the Kubernetes DaemonSet object but does NOT
// clean up the NVMe-oF configfs entries or stop the tgtd daemon.  Those are
// cleaned up when the Kind cluster is deleted (node container removal).
//
// kubectlBinary defaults to "kubectl" when empty.
func RemoveFabricReadinessDaemonSet(ctx context.Context, kubeconfigPath, kubectlBinary string) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9b] RemoveFabricReadinessDaemonSet: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}

	out, err := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"delete", "daemonset", FabricDaemonSetName,
		"-n", FabricDaemonSetNamespace,
		"--ignore-not-found=true",
	)
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9b] kubectl delete fabric DaemonSet %s: %w\n  output: %s",
			FabricDaemonSetName, err, out,
		)
	}
	return nil
}

// ─── Internal helpers ──────────────────────────────────────────────────────────

// renderFabricDaemonSetManifest renders fabricReadinessDaemonSetTemplate with
// the given namespace value.
func renderFabricDaemonSetManifest(namespace string) (string, error) {
	if namespace == "" {
		namespace = FabricDaemonSetNamespace
	}

	tmpl, err := template.New("fabric-ds").Parse(fabricReadinessDaemonSetTemplate)
	if err != nil {
		return "", fmt.Errorf("parse fabric DaemonSet template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{
		"Namespace": namespace,
	}); err != nil {
		return "", fmt.Errorf("execute fabric DaemonSet template: %w", err)
	}
	return buf.String(), nil
}

// fabricDaemonSetReadiness returns (readyPods, desiredPods, error) by running:
//
//	kubectl get daemonset <name> -n <ns> -o jsonpath=...
//
// Returns (0, 0, nil) when the DaemonSet is not yet visible to the API server.
//
//nolint:dupl // intentional structural parallel with backendDaemonSetReadiness
func fabricDaemonSetReadiness(ctx context.Context, kubeconfigPath, kubectlBinary string) (ready, desired int, err error) {
	out, runErr := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"get", "daemonset", FabricDaemonSetName,
		"-n", FabricDaemonSetNamespace,
		"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}",
	)
	if runErr != nil {
		if strings.Contains(strings.ToLower(runErr.Error()), "not found") ||
			strings.Contains(strings.ToLower(runErr.Error()), "error from server (notfound)") {
			return 0, 0, nil
		}
		return 0, 0, runErr
	}

	parts := strings.SplitN(strings.TrimSpace(out), "/", 2)
	if len(parts) != 2 {
		return 0, 0, nil
	}

	_, err = fmt.Sscanf(parts[0], "%d", &ready)
	if err != nil {
		return 0, 0, fmt.Errorf("parse ready count %q: %w", parts[0], err)
	}
	_, err = fmt.Sscanf(parts[1], "%d", &desired)
	if err != nil {
		return 0, 0, fmt.Errorf("parse desired count %q: %w", parts[1], err)
	}
	return ready, desired, nil
}

// buildFabricReadinessTimeoutError creates a rich error message when
// WaitForFabricDaemonSetReady times out.  It fetches pod events and logs to
// explain WHY the pods are not Ready.
func buildFabricReadinessTimeoutError(
	ctx context.Context,
	kubeconfigPath, kubectlBinary string,
	ready, desired int,
	timeout time.Duration,
) error {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"\n[Sub-AC 9b] fabric-readiness DaemonSet not Ready after %s\n"+
			"  Ready pods: %d / %d desired\n\n",
		timeout.Round(time.Second), ready, desired,
	)

	// Collect pod events for diagnosis.
	diagCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	events, evErr := kubectlRun(diagCtx, kubeconfigPath, kubectlBinary,
		"get", "events",
		"-n", FabricDaemonSetNamespace,
		"--field-selector", fmt.Sprintf(
			"involvedObject.name=%s,involvedObject.kind=DaemonSet",
			FabricDaemonSetName,
		),
		"--sort-by=.lastTimestamp",
	)
	if evErr == nil && strings.TrimSpace(events) != "" {
		sb.WriteString("  DaemonSet events:\n")
		for _, line := range strings.Split(events, "\n") {
			fmt.Fprintf(&sb, "    %s\n", line)
		}
		sb.WriteString("\n")
	}

	// Collect pod status for diagnosis.
	pods, podsErr := kubectlRun(diagCtx, kubeconfigPath, kubectlBinary,
		"get", "pods",
		"-n", FabricDaemonSetNamespace,
		"-l", "app.kubernetes.io/name=pillar-csi-fabric-readiness",
		"-o", "wide",
	)
	if podsErr == nil && strings.TrimSpace(pods) != "" {
		sb.WriteString("  Pod status:\n")
		for _, line := range strings.Split(pods, "\n") {
			fmt.Fprintf(&sb, "    %s\n", line)
		}
		sb.WriteString("\n")
	}

	// Collect init container logs for diagnosis.
	logs, logsErr := kubectlRun(diagCtx, kubeconfigPath, kubectlBinary,
		"logs",
		"-n", FabricDaemonSetNamespace,
		"-l", "app.kubernetes.io/name=pillar-csi-fabric-readiness",
		"-c", "fabric-installer",
		"--tail=50",
		"--prefix=true",
	)
	if logsErr == nil && strings.TrimSpace(logs) != "" {
		sb.WriteString("  Init container (fabric-installer) logs:\n")
		for _, line := range strings.Split(logs, "\n") {
			fmt.Fprintf(&sb, "    %s\n", line)
		}
		sb.WriteString("\n")
	}

	// Diagnostic guidance.
	sb.WriteString("  Common causes:\n")
	sb.WriteString("    • nvmet module not loaded on host → run: modprobe nvmet\n")
	sb.WriteString("    • nvmet_tcp module not loaded → run: modprobe nvmet_tcp\n")
	sb.WriteString("    • configfs not mountable (kernel config issue)\n")
	sb.WriteString("    • tgt package unavailable in apt → check apt-get network\n")
	sb.WriteString("    • nvme-cli package unavailable in apt → check apt-get network\n")
	sb.WriteString("    • Kind node image lacks bash/losetup → use Debian/Ubuntu-based Kind image\n")
	sb.WriteString("\n  Re-run CheckFabricKernelModules() to verify module status.\n")
	sb.WriteString("  Verify with: lsmod | grep -E 'nvmet|iscsi'\n")

	return fmt.Errorf("%s", sb.String())
}
