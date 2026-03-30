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

// zfs_backend_core_rpcs_e2e_test.go — E2E tests for the 6 core ZFS backend RPCs
// exercised against the external pillar-agent with a real ZFS loopback pool.
//
// RPCs covered:
//   - CreateVolume  — creates a ZFS zvol and returns device_path + capacity
//   - DeleteVolume  — destroys the ZFS zvol; idempotent on missing volume
//   - ExpandVolume  — grows a ZFS zvol to at least the requested size
//   - GetCapacity   — reports total/available bytes for the ZFS pool
//   - ListVolumes   — enumerates all zvols in the pool with device_path
//   - DevicePath    — verified via CreateVolumeResponse.device_path and
//     VolumeInfo.device_path in ListVolumes response
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true (or EXTERNAL_AGENT_ADDR set).
//   - The ZFS pool (E2E_ZFS_POOL, default "e2e-pool") must exist — created by
//     TestMain.setupZFSPool() before m.Run() is called.
//   - testEnv.zfsHostExec must be non-nil (set by setupZFSPool on success).
//
// # Test isolation
//
// Each test case uses a unique volume name derived from the nanosecond timestamp
// to prevent collisions when parallel runs target the same pool.  Cleanup is
// always registered with DeferCleanup before the operation under test, so the
// zvol is removed even when an assertion fails.
package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// zfsAgentDial opens a plaintext gRPC connection to the external agent and
// returns the connection.  The caller is responsible for DeferCleanup(conn.Close).
func zfsAgentDial(ctx context.Context, addr string) *grpc.ClientConn {
	GinkgoHelper()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck
		dialCtx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	Expect(err).NotTo(HaveOccurred(),
		"gRPC dial to external agent at %s must succeed", addr)
	return conn
}

// zfsVolName returns a unique volume name for a given test tag.
func zfsVolName(tag string) string {
	return fmt.Sprintf("%s-%d", tag, time.Now().UnixNano()%1_000_000)
}

// ─────────────────────────────────────────────────────────────────────────────
// ZFSBackendCoreRPCs — all 6 core backend RPCs exercised against real ZFS
// ─────────────────────────────────────────────────────────────────────────────

