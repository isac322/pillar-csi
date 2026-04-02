// Package lvm — kubectl exec variant for Kind node container access.
//
// This file provides an alternative exec mechanism for LVM Volume Group
// management using "kubectl exec" rather than "docker exec". It is designed for
// CI environments where the Docker daemon is not directly accessible from the
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
//   - A privileged security context (to create loop devices and run lvm2 tools)
//   - Access to /tmp on the host (via hostPath or emptyDir at /tmp)
//
// # Usage
//
//	vg, err := lvm.CreateVGViaKubectl(ctx,
//	    lvm.KubectlExecOptions{
//	        KubeconfigPath: "/tmp/kubeconfig",
//	        Namespace:      "kube-system",
//	        PodName:        "kind-node-accessor-pillar-csi-e2e-control-plane",
//	    },
//	    lvm.CreateVGOptions{
//	        NodeContainer: "pillar-csi-e2e-control-plane",
//	        VGName:        "e2e-vg-abc123",
//	        SizeMiB:       512,
//	    },
//	)
//	if err != nil { ... }
//	defer lvm.DestroyVGViaKubectl(ctx, execOpts, vg)
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

// KubectlExecOptions configures how kubectl exec reaches the privileged pod
// running on a Kind node. All storage commands (dd, losetup, pvcreate, vgcreate)
// are tunnelled through this pod.
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
		return fmt.Errorf("lvm: KubectlExecOptions.PodName must not be empty")
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

// CreateVGViaKubectl creates an ephemeral LVM Volume Group inside a Kind node
// by executing storage commands via "kubectl exec" into a privileged pod.
//
// This function provides the same end result as [CreateVG] but uses
// "kubectl exec" instead of "docker exec", making it suitable for environments
// where the Docker daemon socket is not accessible from the test process.
//
// Prerequisites:
//   - A privileged pod named execOpts.PodName must be running in
//     execOpts.Namespace on the target Kind node.
//   - The pod must have: hostPID: true, a privileged security context, and
//     write access to /tmp (for the loop-device image file).
//   - The pod's container must have dd, losetup, pvcreate, and vgcreate in its PATH.
//
// Steps:
//
//  1. Allocates a loop-device image file at /tmp/lvm-vg-<VGName>.img inside the pod.
//  2. Attaches the image as a loop device via "losetup --find --show".
//  3. Initialises the loop device as an LVM Physical Volume via "pvcreate".
//  4. Creates a Volume Group on that PV via "vgcreate".
//
// On any error, best-effort cleanup of already-created resources is attempted.
// The caller is still responsible for calling [DestroyVGViaKubectl] on success.
func CreateVGViaKubectl(ctx context.Context, execOpts KubectlExecOptions, vgOpts CreateVGOptions) (*VG, error) {
	if err := execOpts.Validate(); err != nil {
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: %w", err)
	}
	if strings.TrimSpace(vgOpts.NodeContainer) == "" {
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: NodeContainer must not be empty")
	}
	if strings.TrimSpace(vgOpts.VGName) == "" {
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: VGName must not be empty")
	}

	sizeMiB := vgOpts.SizeMiB
	if sizeMiB <= 0 {
		sizeMiB = 512
	}

	imagePath := fmt.Sprintf("/tmp/lvm-vg-%s.img", vgOpts.VGName)

	execFn := func(args ...string) (string, error) {
		return kubectlExec(ctx, execOpts, args...)
	}

	// ── Step 1: Allocate the loop device image ─────────────────────────────
	//
	// "dd if=/dev/zero …" is universally available in Kind nodes and avoids any
	// dependency on fallocate / truncate which may behave differently across
	// filesystem types.
	if _, err := execFn("dd", "if=/dev/zero",
		fmt.Sprintf("of=%s", imagePath),
		"bs=1M",
		fmt.Sprintf("count=%d", sizeMiB),
	); err != nil {
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: allocate image %s in pod %s/%s: %w",
			imagePath, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}

	cleanupImage := func() {
		_, _ = execFn("rm", "-f", imagePath)
	}

	// ── Step 2: Attach image as loop device ───────────────────────────────
	//
	// "--find --show" picks a free loop device and prints its path, e.g. "/dev/loop4".
	loopOut, err := execFn("losetup", "--find", "--show", imagePath)
	if err != nil {
		cleanupImage()
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: attach loop device for %s in pod %s/%s: %w",
			imagePath, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}
	loopDevice := strings.TrimSpace(loopOut)
	if loopDevice == "" {
		cleanupImage()
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: losetup returned empty loop device path in pod %s/%s",
			execOpts.ResolvedNamespace(), execOpts.PodName)
	}

	cleanupLoop := func() {
		_, _ = execFn("losetup", "-d", loopDevice)
		cleanupImage()
	}

	// ── Step 3: Initialise the loop device as an LVM Physical Volume ──────
	//
	// "--yes" suppresses the "really initialise?" confirmation prompt.
	// "--force" allows pvcreate to overwrite an existing label.
	if _, err := execFn("pvcreate", "--yes", "--force", loopDevice); err != nil {
		cleanupLoop()
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: pvcreate %s in pod %s/%s: %w",
			loopDevice, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}

	cleanupPV := func() {
		_, _ = execFn("pvremove", "-f", "-f", loopDevice)
		cleanupLoop()
	}

	// ── Step 4: Create the Volume Group ──────────────────────────────────
	if _, err := execFn("vgcreate", vgOpts.VGName, loopDevice); err != nil {
		cleanupPV()
		return nil, fmt.Errorf("lvm: CreateVGViaKubectl: vgcreate %s on %s in pod %s/%s: %w",
			vgOpts.VGName, loopDevice, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}

	return &VG{
		NodeContainer: vgOpts.NodeContainer,
		VGName:        vgOpts.VGName,
		ImagePath:     imagePath,
		LoopDevice:    loopDevice,
	}, nil
}

