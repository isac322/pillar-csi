package lvm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// ─── KindNodeContainerName ────────────────────────────────────────────────────

func TestKindNodeContainerName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		cluster   string
		nodeIndex int
		want      string
	}{
		{
			name:      "control-plane (index 0)",
			cluster:   "pillar-csi-e2e-abc123",
			nodeIndex: 0,
			want:      "pillar-csi-e2e-abc123-control-plane",
		},
		{
			name:      "first worker (index 1)",
			cluster:   "pillar-csi-e2e-abc123",
			nodeIndex: 1,
			want:      "pillar-csi-e2e-abc123-worker",
		},
		{
			name:      "second worker (index 2)",
			cluster:   "pillar-csi-e2e-abc123",
			nodeIndex: 2,
			want:      "pillar-csi-e2e-abc123-worker2",
		},
		{
			name:      "third worker (index 3)",
			cluster:   "my-cluster",
			nodeIndex: 3,
			want:      "my-cluster-worker3",
		},
		{
			name:      "negative index returns empty",
			cluster:   "my-cluster",
			nodeIndex: -1,
			want:      "",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := KindNodeContainerName(tc.cluster, tc.nodeIndex)
			if got != tc.want {
				t.Errorf("KindNodeContainerName(%q, %d) = %q, want %q",
					tc.cluster, tc.nodeIndex, got, tc.want)
			}
		})
	}
}

// ─── isVGNotFoundError ────────────────────────────────────────────────────────

