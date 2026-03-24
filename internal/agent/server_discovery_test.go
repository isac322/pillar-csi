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

package agent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
)

// GetCapabilities tests.

func TestGetCapabilities_SupportedTypes(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     10 << 30,
		capacityAvailable: 7 << 30,
	}
	srv := newTestServer(mb)

	resp, err := srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities unexpected error: %v", err)
	}
	if resp.GetAgentVersion() == "" {
		t.Error("AgentVersion is empty")
	}

	// Must advertise the ZFS_ZVOL backend type.
	wantBackend := agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
	foundBackend := false
	for _, b := range resp.GetSupportedBackends() {
		if b == wantBackend {
			foundBackend = true
		}
	}
	if !foundBackend {
		t.Errorf("SupportedBackends %v does not contain %v", resp.GetSupportedBackends(), wantBackend)
	}

	// Must advertise the NVMe-oF TCP protocol type.
	wantProto := agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP
	foundProto := false
	for _, p := range resp.GetSupportedProtocols() {
		if p == wantProto {
			foundProto = true
		}
	}
	if !foundProto {
		t.Errorf("SupportedProtocols %v does not contain %v", resp.GetSupportedProtocols(), wantProto)
	}
}

func TestGetCapabilities_IncludesPoolInfo(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     100 << 30,
		capacityAvailable: 60 << 30,
	}
	srv := newTestServer(mb)

	resp, err := srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities unexpected error: %v", err)
	}
	if len(resp.GetDiscoveredPools()) != 1 {
		t.Fatalf("DiscoveredPools len = %d, want 1", len(resp.GetDiscoveredPools()))
	}

	pool := resp.GetDiscoveredPools()[0]
	if pool.GetName() != testPool {
		t.Errorf("pool.Name = %q, want %q", pool.GetName(), testPool)
	}
	if pool.GetTotalBytes() != 100<<30 {
		t.Errorf("TotalBytes = %d, want %d", pool.GetTotalBytes(), 100<<30)
	}
	if pool.GetAvailableBytes() != 60<<30 {
		t.Errorf("AvailableBytes = %d, want %d", pool.GetAvailableBytes(), 60<<30)
	}
}

func TestGetCapabilities_CapacityErrorSkipsPool(t *testing.T) {
	t.Parallel()
	// If the backend returns an error for Capacity, the pool is silently
	// omitted from DiscoveredPools (best-effort).
	mb := &mockBackend{capacityErr: errors.New("pool degraded")}
	srv := newTestServer(mb)

	resp, err := srv.GetCapabilities(context.Background(), &agentv1.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities unexpected error: %v", err)
	}
	if len(resp.GetDiscoveredPools()) != 0 {
		t.Errorf("DiscoveredPools = %v, want empty (capacity error should be skipped)",
			resp.GetDiscoveredPools())
	}
}

// HealthCheck tests.

// nvmetSubsysName is the subsystem name reported by HealthCheck for the NVMe-oF configfs check.
// Must match the name convention in health.HealthStatus.ToProtoSubsystems.
const nvmetSubsysName = "nvmet_configfs"

// findSubsystem returns the SubsystemStatus with the given name, or nil.
func findSubsystem(statuses []*agentv1.SubsystemStatus, name string) *agentv1.SubsystemStatus {
	for _, s := range statuses {
		if s.GetName() == name {
			return s
		}
	}
	return nil
}

func TestHealthCheck_NvmetConfigfsUnhealthy(t *testing.T) {
	t.Parallel()
	// The temp configfsRoot has no nvmet/ subdirectory, so the check must
	// report unhealthy.
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	if resp.GetAgentVersion() == "" {
		t.Error("AgentVersion is empty")
	}
	if resp.GetCheckedAt() == nil {
		t.Error("CheckedAt timestamp is nil")
	}

	sub := findSubsystem(resp.GetSubsystems(), nvmetSubsysName)
	if sub == nil {
		t.Fatalf("%s subsystem not found in HealthCheck response", nvmetSubsysName)
	}
	if sub.GetHealthy() {
		t.Errorf("%s should be unhealthy when nvmet/ dir is absent", nvmetSubsysName)
	}
}

func TestHealthCheck_NvmetConfigfsHealthy(t *testing.T) {
	t.Parallel()
	srv, cfgRoot := newExportTestServer(t, &mockBackend{})

	// Create the nvmet/ directory so the configfs check succeeds.
	nvmetDir := filepath.Join(cfgRoot, "nvmet")
	mkErr := os.MkdirAll(nvmetDir, 0o750)
	if mkErr != nil {
		t.Fatalf("create nvmet dir: %v", mkErr)
	}

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	sub := findSubsystem(resp.GetSubsystems(), nvmetSubsysName)
	if sub == nil {
		t.Fatalf("%s subsystem not found in HealthCheck response", nvmetSubsysName)
	}
	if !sub.GetHealthy() {
		t.Errorf("%s should be healthy when nvmet/ dir exists: %s", nvmetSubsysName, sub.GetMessage())
	}
}

func TestHealthCheck_SubsystemsPresent(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	// Expect at least two core subsystems (zfs_module and nvmet_configfs)
	// plus one pool entry for "tank".
	const wantMinSubsystems = 3
	if len(resp.GetSubsystems()) < wantMinSubsystems {
		t.Errorf("expected >= %d subsystems, got %d", wantMinSubsystems, len(resp.GetSubsystems()))
	}
	// Overall healthy must be false when any subsystem is unhealthy.
	if resp.GetHealthy() {
		for _, sub := range resp.GetSubsystems() {
			if !sub.GetHealthy() {
				t.Errorf("overall Healthy=true but subsystem %q is unhealthy: %s",
					sub.GetName(), sub.GetMessage())
			}
		}
	}
}

