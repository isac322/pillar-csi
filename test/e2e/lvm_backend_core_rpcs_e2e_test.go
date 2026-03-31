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

// lvm_backend_core_rpcs_e2e_test.go — E2E tests for the 6 core LVM backend
// RPCs exercised against the in-cluster pillar-agent DaemonSet via
// kubectl port-forward.
//
// RPCs covered:
//   - CreateVolume  — creates an LVM LV and returns device_path + capacity_bytes
//     for both linear and thin provisioning modes.
//   - DeleteVolume  — destroys an LVM LV; idempotent on missing volume.
//   - ExpandVolume  — grows an LVM LV to at least the requested size.
//   - GetCapacity   — reports total/available bytes for the LVM Volume Group.
//   - ListVolumes   — enumerates all LVs in the VG with device_path.
//   - DevicePath    — verified via CreateVolumeResponse.device_path and
//     VolumeInfo.device_path in ListVolumes response.
//
// # Provisioning modes
//
// The agent is started with --backend=type=lvm-lv,vg=e2e-vg,thinpool=e2e-thin-pool
// making thin the backend default.  Linear mode is exercised by passing
// LvmVolumeParams.ProvisionMode="linear" in BackendParams, which overrides the
// backend default on a per-volume basis.
//
// # Prerequisites
//
//   - PILLAR_E2E_LVM_VG must be set (done by TestMain.setupLVMVG on success).
//   - The LVM VG (and thin pool e2e-thin-pool) must exist on the storage-worker node.
//   - The pillar-agent DaemonSet must be Running on the storage-worker node.
//   - testEnv.lvmHostExec must be non-nil (set by setupLVMVG).
//
// # Access mechanism
//
// The in-cluster agent pod exposes gRPC on port 9500 (containerPort).
// The tests use kubectl port-forward to create a tunnel from a local port
// (lvmRPCLocalPort, default 19500) to the agent pod's port 9500, then dial
// gRPC through that tunnel.  kubectl port-forward uses the Kubernetes API
// server as a relay, so it works regardless of whether Docker (and Kind) are
// local or remote.
//
// # Test isolation
//
// Each test case uses a unique volume name derived from the nanosecond timestamp
// to prevent collisions.  DeferCleanup registers DeleteVolume before the
// operation under test so volumes are removed even when an assertion fails.
//
// # Sequential execution
//
// ZFS and LVM tests share the same Kind cluster, storage node, and agent
// DaemonSet pod.  Running them in parallel could cause configfs/NVMe-oF port
// conflicts.  Ginkgo's default sequential execution within a single suite
// guarantees the two backend groups run one after the other.
package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// lvmRPCLocalPort is the local TCP port used by kubectl port-forward to relay
// gRPC calls to the in-cluster agent pod.  It is different from the external
// agent port (9500) to avoid conflicts when both modes are run sequentially.
const lvmRPCLocalPort = "19500"

// lvmRPCAgentAddr is the gRPC dial address for the port-forwarded agent.
const lvmRPCAgentAddr = "127.0.0.1:" + lvmRPCLocalPort

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// lvmVolName returns a unique LVM LV name for the given test tag.
// LV names must be valid LVM identifiers (letters, digits, underscores, hyphens).
func lvmVolName(tag string) string {
	return fmt.Sprintf("e2e-%s-%d", tag, time.Now().UnixNano()%1_000_000)
}

// findAgentPodOnStorageNode returns the name of the first pillar-agent pod
// scheduled on storageNode.  It runs kubectl to list pods with the agent's
// component selector labels.
//
// Returns an error when no pod is found or the kubectl call fails.
func findAgentPodOnStorageNode(storageNode, namespace string) (string, error) {
	out, err := captureOutput("kubectl", "get", "pods",
		"-n", namespace,
		"-l", "app.kubernetes.io/name=pillar-csi,app.kubernetes.io/component=agent",
		"--field-selector", "spec.nodeName="+storageNode+",status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", fmt.Errorf("kubectl get pods: %s: %w", strings.TrimSpace(out), err)
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no running pillar-agent pod found on node %q in namespace %q "+
			"(check that the DaemonSet is deployed and the node has label "+
			"pillar-csi.bhyoo.com/storage-node=true)", storageNode, namespace)
	}
	return name, nil
}

