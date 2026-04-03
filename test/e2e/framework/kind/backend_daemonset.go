// Package kind provides helpers for creating Kind clusters and deploying the
// privileged backend-readiness DaemonSet that pre-installs and validates real
// ZFS and LVM tools on every Kind node.
//
// # Sub-AC 9a Design
//
// The DaemonSet approach is Kubernetes-native and scales across all Kind nodes:
//
//  1. CheckBackendKernelModules — validates that the host kernel has the required
//     modules loaded (zfs, dm_thin_pool). FAILs immediately with an actionable
//     remediation message if any module is absent — never soft-skips.
//
//  2. DeployBackendReadinessDaemonSet — applies the privileged DaemonSet manifest
//     to the Kind cluster via kubectl. The DaemonSet:
//     • Runs one privileged pod per Kind node (tolerates all taints including
//     control-plane).
//     • Uses hostPID: true so the init container can nsenter into the Kind
//     node's mount namespace and run apt-get directly on the node's filesystem.
//     • Init container installs zfsutils-linux and lvm2 on the Kind node.
//     • Main container stays running with a readiness probe that validates ZFS
//     and LVM by calling zpool/vgs inside the Kind node's mount namespace
//     via nsenter.
//
//  3. WaitForBackendDaemonSetReady — polls until all DaemonSet pods are Ready
//     or the deadline is reached. On timeout or pod failure, fetches pod events
//     and logs for a clear diagnosis message. FAILs — never soft-skips.
//
//  4. RemoveBackendReadinessDaemonSet — idempotent cleanup called on teardown.
//
// # Why nsenter?
//
// The DaemonSet pod runs inside the Kind node container (which is itself a
// Docker container). Installing packages inside the pod's own overlay filesystem
// would NOT make the binaries available to the test suite's docker-exec-based
// verifiers (e.g. "docker exec <kind-node> zpool …"). By using nsenter to enter
// the Kind node container's mount namespace (PID 1 of the node), apt-get writes
// to the Kind node's filesystem — so all subsequent `docker exec <kind-node>`
// invocations find the binaries in the standard PATH.
//
// # Fail-fast contract
//
// This package enforces the project constraint: "When host prerequisites are
// missing, FAIL immediately with a clear message — never soft-skip."
//   - CheckBackendKernelModules returns a non-nil error (not a skip) when modules
//     are absent, including the exact modprobe / package-install commands.
//   - WaitForBackendDaemonSetReady returns a non-nil error (not a skip) when pods
//     do not become Ready within the deadline.
package kind

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	// BackendDaemonSetName is the Kubernetes name of the privileged DaemonSet.
	BackendDaemonSetName = "pillar-csi-backend-readiness"

	// BackendDaemonSetNamespace is the namespace where the DaemonSet is deployed.
	// kube-system is used because Kind clusters label it with
	// pod-security.kubernetes.io/enforce=privileged, allowing privileged pods.
	BackendDaemonSetNamespace = "kube-system"

	// BackendDaemonSetReadyTimeout is the default maximum time to wait for all
	// DaemonSet pods to transition to Ready.  3 minutes is sufficient even on
	// cold-cache machines where apt-get must download packages.
	BackendDaemonSetReadyTimeout = 3 * time.Minute

	// backendDaemonSetPollInterval is how often WaitForBackendDaemonSetReady
	// polls the DaemonSet status.
	backendDaemonSetPollInterval = 5 * time.Second
)

