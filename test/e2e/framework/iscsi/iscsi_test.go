package iscsi

import (
	"context"
	"testing"
)

// TestTargetIQNFormat verifies that well-formed IQNs are accepted and
// mis-formed ones (empty) are rejected by CreateTarget validation.
func TestTargetIQNFormat(t *testing.T) {
	cases := []struct {
		name    string
		iqn     string
		wantErr bool
	}{
		{
			name:    "valid IQN",
			iqn:     "iqn.2026-01.com.bhyoo.pillar-csi:abc123",
			wantErr: false,
		},
		{
			name:    "empty IQN",
			iqn:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only IQN",
			iqn:     "   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateTarget(context.Background(), CreateTargetOptions{
				NodeContainer: "dummy-node",
				IQN:           tc.iqn,
				SizeMiB:       64,
			})
			// We expect an error from the docker exec (since there is no real
			// container), but we specifically want a validation error (not a
			// docker error) for the empty/whitespace cases.
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected validation error, got nil")
				}
				// For empty IQN, the error must come from our validation layer,
				// not from docker. Check that it does not mention "docker exec".
				if isDockerExecError(err.Error()) {
					t.Fatalf("expected validation error before docker exec, got: %v", err)
				}
			}
			// For the valid IQN case we expect a docker error (no real node),
			// not a panic or a nil error.
		})
	}
}

// TestCreateTarget_ValidationErrors verifies that CreateTarget returns errors
// for missing required fields without invoking docker.
func TestCreateTarget_ValidationErrors(t *testing.T) {
	t.Run("empty NodeContainer", func(t *testing.T) {
		_, err := CreateTarget(context.Background(), CreateTargetOptions{
			NodeContainer: "",
			IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:test",
			SizeMiB:       64,
		})
		if err == nil {
			t.Fatal("expected error for empty NodeContainer, got nil")
		}
		if isDockerExecError(err.Error()) {
			t.Fatalf("expected validation error, not docker error: %v", err)
		}
	})

	t.Run("whitespace NodeContainer", func(t *testing.T) {
		_, err := CreateTarget(context.Background(), CreateTargetOptions{
			NodeContainer: "   ",
			IQN:           "iqn.2026-01.com.bhyoo.pillar-csi:test",
			SizeMiB:       64,
		})
		if err == nil {
			t.Fatal("expected error for whitespace NodeContainer, got nil")
		}
	})

	t.Run("empty IQN", func(t *testing.T) {
		_, err := CreateTarget(context.Background(), CreateTargetOptions{
			NodeContainer: "some-node",
			IQN:           "",
			SizeMiB:       64,
		})
		if err == nil {
			t.Fatal("expected error for empty IQN, got nil")
		}
	})
}

// TestTarget_Destroy_Nil verifies that calling Destroy on a nil *Target is a
// safe no-op that returns nil.
func TestTarget_Destroy_Nil(t *testing.T) {
	var target *Target
	if err := target.Destroy(context.Background()); err != nil {
		t.Fatalf("nil Target.Destroy returned error: %v", err)
	}
}

// TestContainerExec_EmptyContainer verifies that containerExec returns an
// error (not a panic) when the container name is empty.
func TestContainerExec_EmptyContainer(t *testing.T) {
	_, err := containerExec(context.Background(), "", "echo", "hello")
	if err == nil {
		t.Fatal("expected error for empty container name, got nil")
	}
}

// TestIsTargetNotFoundError is a table-driven test for the error classifier.
func TestIsTargetNotFoundError(t *testing.T) {
	cases := []struct {
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
			name:   "can't find the target",
			errMsg: "docker exec node tgtadm ...: can't find the target",
			want:   true,
		},
		{
			name:   "no such target",
			errMsg: "No such target",
			want:   true,
		},
		{
			name:   "target not found",
			errMsg: "target not found",
			want:   true,
		},
		{
			name:   "invalid target id",
			errMsg: "invalid target id",
			want:   true,
		},
		{
			name:   "unrelated error",
			errMsg: "connection refused",
			want:   false,
		},
		{
			name:   "command not found",
			errMsg: "tgtadm: command not found",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.errMsg != "" {
				err = stringError(tc.errMsg)
			}
			got := isTargetNotFoundError(err)
			if got != tc.want {
				t.Errorf("isTargetNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// TestIsLoopNotFoundError is a table-driven test for the loop error classifier.
func TestIsLoopNotFoundError(t *testing.T) {
	cases := []struct {
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
			errMsg: "losetup -d /dev/loop9: no such file or directory",
			want:   true,
		},
		{
			name:   "no such device",
			errMsg: "no such device",
			want:   true,
		},
		{
			name:   "not a block device",
			errMsg: "not a block device",
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

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if tc.errMsg != "" {
				err = stringError(tc.errMsg)
			}
			got := isLoopNotFoundError(err)
			if got != tc.want {
				t.Errorf("isLoopNotFoundError(%q) = %v, want %v", tc.errMsg, got, tc.want)
			}
		})
	}
}

// isDockerExecError returns true when the error message contains "docker exec",
// which indicates the error originated from an actual docker invocation rather
// than from our validation layer.
func isDockerExecError(msg string) bool {
	return len(msg) >= 10 && containsCI(msg, "docker exec")
}

func containsCI(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	ls := toLower(s)
	lsub := toLower(sub)
	for i := range len(ls) - len(lsub) + 1 {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// stringError is a minimal error type for tests.
type stringError string

func (e stringError) Error() string { return string(e) }
