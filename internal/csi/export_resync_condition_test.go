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

package csi

import (
	"context"
	"reflect"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// conditionSnapshotReader counts only authoritative PillarVolumeState GETs;
// cached discovery and the test's own assertions use the underlying client.
type conditionSnapshotReader struct {
	client.Reader
	objects   []*v1alpha1.PillarVolumeState
	snapshots []*v1alpha1.PillarVolumeState
}

func (r *conditionSnapshotReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if err := r.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if pvs, ok := obj.(*v1alpha1.PillarVolumeState); ok {
		r.objects = append(r.objects, pvs)
		r.snapshots = append(r.snapshots, pvs.DeepCopy())
	}
	return nil
}

type conditionStatusClient struct {
	client.Client
	beforeUpdate      func(context.Context) error
	attemptedVersions []string
	conflicts         int
}

func (c *conditionStatusClient) Status() client.SubResourceWriter {
	return &conditionStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type conditionStatusWriter struct {
	client.SubResourceWriter
	owner *conditionStatusClient
}

func (w *conditionStatusWriter) Update(
	ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption,
) error {
	if _, ok := obj.(*v1alpha1.PillarVolumeState); ok {
		w.owner.attemptedVersions = append(w.owner.attemptedVersions, obj.GetResourceVersion())
		if w.owner.beforeUpdate != nil {
			if err := w.owner.beforeUpdate(ctx); err != nil {
				return err
			}
		}
	}
	err := w.SubResourceWriter.Update(ctx, obj, opts...)
	if k8serrors.IsConflict(err) {
		w.owner.conflicts++
	}
	return err
}

func conditionSuccess(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               ConditionExportReconciled,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             reasonExportReconciled,
		Message:            "export and ACL match the desired state",
		LastTransitionTime: metav1.NewTime(time.Unix(1_700_000_000, 0).UTC()),
	}
}

func conditionStoredVolume(t *testing.T, reader client.Reader, name string) *v1alpha1.PillarVolumeState {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := reader.Get(context.Background(), types.NamespacedName{Name: name}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState %s: %v", name, err)
	}
	return pvs
}

func TestReconcileVolumeExport_ConditionEqualityAndGeneration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		change func(*metav1.Condition)
	}{
		{name: "exact snapshot"},
		{name: "status", change: func(c *metav1.Condition) { c.Status = metav1.ConditionFalse }},
		{name: "reason", change: func(c *metav1.Condition) { c.Reason = "PreviousAttempt" }},
		{name: "message", change: func(c *metav1.Condition) { c.Message = "previous outcome" }},
		{name: "observed generation", change: func(c *metav1.Condition) { c.ObservedGeneration-- }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testConditionEqualityAndGeneration(t, tc.change)
		})
	}
}

func testConditionEqualityAndGeneration(t *testing.T, change func(*metav1.Condition)) {
	t.Helper()
	pvs := resyncPVS(aclSpec, resyncHostA)
	pvs.Generation = 7
	initial := conditionSuccess(pvs.Generation)
	if change != nil {
		change(&initial)
	}
	pvs.Status.Conditions = []metav1.Condition{initial}
	env := newResyncEnv(t, pvs)
	base := env.srv.k8sClient
	before := conditionStoredVolume(t, base, pvs.Name)
	reads := &conditionSnapshotReader{Reader: base}
	writes := &conditionStatusClient{Client: base}
	env.srv.apiReader, env.srv.k8sClient = reads, writes

	if err := env.srv.ReconcileVolumeExportWithCachedState(context.Background(), pvs.Name, before); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}
	if got := env.reconcileCalls.Load(); got != 1 {
		t.Errorf("agent ReconcileState calls = %d, want 1 even when condition is unchanged", got)
	}
	after := conditionStoredVolume(t, base, pvs.Name)
	condition := assertConditionSuccess(t, after)
	if change == nil {
		assertConditionUnchanged(t, before, after, reads, writes)
		return
	}
	assertConditionChanged(t, before, after, initial, condition, writes)
}

func assertConditionSuccess(t *testing.T, pvs *v1alpha1.PillarVolumeState) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(pvs.Status.Conditions, ConditionExportReconciled)
	if condition == nil {
		t.Fatal("ExportReconciled condition is missing")
	}
	want := conditionSuccess(pvs.Generation)
	if condition.Status != want.Status || condition.Reason != want.Reason ||
		condition.Message != want.Message || condition.ObservedGeneration != want.ObservedGeneration {
		t.Fatalf("ExportReconciled = %+v, want %+v", condition, want)
	}
	return condition
}

