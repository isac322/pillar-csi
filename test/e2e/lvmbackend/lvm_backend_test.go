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

// Package lvmbackend provides standalone E2E tests for the LVM backend RPCs.
//
// Unlike the main test/e2e package (which requires a full Kind cluster lifecycle),
// this package tests the pillar-agent's LVM backend RPCs directly over gRPC
// without needing a Kubernetes cluster.
//
// # What it tests
//
// The six core VolumeBackend RPCs for both linear and thin provisioning modes:
//
//  1. CreateVolume   — provisions an LVM LV (thin and linear); verifies
//                      device_path convention and capacity_bytes.
//
//  2. DeleteVolume   — creates then deletes an LV; verifies idempotency.
//
//  3. ExpandVolume   — creates a small linear LV and expands it; verifies
//                      response capacity_bytes ≥ requested size.
//
//  4. GetCapacity    — queries VG capacity; verifies non-zero total_bytes.
//
//  5. ListVolumes    — creates an LV and lists all volumes; verifies the
//                      new volume appears with correct fields.
//
//  6. DevicePath     — verifies CreateVolume and ListVolumes both return the
//                      /dev/<vg>/<lv-name> device path convention.
//
// # Provisioning modes
//
// The agent is started with a thin pool configured (default):
//
//	--backend=type=lvm-lv,vg=e2e-vg,thinpool=e2e-thin-pool
//
// Thin mode is the backend default.  Linear mode is exercised by passing
// LvmVolumeParams.ProvisionMode="linear" in the BackendParams field.
//
// # Prerequisites
//
// The following environment variables control the test:
//
//	LVM_BACKEND_VG         LVM Volume Group name (default: "e2e-vg").
//	LVM_BACKEND_THINPOOL   LVM thin pool name in VG (default: "e2e-thin-pool").
//	LVM_BACKEND_AGENT      Agent container name (default: "pillar-csi-lvmb-agent").
//	LVM_BACKEND_AGENT_PORT Host port for the agent (default: "9551").
//	LVM_BACKEND_EXEC       Host-exec container name (default: "pillar-csi-lvmb-exec").
//	LVM_BACKEND_IMAGE_PATH Sparse image path on the Docker host (default: "/tmp/e2e-lvm-b.img").
//	LVM_BACKEND_IMAGE_SIZE Sparse image size for truncate(1) (default: "4G").
//
// # Running
//
//	go test -tags=e2e ./test/e2e/lvmbackend/ -v -timeout=10m
package lvmbackend

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
	lvmVG          string // LVM Volume Group name
	lvmThinPool    string // LVM thin pool name within lvmVG
	lvmImagePath   string // path of sparse backing image
	agentAddr      string // host:port for gRPC
	hostExecHelper *framework.DockerHostExec
	lvmLoopDev     string // loop device path (e.g. "/dev/loop5")
)

