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

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// stallingBackend blocks Capacity, ignoring ctx like a hung command, until
// release is closed, while stall is set.
type stallingBackend struct {
	*drainTestBackend
	stall   atomic.Bool
	release chan struct{}
}

func (b *stallingBackend) Capacity(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	if b.stall.Load() {
		<-b.release
	}
	return b.drainTestBackend.Capacity(ctx)
}

// scrapePools gathers reg and returns the pool collector's values keyed by
// "<metric>/<pool>".
func scrapePools(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	values := map[string]float64{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			pool := ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "pool" {
					pool = lp.GetValue()
				}
			}
			values[mf.GetName()+"/"+pool] = m.GetGauge().GetValue()
		}
	}
	return values
}

// A pool whose capacity probe hangs past the refresh timeout does not hold
// the scrape: the scrape returns in time without that pool's value series
// (and reports it unhealthy), and a later successful refresh restores them.
func TestPoolCollector_TimedOutPoolDroppedThenRestored(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond

	slow := &stallingBackend{
		drainTestBackend: &drainTestBackend{capacityTotal: 4 << 30, capacityAvailable: 1 << 30},
		release:          make(chan struct{}),
	}
	slow.stall.Store(true)
	t.Cleanup(func() { close(slow.release) })
	fast := &drainTestBackend{capacityTotal: 8 << 30, capacityAvailable: 2 << 30}

	c := newPoolCollector(
		map[string]backend.VolumeBackend{"slow": slow, "fast": fast},
		func() bool { return true },
	)
	c.timeout = timeout
	c.capacityTTL = 0
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	start := time.Now()
	values := scrapePools(t, reg)
	if elapsed := time.Since(start); elapsed > 10*timeout {
		t.Fatalf("scrape took %s with a hung pool; want it bounded by the %s refresh timeout", elapsed, timeout)
	}
	for _, name := range []string{"pillar_csi_pool_size_bytes/slow", "pillar_csi_pool_available_bytes/slow"} {
		if v, ok := values[name]; ok {
			t.Errorf("%s = %v present after a timed-out probe, want absent", name, v)
		}
	}
	if got := values["pillar_csi_agent_subsystem_healthy/slow"]; got != 0 {
		t.Errorf("subsystem_healthy{pool=slow} = %v after a timed-out probe, want 0", got)
	}
	if got := values["pillar_csi_pool_size_bytes/fast"]; got != 8<<30 {
		t.Errorf("pillar_csi_pool_size_bytes{pool=fast} = %v, want %v", got, 8<<30)
	}

	slow.stall.Store(false)
	values = scrapePools(t, reg)
	if got := values["pillar_csi_pool_size_bytes/slow"]; got != 4<<30 {
		t.Errorf("pillar_csi_pool_size_bytes{pool=slow} = %v after a successful refresh, want %v", got, 4<<30)
	}
	if got := values["pillar_csi_pool_available_bytes/slow"]; got != 1<<30 {
		t.Errorf("pillar_csi_pool_available_bytes{pool=slow} = %v after a successful refresh, want %v", got, 1<<30)
	}
	if got := values["pillar_csi_agent_subsystem_healthy/slow"]; got != 1 {
		t.Errorf("subsystem_healthy{pool=slow} = %v after a successful refresh, want 1", got)
	}
}
