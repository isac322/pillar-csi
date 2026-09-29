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

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/csi"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// resourceCollectTimeout bounds the cache reads of one scrape.
const resourceCollectTimeout = 10 * time.Second

// Kind label values of pillar_csi_resource_status_condition.
const (
	kindPillarAgent = "PillarAgent"
	kindPillarStore = "PillarStore"
)

// Phase label values of pillar_csi_volumes. Deleting is status.deleting,
// whatever the recorded phase.
const volumePhaseDeleting = "Deleting"

var volumePhases = []string{
	string(pillarcsiv1alpha1.PillarVolumeStatePhaseProvisioning),
	string(pillarcsiv1alpha1.PillarVolumeStatePhaseCreatePartial),
	string(pillarcsiv1alpha1.PillarVolumeStatePhaseReady),
	volumePhaseDeleting,
}

// exportUnreconciledReasons is the closed reason set of
// pillar_csi_volumes_export_unreconciled: the False reasons of the
// ExportReconciled condition written by internal/csi/export_resync.go.
var exportUnreconciledReasons = []string{
	"ExportSpecMissing",
	"AgentUnavailable",
	"ReconcileFailed",
	"StaleGeneration",
}

// Label names shared by several resource metrics.
const (
	labelAgent   = "agent"
	labelBackend = "backend"
	labelReason  = "reason"
)

var (
	resourceConditionDesc = prometheus.NewDesc(
		"pillar_csi_resource_status_condition",
		"Current status of each PillarAgent and PillarStore condition (1 for the current status). Leader only.",
		[]string{"kind", "name", "type", "status", labelReason}, nil)
	storeInfoDesc = prometheus.NewDesc(
		"pillar_csi_store_info",
		"PillarStore to agent, pool and backend mapping (constant 1). Leader only.",
		[]string{"store", labelAgent, "pool", labelBackend}, nil)
	volumesDesc = prometheus.NewDesc(
		"pillar_csi_volumes",
		"PillarVolumeStates by agent, backend and lifecycle phase. Leader only.",
		[]string{labelAgent, labelBackend, "phase"}, nil)
	exportUnreconciledDesc = prometheus.NewDesc(
		"pillar_csi_volumes_export_unreconciled",
		"PillarVolumeStates whose ExportReconciled condition is False, by agent and reason. Leader only.",
		[]string{labelAgent, labelReason}, nil)
)

// ResourceMetricsCollector exports pillar_csi_resource_status_condition,
// pillar_csi_store_info, pillar_csi_volumes and
// pillar_csi_volumes_export_unreconciled from the manager cache at scrape
// time. It emits nothing until elected is closed, so only the leader reports
// them and a failover does not double the fleet-wide sums.
type ResourceMetricsCollector struct {
	reader  client.Reader
	elected <-chan struct{}
}

var _ prometheus.Collector = (*ResourceMetricsCollector)(nil)

// NewResourceMetricsCollector returns a collector reading from reader
// (normally the manager's cached client) once elected (normally
// Manager.Elected()) is closed.
func NewResourceMetricsCollector(reader client.Reader, elected <-chan struct{}) *ResourceMetricsCollector {
	return &ResourceMetricsCollector{reader: reader, elected: elected}
}

// Describe implements prometheus.Collector.
func (*ResourceMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- resourceConditionDesc
	ch <- storeInfoDesc
	ch <- volumesDesc
	ch <- exportUnreconciledDesc
}

// Collect implements prometheus.Collector. A failed cache read is reported
// as an invalid metric, which fails the scrape visibly.
func (c *ResourceMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	select {
	case <-c.elected:
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), resourceCollectTimeout)
	defer cancel()

	err := c.collectAgents(ctx, ch)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(resourceConditionDesc, err)
	}
	err = c.collectStores(ctx, ch)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(storeInfoDesc, err)
	}
	err = c.collectVolumes(ctx, ch)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(volumesDesc, err)
	}
}

func (c *ResourceMetricsCollector) collectAgents(ctx context.Context, ch chan<- prometheus.Metric) error {
	list := &pillarcsiv1alpha1.PillarAgentList{}
	err := c.reader.List(ctx, list)
	if err != nil {
		return fmt.Errorf("list PillarAgents for metrics: %w", err)
	}
	for i := range list.Items {
		emitConditions(ch, kindPillarAgent, list.Items[i].Name, list.Items[i].Status.Conditions)
	}
	return nil
}

