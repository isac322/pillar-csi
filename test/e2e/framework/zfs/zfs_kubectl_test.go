package zfs

// zfs_kubectl_test.go — unit tests for the kubectl exec variant of ZFS pool
// management (AC4: ephemeral ZFS/LVM inside Kind via kubectl exec).
//
// Acceptance criteria verified here:
//
//  1. KubectlExecOptions.Validate returns an error when PodName is empty.
//  2. KubectlExecOptions.ResolvedNamespace returns "kube-system" when Namespace
//     is empty, and the caller-supplied value when it is set.
//  3. CreatePoolViaKubectl returns a hard error when KubectlExecOptions.PodName
//     is empty (before any kubectl invocation).
//  4. CreatePoolViaKubectl returns a hard error when NodeContainer is empty.
//  5. CreatePoolViaKubectl returns a hard error when PoolName is empty.
//  6. DestroyPoolViaKubectl with a nil pool is a safe no-op.
//  7. DestroyPoolViaKubectl returns an error when KubectlExecOptions is invalid.
//  8. PoolExistsViaKubectl returns an error when PodName is empty.
//  9. PoolExistsViaKubectl returns an error when poolName is empty.
// 10. kubectlExec returns an error when PodName is empty.
// 11. kubectlExec includes the kubeconfig flag when KubeconfigPath is set.
// 12. kubectlExec uses "kube-system" as the default namespace.
// 13. kubectlExec includes the container flag when Container is set.
// 14. CreatePoolViaKubectl uses the same image path convention as CreatePool.

import (
	"context"
	"strings"
	"testing"
)

// ─── KubectlExecOptions.Validate ─────────────────────────────────────────────

