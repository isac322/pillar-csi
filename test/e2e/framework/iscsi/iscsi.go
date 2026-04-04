// Package iscsi provides helpers for creating and destroying ephemeral iSCSI
// targets inside Kind container nodes during E2E tests.
//
// All storage operations run inside the Kind container via "docker exec" — no
// storage operations are performed on the host. DOCKER_HOST is read from the
// environment variable only; it is never hardcoded.
//
// Design summary:
//
//  1. A loop-device image file is created inside the container at a path under
//     /tmp so that it is automatically cleaned up even if Destroy is never called.
//  2. The image is attached as a loop device via "losetup --find --show".
//  3. An available TID is allocated by parsing "tgtadm --lld iscsi --mode target
//     --op show" output (max existing TID + 1, or 1 if none exist).
//  4. "tgtadm" creates the target, adds a LUN backed by the loop device, and
//     binds it to ALL initiator addresses.
//  5. Destroy runs: delete LUN → delete target → losetup -d → rm -f, collecting
//     all errors so teardown continues even when individual steps fail.
//
// Usage:
//
//	target, err := iscsi.CreateTarget(ctx, iscsi.CreateTargetOptions{
//	    NodeContainer: "pillar-csi-e2e-abc123-control-plane",
//	    IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:abc123test",
//	    SizeMiB:       512,
//	})
//	if err != nil { ... }
//	defer target.Destroy(ctx)
package iscsi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Target represents an ephemeral iSCSI target created inside a Kind container
// node. All fields are exported so callers can inspect target state and pass
// the struct across package boundaries in test helpers.
type Target struct {
	// NodeContainer is the Docker container name of the Kind node that hosts
	// this target (e.g. "pillar-csi-e2e-abc123-control-plane").
	NodeContainer string

	// IQN is the iSCSI Qualified Name of the target
	// (e.g. "iqn.2026-01.com.bhyoo.pillar-csi:abc123test").
	IQN string

	// ImagePath is the absolute path of the loop-device image file inside the
	// container (e.g. "/tmp/iscsi-target-abc123.img").
	ImagePath string

	// LoopDevice is the path of the loop device inside the container
	// (e.g. "/dev/loop4") as returned by "losetup --find --show".
	LoopDevice string

	// TID is the tgtadm target ID assigned to this iSCSI target.
	TID int
}

// CreateTargetOptions holds parameters for [CreateTarget].
type CreateTargetOptions struct {
	// NodeContainer is the Docker container name of the Kind node in which the
	// target should be created. Typically "<cluster>-control-plane" or
	// "<cluster>-worker". Must not be empty.
	NodeContainer string

	// IQN is the iSCSI Qualified Name to use for the target. It must be unique
	// within the container for the lifetime of the test. Must not be empty.
	// Recommended format: "iqn.2026-01.com.bhyoo.pillar-csi:<unique-suffix>"
	IQN string

	// SizeMiB is the size of the loop device image in mebibytes.
	// Values ≤ 0 default to 512 MiB which is large enough for most E2E tests
	// while keeping setup time under a few seconds.
	SizeMiB int
}

