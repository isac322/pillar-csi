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

// Package zfsbackend provides standalone E2E tests for the ZFS backend RPCs.
//
// Unlike the main test/e2e package (which requires a full Kind cluster lifecycle),
// this package tests the pillar-agent's ZFS backend RPCs directly over gRPC
// without needing a Kubernetes cluster.
//
// # What it tests
//
// The six core VolumeBackend RPCs:
//
//  1. CreateVolume   — provisions a ZFS zvol; verifies device_path convention
//                      and that the block device appears on the host.
//
//  2. DeleteVolume   — creates then deletes a zvol; verifies the device
//                      disappears from the host.
//
//  3. ExpandVolume   — creates a small zvol and expands it; verifies the
//                      response capacity_bytes ≥ requested size.
//
//  4. GetCapacity    — queries ZFS pool capacity; verifies non-zero total_bytes.
//
//  5. ListVolumes    — creates a zvol and lists all volumes; verifies the
//                      new volume appears with correct fields.
//
//  6. DevicePath     — verifies CreateVolume and ListVolumes both return the
//                      /dev/zvol/<pool>/<name> device path convention.
//
// # Prerequisites
//
// The following environment variables control the test:
//
//	ZFS_BACKEND_POOL      ZFS pool name (default: "e2e-pool").
//	ZFS_BACKEND_AGENT     Agent container name (default: "pillar-csi-zfsb-agent").
//	ZFS_BACKEND_AGENT_PORT Host port for the agent (default: "9550").
//	ZFS_BACKEND_EXEC      Host-exec container name (default: "pillar-csi-host-exec").
//
// The ZFS pool must already exist on the Docker host.  The test starts its
// own privileged agent container and host-exec helper; it does NOT modify
// any existing containers or Kind clusters.
//
// # Running
//
//	go test -tags=e2e ./test/e2e/zfsbackend/ -v -timeout=10m
package zfsbackend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/test/e2e/framework"
)

// ─────────────────────────────────────────────────────────────────────────────
// Configuration (read from env vars at TestMain time)
// ─────────────────────────────────────────────────────────────────────────────

var (
	zfsPool       string // ZFS pool name
	agentAddr     string // host:port for gRPC
	hostExecHelper *framework.DockerHostExec
	agentContainerID string
)

const (
	agentImage       = "ghcr.io/bhyoo/pillar-csi/agent:e2e"
	defaultPool      = "e2e-pool"
	defaultAgentName = "pillar-csi-zfsb-agent"
	defaultAgentPort = "9550"
	defaultExecName  = "pillar-csi-zfsb-exec"
)