func (c *ResourceMetricsCollector) collectStores(ctx context.Context, ch chan<- prometheus.Metric) error {
	list := &pillarcsiv1alpha1.PillarStoreList{}
	err := c.reader.List(ctx, list)
	if err != nil {
		return fmt.Errorf("list PillarStores for metrics: %w", err)
	}
	for i := range list.Items {
		store := &list.Items[i]
		emitConditions(ch, kindPillarStore, store.Name, store.Status.Conditions)
		ch <- prometheus.MustNewConstMetric(storeInfoDesc, prometheus.GaugeValue, 1,
			store.Name, store.Spec.AgentRef, store.Spec.Backend.PoolName(), string(store.Spec.Backend.Kind()))
	}
	return nil
}

// volumeKey groups pillar_csi_volumes.
type volumeKey struct {
	agent, backend string
}

func (c *ResourceMetricsCollector) collectVolumes(ctx context.Context, ch chan<- prometheus.Metric) error {
	list := &pillarcsiv1alpha1.PillarVolumeStateList{}
	err := c.reader.List(ctx, list)
	if err != nil {
		return fmt.Errorf("list PillarVolumeStates for metrics: %w", err)
	}
	volumes := map[volumeKey]map[string]int{}
	unreconciled := map[string]map[string]int{}
	for i := range list.Items {
		pvs := &list.Items[i]
		key := volumeKey{agent: pvs.Spec.AgentRef, backend: pvs.Spec.BackendType}
		if volumes[key] == nil {
			volumes[key] = map[string]int{}
		}
		volumes[key][volumePhaseLabel(pvs)]++

		if unreconciled[pvs.Spec.AgentRef] == nil {
			unreconciled[pvs.Spec.AgentRef] = map[string]int{}
		}
		for _, cond := range pvs.Status.Conditions {
			if cond.Type == csi.ConditionExportReconciled && cond.Status == metav1.ConditionFalse {
				unreconciled[pvs.Spec.AgentRef][closedLabel(exportUnreconciledReasons, cond.Reason)]++
			}
		}
	}
	// Every observed agent reports every phase and reason, so a count that
	// drops to zero reads 0 instead of disappearing.
	for key, counts := range volumes {
		for _, phase := range withOther(volumePhases, counts) {
			ch <- prometheus.MustNewConstMetric(volumesDesc, prometheus.GaugeValue, float64(counts[phase]),
				key.agent, key.backend, phase)
		}
	}
	for agent, counts := range unreconciled {
		for _, reason := range withOther(exportUnreconciledReasons, counts) {
			ch <- prometheus.MustNewConstMetric(exportUnreconciledDesc, prometheus.GaugeValue,
				float64(counts[reason]), agent, reason)
		}
	}
	return nil
}

// emitConditions emits one pillar_csi_resource_status_condition series per
// condition, for its current status.
func emitConditions(ch chan<- prometheus.Metric, kind, name string, conds []metav1.Condition) {
	for _, cond := range conds {
		ch <- prometheus.MustNewConstMetric(resourceConditionDesc, prometheus.GaugeValue, 1,
			kind, name, cond.Type, string(cond.Status), cond.Reason)
	}
}

// volumePhaseLabel maps a PillarVolumeState to the phase label of
// pillar_csi_volumes.
func volumePhaseLabel(pvs *pillarcsiv1alpha1.PillarVolumeState) string {
	if pvs.Status.Deleting {
		return volumePhaseDeleting
	}
	return closedLabel(volumePhases, string(pvs.Status.Phase))
}

// closedLabel returns v when it is in set, else telemetry.LabelOther.
func closedLabel(set []string, v string) string {
	if slices.Contains(set, v) {
		return v
	}
	return telemetry.LabelOther
}

// withOther returns set, plus telemetry.LabelOther when counts has it.
func withOther(set []string, counts map[string]int) []string {
	if counts[telemetry.LabelOther] == 0 {
		return set
	}
	return append(slices.Clone(set), telemetry.LabelOther)
}