// CreateTarget creates an ephemeral iSCSI target inside a Kind container node.
//
// Steps:
//
//  1. Allocates a sparse loop-device image file inside the container under /tmp.
//  2. Attaches the image as a loop device (with mknod pre-creation for safety).
//  3. Allocates a target ID (TID) by inspecting existing tgtadm targets.
//  4. Creates the iSCSI target via "tgtadm".
//  5. Adds LUN 1 backed by the loop device.
//  6. Binds the target to all initiator addresses.
//
// All commands run via "docker exec <NodeContainer> …". The DOCKER_HOST
// environment variable is forwarded automatically because the docker command
// inherits the full parent-process environment.
//
// On any error, CreateTarget attempts best-effort cleanup of already-created
// resources so that the container is left in a clean state. The caller is
// still responsible for calling [Target.Destroy] on success.
func CreateTarget(ctx context.Context, opts CreateTargetOptions) (*Target, error) {
	if strings.TrimSpace(opts.NodeContainer) == "" {
		return nil, fmt.Errorf("iscsi: CreateTarget: NodeContainer must not be empty")
	}
	if strings.TrimSpace(opts.IQN) == "" {
		return nil, fmt.Errorf("iscsi: CreateTarget: IQN must not be empty")
	}

	sizeMiB := opts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = 512
	}

	// Derive a safe filename suffix from the IQN by replacing non-alphanumeric
	// characters with hyphens. This keeps the image path readable and avoids
	// shell-special characters.
	iqnSlug := iqnToSlug(opts.IQN)

	// Place the image under /tmp inside the container so that it is
	// automatically cleaned up if the container is killed before Destroy runs.
	imagePath := fmt.Sprintf("/tmp/iscsi-target-%s.img", iqnSlug)

	// ── Step 1: Allocate the loop device image ────────────────────────────────
	//
	// "truncate -s <sizeMiB>M" creates a sparse file instantly — no actual disk
	// blocks are allocated until data is written. This keeps iSCSI target setup
	// time constant (~50 ms) regardless of size, consistent with the LVM and ZFS
	// framework helpers which also use sparse files per the infrastructure SSOT.
	if _, err := containerExec(ctx, opts.NodeContainer,
		"truncate", "-s", fmt.Sprintf("%dM", sizeMiB), imagePath,
	); err != nil {
		return nil, fmt.Errorf("iscsi: CreateTarget: allocate image %s in %s: %w",
			imagePath, opts.NodeContainer, err)
	}

	// cleanupImage removes the image file on error paths.
	cleanupImage := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "rm", "-f", imagePath)
	}

	// ── Step 2: Attach image as loop device ───────────────────────────────────
	//
	// "losetup --find --show" picks a free loop device and prints its path.
	//
	// In containers (Kind nodes, CI environments) the next free loop number may
	// exceed the pre-created /dev/loop* device nodes. Kind containers run with
	// --privileged so they share the HOST's /dev namespace — if the host has
	// /dev/loop0..loop129 in use, losetup --find returns "/dev/loop130" but
	// /dev/loop130 does not exist as a device node in the container, causing:
	//   "losetup: /path/to/file.img: failed to set up loop device: No such file
	//    or directory"
	//
	// Fix: use a bash compound command that pre-creates /dev/loopN nodes via
	// mknod before calling losetup --find --show, matching the approach used by
	// the LVM and ZFS framework helpers per the infrastructure SSOT.
	const losetupScript = `set -e; ` +
		`_free=$(losetup -f 2>/dev/null || echo /dev/loop0); ` +
		`_max=${_free#/dev/loop}; ` +
		`_max=$((_max + 5)); ` +
		`for _n in $(seq 0 $_max); do ` +
		`[ -e "/dev/loop$_n" ] || mknod "/dev/loop$_n" b 7 "$_n" 2>/dev/null || true; ` +
		`done; ` +
		`losetup --find --show "$1"`
	loopOut, err := containerExec(ctx, opts.NodeContainer,
		"bash", "-c", losetupScript, "bash", imagePath)
	if err != nil {
		cleanupImage()
		return nil, fmt.Errorf("iscsi: CreateTarget: attach loop device for %s in %s: %w",
			imagePath, opts.NodeContainer, err)
	}
	loopDevice := strings.TrimSpace(loopOut)
	if loopDevice == "" {
		cleanupImage()
		return nil, fmt.Errorf("iscsi: CreateTarget: losetup returned empty loop device path in %s",
			opts.NodeContainer)
	}

	// cleanupLoop detaches the loop device on error paths.
	cleanupLoop := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "losetup", "-d", loopDevice)
		cleanupImage()
	}

	// ── Step 3: Allocate a target ID ─────────────────────────────────────────
	//
	// tgtadm requires a numeric TID that is unique within the tgtd instance
	// running inside the container. We parse the existing target list to find
	// the next available TID.
	tid, err := allocateTID(ctx, opts.NodeContainer)
	if err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("iscsi: CreateTarget: allocate TID in %s: %w",
			opts.NodeContainer, err)
	}

	// cleanupTarget removes the tgtadm target on error paths.
	cleanupTarget := func() {
		_, _ = containerExec(ctx, opts.NodeContainer,
			"tgtadm", "--lld", "iscsi", "--mode", "target",
			"--op", "delete", "--tid", strconv.Itoa(tid))
		cleanupLoop()
	}

	// ── Step 4: Create the iSCSI target ──────────────────────────────────────
	if _, err := containerExec(ctx, opts.NodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "target",
		"--op", "new", "--tid", strconv.Itoa(tid),
		"--targetname", opts.IQN,
	); err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("iscsi: CreateTarget: create target %s (tid=%d) in %s: %w",
			opts.IQN, tid, opts.NodeContainer, err)
	}

	// ── Step 5: Add LUN 1 backed by the loop device ───────────────────────────
	if _, err := containerExec(ctx, opts.NodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "logicalunit",
		"--op", "new", "--tid", strconv.Itoa(tid),
		"--lun", "1", "--backing-store", loopDevice,
	); err != nil {
		cleanupTarget()
		return nil, fmt.Errorf("iscsi: CreateTarget: add LUN 1 for tid=%d (%s) in %s: %w",
			tid, opts.IQN, opts.NodeContainer, err)
	}

	// ── Step 6: Bind target to all initiator addresses ────────────────────────
	if _, err := containerExec(ctx, opts.NodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "target",
		"--op", "bind", "--tid", strconv.Itoa(tid),
		"--initiator-address", "ALL",
	); err != nil {
		cleanupTarget()
		return nil, fmt.Errorf("iscsi: CreateTarget: bind tid=%d (%s) in %s: %w",
			tid, opts.IQN, opts.NodeContainer, err)
	}

	return &Target{
		NodeContainer: opts.NodeContainer,
		IQN:           opts.IQN,
		ImagePath:     imagePath,
		LoopDevice:    loopDevice,
		TID:           tid,
	}, nil
}

