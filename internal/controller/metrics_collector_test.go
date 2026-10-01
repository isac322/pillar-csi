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
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/csi"
)

func metricsTestVolume(
	name string,
	phase pillarcsiv1alpha1.PillarVolumeStatePhase,
	deleting bool,
	conds ...metav1.Condition,
) *pillarcsiv1alpha1.PillarVolumeState {
	return &pillarcsiv1alpha1.PillarVolumeState{
		Name: name,
		Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
			VolumeID:      "agent-a/nvmeof-tcp/zfs-zvol/tank/" + name,
			AgentVolumeID: "tank/" + name,
			AgentRef:      "agent-a",
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
		},
		Status: pillarcsiv1alpha1.PillarVolumeStateStatus{Phase: phase, Deleting: deleting, Conditions: conds},
	}
}

// gatherPillar gathers reg and returns the pillar_csi_* series as
// "name{label=value,...}" -> value.
func gatherPillar(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]float64{}
	for _, mf := range families {
		if !strings.HasPrefix(mf.GetName(), "pillar_csi_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := make([]string, 0, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labels = append(labels, lp.GetName()+"="+lp.GetValue())
			}
			got[mf.GetName()+"{"+strings.Join(labels, ",")+"}"] = m.GetGauge().GetValue()
		}
	}
	return got
}

// TestResourceMetricsCollectorLeaderGating checks that no M11-M14 series is
// exported before the manager is elected, and the counts (Deleting included)
// after.
func TestResourceMetricsCollectorLeaderGating(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add pillar-csi scheme: %v", err)
	}

	agent := &pillarcsiv1alpha1.PillarAgent{
		Name: "agent-a",
		Status: pillarcsiv1alpha1.PillarAgentStatus{Conditions: []metav1.Condition{{
			Type: "AgentConnected", Status: metav1.ConditionFalse, Reason: "HealthCheckFailed",
		}}},
	}
	store := &pillarcsiv1alpha1.PillarStore{
		Name: "store-a",
		Spec: pillarcsiv1alpha1.PillarStoreSpec{
			AgentRef: "agent-a",
			Backend:  pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{Pool: "tank"}},
		},
	}
	unreconciled := metav1.Condition{
		Type: csi.ConditionExportReconciled, Status: metav1.ConditionFalse, Reason: "AgentUnavailable",
	}
	objs := []client.Object{
		agent, store,
		metricsTestVolume("pvc-ready", pillarcsiv1alpha1.PillarVolumeStatePhaseReady, false),
		metricsTestVolume("pvc-ready-unreconciled", pillarcsiv1alpha1.PillarVolumeStatePhaseReady, false, unreconciled),
		metricsTestVolume("pvc-provisioning", pillarcsiv1alpha1.PillarVolumeStatePhaseProvisioning, false),
		metricsTestVolume("pvc-deleting", pillarcsiv1alpha1.PillarVolumeStatePhaseReady, true),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	elected := make(chan struct{})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(NewResourceMetricsCollector(reader, elected))

	if got := gatherPillar(t, reg); len(got) != 0 {
		t.Fatalf("before election the collector exported %v, want nothing", got)
	}

	close(elected)
	got := gatherPillar(t, reg)
	want := map[string]float64{
		"pillar_csi_resource_status_condition{kind=PillarAgent,name=agent-a,reason=HealthCheckFailed," +
			"status=False,type=AgentConnected}": 1,
		"pillar_csi_store_info{agent=agent-a,backend=zfs-zvol,pool=tank,store=store-a}":  1,
		"pillar_csi_volumes{agent=agent-a,backend=zfs-zvol,phase=Provisioning}":          1,
		"pillar_csi_volumes{agent=agent-a,backend=zfs-zvol,phase=CreatePartial}":         0,
		"pillar_csi_volumes{agent=agent-a,backend=zfs-zvol,phase=Ready}":                 2,
		"pillar_csi_volumes{agent=agent-a,backend=zfs-zvol,phase=Deleting}":              1,
		"pillar_csi_volumes_export_unreconciled{agent=agent-a,reason=ExportSpecMissing}": 0,
		"pillar_csi_volumes_export_unreconciled{agent=agent-a,reason=AgentUnavailable}":  1,
		"pillar_csi_volumes_export_unreconciled{agent=agent-a,reason=ReconcileFailed}":   0,
		"pillar_csi_volumes_export_unreconciled{agent=agent-a,reason=StaleGeneration}":   0,
	}
	if len(got) != len(want) {
		t.Errorf("after election got %d series, want %d:\n got  %v\n want %v", len(got), len(want), got, want)
	}
	for key, v := range want {
		gv, ok := got[key]
		if !ok {
			t.Errorf("missing series %s", key)
			continue
		}
		if gv != v {
			t.Errorf("%s = %v, want %v", key, gv, v)
		}
	}
}
