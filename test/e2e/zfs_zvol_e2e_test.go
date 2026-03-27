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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ZFSZvolRealExport tests the ExportVolume RPC end-to-end by provisioning a
// real ZFS zvol via CreateVolume and then calling ExportVolume over NVMe-oF
// TCP.  The test verifies that the RPC succeeds and that the returned
// ExportInfo contains a non-empty TargetId — confirming that the agent
// successfully configured the NVMe-oF target in configfs.
//
// # Why NVMe-oF TCP?
//
// NVMe-oF TCP is the primary protocol implemented by pillar-agent and the one
// exercised by the full CSI integration path.  The test uses port 4421 (not the
// standard 4420) to avoid conflicts with any production target that may already
// be listening on the host.
//
// # Constraint: no nvme connect
//
// The test does NOT attempt to connect an initiator to the exported target.
// Connecting via `nvme connect` requires root on the Kind worker node, and the
// test process runs outside the cluster.  Verifying that ExportVolume returns
// a populated ExportInfo is sufficient to confirm end-to-end NVMe-oF target
// creation.
var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ZFSZvolRealExport", Ordered, func() {
		// ── BeforeAll: prerequisite guards ──────────────────────────────────────
		BeforeAll(func() {
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ZFS host-exec helper must be available — setupZFSPool() must have succeeded")
		})

		// ── It: ExportVolume exports a real ZFS zvol over NVMe-oF TCP ─────────
		//
		// Steps:
		//  1. Open a gRPC connection to the external agent.
		//  2. CreateVolume to provision the backing zvol.
		//  3. Call ExportVolume with NVMe-oF TCP params.
		//  4. Assert the call returns without error and ExportInfo.TargetId is set.
		//  5. DeferCleanup handles UnexportVolume + DeleteVolume in LIFO order.
		It("ExportVolume succeeds and returns populated ExportInfo", func(ctx SpecContext) {
			poolName := testEnv.ZFSPoolName
			volName := fmt.Sprintf("zvol-e2e-export-%d", time.Now().UnixNano()%1_000_000)
			volumeID := poolName + "/" + volName
			devPath := "/dev/zvol/" + poolName + "/" + volName

			By(fmt.Sprintf("connecting to external agent at %s", testEnv.ExternalAgentAddr))
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

			client := agentv1.NewAgentServiceClient(conn)

			// Register UnexportVolume + DeleteVolume cleanup BEFORE the
			// operations that create state, so that cleanup runs even when an
			// assertion below fails.  DeferCleanup handlers execute in LIFO
			// order: DeleteVolume fires last (after UnexportVolume) because it
			// is registered first.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting volume %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: unexporting volume %q", volumeID))
				if _, unexpErr := client.UnexportVolume(dctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     volumeID,
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				}); unexpErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup UnexportVolume %q: %v\n", volumeID, unexpErr)
				}
			})

			// ── Step 1: Create the backing zvol ───────────────────────────────
			By(fmt.Sprintf("calling CreateVolume for %q (256 MiB)", volumeID))
			createResp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 256 << 20, // 256 MiB
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume(%q) must succeed before ExportVolume can be called", volumeID)
			Expect(createResp.GetCapacityBytes()).To(BeNumerically(">", 0),
				"CreateVolume must return a positive allocated capacity")
			By(fmt.Sprintf("CreateVolume returned %d bytes allocated", createResp.GetCapacityBytes()))

			// ── Step 2: Wait for the zvol device node ────────────────────────
			//
			// ExportVolume needs the block device to exist before it can
			// configure the NVMe-oF target.  We poll the remote host for up to
			// 10 s to tolerate asynchronous udev processing.
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
				"zvol device %s must appear on the remote host before ExportVolume", devPath)
			By(fmt.Sprintf("confirmed: %s exists — proceeding with ExportVolume", devPath))

			// ── Step 3: ExportVolume over NVMe-oF TCP ─────────────────────────
			//
			// Port 4421 avoids conflicts with any production NVMe-oF target
			// that may be listening on the standard port 4420.
			//
			// AclEnabled=false means any initiator may connect; this avoids
			// needing to supply an initiator NQN for the basic export test.
			By(fmt.Sprintf("calling ExportVolume for %q over NVMe-oF TCP (port 4421)", volumeID))
			exportResp, err := client.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
				VolumeId:     volumeID,
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: &agentv1.ExportParams{
					Params: &agentv1.ExportParams_NvmeofTcp{
						NvmeofTcp: &agentv1.NvmeofTcpExportParams{
							BindAddress: "0.0.0.0",
							Port:        4421,
						},
					},
				},
				DevicePath: devPath,
				AclEnabled: false, // allow any initiator — no AllowInitiator call needed
			})
			Expect(err).NotTo(HaveOccurred(),
				"ExportVolume(%q, NVMe-oF TCP) must succeed — verify the agent "+
					"container has CAP_SYS_ADMIN and /sys/kernel/config is mounted "+
					"(configfs is required for NVMe-oF target configuration)",
				volumeID)

			// ── Step 4: Assert ExportInfo is populated ────────────────────────
			//
			// A non-nil ExportInfo with a non-empty TargetId confirms that the
			// agent successfully wrote the NVMe-oF subsystem entry into configfs
			// and returned the subsystem NQN as the target identifier.
			Expect(exportResp.GetExportInfo()).NotTo(BeNil(),
				"ExportVolume must return a non-nil ExportInfo")
			Expect(exportResp.GetExportInfo().GetTargetId()).NotTo(BeEmpty(),
				"ExportInfo.TargetId must be non-empty — it carries the NVMe-oF "+
					"subsystem NQN that the initiator uses to connect")

			By(fmt.Sprintf(
				"ExportVolume succeeded: targetId=%q address=%q port=%d",
				exportResp.GetExportInfo().GetTargetId(),
				exportResp.GetExportInfo().GetAddress(),
				exportResp.GetExportInfo().GetPort(),
			))

			// ── Step 5: Verify configfs entries ───────────────────────────────
			//
			// The agent is started with --configfs-root=/tmp so all NVMe-oF
			// configfs entries live under /tmp/nvmet/ inside the agent container
			// (not in the kernel's real /sys/kernel/config/nvmet/).
			//
			// We check three configfs entries to confirm the agent fully completed
			// the target setup:
			//
			//  a) Subsystem directory — the NQN returned in ExportInfo.TargetId.
			//  b) Namespace directory — namespace ID from ExportInfo.VolumeRef.
			//  c) Port directory     — the configfs port entry whose addr_trsvcid
			//                          matches the TCP port we requested (4421).
			//
			// configfs entries are written synchronously during ExportVolume, so
			// there is no need to poll: the entries must exist immediately after
			// the RPC returns successfully.

			nqn := exportResp.GetExportInfo().GetTargetId()
			tcpPort := exportResp.GetExportInfo().GetPort()
			volumeRef := exportResp.GetExportInfo().GetVolumeRef()

			// Parse the namespace ID from VolumeRef (a uint32 serialised as string).
			nsidU64, parseErr := strconv.ParseUint(volumeRef, 10, 32)
			Expect(parseErr).NotTo(HaveOccurred(),
				"ExportInfo.VolumeRef must be a numeric namespace ID string, got %q", volumeRef)
			nsid := uint32(nsidU64)

			// Compute the deterministic configfs port ID from the bind address
			// and TCP port that were passed in ExportVolumeRequest.
			portID := framework.StablePortID("0.0.0.0", tcpPort)

			agentContainer := externalAgentContainerName()
			agentCfg := framework.NewNVMeConfigfs(testEnv.DockerHost, agentContainer, "/tmp")

			// ── 5a: Subsystem exists ───────────────────────────────────────────
			By(fmt.Sprintf(
				"verifying configfs subsystem %q exists in agent container %q",
				nqn, agentContainer))
			subsysExists, subsysErr := agentCfg.SubsystemExists(ctx, nqn)
			Expect(subsysErr).NotTo(HaveOccurred(),
				"docker exec into agent container %q must succeed when checking subsystem dir",
				agentContainer)
			Expect(subsysExists).To(BeTrue(),
				"configfs subsystem directory %q must exist after ExportVolume — "+
					"check that the agent wrote to <configfsRoot>/nvmet/subsystems/<nqn>/",
				agentCfg.SubsystemPath(nqn))

			// ── 5b: Namespace exists ──────────────────────────────────────────
			By(fmt.Sprintf(
				"verifying configfs namespace nsid=%d for subsystem %q", nsid, nqn))
			nsExists, nsErr := agentCfg.NamespaceExists(ctx, nqn, nsid)
			Expect(nsErr).NotTo(HaveOccurred(),
				"docker exec into agent container %q must succeed when checking namespace dir",
				agentContainer)
			Expect(nsExists).To(BeTrue(),
				"configfs namespace directory %q must exist after ExportVolume",
				agentCfg.NamespacePath(nqn, nsid))

			// ── 5c: Port exists ───────────────────────────────────────────────
			By(fmt.Sprintf(
				"verifying configfs port entry portID=%d (tcp port %d) in agent container %q",
				portID, tcpPort, agentContainer))
			portExists, portErr := agentCfg.PortExists(ctx, portID)
			Expect(portErr).NotTo(HaveOccurred(),
				"docker exec into agent container %q must succeed when checking port dir",
				agentContainer)
			Expect(portExists).To(BeTrue(),
				"configfs port directory %q must exist after ExportVolume on TCP port %d — "+
					"expected portID derived via StablePortID(\"0.0.0.0\", %d) = %d",
				agentCfg.PortPath(portID), tcpPort, tcpPort, portID)

			By(fmt.Sprintf(
				"configfs verified: subsystem=%q namespace=%d portID=%d",
				nqn, nsid, portID))
		})
	}) // end Describe("ZFSZvolRealExport")
	return true
}()

var _ = func() bool {
	if !isExternalAgentMode() {
		return false
	}
	Describe("ZFSZvolRealCreate", Ordered, func() {
		// ── BeforeAll: prerequisite guards ──────────────────────────────────────
		//
		// Both the external agent and the ZFS host-exec helper must be available.
		// If either is missing, the entire Describe block is skipped so that the
		// suite can run in environments that lack a real ZFS kernel module or
		// in internal-agent mode.
		BeforeAll(func() {
			Expect(testEnv.zfsHostExec).NotTo(BeNil(),
				"ZFS host-exec helper must be available — setupZFSPool() must have succeeded")
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
	}) // end Describe("ZFSZvolRealCreate")
	return true
}()