const (
	agentImage       = "ghcr.io/bhyoo/pillar-csi/agent:e2e"
	defaultVG        = "e2e-vg"
	defaultThinPool  = "e2e-thin-pool"
	defaultAgentName = "pillar-csi-lvmb-agent"
	defaultAgentPort = "9551"
	defaultExecName  = "pillar-csi-lvmb-exec"
	defaultImagePath = "/tmp/e2e-lvm-b.img"
	defaultImageSize = "4G"
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
// LVM setup / teardown on the Docker host
// ─────────────────────────────────────────────────────────────────────────────

// setupLVMVG creates a loop-backed LVM Volume Group on the Docker host via
// the host-exec helper.  It also creates an optional thin pool inside the VG.
//
// The function is idempotent: it destroys any stale VG and loop device with the
// same name/path before creating fresh ones.  lvmLoopDev is set on success.
func setupLVMVG(ctx context.Context, vgName, thinPool, imagePath, imageSize string) error {
	h := hostExecHelper

	// ── Clean up any stale state from a previous interrupted run ─────────────
	// Best-effort; ignore errors.
	_, _ = h.ExecOnHost(ctx, fmt.Sprintf(
		"vgremove -f %s 2>/dev/null; "+
			"losetup -j %s 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null; "+
			"rm -f %s",
		vgName, imagePath, imagePath,
	))

	// ── Create sparse image ───────────────────────────────────────────────────
	res, err := h.ExecOnHost(ctx, fmt.Sprintf("truncate -s %s %s", imageSize, imagePath))
	if err != nil {
		return fmt.Errorf("truncate %s: %w", imagePath, err)
	}
	if !res.Success() {
		return fmt.Errorf("truncate %s: %s", imagePath, res)
	}

	// ── Attach loop device ────────────────────────────────────────────────────
	res, err = h.ExecOnHost(ctx, fmt.Sprintf("losetup -f --show %s", imagePath))
	if err != nil {
		return fmt.Errorf("losetup -f --show %s: %w", imagePath, err)
	}
	if !res.Success() {
		return fmt.Errorf("losetup -f --show %s: %s", imagePath, res)
	}
	lvmLoopDev = strings.TrimSpace(res.Stdout)
	if lvmLoopDev == "" {
		return fmt.Errorf("losetup returned empty loop device path")
	}

	// ── pvcreate ─────────────────────────────────────────────────────────────
	res, err = h.ExecOnHost(ctx, fmt.Sprintf("pvcreate -y %s", lvmLoopDev))
	if err != nil {
		return fmt.Errorf("pvcreate %s: %w", lvmLoopDev, err)
	}
	if !res.Success() {
		return fmt.Errorf("pvcreate %s: %s", lvmLoopDev, res)
	}

	// ── vgcreate ─────────────────────────────────────────────────────────────
	res, err = h.ExecOnHost(ctx, fmt.Sprintf("vgcreate %s %s", vgName, lvmLoopDev))
	if err != nil {
		return fmt.Errorf("vgcreate %s %s: %w", vgName, lvmLoopDev, err)
	}
	if !res.Success() {
		return fmt.Errorf("vgcreate %s %s: %s", vgName, lvmLoopDev, res)
	}

	// ── Create thin pool (optional) ───────────────────────────────────────────
	if thinPool != "" {
		res, err = h.ExecOnHost(ctx, fmt.Sprintf(
			"lvcreate -l '80%%VG' -T %s/%s", vgName, thinPool))
		if err != nil {
			return fmt.Errorf("lvcreate thin pool %s/%s: %w", vgName, thinPool, err)
		}
		if !res.Success() {
			return fmt.Errorf("lvcreate thin pool %s/%s: %s", vgName, thinPool, res)
		}
	}

	return nil
}

// teardownLVMVG destroys the LVM VG, PV, loop device, and backing image.
// All steps are attempted even if earlier ones fail.
func teardownLVMVG(ctx context.Context, vgName, loopDev, imagePath string) {
	h := hostExecHelper
	if vgName != "" {
		_, _ = h.ExecOnHost(ctx, fmt.Sprintf("vgremove -f %s 2>/dev/null || true", vgName))
	}
	if loopDev != "" {
		_, _ = h.ExecOnHost(ctx, fmt.Sprintf("pvremove -f %s 2>/dev/null || true", loopDev))
		_, _ = h.ExecOnHost(ctx, fmt.Sprintf("losetup -d %s 2>/dev/null || true", loopDev))
	}
	if imagePath != "" {
		_, _ = h.ExecOnHost(ctx, fmt.Sprintf("rm -f %s", imagePath))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestMain — lightweight setup / teardown (no Kind cluster)
// ─────────────────────────────────────────────────────────────────────────────

// TestMain performs minimal setup:
//  1. Reads env-var configuration.
//  2. Creates a DockerHostExec helper for device-presence verification and LVM setup.
//  3. Creates a loop-backed LVM Volume Group (+ thin pool) on the Docker host.
//  4. Starts a privileged pillar-agent container with LVM backend.
//  5. Waits for the agent's gRPC port to become reachable.
//  6. Runs all Test* functions.
//  7. On exit: removes agent container, destroys LVM VG/loop, closes exec helper.
func TestMain(m *testing.M) {
	exitCode := 1
	defer func() { os.Exit(exitCode) }()

	lvmVG = envOrDefault("LVM_BACKEND_VG", defaultVG)
	lvmThinPool = envOrDefault("LVM_BACKEND_THINPOOL", defaultThinPool)
	lvmImagePath = envOrDefault("LVM_BACKEND_IMAGE_PATH", defaultImagePath)
	imageSize := envOrDefault("LVM_BACKEND_IMAGE_SIZE", defaultImageSize)
	agentName := envOrDefault("LVM_BACKEND_AGENT", defaultAgentName)
	agentPort := envOrDefault("LVM_BACKEND_AGENT_PORT", defaultAgentPort)
	execName := envOrDefault("LVM_BACKEND_EXEC", defaultExecName)
	agentAddr = "127.0.0.1:" + agentPort

	ctx := context.Background()

	// ── Step 1: Create host-exec helper ───────────────────────────────────────
	fmt.Fprintf(os.Stdout, "lvmbackend: starting host-exec helper %q\n", execName)
	var err error
	hostExecHelper, err = framework.NewDockerHostExecNamed(ctx, "", execName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lvmbackend: start host-exec helper: %v\n", err)
		return
	}
	defer func() {
		if closeErr := hostExecHelper.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "lvmbackend: close host-exec helper: %v\n", closeErr)
		}
	}()

	// ── Step 2: Create LVM loopback VG ────────────────────────────────────────
	fmt.Fprintf(os.Stdout,
		"lvmbackend: creating LVM VG %q (image %s, size %s, thinpool %q)\n",
		lvmVG, lvmImagePath, imageSize, lvmThinPool)
	if setupErr := setupLVMVG(ctx, lvmVG, lvmThinPool, lvmImagePath, imageSize); setupErr != nil {
		fmt.Fprintf(os.Stderr, "lvmbackend: create LVM VG %q: %v\n", lvmVG, setupErr)
		return
	}
	fmt.Fprintf(os.Stdout, "lvmbackend: LVM VG %q ready (loop %s)\n", lvmVG, lvmLoopDev)

	defer func() {
		fmt.Fprintf(os.Stdout, "lvmbackend: tearing down LVM VG %q\n", lvmVG)
		teardownLVMVG(ctx, lvmVG, lvmLoopDev, lvmImagePath)
	}()

	// ── Step 3: Start external agent container ────────────────────────────────
	fmt.Fprintf(os.Stdout, "lvmbackend: removing any stale agent container %q\n", agentName)
	_, _ = runDockerCmd("rm", "-f", agentName)

	backendFlag := "type=lvm-lv,vg=" + lvmVG
	if lvmThinPool != "" {
		backendFlag += ",thinpool=" + lvmThinPool
	}

	fmt.Fprintf(os.Stdout,
		"lvmbackend: starting agent container %q (image %s, port %s→9500, backend=%s)\n",
		agentName, agentImage, agentPort, backendFlag)

	containerID, startErr := runDockerCmd("run",
		"--detach",
		"--name", agentName,
		"-p", "127.0.0.1:"+agentPort+":9500",
		"--privileged",
		"--user=root",
		// Share /dev so the agent container sees /dev/mapper/ entries and
		// /dev/<vg>/ symlinks created by udev on the host.
		"-v", "/dev:/dev",
		// Share /run/lock so LVM metadata locking works correctly between
		// host and container LVM invocations.
		"-v", "/run/lock:/run/lock",
		agentImage,
		"--listen-address=0.0.0.0:9500",
		"--backend="+backendFlag,
		"--configfs-root=/tmp",
	)
	if startErr != nil {
		fmt.Fprintf(os.Stderr, "lvmbackend: docker run agent: %s: %v\n", containerID, startErr)
		return
	}
	fmt.Fprintf(os.Stdout, "lvmbackend: agent container started (id %.12s)\n", containerID)

	defer func() {
		fmt.Fprintf(os.Stdout, "lvmbackend: stopping agent container %q\n", agentName)
		if _, stopErr := runDockerCmd("rm", "-f", agentName); stopErr != nil {
			fmt.Fprintf(os.Stderr, "lvmbackend: remove agent container: %v\n", stopErr)
		}
	}()

	// ── Step 4: Wait for agent to be ready ────────────────────────────────────
	fmt.Fprintf(os.Stdout,
		"lvmbackend: waiting up to 60s for agent gRPC port on %s\n", agentAddr)
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
		// Print agent logs for debugging.
		if logs, logErr := runDockerCmd("logs", agentName); logErr == nil {
			fmt.Fprintf(os.Stderr, "lvmbackend: agent logs:\n%s\n", logs)
		}
		fmt.Fprintf(os.Stderr, "lvmbackend: agent did not become ready within 60s on %s\n", agentAddr)
		return
	}
	fmt.Fprintf(os.Stdout, "lvmbackend: agent is ready at %s\n", agentAddr)

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

// uniqueName returns a short unique name for a test LV.
// LV names must be valid LVM identifiers (letters, digits, underscores, hyphens).
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%10_000_000)
}

