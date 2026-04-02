// Package lvm provides helpers for creating and destroying ephemeral LVM
// Volume Groups inside Kind container nodes during E2E tests.
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
//  3. "pvcreate" initialises the loop device as an LVM Physical Volume.
//  4. "vgcreate" creates the Volume Group on that PV.
//  5. Destroy runs: vgremove -f → pvremove -f → losetup -d → rm -f, collecting
//     all errors so teardown continues even when individual steps fail.
//
// Usage:
//
//	vg, err := lvm.CreateVG(ctx, lvm.CreateVGOptions{
//	    NodeContainer: "pillar-csi-e2e-abc123-control-plane",
//	    VGName:        "e2e-vg-abc123",
//	    SizeMiB:       512,
//	})
//	if err != nil { ... }
//	defer vg.Destroy(ctx)
package lvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// VG represents an ephemeral LVM Volume Group created inside a Kind container
// node. All fields are exported so callers can inspect VG state and pass the
// struct across package boundaries in test helpers.
type VG struct {
	// NodeContainer is the Docker container name of the Kind node that hosts
	// this Volume Group (e.g. "pillar-csi-e2e-abc123-control-plane").
	NodeContainer string

	// VGName is the LVM Volume Group name as given to "vgcreate".
	VGName string

	// ImagePath is the absolute path of the loop-device image file inside the
	// container (e.g. "/tmp/lvm-vg-e2e-vg-abc123.img").
	ImagePath string

	// LoopDevice is the path of the loop device inside the container
	// (e.g. "/dev/loop4") as returned by "losetup --find --show".
	LoopDevice string
}

// CreateVGOptions holds parameters for [CreateVG].
type CreateVGOptions struct {
	// NodeContainer is the Docker container name of the Kind node in which the
	// Volume Group should be created. Typically "<cluster>-control-plane" or
	// "<cluster>-worker". Must not be empty.
	NodeContainer string

	// VGName is the LVM Volume Group name to pass to "vgcreate". It must be
	// unique within the container for the lifetime of the test. Must not be
	// empty.
	VGName string

	// SizeMiB is the size of the loop device image in mebibytes.
	// Values ≤ 0 default to 512 MiB which is large enough for most E2E tests
	// while keeping setup time under a few seconds.
	SizeMiB int
}