// backendReadinessDaemonSetTemplate is the DaemonSet manifest applied by
// DeployBackendReadinessDaemonSet.  It is a Go text/template with one field:
//   - .Namespace — the target namespace (BackendDaemonSetNamespace).
//
// Design notes:
//   - hostPID: true is required so nsenter in the init container can enter
//     the Kind node container's mount namespace via /proc/1/ns/mnt.
//   - securityContext.privileged: true is required for nsenter and for ZFS/LVM
//     operations that access block devices and kernel interfaces.
//   - tolerations with operator: Exists ensures the pod is scheduled on
//     control-plane nodes as well as worker nodes.
//   - The installer init container runs apt-get INSIDE the Kind node's mount
//     namespace (not the pod's overlay) so that `docker exec <kind-node> zpool`
//     succeeds in the test suite's existing backend-provisioning code.
//   - The readiness probe validates BOTH ZFS and LVM readiness via nsenter:
//   - zpool list requires the zfs kernel module (opens /dev/zfs).
//   - vgs requires device-mapper (opens /dev/mapper control).
const backendReadinessDaemonSetTemplate = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: pillar-csi-backend-readiness
  namespace: {{ .Namespace }}
  labels:
    app.kubernetes.io/name: pillar-csi-backend-readiness
    app.kubernetes.io/component: backend-readiness
    app.kubernetes.io/managed-by: pillar-csi-e2e
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: pillar-csi-backend-readiness
  updateStrategy:
    type: RollingUpdate
  template:
    metadata:
      labels:
        app.kubernetes.io/name: pillar-csi-backend-readiness
        app.kubernetes.io/component: backend-readiness
    spec:
      # Tolerate every taint — ensures one pod runs on the control-plane node
      # and on every worker node regardless of taints applied by the Kind config.
      tolerations:
      - operator: Exists

      # hostPID: true shares the Kind node's PID namespace with each container.
      # This makes /proc/1/ns/mnt point to the mount namespace of the Kind
      # node container's init process, allowing nsenter to enter that namespace
      # and run commands (apt-get, zpool, vgs) on the node's own filesystem.
      hostPID: true

      initContainers:
      # backend-installer runs once per pod start and installs zfsutils-linux
      # and lvm2 on the Kind node's filesystem via nsenter.
      - name: backend-installer
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
          echo "[backend-installer] Entering Kind node mount namespace via nsenter..."

          # nsenter --mount=/proc/1/ns/mnt enters the mount namespace of the
          # Kind node container's PID 1.  All subsequent commands see and write
          # to the Kind node's filesystem — not the pod's overlay filesystem.
          nsenter --mount=/proc/1/ns/mnt -- /bin/bash -c '
            set -e
            export DEBIAN_FRONTEND=noninteractive

            # Enable contrib sources for zfsutils-linux (in Debian contrib, not main).
            if [ -d /etc/apt/sources.list.d ]; then
              cat > /etc/apt/sources.list.d/e2e-contrib.list <<APTEOF
deb http://deb.debian.org/debian bookworm main contrib
deb http://deb.debian.org/debian bookworm-updates main contrib
deb http://security.debian.org/debian-security bookworm-security main contrib
APTEOF
            fi

            echo "[backend-installer] apt-get update..."
            apt-get update -qq

            echo "[backend-installer] Installing zfsutils-linux and lvm2..."
            apt-get install -y -q --no-install-recommends zfsutils-linux lvm2

            # Disable udev integration in LVM.  Inside a Docker container udev
            # is not running, so LVM commands fail or warn if udev integration
            # is enabled.  Patching lvm.conf makes pvcreate/vgcreate work reliably.
            echo "[backend-installer] Patching lvm.conf for container operation..."
            for setting in udev_sync udev_rules obtain_device_list_from_udev; do
              sed -i "s/${setting} = 1/${setting} = 0/" /etc/lvm/lvm.conf 2>/dev/null || true
            done

            echo "[backend-installer] Verifying installations..."
            zpool version
            pvcreate --version

            echo "[backend-installer] ZFS and LVM tools installed and verified."
          '

      containers:
      # backend-readiness is the main container. It stays running so the
      # readiness probe can be evaluated continuously.  The pod transitions
      # to Ready only when both ZFS and LVM pass their readiness checks.
      - name: backend-readiness
        image: debian:bookworm-slim
        imagePullPolicy: IfNotPresent
        securityContext:
          privileged: true
          runAsUser: 0
        command:
        - /bin/sh
        - -c
        - |
          echo "[backend-readiness] Init complete. Keeping pod alive for readiness probing."
          exec sleep infinity
        readinessProbe:
          exec:
            command:
            - /bin/sh
            - -c
            - |
              # ZFS readiness: zpool list requires the zfs kernel module.
              # If the module is not loaded, this exits non-zero.
              nsenter --mount=/proc/1/ns/mnt -- zpool list > /dev/null 2>&1 || {
                echo "[readiness] FAIL: zpool list failed — zfs kernel module not loaded?"
                exit 1
              }

              # LVM readiness: vgs requires device-mapper (dm_thin_pool module).
              # Exit code 5 ("no VGs found") is SUCCESS — it means LVM works but
              # no VGs exist yet.  Any other non-zero exit indicates a real failure.
              nsenter --mount=/proc/1/ns/mnt -- /bin/sh -c '
                vgs --noheadings 2>/dev/null
                rc=$?
                [ $rc -eq 0 ] || [ $rc -eq 5 ] && exit 0
                echo "[readiness] FAIL: vgs failed (exit $rc) — dm_thin_pool module not loaded?"
                exit 1
              ' || exit 1

              echo "[readiness] PASS: ZFS and LVM are operational."
          # initialDelaySeconds accounts for apt-get download + installation time.
          # 15 s is conservative; typical installation takes 20-60 s depending on
          # network speed and image cache.
          initialDelaySeconds: 15
          # periodSeconds controls how often the probe runs after the initial delay.
          periodSeconds: 10
          # failureThreshold * periodSeconds = 180 s maximum probe wait after
          # initialDelaySeconds, giving a total readiness window of ~3 minutes.
          failureThreshold: 18
          successThreshold: 1
          timeoutSeconds: 15