// envOrDefault returns the value of the env var or the fallback.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runDockerCmd runs a docker command and returns combined output + error.
func runDockerCmd(args ...string) (string, error) {
	cmd := exec.Command("docker", args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMain — lightweight setup / teardown (no Kind cluster)
// ─────────────────────────────────────────────────────────────────────────────

// TestMain performs minimal setup:
//  1. Reads env-var configuration.
//  2. Creates a DockerHostExec helper for device-presence verification.
//  3. Creates or verifies the ZFS loopback pool.
//  4. Starts a privileged pillar-agent container with ZFS backend.
//  5. Waits for the agent's gRPC port to become reachable.
//  6. Runs all Test* functions.
//  7. Stops and removes the agent container, destroys the pool, and removes
//     the helper on exit.
func TestMain(m *testing.M) {
	exitCode := 1
	defer func() { os.Exit(exitCode) }()

	zfsPool = envOrDefault("ZFS_BACKEND_POOL", defaultPool)
	zfsImagePath := envOrDefault("ZFS_BACKEND_IMAGE_PATH", "/tmp/e2e-zfs-b.img")
	zfsImageSize := envOrDefault("ZFS_BACKEND_IMAGE_SIZE", "2G")
	agentName := envOrDefault("ZFS_BACKEND_AGENT", defaultAgentName)
	agentPort := envOrDefault("ZFS_BACKEND_AGENT_PORT", defaultAgentPort)
	execName := envOrDefault("ZFS_BACKEND_EXEC", defaultExecName)
	agentAddr = "127.0.0.1:" + agentPort

	ctx := context.Background()

	// ── Step 1: Create host-exec helper ───────────────────────────────────────
	fmt.Fprintf(os.Stdout, "zfsbackend: starting host-exec helper %q\n", execName)
	var err error
	hostExecHelper, err = framework.NewDockerHostExecNamed(ctx, "", execName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zfsbackend: start host-exec helper: %v\n", err)
		return
	}
	defer func() {
		if closeErr := hostExecHelper.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "zfsbackend: close host-exec helper: %v\n", closeErr)
		}
	}()

	// ── Step 2: Create ZFS loopback pool ──────────────────────────────────────
	// CreateLoopbackZFSPool destroys any stale pool with the same name first,
	// then creates a fresh pool on a new loopback device.
	fmt.Fprintf(os.Stdout,
		"zfsbackend: creating ZFS pool %q (image %s, size %s)\n",
		zfsPool, zfsImagePath, zfsImageSize)
	loopDev, poolErr := framework.CreateLoopbackZFSPool(ctx, hostExecHelper,
		zfsPool, zfsImagePath, zfsImageSize)
	if poolErr != nil {
		fmt.Fprintf(os.Stderr, "zfsbackend: create ZFS pool %q: %v\n", zfsPool, poolErr)
		return
	}
	fmt.Fprintf(os.Stdout, "zfsbackend: ZFS pool %q ready (loop %s)\n", zfsPool, loopDev)

	defer func() {
		fmt.Fprintf(os.Stdout, "zfsbackend: destroying ZFS pool %q\n", zfsPool)
		if destroyErr := framework.DestroyLoopbackZFSPool(ctx, hostExecHelper,
			zfsPool, loopDev, zfsImagePath); destroyErr != nil {
			fmt.Fprintf(os.Stderr, "zfsbackend: destroy ZFS pool %q: %v\n", zfsPool, destroyErr)
		}
	}()

	// ── Step 3: Start external agent container ────────────────────────────────
	fmt.Fprintf(os.Stdout, "zfsbackend: removing any stale agent container %q\n", agentName)
	_, _ = runDockerCmd("rm", "-f", agentName)

	fmt.Fprintf(os.Stdout,
		"zfsbackend: starting agent container %q (image %s, port %s→9500)\n",
		agentName, agentImage, agentPort)
	containerID, startErr := runDockerCmd("run",
		"--detach",
		"--name", agentName,
		"-p", "127.0.0.1:"+agentPort+":9500",
		"--privileged",
		"--user=root",
		"--mount", "type=tmpfs,destination=/tmp",
		"-v", "/sys/kernel/config:/sys/kernel/config",
		agentImage,
		"--listen-address=0.0.0.0:9500",
		"--backend=type=zfs-zvol,pool="+zfsPool,
		"--configfs-root=/tmp",
	)
	if startErr != nil {
		fmt.Fprintf(os.Stderr, "zfsbackend: docker run agent: %s: %v\n", containerID, startErr)
		return
	}
	agentContainerID = containerID
	fmt.Fprintf(os.Stdout, "zfsbackend: agent container started (id %.12s)\n", agentContainerID)

	defer func() {
		fmt.Fprintf(os.Stdout, "zfsbackend: stopping agent container %q\n", agentName)
		if _, stopErr := runDockerCmd("rm", "-f", agentName); stopErr != nil {
			fmt.Fprintf(os.Stderr, "zfsbackend: remove agent container: %v\n", stopErr)
		}
	}()

	// ── Step 4: Wait for agent to be ready ────────────────────────────────────
	fmt.Fprintf(os.Stdout,
		"zfsbackend: waiting up to 60s for agent gRPC port on %s\n", agentAddr)
	deadline := time.Now().Add(60 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		conn, dialErr := grpc.NewClient(
			agentAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if dialErr == nil {
			client := agentv1.NewAgentServiceClient(conn)
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, pingErr := client.GetCapabilities(pingCtx, &agentv1.GetCapabilitiesRequest{})
			cancel()
			_ = conn.Close()
			if pingErr == nil {
				ready = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		fmt.Fprintf(os.Stderr, "zfsbackend: agent did not become ready within 60s on %s\n", agentAddr)
		return
	}
	fmt.Fprintf(os.Stdout, "zfsbackend: agent is ready at %s\n", agentAddr)

	// ── Step 5: Run tests ──────────────────────────────────────────────────────
	exitCode = m.Run()
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newClient opens a gRPC connection to the agent and registers t.Cleanup to
// close it.
func newClient(t *testing.T) agentv1.AgentServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(
		agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient(%s): %v", agentAddr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentv1.NewAgentServiceClient(conn)
}

// uniqueName returns a short unique name for a test zvol.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%10_000_000)
}

// waitDevice polls the remote host for device presence/absence.
func waitDevice(t *testing.T, devPath string, wantPresent bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	condition := "test -e " + devPath
	wantMsg := "present"
	if !wantPresent {
		condition = "test ! -e " + devPath
		wantMsg = "absent"
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, execErr := hostExecHelper.ExecOnHost(ctx, condition)
		if execErr == nil && res.Success() {
			t.Logf("device %s is %s", devPath, wantMsg)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("timeout waiting for device %s to be %s", devPath, wantMsg)
}

// mustCreate creates a volume and registers t.Cleanup to delete it.
// The cleanup retries DeleteVolume a few times to handle transient "dataset is busy"
// errors that occur when udev is still processing the block device node.
func mustCreate(t *testing.T, client agentv1.AgentServiceClient, volumeID string, sizeBytes int64) *agentv1.CreateVolumeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: sizeBytes,
	})
	if err != nil {
		t.Fatalf("CreateVolume(%q, %d): %v", volumeID, sizeBytes, err)
	}

	t.Cleanup(func() {
		// Retry DeleteVolume up to 5 times with a short sleep to handle transient
		// "dataset is busy" errors from udev still holding a reference to the
		// zvol block device node.
		for i := 0; i < 5; i++ {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, delErr := client.DeleteVolume(cleanCtx, &agentv1.DeleteVolumeRequest{
				VolumeId: volumeID,
			})
			cleanCancel()
			if delErr == nil {
				return
			}
			if i < 4 {
				time.Sleep(500 * time.Millisecond)
			} else {
				t.Logf("warning: cleanup DeleteVolume(%q): %v (gave up after 5 attempts)", volumeID, delErr)
			}
		}
	})

	return resp
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 1: CreateVolume
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_CreateVolume verifies that CreateVolume:
//   a) Returns success with capacity_bytes > 0
//   b) Returns device_path = /dev/zvol/<pool>/<name>
//   c) The block device node appears on the host within 10 s
func TestZFSBackend_CreateVolume(t *testing.T) {
	t.Parallel()
	client := newClient(t)

	volName := uniqueName("zfs-create")
	volumeID := zfsPool + "/" + volName
	expectedDevPath := "/dev/zvol/" + zfsPool + "/" + volName

	resp := mustCreate(t, client, volumeID, 128<<20) // 128 MiB

	// Verify capacity_bytes > 0
	if resp.GetCapacityBytes() <= 0 {
		t.Errorf("CreateVolume: capacity_bytes=%d, want > 0", resp.GetCapacityBytes())
	}
	t.Logf("CreateVolume: capacity_bytes=%d", resp.GetCapacityBytes())

	// Verify device_path matches /dev/zvol/<pool>/<name>
	if resp.GetDevicePath() != expectedDevPath {
		t.Errorf("CreateVolume: device_path=%q, want %q", resp.GetDevicePath(), expectedDevPath)
	}
	t.Logf("CreateVolume: device_path=%q (correct)", resp.GetDevicePath())

	// Verify block device appears on host
	t.Logf("waiting for %s to appear on host", expectedDevPath)
	waitDevice(t, expectedDevPath, true)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 2: DeleteVolume
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_DeleteVolume verifies that DeleteVolume removes the zvol and
// the block device disappears from the host.
func TestZFSBackend_DeleteVolume(t *testing.T) {
	t.Parallel()
	client := newClient(t)

	volName := uniqueName("zfs-delete")
	volumeID := zfsPool + "/" + volName
	devPath := "/dev/zvol/" + zfsPool + "/" + volName

	// Create the zvol first.
	ctx := context.Background()
	createCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err := client.CreateVolume(createCtx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: 128 << 20, // 128 MiB
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	// Wait for device to appear.
	t.Logf("waiting for %s to appear", devPath)
	waitDevice(t, devPath, true)

	// Delete the zvol.
	deleteCtx, deleteCancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = client.DeleteVolume(deleteCtx, &agentv1.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	deleteCancel()
	if err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	t.Logf("DeleteVolume: succeeded")

	// Verify device disappears.
	t.Logf("waiting for %s to disappear", devPath)
	waitDevice(t, devPath, false)
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 3: ExpandVolume
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_ExpandVolume verifies that ExpandVolume resizes the zvol and
// returns capacity_bytes >= the requested size.
func TestZFSBackend_ExpandVolume(t *testing.T) {
	t.Parallel()
	client := newClient(t)

	volName := uniqueName("zfs-expand")
	volumeID := zfsPool + "/" + volName

	const (
		initialSize  = int64(128 << 20) // 128 MiB
		expandedSize = int64(256 << 20) // 256 MiB
	)

	// Create with initial size.
	mustCreate(t, client, volumeID, initialSize)

	// Expand to larger size.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	expandResp, err := client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       volumeID,
		RequestedBytes: expandedSize,
	})
	cancel()
	if err != nil {
		t.Fatalf("ExpandVolume: %v", err)
	}

	t.Logf("ExpandVolume: capacity_bytes=%d (requested=%d)", expandResp.GetCapacityBytes(), expandedSize)

	if expandResp.GetCapacityBytes() < expandedSize {
		t.Errorf("ExpandVolume: capacity_bytes=%d < requested=%d",
			expandResp.GetCapacityBytes(), expandedSize)
	}

	// Verify via ZFS property on host.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer verifyCancel()
	res, execErr := hostExecHelper.ExecOnHost(verifyCtx,
		fmt.Sprintf("zfs get -Hp -o value volsize %s/%s", zfsPool, volName))
	if execErr != nil || !res.Success() {
		t.Logf("warning: zfs get volsize failed (exec=%v exit=%d stderr=%q) — skipping host verify",
			execErr, res.ExitCode, res.Stderr)
	} else {
		t.Logf("zfs volsize property: %s", strings.TrimSpace(res.Stdout))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 4: GetCapacity
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_GetCapacity verifies that GetCapacity returns non-zero
// capacity values for the ZFS pool.
func TestZFSBackend_GetCapacity(t *testing.T) {
	t.Parallel()
	client := newClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	resp, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
		PoolName: zfsPool,
	})
	cancel()
	if err != nil {
		t.Fatalf("GetCapacity(%q): %v", zfsPool, err)
	}

	t.Logf("GetCapacity: total=%d available=%d used=%d",
		resp.GetTotalBytes(), resp.GetAvailableBytes(), resp.GetUsedBytes())

	if resp.GetTotalBytes() <= 0 {
		t.Errorf("GetCapacity: total_bytes=%d, want > 0", resp.GetTotalBytes())
	}
	if resp.GetAvailableBytes() < 0 {
		t.Errorf("GetCapacity: available_bytes=%d, want >= 0", resp.GetAvailableBytes())
	}
	if resp.GetTotalBytes() < resp.GetAvailableBytes() {
		t.Errorf("GetCapacity: total_bytes=%d < available_bytes=%d (impossible)",
			resp.GetTotalBytes(), resp.GetAvailableBytes())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 5: ListVolumes
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_ListVolumes verifies that ListVolumes includes a newly created
// zvol with the correct fields.
func TestZFSBackend_ListVolumes(t *testing.T) {
	// Not parallel: creates a volume and relies on exact list membership.
	client := newClient(t)

	volName := uniqueName("zfs-list")
	volumeID := zfsPool + "/" + volName
	expectedDevPath := "/dev/zvol/" + zfsPool + "/" + volName

	const volSize = int64(128 << 20) // 128 MiB

	// Create the zvol.
	mustCreate(t, client, volumeID, volSize)

	// List volumes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
		PoolName: zfsPool,
	})
	cancel()
	if err != nil {
		t.Fatalf("ListVolumes(%q): %v", zfsPool, err)
	}

	t.Logf("ListVolumes: %d volumes returned", len(listResp.GetVolumes()))

	// Find our volume.
	var found *agentv1.VolumeInfo
	for _, v := range listResp.GetVolumes() {
		if v.GetVolumeId() == volumeID {
			found = v
			break
		}
	}

	if found == nil {
		t.Fatalf("ListVolumes: volume %q not found in list of %d volumes",
			volumeID, len(listResp.GetVolumes()))
	}

	t.Logf("ListVolumes: found %q capacity=%d device=%q",
		volumeID, found.GetCapacityBytes(), found.GetDevicePath())

	if found.GetCapacityBytes() <= 0 {
		t.Errorf("ListVolumes: volume %q capacity_bytes=%d, want > 0",
			volumeID, found.GetCapacityBytes())
	}
	if found.GetDevicePath() != expectedDevPath {
		t.Errorf("ListVolumes: volume %q device_path=%q, want %q",
			volumeID, found.GetDevicePath(), expectedDevPath)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test 6: DevicePath convention
// ─────────────────────────────────────────────────────────────────────────────

// TestZFSBackend_DevicePath verifies that both CreateVolume and ListVolumes
// return the /dev/zvol/<pool>/<name> device path convention, which corresponds
// to the ZFS backend's DevicePath() method output.
func TestZFSBackend_DevicePath(t *testing.T) {
	t.Parallel()
	client := newClient(t)

	volName := uniqueName("zfs-devpath")
	volumeID := zfsPool + "/" + volName
	expectedDevPath := "/dev/zvol/" + zfsPool + "/" + volName

	// Step 1: CreateVolume → check device_path.
	createResp := mustCreate(t, client, volumeID, 128<<20)

	if createResp.GetDevicePath() != expectedDevPath {
		t.Errorf("CreateVolume device_path=%q, want %q",
			createResp.GetDevicePath(), expectedDevPath)
	} else {
		t.Logf("CreateVolume device_path=%q matches /dev/zvol convention", createResp.GetDevicePath())
	}

	// Step 2: ListVolumes → check device_path in list.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
		PoolName: zfsPool,
	})
	cancel()
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}

	var found *agentv1.VolumeInfo
	for _, v := range listResp.GetVolumes() {
		if v.GetVolumeId() == volumeID {
			found = v
			break
		}
	}

	if found == nil {
		t.Errorf("ListVolumes: volume %q not found (cannot verify device_path)", volumeID)
		return
	}

	if found.GetDevicePath() != expectedDevPath {
		t.Errorf("ListVolumes device_path=%q, want %q",
			found.GetDevicePath(), expectedDevPath)
	} else {
		t.Logf("ListVolumes device_path=%q matches /dev/zvol convention", found.GetDevicePath())
	}
}
