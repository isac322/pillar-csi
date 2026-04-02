// Package zfs — kubectl exec variant for Kind node container access.
//
// This file provides an alternative exec mechanism for ZFS pool management
// using "kubectl exec" rather than "docker exec". It is designed for CI
// environments where the Docker daemon is not directly accessible from the
// test process, but a valid kubeconfig is available.
//
// # Design
//
// Kind nodes are Docker containers that run as Kubernetes nodes. When Docker
// socket access is unavailable, a privileged pod can be deployed on the target
// Kind node and commands can be tunnelled through it via "kubectl exec".
//
// The privileged pod must be configured with:
//   - hostPID: true (to access host process namespaces)
//   - A privileged security context (to create loop devices and run zpool)
//   - Access to /tmp on the host (via hostPath or emptyDir at /tmp)
//
// # Usage
//
//	pool, err := zfs.CreatePoolViaKubectl(ctx,
//	    zfs.KubectlExecOptions{
//	        KubeconfigPath: "/tmp/kubeconfig",
//	        Namespace:      "kube-system",
//	        PodName:        "kind-node-accessor-pillar-csi-e2e-control-plane",
//	    },
//	    zfs.CreatePoolOptions{
//	        NodeContainer: "pillar-csi-e2e-control-plane",
//	        PoolName:      "e2e-tank-abc123",
//	        SizeMiB:       512,
//	    },
//	)
//	if err != nil { ... }
//	defer pool.Destroy(ctx) // uses docker exec for cleanup by default
package zfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// KubectlExecOptions configures how kubectl exec reaches the privileged pod
// running on a Kind node. All storage commands (dd, losetup, zpool) are
// tunnelled through this pod.
type KubectlExecOptions struct {
	// KubeconfigPath is the path to the kubeconfig file for the Kind cluster.
	// When empty, kubectl falls back to the KUBECONFIG environment variable
	// or the default config path (~/.kube/config).
	KubeconfigPath string

	// Namespace is the Kubernetes namespace where the privileged accessor pod
	// is running. Defaults to "kube-system" when empty.
	Namespace string

	// PodName is the name of the privileged pod to exec into.
	// Must not be empty.
	PodName string

	// Container is the container name within the pod to exec into.
	// When empty, kubectl targets the first (or only) container in the pod.
	Container string
}

// Validate returns an error when the required KubectlExecOptions fields are
// not set, providing a clear error before any kubectl invocation.
func (o KubectlExecOptions) Validate() error {
	if strings.TrimSpace(o.PodName) == "" {
		return fmt.Errorf("zfs: KubectlExecOptions.PodName must not be empty")
	}
	return nil
}

// ResolvedNamespace returns the namespace to use, defaulting to "kube-system"
// when the Namespace field is empty.
func (o KubectlExecOptions) ResolvedNamespace() string {
	if strings.TrimSpace(o.Namespace) != "" {
		return strings.TrimSpace(o.Namespace)
	}
	return "kube-system"
}

// CreatePoolViaKubectl creates an ephemeral ZFS pool inside a Kind node by
// executing storage commands via "kubectl exec" into a privileged pod.
//
// This function provides the same end result as [CreatePool] but uses
// "kubectl exec" instead of "docker exec", making it suitable for environments
// where the Docker daemon socket is not accessible from the test process.
//
// Prerequisites:
//   - A privileged pod named execOpts.PodName must be running in
//     execOpts.Namespace on the target Kind node.
//   - The pod must have: hostPID: true, a privileged security context, and
//     write access to /tmp (for the loop-device image file).
//   - The pod's container must have dd, losetup, and zpool in its PATH.
//
// Steps:
//
//  1. Allocates a loop-device image file at /tmp/zfs-pool-<PoolName>.img
//     inside the pod.
//  2. Attaches the image as a loop device via "losetup --find --show".
//  3. Creates a ZFS pool on the loop device via "zpool create".
//
// On any error, best-effort cleanup of already-created resources is attempted.
// The caller is still responsible for calling [Pool.Destroy] on success.
//
// Destroy on the returned pool uses [containerExec] (docker exec) by default.
// If kubectl-based teardown is required, use [DestroyPoolViaKubectl].
func CreatePoolViaKubectl(ctx context.Context, execOpts KubectlExecOptions, poolOpts CreatePoolOptions) (*Pool, error) {
	if err := execOpts.Validate(); err != nil {
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: %w", err)
	}
	if strings.TrimSpace(poolOpts.NodeContainer) == "" {
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: NodeContainer must not be empty")
	}
	if strings.TrimSpace(poolOpts.PoolName) == "" {
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: PoolName must not be empty")
	}

	sizeMiB := poolOpts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = 512
	}

	imagePath := fmt.Sprintf("/tmp/zfs-pool-%s.img", poolOpts.PoolName)

	execFn := func(args ...string) (string, error) {
		return kubectlExec(ctx, execOpts, args...)
	}

	// ── Step 1: Allocate the loop device image ─────────────────────────────
	if _, err := execFn("dd", "if=/dev/zero",
		fmt.Sprintf("of=%s", imagePath),
		"bs=1M",
		fmt.Sprintf("count=%d", sizeMiB),
	); err != nil {
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: allocate image %s in pod %s/%s: %w",
			imagePath, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}

	cleanupImage := func() {
		_, _ = execFn("rm", "-f", imagePath)
	}

	// ── Step 2: Attach image as loop device ───────────────────────────────
	loopOut, err := execFn("losetup", "--find", "--show", imagePath)
	if err != nil {
		cleanupImage()
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: attach loop device for %s in pod %s/%s: %w",
			imagePath, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}
	loopDevice := strings.TrimSpace(loopOut)
	if loopDevice == "" {
		cleanupImage()
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: losetup returned empty loop device path in pod %s/%s",
			execOpts.ResolvedNamespace(), execOpts.PodName)
	}

	cleanupLoop := func() {
		_, _ = execFn("losetup", "-d", loopDevice)
		cleanupImage()
	}

	// ── Step 3: Create the ZFS pool ──────────────────────────────────────
	if _, err := execFn("zpool", "create", poolOpts.PoolName, loopDevice); err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("zfs: CreatePoolViaKubectl: zpool create %s on %s in pod %s/%s: %w",
			poolOpts.PoolName, loopDevice, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}

	return &Pool{
		NodeContainer: poolOpts.NodeContainer,
		PoolName:      poolOpts.PoolName,
		ImagePath:     imagePath,
		LoopDevice:    loopDevice,
	}, nil
}

