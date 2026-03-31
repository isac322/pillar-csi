//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework

// lvm.go — LVM volume group lifecycle helpers for pillar-csi e2e tests.
//
// SetupLoopbackLVMVG sets up an LVM volume group (and optionally a thin pool)
// inside a running Docker container (typically a Kind worker node) by:
//
//  1. Loading dm_thin_pool kernel module (best-effort via modprobe).
//  2. Installing lvm2 if not already present in the container (requires
//     internet access for apt-get; skipped when lvm2 binaries are found).
//  3. Cleaning up any stale VG + loop device from a prior interrupted run.
//  4. Creating a sparse loopback image file (truncate -s <size>).
//  5. Attaching the image to a loop device (losetup -f --show).
//  6. Creating a Physical Volume, Volume Group, and (optionally) a thin pool.
//
// TeardownLoopbackLVMVG mirrors the creation: it deactivates and removes the
// VG, detaches the loop device, and removes the image file.
//
// All commands run via "docker exec -i <containerName> bash -s" so the script
// is piped through stdin — no shell-quoting hazards, no command-length limits.
//
// Typical usage in a TestMain or Ginkgo BeforeSuite:
//
//	var _ = BeforeSuite(func() {
//	    err := framework.SetupLoopbackLVMVG(ctx, "",
//	        "pillar-csi-e2e-worker",
//	        "e2e-vg", "e2e-thinpool",
//	        "/tmp/e2e-lvm.img", "4G",
//	        false, // reuseIfHealthy
//	    )
//	    Expect(err).NotTo(HaveOccurred())
//	})
//
//	var _ = AfterSuite(func() {
//	    _ = framework.TeardownLoopbackLVMVG(ctx, "",
//	        "pillar-csi-e2e-worker",
//	        "e2e-vg", "/tmp/e2e-lvm.img")
//	})

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SetupLoopbackLVMVG creates an LVM volume group inside the given Docker
// container using a loopback-backed Physical Volume.
//
// Parameters:
//
//	dockerHost    – Docker daemon endpoint (empty = local Unix socket).
//	               Forwarded as DOCKER_HOST to the docker exec subprocess.
//	containerName – Docker container name or ID (e.g. "pillar-csi-e2e-worker").
//	               This is the Kind worker node container in which lvm2 tools
//	               are installed and the VG is created.
//	vgName        – LVM Volume Group name (e.g. "e2e-vg").
//	thinPoolName  – Thin pool LV name (e.g. "e2e-thinpool").
//	               Pass an empty string to create a linear VG only (no thin pool).
//	imagePath     – Absolute path inside the container for the sparse loopback
//	               image file (e.g. "/tmp/e2e-lvm.img").
//	imageSize     – Size string accepted by truncate(1), e.g. "4G", "2G".
//
// On success the VG (and thin pool if thinPoolName is non-empty) are active and
// ready for use.  On error a descriptive message is returned; partial resources
// (loop device, image file) are cleaned up by the inline script's error handler.
// SetupLoopbackLVMVG creates a loopback-backed LVM VG inside a Kind worker
// container.  When reuseIfHealthy is true and the VG already exists with a
// valid backing PV device, the VG is reused.  Otherwise any stale VG is
// destroyed before creating a fresh one.
func SetupLoopbackLVMVG(
	ctx context.Context,
	dockerHost, containerName, vgName, thinPoolName, imagePath, imageSize string,
	reuseIfHealthy bool,
) error {
	// Build thin-pool creation fragment (empty when not requested).
	thinPoolSection := ""
	if thinPoolName != "" {
		thinPoolSection = fmt.Sprintf(`
# ── Create thin pool LV ──────────────────────────────────────────────────
# -T creates a thin-provisioned pool; -l 80%%FREE uses 80%% of the VG space
# leaving headroom for LVM metadata.
#
# Container udev fix: Kind node containers do not run udevd, so device-mapper
# nodes under /dev/mapper/ are not automatically created when lvcreate issues
# DM ioctls.  We disable udev synchronisation in LVM so that lvcreate creates
# the device nodes itself via mknod(2) instead of waiting for a udev event
# that will never arrive.  This is the standard approach for LVM in containers.
echo "Ensuring device-mapper nodes exist..."
dmsetup mknodes 2>/dev/null || true
echo "Creating thin pool %s in VG %s..."
"${LVM_BIN}lvcreate" --config 'activation { udev_sync = 0 udev_rules = 0 }' \
    -Zn -Wn -T -l '80%%FREE' -n %s %s
# Refresh device nodes after thin pool creation.
dmsetup mknodes 2>/dev/null || true
"${LVM_BIN}vgscan" --mknodes 2>/dev/null || true
`,
			shellQuote(thinPoolName), shellQuote(vgName),
			shellQuote(thinPoolName), shellQuote(vgName),
		)
	}

	script := fmt.Sprintf(`#!/bin/bash
# LVM VG setup script — runs inside a Kind worker node container.
# Executed via "docker exec -i <container> bash -s".
set -euo pipefail

IMG=%s
VG=%s
SIZE=%s

echo "=== pillar-csi e2e: LVM VG setup (VG=${VG}, img=${IMG}, size=${SIZE}) ==="

# ── Step 0: load device-mapper kernel modules ─────────────────────────────
# Failures are silently ignored: the module may already be built into the
# kernel, the container may lack CAP_SYS_MODULE, or it may be loaded later
# by the agent DaemonSet's initModprobe init container.
echo "Loading dm_thin_pool module (best-effort)..."
modprobe dm_mod       2>/dev/null || true
modprobe dm_thin_pool 2>/dev/null || true

# ── Step 1: locate or install lvm2 binaries ───────────────────────────────
# Kind node images (kindest/node) are Ubuntu-based but ship without lvm2.
# We detect the binary location first; if missing, install via apt-get.
LVM_BIN=""
if   command -v lvcreate  >/dev/null 2>&1; then LVM_BIN=""
elif [ -x /usr/sbin/lvcreate ];              then LVM_BIN="/usr/sbin/"
elif [ -x /sbin/lvcreate ];                  then LVM_BIN="/sbin/"
else
    echo "lvm2 not found in PATH — installing via apt-get..."
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq 2>&1 | tail -3
    apt-get install -y -q lvm2 2>&1 | tail -5
    # Re-check after install
    if   command -v lvcreate  >/dev/null 2>&1; then LVM_BIN=""
    elif [ -x /usr/sbin/lvcreate ];              then LVM_BIN="/usr/sbin/"
    elif [ -x /sbin/lvcreate ];                  then LVM_BIN="/sbin/"
    else
        echo "ERROR: lvm2 installation failed — lvcreate not found" >&2
        exit 1
    fi
fi
echo "lvm2 binaries prefix: '${LVM_BIN}' (empty = default PATH)"

# ── Step 1.5: disable udev synchronisation for container environments ─────
# Kind node containers do not run udevd, so LVM commands that create
# device-mapper devices would hang or fail when waiting for udev events.
# Disable udev sync/rules globally so LVM creates device nodes itself.
echo "Disabling LVM udev synchronisation (container mode)..."
if [ -f /etc/lvm/lvm.conf ]; then
    sed -i 's/udev_sync = 1/udev_sync = 0/g'   /etc/lvm/lvm.conf 2>/dev/null || true
    sed -i 's/udev_rules = 1/udev_rules = 0/g'  /etc/lvm/lvm.conf 2>/dev/null || true
fi
# Export DM_DISABLE_UDEV to tell libdevmapper to skip udev interaction.
export DM_DISABLE_UDEV=1

# ── Step 2: reuse existing healthy VG or clean up stale state ────────────
if "${LVM_BIN}vgdisplay" "$VG" >/dev/null 2>&1; then
    # Activate the VG and verify that its backing PV device is accessible.
    # A VG whose underlying loop device was detached (e.g. after Kind cluster
    # deletion) still appears in vgdisplay but cannot service I/O.
    "${LVM_BIN}vgchange" -ay "$VG" 2>/dev/null || true
    dmsetup mknodes 2>/dev/null || true
    PV_DEV=$("${LVM_BIN}pvs" --noheadings -o pv_name -S "vg_name=$VG" 2>/dev/null | tr -d ' ')
    if %s && [ -n "$PV_DEV" ] && [ -b "$PV_DEV" ] && [ -f "$IMG" ] && \
       "${LVM_BIN}vgs" --noheadings -o vg_name "$VG" >/dev/null 2>&1; then
        echo "VG ${VG} already exists and is healthy (PV=${PV_DEV}) — reusing"
        echo "SUCCESS: LVM VG '${VG}' reused (existing)"
        exit 0
    fi
    # VG exists but PV device is gone or VG is unhealthy — destroy and recreate.
    echo "VG ${VG} exists but is stale (PV=${PV_DEV:-unknown}) — cleaning up..."
    # Remove all LVs, then VG, then detach PV loop device.
    for LV in $("${LVM_BIN}lvs" --noheadings -o lv_name "$VG" 2>/dev/null | tr -d ' '); do
        echo "  removing LV ${VG}/${LV}..."
        "${LVM_BIN}lvremove" -f "${VG}/${LV}" 2>/dev/null || true
    done
    "${LVM_BIN}vgchange" -an "$VG" 2>/dev/null || true
    "${LVM_BIN}vgremove" -ff "$VG" 2>/dev/null || true
    # Wipe VG metadata from the PV if still accessible.
    if [ -n "$PV_DEV" ] && [ -b "$PV_DEV" ]; then
        "${LVM_BIN}pvremove" -ff "$PV_DEV" 2>/dev/null || true
    fi
    if [ -n "$PV_DEV" ] && echo "$PV_DEV" | grep -q '^/dev/loop'; then
        losetup -d "$PV_DEV" 2>/dev/null || true
    fi
fi
if [ -f "$IMG" ]; then
    echo "Cleaning up stale image ${IMG}..."
    losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null || true
    rm -f "$IMG"
fi
# Detach any stale loop devices pointing to old LVM image files from
# previous host environments (visible via bind-mounted /dev).
for dev in $(losetup -a 2>/dev/null | grep 'e2e-lvm' | cut -d: -f1); do
    echo "Detaching stale loop device $dev (old LVM image)..."
    losetup -d "$dev" 2>/dev/null || true
done

# ── Step 3: create sparse loopback image ──────────────────────────────────
echo "Creating sparse image ${IMG} (${SIZE})..."
truncate -s "$SIZE" "$IMG"

# ── Step 4: attach image to a loop device ─────────────────────────────────
LOOP=$(losetup -f --show "$IMG")
echo "Attached loop device: ${LOOP}"

# Deferred cleanup: if any subsequent step fails, detach the loop device
# and remove the image so no dangling resources are left behind.
cleanup() {
    echo "ERROR during LVM setup — cleaning up ${LOOP} and ${IMG}" >&2
    losetup -d "$LOOP" 2>/dev/null || true
    rm -f "$IMG" 2>/dev/null || true
}
trap cleanup ERR

# ── Step 5: create Physical Volume and Volume Group ───────────────────────
echo "Creating PV on ${LOOP}..."
"${LVM_BIN}pvcreate" "$LOOP"

echo "Creating VG ${VG} on ${LOOP}..."
"${LVM_BIN}vgcreate" "$VG" "$LOOP"

# Ensure device-mapper nodes exist after VG creation (udev may not run in containers).
dmsetup mknodes 2>/dev/null || true
"${LVM_BIN}vgscan" --mknodes 2>/dev/null || true
%s
# Activate the VG so its block device symlinks appear under /dev/<vg>/
echo "Activating VG ${VG}..."
"${LVM_BIN}vgchange" -ay "$VG"

# Remove the ERR trap now that setup succeeded.
trap - ERR

echo "SUCCESS: LVM VG '${VG}' ready (loop=${LOOP}, image=${IMG})"
`,
		shellQuote(imagePath), shellQuote(vgName), shellQuote(imageSize),
		boolShell(reuseIfHealthy),
		thinPoolSection,
	)

	out, err := lvmDockerExecScript(ctx, dockerHost, containerName, script)
	if err != nil {
		return fmt.Errorf("SetupLoopbackLVMVG in container %q (vg=%s): %w\n  output: %s",
			containerName, vgName, err, strings.TrimSpace(out))
	}
	fmt.Fprintf(os.Stdout, "  lvm setup output:\n%s\n", indentLines(strings.TrimSpace(out), "    "))
	return nil
}

