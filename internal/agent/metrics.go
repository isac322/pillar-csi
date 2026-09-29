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
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/singleflight"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// fencingDecisions is M7, pillar_csi_agent_fencing_decisions_total: one
// increment per fencing admit or recheck.
var fencingDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pillar_csi_agent_fencing_decisions_total",
	Help: "Fencing admit and recheck outcomes of mutating agent RPCs, by fencing operation and decision.",
}, []string{"fence_op", "decision"})

// Pool collector timing (M1-M5).
const (
	// A capacity refresh serves scrapes for poolCapacityTTL.
	poolCapacityTTL = 15 * time.Second
	// A provisioned-bytes refresh serves scrapes for poolProvisionedTTL;
	// listing every volume is costlier than a capacity probe.
	poolProvisionedTTL = 5 * time.Minute
	// One refresh, and so one scrape, is bounded by poolRefreshTimeout.
	poolRefreshTimeout = 10 * time.Second
)

// Subsystem label values of pillar_csi_agent_subsystem_healthy (M5).
const (
	subsystemNvmetConfigfs = "nvmet_configfs"
	subsystemPool          = "pool"
)

// RegisterMetrics registers the agent's metrics on reg: the fencing decision
// counter (M7), the nvmet configfs error counter (M8) and the scrape-time
// pool collector (M1-M5) over s's backends.
func (s *Server) RegisterMetrics(reg prometheus.Registerer) error {
	err := reg.Register(fencingDecisions)
	if err != nil {
		return fmt.Errorf("register pillar_csi_agent_fencing_decisions_total: %w", err)
	}
	err = nvmeof.RegisterMetrics(reg)
	if err != nil {
		return fmt.Errorf("register nvmeof metrics: %w", err)
	}
	err = reg.Register(newPoolCollector(s.backends, func() bool { return s.checkNvmetConfigfs().Healthy }))
	if err != nil {
		return fmt.Errorf("register pool collector: %w", err)
	}
	return nil
}

// backendLabel returns the backend label value (the backend routing token of
// CSI volume IDs) for t.
func backendLabel(t agentv1.BackendType) string {
	switch t {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:
		return "zfs-zvol"
	case agentv1.BackendType_BACKEND_TYPE_LVM:
		return "lvm-lv"
	default:
		return telemetry.LabelOther
	}
}

// poolCapacity is one pool's result of a capacity refresh.  When ok is false
// the probe failed or timed out, and the pool only reports
// subsystem_healthy=0.
type poolCapacity struct {
	pool        string
	backendType string
	ok          bool
	details     backend.CapacityDetails
}

// poolProvisioned is one pool's result of a provisioned-bytes refresh.
type poolProvisioned struct {
	pool        string
	backendType string
	bytes       int64
}

// capacitySnapshot is the cached result of one capacity refresh.
type capacitySnapshot struct {
	at           time.Time
	nvmetHealthy bool
	pools        []poolCapacity
}

// provisionedSnapshot is the cached result of one provisioned-bytes refresh.
type provisionedSnapshot struct {
	at    time.Time
	pools []poolProvisioned
}

// poolCollector is the scrape-time collector of M1-M5.  A scrape reads a TTL
// cache; a stale cache is refreshed once (singleflight) under a timeout, and a
// pool whose probe fails or times out has its value series dropped.  Probes
// run once per pool per refresh, never once per series.
type poolCollector struct {
	backends     map[string]backend.VolumeBackend
	nvmetHealthy func() bool

	capacityTTL    time.Duration
	provisionedTTL time.Duration
	timeout        time.Duration

	group       singleflight.Group
	mu          sync.Mutex
	capacity    *capacitySnapshot
	provisioned *provisionedSnapshot

	sizeDesc        *prometheus.Desc
	availableDesc   *prometheus.Desc
	provisionedDesc *prometheus.Desc
	metadataDesc    *prometheus.Desc
	healthyDesc     *prometheus.Desc
}