func assertConditionUnchanged(
	t *testing.T, before, after *v1alpha1.PillarVolumeState,
	reads *conditionSnapshotReader, writes *conditionStatusClient,
) {
	t.Helper()
	if got := len(reads.snapshots); got != 1 {
		t.Errorf("unchanged condition used %d authoritative volume GETs, want 1 under-lock GET", got)
	}
	if got := len(writes.attemptedVersions); got != 0 {
		t.Errorf("unchanged condition attempted %d status writes, want 0", got)
	}
	if after.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(after.Status, before.Status) {
		t.Errorf("unchanged condition modified persisted volume: before=%+v after=%+v", before, after)
	}
	for i, snapshot := range reads.snapshots {
		if !reflect.DeepEqual(reads.objects[i], snapshot) {
			t.Errorf("unchanged condition mutated authoritative snapshot %d", i)
		}
	}
}

func assertConditionChanged(
	t *testing.T, before, after *v1alpha1.PillarVolumeState,
	initial metav1.Condition, condition *metav1.Condition, writes *conditionStatusClient,
) {
	t.Helper()
	if got := len(writes.attemptedVersions); got != 1 {
		t.Errorf("changed condition attempted %d status writes, want 1", got)
	}
	if after.ResourceVersion == before.ResourceVersion {
		t.Error("changed condition did not advance resourceVersion")
	}
	if condition.LastTransitionTime.IsZero() {
		t.Error("changed condition has no transition time")
	}
	if initial.Status == condition.Status && !condition.LastTransitionTime.Equal(&initial.LastTransitionTime) {
		t.Error("reason, message, or generation update changed the status transition time")
	}
	expected := before.DeepCopy()
	expected.Status.Conditions = []metav1.Condition{*condition}
	if !reflect.DeepEqual(after.Status, expected.Status) {
		t.Errorf("condition update changed unrelated status: got=%+v want=%+v", after.Status, expected.Status)
	}
}

func TestReconcileVolumeExport_ConditionConflictPreservesConcurrentStatus(t *testing.T) {
	t.Parallel()
	pvs := resyncPVS(aclSpec, resyncHostA)
	pvs.Generation = 9
	initial := conditionSuccess(pvs.Generation)
	initial.Status, initial.Reason = metav1.ConditionFalse, "PreviousAttempt"
	pvs.Status.Conditions = []metav1.Condition{initial}
	env := newResyncEnv(t, pvs)
	base := env.srv.k8sClient
	cached := conditionStoredVolume(t, base, pvs.Name)
	reads := &conditionSnapshotReader{Reader: base}
	writes := &conditionStatusClient{Client: base}
	env.srv.apiReader, env.srv.k8sClient = reads, writes
	var concurrent *v1alpha1.PillarVolumeState
	writes.beforeUpdate = func(ctx context.Context) error {
		writes.beforeUpdate = nil
		var err error
		concurrent, err = commitConcurrentConditionStatus(ctx, base, pvs.Name)
		return err
	}

	if err := env.srv.ReconcileVolumeExportWithCachedState(context.Background(), pvs.Name, cached); err != nil {
		t.Fatalf("ReconcileVolumeExport: %v", err)
	}
	assertConditionConflictRetry(t, writes, reads, concurrent)
	after := conditionStoredVolume(t, base, pvs.Name)
	condition := assertConditionSuccess(t, after)
	expected := concurrent.DeepCopy()
	meta.SetStatusCondition(&expected.Status.Conditions, *condition)
	if !reflect.DeepEqual(after.Status, expected.Status) {
		t.Errorf("condition retry lost concurrent publication, deletion, or other condition: got=%+v want=%+v",
			after.Status, expected.Status)
	}
	if after.ResourceVersion == concurrent.ResourceVersion {
		t.Error("successful condition retry did not advance resourceVersion")
	}
	if got := env.reconcileCalls.Load(); got != 1 {
		t.Errorf("condition retry repeated the agent reconciliation: %d calls, want 1", got)
	}
}

