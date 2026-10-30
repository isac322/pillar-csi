//go:build integration

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
	"reflect"
	"sync/atomic"
	"testing"

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/csi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// issue199ReadBudgetReader is the authoritative API reader. Only
// PillarVolumeState GETs are counted because those are the reads whose budget
// is part of the export resync contract.
type issue199ReadBudgetReader struct {
	client.Reader
	pillarVolumeStateGets atomic.Int64
}

func (r *issue199ReadBudgetReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*pillarcsiv1alpha1.PillarVolumeState); ok {
		r.pillarVolumeStateGets.Add(1)
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// issue199ReadBudgetClient models the manager client/cache separately from the
// authoritative reader while sharing the same backing store.
type issue199ReadBudgetClient struct {
	client.Client
	pillarVolumeStateGets atomic.Int64
	statusUpdates         atomic.Int64
}

func (c *issue199ReadBudgetClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if _, ok := obj.(*pillarcsiv1alpha1.PillarVolumeState); ok {
		c.pillarVolumeStateGets.Add(1)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *issue199ReadBudgetClient) Status() client.SubResourceWriter {
	return &issue199ReadBudgetStatusWriter{
		SubResourceWriter: c.Client.Status(),
		updates:           &c.statusUpdates,
	}
}

type issue199ReadBudgetStatusWriter struct {
	client.SubResourceWriter
	updates *atomic.Int64
}

func (w *issue199ReadBudgetStatusWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	w.updates.Add(1)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func issue199ReadBudgetScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := pillarcsiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func issue199DynamicNFSVolumeState(name string) *pillarcsiv1alpha1.PillarVolumeState {
	return &pillarcsiv1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
			VolumeID:      "agent/nfs/zfs-dataset/tank/" + name,
			AgentVolumeID: "tank/" + name,
			AgentRef:      "agent",
			BackendType:   "zfs-dataset",
			ProtocolType:  "nfs",
			CapacityBytes: 1 << 30,
			Resolved: &pillarcsiv1alpha1.ResolvedVolumeConfig{
				Backend: pillarcsiv1alpha1.BackendSpec{ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
					Pool: "tank", VolumeType: pillarcsiv1alpha1.ZFSVolumeTypeDataset,
				}},
				Protocol: pillarcsiv1alpha1.ProtocolSpec{NFS: &pillarcsiv1alpha1.NFSConfig{}},
			},
		},
		Status: pillarcsiv1alpha1.PillarVolumeStateStatus{
			Phase: pillarcsiv1alpha1.PillarVolumeStatePhaseReady,
		},
	}
}

func issue199ReadBudgetCondition(
	conditions []metav1.Condition,
) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == csi.ConditionExportReconciled {
			return &conditions[i]
		}
	}
	return nil
}

func TestIssue199_PillarVolumeStateReadyReadBudget(t *testing.T) {
	tests := []struct {
		name  string
		build func(string) *pillarcsiv1alpha1.PillarVolumeState
	}{
		{
			name:  "default-dynamic-nfs",
			build: issue199DynamicNFSVolumeState,
		},
		{
			name:  "file-adopted-nfs",
			build: nativeDatasetVolumeState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			name := "issue199-read-budget-" + tt.name
			pvs := tt.build(name)
			backing := fake.NewClientBuilder().
				WithScheme(issue199ReadBudgetScheme(t)).
				WithStatusSubresource(&pillarcsiv1alpha1.PillarVolumeState{}).
				WithObjects(pvs).
				Build()

			cached := &issue199ReadBudgetClient{Client: backing}
			authoritative := &issue199ReadBudgetReader{Reader: backing}
			defaultServer := csi.NewControllerServer(cached, authoritative, pillarcsiv1alpha1.DefaultCSIDriver)
			fileServer := csi.NewControllerServer(cached, authoritative, pillarcsiv1alpha1.FileCSIDriver)
			reconciler := &PillarVolumeStateReconciler{
				Client:      cached,
				Exports:     defaultServer,
				Reaper:      defaultServer,
				FileExports: fileServer,
				FileReaper:  fileServer,
			}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}

			first, err := reconciler.Reconcile(ctx, req)
			if err != nil {
				t.Fatalf("establishing missing export condition: %v", err)
			}
			if first.RequeueAfter != volumeExportResyncInterval {
				t.Fatalf("initial reconcile result = %+v, want periodic export resync", first)
			}

			stable := &pillarcsiv1alpha1.PillarVolumeState{}
			if err := backing.Get(ctx, types.NamespacedName{Name: name}, stable); err != nil {
				t.Fatal(err)
			}
			condition := issue199ReadBudgetCondition(stable.Status.Conditions)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ExportSpecMissing" {
				t.Fatalf("initial condition = %v, want ExportReconciled=False/ExportSpecMissing", stable.Status.Conditions)
			}

			cached.pillarVolumeStateGets.Store(0)
			authoritative.pillarVolumeStateGets.Store(0)
			cached.statusUpdates.Store(0)
			var previousAuthoritativeGets int64
			for i := range 35 {
				result, err := reconciler.Reconcile(ctx, req)
				if err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
				if result.RequeueAfter != volumeExportResyncInterval {
					t.Fatalf("reconcile %d result = %+v, want periodic export resync", i, result)
				}
				authoritativeGets := authoritative.pillarVolumeStateGets.Load()
				if gets := authoritativeGets - previousAuthoritativeGets; gets != 1 {
					t.Fatalf("reconcile %d used %d authoritative PillarVolumeState GETs, want 1", i, gets)
				}
				previousAuthoritativeGets = authoritativeGets
			}
			if updates := cached.statusUpdates.Load(); updates != 0 {
				t.Fatalf("stable reconciles changed status with %d updates", updates)
			}

			got := &pillarcsiv1alpha1.PillarVolumeState{}
			if err := backing.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
				t.Fatal(err)
			}
			gotCondition := issue199ReadBudgetCondition(got.Status.Conditions)
			if gotCondition == nil || gotCondition.Status != condition.Status ||
				gotCondition.Reason != condition.Reason || gotCondition.Message != condition.Message ||
				gotCondition.ObservedGeneration != condition.ObservedGeneration {
				t.Fatalf("stable condition changed: before=%v after=%v", condition, gotCondition)
			}
			if !reflect.DeepEqual(stable.Status, got.Status) || stable.ResourceVersion != got.ResourceVersion {
				t.Fatalf("periodic reconcile mutated stable status: before=%+v after=%+v", stable.Status, got.Status)
			}
		})
	}
}
