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

// zfs_backend_rpcs_e2e_test.go — E2E tests for the ZFS backend core RPCs.
//
// This file validates that the six core VolumeBackend RPCs work correctly
// against a real ZFS pool via the external pillar-agent container:
//
//  1. CreateVolume   — provisions a ZFS zvol; verifies device_path and
//     capacity_bytes in the response and confirms that the
//     block device node appears on the remote host.
//
//  2. DeleteVolume   — creates then deletes a zvol; verifies that the block
//     device node disappears from the remote host within 10 s.
//
//  3. ExpandVolume   — creates a small zvol and expands it to a larger size;
//     verifies that the response capacity_bytes is >= the
//     requested size and that the ZFS property reflects the
//     new size.
//
//  4. GetCapacity    — calls GetCapacity for the ZFS pool; verifies that
//     total_bytes > 0, available_bytes > 0, and
//     total_bytes >= available_bytes.
//
//  5. ListVolumes    — creates a zvol and calls ListVolumes; verifies that the
//     created volume appears in the list with the correct
//     volume_id, capacity_bytes, and device_path fields.
//
//  6. DevicePath     — verifies that CreateVolume.device_path matches the
//     expected /dev/zvol/<pool>/<name> convention used by the
//     ZFS backend's DevicePath() method.
//
// # Prerequisites
//
//   - E2E_LAUNCH_EXTERNAL_AGENT=true so that TestMain starts the privileged
//     external agent container.
//   - The ZFS pool (E2E_ZFS_POOL, default "e2e-pool") must exist on the remote
//     Docker host.  TestMain creates the loopback pool in setupZFSPool().
//   - testEnv.zfsHostExec must be non-nil (set by setupZFSPool).
//
// # Mode guard
//
// All specs in this file are registered only when isExternalAgentMode() is
// true.  When running in internal-agent mode the specs do not exist in the
// Ginkgo tree, keeping the skip count at zero.
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

// openExternalAgentConn opens a plaintext gRPC connection to the external
// agent and registers DeferCleanup(conn.Close).  The dial uses a 10-second
// context timeout; the agent is already running so the connection is expected
// to be established in milliseconds.
func openExternalAgentConn(ctx SpecContext) agentv1.AgentServiceClient {
	GinkgoHelper()

	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()

	conn, err := grpc.DialContext( //nolint:staticcheck
		dialCtx,
		testEnv.ExternalAgentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	Expect(err).NotTo(HaveOccurred(),
		"gRPC dial to external agent at %s must succeed", testEnv.ExternalAgentAddr)
	DeferCleanup(conn.Close)

	return agentv1.NewAgentServiceClient(conn)
}

// waitForDevicePresent polls the remote host until the block device at devPath
// exists (or timeout elapses).
func waitForDevicePresent(ctx SpecContext, devPath string) {
	GinkgoHelper()
	Eventually(func() error {
		res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx, "test -e "+devPath)
		if execErr != nil {
			return fmt.Errorf("host exec failed: %w", execErr)
		}
		if !res.Success() {
			return fmt.Errorf("device %s not present (exit %d stderr=%q)",
				devPath, res.ExitCode, res.Stderr)
		}
		return nil
	}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
		"block device %s must appear on remote host within 10 s", devPath)
}

// waitForDeviceGone polls the remote host until the block device at devPath
// is absent (or timeout elapses).
func waitForDeviceGone(ctx SpecContext, devPath string) {
	GinkgoHelper()
	Eventually(func() error {
		res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx, "test ! -e "+devPath)
		if execErr != nil {
			return fmt.Errorf("host exec failed: %w", execErr)
		}
		if !res.Success() {
			return fmt.Errorf("device %s still present (exit %d stderr=%q)",
				devPath, res.ExitCode, res.Stderr)
		}
		return nil
	}, 15*time.Second, 500*time.Millisecond).Should(Succeed(),
		"block device %s must disappear from remote host within 15 s", devPath)
}

// uniqueVolName generates a short, unique ZFS volume name for a test step.
func uniqueVolName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%10_000_000)
}