// startAgentPortForward starts a kubectl port-forward process that tunnels
// localPort on 127.0.0.1 to port 9500 on the named pod in namespace.
//
// It waits up to 30 seconds for the local port to accept TCP connections before
// returning.  The returned stop function kills the port-forward process; callers
// MUST call it (typically via DeferCleanup) to avoid leaking the process.
//
// The function fails the current spec immediately via Expect if the port-forward
// cannot be started or if the local port does not become reachable within the
// timeout.
func startAgentPortForward(podName, namespace, localPort string) (addr string, stop func()) {
	GinkgoHelper()

	addr = "127.0.0.1:" + localPort

	// Kill any existing process occupying the port (defensive cleanup from a
	// previous interrupted run).
	_ = exec.Command("fuser", "-k", localPort+"/tcp").Run() //nolint:gosec
	time.Sleep(200 * time.Millisecond)

	cmd := exec.CommandContext( //nolint:gosec
		context.Background(),
		"kubectl", "port-forward",
		"pod/"+podName,
		localPort+":9500",
		"--namespace", namespace,
	)
	cmd.Env = os.Environ()
	// Capture output to /dev/null — errors appear as a failed TCP connection
	// below, which gives a clearer test failure message.
	cmd.Stdout = nil
	cmd.Stderr = nil

	Expect(cmd.Start()).To(Succeed(),
		"kubectl port-forward pod/%s %s:9500 must start without error", podName, localPort)

	stop = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}

	// Wait for the port to accept TCP connections.
	By(fmt.Sprintf("waiting for kubectl port-forward to be ready on %s (up to 30 s)", addr))
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			stop()
			Fail(fmt.Sprintf("kubectl port-forward to pod/%s did not become ready within 30 s "+
				"(last error: %v)", podName, dialErr))
		}
		time.Sleep(300 * time.Millisecond)
	}
	By(fmt.Sprintf("kubectl port-forward ready: gRPC tunnel to pod/%s at %s", podName, addr))
	return addr, stop
}

// lvmDial opens a plaintext gRPC connection to the agent via the port-forwarded
// addr and returns the connection.  The caller is responsible for
// DeferCleanup(conn.Close).
func lvmDial(ctx context.Context, addr string) *grpc.ClientConn {
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
		"gRPC dial to in-cluster agent via port-forward at %s must succeed", addr)
	return conn
}

// lvmLVExists verifies that an LV named lvName exists in vgName on the Docker
// host by running `lvs <vg>/<lv>` through testEnv.lvmHostExec.
// Returns nil when the LV exists, a descriptive error otherwise.
func lvmLVExists(ctx context.Context, vgName, lvName string) error {
	if testEnv.lvmHostExec == nil {
		return fmt.Errorf("lvmHostExec is nil — LVM host-exec helper was not initialised by setupLVMVG")
	}
	res, err := testEnv.lvmHostExec.ExecOnHost(ctx,
		fmt.Sprintf("lvs --noheadings -o lv_name %s/%s 2>/dev/null", vgName, lvName))
	if err != nil {
		return fmt.Errorf("lvmHostExec: %w", err)
	}
	if !res.Success() {
		return fmt.Errorf("LV %s/%s does not exist on Docker host (lvs exit %d stderr=%q)",
			vgName, lvName, res.ExitCode, res.Stderr)
	}
	return nil
}