`

// ─── Kernel module prerequisite check ─────────────────────────────────────────

// BackendKernelModule describes a kernel module required by a storage backend.
type BackendKernelModule struct {
	// Name is the module name as it appears in /proc/modules (underscores).
	Name string
	// Purpose is a human-readable description of the module's role.
	Purpose string
	// ModprobeCmd is the command to load the module.
	ModprobeCmd string
	// InstallHints lists OS-specific package install commands.
	InstallHints []string
}

// requiredBackendModules lists the kernel modules required by the E2E backend
// validation DaemonSet.  All entries are treated as required: if any is absent,
// CheckBackendKernelModules returns a non-nil error.
var requiredBackendModules = []BackendKernelModule{
	{
		Name:        "zfs",
		Purpose:     "ZFS pool backend — required for zpool/zfs binaries and /dev/zfs",
		ModprobeCmd: "modprobe zfs",
		InstallHints: []string{
			"Ubuntu/Debian:  sudo apt install zfsutils-linux",
			"Fedora/RHEL:    sudo dnf install https://zfsonlinux.org/epel/zfs-release.el9_3.noarch.rpm && sudo dnf install zfs",
			"Arch Linux:     sudo pacman -S zfs-dkms",
		},
	},
	{
		Name:        "dm_thin_pool",
		Purpose:     "LVM thin-pool backend — required for device-mapper and pvcreate/vgcreate/lvcreate",
		ModprobeCmd: "modprobe dm_thin_pool",
		InstallHints: []string{
			"Ubuntu/Debian:  sudo apt install lvm2",
			"Fedora/RHEL:    sudo dnf install lvm2",
			"Arch Linux:     sudo pacman -S lvm2",
		},
	},
}

// CheckBackendKernelModules validates that every module in requiredBackendModules
// is loaded in the running kernel by reading /proc/modules.
//
// This function enforces the fail-fast policy: if any required module is absent,
// it returns a non-nil error with actionable remediation instructions.  The
// caller should print the error and exit non-zero rather than soft-skipping.
//
// The check is non-destructive and does not require elevated privileges.
// It completes in < 10 ms.
func CheckBackendKernelModules() error {
	loaded, err := readLoadedKernelModules()
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9a] cannot read /proc/modules: %w\n"+
				"  Ensure the test runs on a Linux host with a mounted /proc filesystem",
			err,
		)
	}

	var missing []BackendKernelModule
	for _, mod := range requiredBackendModules {
		// Normalise hyphens to underscores: the kernel stores module names with
		// underscores regardless of how the user typed them (e.g. "dm-thin-pool"
		// → "dm_thin_pool").
		target := strings.ReplaceAll(mod.Name, "-", "_")
		if _, ok := loaded[target]; !ok {
			missing = append(missing, mod)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	return formatMissingModulesError(missing)
}

// formatMissingModulesError builds the human-readable error message for missing
// kernel modules, including remediation instructions for common Linux distros.
func formatMissingModulesError(missing []BackendKernelModule) error {
	var sb strings.Builder
	sb.WriteString("\n╔══════════════════════════════════════════════════════════════╗\n")
	sb.WriteString("║      pillar-csi E2E backend kernel modules MISSING           ║\n")
	sb.WriteString("╚══════════════════════════════════════════════════════════════╝\n")
	sb.WriteString("\n  All 421 test cases require real storage backends.\n")
	sb.WriteString("  Soft-skip is DISABLED — missing modules cause FAIL, not SKIP.\n")
	sb.WriteString("\n  Missing modules:\n\n")

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

	return fmt.Errorf("%s", sb.String())
}

// readLoadedKernelModules reads /proc/modules and returns the set of currently
// loaded module names (normalised: hyphens → underscores).
func readLoadedKernelModules() (map[string]struct{}, error) {
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
		// Column 0 is the module name; normalise hyphens to underscores.
		name := strings.ReplaceAll(fields[0], "-", "_")
		modules[name] = struct{}{}
	}
	return modules, nil
}

// ─── DaemonSet deploy / wait / remove ─────────────────────────────────────────

// DeployBackendReadinessDaemonSet writes the DaemonSet manifest to a temporary
// file under /tmp and applies it to the Kind cluster via:
//
//	kubectl apply -f /tmp/<manifest> --kubeconfig <kubeconfigPath>
//
// The manifest is deleted from /tmp after the apply call.
//
// Call CheckBackendKernelModules before this function to catch missing kernel
// modules early with a clear error message.
//
// kubectlBinary defaults to "kubectl" when empty.
func DeployBackendReadinessDaemonSet(ctx context.Context, kubeconfigPath, kubectlBinary string) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9a] DeployBackendReadinessDaemonSet: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}

	// Render the manifest template.
	manifest, err := renderDaemonSetManifest(BackendDaemonSetNamespace)
	if err != nil {
		return fmt.Errorf("[Sub-AC 9a] render DaemonSet manifest: %w", err)
	}

	// Write to a temp file under /tmp (never outside /tmp).
	// Use os.TempDir() explicitly rather than "" so the path is unambiguous.
	tmpFile, err := os.CreateTemp(os.TempDir(), "pillar-csi-backend-ds-*.yaml")
	if err != nil {
		return fmt.Errorf("[Sub-AC 9a] create temp manifest file: %w", err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(manifest); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("[Sub-AC 9a] write manifest to %s: %w", tmpFile.Name(), err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("[Sub-AC 9a] close temp manifest file: %w", err)
	}

	// Apply the manifest.
	out, err := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"apply", "-f", tmpFile.Name(),
	)
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9a] kubectl apply DaemonSet: %w\n  output: %s",
			err, out,
		)
	}
	return nil
}

// WaitForBackendDaemonSetReady polls the DaemonSet status until all desired pods
// are Ready or the deadline is reached.
//
// On timeout, the function fetches pod events and logs to build a diagnostic
// message and returns a descriptive error.  The error includes:
//   - The number of pods that became Ready vs the desired count.
//   - Recent pod events (for CrashLoopBackOff / ImagePullBackOff diagnosis).
//   - A reminder to check kernel modules if the readiness probe failed.
//
// The function never returns nil when any pod failed to become Ready.
// kubectlBinary defaults to "kubectl" when empty.
func WaitForBackendDaemonSetReady(
	ctx context.Context,
	kubeconfigPath, kubectlBinary string,
	timeout time.Duration,
) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9a] WaitForBackendDaemonSetReady: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}
	if timeout <= 0 {
		timeout = BackendDaemonSetReadyTimeout
	}

	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(backendDaemonSetPollInterval)
	defer tick.Stop()

	for {
		// Check readiness.
		ready, desired, err := backendDaemonSetReadiness(ctx, kubeconfigPath, kubectlBinary)
		if err == nil && ready >= desired && desired > 0 {
			return nil // all pods Ready
		}

		// Check deadline.
		if time.Now().After(deadline) {
			return buildReadinessTimeoutError(ctx, kubeconfigPath, kubectlBinary, ready, desired, timeout)
		}

		// Wait for next tick or context cancellation.
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"[Sub-AC 9a] WaitForBackendDaemonSetReady: context cancelled "+
					"after %s (ready=%d desired=%d): %w",
				timeout, ready, desired, ctx.Err(),
			)
		case <-tick.C:
		}
	}
}

// RemoveBackendReadinessDaemonSet deletes the backend-readiness DaemonSet from
// the Kind cluster.  It is idempotent: calling it when the DaemonSet does not
// exist returns nil.
//
// kubectlBinary defaults to "kubectl" when empty.
func RemoveBackendReadinessDaemonSet(ctx context.Context, kubeconfigPath, kubectlBinary string) error {
	if strings.TrimSpace(kubeconfigPath) == "" {
		return fmt.Errorf("[Sub-AC 9a] RemoveBackendReadinessDaemonSet: kubeconfigPath must not be empty")
	}
	if strings.TrimSpace(kubectlBinary) == "" {
		kubectlBinary = "kubectl"
	}

	out, err := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"delete", "daemonset", BackendDaemonSetName,
		"-n", BackendDaemonSetNamespace,
		"--ignore-not-found=true",
	)
	if err != nil {
		return fmt.Errorf(
			"[Sub-AC 9a] kubectl delete DaemonSet %s: %w\n  output: %s",
			BackendDaemonSetName, err, out,
		)
	}
	return nil
}

// ─── Internal helpers ──────────────────────────────────────────────────────────

// renderDaemonSetManifest renders backendReadinessDaemonSetTemplate with the
// given namespace value.
func renderDaemonSetManifest(namespace string) (string, error) {
	if namespace == "" {
		namespace = BackendDaemonSetNamespace
	}

	tmpl, err := template.New("backend-ds").Parse(backendReadinessDaemonSetTemplate)
	if err != nil {
		return "", fmt.Errorf("parse DaemonSet template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{
		"Namespace": namespace,
	}); err != nil {
		return "", fmt.Errorf("execute DaemonSet template: %w", err)
	}
	return buf.String(), nil
}

// backendDaemonSetReadiness returns (readyPods, desiredPods, error) by running:
//
//	kubectl get daemonset <name> -n <ns> -o jsonpath=...
//
// Returns (0, 0, nil) when the DaemonSet is not yet visible to the API server.
//
//nolint:dupl // intentional structural parallel with fabricDaemonSetReadiness
func backendDaemonSetReadiness(ctx context.Context, kubeconfigPath, kubectlBinary string) (ready, desired int, err error) {
	// Use kubectl get with jsonpath to extract the ready/desired counts.
	// The jsonpath selects numberReady and desiredNumberScheduled from the status.
	out, runErr := kubectlRun(ctx, kubeconfigPath, kubectlBinary,
		"get", "daemonset", BackendDaemonSetName,
		"-n", BackendDaemonSetNamespace,
		"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}",
	)
	if runErr != nil {
		// DaemonSet may not exist yet — treat as not-ready-yet (not an error).
		if strings.Contains(strings.ToLower(runErr.Error()), "not found") ||
			strings.Contains(strings.ToLower(runErr.Error()), "error from server (notfound)") {
			return 0, 0, nil
		}
		return 0, 0, runErr
	}

	parts := strings.SplitN(strings.TrimSpace(out), "/", 2)
	if len(parts) != 2 {
		// Output is empty when the DaemonSet status is not yet populated.
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

// buildReadinessTimeoutError creates a rich error message when WaitForBackendDaemonSetReady
// times out.  It fetches pod events and logs to explain WHY the pods are not Ready.
func buildReadinessTimeoutError(
	ctx context.Context,
	kubeconfigPath, kubectlBinary string,
	ready, desired int,
	timeout time.Duration,
) error {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"\n[Sub-AC 9a] backend-readiness DaemonSet not Ready after %s\n"+
			"  Ready pods: %d / %d desired\n\n",
		timeout.Round(time.Second), ready, desired,
	)

	// Collect pod events for diagnosis.
	diagCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	events, evErr := kubectlRun(diagCtx, kubeconfigPath, kubectlBinary,
		"get", "events",
		"-n", BackendDaemonSetNamespace,
		"--field-selector", fmt.Sprintf(
			"involvedObject.name=%s,involvedObject.kind=DaemonSet",
			BackendDaemonSetName,
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
		"-n", BackendDaemonSetNamespace,
		"-l", "app.kubernetes.io/name=pillar-csi-backend-readiness",
		"-o", "wide",
	)
	if podsErr == nil && strings.TrimSpace(pods) != "" {
		sb.WriteString("  Pod status:\n")
		for _, line := range strings.Split(pods, "\n") {
			fmt.Fprintf(&sb, "    %s\n", line)
		}
		sb.WriteString("\n")
	}

	// Diagnostic guidance.
	sb.WriteString("  Common causes:\n")
	sb.WriteString("    • zfs kernel module not loaded → run: modprobe zfs\n")
	sb.WriteString("    • dm_thin_pool module not loaded → run: modprobe dm_thin_pool\n")
	sb.WriteString("    • Network unavailable for apt-get → check container DNS/connectivity\n")
	sb.WriteString("    • Kind node image lacks apt-get → use a Debian/Ubuntu-based Kind image\n")
	sb.WriteString("\n  Re-run CheckBackendKernelModules() to verify module status.\n")

	return fmt.Errorf("%s", sb.String())
}

// kubectlRun executes kubectl with the given arguments, forwarding DOCKER_HOST
// and all other environment variables from the calling process.
//
// Returns (stdout, nil) on success or (stdout+stderr, error) on failure.
// All temp files used by kubectl (e.g. for manifests) must be under /tmp.
func kubectlRun(ctx context.Context, kubeconfigPath, kubectlBinary string, args ...string) (string, error) {
	fullArgs := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, kubectlBinary, fullArgs...) //nolint:gosec

	// Propagate DOCKER_HOST and all env vars; never hardcode a daemon address.
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
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), errText)
	}
	return strings.TrimSpace(stdout.String()), nil
}