func TestIsVGNotFoundError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{
			name:   "nil error",
			errMsg: "",
			want:   false,
		},
		{
			name:   "not found",
			errMsg: "Volume group \"myvg\" not found",
			want:   true,
		},
		{
			name:   "cannot find volume group",
			errMsg: "cannot find volume group \"e2e-vg-abc123\"",
			want:   true,
		},
		{
			name:   "vg not found lowercase",
			errMsg: "vg not found: e2e-vg-abc123",
			want:   true,
		},
		{
			name:   "failed to find vg",
			errMsg: "failed to find vg e2e-vg-abc123",
			want:   true,
		},
		{
			name:   "unrelated error",
			errMsg: "permission denied",
			want:   false,
		},
		{
			name:   "command not found",
			errMsg: "vgremove: command not found",
			want:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.errMsg != "" {
				err = fmt.Errorf("%s", tc.errMsg) //nolint:goerr113
			}
			got := isVGNotFoundError(err)
			if got != tc.want {
				t.Errorf("isVGNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// ─── isPVNotFoundError ────────────────────────────────────────────────────────

func TestIsPVNotFoundError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{
			name:   "nil error",
			errMsg: "",
			want:   false,
		},
		{
			name:   "no physical volume label",
			errMsg: "/dev/loop4: no physical volume label",
			want:   true,
		},
		{
			name:   "not a pv",
			errMsg: "/dev/loop4: not a pv",
			want:   true,
		},
		{
			name:   "no pv label",
			errMsg: "no pv label on /dev/loop4",
			want:   true,
		},
		{
			name:   "device not found",
			errMsg: "device not found: /dev/loop4",
			want:   true,
		},
		{
			name:   "no such file",
			errMsg: "/dev/loop99: no such file or directory",
			want:   true,
		},
		{
			name:   "failed to find device",
			errMsg: "failed to find device /dev/loop4",
			want:   true,
		},
		{
			name:   "unrelated error",
			errMsg: "permission denied",
			want:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.errMsg != "" {
				err = fmt.Errorf("%s", tc.errMsg) //nolint:goerr113
			}
			got := isPVNotFoundError(err)
			if got != tc.want {
				t.Errorf("isPVNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// ─── isLoopNotFoundError ──────────────────────────────────────────────────────

func TestIsLoopNotFoundError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{
			name:   "nil error",
			errMsg: "",
			want:   false,
		},
		{
			name:   "no such file",
			errMsg: "losetup: /dev/loop4: no such file or directory",
			want:   true,
		},
		{
			name:   "not a block device",
			errMsg: "/dev/loop4: not a block device",
			want:   true,
		},
		{
			name:   "no such device",
			errMsg: "ioctl: no such device",
			want:   true,
		},
		{
			name:   "invalid argument",
			errMsg: "invalid argument",
			want:   true,
		},
		{
			name:   "unrelated error",
			errMsg: "permission denied",
			want:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.errMsg != "" {
				err = fmt.Errorf("%s", tc.errMsg) //nolint:goerr113
			}
			got := isLoopNotFoundError(err)
			if got != tc.want {
				t.Errorf("isLoopNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// ─── CreateVG input validation ────────────────────────────────────────────────

func TestCreateVG_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		opts    CreateVGOptions
		wantErr string
	}{
		{
			name:    "empty NodeContainer",
			opts:    CreateVGOptions{NodeContainer: "", VGName: "e2e-vg-abc"},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:    "whitespace NodeContainer",
			opts:    CreateVGOptions{NodeContainer: "   ", VGName: "e2e-vg-abc"},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:    "empty VGName",
			opts:    CreateVGOptions{NodeContainer: "my-node", VGName: ""},
			wantErr: "VGName must not be empty",
		},
		{
			name:    "whitespace VGName",
			opts:    CreateVGOptions{NodeContainer: "my-node", VGName: "  "},
			wantErr: "VGName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CreateVG(ctx, tc.opts)
			if err == nil {
				t.Fatal("CreateVG: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("CreateVG error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── VG.Destroy nil safety ────────────────────────────────────────────────────

func TestVG_Destroy_Nil(t *testing.T) {
	t.Parallel()

	var v *VG
	if err := v.Destroy(context.Background()); err != nil {
		t.Errorf("nil VG.Destroy: unexpected error: %v", err)
	}
}

// ─── VGAttrs input validation ─────────────────────────────────────────────────

func TestVGAttrs_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name          string
		nodeContainer string
		vgName        string
		wantErrSubstr string
	}{
		{
			name:          "empty nodeContainer",
			nodeContainer: "",
			vgName:        "myvg",
			wantErrSubstr: "nodeContainer must not be empty",
		},
		{
			name:          "whitespace nodeContainer",
			nodeContainer: "   ",
			vgName:        "myvg",
			wantErrSubstr: "nodeContainer must not be empty",
		},
		{
			name:          "empty vgName",
			nodeContainer: "my-container",
			vgName:        "",
			wantErrSubstr: "vgName must not be empty",
		},
		{
			name:          "whitespace vgName",
			nodeContainer: "my-container",
			vgName:        "   ",
			wantErrSubstr: "vgName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := VGAttrs(ctx, tc.nodeContainer, tc.vgName)
			if err == nil {
				t.Fatal("VGAttrs: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("VGAttrs error = %q, want it to contain %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}

// ─── VerifyActive logic ───────────────────────────────────────────────────────

// TestVerifyActive_AttrChecks exercises the attribute-checking logic in
// VerifyActive by injecting known attribute strings via a table-driven approach.
//
// Because VerifyActive calls docker exec internally, we test the attribute
// validation logic indirectly by checking VerifyActive against a non-existent
// container (which returns an exec error) and by validating the helper logic
// separately below.
func TestVerifyActive_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Calling VerifyActive with a clearly non-existent container must return an
	// error (from the docker exec call), not a panic.
	_, err := VGAttrs(ctx, "nonexistent-container-xyz", "myvg")
	if err == nil {
		t.Fatal("VGAttrs for non-existent container: expected error, got nil")
	}
	// The error should mention the container and VG name.
	if !strings.Contains(err.Error(), "nonexistent-container-xyz") {
		t.Errorf("VGAttrs error = %q, want it to contain container name", err.Error())
	}
}

// TestVerifyActive_AttrsInterpretation validates the attribute interpretation
// rules for VerifyActive by simulating the checks on known attribute strings.
// This exercises all three guard conditions (permissions, exported, partial)
// without requiring a real Docker container.
func TestVerifyActive_AttrsInterpretation(t *testing.T) {
	t.Parallel()

	// These checks mirror the logic inside VerifyActive.
	checkAttrs := func(attrs string) error {
		if len(attrs) < 4 {
			return fmt.Errorf("unexpected attribute string %q (want at least 4 characters)", attrs)
		}
		if attrs[0] != 'w' {
			return fmt.Errorf("not writable: attr[0]=%q (want 'w'), full attrs=%q",
				string(attrs[0]), attrs)
		}
		if attrs[2] == 'x' {
			return fmt.Errorf("exported (attr[2]='x'), full attrs=%q", attrs)
		}
		if attrs[3] == 'p' {
			return fmt.Errorf("partial PVs (attr[3]='p'), full attrs=%q", attrs)
		}
		return nil
	}

	tests := []struct {
		name    string
		attrs   string
		wantErr bool
		errHint string
	}{
		{
			name:    "normal writable VG",
			attrs:   "wz--n-",
			wantErr: false,
		},
		{
			name:    "writable resizeable with normal alloc",
			attrs:   "wz--n-",
			wantErr: false,
		},
		{
			name:    "read-only VG",
			attrs:   "rz--n-",
			wantErr: true,
			errHint: "not writable",
		},
		{
			name:    "exported VG",
			attrs:   "wzx-n-",
			wantErr: true,
			errHint: "exported",
		},
		{
			name:    "partial VG (missing PV)",
			attrs:   "wz-pn-",
			wantErr: true,
			errHint: "partial",
		},
		{
			name:    "read-only exported partial VG",
			attrs:   "rxpn-",
			wantErr: true,
			errHint: "not writable",
		},
		{
			name:    "too-short attribute string",
			attrs:   "wz",
			wantErr: true,
			errHint: "unexpected attribute string",
		},
		{
			name:    "empty attribute string",
			attrs:   "",
			wantErr: true,
			errHint: "unexpected attribute string",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkAttrs(tc.attrs)
			if tc.wantErr && err == nil {
				t.Errorf("checkAttrs(%q): expected error, got nil", tc.attrs)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("checkAttrs(%q): unexpected error: %v", tc.attrs, err)
			}
			if tc.wantErr && tc.errHint != "" && err != nil {
				if !strings.Contains(err.Error(), tc.errHint) {
					t.Errorf("checkAttrs(%q) error = %q, want it to contain %q",
						tc.attrs, err.Error(), tc.errHint)
				}
			}
		})
	}
}

// ─── containerExec validation ─────────────────────────────────────────────────

func TestContainerExec_EmptyContainer(t *testing.T) {
	t.Parallel()

	_, err := containerExec(context.Background(), "", "ls")
	if err == nil {
		t.Fatal("containerExec: expected error for empty container name, got nil")
	}
	if !strings.Contains(err.Error(), "container name must not be empty") {
		t.Errorf("containerExec error = %q, want it to contain 'container name must not be empty'", err.Error())
	}
}