// lvmLVAbsent verifies that an LV named lvName does NOT exist in vgName on the
// Docker host.  Returns nil when the LV is absent, a descriptive error if it
// still exists.
func lvmLVAbsent(ctx context.Context, vgName, lvName string) error {
	if testEnv.lvmHostExec == nil {
		return fmt.Errorf("lvmHostExec is nil")
	}
	res, err := testEnv.lvmHostExec.ExecOnHost(ctx,
		fmt.Sprintf("lvs --noheadings -o lv_name %s/%s 2>/dev/null", vgName, lvName))
	if err != nil {
		return fmt.Errorf("lvmHostExec: %w", err)
	}
	if res.Success() {
		return fmt.Errorf("LV %s/%s still exists on Docker host after DeleteVolume", vgName, lvName)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// LVMBackendCoreRPCs — all 6 core backend RPCs against a real LVM VG
// ─────────────────────────────────────────────────────────────────────────────

var _ = func() bool {
	if isExternalAgentMode() {
		// LVM RPC tests only run in internal-agent mode (DaemonSet with LVM backend).
		return false
	}
	Describe("LVMBackendCoreRPCs", Ordered, Label("internal-agent", "lvm", "rpc"), func() {

		// ── Suite-level state ──────────────────────────────────────────────────
		var (
			agentAddr   string // 127.0.0.1:19500 via port-forward
			vgName      string // PILLAR_E2E_LVM_VG (e.g. "e2e-vg")
			thinPool    string // PILLAR_E2E_LVM_THIN_POOL (e.g. "e2e-thin-pool"); "" = linear default
			stopForward func() // kills the kubectl port-forward process
		)

		// ── BeforeAll: prerequisites + port-forward ────────────────────────────
		BeforeAll(func(ctx context.Context) {
			vgName = lvmVGName()
			if vgName == "" {
				Skip("PILLAR_E2E_LVM_VG not set — skipping LVM backend core RPC tests " +
					"(set to the LVM Volume Group name on the storage-worker node, e.g. 'e2e-vg')")
			}
			thinPool = lvmThinPoolName()

			By(fmt.Sprintf("LVM VG: %q  thin pool: %q", vgName, thinPool))

			Expect(testEnv.lvmHostExec).NotTo(BeNil(),
				"testEnv.lvmHostExec must be non-nil — setupLVMVG() must have succeeded")

			// Resolve the storage-worker node name.
			// Prefer the env var (set by ensureStorageNodeLabel) since the
			// controller may remove the label between test groups.
			storageNode := os.Getenv("PILLAR_E2E_STORAGE_NODE")
			if storageNode == "" {
				out, err := captureOutput("kubectl", "get", "nodes",
					"-l", "pillar-csi.bhyoo.com/storage-node=true",
					"-o", "jsonpath={.items[0].metadata.name}")
				Expect(err).NotTo(HaveOccurred(),
					"find storage worker node: %s", strings.TrimSpace(out))
				storageNode = strings.TrimSpace(out)
			}
			Expect(storageNode).NotTo(BeEmpty(), "must find a storage-worker node")
			By(fmt.Sprintf("storage-worker node: %s", storageNode))

			// Re-apply the storage-node label (the controller removes it when
			// PillarTargets from earlier test groups are deleted).
			_ = runCmd("kubectl", "label", "node", storageNode,
				"pillar-csi.bhyoo.com/storage-node=true", "--overwrite")

			// Wait for the agent pod to be scheduled and running.
			var podName string
			Eventually(func() error {
				var err error
				podName, err = findAgentPodOnStorageNode(storageNode, testEnv.HelmNamespace)
				return err
			}, 60*time.Second, 2*time.Second).Should(Succeed(),
				"must find a running pillar-agent pod on the storage-worker node %q "+
					"(the DaemonSet may need time to reschedule after re-labelling)", storageNode)
			By(fmt.Sprintf("agent pod: %s", podName))

			// Start kubectl port-forward to the agent pod.
			agentAddr, stopForward = startAgentPortForward(podName, testEnv.HelmNamespace, lvmRPCLocalPort)

			// DeferCleanup in BeforeAll fires after the last spec in this Ordered
			// block (or after AfterAll if one is defined).
			DeferCleanup(func() {
				By("stopping kubectl port-forward for LVM agent")
				if stopForward != nil {
					stopForward()
				}
			})
		})

		// ── GetCapacity: real LVM VG returns positive capacity values ──────────
		It("GetCapacity returns positive total and available bytes for the LVM VG", func(ctx SpecContext) {
			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			By(fmt.Sprintf("calling GetCapacity for LVM VG %q", vgName))
			resp, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vgName,
			})
			Expect(err).NotTo(HaveOccurred(),
				"GetCapacity for LVM VG %q must succeed", vgName)

			Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
				"GetCapacity.TotalBytes must be > 0 — the VG exists and has physical storage")
			Expect(resp.GetAvailableBytes()).To(BeNumerically(">=", 0),
				"GetCapacity.AvailableBytes must be >= 0")
			Expect(resp.GetTotalBytes()).To(BeNumerically(">=", resp.GetAvailableBytes()),
				"total bytes must be >= available bytes")

			By(fmt.Sprintf("GetCapacity: total=%d available=%d",
				resp.GetTotalBytes(), resp.GetAvailableBytes()))
		})

		// ── CreateVolume (thin): creates a thin LV and returns correct device_path
		It("CreateVolume (thin) returns device_path=/dev/<vg>/<lv> for a thin LV", func(ctx SpecContext) {
			if thinPool == "" {
				Skip("PILLAR_E2E_LVM_THIN_POOL not set — skipping thin-provisioning CreateVolume test " +
					"(set to the thin pool name to enable thin-mode tests)")
			}

			lvName := lvmVolName("thin")
			volumeID := vgName + "/" + lvName
			expectedDevPath := "/dev/" + vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before the create so the LV is removed even on failure.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting thin LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			By(fmt.Sprintf("calling CreateVolume (thin default) for %q (128 MiB)", volumeID))
			resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 128 << 20, // 128 MiB
				// No BackendParams.ProvisionMode override — use backend default (thin)
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume(%q) thin must succeed", volumeID)
			Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", 128<<20),
				"allocated capacity must be at least the requested 128 MiB")

			// ── DevicePath: verify device_path in CreateVolumeResponse ───────────
			By("verifying device_path in CreateVolumeResponse (DevicePath RPC — thin LV)")
			Expect(resp.GetDevicePath()).To(Equal(expectedDevPath),
				"device_path must be /dev/<vg>/<lv> for a thin LV")

			// Verify the LV was actually created on the Docker host.
			By(fmt.Sprintf("verifying LV %s/%s exists on Docker host via lvs", vgName, lvName))
			Expect(lvmLVExists(ctx, vgName, lvName)).To(Succeed(),
				"thin LV must exist on the Docker host after CreateVolume")

			By(fmt.Sprintf("CreateVolume (thin): device_path=%q capacity=%d",
				resp.GetDevicePath(), resp.GetCapacityBytes()))
		})

		// ── CreateVolume (linear): creates a linear LV via ProvisionMode override
		It("CreateVolume (linear) creates a linear LV using ProvisionMode override", func(ctx SpecContext) {
			lvName := lvmVolName("lin")
			volumeID := vgName + "/" + lvName
			expectedDevPath := "/dev/" + vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before the create.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting linear LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			By(fmt.Sprintf("calling CreateVolume (linear override) for %q (128 MiB)", volumeID))
			resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 128 << 20, // 128 MiB
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume(%q) linear must succeed", volumeID)
			Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", 128<<20),
				"linear LV capacity must be at least the requested 128 MiB")

			// ── DevicePath: verify device_path for linear LV ──────────────────────
			By("verifying device_path in CreateVolumeResponse (DevicePath RPC — linear LV)")
			Expect(resp.GetDevicePath()).To(Equal(expectedDevPath),
				"device_path must be /dev/<vg>/<lv> for a linear LV (same convention as thin)")

			// Verify the LV was actually created on the Docker host.
			By(fmt.Sprintf("verifying LV %s/%s exists on Docker host via lvs", vgName, lvName))
			Expect(lvmLVExists(ctx, vgName, lvName)).To(Succeed(),
				"linear LV must exist on the Docker host after CreateVolume")

			By(fmt.Sprintf("CreateVolume (linear): device_path=%q capacity=%d",
				resp.GetDevicePath(), resp.GetCapacityBytes()))
		})

		// ── DeleteVolume: destroys an LV; idempotent on missing volume ─────────
		It("DeleteVolume destroys an LV and is idempotent on a non-existent LV", func(ctx SpecContext) {
			lvName := lvmVolName("del")
			volumeID := vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Step 1: Create an LV so there is something to delete.
			// Use linear mode to avoid depending on thin pool availability.
			By(fmt.Sprintf("creating LV %q (linear) before testing DeleteVolume", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 128 << 20,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume must succeed before DeleteVolume test")

			// Step 2: Verify the LV exists on the Docker host.
			By(fmt.Sprintf("verifying LV %s/%s exists on Docker host", vgName, lvName))
			Expect(lvmLVExists(ctx, vgName, lvName)).To(Succeed(),
				"LV must exist before DeleteVolume")

			// Step 3: Delete the LV.
			By(fmt.Sprintf("calling DeleteVolume for %q", volumeID))
			_, err = client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			Expect(err).NotTo(HaveOccurred(),
				"DeleteVolume(%q) must succeed", volumeID)

			// Step 4: Verify the LV is gone on the Docker host.
			By(fmt.Sprintf("verifying LV %s/%s is removed on Docker host after DeleteVolume", vgName, lvName))
			Eventually(func() error {
				return lvmLVAbsent(ctx, vgName, lvName)
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed(),
				"LV must disappear from the Docker host after DeleteVolume")

			// Step 5: Idempotency — deleting again must succeed.
			By(fmt.Sprintf("calling DeleteVolume again for %q (idempotency check)", volumeID))
			_, err = client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			Expect(err).NotTo(HaveOccurred(),
				"DeleteVolume on a non-existent LV must return success (idempotent)")

			By("DeleteVolume idempotency confirmed")
		})

		// ── ExpandVolume: grows an LV to at least the requested size ───────────
		It("ExpandVolume grows an LVM LV to at least the requested size", func(ctx SpecContext) {
			lvName := lvmVolName("exp")
			volumeID := vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before creating the LV.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			const initialBytes = 128 << 20  // 128 MiB
			const expandedBytes = 256 << 20 // 256 MiB

			// Step 1: Create the LV at initial size (linear so no thin-pool dependency).
			By(fmt.Sprintf("creating LV %q at %d bytes (128 MiB, linear)", volumeID, initialBytes))
			createResp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: initialBytes,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume must succeed before ExpandVolume test")
			Expect(createResp.GetCapacityBytes()).To(BeNumerically(">=", initialBytes),
				"initial allocated capacity must be at least the requested size")

			// Step 2: Expand to double the initial size.
			By(fmt.Sprintf("calling ExpandVolume for %q to %d bytes (256 MiB)", volumeID, expandedBytes))
			expandResp, err := client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
				VolumeId:       volumeID,
				RequestedBytes: expandedBytes,
			})
			Expect(err).NotTo(HaveOccurred(),
				"ExpandVolume(%q, %d) must succeed", volumeID, expandedBytes)
			Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", expandedBytes),
				"ExpandVolume must allocate at least the requested 256 MiB")

			By(fmt.Sprintf("ExpandVolume: new capacity=%d bytes (%.1f MiB)",
				expandResp.GetCapacityBytes(), float64(expandResp.GetCapacityBytes())/(1<<20)))
		})

		// ── ListVolumes + DevicePath: enumerates LVs including device_path ──────
		It("ListVolumes returns created LVs with correct device_path", func(ctx SpecContext) {
			lvName := lvmVolName("list")
			volumeID := vgName + "/" + lvName
			expectedDevPath := "/dev/" + vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			// Register cleanup before creating.
			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			// Step 1: Create a volume so there is something to list.
			By(fmt.Sprintf("creating LV %q (linear) before testing ListVolumes", volumeID))
			_, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: 128 << 20,
				BackendParams: &agentv1.BackendParams{
					Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{
							ProvisionMode: "linear",
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred(),
				"CreateVolume must succeed before ListVolumes test")

			// Step 2: List volumes and verify the new LV appears.
			By(fmt.Sprintf("calling ListVolumes for VG %q", vgName))
			listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    vgName,
			})
			Expect(err).NotTo(HaveOccurred(),
				"ListVolumes for VG %q must succeed", vgName)

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
				volumeID, len(listResp.GetVolumes()), lvmVolumeIDs(listResp.GetVolumes()))

			// ── DevicePath in ListVolumes ─────────────────────────────────────────
			//
			// VolumeInfo.device_path is populated by the LVM backend's DevicePath
			// method, which constructs /dev/<vg>/<lv-name> as a pure string
			// derivation from the volume ID (no exec calls).
			Expect(found.GetDevicePath()).To(Equal(expectedDevPath),
				"VolumeInfo.device_path must be /dev/<vg>/<lv-name> for LVM LVs")
			Expect(found.GetCapacityBytes()).To(BeNumerically(">", 0),
				"VolumeInfo.capacity_bytes must be positive")

			By(fmt.Sprintf("ListVolumes: found volume %q device_path=%q capacity=%d",
				found.GetVolumeId(), found.GetDevicePath(), found.GetCapacityBytes()))
		})

		// ── CreateVolume idempotency: re-create with same params succeeds ───────
		It("CreateVolume is idempotent: re-creating with same volume ID succeeds (linear)", func(ctx SpecContext) {
			lvName := lvmVolName("idem")
			volumeID := vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			const reqBytes = 128 << 20 // 128 MiB
			linearParams := &agentv1.BackendParams{
				Params: &agentv1.BackendParams_Lvm{
					Lvm: &agentv1.LvmVolumeParams{
						ProvisionMode: "linear",
					},
				},
			}

			By(fmt.Sprintf("first CreateVolume (linear) for %q (%d bytes)", volumeID, reqBytes))
			resp1, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
				BackendParams: linearParams,
			})
			Expect(err).NotTo(HaveOccurred(), "first CreateVolume must succeed")

			By(fmt.Sprintf("second CreateVolume (linear) for %q (idempotency check)", volumeID))
			resp2, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
				BackendParams: linearParams,
			})
			Expect(err).NotTo(HaveOccurred(),
				"second CreateVolume with same ID and capacity must succeed (idempotent)")
			Expect(resp2.GetDevicePath()).To(Equal(resp1.GetDevicePath()),
				"device_path must be the same on idempotent re-create")
			Expect(resp2.GetCapacityBytes()).To(Equal(resp1.GetCapacityBytes()),
				"capacity_bytes must be the same on idempotent re-create")

			By("CreateVolume (linear) idempotency confirmed")
		})

		// ── CreateVolume idempotency (thin): re-create with same thin params ────
		It("CreateVolume is idempotent: re-creating with same volume ID succeeds (thin)", func(ctx SpecContext) {
			if thinPool == "" {
				Skip("PILLAR_E2E_LVM_THIN_POOL not set — skipping thin idempotency check")
			}

			lvName := lvmVolName("idth")
			volumeID := vgName + "/" + lvName

			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)

			DeferCleanup(func(dctx SpecContext) {
				By(fmt.Sprintf("cleanup: deleting thin LV %q", volumeID))
				if _, delErr := client.DeleteVolume(dctx, &agentv1.DeleteVolumeRequest{
					VolumeId: volumeID,
				}); delErr != nil {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"warning: cleanup DeleteVolume %q: %v\n", volumeID, delErr)
				}
			})

			const reqBytes = 128 << 20 // 128 MiB

			By(fmt.Sprintf("first CreateVolume (thin default) for %q (%d bytes)", volumeID, reqBytes))
			resp1, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
				// No ProvisionMode override — use backend default (thin)
			})
			Expect(err).NotTo(HaveOccurred(), "first CreateVolume (thin) must succeed")

			By(fmt.Sprintf("second CreateVolume (thin default) for %q (idempotency check)", volumeID))
			resp2, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      volumeID,
				CapacityBytes: reqBytes,
			})
			Expect(err).NotTo(HaveOccurred(),
				"second CreateVolume (thin) with same ID and capacity must succeed (idempotent)")
			Expect(resp2.GetDevicePath()).To(Equal(resp1.GetDevicePath()),
				"device_path must be the same on idempotent thin re-create")
			Expect(resp2.GetCapacityBytes()).To(Equal(resp1.GetCapacityBytes()),
				"capacity_bytes must be the same on idempotent thin re-create")

			By("CreateVolume (thin) idempotency confirmed")
		})

	}) // end Describe("LVMBackendCoreRPCs")
	return true
}()