// Destroy idempotently destroys the iSCSI target, removes the LUN, and
// releases the loop device.
//
// Sequence:
//
//  1. "tgtadm … --mode logicalunit --op delete --tid <TID> --lun 1" — tolerates
//     "not found" errors so Destroy is safe to call even if the LUN was never
//     added or has already been removed.
//  2. "tgtadm … --mode target --op delete --tid <TID>" — tolerates "not found"
//     for the same reason.
//  3. "losetup -d <LoopDevice>" — tolerates device-not-found errors.
//  4. "rm -f <ImagePath>" — removes the backing image file.
//
// All four steps always execute; errors are collected and returned together
// so that a failure in an early step does not prevent later steps from running.
// Calling Destroy on a nil *Target is a safe no-op.
func (t *Target) Destroy(ctx context.Context) error {
	if t == nil {
		return nil
	}

	var errs []error

	// Step 1: Remove LUN 1.
	if t.TID > 0 {
		if _, err := containerExec(ctx, t.NodeContainer,
			"tgtadm", "--lld", "iscsi", "--mode", "logicalunit",
			"--op", "delete", "--tid", strconv.Itoa(t.TID),
			"--lun", "1",
		); err != nil {
			if !isTargetNotFoundError(err) {
				errs = append(errs, fmt.Errorf("tgtadm delete lun tid=%d: %w", t.TID, err))
			}
			// Tolerate "not found" — target/LUN may have already been removed.
		}
	}

	// Step 2: Delete the iSCSI target.
	if t.TID > 0 {
		if _, err := containerExec(ctx, t.NodeContainer,
			"tgtadm", "--lld", "iscsi", "--mode", "target",
			"--op", "delete", "--tid", strconv.Itoa(t.TID),
		); err != nil {
			if !isTargetNotFoundError(err) {
				errs = append(errs, fmt.Errorf("tgtadm delete target tid=%d: %w", t.TID, err))
			}
			// Tolerate "can't find the target" / "no such target".
		}
	}

	// Step 3: Detach the loop device.
	if strings.TrimSpace(t.LoopDevice) != "" {
		if _, err := containerExec(ctx, t.NodeContainer,
			"losetup", "-d", t.LoopDevice,
		); err != nil {
			if !isLoopNotFoundError(err) {
				errs = append(errs, fmt.Errorf("losetup -d %s: %w", t.LoopDevice, err))
			}
			// Tolerate "no such device" — loop device may have already been detached.
		}
	}

	// Step 4: Remove the image file.
	if strings.TrimSpace(t.ImagePath) != "" {
		if _, err := containerExec(ctx, t.NodeContainer,
			"rm", "-f", t.ImagePath,
		); err != nil {
			errs = append(errs, fmt.Errorf("rm -f %s: %w", t.ImagePath, err))
		}
	}

	return errors.Join(errs...)
}

// Description returns a human-readable identifier for this iSCSI target,
// satisfying the [registry.Resource] interface.
//
// Example output: `iscsi target "iqn.2026-01.com.bhyoo.pillar-csi:abc123" (tid=3) on container "pillar-csi-e2e-abc123-control-plane"`
func (t *Target) Description() string {
	return fmt.Sprintf("iscsi target %q (tid=%d) on container %q", t.IQN, t.TID, t.NodeContainer)
}