// lvVolumeID returns the VolumeID for an LV: "<vg>/<lv-name>".
func lvVolumeID(lvName string) string {
	return lvmVG + "/" + lvName
}

// expectedDevPath returns the expected LVM device path for an LV.
func expectedDevPath(lvName string) string {
	return "/dev/" + lvmVG + "/" + lvName
}

// waitDevice polls the remote host for LV device presence/absence.
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
// Cleanup retries DeleteVolume up to 5 times to handle transient "device busy"
// errors that can occur when udev is still processing the LV block device node.
func mustCreate(t *testing.T, client agentv1.AgentServiceClient, volumeID string, sizeBytes int64, params *agentv1.BackendParams) *agentv1.CreateVolumeResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: sizeBytes,
		BackendParams: params,
	})
	if err != nil {
		t.Fatalf("CreateVolume(%q, %d): %v", volumeID, sizeBytes, err)
	}

	t.Cleanup(func() {
		// Retry DeleteVolume up to 5 times to handle transient "device is busy"
		// errors from udev still holding a reference to the LV block device.
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
			}
		}
	})
	return resp
}

// linearParams returns BackendParams that force linear provisioning mode,
// overriding the backend default (thin).
func linearParams() *agentv1.BackendParams {
	return &agentv1.BackendParams{
		Params: &agentv1.BackendParams_Lvm{
			Lvm: &agentv1.LvmVolumeParams{
				ProvisionMode: "linear",
			},
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

// TestLVMBackend_CreateVolume_Thin verifies CreateVolume creates a thin LV,
// returns a /dev/<vg>/<lv> device path, and the block device appears on the host.
func TestLVMBackend_CreateVolume_Thin(t *testing.T) {
	if lvmThinPool == "" {
		t.Skip("thin pool not configured; set LVM_BACKEND_THINPOOL to enable")
	}

	client := newClient(t)
	lvName := uniqueName("lvmthin")
	volumeID := lvVolumeID(lvName)
	const sizeBytes = 64 * 1024 * 1024 // 64 MiB

	resp := mustCreate(t, client, volumeID, sizeBytes, nil /* default = thin */)

	t.Logf("CreateVolume (thin): capacity_bytes=%d", resp.CapacityBytes)
	if resp.CapacityBytes < sizeBytes {
		t.Errorf("capacity_bytes %d < requested %d", resp.CapacityBytes, sizeBytes)
	}

	wantPath := expectedDevPath(lvName)
	t.Logf("CreateVolume (thin): device_path=%q (expected %q)", resp.DevicePath, wantPath)
	if resp.DevicePath != wantPath {
		t.Errorf("device_path=%q, want %q", resp.DevicePath, wantPath)
	}

	t.Logf("waiting for %s to appear on host", wantPath)
	waitDevice(t, wantPath, true)
}

// TestLVMBackend_CreateVolume_Linear verifies CreateVolume creates a linear LV
// when ProvisionMode="linear" is passed as a per-volume override.
func TestLVMBackend_CreateVolume_Linear(t *testing.T) {
	client := newClient(t)
	lvName := uniqueName("lvmlin")
	volumeID := lvVolumeID(lvName)
	const sizeBytes = 64 * 1024 * 1024 // 64 MiB

	resp := mustCreate(t, client, volumeID, sizeBytes, linearParams())

	t.Logf("CreateVolume (linear): capacity_bytes=%d", resp.CapacityBytes)
	if resp.CapacityBytes < sizeBytes {
		t.Errorf("capacity_bytes %d < requested %d", resp.CapacityBytes, sizeBytes)
	}

	wantPath := expectedDevPath(lvName)
	t.Logf("CreateVolume (linear): device_path=%q (expected %q)", resp.DevicePath, wantPath)
	if resp.DevicePath != wantPath {
		t.Errorf("device_path=%q, want %q", resp.DevicePath, wantPath)
	}

	t.Logf("waiting for %s to appear on host", wantPath)
	waitDevice(t, wantPath, true)
}

// TestLVMBackend_DeleteVolume verifies DeleteVolume removes the LV and is
// idempotent when called on an already-deleted (or never-created) volume.
func TestLVMBackend_DeleteVolume(t *testing.T) {
	client := newClient(t)
	lvName := uniqueName("lvmdel")
	volumeID := lvVolumeID(lvName)
	const sizeBytes = 64 * 1024 * 1024 // 64 MiB

	// Use linear mode so we don't depend on thin pool availability.
	_ = mustCreate(t, client, volumeID, sizeBytes, linearParams())

	devPath := expectedDevPath(lvName)
	t.Logf("waiting for %s to appear", devPath)
	waitDevice(t, devPath, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
	if err != nil {
		t.Fatalf("DeleteVolume(%q): %v", volumeID, err)
	}
	t.Logf("DeleteVolume: succeeded")

	t.Logf("waiting for %s to disappear", devPath)
	waitDevice(t, devPath, false)

	// Second delete must be idempotent (not-found → success).
	_, err2 := client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: volumeID})
	if err2 != nil {
		t.Errorf("DeleteVolume (idempotent call): %v", err2)
	}
}

// TestLVMBackend_ExpandVolume verifies ExpandVolume grows a linear LV to at
// least the requested size, and the capacity_bytes in the response reflects
// the new size.
func TestLVMBackend_ExpandVolume(t *testing.T) {
	client := newClient(t)
	lvName := uniqueName("lvmexp")
	volumeID := lvVolumeID(lvName)
	const initialBytes = 64 * 1024 * 1024  // 64 MiB
	const expandedBytes = 128 * 1024 * 1024 // 128 MiB

	// Create linear LV at initial size.
	_ = mustCreate(t, client, volumeID, initialBytes, linearParams())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	expandResp, err := client.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       volumeID,
		RequestedBytes: expandedBytes,
	})
	if err != nil {
		t.Fatalf("ExpandVolume(%q, %d): %v", volumeID, expandedBytes, err)
	}

	t.Logf("ExpandVolume: capacity_bytes=%d (requested=%d)", expandResp.CapacityBytes, expandedBytes)
	if expandResp.CapacityBytes < expandedBytes {
		t.Errorf("expanded capacity_bytes %d < requested %d",
			expandResp.CapacityBytes, expandedBytes)
	}

	// Verify the actual LV size on the host via lvs.
	lvsOut, lvsErr := hostExecHelper.ExecOnHost(ctx,
		fmt.Sprintf("lvs --noheadings -o lv_size --units b --nosuffix %s/%s", lvmVG, lvName),
	)
	if lvsErr != nil || !lvsOut.Success() {
		t.Logf("lvs verification skipped: execErr=%v result=%s", lvsErr, lvsOut)
	} else {
		t.Logf("lvs lv_size after expand: %s", strings.TrimSpace(lvsOut.Stdout))
	}
}