// TeardownLoopbackLVMVG tears down a VG that was created by SetupLoopbackLVMVG.
// It performs three steps and collects errors from all of them so that a single
// failure does not skip remaining cleanup:
//
//  1. Deactivate and remove the VG (vgchange -an + vgremove -f).
//  2. Detach the loop device that backs the VG (losetup -j -d).
//  3. Remove the loopback image file (rm -f).
//
// The function is a no-op for resources that are already absent (idempotent).
// Passing an empty vgName or imagePath skips the corresponding step.
func TeardownLoopbackLVMVG(
	ctx context.Context,
	dockerHost, containerName, vgName, imagePath string,
) error {
	script := fmt.Sprintf(`#!/bin/bash
# LVM VG teardown script — runs inside a Kind worker node container.
set -uo pipefail

VG=%s
IMG=%s
ERRORS=""

# Locate lvm2 binaries (may or may not be installed).
LVM_BIN=""
if   command -v vgremove >/dev/null 2>&1; then LVM_BIN=""
elif [ -x /usr/sbin/vgremove ];              then LVM_BIN="/usr/sbin/"
elif [ -x /sbin/vgremove ];                  then LVM_BIN="/sbin/"
fi

# ── Step 1: deactivate and remove the VG ─────────────────────────────────
if [ -n "$LVM_BIN" ] || command -v vgremove >/dev/null 2>&1; then
    if "${LVM_BIN}vgdisplay" "$VG" >/dev/null 2>&1; then
        echo "Deactivating VG ${VG}..."
        "${LVM_BIN}vgchange" -an "$VG" 2>/dev/null || true
        echo "Removing VG ${VG}..."
        if ! "${LVM_BIN}vgremove" -f "$VG" 2>/dev/null; then
            ERRORS="${ERRORS}; vgremove ${VG} failed"
        fi
    else
        echo "VG ${VG} not found — skipping removal"
    fi
else
    echo "lvm2 binaries not found — skipping VG removal"
fi

# ── Step 2: detach loop device(s) backing the image ───────────────────────
if [ -f "$IMG" ]; then
    echo "Detaching loop device(s) for ${IMG}..."
    LOOP_DEVS=$(losetup -j "$IMG" 2>/dev/null | cut -d: -f1)
    for DEV in $LOOP_DEVS; do
        if ! losetup -d "$DEV" 2>/dev/null; then
            ERRORS="${ERRORS}; losetup -d ${DEV} failed"
        fi
    done
fi

# ── Step 3: remove image file ─────────────────────────────────────────────
if [ -f "$IMG" ]; then
    echo "Removing image file ${IMG}..."
    if ! rm -f "$IMG"; then
        ERRORS="${ERRORS}; rm -f ${IMG} failed"
    fi
fi

if [ -n "$ERRORS" ]; then
    echo "ERRORS during teardown: ${ERRORS}" >&2
    exit 1
fi
echo "SUCCESS: LVM VG '${VG}' torn down (image=${IMG})"
`,
		shellQuote(vgName), shellQuote(imagePath),
	)

	out, err := lvmDockerExecScript(ctx, dockerHost, containerName, script)
	if err != nil {
		return fmt.Errorf("TeardownLoopbackLVMVG in container %q (vg=%s): %w\n  output: %s",
			containerName, vgName, err, strings.TrimSpace(out))
	}
	fmt.Fprintf(os.Stdout, "  lvm teardown output:\n%s\n", indentLines(strings.TrimSpace(out), "    "))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Host-exec variants (for standalone tests without a Kind cluster)
// ─────────────────────────────────────────────────────────────────────────────

// SetupKindLVMVG creates a loopback-backed LVM volume group (and optionally a
// thin pool) on the Docker HOST by running a bash script via the privileged
// DockerHostExec helper (nsenter into host namespaces).
//
// Unlike SetupLoopbackLVMVG (which runs inside a named container), this
// function runs directly on the Docker host.  It is designed for use in the
// standalone lvmbackend test package which has no Kind cluster.
//
// Parameters:
//
//	h               – DockerHostExec helper (must be non-nil, already started).
//	vgName          – LVM Volume Group name (e.g. "e2e-vg").
//	thinPoolName    – Thin pool LV name (empty = linear VG only).
//	imagePath       – Absolute path on the Docker HOST for the sparse image.
//	imageSize       – Size string for truncate(1), e.g. "4G".
//	kindNodeContainer – If non-empty, docker-exec into this container name to
//	                   make the loop device visible inside Kind.  Pass "" for
//	                   standalone tests.
//
// Returns the loop device path (e.g. "/dev/loop5") allocated by losetup.
func SetupKindLVMVG(
	ctx context.Context,
	h *DockerHostExec,
	vgName, thinPoolName, imagePath, imageSize string,
	kindNodeContainer string,
) (loopDev string, err error) {
	if h == nil {
		return "", fmt.Errorf("SetupKindLVMVG: DockerHostExec helper is nil")
	}

	thinPoolSection := ""
	if thinPoolName != "" {
		thinPoolSection = fmt.Sprintf(`
echo "Creating thin pool %s in VG %s..."
lvcreate -T -l '80%%FREE' -n %s %s
`,
			shellQuote(thinPoolName), shellQuote(vgName),
			shellQuote(thinPoolName), shellQuote(vgName),
		)
	}

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
IMG=%s
VG=%s
SIZE=%s

echo "=== SetupKindLVMVG: VG=${VG} img=${IMG} size=${SIZE} ==="

# Load device-mapper modules (best-effort).
modprobe dm_mod       2>/dev/null || true
modprobe dm_thin_pool 2>/dev/null || true

# Clean up stale state from a previous interrupted run.
if vgdisplay "$VG" >/dev/null 2>&1; then
    echo "Cleaning up stale VG ${VG}..."
    vgchange -an "$VG" 2>/dev/null || true
    vgremove -f  "$VG" 2>/dev/null || true
fi
if [ -f "$IMG" ]; then
    echo "Cleaning up stale image ${IMG}..."
    losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null || true
    rm -f "$IMG"
fi

# Create sparse loopback image.
echo "Creating sparse image ${IMG} (${SIZE})..."
truncate -s "$SIZE" "$IMG"

# Attach to a loop device.
LOOP=$(losetup -f --show "$IMG")
echo "LOOP_DEV=${LOOP}"

# Cleanup trap.
cleanup() {
    echo "ERROR during SetupKindLVMVG — cleaning up ${LOOP} and ${IMG}" >&2
    losetup -d "$LOOP" 2>/dev/null || true
    rm -f "$IMG" 2>/dev/null || true
}
trap cleanup ERR

pvcreate "$LOOP"
vgcreate "$VG" "$LOOP"
%s
vgchange -ay "$VG"
trap - ERR
echo "SUCCESS: VG '${VG}' ready on ${LOOP}"
`,
		shellQuote(imagePath), shellQuote(vgName), shellQuote(imageSize),
		thinPoolSection,
	)

	res, execErr := h.ExecOnHost(ctx, script)
	if execErr != nil || !res.Success() {
		combined := res.Stdout + res.Stderr
		if execErr != nil {
			return "", fmt.Errorf("SetupKindLVMVG (ExecOnHost): %w\n  output: %s", execErr, combined)
		}
		return "", fmt.Errorf("SetupKindLVMVG: script failed (exit %d)\n  output: %s",
			res.ExitCode, combined)
	}

	// Parse "LOOP_DEV=/dev/loopN" from script output.
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "LOOP_DEV=") {
			loopDev = strings.TrimPrefix(line, "LOOP_DEV=")
			break
		}
	}

	fmt.Fprintf(os.Stdout, "SetupKindLVMVG: VG %q ready (loop=%s)\n  output:\n%s\n",
		vgName, loopDev, indentLines(strings.TrimSpace(res.Stdout), "    "))
	return loopDev, nil
}

// TeardownKindLVMVG tears down a VG and loop device created by SetupKindLVMVG.
//
// All steps are attempted even when earlier steps fail; errors are collected and
// returned together.  This matches the best-effort teardown contract used by the
// rest of the e2e framework.
//
// Parameters:
//
//	h         – DockerHostExec helper (must be non-nil).
//	vgName    – LVM Volume Group name to remove.
//	loopDev   – Loop device path returned by SetupKindLVMVG (e.g. "/dev/loop5").
//	imagePath – Path of the sparse image file to remove.
func TeardownKindLVMVG(
	ctx context.Context,
	h *DockerHostExec,
	vgName, loopDev, imagePath string,
) error {
	if h == nil {
		return fmt.Errorf("TeardownKindLVMVG: DockerHostExec helper is nil")
	}

	script := fmt.Sprintf(`#!/bin/bash
set -uo pipefail
VG=%s
LOOP=%s
IMG=%s
ERRORS=""

echo "=== TeardownKindLVMVG: VG=${VG} loop=${LOOP} img=${IMG} ==="

# Step 1: deactivate and remove VG.
if vgdisplay "$VG" >/dev/null 2>&1; then
    echo "Deactivating VG ${VG}..."
    vgchange -an "$VG" 2>/dev/null || true
    echo "Removing VG ${VG}..."
    if ! vgremove -f "$VG" 2>/dev/null; then
        ERRORS="${ERRORS}; vgremove ${VG} failed"
    fi
else
    echo "VG ${VG} not found — skipping removal"
fi

# Step 2: detach loop device.
if [ -n "$LOOP" ] && losetup "$LOOP" >/dev/null 2>&1; then
    echo "Detaching loop device ${LOOP}..."
    if ! losetup -d "$LOOP" 2>/dev/null; then
        # Fallback: find by image path.
        losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null || \
            ERRORS="${ERRORS}; losetup -d ${LOOP} failed"
    fi
elif [ -f "$IMG" ]; then
    echo "Loop device not given or gone — detaching by image path..."
    losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null || true
fi

# Step 3: remove image file.
if [ -f "$IMG" ]; then
    echo "Removing image file ${IMG}..."
    if ! rm -f "$IMG"; then
        ERRORS="${ERRORS}; rm -f ${IMG} failed"
    fi
fi

if [ -n "$ERRORS" ]; then
    echo "ERRORS during teardown:${ERRORS}" >&2
    exit 1
fi
echo "SUCCESS: VG '${VG}' torn down"
`,
		shellQuote(vgName), shellQuote(loopDev), shellQuote(imagePath),
	)

	res, execErr := h.ExecOnHost(ctx, script)
	combined := res.Stdout + res.Stderr
	if execErr != nil {
		return fmt.Errorf("TeardownKindLVMVG (ExecOnHost): %w\n  output: %s", execErr, combined)
	}
	if !res.Success() {
		return fmt.Errorf("TeardownKindLVMVG: script failed (exit %d)\n  output: %s",
			res.ExitCode, combined)
	}
	fmt.Fprintf(os.Stdout, "TeardownKindLVMVG: VG %q torn down\n  output:\n%s\n",
		vgName, indentLines(strings.TrimSpace(res.Stdout), "    "))
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// lvmDockerExecScript runs a bash script inside the named Docker container by
// piping it via stdin to "docker exec -i <containerName> bash -s".
//
// DOCKER_HOST is injected into the subprocess environment when dockerHost is
// non-empty, allowing the function to target a remote Docker daemon.
//
// Returns combined stdout+stderr output and any exec error.
func lvmDockerExecScript(ctx context.Context, dockerHost, containerName, script string) (string, error) {
	args := []string{"exec", "-i", containerName, "bash", "-s"}
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec

	// Inject DOCKER_HOST so the subprocess reaches the correct daemon.
	env := os.Environ()
	const dockerHostKey = "DOCKER_HOST="
	if dockerHost != "" {
		// Replace any existing DOCKER_HOST entry.
		filtered := make([]string, 0, len(env)+1)
		for _, e := range env {
			if !strings.HasPrefix(e, dockerHostKey) {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = append(filtered, dockerHostKey+dockerHost)
	} else {
		// Strip DOCKER_HOST so Docker uses its default local socket.
		filtered := make([]string, 0, len(env))
		for _, e := range env {
			if !strings.HasPrefix(e, dockerHostKey) {
				filtered = append(filtered, e)
			}
		}
		cmd.Env = filtered
	}

	// Feed the script via stdin to avoid shell-quoting and length issues.
	cmd.Stdin = strings.NewReader(script)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker exec %s bash -s: %w", containerName, err)
	}
	return string(out), nil
}

// indentLines prepends prefix to every non-empty line in s.
// Used to format multi-line script output for readable log messages.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