func commitConcurrentConditionStatus(
	ctx context.Context, base client.Client, name string,
) (*v1alpha1.PillarVolumeState, error) {
	concurrent := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, types.NamespacedName{Name: name}, concurrent); err != nil {
		return nil, err
	}
	concurrent.Status.PublishedNodes = append(concurrent.Status.PublishedNodes, v1alpha1.VolumePublication{
		NodeID: "node-b", InitiatorID: resyncHostB, AccessMode: "SINGLE_NODE_WRITER",
	})
	concurrent.Status.PublicationGeneration++
	concurrent.Status.Deleting = true
	concurrent.Status.Conditions = append(concurrent.Status.Conditions, metav1.Condition{
		Type: "ConcurrentDeletion", Status: metav1.ConditionTrue, Reason: "DeletionStarted",
		Message: "another controller committed deletion", ObservedGeneration: concurrent.Generation,
		LastTransitionTime: metav1.NewTime(time.Unix(1_700_000_100, 0).UTC()),
	})
	// Commit through the real fake-client CAS, then submit the original stale
	// write unchanged: the resourceVersion mismatch must produce the conflict.
	if err := base.Status().Update(ctx, concurrent); err != nil {
		return nil, err
	}
	return concurrent, nil
}

func assertConditionConflictRetry(
	t *testing.T, writes *conditionStatusClient, reads *conditionSnapshotReader,
	concurrent *v1alpha1.PillarVolumeState,
) {
	t.Helper()
	if writes.conflicts != 1 || len(writes.attemptedVersions) != 2 {
		t.Fatalf("status writes = %v, conflicts = %d, want one real conflict then one successful retry",
			writes.attemptedVersions, writes.conflicts)
	}
	if concurrent == nil || writes.attemptedVersions[0] == concurrent.ResourceVersion ||
		writes.attemptedVersions[1] != concurrent.ResourceVersion {
		t.Fatalf("retry did not use concurrent resourceVersion: attempts=%v concurrent=%+v",
			writes.attemptedVersions, concurrent)
	}
	for _, snapshot := range reads.snapshots {
		if snapshot.ResourceVersion == concurrent.ResourceVersion && reflect.DeepEqual(snapshot.Status, concurrent.Status) {
			return
		}
	}
	t.Error("conflict retry never read the freshly committed concurrent status")
}

func TestRestoreAgentExports_UnchangedConditionsUsePreparedSnapshots(t *testing.T) {
	t.Parallel()
	volumes := []*v1alpha1.PillarVolumeState{
		restorePVS(resyncAgentName, "condition-ready-a", "bbbbbbbb-0000-0000-0000-000000000001"),
		restorePVS(resyncAgentName, "condition-ready-b", "bbbbbbbb-0000-0000-0000-000000000002"),
	}
	objects := make([]client.Object, 0, len(volumes))
	for i, pvs := range volumes {
		pvs.Generation = int64(i + 3)
		pvs.Status.Conditions = []metav1.Condition{conditionSuccess(pvs.Generation)}
		objects = append(objects, pvs)
	}
	env := newResyncEnvObjects(t, t.TempDir(), objects)
	base := env.srv.k8sClient
	before := make(map[string]*v1alpha1.PillarVolumeState, len(volumes))
	for _, pvs := range volumes {
		before[pvs.Name] = conditionStoredVolume(t, base, pvs.Name)
	}
	reads := &conditionSnapshotReader{Reader: base}
	writes := &conditionStatusClient{Client: base}
	env.srv.apiReader, env.srv.k8sClient = reads, writes

	if err := env.srv.RestoreAgentExports(context.Background(), resyncAgentName); err != nil {
		t.Fatalf("RestoreAgentExports: %v", err)
	}
	if got := len(reads.snapshots); got != len(volumes) {
		t.Errorf("unchanged batch used %d authoritative volume GETs, want %d prepared snapshots",
			got, len(volumes))
	}
	if got := len(writes.attemptedVersions); got != 0 {
		t.Errorf("unchanged batch attempted %d status writes, want 0", got)
	}
	if got := env.reconcileCalls.Load(); got != 1 {
		t.Errorf("agent ReconcileState calls = %d, want one complete batch", got)
	}
	for name, original := range before {
		after := conditionStoredVolume(t, base, name)
		if after.ResourceVersion != original.ResourceVersion || !reflect.DeepEqual(after.Status, original.Status) {
			t.Errorf("unchanged batch modified %s: before=%+v after=%+v", name, original, after)
		}
	}
	for i, snapshot := range reads.snapshots {
		if !reflect.DeepEqual(reads.objects[i], snapshot) {
			t.Errorf("unchanged batch mutated prepared snapshot %d", i)
		}
	}
}