// DestroyVGViaKubectl destroys an LVM Volume Group, removes the Physical Volume
// label, and releases the loop device using "kubectl exec" into a privileged pod.
//
// This mirrors the teardown sequence of [VG.Destroy] but uses kubectl exec
// instead of docker exec. Use this function when Docker daemon access is
// unavailable during teardown.
//
// Steps:
//
//  1. "vgremove -f <VGName>" via kubectl exec.
//  2. "pvremove -f -f <LoopDevice>" via kubectl exec.
//  3. "losetup -d <LoopDevice>" via kubectl exec.
//  4. "rm -f <ImagePath>" via kubectl exec.
//
// All four steps always execute; errors are collected and returned together so
// that a failure in an early step does not prevent later steps from running.
// Calling DestroyVGViaKubectl with a nil VG is a safe no-op.
func DestroyVGViaKubectl(ctx context.Context, execOpts KubectlExecOptions, vg *VG) error {
	if vg == nil {
		return nil
	}
	if err := execOpts.Validate(); err != nil {
		return fmt.Errorf("lvm: DestroyVGViaKubectl: %w", err)
	}

	execFn := func(args ...string) (string, error) {
		return kubectlExec(ctx, execOpts, args...)
	}

	var errs []error

	// Step 1: Remove the Volume Group.
	if _, err := execFn("vgremove", "-f", vg.VGName); err != nil {
		if !isVGNotFoundError(err) {
			errs = append(errs, fmt.Errorf("vgremove -f %s: %w", vg.VGName, err))
		}
		// Tolerate "not found" — VG may have already been removed.
	}

	// Step 2: Remove the Physical Volume label.
	if strings.TrimSpace(vg.LoopDevice) != "" {
		if _, err := execFn("pvremove", "-f", "-f", vg.LoopDevice); err != nil {
			if !isPVNotFoundError(err) {
				errs = append(errs, fmt.Errorf("pvremove -f -f %s: %w", vg.LoopDevice, err))
			}
			// Tolerate "not a PV" — PV may have already been removed.
		}
	}

	// Step 3: Detach the loop device.
	if strings.TrimSpace(vg.LoopDevice) != "" {
		if _, err := execFn("losetup", "-d", vg.LoopDevice); err != nil {
			if !isLoopNotFoundError(err) {
				errs = append(errs, fmt.Errorf("losetup -d %s: %w", vg.LoopDevice, err))
			}
			// Tolerate "no such device" — loop device may have already been detached.
		}
	}

	// Step 4: Remove the image file.
	if strings.TrimSpace(vg.ImagePath) != "" {
		if _, err := execFn("rm", "-f", vg.ImagePath); err != nil {
			errs = append(errs, fmt.Errorf("rm -f %s: %w", vg.ImagePath, err))
		}
	}

	return errors.Join(errs...)
}