// CreateVG creates an ephemeral LVM Volume Group inside a Kind container node.
//
// Steps:
//
//  1. Allocates a loop-device image file inside the container under /tmp.
//  2. Attaches the image as a loop device.
//  3. Initialises the loop device as an LVM Physical Volume via "pvcreate".
//  4. Creates a Volume Group on that PV via "vgcreate".
//
// All commands run via "docker exec <NodeContainer> …". The DOCKER_HOST
// environment variable is forwarded automatically because the docker command
// inherits the full parent-process environment.
//
// On any error, CreateVG attempts best-effort cleanup of already-created
// resources so that the container is left in a clean state. The caller is
// still responsible for calling [VG.Destroy] on success.
func CreateVG(ctx context.Context, opts CreateVGOptions) (*VG, error) {
	if strings.TrimSpace(opts.NodeContainer) == "" {
		return nil, fmt.Errorf("lvm: CreateVG: NodeContainer must not be empty")
	}
	if strings.TrimSpace(opts.VGName) == "" {
		return nil, fmt.Errorf("lvm: CreateVG: VGName must not be empty")
	}

	sizeMiB := opts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = 512
	}

	// Place the image under /tmp inside the container so that it is
	// automatically cleaned up if the container is killed before Destroy runs.
	imagePath := fmt.Sprintf("/tmp/lvm-vg-%s.img", opts.VGName)

	// ── Step 1: Allocate the loop device image ────────────────────────────────
	//
	// "dd if=/dev/zero …" is universally available in Kind nodes and avoids any
	// dependency on fallocate / truncate which may behave differently across
	// filesystem types.
	if _, err := containerExec(ctx, opts.NodeContainer,
		"dd", "if=/dev/zero",
		fmt.Sprintf("of=%s", imagePath),
		"bs=1M",
		fmt.Sprintf("count=%d", sizeMiB),
	); err != nil {
		return nil, fmt.Errorf("lvm: CreateVG: allocate image %s in %s: %w",
			imagePath, opts.NodeContainer, err)
	}

	// cleanupImage removes the image file on error paths.
	cleanupImage := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "rm", "-f", imagePath)
	}

	// ── Step 2: Attach image as loop device ───────────────────────────────────
	//
	// "--find --show" picks a free loop device and prints its path, e.g. "/dev/loop4".
	loopOut, err := containerExec(ctx, opts.NodeContainer,
		"losetup", "--find", "--show", imagePath)
	if err != nil {
		cleanupImage()
		return nil, fmt.Errorf("lvm: CreateVG: attach loop device for %s in %s: %w",
			imagePath, opts.NodeContainer, err)
	}
	loopDevice := strings.TrimSpace(loopOut)
	if loopDevice == "" {
		cleanupImage()
		return nil, fmt.Errorf("lvm: CreateVG: losetup returned empty loop device path in %s",
			opts.NodeContainer)
	}

	// cleanupLoop detaches the loop device on error paths.
	cleanupLoop := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "losetup", "-d", loopDevice)
		cleanupImage()
	}

	// ── Step 3: Initialise the loop device as an LVM Physical Volume ──────────
	//
	// "--yes" suppresses the "really initialise?" confirmation prompt.
	// "--force" allows pvcreate to overwrite an existing label (shouldn't happen
	// on a freshly attached loop device, but guards against stale state).
	if _, err := containerExec(ctx, opts.NodeContainer,
		"pvcreate", "--yes", "--force", loopDevice,
	); err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("lvm: CreateVG: pvcreate %s in %s: %w",
			loopDevice, opts.NodeContainer, err)
	}

	// cleanupPV removes the PV label on error paths after pvcreate succeeds.
	cleanupPV := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "pvremove", "-f", "-f", loopDevice)
		cleanupLoop()
	}

	// ── Step 4: Create the Volume Group ──────────────────────────────────────
	if _, err := containerExec(ctx, opts.NodeContainer,
		"vgcreate", opts.VGName, loopDevice,
	); err != nil {
		cleanupPV()
		return nil, fmt.Errorf("lvm: CreateVG: vgcreate %s on %s in %s: %w",
			opts.VGName, loopDevice, opts.NodeContainer, err)
	}

	return &VG{
		NodeContainer: opts.NodeContainer,
		VGName:        opts.VGName,
		ImagePath:     imagePath,
		LoopDevice:    loopDevice,
	}, nil
}