// lvmVolumeIDs extracts volume IDs from a slice of VolumeInfo for use in error
// messages.
func lvmVolumeIDs(vols []*agentv1.VolumeInfo) []string {
	ids := make([]string, 0, len(vols))
	for _, v := range vols {
		ids = append(ids, v.GetVolumeId())
	}
	return ids
}

// ─────────────────────────────────────────────────────────────────────────────
// LVMGetCapacityVGNotFound — GetCapacity with a non-existent VG
// ─────────────────────────────────────────────────────────────────────────────
//
// This Describe block verifies that GetCapacity returns an error for a VG name
// that is not registered with the agent.  It runs even without an LVM VG
// set up (the agent always has a "no backend registered" code path).
//
// Gate: internal-agent mode only (external-agent mode has its own error-path tests).

var _ = func() bool {
	if isExternalAgentMode() {
		return false
	}
	Describe("LVMGetCapacityVGNotFound", Ordered, Label("internal-agent", "lvm"), func() {
		var (
			agentAddr   string
			stopForward func()
		)

		BeforeAll(func(ctx context.Context) {
			if lvmVGName() == "" {
				Skip("PILLAR_E2E_LVM_VG not set — skipping LVM GetCapacity error-path test " +
					"(requires a running agent with LVM backend registered)")
			}

			// Resolve the storage-worker node name.
			storageNode := os.Getenv("PILLAR_E2E_STORAGE_NODE")
			if storageNode == "" {
				out, err := captureOutput("kubectl", "get", "nodes",
					"-l", "pillar-csi.bhyoo.com/storage-node=true",
					"-o", "jsonpath={.items[0].metadata.name}")
				Expect(err).NotTo(HaveOccurred(),
					"find storage worker node: %s", strings.TrimSpace(out))
				storageNode = strings.TrimSpace(out)
			}
			Expect(storageNode).NotTo(BeEmpty(), "must find a storage-worker node")

			// Re-apply storage-node label and wait for agent pod.
			_ = runCmd("kubectl", "label", "node", storageNode,
				"pillar-csi.bhyoo.com/storage-node=true", "--overwrite")

			var podName string
			Eventually(func() error {
				var err error
				podName, err = findAgentPodOnStorageNode(storageNode, testEnv.HelmNamespace)
				return err
			}, 60*time.Second, 2*time.Second).Should(Succeed(),
				"must find a running pillar-agent pod on the storage-worker node %q", storageNode)
			Expect(podName).NotTo(BeEmpty(),
				"must find running agent pod on storage-worker node %q", storageNode)

			// Use a different local port from LVMBackendCoreRPCs to avoid collisions.
			const notFoundTestPort = "19501"
			agentAddr, stopForward = startAgentPortForward(podName, testEnv.HelmNamespace, notFoundTestPort)

			DeferCleanup(func() {
				if stopForward != nil {
					stopForward()
				}
			})
		})

		It("returns an error for a non-existent LVM VG pool name", func(ctx SpecContext) {
			conn := lvmDial(ctx, agentAddr)
			DeferCleanup(conn.Close)

			client := agentv1.NewAgentServiceClient(conn)
			_, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
				BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
				PoolName:    "does-not-exist-vg-" + strings.Repeat("x", 8),
			})
			Expect(err).To(HaveOccurred(),
				"GetCapacity for a non-existent VG must return an error "+
					"(agent has no backend registered for that pool name)")
			By("GetCapacity correctly returned an error for the unknown VG")
		})
	})
	return true
}()
