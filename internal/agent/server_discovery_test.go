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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// GetCapacity handler tests for LVM linear provisioning mode.
//
// The GetCapacity RPC is backend-agnostic: it delegates to the registered
// backend's Capacity() method and computes UsedBytes = TotalBytes - AvailableBytes.
// For the LVM linear provisioning mode, the underlying backend queries:
//
//	vgs --noheadings -o vg_size,vg_free --units b --nosuffix <vg>
//
// These tests verify the RPC handler behavior using a mock backend that
// simulates LVM-style capacity responses.

// TestGetCapacity_LVM_LinearVGFreeReturned verifies that the VG free space
// reported by the backend (simulating `vg_free` from `vgs`) is passed through
// correctly as AvailableBytes, and that UsedBytes is derived as
// TotalBytes - AvailableBytes.
func TestGetCapacity_LVM_LinearVGFreeReturned(t *testing.T) {
	t.Parallel()

	const total = int64(100 << 30) // 100 GiB — simulates vg_size
	const avail = int64(60 << 30)  // 60 GiB free — simulates vg_free
	const wantUsed = total - avail // 40 GiB used

	mb := &mockBackend{
		capacityTotal:     total,
		capacityAvailable: avail,
	}
	srv := newTestServer(mb)

	resp, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: testPool,
	})
	if err != nil {
		t.Fatalf("GetCapacity (LVM linear): unexpected error: %v", err)
	}
	if resp.GetTotalBytes() != total {
		t.Errorf("TotalBytes = %d; want %d (vg_size)", resp.GetTotalBytes(), total)
	}
	if resp.GetAvailableBytes() != avail {
		t.Errorf("AvailableBytes = %d; want %d (vg_free)", resp.GetAvailableBytes(), avail)
	}
	if resp.GetUsedBytes() != wantUsed {
		t.Errorf("UsedBytes = %d; want %d (= total - vg_free)", resp.GetUsedBytes(), wantUsed)
	}
}

// TestGetCapacity_LVM_ZeroVGFree verifies that a fully-consumed VG (vg_free == 0)
// is reported correctly: AvailableBytes = 0, UsedBytes = TotalBytes.
func TestGetCapacity_LVM_ZeroVGFree(t *testing.T) {
	t.Parallel()

	const total = int64(50 << 30)
	mb := &mockBackend{
		capacityTotal:     total,
		capacityAvailable: 0, // VG entirely consumed — vg_free = 0
	}
	srv := newTestServer(mb)

	resp, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: testPool,
	})
	if err != nil {
		t.Fatalf("GetCapacity (LVM full VG): unexpected error: %v", err)
	}
	if resp.GetAvailableBytes() != 0 {
		t.Errorf("AvailableBytes = %d; want 0 (full VG)", resp.GetAvailableBytes())
	}
	if resp.GetUsedBytes() != total {
		t.Errorf("UsedBytes = %d; want %d (full VG)", resp.GetUsedBytes(), total)
	}
}

// TestGetCapacity_LVM_VGNotFound verifies that when the backend's Capacity
// call fails (e.g. the `vgs` command reports VG not found), GetCapacity
// surfaces a codes.Internal gRPC error.
func TestGetCapacity_LVM_VGNotFound(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{
		capacityErr: errors.New("vgs: volume group \"data-vg\" not found"),
	}
	srv := newTestServer(mb)

	_, err := srv.GetCapacity(context.Background(), &agentv1.GetCapacityRequest{
		PoolName: testPool,
	})
	if err == nil {
		t.Fatal("GetCapacity (LVM VG not found): expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("code = %v; want %v (vgs failure should map to Internal)", st.Code(), codes.Internal)
	}
}

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

	// Expect at least one core subsystem (nvmet_configfs)
	// plus one pool entry for "tank".
	const wantMinSubsystems = 2
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