// VGExistsViaKubectl checks whether an LVM Volume Group with the given name
// currently exists inside a privileged pod by running "vgs <vgName>" via
// kubectl exec.
//
// Returns (true, nil) if the VG exists, (false, nil) if it does not, and
// (false, err) for unexpected errors.
func VGExistsViaKubectl(ctx context.Context, execOpts KubectlExecOptions, vgName string) (bool, error) {
	if err := execOpts.Validate(); err != nil {
		return false, fmt.Errorf("lvm: VGExistsViaKubectl: %w", err)
	}
	if strings.TrimSpace(vgName) == "" {
		return false, fmt.Errorf("lvm: VGExistsViaKubectl: vgName must not be empty")
	}

	_, err := kubectlExec(ctx, execOpts, "vgs", "--noheadings", vgName)
	if err != nil {
		if isVGNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lvm: VGExistsViaKubectl: %w", err)
	}
	return true, nil
}

// VGAttrsViaKubectl returns the LVM Volume Group attribute flags as reported by
// "vgs --noheadings -o vg_attr <vgName>" via kubectl exec into a privileged pod.
//
// The returned string is in the same 6-character format as the vg_attr column
// of vgs(8). Returns an error when the VG does not exist or the kubectl exec
// call fails.
//
// Use [VerifyActiveViaKubectl] when you need a simple pass/fail readiness check.
func VGAttrsViaKubectl(ctx context.Context, execOpts KubectlExecOptions, vgName string) (string, error) {
	if err := execOpts.Validate(); err != nil {
		return "", fmt.Errorf("lvm: VGAttrsViaKubectl: %w", err)
	}
	if strings.TrimSpace(vgName) == "" {
		return "", fmt.Errorf("lvm: VGAttrsViaKubectl: vgName must not be empty")
	}

	out, err := kubectlExec(ctx, execOpts, "vgs", "--noheadings", "-o", "vg_attr", vgName)
	if err != nil {
		return "", fmt.Errorf("lvm: VGAttrsViaKubectl: vgs --noheadings -o vg_attr %s in pod %s/%s: %w",
			vgName, execOpts.ResolvedNamespace(), execOpts.PodName, err)
	}
	return strings.TrimSpace(out), nil
}

// VerifyActiveViaKubectl checks that the named LVM Volume Group is in a
// writable, non-partial, non-exported state via kubectl exec.
//
// It runs "vgs --noheadings -o vg_attr <vgName>" via kubectl exec and checks:
//
//   - Position 0 must be 'w' (writable, not read-only).
//   - Position 2 must not be 'x' (not exported).
//   - Position 3 must not be 'p' (no partial PVs).
//
// Returns nil when the VG is healthy, or a descriptive error otherwise.
func VerifyActiveViaKubectl(ctx context.Context, execOpts KubectlExecOptions, vgName string) error {
	attrs, err := VGAttrsViaKubectl(ctx, execOpts, vgName)
	if err != nil {
		return fmt.Errorf("lvm: VerifyActiveViaKubectl: %w", err)
	}

	if len(attrs) < 4 {
		return fmt.Errorf("lvm: VerifyActiveViaKubectl: vg %q returned "+
			"unexpected attribute string %q (want at least 4 characters)", vgName, attrs)
	}

	if attrs[0] != 'w' {
		return fmt.Errorf("lvm: VerifyActiveViaKubectl: vg %q is not writable: "+
			"attr[0]=%q (want 'w'), full attrs=%q",
			vgName, string(attrs[0]), attrs)
	}

	if attrs[2] == 'x' {
		return fmt.Errorf("lvm: VerifyActiveViaKubectl: vg %q is exported "+
			"(attr[2]='x') — VG is not usable on this host, full attrs=%q",
			vgName, attrs)
	}

	if attrs[3] == 'p' {
		return fmt.Errorf("lvm: VerifyActiveViaKubectl: vg %q has partial PVs "+
			"(attr[3]='p') — one or more backing devices are missing, full attrs=%q",
			vgName, attrs)
	}

	return nil
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
		return "", fmt.Errorf("lvm: kubectlExec: PodName must not be empty")
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