// TargetExists checks whether an iSCSI target with the given TID currently
// exists inside the container by running "tgtadm --lld iscsi --mode target
// --op show --tid <tid>".
//
// Returns (true, nil) if the target exists, (false, nil) if it does not, and
// (false, err) for unexpected errors.
func TargetExists(ctx context.Context, nodeContainer string, tid int) (bool, error) {
	if tid <= 0 {
		return false, nil
	}
	_, err := containerExec(ctx, nodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "target",
		"--op", "show", "--tid", strconv.Itoa(tid))
	if err != nil {
		if isTargetNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("iscsi: TargetExists: %w", err)
	}
	return true, nil
}

// KindNodeContainerName returns the Docker container name for a node in a Kind
// cluster following Kind's default naming convention.
//
//   - nodeIndex == 0 → "<clusterName>-control-plane"
//   - nodeIndex == 1 → "<clusterName>-worker"
//   - nodeIndex >= 2 → "<clusterName>-worker<nodeIndex>"  (e.g. "…-worker2")
//
// nodeIndex is 0-based.
func KindNodeContainerName(clusterName string, nodeIndex int) string {
	switch {
	case nodeIndex < 0:
		return ""
	case nodeIndex == 0:
		return clusterName + "-control-plane"
	case nodeIndex == 1:
		return clusterName + "-worker"
	default:
		return fmt.Sprintf("%s-worker%d", clusterName, nodeIndex)
	}
}

// ─── internal helpers ────────────────────────────────────────────────────────

// allocateTID returns the next available tgtadm TID inside the container.
//
// It runs "tgtadm --lld iscsi --mode target --op show" and parses lines of the
// form "Target <N>: <IQN>" to find the maximum existing TID, then returns
// maxTID + 1. If no targets exist, it returns 1.
func allocateTID(ctx context.Context, nodeContainer string) (int, error) {
	out, err := containerExec(ctx, nodeContainer,
		"tgtadm", "--lld", "iscsi", "--mode", "target", "--op", "show")
	if err != nil {
		// tgtadm exits non-zero when no targets exist; treat that as TID=1.
		return 1, nil //nolint:nilerr
	}

	maxTID := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		// Lines look like: "Target 1: iqn.2026-01.com.bhyoo.pillar-csi:foo"
		if !strings.HasPrefix(line, "Target ") {
			continue
		}
		rest := strings.TrimPrefix(line, "Target ")
		// rest is "<N>: <IQN>"
		colonIdx := strings.Index(rest, ":")
		if colonIdx < 0 {
			continue
		}
		tidStr := strings.TrimSpace(rest[:colonIdx])
		tid, convErr := strconv.Atoi(tidStr)
		if convErr != nil {
			continue
		}
		if tid > maxTID {
			maxTID = tid
		}
	}

	return maxTID + 1, nil
}

// iqnToSlug converts an IQN to a filesystem-safe slug by replacing all
// characters that are not alphanumeric or hyphens with hyphens and collapsing
// consecutive hyphens into one.
func iqnToSlug(iqn string) string {
	var b strings.Builder
	b.Grow(len(iqn))

	hyphen := false
	for _, r := range strings.ToLower(iqn) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			hyphen = false
			continue
		}
		if !hyphen {
			b.WriteByte('-')
			hyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "target"
	}
	// Keep to a reasonable length to avoid exceeding filesystem limits.
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return strings.Trim(slug, "-")
}

// containerExec runs a command inside a Docker container via "docker exec".
//
// DOCKER_HOST is forwarded automatically: cmd.Env is set to os.Environ() so
// that any DOCKER_HOST value in the calling process's environment is passed to
// the docker client. This is the only supported way to configure the Docker
// daemon endpoint — the function never reads or hardcodes the daemon address.
//
// Returns (stdout, nil) on success or ("", error) on failure with the
// container's stderr included in the error message.
func containerExec(ctx context.Context, container string, args ...string) (string, error) {
	if strings.TrimSpace(container) == "" {
		return "", fmt.Errorf("iscsi: containerExec: container name must not be empty")
	}

	dockerArgs := append([]string{"exec", container}, args...)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...) //nolint:gosec

	// Propagate DOCKER_HOST (and all other env vars) from the parent process.
	// os.Environ() includes DOCKER_HOST when it is set; when it is not set, the
	// Docker client falls back to the default daemon socket. We never hardcode
	// a daemon address.
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

// isTargetNotFoundError returns true when err looks like a "target not found"
// error from a tgtadm command.
func isTargetNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't find the target") ||
		strings.Contains(msg, "no such target") ||
		strings.Contains(msg, "target not found") ||
		strings.Contains(msg, "invalid target id")
}

// isLoopNotFoundError returns true when err indicates that the loop device no
// longer exists or was never a loop device.
func isLoopNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "not a block device") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "invalid argument")
}
