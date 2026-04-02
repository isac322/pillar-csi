// Package zfs provides helpers for creating and destroying ephemeral ZFS pools
// inside Kind container nodes during E2E tests.
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
//  3. "zpool create" creates the pool on the loop device.
//  4. Destroy runs: zpool destroy -f → losetup -d → rm -f, collecting all
//     errors so teardown continues even when individual steps fail.
//
// Usage:
//
//	pool, err := zfs.CreatePool(ctx, zfs.CreatePoolOptions{
//	    NodeContainer: "pillar-csi-e2e-abc123-control-plane",
//	    PoolName:      "e2e-tank-abc123",
//	    SizeMiB:       512,
//	})
//	if err != nil { ... }
//	defer pool.Destroy(ctx)
package zfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Pool represents an ephemeral ZFS pool created inside a Kind container node.
// All fields are exported so callers can inspect pool state and pass the struct
// across package boundaries in test helpers.
type Pool struct {
	// NodeContainer is the Docker container name of the Kind node that hosts
	// this pool (e.g. "pillar-csi-e2e-abc123-control-plane").
	NodeContainer string

	// PoolName is the ZFS pool name as given to "zpool create".
	PoolName string

	// ImagePath is the absolute path of the loop-device image file inside the
	// container (e.g. "/tmp/zfs-pool-e2e-tank-abc123.img").
	ImagePath string

	// LoopDevice is the path of the loop device inside the container
	// (e.g. "/dev/loop4") as returned by "losetup --find --show".
	LoopDevice string
}

// CreatePoolOptions holds parameters for [CreatePool].
type CreatePoolOptions struct {
	// NodeContainer is the Docker container name of the Kind node in which the
	// pool should be created. Typically "<cluster>-control-plane" or
	// "<cluster>-worker". Must not be empty.
	NodeContainer string

	// PoolName is the ZFS pool name to pass to "zpool create". It must be
	// unique within the container for the lifetime of the test. Must not be
	// empty.
	PoolName string

	// SizeMiB is the size of the loop device image in mebibytes.
	// Values ≤ 0 default to 512 MiB which is large enough for most E2E tests
	// while keeping setup time under a few seconds.
	SizeMiB int
}

// CreatePool creates an ephemeral ZFS pool inside a Kind container node.
//
// Steps:
//
//  1. Allocates a loop-device image file inside the container under /tmp.
//  2. Attaches the image as a loop device.
//  3. Creates a ZFS pool on the loop device.
//
// All commands run via "docker exec <NodeContainer> …". The DOCKER_HOST
// environment variable is forwarded automatically because the docker command
// inherits the full parent-process environment.
//
// On any error, CreatePool attempts best-effort cleanup of already-created
// resources so that the container is left in a clean state. The caller is
// still responsible for calling [Pool.Destroy] on success.
func CreatePool(ctx context.Context, opts CreatePoolOptions) (*Pool, error) {
	if strings.TrimSpace(opts.NodeContainer) == "" {
		return nil, fmt.Errorf("zfs: CreatePool: NodeContainer must not be empty")
	}
	if strings.TrimSpace(opts.PoolName) == "" {
		return nil, fmt.Errorf("zfs: CreatePool: PoolName must not be empty")
	}

	sizeMiB := opts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = 512
	}

	// Place the image under /tmp inside the container so that it is
	// automatically cleaned up if the container is killed before Destroy runs.
	imagePath := fmt.Sprintf("/tmp/zfs-pool-%s.img", opts.PoolName)

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
		return nil, fmt.Errorf("zfs: CreatePool: allocate image %s in %s: %w",
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
		return nil, fmt.Errorf("zfs: CreatePool: attach loop device for %s in %s: %w",
			imagePath, opts.NodeContainer, err)
	}
	loopDevice := strings.TrimSpace(loopOut)
	if loopDevice == "" {
		cleanupImage()
		return nil, fmt.Errorf("zfs: CreatePool: losetup returned empty loop device path in %s",
			opts.NodeContainer)
	}

	// cleanupLoop detaches the loop device on error paths.
	cleanupLoop := func() {
		_, _ = containerExec(ctx, opts.NodeContainer, "losetup", "-d", loopDevice)
		cleanupImage()
	}

	// ── Step 3: Create the ZFS pool ───────────────────────────────────────────
	if _, err := containerExec(ctx, opts.NodeContainer,
		"zpool", "create", opts.PoolName, loopDevice,
	); err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("zfs: CreatePool: zpool create %s on %s in %s: %w",
			opts.PoolName, loopDevice, opts.NodeContainer, err)
	}

	return &Pool{
		NodeContainer: opts.NodeContainer,
		PoolName:      opts.PoolName,
		ImagePath:     imagePath,
		LoopDevice:    loopDevice,
	}, nil
}