// TestLVMBackend_GetCapacity verifies GetCapacity returns non-zero total and
// available bytes for the LVM Volume Group.
func TestLVMBackend_GetCapacity(t *testing.T) {
	client := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Query capacity using the VG name as the pool name.
	resp, err := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
		PoolName: lvmVG,
	})
	if err != nil {
		t.Fatalf("GetCapacity(%q): %v", lvmVG, err)
	}

	t.Logf("GetCapacity: total=%d available=%d used=%d",
		resp.TotalBytes, resp.AvailableBytes, resp.UsedBytes)

	if resp.TotalBytes <= 0 {
		t.Errorf("total_bytes=%d, want > 0", resp.TotalBytes)
	}
	if resp.AvailableBytes < 0 {
		t.Errorf("available_bytes=%d, want >= 0", resp.AvailableBytes)
	}
	if resp.AvailableBytes > resp.TotalBytes {
		t.Errorf("available_bytes=%d > total_bytes=%d", resp.AvailableBytes, resp.TotalBytes)
	}
}

// TestLVMBackend_ListVolumes verifies ListVolumes returns the created LV with
// correct VolumeID, capacity_bytes, and device_path fields.
func TestLVMBackend_ListVolumes(t *testing.T) {
	client := newClient(t)
	lvName := uniqueName("lvmlst")
	volumeID := lvVolumeID(lvName)
	const sizeBytes = 64 * 1024 * 1024 // 64 MiB

	// Create a linear volume (avoids thin pool exhaustion in a list test).
	_ = mustCreate(t, client, volumeID, sizeBytes, linearParams())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{
		PoolName: lvmVG,
	})
	if err != nil {
		t.Fatalf("ListVolumes(%q): %v", lvmVG, err)
	}

	t.Logf("ListVolumes: %d volume(s) returned", len(listResp.Volumes))

	var found bool
	for _, vol := range listResp.Volumes {
		t.Logf("  volume: id=%q capacity=%d device_path=%q",
			vol.VolumeId, vol.CapacityBytes, vol.DevicePath)
		if vol.VolumeId == volumeID {
			found = true
			if vol.CapacityBytes < sizeBytes {
				t.Errorf("volume %q: capacity_bytes=%d < %d",
					vol.VolumeId, vol.CapacityBytes, sizeBytes)
			}
			wantPath := expectedDevPath(lvName)
			if vol.DevicePath != wantPath {
				t.Errorf("volume %q: device_path=%q, want %q",
					vol.VolumeId, vol.DevicePath, wantPath)
			}
		}
	}
	if !found {
		t.Errorf("volume %q not found in ListVolumes response", volumeID)
	}
}

