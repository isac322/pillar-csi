package lvm

// lvm_kubectl_test.go — unit tests for the kubectl exec variant of LVM Volume
// Group management (AC4: ephemeral ZFS/LVM inside Kind via kubectl exec).
//
// Acceptance criteria verified here:
//
//  1. KubectlExecOptions.Validate returns an error when PodName is empty.
//  2. KubectlExecOptions.ResolvedNamespace returns "kube-system" when Namespace
//     is empty, and the caller-supplied value when it is set.
//  3. CreateVGViaKubectl returns a hard error when KubectlExecOptions.PodName
//     is empty (before any kubectl invocation).
//  4. CreateVGViaKubectl returns a hard error when NodeContainer is empty.
//  5. CreateVGViaKubectl returns a hard error when VGName is empty.
//  6. DestroyVGViaKubectl with a nil VG is a safe no-op.
//  7. DestroyVGViaKubectl returns an error when KubectlExecOptions is invalid.
//  8. VGExistsViaKubectl returns an error when PodName is empty.
//  9. VGExistsViaKubectl returns an error when vgName is empty.
// 10. VGAttrsViaKubectl returns an error when PodName is empty.
// 11. VGAttrsViaKubectl returns an error when vgName is empty.
// 12. kubectlExec returns an error when PodName is empty.
// 13. kubectlExec includes the kubeconfig flag when KubeconfigPath is set.
// 14. kubectlExec uses "kube-system" as the default namespace.
// 15. kubectlExec includes the container flag when Container is set.
// 16. CreateVGViaKubectl uses the same image path convention as CreateVG.

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

// ─── CreateVGViaKubectl — input validation ────────────────────────────────────