// Destroy idempotently destroys the LVM Volume Group, removes the Physical
// Volume label, and releases the loop device.
//
// Sequence:
//
//  1. "vgremove -f <VGName>" — tolerates "not found" so Destroy is safe to
//     call even after the VG has already been removed.
//  2. "pvremove -f -f <LoopDevice>" — removes the PV label; tolerates
//     "not a PV" errors for the same reason.
//  3. "losetup -d <LoopDevice>" — tolerates device-not-found errors.
//  4. "rm -f <ImagePath>" — removes the backing image file.
//
// All four steps always execute; errors are collected and returned together
// so that a failure in an early step does not prevent later steps from running.
// Calling Destroy on a nil *VG is a safe no-op.
func (v *VG) Destroy(ctx context.Context) error {
	if v == nil {
		return nil
	}

	var errs []error

	// Step 1: Remove the Volume Group.
	if _, err := containerExec(ctx, v.NodeContainer,
		"vgremove", "-f", v.VGName,
	); err != nil {
		if !isVGNotFoundError(err) {
			errs = append(errs, fmt.Errorf("vgremove -f %s: %w", v.VGName, err))
		}
		// Tolerate "not found" — VG may have already been removed.
	}

	// Step 2: Remove the Physical Volume label.
	if strings.TrimSpace(v.LoopDevice) != "" {
		if _, err := containerExec(ctx, v.NodeContainer,
			"pvremove", "-f", "-f", v.LoopDevice,
		); err != nil {
			if !isPVNotFoundError(err) {
				errs = append(errs, fmt.Errorf("pvremove -f -f %s: %w", v.LoopDevice, err))
			}
			// Tolerate "not a PV" — PV may have already been removed.
		}
	}

	// Step 3: Detach the loop device.
	if strings.TrimSpace(v.LoopDevice) != "" {
		if _, err := containerExec(ctx, v.NodeContainer,
			"losetup", "-d", v.LoopDevice,
		); err != nil {
			if !isLoopNotFoundError(err) {
				errs = append(errs, fmt.Errorf("losetup -d %s: %w", v.LoopDevice, err))
			}
			// Tolerate "no such device" — loop device may have already been detached.
		}
	}

	// Step 4: Remove the image file.
	if strings.TrimSpace(v.ImagePath) != "" {
		if _, err := containerExec(ctx, v.NodeContainer,
			"rm", "-f", v.ImagePath,
		); err != nil {
			errs = append(errs, fmt.Errorf("rm -f %s: %w", v.ImagePath, err))
		}
	}

	return errors.Join(errs...)
}

// Description returns a human-readable identifier for this LVM Volume Group,
// satisfying the [registry.Resource] interface.
//
// Example output: `lvm vg "e2e-vg-abc123" on container "pillar-csi-e2e-abc123-control-plane"`
func (v *VG) Description() string {
	return fmt.Sprintf("lvm vg %q on container %q", v.VGName, v.NodeContainer)
}

// VGExists checks whether an LVM Volume Group with the given name currently
// exists inside the container by running "vgs <vgName>".
//
// Returns (true, nil) if the VG exists, (false, nil) if it does not, and
// (false, err) for unexpected errors.
func VGExists(ctx context.Context, nodeContainer, vgName string) (bool, error) {
	_, err := containerExec(ctx, nodeContainer, "vgs", "--noheadings", vgName)
	if err != nil {
		if isVGNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lvm: VGExists: %w", err)
	}
	return true, nil
}

// VGAttrs returns the LVM Volume Group attribute flags as reported by
// "vgs --noheadings -o vg_attr <vgName>" inside the container.
//
// The returned string is in the same 6-character format as the vg_attr column
// of vgs(8). Each character encodes a different property:
//
//   - [0] permissions:        'w'=writable, 'r'=read-only
//   - [1] resizeable:         'z'=resizeable, '-'=not resizeable
//   - [2] exported:           'x'=exported, '-'=not exported
//   - [3] partial:            'p'=one or more PVs missing, '-'=all PVs present
//   - [4] allocation policy:  'c','l','n','a','i'
//   - [5] cluster:            'c'=clustered, '-'=not clustered
//
// Example: "wz--n-" represents a normal writable resizeable VG with normal
// allocation policy and no clustering.
//
// Returns an error when the VG does not exist or the docker exec call fails.
//
// Use [VerifyActive] when you need a simple pass/fail readiness check.
func VGAttrs(ctx context.Context, nodeContainer, vgName string) (string, error) {
	if strings.TrimSpace(nodeContainer) == "" {
		return "", fmt.Errorf("lvm: VGAttrs: nodeContainer must not be empty")
	}
	if strings.TrimSpace(vgName) == "" {
		return "", fmt.Errorf("lvm: VGAttrs: vgName must not be empty")
	}

	out, err := containerExec(ctx, nodeContainer,
		"vgs", "--noheadings", "-o", "vg_attr", vgName)
	if err != nil {
		return "", fmt.Errorf("lvm: VGAttrs: vgs --noheadings -o vg_attr %s in %s: %w",
			vgName, nodeContainer, err)
	}
	return strings.TrimSpace(out), nil
}