var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ZFSBackendCoreRPCs", Ordered, func() {

		// ── BeforeAll: prerequisite guards ──────────────────────────────────────
		BeforeAll(func() {
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ZFS host-exec helper must be available — setupZFSPool() must have succeeded")
			Expect(testEnv.ZFSPoolName).NotTo(BeEmpty(),
				"ZFS pool name must be set (E2E_ZFS_POOL or default 'e2e-pool')")
		})

		// ── GetCapacity: real ZFS pool returns positive capacity values ─────────
		It("GetCapacity returns positive total and available bytes for the real ZFS pool", func(ctx SpecContext) {
			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			By(fmt.Sprintf("calling GetCapacity for pool %q", testEnv.ZFSPoolName))
			resp, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				PoolName:    testEnv.ZFSPoolName,
			})
			Expect(err).NotTo(HaveOccurred(),
				"GetCapacity for real ZFS pool %q must succeed", testEnv.ZFSPoolName)

			Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
				"GetCapacity.TotalBytes must be > 0 — the pool exists and has physical storage")
			Expect(resp.GetAvailableBytes()).To(BeNumerically(">", 0),
				"GetCapacity.AvailableBytes must be > 0 — no volumes have consumed all space yet")
			Expect(resp.GetTotalBytes()).To(BeNumerically(">=", resp.GetAvailableBytes()),
				"total bytes must be >= available bytes")

			By(fmt.Sprintf("GetCapacity: total=%d available=%d used=%d",
				resp.GetTotalBytes(), resp.GetAvailableBytes(), resp.GetUsedBytes()))
		})

		// ── CreateVolume: creates a zvol and returns correct device_path ────────
		It("CreateVolume returns a non-empty device_path matching /dev/zvol/<pool>/<name>", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := zfsVolName("rpc-create")
			volumeID := poolName + "/" + volName
			expectedDevPath := "/dev/zvol/" + poolName + "/" + volName

			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before the create so the zvol is removed even on failure.
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
			resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20, // 256 MiB
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume(%q) must succeed", volumeID)
			Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", 256<<20),
				"allocated capacity must be at least the requested 256 MiB")

			// ── DevicePath: verify device_path in CreateVolume response ──────────
			//
			// The ZFS backend computes device_path as /dev/zvol/<pool>/<name>
			// without making any kernel calls — it is a pure string derivation
			// from the volume ID.  The response must match this convention.
			By("verifying device_path in CreateVolumeResponse (DevicePath RPC)")
			Expect(resp.GetDevicePath()).To(Equal(expectedDevPath),
				"device_path must be /dev/zvol/<pool>/<name> for a ZFS zvol")

			By(fmt.Sprintf("CreateVolume: device_path=%q capacity=%d",
				resp.GetDevicePath(), resp.GetCapacityBytes()))
		})

		// ── DeleteVolume: destroys a zvol; idempotent on missing volume ─────────
		It("DeleteVolume destroys a zvol and is idempotent on a non-existent volume", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := zfsVolName("rpc-delete")
			volumeID := poolName + "/" + volName
			devPath := "/dev/zvol/" + poolName + "/" + volName

			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Step 1: Create a zvol so there is something to delete.
			By(fmt.Sprintf("creating volume %q before testing DeleteVolume", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20,
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed before DeleteVolume test")

			// Step 2: Wait for the block device to appear (udev async).
			By(fmt.Sprintf("waiting for %s to appear (zvol udev device node)", devPath))
			Eventually(func() error {
				res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx, "test -e "+devPath)
				if execErr != nil {
					return fmt.Errorf("host exec: %w", execErr)
				}
				if !res.Success() {
					return fmt.Errorf("device %s not present yet (exit %d)", devPath, res.ExitCode)
				}
				return nil
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
				"zvol device %s must appear before DeleteVolume", devPath)

			// Step 3: Delete the zvol.
			By(fmt.Sprintf("calling DeleteVolume for %q", volumeID))
			_, err = client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			Expect(err).NotTo(HaveOccurred(),
				"DeleteVolume(%q) must succeed", volumeID)

			// Step 4: Verify the block device is gone.
			By(fmt.Sprintf("verifying %s is removed after DeleteVolume", devPath))
			Eventually(func() error {
				res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx, "test -e "+devPath)
				if execErr != nil {
					return fmt.Errorf("host exec: %w", execErr)
				}
				if res.Success() {
					return fmt.Errorf("device %s still present after DeleteVolume", devPath)
				}
				return nil
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
				"zvol device %s must disappear after DeleteVolume", devPath)

			// Step 5: Idempotency — deleting again must succeed.
			By(fmt.Sprintf("calling DeleteVolume again for %q (idempotency check)", volumeID))
			_, err = client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			Expect(err).NotTo(HaveOccurred(),
				"DeleteVolume on a non-existent volume must return success (idempotent)")

			By("DeleteVolume idempotency confirmed")
		})

		// ── ExpandVolume: grows a zvol to the requested size ────────────────────
		It("ExpandVolume grows a ZFS zvol to at least the requested size", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := zfsVolName("rpc-expand")
			volumeID := poolName + "/" + volName

			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before creating the volume.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting volume %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			const initialBytes = 256 << 20  // 256 MiB
			const expandedBytes = 512 << 20 // 512 MiB

			// Step 1: Create the volume at initial size.
			By(fmt.Sprintf("creating volume %q at %d bytes (256 MiB)", volumeID, initialBytes))
			createResp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: initialBytes,
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed before ExpandVolume test")
			Expect(createResp.GetCapacityBytes()).To(BeNumerically(">=", initialBytes),
				"initial allocated capacity must be at least the requested size")

			// Step 2: Expand to double the initial size.
			By(fmt.Sprintf("calling ExpandVolume for %q to %d bytes (512 MiB)", volumeID, expandedBytes))
			expandResp, err := client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volumeID,
				RequestedBytes: expandedBytes,
			})
			Expect(err).NotTo(HaveOccurred(),
				"ExpandVolume(%q, %d) must succeed", volumeID, expandedBytes)
			Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", expandedBytes),
				"ExpandVolume must allocate at least the requested 512 MiB")

			By(fmt.Sprintf("ExpandVolume: new capacity=%d bytes (%.1f MiB)",
				expandResp.GetCapacityBytes(), float64(expandResp.GetCapacityBytes())/(1<<20)))
		})

		// ── ListVolumes: enumerates zvols including device_path ─────────────────
		It("ListVolumes returns created volumes with correct device_path", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := zfsVolName("rpc-list")
			volumeID := poolName + "/" + volName
			expectedDevPath := "/dev/zvol/" + poolName + "/" + volName

			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before creating.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting volume %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			// Step 1: Create a volume so there is something to list.
			By(fmt.Sprintf("creating volume %q before testing ListVolumes", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20,
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed before ListVolumes test")

			// Step 2: List volumes and verify the new zvol appears.
			By(fmt.Sprintf("calling ListVolumes for pool %q", poolName))
			listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				PoolName:    poolName,
			})
			Expect(err).NotTo(HaveOccurred(),
				"ListVolumes for pool %q must succeed", poolName)

			// Find our volume in the list.
			var found *agentv1.VolumeInfo
			for _, vi := range listResp.GetVolumes() {
				if vi.GetVolumeId() == volumeID {
					found = vi
					break
				}
			}
			Expect(found).NotTo(BeNil(),
				"volume %q must appear in ListVolumes response; got %d volume(s): %v",
				volumeID, len(listResp.GetVolumes()), volumeIDs(listResp.GetVolumes()))

			// ── DevicePath in ListVolumes ─────────────────────────────────────────
			//
			// VolumeInfo.device_path is populated by the backend's DevicePath method.
			// For ZFS zvols this must be /dev/zvol/<pool>/<name>.
			Expect(found.GetDevicePath()).To(Equal(expectedDevPath),
				"VolumeInfo.device_path must be /dev/zvol/<pool>/<name> for ZFS zvols")
			Expect(found.GetCapacityBytes()).To(BeNumerically(">", 0),
				"VolumeInfo.capacity_bytes must be positive")

			By(fmt.Sprintf("ListVolumes: found volume %q device_path=%q capacity=%d",
				found.GetVolumeId(), found.GetDevicePath(), found.GetCapacityBytes()))
		})

		// ── CreateVolume idempotency: re-create with same params succeeds ───────
		It("CreateVolume is idempotent: re-creating with same volume ID succeeds", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := zfsVolName("rpc-idempotent")
			volumeID := poolName + "/" + volName

			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting volume %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			const reqBytes = 256 << 20

			By(fmt.Sprintf("first CreateVolume for %q (%d bytes)", volumeID, reqBytes))
			resp1, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
			})
			Expect(err).NotTo(HaveOccurred(), "first CreateVolume must succeed")

			By(fmt.Sprintf("second CreateVolume for %q (idempotency check)", volumeID))
			resp2, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
			})
			Expect(err).NotTo(HaveOccurred(),
				"second CreateVolume with same ID and capacity must succeed (idempotent)")
			Expect(resp2.GetDevicePath()).To(Equal(resp1.GetDevicePath()),
				"device_path must be the same on idempotent re-create")
			Expect(resp2.GetCapacityBytes()).To(Equal(resp1.GetCapacityBytes()),
				"capacity_bytes must be the same on idempotent re-create")

			By("CreateVolume idempotency confirmed")
		})

	}) // end Describe("ZFSBackendCoreRPCs")
	return true
}()