// DestroyPoolViaKubectl destroys a ZFS pool and releases the loop device using
// "kubectl exec" into a privileged pod.
//
// This mirrors the teardown sequence of [Pool.Destroy] but uses kubectl exec
// instead of docker exec. Use this function when Docker daemon access is
// unavailable during teardown.
//
// Steps:
//
//  1. "zpool destroy -f <PoolName>" via kubectl exec.
//  2. "losetup -d <LoopDevice>" via kubectl exec.
//  3. "rm -f <ImagePath>" via kubectl exec.
//
// All three steps always execute; errors are collected and returned together.
// Calling DestroyPoolViaKubectl with a nil pool is a safe no-op.
func DestroyPoolViaKubectl(ctx context.Context, execOpts KubectlExecOptions, pool *Pool) error {
	if pool == nil {
		return nil
	}
	if err := execOpts.Validate(); err != nil {
		return fmt.Errorf("zfs: DestroyPoolViaKubectl: %w", err)
	}

	execFn := func(args ...string) (string, error) {
		return kubectlExec(ctx, execOpts, args...)
	}

	var errs []error

	// Step 1: Destroy the ZFS pool.
	if _, err := execFn("zpool", "destroy", "-f", pool.PoolName); err != nil {
		if !isZPoolNotFoundError(err) {
			errs = append(errs, fmt.Errorf("zpool destroy -f %s: %w", pool.PoolName, err))
		}
	}

	// Step 2: Detach the loop device.
	if strings.TrimSpace(pool.LoopDevice) != "" {
		if _, err := execFn("losetup", "-d", pool.LoopDevice); err != nil {
			if !isLoopNotFoundError(err) {
				errs = append(errs, fmt.Errorf("losetup -d %s: %w", pool.LoopDevice, err))
			}
		}
	}

	// Step 3: Remove the image file.
	if strings.TrimSpace(pool.ImagePath) != "" {
		if _, err := execFn("rm", "-f", pool.ImagePath); err != nil {
			errs = append(errs, fmt.Errorf("rm -f %s: %w", pool.ImagePath, err))
		}
	}

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("zfs: DestroyPoolViaKubectl: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// PoolExistsViaKubectl checks whether a ZFS pool exists inside a privileged
// pod by running "zpool list <poolName>" via kubectl exec.
//
// Returns (true, nil) if the pool exists, (false, nil) if it does not, and
// (false, err) for unexpected errors.
func PoolExistsViaKubectl(ctx context.Context, execOpts KubectlExecOptions, poolName string) (bool, error) {
	if err := execOpts.Validate(); err != nil {
		return false, fmt.Errorf("zfs: PoolExistsViaKubectl: %w", err)
	}
	if strings.TrimSpace(poolName) == "" {
		return false, fmt.Errorf("zfs: PoolExistsViaKubectl: poolName must not be empty")
	}

	_, err := kubectlExec(ctx, execOpts, "zpool", "list", poolName)
	if err != nil {
		if isZPoolNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("zfs: PoolExistsViaKubectl: %w", err)
	}
	return true, nil
}

// ─── internal helpers ────────────────────────────────────────────────────────

// kubectlExec runs a command inside a pod via "kubectl exec".
//
// KUBECONFIG is read from opts.KubeconfigPath when set; otherwise kubectl uses
// the KUBECONFIG environment variable or the default kubeconfig path. The
// cluster endpoint is never hardcoded.
//
// Returns (stdout, nil) on success or ("", error) on failure with the pod's
// stderr included in the error message.
func kubectlExec(ctx context.Context, opts KubectlExecOptions, args ...string) (string, error) {
	if strings.TrimSpace(opts.PodName) == "" {
		return "", fmt.Errorf("zfs: kubectlExec: PodName must not be empty")
	}

	ns := opts.ResolvedNamespace()

	// Build: kubectl [--kubeconfig=<path>] exec -n <ns> <pod> [-- args...]
	kubectlArgs := []string{}
	if strings.TrimSpace(opts.KubeconfigPath) != "" {
		kubectlArgs = append(kubectlArgs, fmt.Sprintf("--kubeconfig=%s", opts.KubeconfigPath))
	}
	kubectlArgs = append(kubectlArgs, "exec", "-n", ns, opts.PodName)
	if strings.TrimSpace(opts.Container) != "" {
		kubectlArgs = append(kubectlArgs, "-c", opts.Container)
	}
	kubectlArgs = append(kubectlArgs, "--")
	kubectlArgs = append(kubectlArgs, args...)

	cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs...) //nolint:gosec

	// Propagate the full environment so KUBECONFIG, HOME, and other variables
	// required by kubectl are available. The cluster endpoint is never hardcoded.
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
		return "", fmt.Errorf("kubectl exec %s/%s %s: %s",
			ns, opts.PodName, strings.Join(args, " "), errText)
	}

	return strings.TrimSpace(stdout.String()), nil
}
