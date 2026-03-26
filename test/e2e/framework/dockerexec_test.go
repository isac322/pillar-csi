//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package framework_test

// dockerexec_test.go — Unit tests for the ExecResult helpers and the
// buildDockerEnv utility (no Docker daemon required).
//
// The DockerHostExec.Exec / ExecOnHost methods require a live remote Docker
// daemon and are therefore covered by the full e2e suite, not by this file.

import (
	"testing"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// ExecResult helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestExecResult_Success(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		exitCode int
		want     bool
	}{
		{"zero exit is success", 0, true},
		{"non-zero exit is failure", 1, false},
		{"non-zero exit 127 is failure", 127, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := framework.ExecResult{ExitCode: tc.exitCode}
			if got := r.Success(); got != tc.want {
				t.Errorf("ExecResult{ExitCode:%d}.Success() = %v, want %v",
					tc.exitCode, got, tc.want)
			}
		})
	}
}

func TestExecResult_String(t *testing.T) {
	t.Parallel()

	r := framework.ExecResult{
		Stdout:   "hello\n",
		Stderr:   "warning\n",
		ExitCode: 2,
	}
	s := r.String()
	if s == "" {
		t.Fatal("ExecResult.String() returned empty string")
	}
	// Just verify all three fields appear in the output.
	for _, sub := range []string{"2", "hello", "warning"} {
		if len(s) == 0 {
			t.Fatalf("ExecResult.String() is empty")
		}
		found := false
		for i := range len(s) - len(sub) + 1 {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ExecResult.String() = %q, expected to contain %q", s, sub)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DockerHostExec — nil safety
// ─────────────────────────────────────────────────────────────────────────────

func TestDockerHostExec_Close_NilSafe(t *testing.T) {
	t.Parallel()

	var h *framework.DockerHostExec
	// Close on a nil pointer must not panic.
	if err := h.Close(); err != nil {
		t.Errorf("Close() on nil DockerHostExec returned unexpected error: %v", err)
	}
}

func TestDockerHostExec_ContainerID_NilSafe(t *testing.T) {
	t.Parallel()

	var h *framework.DockerHostExec
	if id := h.ContainerID(); id != "" {
		t.Errorf("ContainerID() on nil DockerHostExec = %q, want empty", id)
	}
}