// volumeIDs extracts volume IDs from a slice of VolumeInfo for use in error
// messages that show which volumes were actually returned by ListVolumes.
func volumeIDs(vols []*agentv1.VolumeInfo) []string {
	ids := make([]string, 0, len(vols))
	for _, v := range vols {
		ids = append(ids, v.GetVolumeId())
	}
	return ids
}

// ─────────────────────────────────────────────────────────────────────────────
// ZFSGetCapacityPoolNotFound — GetCapacity with a non-existent pool
// ─────────────────────────────────────────────────────────────────────────────
//
// This Describe block runs even when zfsHostExec is nil because it tests the
// agent's error-path (no ZFS pool required) and duplicates the check in
// external_agent_test.go to ensure it is included in the ZFS RPC coverage.

var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ZFSGetCapacityPoolNotFound", func() {
		It("returns an error for a non-existent ZFS pool", func(ctx SpecContext) {
			conn := zfsAgentDial(ctx, testEnv.ExternalAgentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)
			_, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
				PoolName:    "does-not-exist-" + strings.Repeat("x", 8),
			})
			Expect(err).To(HaveOccurred(),
				"GetCapacity for a non-existent pool must return an error")
			By("GetCapacity correctly returned an error for the unknown pool")
		})
	})
	return true
}()
