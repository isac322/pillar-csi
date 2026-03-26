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

// zfs_zvol_e2e_test.go — E2E tests for real ZFS zvol creation via the
// pillar-agent gRPC API.
//
// These tests validate that a CreateVolume RPC to the pillar-agent results in
// a real ZFS zvol being created on the host's ZFS pool, and that the
// corresponding block device symlink appears at /dev/zvol/<pool>/<name> on
// the remote host.
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true (or EXTERNAL_AGENT_ADDR set) so that the
//     external agent container is running.  TestMain starts the container with
//     --privileged so the agent binary can call 'zfs create' via the host
//     kernel's /dev/zfs control device.
//
//   - The ZFS pool (E2E_ZFS_POOL, default "e2e-pool") must exist on the remote
//     Docker host.  TestMain creates this loopback pool in setupZFSPool()
//     before m.Run() is called.
//
// # Verification strategy
//
// The test calls CreateVolume over gRPC and then polls the remote host for the
// zvol device node at /dev/zvol/<pool>/<name>.  The poll uses the
// testEnv.zfsHostExec helper (a privileged container that runs in the host's
// mount namespace) to run 'test -e <path>', retrying for up to 10 s to
// tolerate asynchronous udev event processing on heavily-loaded CI hosts.
//
// # Cleanup
//
// DeleteVolume is registered with DeferCleanup before the poll begins so that
// the zvol dataset is removed even when the verification assertion fails.
package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

var _ = Describe("ZFSZvolRealCreate", Ordered, func() {
	// ── BeforeAll: prerequisite guards ──────────────────────────────────────
	//
	// Both the external agent and the ZFS host-exec helper must be available.
	// If either is missing, the entire Describe block is skipped so that the
	// suite can run in environments that lack a real ZFS kernel module or
	// in internal-agent mode.
	BeforeAll(func() {
		if testEnv.ExternalAgentAddr == "" {
			Skip(
				"ZFSZvolRealCreate: external agent not running — " +
					"set E2E_LAUNCH_EXTERNAL_AGENT=true or EXTERNAL_AGENT_ADDR " +
					"to enable ZFS zvol real-create tests",
			)
		}
		if testEnv.zfsHostExec == nil {
			Skip(
				"ZFSZvolRealCreate: ZFS host-exec helper not available — " +
					"setupZFSPool() must have succeeded (check DOCKER_HOST and ZFS " +
					"module availability on the remote host)",
			)
		}
	})

	// ── It: CreateVolume creates a real ZFS zvol ──────────────────────────
	//
	// This spec is the core of AC-3.  It:
	//
	//  1. Opens a plaintext gRPC connection to the external agent.
	//  2. Calls CreateVolume with a unique volume ID under the e2e ZFS pool.
	//  3. Verifies that /dev/zvol/<pool>/<name> appears on the remote host
	//     within 10 s (zvol device nodes are created asynchronously by udev).
	//  4. Calls DeleteVolume to clean up regardless of assertion outcome.
	It("CreateVolume creates /dev/zvol/<pool>/<name> on the remote host", func(ctx SpecContext) {
		poolName := testEnv.ZFSPoolName // e.g. "e2e-pool"
		// Use a sub-millisecond suffix to keep the name short and unique across
		// concurrent test runs on the same host.
		volName := fmt.Sprintf("zvol-e2e-create-%d", time.Now().UnixNano()%1_000_000)
		volumeID := poolName + "/" + volName
		devPath := "/dev/zvol/" + poolName + "/" + volName

		By(fmt.Sprintf("connecting to external agent at %s", testEnv.ExternalAgentAddr))
		// Use a 10-second dial timeout; the agent is already running (started by
		// TestMain) so the connection should be established immediately.
		dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
		defer cancelDial()

		conn, err := grpc.DialContext( //nolint:staticcheck // DialContext is still widely used; NewClient lacks per-call ctx
			dialCtx,
			testEnv.ExternalAgentAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(), //nolint:staticcheck
		)
		Expect(err).NotTo(HaveOccurred(),
			"gRPC dial to external agent at %s must succeed", testEnv.ExternalAgentAddr)
		DeferCleanup(conn.Close)

		client := agentv1.NewAgentServiceClient(conn)

		// Register DeleteVolume cleanup BEFORE CreateVolume so the zvol is
		// removed even when the assertion below fails.  Ginkgo runs
		// DeferCleanup handlers in LIFO order, so this fires after conn.Close.
		DeferCleanup(func(dctx SpecContext) {
			By(fmt.Sprintf("cleanup: deleting volume %q", volumeID))
			if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			}); delErr != nil {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
			}
		})

		By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
		// 256 MiB is small enough to be cheap on a loopback-backed ZFS pool
		// but large enough to exercise the zvol block-device allocation path.
		resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
			VolumeId:      volumeID,
			CapacityBytes: 256 << 20, // 256 MiB
		})
		Expect(err).NotTo(HaveOccurred(),
			"CreateVolume(%q) must succeed — verify the external agent container "+
				"is running with --privileged and that the ZFS pool %q exists on "+
				"the remote host",
			volumeID, poolName)
		Expect(resp.GetCapacityBytes()).To(BeNumerically(">", 0),
			"CreateVolume must return a positive allocated capacity in bytes")
		By(fmt.Sprintf("CreateVolume returned %d bytes allocated", resp.GetCapacityBytes()))

		// ── Verify /dev/zvol/<pool>/<name> exists on the remote host ──────────
		//
		// ZFS zvol device nodes are created asynchronously by udev after the
		// kernel dataset is allocated.  On a quiescent host the node typically
		// appears within a few milliseconds, but under CI load we allow up to
		// 10 s with 500 ms polling.
		//
		// We use ExecOnHost (nsenter into the host namespaces) rather than Exec
		// (inside the helper container) because the zvol symlink is created in
		// the HOST's device filesystem, not inside any container's /dev.
		By(fmt.Sprintf("waiting for %s on remote host (up to 10 s)", devPath))
		Eventually(func() error {
			res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx,
				"test -e "+devPath)
			if execErr != nil {
				return fmt.Errorf("host exec failed: %w", execErr)
			}
			if !res.Success() {
				return fmt.Errorf("device %s not present yet (exit %d stderr=%q)",
					devPath, res.ExitCode, res.Stderr)
			}
			return nil
		}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
			"zvol device %s must appear on the remote host within 10 s of "+
				"CreateVolume returning — check that the ZFS kernel module is "+
				"loaded and that /dev/zvol is present on the remote host",
			devPath)

		By(fmt.Sprintf("confirmed: %s exists on the remote host", devPath))
	})
})