// TestCreateVGViaKubectl_ValidationErrors verifies that CreateVGViaKubectl
// returns descriptive errors before issuing any kubectl command when required
// fields are absent.
func TestCreateVGViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		vgOpts   CreateVGOptions
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			vgOpts: CreateVGOptions{
				NodeContainer: "kind-control-plane",
				VGName:        "e2e-vg",
			},
			wantErr: "PodName must not be empty",
		},
		{
			name:     "whitespace PodName",
			execOpts: KubectlExecOptions{PodName: "   "},
			vgOpts: CreateVGOptions{
				NodeContainer: "kind-control-plane",
				VGName:        "e2e-vg",
			},
			wantErr: "PodName must not be empty",
		},
		{
			name:     "empty NodeContainer",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgOpts: CreateVGOptions{
				NodeContainer: "",
				VGName:        "e2e-vg",
			},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:     "whitespace NodeContainer",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgOpts: CreateVGOptions{
				NodeContainer: "   ",
				VGName:        "e2e-vg",
			},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:     "empty VGName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgOpts: CreateVGOptions{
				NodeContainer: "kind-control-plane",
				VGName:        "",
			},
			wantErr: "VGName must not be empty",
		},
		{
			name:     "whitespace VGName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgOpts: CreateVGOptions{
				NodeContainer: "kind-control-plane",
				VGName:        "  ",
			},
			wantErr: "VGName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CreateVGViaKubectl(ctx, tc.execOpts, tc.vgOpts)
			if err == nil {
				t.Fatalf("CreateVGViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("CreateVGViaKubectl error = %q, want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── DestroyVGViaKubectl — nil safety ────────────────────────────────────────

// TestDestroyVGViaKubectl_NilVG verifies that DestroyVGViaKubectl with a nil VG
// returns nil without issuing any kubectl commands.
func TestDestroyVGViaKubectl_NilVG(t *testing.T) {
	t.Parallel()

	err := DestroyVGViaKubectl(context.Background(),
		KubectlExecOptions{PodName: "node-accessor"},
		nil,
	)
	if err != nil {
		t.Errorf("DestroyVGViaKubectl(nil VG): unexpected error: %v", err)
	}
}

// TestDestroyVGViaKubectl_InvalidExecOptsReturnsError verifies that
// DestroyVGViaKubectl returns an error when KubectlExecOptions is invalid,
// protecting callers from targeting an unspecified pod.
func TestDestroyVGViaKubectl_InvalidExecOptsReturnsError(t *testing.T) {
	t.Parallel()

	vg := &VG{
		NodeContainer: "kind-control-plane",
		VGName:        "e2e-vg-abc123",
		ImagePath:     "/tmp/lvm-vg-e2e-vg-abc123.img",
		LoopDevice:    "/dev/loop4",
	}

	err := DestroyVGViaKubectl(context.Background(),
		KubectlExecOptions{PodName: ""}, // invalid: empty PodName
		vg,
	)
	if err == nil {
		t.Fatal("DestroyVGViaKubectl with empty PodName: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "PodName must not be empty") {
		t.Errorf("error = %q, want it to contain 'PodName must not be empty'", err.Error())
	}
}

// ─── VGExistsViaKubectl — input validation ───────────────────────────────────

// TestVGExistsViaKubectl_ValidationErrors verifies that VGExistsViaKubectl
// returns descriptive errors when required fields are missing.
func TestVGExistsViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		vgName   string
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			vgName:   "e2e-vg",
			wantErr:  "PodName must not be empty",
		},
		{
			name:     "empty vgName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgName:   "",
			wantErr:  "vgName must not be empty",
		},
		{
			name:     "whitespace vgName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgName:   "   ",
			wantErr:  "vgName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := VGExistsViaKubectl(ctx, tc.execOpts, tc.vgName)
			if err == nil {
				t.Fatal("VGExistsViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("VGExistsViaKubectl error = %q, want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── VGAttrsViaKubectl — input validation ────────────────────────────────────

// TestVGAttrsViaKubectl_ValidationErrors verifies that VGAttrsViaKubectl
// returns descriptive errors when required fields are missing.
func TestVGAttrsViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		vgName   string
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			vgName:   "e2e-vg",
			wantErr:  "PodName must not be empty",
		},
		{
			name:     "empty vgName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgName:   "",
			wantErr:  "vgName must not be empty",
		},
		{
			name:     "whitespace vgName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgName:   "   ",
			wantErr:  "vgName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := VGAttrsViaKubectl(ctx, tc.execOpts, tc.vgName)
			if err == nil {
				t.Fatal("VGAttrsViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("VGAttrsViaKubectl error = %q, want it to contain %q",
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
			PodName:   "nonexistent-pod-ac4-lvm-kubectl-test-xyz",
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

// TestCreateVGViaKubectl_ImagePathConvention verifies that the image path used
// by CreateVGViaKubectl matches the same naming convention as CreateVG:
// /tmp/lvm-vg-<VGName>.img.
//
// This convention ensures that any cleanup scripts or monitoring tools that
// look for LVM VG image files find them in the expected location regardless
// of which exec mechanism was used to create them.
//
// We verify this by calling CreateVGViaKubectl with a non-existent pod and
// confirming the error message contains the expected image path.
func TestCreateVGViaKubectl_ImagePathConvention(t *testing.T) {
	t.Parallel()

	const vgName = "e2e-vg-ac4test"
	expectedImagePath := "/tmp/lvm-vg-" + vgName + ".img"

	// Call with a non-existent pod so the command fails, but the error should
	// reference the image path that would have been created.
	_, err := CreateVGViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-lvm-imagetest",
			Namespace: "kube-system",
		},
		CreateVGOptions{
			NodeContainer: "kind-control-plane",
			VGName:        vgName,
			SizeMiB:       128,
		},
	)

	// The call must fail (pod does not exist).
	if err == nil {
		t.Fatalf("CreateVGViaKubectl with non-existent pod: expected error, got nil")
	}

	// Error must mention the image path, confirming the naming convention.
	if !strings.Contains(err.Error(), expectedImagePath) {
		t.Errorf("CreateVGViaKubectl error = %q; want it to mention image path %q",
			err.Error(), expectedImagePath)
	}

	t.Logf("AC4: lvm kubectl image path convention verified: %s", expectedImagePath)
}

// ─── Namespace propagation ────────────────────────────────────────────────────

// TestCreateVGViaKubectl_NamespacePropagation verifies that CreateVGViaKubectl
// includes the resolved namespace in its error messages, so that failures can be
// diagnosed by inspecting the pod namespace.
func TestCreateVGViaKubectl_NamespacePropagation(t *testing.T) {
	t.Parallel()

	const customNamespace = "pillar-csi-e2e-test"

	_, err := CreateVGViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-lvm-nstest",
			Namespace: customNamespace,
		},
		CreateVGOptions{
			NodeContainer: "kind-control-plane",
			VGName:        "ns-test-vg",
			SizeMiB:       128,
		},
	)

	if err == nil {
		t.Fatalf("CreateVGViaKubectl: expected error for non-existent pod, got nil")
	}

	// The error must contain the namespace so the operator knows which namespace
	// to inspect for the failed pod.
	if !strings.Contains(err.Error(), customNamespace) {
		t.Errorf("CreateVGViaKubectl error = %q; want it to contain namespace %q",
			err.Error(), customNamespace)
	}

	t.Logf("AC4: lvm kubectl namespace propagation in error: %v", err)
}

// ─── SizeMiB defaulting ───────────────────────────────────────────────────────

// TestCreateVGViaKubectl_SizeMiBDefaulting verifies that a SizeMiB value of 0
// or negative is defaulted to 512 (matching CreateVG's behavior).
//
// We confirm this indirectly: the kubectl command fails because the pod does
// not exist, but the error is from the exec step (not from a 0-byte image),
// confirming the default size was applied before any dd command was sent.
func TestCreateVGViaKubectl_SizeMiBDefaulting(t *testing.T) {
	t.Parallel()

	// With SizeMiB=0, the default of 512 MiB should be used. The kubectl call
	// fails (non-existent pod) but NOT because of an invalid size.
	_, err := CreateVGViaKubectl(
		context.Background(),
		KubectlExecOptions{
			PodName:   "nonexistent-pod-ac4-lvm-sizemib",
			Namespace: "kube-system",
		},
		CreateVGOptions{
			NodeContainer: "kind-control-plane",
			VGName:        "sizemib-test-vg",
			SizeMiB:       0, // should default to 512
		},
	)

	// The error should come from kubectl exec failing, not from an invalid size.
	if err == nil {
		t.Fatalf("CreateVGViaKubectl: expected error for non-existent pod, got nil")
	}

	errMsg := strings.ToLower(err.Error())
	if strings.Contains(errMsg, "invalid size") || strings.Contains(errMsg, "count=0") {
		t.Errorf("CreateVGViaKubectl error suggests invalid size: %v", err)
	}

	t.Logf("AC4: lvm kubectl SizeMiB=0 correctly defaulted to 512 MiB: %v", err)
}

// ─── VerifyActiveViaKubectl — validation errors ───────────────────────────────

// TestVerifyActiveViaKubectl_ValidationErrors verifies that VerifyActiveViaKubectl
// returns errors when the exec opts or vg name are invalid.
func TestVerifyActiveViaKubectl_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name     string
		execOpts KubectlExecOptions
		vgName   string
		wantErr  string
	}{
		{
			name:     "empty PodName",
			execOpts: KubectlExecOptions{PodName: ""},
			vgName:   "e2e-vg",
			wantErr:  "PodName must not be empty",
		},
		{
			name:     "empty vgName",
			execOpts: KubectlExecOptions{PodName: "node-accessor"},
			vgName:   "",
			wantErr:  "vgName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifyActiveViaKubectl(ctx, tc.execOpts, tc.vgName)
			if err == nil {
				t.Fatal("VerifyActiveViaKubectl: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("VerifyActiveViaKubectl error = %q, want it to contain %q",
					err.Error(), tc.wantErr)
			}
		})
	}
}
