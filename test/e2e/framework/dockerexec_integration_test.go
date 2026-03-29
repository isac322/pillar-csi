//go:build e2e && integration

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

// dockerexec_integration_test.go — Live integration tests for DockerHostExec.
//
// These tests require the remote Docker daemon to be reachable and are
// therefore gated behind both the "e2e" and "integration" build tags:
//
//	go test -tags='e2e integration' ./test/e2e/framework/ \
//	    -run TestDockerHostExec_Integration -v
//
// DOCKER_HOST must point at the Docker daemon (default tcp://localhost:2375).
// The tests pull debian:bookworm-slim if not already present.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// dockerHost returns the Docker daemon endpoint for integration tests.
// DOCKER_HOST must be set; the test is skipped when it is absent because
// DockerHostExec requires an explicit endpoint string.
func dockerHost(t *testing.T) string {
	t.Helper()
	h := os.Getenv("DOCKER_HOST")
	if h == "" {
		t.Skip("DOCKER_HOST not set — skipping DockerHostExec integration test")
	}
	return h
}

func TestDockerHostExec_Integration_ExecBasic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	h, err := framework.NewDockerHostExec(ctx, dockerHost(t))
	if err != nil {
		t.Fatalf("NewDockerHostExec: %v", err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	t.Run("echo stdout", func(t *testing.T) {
		res, err := h.Exec(ctx, "echo hello-world")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "hello-world") {
			t.Errorf("stdout = %q, expected to contain 'hello-world'", res.Stdout)
		}
	})

	t.Run("capture stderr separately", func(t *testing.T) {
		// Write only to stderr; stdout must be empty.
		res, err := h.Exec(ctx, "echo err-only >&2")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected exit 0, got %d", res.ExitCode)
		}
		if strings.TrimSpace(res.Stdout) != "" {
			t.Errorf("expected empty stdout, got %q", res.Stdout)
		}
		if !strings.Contains(res.Stderr, "err-only") {
			t.Errorf("stderr = %q, expected to contain 'err-only'", res.Stderr)
		}
	})

	t.Run("non-zero exit code", func(t *testing.T) {
		res, err := h.Exec(ctx, "exit 42")
		if err != nil {
			t.Fatalf("Exec returned unexpected Go error: %v", err)
		}
		if res.ExitCode != 42 {
			t.Errorf("expected exit 42, got %d", res.ExitCode)
		}
	})

	t.Run("pipe works", func(t *testing.T) {
		res, err := h.Exec(ctx, "printf 'foo\\nbar\\nbaz' | grep bar")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "bar") {
			t.Errorf("stdout = %q, expected to contain 'bar'", res.Stdout)
		}
	})
}

func TestDockerHostExec_Integration_ExecOnHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	h, err := framework.NewDockerHostExec(ctx, dockerHost(t))
	if err != nil {
		t.Fatalf("NewDockerHostExec: %v", err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	t.Run("runs on host namespace", func(t *testing.T) {
		// /proc/1/cmdline is PID 1 on the *host*.  If we're in the host PID
		// namespace this must be readable and non-empty.
		res, err := h.ExecOnHost(ctx, "cat /proc/1/cmdline | tr '\\0' ' '")
		if err != nil {
			t.Fatalf("ExecOnHost: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
		}
		if strings.TrimSpace(res.Stdout) == "" {
			t.Error("expected non-empty /proc/1/cmdline on host")
		}
		t.Logf("host PID 1 cmdline: %s", strings.TrimSpace(res.Stdout))
	})

	t.Run("uname shows host kernel", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "uname -r")
		if err != nil {
			t.Fatalf("ExecOnHost: %v", err)
		}
		if res.ExitCode != 0 {
			t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
		}
		kernel := strings.TrimSpace(res.Stdout)
		if kernel == "" {
			t.Error("uname -r returned empty string")
		}
		t.Logf("host kernel: %s", kernel)
	})
}

func TestDockerHostExec_Integration_Idempotent(t *testing.T) {
	// Verify that creating a second DockerHostExec with the same container
	// name cleans up the first one rather than failing.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	h1, err := framework.NewDockerHostExec(ctx, dockerHost(t))
	if err != nil {
		t.Fatalf("first NewDockerHostExec: %v", err)
	}
	// Do NOT close h1 — intentionally simulate an interrupted run.

	h2, err := framework.NewDockerHostExec(ctx, dockerHost(t))
	if err != nil {
		t.Fatalf("second NewDockerHostExec (idempotent): %v", err)
	}
	defer func() {
		if err := h2.Close(); err != nil {
			t.Errorf("h2.Close: %v", err)
		}
	}()

	// h1 should be orphaned but h2 should work fine.
	_ = h1 // avoid unused-variable error

	res, err := h2.Exec(ctx, "echo idempotent-ok")
	if err != nil {
		t.Fatalf("Exec on h2: %v", err)
	}
	if !strings.Contains(res.Stdout, "idempotent-ok") {
		t.Errorf("unexpected stdout: %q", res.Stdout)
	}
}