// TestHealthCheck_NamedSubsystemFields verifies that the structured HealthStatus
// model produces subsystem entries with the expected stable name conventions:
//   - "zfs_module"       for the ZFS kernel module check
//   - "nvmet_configfs"   for the nvmet configfs check
//   - "pool/<pool-name>" for each registered pool
func TestHealthCheck_NamedSubsystemFields(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{
		capacityTotal:     10 << 30,
		capacityAvailable: 8 << 30,
	})

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	// zfs_module must be present.
	if sub := findSubsystem(resp.GetSubsystems(), "zfs_module"); sub == nil {
		t.Error("subsystem \"zfs_module\" not found in HealthCheck response")
	}

	// nvmet_configfs must be present.
	if sub := findSubsystem(resp.GetSubsystems(), nvmetSubsysName); sub == nil {
		t.Errorf("subsystem %q not found in HealthCheck response", nvmetSubsysName)
	}

	// pool/tank must be present (the mock backend is registered for pool "tank").
	const wantPoolEntry = "pool/" + testPool
	if sub := findSubsystem(resp.GetSubsystems(), wantPoolEntry); sub == nil {
		t.Errorf("subsystem %q not found in HealthCheck response", wantPoolEntry)
	}
}

// TestHealthCheck_PoolStatusHealthy verifies that a pool whose backend
// answers Capacity successfully is reported as healthy.
func TestHealthCheck_PoolStatusHealthy(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     50 << 30,
		capacityAvailable: 40 << 30,
	}
	srv, _ := newExportTestServer(t, mb)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	sub := findSubsystem(resp.GetSubsystems(), "pool/"+testPool)
	if sub == nil {
		t.Fatalf("pool/%s subsystem not found", testPool)
	}
	if !sub.GetHealthy() {
		t.Errorf("pool/%s should be healthy when Capacity succeeds: %s", testPool, sub.GetMessage())
	}
}

// TestHealthCheck_PoolStatusDegraded verifies that a pool whose backend
// returns an error from Capacity is reported as unhealthy, and the overall
// response Healthy flag is also false.
func TestHealthCheck_PoolStatusDegraded(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityErr: errors.New("pool degraded"),
	}
	srv, _ := newExportTestServer(t, mb)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	sub := findSubsystem(resp.GetSubsystems(), "pool/"+testPool)
	if sub == nil {
		t.Fatalf("pool/%s subsystem not found", testPool)
	}
	if sub.GetHealthy() {
		t.Errorf("pool/%s should be unhealthy when Capacity fails", testPool)
	}
	if resp.GetHealthy() {
		t.Error("overall Healthy should be false when a pool is degraded")
	}
}

// ZFS module health-check tests.
//
// These tests use SetServerSysModuleZFSPath (exported via export_test.go) to
// inject a t.TempDir()-based path so the check can be exercised in both the
// healthy and unhealthy states without requiring the ZFS kernel module to be
// installed in the CI environment.

// TestHealthCheck_ZFSModuleHealthy verifies that checkZFSModule reports
// healthy when the target path exists (simulating a loaded kernel module).
func TestHealthCheck_ZFSModuleHealthy(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	// Create a fake "zfs" directory to simulate /sys/module/zfs being present.
	fakeZFSPath := filepath.Join(t.TempDir(), "zfs")
	if err := os.MkdirAll(fakeZFSPath, 0o750); err != nil {
		t.Fatalf("create fake ZFS module dir: %v", err)
	}
	agent.SetServerSysModuleZFSPath(t, srv, fakeZFSPath)

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	sub := findSubsystem(resp.GetSubsystems(), "zfs_module")
	if sub == nil {
		t.Fatal("zfs_module subsystem not found in HealthCheck response")
	}
	if !sub.GetHealthy() {
		t.Errorf("zfs_module should be healthy when path exists: %s", sub.GetMessage())
	}
}

// TestHealthCheck_ZFSModuleUnhealthy verifies that checkZFSModule reports
// unhealthy when the target path does not exist (simulating a missing module).
func TestHealthCheck_ZFSModuleUnhealthy(t *testing.T) {
	t.Parallel()
	srv, _ := newExportTestServer(t, &mockBackend{})

	// Point to a path that does not exist — no ZFS module present.
	agent.SetServerSysModuleZFSPath(t, srv, filepath.Join(t.TempDir(), "zfs-not-present"))

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}

	sub := findSubsystem(resp.GetSubsystems(), "zfs_module")
	if sub == nil {
		t.Fatal("zfs_module subsystem not found in HealthCheck response")
	}
	if sub.GetHealthy() {
		t.Error("zfs_module should be unhealthy when path does not exist")
	}
	if sub.GetMessage() == "" {
		t.Error("zfs_module unhealthy message must be non-empty")
	}
}

// TestHealthCheck_ZFSModuleUnhealthy_OverallHealthFalse verifies that an
// unhealthy ZFS module causes the overall HealthCheck response to be unhealthy.
func TestHealthCheck_ZFSModuleUnhealthy_OverallHealthFalse(t *testing.T) {
	t.Parallel()
	mb := &mockBackend{
		capacityTotal:     10 << 30,
		capacityAvailable: 8 << 30,
	}
	srv, cfgRoot := newExportTestServer(t, mb)

	// Create nvmet dir so that subsystem is healthy.
	if err := os.MkdirAll(filepath.Join(cfgRoot, "nvmet"), 0o750); err != nil {
		t.Fatalf("create nvmet dir: %v", err)
	}

	// Point ZFS module check at a non-existent path.
	agent.SetServerSysModuleZFSPath(t, srv, filepath.Join(t.TempDir(), "no-zfs"))

	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	if resp.GetHealthy() {
		t.Error("overall Healthy should be false when ZFS module is missing")
	}
}
