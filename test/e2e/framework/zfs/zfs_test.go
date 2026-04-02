package zfs

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

// ─── isZPoolNotFoundError ─────────────────────────────────────────────────────

func TestIsZPoolNotFoundError(t *testing.T) {
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
			name:   "no such pool",
			errMsg: "cannot open 'tank': no such pool",
			want:   true,
		},
		{
			name:   "cannot open",
			errMsg: "docker exec node zpool list mypool: cannot open 'mypool': no such pool",
			want:   true,
		},
		{
			name:   "does not exist",
			errMsg: "pool does not exist",
			want:   true,
		},
		{
			name:   "unrelated error",
			errMsg: "permission denied",
			want:   false,
		},
		{
			name:   "zpool import required",
			errMsg: "missing device in configuration",
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
			got := isZPoolNotFoundError(err)
			if got != tc.want {
				t.Errorf("isZPoolNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
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

// ─── CreatePool input validation ──────────────────────────────────────────────

func TestCreatePool_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		opts    CreatePoolOptions
		wantErr string
	}{
		{
			name:    "empty NodeContainer",
			opts:    CreatePoolOptions{NodeContainer: "", PoolName: "tank"},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:    "whitespace NodeContainer",
			opts:    CreatePoolOptions{NodeContainer: "   ", PoolName: "tank"},
			wantErr: "NodeContainer must not be empty",
		},
		{
			name:    "empty PoolName",
			opts:    CreatePoolOptions{NodeContainer: "my-node", PoolName: ""},
			wantErr: "PoolName must not be empty",
		},
		{
			name:    "whitespace PoolName",
			opts:    CreatePoolOptions{NodeContainer: "my-node", PoolName: "  "},
			wantErr: "PoolName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CreatePool(ctx, tc.opts)
			if err == nil {
				t.Fatal("CreatePool: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("CreatePool error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── Pool.Destroy nil safety ──────────────────────────────────────────────────

func TestPool_Destroy_Nil(t *testing.T) {
	t.Parallel()

	var p *Pool
	if err := p.Destroy(context.Background()); err != nil {
		t.Errorf("nil Pool.Destroy: unexpected error: %v", err)
	}
}

// ─── PoolState input validation ───────────────────────────────────────────────

func TestPoolState_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		cont    string
		pool    string
		wantErr string
	}{
		{
			name:    "empty nodeContainer",
			cont:    "",
			pool:    "tank",
			wantErr: "nodeContainer must not be empty",
		},
		{
			name:    "whitespace nodeContainer",
			cont:    "   ",
			pool:    "tank",
			wantErr: "nodeContainer must not be empty",
		},
		{
			name:    "empty poolName",
			cont:    "my-node",
			pool:    "",
			wantErr: "poolName must not be empty",
		},
		{
			name:    "whitespace poolName",
			cont:    "my-node",
			pool:    "  ",
			wantErr: "poolName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := PoolState(ctx, tc.cont, tc.pool)
			if err == nil {
				t.Fatal("PoolState: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("PoolState error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ─── VerifyOnline input validation ────────────────────────────────────────────

func TestVerifyOnline_ValidationErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name    string
		cont    string
		pool    string
		wantErr string
	}{
		{
			name:    "empty nodeContainer propagated",
			cont:    "",
			pool:    "tank",
			wantErr: "nodeContainer must not be empty",
		},
		{
			name:    "empty poolName propagated",
			cont:    "my-node",
			pool:    "",
			wantErr: "poolName must not be empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := VerifyOnline(ctx, tc.cont, tc.pool)
			if err == nil {
				t.Fatal("VerifyOnline: expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("VerifyOnline error = %q, want it to contain %q", err.Error(), tc.wantErr)
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
