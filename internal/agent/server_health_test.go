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

// Package agent_test provides focused unit tests for the HealthCheck RPC.
//
// Design principles applied in this file:
//   - os.Stat is mocked by pointing sysModuleZFSPath (via SetServerSysModuleZFSPath)
//     and configfsRoot (via NewServer) at t.TempDir() paths so that the ZFS module
//     and nvmet configfs checks can be driven into healthy or unhealthy states
//     without requiring kernel modules or real configfs mounts.
//   - backend.Capacity() is mocked via mockBackend.capacityErr so that per-pool
//     reachability can be driven to healthy or unhealthy.
//   - Every assertion targets a NAMED SubsystemStatus field (Name, Healthy, Message)
//     rather than the top-level resp.GetHealthy() boolean alone.  This ensures the
//     structured health.HealthStatus model produces the exact wire values consumed
//     by callers that inspect individual subsystems by name.
package agent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
)

// Test helpers.

// healthCheckResp calls HealthCheck and fatals on error.
func healthCheckResp(t *testing.T, srv *agent.Server) *agentv1.HealthCheckResponse {
	t.Helper()
	resp, err := srv.HealthCheck(context.Background(), &agentv1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("HealthCheck unexpected error: %v", err)
	}
	return resp
}

// requireSubsystem looks up a SubsystemStatus by exact name and fatals if absent.
func requireSubsystem(t *testing.T, subs []*agentv1.SubsystemStatus, name string) *agentv1.SubsystemStatus {
	t.Helper()
	for _, s := range subs {
		if s.GetName() == name {
			return s
		}
	}
	t.Fatalf("subsystem %q not found in HealthCheck response; got names: %v",
		name, subsysNames(subs))
	return nil // unreachable
}

// subsysNames extracts a slice of name strings for diagnostic messages.
func subsysNames(subs []*agentv1.SubsystemStatus) []string {
	names := make([]string, len(subs))
	for i, s := range subs {
		names[i] = s.GetName()
	}
	return names
}

// makeZFSDir creates a directory at path, simulating a loaded ZFS kernel module.
func makeZFSDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("create fake ZFS module dir %q: %v", path, err)
	}
}

// makeNvmetDir creates the nvmet/ subdirectory inside cfgRoot,
// simulating a mounted nvmet configfs tree.
func makeNvmetDir(t *testing.T, cfgRoot string) {
	t.Helper()
	nvmetDir := filepath.Join(cfgRoot, "nvmet")
	if err := os.MkdirAll(nvmetDir, 0o750); err != nil {
		t.Fatalf("create nvmet dir: %v", err)
	}
}

// Test cases.

// TestHealthCheck_Structured_AllHealthy verifies the structured field values
// produced by HealthCheck when every component is fully operational:
//   - ZFS module path exists          → zfs_module:   Healthy=true, Message non-empty
//   - nvmet configfs dir exists       → nvmet_configfs: Healthy=true, Message non-empty
//   - backend.Capacity() succeeds     → pool/tank:    Healthy=true, Message non-empty
//   - top-level Healthy flag          → true
func TestHealthCheck_Structured_AllHealthy(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{
		capacityTotal:     20 << 30,
		capacityAvailable: 15 << 30,
	}
	srv, cfgRoot := newExportTestServer(t, mb)

	// Simulate loaded ZFS module.
	fakeZFSPath := filepath.Join(t.TempDir(), "zfs")
	makeZFSDir(t, fakeZFSPath)
	agent.SetServerSysModuleZFSPath(t, srv, fakeZFSPath)

	// Simulate mounted nvmet configfs.
	makeNvmetDir(t, cfgRoot)

	resp := healthCheckResp(t, srv)

	// --- top-level healthy flag ---
	if !resp.GetHealthy() {
		t.Error("resp.Healthy = false, want true when all components are healthy")
	}

	// --- zfs_module structured fields ---
	zfsSub := requireSubsystem(t, resp.GetSubsystems(), "zfs_module")
	if !zfsSub.GetHealthy() {
		t.Errorf("zfs_module.Healthy = false, want true; Message: %q", zfsSub.GetMessage())
	}
	if zfsSub.GetMessage() == "" {
		t.Error("zfs_module.Message must be non-empty when healthy")
	}

	// --- nvmet_configfs structured fields ---
	nvmetSub := requireSubsystem(t, resp.GetSubsystems(), "nvmet_configfs")
	if !nvmetSub.GetHealthy() {
		t.Errorf("nvmet_configfs.Healthy = false, want true; Message: %q", nvmetSub.GetMessage())
	}
	if nvmetSub.GetMessage() == "" {
		t.Error("nvmet_configfs.Message must be non-empty when healthy")
	}

	// --- pool/tank structured fields ---
	poolSub := requireSubsystem(t, resp.GetSubsystems(), "pool/"+testPool)
	if !poolSub.GetHealthy() {
		t.Errorf("pool/%s.Healthy = false, want true; Message: %q", testPool, poolSub.GetMessage())
	}
	if poolSub.GetMessage() == "" {
		t.Errorf("pool/%s.Message must be non-empty when healthy", testPool)
	}
}