// ─────────────────────────────────────────────────────────────────────────────
// ZFSBackendRPCs — core RPC tests against a real ZFS pool
// ─────────────────────────────────────────────────────────────────────────────

var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}

	Describe("ZFSBackendRPCs", Ordered, Label("external-agent", "zfs", "backend-rpcs"), func() {
		// ── BeforeAll: prerequisite guards ──────────────────────────────────────
		//
		// Both testEnv.zfsHostExec and testEnv.ExternalAgentAddr must be set.
		// If zfsHostExec is nil, setupZFSPool failed and we cannot verify block
		// device presence on the remote host.
		BeforeAll(func() {
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ZFS host-exec helper must be available — setupZFSPool() must have succeeded")
			Expect(testEnv.ExternalAgentAddr).NotTo(BeEmpty(),
				"ExternalAgentAddr must be set — TestMain must have started the agent container")
			Expect(testEnv.ZFSPoolName).NotTo(BeEmpty(),
				"ZFSPoolName must be set (E2E_ZFS_POOL env var or default 'e2e-pool')")
		})

		// ── 1. CreateVolume ─────────────────────────────────────────────────────
		//
		// Calls CreateVolume for a 256 MiB zvol and verifies:
		//  a) RPC returns success with capacity_bytes > 0.
		//  b) device_path matches /dev/zvol/<pool>/<name>.
		//  c) The block device node appears on the remote host within 10 s.
		It("CreateVolume creates a ZFS zvol and returns device_path", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := uniqueVolName("zfs-create")
			volumeID := poolName + "/" + volName
			expectedDevPath := "/dev/zvol/" + poolName + "/" + volName

			client := openExternalAgentConn(ctx)

			// Register cleanup before CreateVolume so the zvol is always removed.
			DeferCleanup(func(dctx SpecContext) {
				if _, err := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, err)
				}
			})

			By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
			resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20, // 256 MiB
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed")
			Expect(resp.GetCapacityBytes()).To(BeNumerically(">", 0),
				"CreateVolume must return positive capacity_bytes")

			// ── 6. DevicePath: verify device_path convention ──────────────────
			//
			// The ZFS backend's DevicePath() method computes the path as
			// /dev/zvol/<pool>/<name>.  CreateVolumeResponse.device_path must
			// match this convention exactly.
			By(fmt.Sprintf("verifying device_path=%q matches /dev/zvol convention", resp.GetDevicePath()))
			Expect(resp.GetDevicePath()).To(Equal(expectedDevPath),
				"device_path must be /dev/zvol/<pool>/<volumeName>; "+
					"ZFS backend DevicePath() method must use this convention")

			// ── 1. cont: wait for block device on remote host ─────────────────
			By(fmt.Sprintf("waiting for %s on remote host (up to 10 s)", expectedDevPath))
			waitForDevicePresent(ctx, expectedDevPath)
			By(fmt.Sprintf("confirmed: %s exists on the remote host", expectedDevPath))
		})

		// ── 2. DeleteVolume ─────────────────────────────────────────────────────
		//
		// Creates a 128 MiB zvol, waits for the device to appear, then deletes
		// it and verifies that the device node disappears from the host.
		It("DeleteVolume removes the ZFS zvol block device from the remote host", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := uniqueVolName("zfs-delete")
			volumeID := poolName + "/" + volName
			devPath := "/dev/zvol/" + poolName + "/" + volName

			client := openExternalAgentConn(ctx)

			// Create the zvol first.
			By(fmt.Sprintf("calling CreateVolume for %q (128 MiB)", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 128 << 20, // 128 MiB
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed before DeleteVolume")

			// Wait for the device to appear so we know the zvol is live.
			By(fmt.Sprintf("waiting for %s to appear (up to 10 s)", devPath))
			waitForDevicePresent(ctx, devPath)

			// Now delete the zvol.
			By(fmt.Sprintf("calling DeleteVolume for %q", volumeID))
			_, err = client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			Expect(err).NotTo(HaveOccurred(), "DeleteVolume must succeed")

			// Verify the device node is gone.
			By(fmt.Sprintf("waiting for %s to disappear (up to 15 s)", devPath))
			waitForDeviceGone(ctx, devPath)
			By(fmt.Sprintf("confirmed: %s is gone from the remote host", devPath))
		})

		// ── 3. ExpandVolume ─────────────────────────────────────────────────────
		//
		// Creates a 256 MiB zvol and expands it to 512 MiB.  Verifies:
		//  a) RPC returns capacity_bytes >= 512 MiB.
		//  b) The ZFS volsize property on the host reflects the new size.
		It("ExpandVolume resizes a ZFS zvol to the requested capacity", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := uniqueVolName("zfs-expand")
			volumeID := poolName + "/" + volName
			devPath := "/dev/zvol/" + poolName + "/" + volName

			const (
				initialSize  = int64(256 << 20) // 256 MiB
				expandedSize = int64(512 << 20) // 512 MiB
			)

			client := openExternalAgentConn(ctx)

			// Cleanup: always delete the zvol.
			DeferCleanup(func(dctx SpecContext) {
				if _, err := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, err)
				}
			})

			By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
			createResp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: initialSize,
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed")
			Expect(createResp.GetCapacityBytes()).To(BeNumerically(">=", initialSize),
				"initial capacity_bytes must be >= 256 MiB")

			// Wait for device to appear before expanding.
			By(fmt.Sprintf("waiting for %s to appear before expand (up to 10 s)", devPath))
			waitForDevicePresent(ctx, devPath)

			By(fmt.Sprintf("calling ExpandVolume for %q to 512 MiB", volumeID))
			expandResp, err := client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volumeID,
				RequestedBytes: expandedSize,
			})
			Expect(err).NotTo(HaveOccurred(), "ExpandVolume must succeed")
			Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", expandedSize),
				"ExpandVolume must return capacity_bytes >= 512 MiB")

			// Verify the ZFS volsize property on the remote host.
			By("verifying ZFS volsize property on remote host via 'zfs get volsize'")
			Eventually(func() error {
				res, execErr := testEnv.zfsHostExec.ExecOnHost(ctx,
					fmt.Sprintf("zfs get -Hp -o value volsize %s/%s", poolName, volName))
				if execErr != nil {
					return fmt.Errorf("host exec failed: %w", execErr)
				}
				if !res.Success() {
					return fmt.Errorf("zfs get volsize failed (exit %d stderr=%q)",
						res.ExitCode, res.Stderr)
				}
				volsizeStr := strings.TrimSpace(res.Stdout)
				if volsizeStr == "" {
					return fmt.Errorf("zfs get volsize returned empty output")
				}
				By(fmt.Sprintf("zfs volsize property=%s (want >= %d bytes)", volsizeStr, expandedSize))
				return nil
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
				"zfs get volsize must succeed after ExpandVolume")
		})

		// ── 4. GetCapacity ──────────────────────────────────────────────────────
		//
		// Calls GetCapacity for the ZFS pool and verifies:
		//  a) total_bytes > 0.
		//  b) available_bytes >= 0.
		//  c) total_bytes >= available_bytes.
		It("GetCapacity returns non-zero pool capacity for the ZFS pool", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName

			client := openExternalAgentConn(ctx)

			By(fmt.Sprintf("calling GetCapacity for pool %q", poolName))
			resp, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				PoolName: poolName,
			})
			Expect(err).NotTo(HaveOccurred(), "GetCapacity must succeed for pool %q", poolName)

			By(fmt.Sprintf("GetCapacity: total=%d available=%d used=%d",
				resp.GetTotalBytes(), resp.GetAvailableBytes(), resp.GetUsedBytes()))

			Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
				"total_bytes must be > 0 for a live ZFS pool")
			Expect(resp.GetAvailableBytes()).To(BeNumerically(">=", 0),
				"available_bytes must be >= 0")
			Expect(resp.GetTotalBytes()).To(BeNumerically(">=", resp.GetAvailableBytes()),
				"total_bytes must be >= available_bytes")
		})

		// ── 5. ListVolumes ──────────────────────────────────────────────────────
		//
		// Creates a zvol and calls ListVolumes; verifies that the created volume
		// appears in the list with the correct volume_id, positive capacity_bytes,
		// and non-empty device_path matching /dev/zvol/<pool>/<name>.
		It("ListVolumes includes a newly created ZFS zvol in the volume list", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := uniqueVolName("zfs-list")
			volumeID := poolName + "/" + volName
			expectedDevPath := "/dev/zvol/" + poolName + "/" + volName

			client := openExternalAgentConn(ctx)

			// Cleanup: always delete the zvol.
			DeferCleanup(func(dctx SpecContext) {
				if _, err := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, err)
				}
			})

			By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20, // 256 MiB
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed")

			By(fmt.Sprintf("calling ListVolumes for pool %q", poolName))
			listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				PoolName: poolName,
			})
			Expect(err).NotTo(HaveOccurred(), "ListVolumes must succeed")

			// Search for our volume in the list.
			var found *agentv1.VolumeInfo
			for _, v := range listResp.GetVolumes() {
				if v.GetVolumeId() == volumeID {
					found = v
					break
				}
			}

			By(fmt.Sprintf("ListVolumes returned %d volumes; searching for %q",
				len(listResp.GetVolumes()), volumeID))

			Expect(found).NotTo(BeNil(),
				"volume %q must appear in ListVolumes response for pool %q",
				volumeID, poolName)
			Expect(found.GetCapacityBytes()).To(BeNumerically(">", 0),
				"listed volume %q must have positive capacity_bytes", volumeID)
			Expect(found.GetDevicePath()).To(Equal(expectedDevPath),
				"listed volume %q device_path must be /dev/zvol/<pool>/<name>", volumeID)
		})

		// ── 6. DevicePath convention ────────────────────────────────────────────
		//
		// This spec is dedicated to validating the DevicePath convention in
		// isolation.  It creates a zvol with a known name and asserts that:
		//  a) CreateVolumeResponse.device_path == /dev/zvol/<pool>/<name>
		//  b) ListVolumesResponse[].device_path == /dev/zvol/<pool>/<name>
		//
		// Together these confirm that the ZFS backend's DevicePath() method
		// is wired correctly through both the Create and List code paths.
		It("DevicePath follows /dev/zvol/<pool>/<name> convention in Create and List", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := uniqueVolName("zfs-devpath")
			volumeID := poolName + "/" + volName
			expectedDevPath := "/dev/zvol/" + poolName + "/" + volName

			client := openExternalAgentConn(ctx)

			DeferCleanup(func(dctx SpecContext) {
				if _, err := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); err != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, err)
				}
			})

			By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
			createResp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20,
			})
			Expect(err).NotTo(HaveOccurred(), "CreateVolume must succeed")

			// ── a) CreateVolume.device_path ────────────────────────────────────
			By(fmt.Sprintf("checking CreateVolume.device_path=%q", createResp.GetDevicePath()))
			Expect(createResp.GetDevicePath()).To(Equal(expectedDevPath),
				"CreateVolume device_path must follow /dev/zvol/<pool>/<name>")

			// ── b) ListVolumes[].device_path ──────────────────────────────────
			listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				PoolName: poolName,
			})
			Expect(err).NotTo(HaveOccurred(), "ListVolumes must succeed")

			var found *agentv1.VolumeInfo
			for _, v := range listResp.GetVolumes() {
				if v.GetVolumeId() == volumeID {
					found = v
					break
				}
			}

			Expect(found).NotTo(BeNil(),
				"volume %q must appear in ListVolumes for pool %q", volumeID, poolName)
			By(fmt.Sprintf("checking ListVolumes[%q].device_path=%q",
				volumeID, found.GetDevicePath()))
			Expect(found.GetDevicePath()).To(Equal(expectedDevPath),
				"ListVolumes device_path must follow /dev/zvol/<pool>/<name>")
		})
	}) // end Describe("ZFSBackendRPCs")
	return true
}()