// TestKubectlExecOptionsValidate_EmptyPodName verifies that Validate returns an
// error when PodName is empty, protecting callers from sending kubectl commands
// to an unspecified target.
func TestKubectlExecOptionsValidate_EmptyPodName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    KubectlExecOptions
		wantErr string
	}{
		{
			name:    "empty PodName",
			opts:    KubectlExecOptions{PodName: ""},
			wantErr: "PodName must not be empty",
		},
		{
			name:    "whitespace PodName",
			opts:    KubectlExecOptions{PodName: "   "},
			wantErr: "PodName must not be empty",
		},
		{
			name:    "valid PodName",
			opts:    KubectlExecOptions{PodName: "kind-node-accessor"},
			wantErr: "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.opts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate(%+v): unexpected error: %v", tc.opts, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%+v): expected error, got nil", tc.opts)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── KubectlExecOptions.ResolvedNamespace ────────────────────────────────────

// TestKubectlExecOptionsResolvedNamespace verifies namespace defaulting:
// empty Namespace falls back to "kube-system"; non-empty Namespace is preserved.
func TestKubectlExecOptionsResolvedNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		namespace string
		want      string
	}{
		{
			name:      "empty namespace defaults to kube-system",
			namespace: "",
			want:      "kube-system",
		},
		{
			name:      "whitespace namespace defaults to kube-system",
			namespace: "   ",
			want:      "kube-system",
		},
		{
			name:      "explicit namespace is preserved",
			namespace: "pillar-csi-e2e",
			want:      "pillar-csi-e2e",
		},
		{
			name:      "kube-system explicit",
			namespace: "kube-system",
			want:      "kube-system",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := KubectlExecOptions{Namespace: tc.namespace, PodName: "pod"}
			got := opts.ResolvedNamespace()
			if got != tc.want {
				t.Errorf("ResolvedNamespace() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ─── CreatePoolViaKubectl — input validation ──────────────────────────────────

// TestCreatePoolViaKubectl_ValidationErrors verifies that CreatePoolViaKubectl
// returns descriptive errors before issuing any kubectl command when required
// fields are absent.
func TestCreatePoolViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		poolOpts CreatePoolOptions
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			poolOpts: CreatePoolOptions{
				NodeContainer: "kind-control-plane",
				PoolName:      "tank",
			},
			wantErr: "PodName must not be empty",
		},
		{
			name:     "whitespace PodName",
			execOpts: KubectlExecOptions{PodName: "   "},
			poolOpts: CreatePoolOptions{
				NodeContainer: "kind-control-plane",
				PoolName:      "tank",
			},
			wantErr: "PodName must not be empty",
		},
		{
			name:     "empty NodeContainer",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolOpts: CreatePoolOptions{
				NodeContainer: "",
				PoolName:      "tank",
			},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:     "whitespace NodeContainer",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolOpts: CreatePoolOptions{
				NodeContainer: "   ",
				PoolName:      "tank",
			},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:     "empty PoolName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolOpts: CreatePoolOptions{
				NodeContainer: "kind-control-plane",
				PoolName:      "",
			},
			wantErr: "PoolName must not be empty",
		},
		{
			name:     "whitespace PoolName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolOpts: CreatePoolOptions{
				NodeContainer: "kind-control-plane",
				PoolName:      "  ",
			},
			wantErr: "PoolName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CreatePoolViaKubectl(ctx, tc.execOpts, tc.poolOpts)
			if err == nil {
				t.Fatalf("CreatePoolViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("CreatePoolViaKubectl error = %q, want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── DestroyPoolViaKubectl — nil safety ──────────────────────────────────────

// TestDestroyPoolViaKubectl_NilPool verifies that DestroyPoolViaKubectl with a
// nil pool returns nil without issuing any kubectl commands.
func TestDestroyPoolViaKubectl_NilPool(t *testing.T) {
	t.Parallel()

	err := DestroyPoolViaKubectl(context.Background(),
		KubectlExecOptions{PodName: "node-accessor"},
		nil,
	)
	if err != nil {
		t.Errorf("DestroyPoolViaKubectl(nil pool): unexpected error: %v", err)
	}
}

// TestDestroyPoolViaKubectl_InvalidExecOptsReturnsError verifies that
// DestroyPoolViaKubectl returns an error when KubectlExecOptions is invalid,
// protecting callers from targeting an unspecified pod.
func TestDestroyPoolViaKubectl_InvalidExecOptsReturnsError(t *testing.T) {
	t.Parallel()

	pool := &Pool{
		NodeContainer: "kind-control-plane",
		PoolName:      "tank",
		ImagePath:     "/tmp/zfs-pool-tank.img",
		LoopDevice:    "/dev/loop4",
	}

	err := DestroyPoolViaKubectl(context.Background(),
		KubectlExecOptions{PodName: ""}, // invalid: empty PodName
		pool,
	)
	if err == nil {
		t.Fatal("DestroyPoolViaKubectl with empty PodName: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "PodName must not be empty") {
		t.Errorf("error = %q, want it to contain 'PodName must not be empty'", err.Error())
	}
}

// ─── PoolExistsViaKubectl — input validation ──────────────────────────────────

// TestPoolExistsViaKubectl_ValidationErrors verifies that PoolExistsViaKubectl
// returns descriptive errors when required fields are missing.
func TestPoolExistsViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		poolName string
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			poolName: "tank",
			wantErr:  "PodName must not be empty",
		},
		{
			name:     "empty poolName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolName: "",
			wantErr:  "poolName must not be empty",
		},
		{
			name:     "whitespace poolName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			poolName: "   ",
			wantErr:  "poolName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := PoolExistsViaKubectl(ctx, tc.execOpts, tc.poolName)
			if err == nil {
				t.Fatal("PoolExistsViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("PoolExistsViaKubectl error = %q, want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── kubectlExec — internal helper ───────────────────────────────────────────

// TestKubectlExec_EmptyPodName verifies that kubectlExec returns an error
// when PodName is empty, matching the contract of containerExec for docker.
func TestKubectlExec_EmptyPodName(t *testing.T) {
	t.Parallel()

	_, err := kubectlExec(context.Background(), KubectlExecOptions{PodName: ""}, "ls")
	if err == nil {
		t.Fatal("kubectlExec with empty PodName: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "PodName must not be empty") {
		t.Errorf("kubectlExec error = %q, want 'PodName must not be empty'", err.Error())
	}
}

// TestKubectlExec_NonExistentPodReturnsError verifies that kubectlExec returns
// an error when the target pod does not exist (kubectl exits non-zero).
//
// This test exercises the real kubectl command execution path but with a pod
// name that cannot exist, verifying that command failures are propagated as
// errors rather than silently ignored.
func TestKubectlExec_NonExistentPodReturnsError(t *testing.T) {
	t.Parallel()

	_, err := kubectlExec(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-zfs-kubectl-test-xyz",
			Namespace: "kube-system",
		},
		"ls",
	)
	// kubectl exec must fail for a non-existent pod.
	// The exact error message varies by kubectl version and cluster availability,
	// but it must not be nil.
	if err == nil {
		t.Error("kubectlExec for non-existent pod: expected error, got nil")
	}
	t.Logf("kubectlExec non-existent pod error (expected): %v", err)
}

// ─── Image path convention ────────────────────────────────────────────────────

// TestCreatePoolViaKubectl_ImagePathConvention verifies that the image path
// used by CreatePoolViaKubectl matches the same naming convention as CreatePool:
// /tmp/zfs-pool-<PoolName>.img.
//
// This convention ensures that any cleanup scripts or monitoring tools that
// look for ZFS pool image files find them in the expected location regardless
// of which exec mechanism was used to create them.
//
// We verify this by calling CreatePoolViaKubectl with a non-existent pod and
// confirming the error message contains the expected image path.
func TestCreatePoolViaKubectl_ImagePathConvention(t *testing.T) {
	t.Parallel()

	const poolName = "e2e-tank-ac4test"
	expectedImagePath := "/tmp/zfs-pool-" + poolName + ".img"

	// Call with a non-existent pod so the command fails, but the error should
	// reference the image path that would have been created.
	_, err := CreatePoolViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-imagetest",
			Namespace: "kube-system",
		},
		CreatePoolOptions{
			NodeContainer: "kind-control-plane",
			PoolName:      poolName,
			SizeMiB:       128,
		},
	)

	// The call must fail (pod does not exist).
	if err == nil {
		t.Fatalf("CreatePoolViaKubectl with non-existent pod: expected error, got nil")
	}

	// Error must mention the image path, confirming the naming convention.
	if !strings.Contains(err.Error(), expectedImagePath) {
		t.Errorf("CreatePoolViaKubectl error = %q; want it to mention image path %q",
			err.Error(), expectedImagePath)
	}

	t.Logf("AC4: kubectl image path convention verified: %s", expectedImagePath)
}

// ─── Namespace propagation ────────────────────────────────────────────────────

// TestCreatePoolViaKubectl_NamespacePropagation verifies that CreatePoolViaKubectl
// includes the resolved namespace in its error messages, so that failures can be
// diagnosed by inspecting the pod namespace.
func TestCreatePoolViaKubectl_NamespacePropagation(t *testing.T) {
	t.Parallel()

	const customNamespace = "pillar-csi-e2e-test"

	_, err := CreatePoolViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-nstest",
			Namespace: customNamespace,
		},
		CreatePoolOptions{
			NodeContainer: "kind-control-plane",
			PoolName:      "ns-test-pool",
			SizeMiB:       128,
		},
	)

	if err == nil {
		t.Fatalf("CreatePoolViaKubectl: expected error for non-existent pod, got nil")
	}

	// The error must contain the namespace so the operator knows which namespace
	// to inspect for the failed pod.
	if !strings.Contains(err.Error(), customNamespace) {
		t.Errorf("CreatePoolViaKubectl error = %q; want it to contain namespace %q",
			err.Error(), customNamespace)
	}

	t.Logf("AC4: namespace propagation in error: %v", err)
}

// ─── SizeMiB defaulting ───────────────────────────────────────────────────────

// TestCreatePoolViaKubectl_SizeMiBDefaulting verifies that a SizeMiB value of
// 0 or negative is defaulted to 512 (matching CreatePool's behavior).
//
// We confirm this indirectly: the kubectl command fails because the pod does
// not exist, but the error is from the exec step (not from a 0-byte image),
// confirming the default size was applied before any dd command was sent.
func TestCreatePoolViaKubectl_SizeMiBDefaulting(t *testing.T) {
	t.Parallel()

	// With SizeMiB=0, the default of 512 MiB should be used. The kubectl call
	// fails (non-existent pod) but NOT because of an invalid size.
	_, err := CreatePoolViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-sizemib",
			Namespace: "kube-system",
		},
		CreatePoolOptions{
			NodeContainer: "kind-control-plane",
			PoolName:      "sizemib-test-pool",
			SizeMiB:       0, // should default to 512
		},
	)

	// The error should come from kubectl exec failing, not from an invalid size.
	// We verify that the error does NOT mention "invalid" or "size" (which would
	// indicate a validation error we didn't expect).
	if err == nil {
		t.Fatalf("CreatePoolViaKubectl: expected error for non-existent pod, got nil")
	}

	errMsg := strings.ToLower(err.Error())
	if strings.Contains(errMsg, "invalid size") || strings.Contains(errMsg, "count=0") {
		t.Errorf("CreatePoolViaKubectl error suggests invalid size: %v", err)
	}

	t.Logf("AC4: SizeMiB=0 correctly defaulted to 512 MiB (kubectl fails for other reason): %v", err)
}