// Destroy idempotently destroys the ZFS pool and releases the loop device.
//
// Sequence:
//
//  1. "zpool destroy -f <PoolName>" — tolerates "no such pool" so Destroy is
//     safe to call even after the pool has already been destroyed.
//  2. "losetup -d <LoopDevice>" — tolerates device-not-found errors.
//  3. "rm -f <ImagePath>" — removes the backing image file.
//
// All three steps always execute; errors are collected and returned together
// so that a failure in step 1 does not prevent steps 2 and 3 from running.
// Calling Destroy on a nil *Pool is a safe no-op.
func (p *Pool) Destroy(ctx context.Context) error {
	if p == nil {
		return nil
	}

	var errs []error

	// Step 1: Destroy the ZFS pool.
	if _, err := containerExec(ctx, p.NodeContainer,
		"zpool", "destroy", "-f", p.PoolName,
	); err != nil {
		if !isZPoolNotFoundError(err) {
			errs = append(errs, fmt.Errorf("zpool destroy -f %s: %w", p.PoolName, err))
		}
		// Tolerate "no such pool" — pool may have already been destroyed.
	}

	// Step 2: Detach the loop device.
	if strings.TrimSpace(p.LoopDevice) != "" {
		if _, err := containerExec(ctx, p.NodeContainer,
			"losetup", "-d", p.LoopDevice,
		); err != nil {
			if !isLoopNotFoundError(err) {
				errs = append(errs, fmt.Errorf("losetup -d %s: %w", p.LoopDevice, err))
			}
			// Tolerate "no such device" — loop device may have already been detached.
		}
	}

	// Step 3: Remove the image file.
	if strings.TrimSpace(p.ImagePath) != "" {
		if _, err := containerExec(ctx, p.NodeContainer,
			"rm", "-f", p.ImagePath,
		); err != nil {
			errs = append(errs, fmt.Errorf("rm -f %s: %w", p.ImagePath, err))
		}
	}

	return errors.Join(errs...)
}

// Description returns a human-readable identifier for this ZFS pool,
// satisfying the [registry.Resource] interface.
//
// Example output: `zfs pool "e2e-tank-abc123" on container "pillar-csi-e2e-abc123-control-plane"`
func (p *Pool) Description() string {
	return fmt.Sprintf("zfs pool %q on container %q", p.PoolName, p.NodeContainer)
}

// PoolExists checks whether a ZFS pool with the given name currently exists
// inside the container by running "zpool list <poolName>".
//
// Returns (true, nil) if the pool exists, (false, nil) if it does not, and
// (false, err) for unexpected errors.
func PoolExists(ctx context.Context, nodeContainer, poolName string) (bool, error) {
	_, err := containerExec(ctx, nodeContainer, "zpool", "list", poolName)
	if err != nil {
		if isZPoolNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("zfs: PoolExists: %w", err)
	}
	return true, nil
}

// PoolState returns the health state of the ZFS pool as reported by
// "zpool list -H -o health <poolName>" inside the container.
//
// Typical return values are "ONLINE", "DEGRADED", "FAULTED", "OFFLINE",
// "UNAVAIL", or "REMOVED". Returns an error when the pool does not exist or
// the docker exec call fails.
//
// Use [VerifyOnline] when you need a simple pass/fail check.
func PoolState(ctx context.Context, nodeContainer, poolName string) (string, error) {
	if strings.TrimSpace(nodeContainer) == "" {
		return "", fmt.Errorf("zfs: PoolState: nodeContainer must not be empty")
	}
	if strings.TrimSpace(poolName) == "" {
		return "", fmt.Errorf("zfs: PoolState: poolName must not be empty")
	}

	out, err := containerExec(ctx, nodeContainer,
		"zpool", "list", "-H", "-o", "health", poolName)
	if err != nil {
		return "", fmt.Errorf("zfs: PoolState: zpool list -H -o health %s in %s: %w",
			poolName, nodeContainer, err)
	}
	return strings.TrimSpace(out), nil
}

// VerifyOnline checks that the named ZFS pool is in the ONLINE state inside the
// given Kind container node.
//
// It runs "zpool list -H -o health <poolName>" via docker exec and returns nil
// only when the reported state is exactly "ONLINE". Any other state — including
// "DEGRADED", "FAULTED", "OFFLINE", "UNAVAIL", or "REMOVED" — is treated as an
// error so that tests are not silently run against a degraded pool.
//
// The function returns a descriptive error message that includes both the
// container name and the actual health value to aid diagnostics.
//
// Typical call-site after [CreatePool]:
//
//	pool, err := zfs.CreatePool(ctx, opts)
//	if err != nil { ... }
//	if err := zfs.VerifyOnline(ctx, opts.NodeContainer, opts.PoolName); err != nil { ... }
func VerifyOnline(ctx context.Context, nodeContainer, poolName string) error {
	state, err := PoolState(ctx, nodeContainer, poolName)
	if err != nil {
		return fmt.Errorf("zfs: VerifyOnline: %w", err)
	}
	if state != "ONLINE" {
		return fmt.Errorf("zfs: VerifyOnline: pool %q in container %q is %q, want ONLINE",
			poolName, nodeContainer, state)
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
		return "", fmt.Errorf("zfs: containerExec: container name must not be empty")
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

// isZPoolNotFoundError returns true when err looks like a "no such pool" error
// from a "zpool" command. zpool uses stable error text across versions.
func isZPoolNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such pool") ||
		strings.Contains(msg, "cannot open") ||
		strings.Contains(msg, "does not exist")
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
