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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// cacheResyncStaleClient models an informer cache that still serves one old
// PillarVolumeState while all writes and the authoritative reader use base.
// The production path must use this object only for discovery, then re-read
// the lifecycle from apiReader under the correct volume lock.
type cacheResyncStaleClient struct {
	client.Client
	stale *v1alpha1.PillarVolumeState
}

func (c *cacheResyncStaleClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if pvs, ok := obj.(*v1alpha1.PillarVolumeState); ok && c.stale != nil && key.Name == c.stale.Name {
		*pvs = *c.stale.DeepCopy()
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func installCacheResyncStaleClient(
	srv *ControllerServer,
	stale *v1alpha1.PillarVolumeState,
) *cacheResyncStaleClient {
	base := srv.k8sClient
	cache := &cacheResyncStaleClient{Client: base, stale: stale.DeepCopy()}
	srv.k8sClient = cache
	srv.apiReader = base
	return cache
}

func TestReconcileVolumeExport_StaleDiscoveryVolumeIDRefusesActions(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	ctx := context.Background()
	base := env.srv.k8sClient

	stale := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, types.NamespacedName{Name: resyncPVSName}, stale); err != nil {
		t.Fatal(err)
	}
	fresh := stale.DeepCopy()
	fresh.Spec.VolumeID = resyncVolumeID + "/replacement"
	if err := base.Update(ctx, fresh); err != nil {
		t.Fatalf("replace authoritative VolumeID: %v", err)
	}
	installCacheResyncStaleClient(env.srv, stale)

	wantStatus := fresh.Status.DeepCopy()
	if err := env.srv.ReconcileVolumeExportWithCachedState(ctx, resyncPVSName, stale); err == nil {
		t.Fatal("ReconcileVolumeExport succeeded across a stale VolumeID identity")
	}
	if got := env.reconcileCalls.Load(); got != 0 {
		t.Fatalf("agent ReconcileState calls = %d, want 0", got)
	}
	got := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, types.NamespacedName{Name: resyncPVSName}, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status, *wantStatus) {
		t.Fatalf("authoritative status changed before identity refusal: got %+v, want %+v", got.Status, *wantStatus)
	}
}

func TestReapAbandonedVolume_StaleDiscoveryVolumeIDRefusesActions(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	ctx := context.Background()
	base := env.srv.k8sClient

	stale := volumeState(t, env, req.Name)
	fresh := stale.DeepCopy()
	fresh.Spec.VolumeID += "/replacement"
	if err := base.Update(ctx, fresh); err != nil {
		t.Fatalf("replace authoritative VolumeID: %v", err)
	}
	installCacheResyncStaleClient(env.srv, stale)
	wantStatus := fresh.Status.DeepCopy()

	if _, err := env.srv.ReapAbandonedVolumeWithCachedState(ctx, req.Name, stale); err == nil {
		t.Fatal("ReapAbandonedVolume succeeded across a stale VolumeID identity")
	}
	if got := agentTeardownCalls(env); got != 0 {
		t.Fatalf("agent teardown calls = %d, want 0", got)
	}
	got := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, types.NamespacedName{Name: req.Name}, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status, *wantStatus) {
		t.Fatalf("authoritative status changed before identity refusal: got %+v, want %+v", got.Status, *wantStatus)
	}
}

func TestReapAbandonedVolume_CachedReadyDefersUntilCacheRefresh(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	ctx := context.Background()

	fresh := volumeState(t, env, req.Name)
	staleReady := fresh.DeepCopy()
	staleReady.Status.Phase = v1alpha1.PillarVolumeStatePhaseReady
	cache := installCacheResyncStaleClient(env.srv, staleReady)

	if reaped, err := env.srv.ReapAbandonedVolumeWithCachedState(ctx, req.Name, staleReady); err != nil || reaped {
		t.Fatalf("stale cached Ready volume reaped = %t, err=%v; want false, nil", reaped, err)
	}
	if got := agentTeardownCalls(env); got != 0 {
		t.Fatalf("agent teardown calls before cache refresh = %d, want 0", got)
	}
	if got := volumeState(t, env, req.Name); got == nil || got.Status.Deleting {
		t.Fatalf("authoritative lifecycle changed while cache said Ready: %+v", got)
	}

	cache.stale = fresh.DeepCopy()
	if reaped, err := env.srv.ReapAbandonedVolumeWithCachedState(ctx, req.Name, cache.stale); err != nil || !reaped {
		t.Fatalf("refreshed cached provisioning volume reaped = %t, err=%v; want true, nil", reaped, err)
	}
	if got := agentTeardownCalls(env); got != 2 {
		t.Fatalf("agent teardown calls after cache refresh = %d, want 2", got)
	}
	if got := volumeState(t, env, req.Name); got != nil {
		t.Fatalf("PillarVolumeState remains after refreshed reap: %+v", got)
	}
}

func TestReapAbandonedVolume_CachedCandidateFreshReadySkipsTeardown(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	ctx := context.Background()
	base := env.srv.k8sClient

	stale := volumeState(t, env, req.Name)
	fresh := stale.DeepCopy()
	fresh.Status.Phase = v1alpha1.PillarVolumeStatePhaseReady
	if err := base.Status().Update(ctx, fresh); err != nil {
		t.Fatalf("publish authoritative Ready phase: %v", err)
	}
	installCacheResyncStaleClient(env.srv, stale)

	if reaped, err := env.srv.ReapAbandonedVolumeWithCachedState(ctx, req.Name, stale); err != nil || reaped {
		t.Fatalf("cached candidate with authoritative Ready reaped = %t, err=%v; want false, nil", reaped, err)
	}
	if got := agentTeardownCalls(env); got != 0 {
		t.Fatalf("agent teardown calls for authoritative Ready volume = %d, want 0", got)
	}
	got := volumeState(t, env, req.Name)
	if got == nil || got.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady || got.Status.Deleting {
		t.Fatalf("authoritative Ready lifecycle changed: %+v", got)
	}
}

func TestReconcileVolumeExport_SameVolumeIDReplacementUsesFreshUIDAndFence(t *testing.T) {
	t.Parallel()
	env := newResyncEnv(t, resyncPVS(aclSpec, resyncHostA))
	ctx := context.Background()
	base := env.srv.k8sClient

	stale := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, types.NamespacedName{Name: resyncPVSName}, stale); err != nil {
		t.Fatal(err)
	}
	fresh := stale.DeepCopy()
	fresh.UID = types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	fresh.Status.PublicationGeneration = 2
	if err := base.Delete(ctx, stale); err != nil {
		t.Fatalf("delete old lifecycle: %v", err)
	}
	fresh.ResourceVersion = ""
	if err := base.Create(ctx, fresh); err != nil {
		t.Fatalf("create replacement lifecycle: %v", err)
	}
	installCacheResyncStaleClient(env.srv, stale)

	fence := &agentv1.FencingToken{VolumeUid: string(fresh.UID), Generation: 2}
	if _, err := env.agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: exportParamsFor(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, aclSpec),
		DevicePath:   resyncDevicePath, AclEnabled: true, Fence: fence,
	}); err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
	if _, err := env.agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId: resyncAgentVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId: resyncHostA, Fence: fence,
	}); err != nil {
		t.Fatalf("AllowInitiator: %v", err)
	}

	if err := env.srv.ReconcileVolumeExportWithCachedState(ctx, resyncPVSName, stale); err != nil {
		t.Fatalf("ReconcileVolumeExport with same-ID replacement: %v", err)
	}
	if !env.hostAllowed(resyncHostA) {
		t.Fatal("fresh replacement publication was not reconciled")
	}
}