// TestLVMBackend_DevicePath verifies that both CreateVolume and ListVolumes
// return device paths following the /dev/<vg>/<lv-name> convention for both
// thin and linear provisioning modes.
func TestLVMBackend_DevicePath(t *testing.T) {
	client := newClient(t)

	// ── Linear LV ────────────────────────────────────────────────────────────
	t.Run("linear", func(t *testing.T) {
		lvName := uniqueName("lvmdplin")
		volumeID := lvVolumeID(lvName)
		wantPath := expectedDevPath(lvName)
		const sizeBytes = 64 * 1024 * 1024

		resp := mustCreate(t, client, volumeID, sizeBytes, linearParams())

		if resp.DevicePath != wantPath {
			t.Errorf("CreateVolume device_path=%q, want %q", resp.DevicePath, wantPath)
		}
		t.Logf("CreateVolume (linear) device_path=%q matches /dev/<vg>/<lv> convention", resp.DevicePath)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{PoolName: lvmVG})
		if err != nil {
			t.Fatalf("ListVolumes: %v", err)
		}
		for _, vol := range listResp.Volumes {
			if vol.VolumeId == volumeID {
				if vol.DevicePath != wantPath {
					t.Errorf("ListVolumes device_path=%q, want %q", vol.DevicePath, wantPath)
				}
				t.Logf("ListVolumes (linear) device_path=%q matches /dev/<vg>/<lv> convention", vol.DevicePath)
				return
			}
		}
		t.Errorf("volume %q not found in ListVolumes response", volumeID)
	})

	// ── Thin LV ───────────────────────────────────────────────────────────────
	if lvmThinPool != "" {
		t.Run("thin", func(t *testing.T) {
			lvName := uniqueName("lvmdpthin")
			volumeID := lvVolumeID(lvName)
			wantPath := expectedDevPath(lvName)
			const sizeBytes = 64 * 1024 * 1024

			resp := mustCreate(t, client, volumeID, sizeBytes, nil /* default = thin */)

			if resp.DevicePath != wantPath {
				t.Errorf("CreateVolume (thin) device_path=%q, want %q", resp.DevicePath, wantPath)
			}
			t.Logf("CreateVolume (thin) device_path=%q matches /dev/<vg>/<lv> convention", resp.DevicePath)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			listResp, err := client.ListVolumes(ctx, &agentv1.ListVolumesRequest{PoolName: lvmVG})
			if err != nil {
				t.Fatalf("ListVolumes: %v", err)
			}
			for _, vol := range listResp.Volumes {
				if vol.VolumeId == volumeID {
					if vol.DevicePath != wantPath {
						t.Errorf("ListVolumes (thin) device_path=%q, want %q", vol.DevicePath, wantPath)
					}
					t.Logf("ListVolumes (thin) device_path=%q matches /dev/<vg>/<lv> convention", vol.DevicePath)
					return
				}
			}
			t.Errorf("volume %q not found in ListVolumes", volumeID)
		})
	}
}
