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

package health_test

import (
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agent/health"
)

// ComponentStatus constructor helpers.

func TestOK_IsHealthy(t *testing.T) {
	t.Parallel()
	cs := health.OK("module loaded")
	if !cs.Healthy {
		t.Error("OK() should produce Healthy=true")
	}
	if cs.Message != "module loaded" {
		t.Errorf("Message = %q, want %q", cs.Message, "module loaded")
	}
}

func TestDegraded_IsUnhealthy(t *testing.T) {
	t.Parallel()
	cs := health.Degraded("module not found")
	if cs.Healthy {
		t.Error("Degraded() should produce Healthy=false")
	}
	if cs.Message != "module not found" {
		t.Errorf("Message = %q, want %q", cs.Message, "module not found")
	}
}

// HealthStatus.AllHealthy tests.

func TestAllHealthy_AllOK(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("configfs mounted"),
		PerPoolStatus: []health.PoolStatus{
			{Pool: "tank", Status: health.OK("pool healthy")},
		},
	}
	if !hs.AllHealthy() {
		t.Error("AllHealthy() should be true when all components are healthy")
	}
}

func TestAllHealthy_NvmetDegraded(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.Degraded("nvmet dir missing"),
	}
	if hs.AllHealthy() {
		t.Error("AllHealthy() should be false when NvmetConfigfs is degraded")
	}
}

func TestAllHealthy_PoolDegraded(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("configfs mounted"),
		PerPoolStatus: []health.PoolStatus{
			{Pool: "hot-data", Status: health.OK("pool healthy")},
			{Pool: "cold-data", Status: health.Degraded("pool degraded")},
		},
	}
	if hs.AllHealthy() {
		t.Error("AllHealthy() should be false when any pool is degraded")
	}
}

func TestAllHealthy_EmptyPools(t *testing.T) {
	t.Parallel()
	// Zero PerPoolStatus entries should not affect overall health.
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("configfs mounted"),
	}
	if !hs.AllHealthy() {
		t.Error("AllHealthy() should be true with no pool entries if all other components are healthy")
	}
}

// HealthStatus.ToProtoSubsystems tests.

func TestToProtoSubsystems_AlwaysContainsCoreEntries(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.Degraded("nvmet dir missing"),
	}

	subs := hs.ToProtoSubsystems()

	// Must always emit exactly one core entry (nvmet_configfs) plus pool entries.
	if len(subs) != 1 {
		t.Fatalf("len(subsystems) = %d, want 1", len(subs))
	}
}

func TestToProtoSubsystems_NameConventions(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("configfs mounted"),
		PerPoolStatus: []health.PoolStatus{
			{Pool: "tank", Status: health.OK("pool healthy")},
		},
	}

	subs := hs.ToProtoSubsystems()

	wantNames := []string{"nvmet_configfs", "pool/tank"}
	if len(subs) != len(wantNames) {
		t.Fatalf("len(subsystems) = %d, want %d", len(subs), len(wantNames))
	}
	for i, want := range wantNames {
		if subs[i].GetName() != want {
			t.Errorf("subsystems[%d].Name = %q, want %q", i, subs[i].GetName(), want)
		}
	}
}

func TestToProtoSubsystems_HealthyFieldMirrored(t *testing.T) {
	t.Parallel()
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("mounted"),
		PerPoolStatus: []health.PoolStatus{
			{Pool: "tank", Status: health.Degraded("io error")},
		},
	}

	subs := hs.ToProtoSubsystems()

	// nvmet_configfs → healthy
	if !subs[0].GetHealthy() {
		t.Error("nvmet_configfs subsystem should be healthy")
	}
	// pool/tank → unhealthy
	if subs[1].GetHealthy() {
		t.Error("pool/tank subsystem should be unhealthy")
	}
}

func TestToProtoSubsystems_MessageMirrored(t *testing.T) {
	t.Parallel()
	const nvmetMsg = "nvmet configfs directory accessible."

	hs := health.HealthStatus{
		NvmetConfigfs: health.OK(nvmetMsg),
	}

	subs := hs.ToProtoSubsystems()

	if subs[0].GetMessage() != nvmetMsg {
		t.Errorf("nvmet_configfs message = %q, want %q", subs[0].GetMessage(), nvmetMsg)
	}
}

func TestToProtoSubsystems_MultiplePoolsPreserveOrder(t *testing.T) {
	t.Parallel()
	pools := []health.PoolStatus{
		{Pool: "alpha", Status: health.OK("ok")},
		{Pool: "beta", Status: health.OK("ok")},
		{Pool: "gamma", Status: health.Degraded("degraded")},
	}
	hs := health.HealthStatus{
		NvmetConfigfs: health.OK("mounted"),
		PerPoolStatus: pools,
	}

	subs := hs.ToProtoSubsystems()

	// 1 core + 3 pool entries = 4 total
	if len(subs) != 4 {
		t.Fatalf("len(subsystems) = %d, want 4", len(subs))
	}
	wantPoolNames := []string{"pool/alpha", "pool/beta", "pool/gamma"}
	for i, want := range wantPoolNames {
		got := subs[i+1].GetName()
		if got != want {
			t.Errorf("subsystems[%d].Name = %q, want %q", i+1, got, want)
		}
	}
}