func newPoolCollector(backends map[string]backend.VolumeBackend, nvmetHealthy func() bool) *poolCollector {
	poolLabels := []string{"pool", "backend"}
	return &poolCollector{
		backends:       backends,
		nvmetHealthy:   nvmetHealthy,
		capacityTTL:    poolCapacityTTL,
		provisionedTTL: poolProvisionedTTL,
		timeout:        poolRefreshTimeout,
		sizeDesc: prometheus.NewDesc("pillar_csi_pool_size_bytes",
			"Total bytes of the storage boundary the agent provisions volumes in (ZFS dataset, LVM VG or thin pool).",
			poolLabels, nil),
		availableDesc: prometheus.NewDesc("pillar_csi_pool_available_bytes",
			"Bytes still available to new volumes in the pool, as CreateVolume sees them.",
			poolLabels, nil),
		provisionedDesc: prometheus.NewDesc("pillar_csi_pool_provisioned_bytes",
			"Summed virtual size of the volumes in a pool that can overcommit (ZFS, LVM thin).",
			poolLabels, nil),
		metadataDesc: prometheus.NewDesc("pillar_csi_pool_thin_metadata_used_ratio",
			"Used fraction (0-1) of the LVM thin pool's metadata.",
			poolLabels, nil),
		healthyDesc: prometheus.NewDesc("pillar_csi_agent_subsystem_healthy",
			"1 when the agent subsystem (nvmet configfs, or a pool) passed its last probe, else 0.",
			[]string{"subsystem", "pool"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.sizeDesc
	ch <- c.availableDesc
	ch <- c.provisionedDesc
	ch <- c.metadataDesc
	ch <- c.healthyDesc
}

// Collect implements prometheus.Collector.
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	var (
		wg          sync.WaitGroup
		capacity    *capacitySnapshot
		provisioned *provisionedSnapshot
	)
	wg.Go(func() { capacity = c.capacitySnapshot() })
	wg.Go(func() { provisioned = c.provisionedSnapshot() })
	wg.Wait()

	ch <- prometheus.MustNewConstMetric(c.healthyDesc, prometheus.GaugeValue,
		boolGauge(capacity.nvmetHealthy), subsystemNvmetConfigfs, "")
	for _, p := range capacity.pools {
		ch <- prometheus.MustNewConstMetric(c.healthyDesc, prometheus.GaugeValue,
			boolGauge(p.ok), subsystemPool, p.pool)
		if !p.ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue,
			float64(p.details.TotalBytes), p.pool, p.backendType)
		ch <- prometheus.MustNewConstMetric(c.availableDesc, prometheus.GaugeValue,
			float64(p.details.AvailableBytes), p.pool, p.backendType)
		if p.details.HasThinMetadata {
			ch <- prometheus.MustNewConstMetric(c.metadataDesc, prometheus.GaugeValue,
				p.details.ThinMetadataUsedRatio, p.pool, p.backendType)
		}
	}
	for _, p := range provisioned.pools {
		ch <- prometheus.MustNewConstMetric(c.provisionedDesc, prometheus.GaugeValue,
			float64(p.bytes), p.pool, p.backendType)
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// capacitySnapshot returns the cached capacity refresh, refreshing it first
// when it is older than the TTL.
func (c *poolCollector) capacitySnapshot() *capacitySnapshot {
	return cachedRefresh(c, "capacity", c.capacityTTL, &c.capacity,
		func(s *capacitySnapshot) time.Time { return s.at }, c.refreshCapacity)
}

// provisionedSnapshot returns the cached provisioned-bytes refresh,
// refreshing it first when it is older than its TTL.
func (c *poolCollector) provisionedSnapshot() *provisionedSnapshot {
	return cachedRefresh(c, "provisioned", c.provisionedTTL, &c.provisioned,
		func(s *provisionedSnapshot) time.Time { return s.at }, c.refreshProvisioned)
}

// cachedRefresh returns *slot while it is younger than ttl.  Otherwise it
// runs refresh, once for all concurrent scrapes (singleflight under key),
// stores the result in *slot and returns it.  Slot is guarded by c.mu.
func cachedRefresh[S any](
	c *poolCollector,
	key string,
	ttl time.Duration,
	slot **S,
	refreshedAt func(*S) time.Time,
	refresh func() *S,
) *S {
	c.mu.Lock()
	cached := *slot
	c.mu.Unlock()
	if cached != nil && time.Since(refreshedAt(cached)) < ttl {
		return cached
	}
	res := <-c.group.DoChan(key, func() (any, error) {
		snap := refresh()
		c.mu.Lock()
		*slot = snap
		c.mu.Unlock()
		return snap, nil
	})
	snap, ok := res.Val.(*S)
	if !ok {
		// Should never happen: refresh only returns *S.
		return new(S)
	}
	return snap
}

// refreshCapacity probes every pool concurrently under the refresh timeout.
// A probe still running at the deadline is abandoned: its pool reports
// unhealthy and drops its value series, and its goroutine finishes on its own
// once the probe honors the canceled context.
func (c *poolCollector) refreshCapacity() *capacitySnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	results := make(chan poolCapacity, len(c.backends))
	for name, b := range c.backends {
		go func() {
			details, err := capacityDetails(ctx, b)
			results <- poolCapacity{
				pool: name, backendType: backendLabel(b.Type()), ok: err == nil, details: details,
			}
		}()
	}

	snap := &capacitySnapshot{
		nvmetHealthy: c.nvmetHealthy(),
		pools:        make([]poolCapacity, 0, len(c.backends)),
	}
	got := make(map[string]poolCapacity, len(c.backends))
wait:
	for range c.backends {
		select {
		case r := <-results:
			got[r.pool] = r
		case <-ctx.Done():
			break wait
		}
	}
	for name, b := range c.backends {
		r, ok := got[name]
		if !ok {
			r = poolCapacity{pool: name, backendType: backendLabel(b.Type())}
		}
		snap.pools = append(snap.pools, r)
	}
	snap.at = time.Now()
	return snap
}

// capacityDetails probes b's capacity, with thin-pool metadata when b
// reports it.
func capacityDetails(ctx context.Context, b backend.VolumeBackend) (backend.CapacityDetails, error) {
	if d, ok := b.(backend.CapacityDetailer); ok {
		details, err := d.CapacityDetails(ctx)
		if err != nil {
			return backend.CapacityDetails{}, fmt.Errorf("capacity details: %w", err)
		}
		return details, nil
	}
	total, avail, err := b.Capacity(ctx)
	if err != nil {
		return backend.CapacityDetails{}, fmt.Errorf("capacity: %w", err)
	}
	return backend.CapacityDetails{TotalBytes: total, AvailableBytes: avail}, nil
}

// refreshProvisioned sums the provisioned bytes of every pool that can
// overcommit, concurrently under the refresh timeout.  A failed or timed-out
// pool is omitted.
func (c *poolCollector) refreshProvisioned() *provisionedSnapshot {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	type result struct {
		p  poolProvisioned
		ok bool
	}
	results := make(chan result, len(c.backends))
	pending := 0
	for name, b := range c.backends {
		r, ok := b.(backend.ProvisionedBytesReporter)
		if !ok {
			continue
		}
		pending++
		go func() {
			bytes, applies, err := r.ProvisionedBytes(ctx)
			results <- result{
				p:  poolProvisioned{pool: name, backendType: backendLabel(b.Type()), bytes: bytes},
				ok: applies && err == nil,
			}
		}()
	}

	snap := &provisionedSnapshot{pools: make([]poolProvisioned, 0, pending)}
wait:
	for ; pending > 0; pending-- {
		select {
		case r := <-results:
			if r.ok {
				snap.pools = append(snap.pools, r.p)
			}
		case <-ctx.Done():
			break wait
		}
	}
	snap.at = time.Now()
	return snap
}