// TestHealthCheck_Structured_ZFSModuleAbsent verifies the structured field
// values produced by HealthCheck when the ZFS kernel module is not loaded
// (the sysModuleZFSPath does not exist):
//   - zfs_module: Healthy=false, Name="zfs_module", Message non-empty
//   - top-level Healthy flag: false
func TestHealthCheck_Structured_ZFSModuleAbsent(t *testing.T) {
	t.Parallel()

	srv, _ := newExportTestServer(t, &mockBackend{})

	// Point at a non-existent path — no ZFS module.
	agent.SetServerSysModuleZFSPath(t, srv, filepath.Join(t.TempDir(), "zfs-missing"))

	resp := healthCheckResp(t, srv)

	// --- top-level healthy flag must be false ---
	if resp.GetHealthy() {
		t.Error("resp.Healthy = true, want false when ZFS module is absent")
	}

	// --- zfs_module structured fields ---
	sub := requireSubsystem(t, resp.GetSubsystems(), "zfs_module")

	if sub.GetName() != "zfs_module" {
		t.Errorf("zfs_module.Name = %q, want \"zfs_module\"", sub.GetName())
	}
	if sub.GetHealthy() {
		t.Error("zfs_module.Healthy = true, want false when module path is absent")
	}
	if sub.GetMessage() == "" {
		t.Error("zfs_module.Message must be non-empty when unhealthy")
	}
	// Message should mention the failure context (not just a generic error).
	if !strings.Contains(strings.ToLower(sub.GetMessage()), "zfs") {
		t.Errorf("zfs_module.Message %q expected to reference \"zfs\"", sub.GetMessage())
	}
}

// TestHealthCheck_Structured_NvmetConfigfsAbsent verifies the structured field
// values when the nvmet configfs directory is not mounted (the directory under
// configfsRoot/nvmet does not exist):
//   - nvmet_configfs: Healthy=false, Name="nvmet_configfs", Message non-empty
//   - top-level Healthy flag: false
func TestHealthCheck_Structured_NvmetConfigfsAbsent(t *testing.T) {
	t.Parallel()

	// newExportTestServer creates a fresh temp dir without an nvmet/ subdir,
	// so the nvmet configfs check will fail immediately.
	srv, _ := newExportTestServer(t, &mockBackend{})

	resp := healthCheckResp(t, srv)

	// --- top-level healthy flag must be false ---
	if resp.GetHealthy() {
		t.Error("resp.Healthy = true, want false when nvmet configfs is absent")
	}

	// --- nvmet_configfs structured fields ---
	sub := requireSubsystem(t, resp.GetSubsystems(), "nvmet_configfs")

	if sub.GetName() != "nvmet_configfs" {
		t.Errorf("nvmet_configfs.Name = %q, want \"nvmet_configfs\"", sub.GetName())
	}
	if sub.GetHealthy() {
		t.Error("nvmet_configfs.Healthy = true, want false when nvmet dir is absent")
	}
	if sub.GetMessage() == "" {
		t.Error("nvmet_configfs.Message must be non-empty when unhealthy")
	}
	// Message should mention the failure context.
	if !strings.Contains(strings.ToLower(sub.GetMessage()), "nvmet") {
		t.Errorf("nvmet_configfs.Message %q expected to reference \"nvmet\"", sub.GetMessage())
	}
}

// TestHealthCheck_Structured_PoolCapacityFailure verifies the structured field
// values when at least one pool's backend.Capacity() call returns an error:
//   - pool/tank: Healthy=false, Name="pool/tank", Message non-empty
//   - top-level Healthy flag: false
func TestHealthCheck_Structured_PoolCapacityFailure(t *testing.T) {
	t.Parallel()

	mb := &mockBackend{
		capacityErr: errors.New("pool degraded: checksum errors"),
	}
	srv, _ := newExportTestServer(t, mb)

	resp := healthCheckResp(t, srv)

	// --- top-level healthy flag must be false ---
	if resp.GetHealthy() {
		t.Error("resp.Healthy = true, want false when pool Capacity() fails")
	}

	// --- pool/tank structured fields ---
	const wantPoolName = "pool/" + testPool
	sub := requireSubsystem(t, resp.GetSubsystems(), wantPoolName)

	if sub.GetName() != wantPoolName {
		t.Errorf("pool subsystem.Name = %q, want %q", sub.GetName(), wantPoolName)
	}
	if sub.GetHealthy() {
		t.Error("pool/tank.Healthy = true, want false when Capacity() returns error")
	}
	if sub.GetMessage() == "" {
		t.Error("pool/tank.Message must be non-empty when unhealthy")
	}
	// Message should mention the pool name for operator diagnostics.
	if !strings.Contains(sub.GetMessage(), testPool) {
		t.Errorf("pool/tank.Message %q expected to mention pool name %q",
			sub.GetMessage(), testPool)
	}
}

// TestHealthCheck_Structured_AgentMetadata verifies that the HealthCheck
// response always populates the non-subsystem metadata fields regardless of
// individual component health.
func TestHealthCheck_Structured_AgentMetadata(t *testing.T) {
	t.Parallel()

	srv, _ := newExportTestServer(t, &mockBackend{})

	resp := healthCheckResp(t, srv)

	if resp.GetAgentVersion() == "" {
		t.Error("AgentVersion must be non-empty in every HealthCheck response")
	}
	if resp.GetCheckedAt() == nil {
		t.Error("CheckedAt timestamp must be set in every HealthCheck response")
	}
}

// TestHealthCheck_Structured_SubsystemCount verifies that the response always
// contains exactly two core entries (zfs_module + nvmet_configfs) plus one
// entry per registered pool.
func TestHealthCheck_Structured_SubsystemCount(t *testing.T) {
	t.Parallel()

	srv, _ := newExportTestServer(t, &mockBackend{})

	resp := healthCheckResp(t, srv)

	// 1 pool ("tank") registered, so 2 core + 1 pool = 3 total.
	const wantTotal = 3
	if got := len(resp.GetSubsystems()); got != wantTotal {
		t.Errorf("len(Subsystems) = %d, want %d; names: %v",
			got, wantTotal, subsysNames(resp.GetSubsystems()))
	}
}