// VerifyActive checks that the named LVM Volume Group is in a writable,
// non-partial, non-exported state inside the given Kind container node.
//
// It runs "vgs --noheadings -o vg_attr <vgName>" via docker exec and checks
// the attribute string:
//
//   - Position 0 must be 'w' (writable, not read-only).
//   - Position 2 must not be 'x' (not exported — exported VGs are unusable on
//     the current host).
//   - Position 3 must not be 'p' (no partial PVs — partial VGs cannot reliably
//     create new LVs).
//
// Any check failure returns a descriptive error that includes the VG name, the
// container name, the specific failing attribute position, and the full
// attribute string so that failures can be diagnosed without re-running docker
// exec manually.
//
// Typical call-site after [CreateVG]:
//
//	vg, err := lvm.CreateVG(ctx, opts)
//	if err != nil { ... }
//	if err := lvm.VerifyActive(ctx, opts.NodeContainer, opts.VGName); err != nil { ... }
func VerifyActive(ctx context.Context, nodeContainer, vgName string) error {
	attrs, err := VGAttrs(ctx, nodeContainer, vgName)
	if err != nil {
		return fmt.Errorf("lvm: VerifyActive: %w", err)
	}

	// vg_attr is a 6-character field. Require at least 4 characters to safely
	// index positions 0–3. Fewer characters indicates an unexpected vgs output
	// format (e.g. old LVM2 version).
	if len(attrs) < 4 {
		return fmt.Errorf("lvm: VerifyActive: vg %q in container %q returned "+
			"unexpected attribute string %q (want at least 4 characters)",
			vgName, nodeContainer, attrs)
	}

	// Position 0: permissions — 'w'=writable, 'r'=read-only.
	// A read-only VG cannot be used to create new LVs.
	if attrs[0] != 'w' {
		return fmt.Errorf("lvm: VerifyActive: vg %q in container %q is not writable: "+
			"attr[0]=%q (want 'w'), full attrs=%q",
			vgName, nodeContainer, string(attrs[0]), attrs)
	}

	// Position 2: exported — 'x'=exported, '-'=not exported.
	// Exported VGs are locked for use on a different host; operations will fail.
	if attrs[2] == 'x' {
		return fmt.Errorf("lvm: VerifyActive: vg %q in container %q is exported "+
			"(attr[2]='x') — VG is not usable on this host, full attrs=%q",
			vgName, nodeContainer, attrs)
	}

	// Position 3: partial — 'p'=one or more PVs missing, '-'=all PVs present.
	// A partial VG may silently fail LV creation or produce incomplete data.
	if attrs[3] == 'p' {
		return fmt.Errorf("lvm: VerifyActive: vg %q in container %q has partial PVs "+
			"(attr[3]='p') — one or more backing devices are missing, full attrs=%q",
			vgName, nodeContainer, attrs)
	}

	return nil
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
		return "", fmt.Errorf("lvm: containerExec: container name must not be empty")
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

// isVGNotFoundError returns true when err looks like a "VG not found" error
// from an LVM command. The check is intentionally specific to LVM error text
// so that unrelated "not found" messages (e.g. "command not found") are not
// misidentified.
func isVGNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return (strings.Contains(msg, "volume group") && strings.Contains(msg, "not found")) ||
		strings.Contains(msg, "cannot find volume group") ||
		strings.Contains(msg, "vg not found") ||
		strings.Contains(msg, "failed to find vg")
}

// isPVNotFoundError returns true when err indicates that the device is not an
// LVM Physical Volume or no longer exists.
func isPVNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no physical volume label") ||
		strings.Contains(msg, "not a pv") ||
		strings.Contains(msg, "no pv label") ||
		strings.Contains(msg, "device not found") ||
		strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "failed to find device")
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
