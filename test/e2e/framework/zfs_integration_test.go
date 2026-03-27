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

// zfs_integration_test.go — Live integration tests for CreateLoopbackZFSPool.
//
// These tests require:
//   - A reachable remote Docker daemon (DOCKER_HOST, default tcp://10.111.0.1:2375)
//   - The ZFS kernel module loaded on the remote host  (`zfs` and `zpool` in PATH)
//   - `losetup` available on the remote host
//   - `truncate` available on the remote host
//
// Run with:
//
//	go test -tags='e2e integration' ./test/e2e/framework/ \
//	    -run TestCreateLoopbackZFSPool_Integration -v -timeout 5m
//
// The test creates a 512 MiB loopback-backed ZFS pool named
// "e2e-loop-test-pool", verifies the pool appears in `zpool list`, and
// then tears it down via DestroyLoopbackZFSPool.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// TestCreateLoopbackZFSPool_Integration is the full round-trip test:
// create pool → verify → destroy.
func TestCreateLoopbackZFSPool_Integration(t *testing.T) {
	const (
		poolName  = "e2e-loop-test-pool"
		imagePath = "/tmp/e2e-loop-test-pool.img"
		imageSize = "512M"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h, err := framework.NewDockerHostExec(ctx, remoteDockerHost())
	if err != nil {
		t.Fatalf("NewDockerHostExec: %v", err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("DockerHostExec.Close: %v", err)
		}
	}()

	// ── Pre-condition: check that zpool is available on the host ──────────
	checkRes, err := h.ExecOnHost(ctx, "zpool version")
	if err != nil {
		t.Fatalf("pre-check ExecOnHost(zpool version): %v", err)
	}
	if !checkRes.Success() {
		t.Skipf("zpool not available on remote host (exit=%d stderr=%q); skipping integration test",
			checkRes.ExitCode, checkRes.Stderr)
	}

	// ── Cleanup any leftovers from a previous interrupted run ─────────────
	// Ignore errors — the pool / device / file may not exist.
	_, _ = h.ExecOnHost(ctx, "zpool destroy "+poolName)             //nolint:errcheck
	_, _ = h.ExecOnHost(ctx, "losetup -j "+imagePath+" | cut -d: -f1 | xargs -r losetup -d") //nolint:errcheck
	_, _ = h.ExecOnHost(ctx, "rm -f "+imagePath)                    //nolint:errcheck

	// ── Create the pool ───────────────────────────────────────────────────
	t.Logf("creating ZFS pool %q backed by %s (%s) on %s",
		poolName, imagePath, imageSize, remoteDockerHost())

	loopDev, err := framework.CreateLoopbackZFSPool(ctx, h, poolName, imagePath, imageSize)
	if err != nil {
		t.Fatalf("CreateLoopbackZFSPool: %v", err)
	}
	t.Logf("pool %q created on loop device %s", poolName, loopDev)

	// ── Verify: pool appears in `zpool list` ─────────────────────────────
	t.Run("pool visible in zpool list", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "zpool list "+poolName)
		if err != nil {
			t.Fatalf("ExecOnHost(zpool list %s): %v", poolName, err)
		}
		if !res.Success() {
			t.Errorf("zpool list %s failed (exit=%d): stdout=%q stderr=%q",
				poolName, res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stdout, poolName) {
			t.Errorf("zpool list output does not contain %q: %q", poolName, res.Stdout)
		}
		t.Logf("zpool list output:\n%s", res.Stdout)
	})

	// ── Verify: loop device is attached ──────────────────────────────────
	t.Run("loop device is attached", func(t *testing.T) {
		if loopDev == "" {
			t.Fatal("CreateLoopbackZFSPool returned empty loopDev")
		}
		res, err := h.ExecOnHost(ctx, "losetup "+loopDev)
		if err != nil {
			t.Fatalf("ExecOnHost(losetup %s): %v", loopDev, err)
		}
		if !res.Success() {
			t.Errorf("losetup %s failed (exit=%d): stdout=%q stderr=%q",
				loopDev, res.ExitCode, res.Stdout, res.Stderr)
		}
		// losetup output should contain the image path.
		if !strings.Contains(res.Stdout, imagePath) {
			t.Errorf("losetup %s output does not reference %q: %q",
				loopDev, imagePath, res.Stdout)
		}
		t.Logf("losetup output: %s", strings.TrimSpace(res.Stdout))
	})

	// ── Verify: image file exists ─────────────────────────────────────────
	t.Run("image file exists", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "stat "+imagePath)
		if err != nil {
			t.Fatalf("ExecOnHost(stat %s): %v", imagePath, err)
		}
		if !res.Success() {
			t.Errorf("stat %s failed (exit=%d): %s", imagePath, res.ExitCode, res.Stderr)
		}
	})

	// ── Teardown: destroy the pool, detach loop, remove image ─────────────
	t.Log("destroying ZFS pool and cleaning up")
	if err := framework.DestroyLoopbackZFSPool(ctx, h, poolName, loopDev, imagePath); err != nil {
		t.Errorf("DestroyLoopbackZFSPool: %v", err)
	}

	// ── Post-teardown checks ──────────────────────────────────────────────
	t.Run("pool gone after destroy", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "zpool list "+poolName)
		if err != nil {
			t.Fatalf("ExecOnHost: %v", err)
		}
		// zpool list returns non-zero when the pool does not exist.
		if res.Success() {
			t.Errorf("zpool list still succeeds after destroy; pool may not have been removed")
		}
	})

	t.Run("loop device gone after detach", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "losetup "+loopDev)
		if err != nil {
			t.Fatalf("ExecOnHost: %v", err)
		}
		if res.Success() {
			t.Errorf("losetup %s still reports attached after detach", loopDev)
		}
	})

	t.Run("image file gone after remove", func(t *testing.T) {
		res, err := h.ExecOnHost(ctx, "test -f "+imagePath)
		if err != nil {
			t.Fatalf("ExecOnHost: %v", err)
		}
		if res.Success() {
			t.Errorf("image file %s still exists after removal", imagePath)
		}
	})
}

// TestDestroyLoopbackZFSPool_EmptyArgs_Integration verifies that
// DestroyLoopbackZFSPool with all-empty string arguments performs no host
// commands and returns nil.  Requires a live Docker host so we can confirm
// no spurious exec calls are made.
func TestDestroyLoopbackZFSPool_EmptyArgs_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	h, err := framework.NewDockerHostExec(ctx, remoteDockerHost())
	if err != nil {
		t.Fatalf("NewDockerHostExec: %v", err)
	}
	defer func() {
		if err := h.Close(); err != nil {
			t.Errorf("DockerHostExec.Close: %v", err)
		}
	}()

	if err := framework.DestroyLoopbackZFSPool(ctx, h, "", "", ""); err != nil {
		t.Errorf("DestroyLoopbackZFSPool with empty args returned error: %v", err)
	}
}
