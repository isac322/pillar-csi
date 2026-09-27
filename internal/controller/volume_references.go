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
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// volumeDeletions admits only delete events: removing a PersistentVolume or
// PillarVolumeState is the only volume change that can unblock a deletion.
var volumeDeletions = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// watchVolumeDeletions makes bldr re-reconcile every object of the list kind
// that is being deleted whenever a PersistentVolume or PillarVolumeState is
// deleted, so that a deletion blocked on that volume proceeds without
// waiting for the requeue interval.  Objects not being deleted are left
// alone: a volume deletion cannot change their reconcile outcome.
func watchVolumeDeletions(
	bldr *builder.Builder,
	reader client.Reader,
	newList func() client.ObjectList,
) *builder.Builder {
	mapFn := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return deletingObjectRequests(ctx, reader, newList())
	})
	return bldr.
		Watches(&corev1.PersistentVolume{}, mapFn, builder.WithPredicates(volumeDeletions)).
		Watches(&pillarcsiv1alpha1.PillarVolumeState{}, mapFn, builder.WithPredicates(volumeDeletions))
}

// deletingObjectRequests lists list and returns a request for every item
// that has a deletion timestamp.
func deletingObjectRequests(ctx context.Context, reader client.Reader, list client.ObjectList) []reconcile.Request {
	log := logf.FromContext(ctx)
	err := reader.List(ctx, list)
	if err != nil {
		log.Error(err, "Failed to list objects to re-check deletions blocked on a volume")
		return nil
	}
	var requests []reconcile.Request
	err = apimeta.EachListItem(list, func(item runtime.Object) error {
		obj, ok := item.(client.Object)
		if ok && !obj.GetDeletionTimestamp().IsZero() {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: obj.GetName()}})
		}
		return nil
	})
	if err != nil {
		log.Error(err, "Failed to walk objects to re-check deletions blocked on a volume")
		return nil
	}
	return requests
}

// Volume references let the deletion guards of PillarStorageClass,
// PillarStore and PillarAgent see the volumes provisioned through them.
//
// DeleteVolume needs the PillarAgent (and, through the StorageClass, the
// PillarStore) of a volume to tear it down on the storage node.  As long as a
// PersistentVolume or a PillarVolumeState of a volume exists, the objects it
// was provisioned through must not be deleted, or the backend volume and its
// export leak on the storage node.  PillarVolumeState is authoritative: the
// CSI controller creates it before any agent call and deletes it only after
// the agent destroyed the backend volume, so it also covers a failed
// CreateVolume that never produced a PV.  PersistentVolumes are checked too,
// because a PV may outlive its PillarVolumeState (e.g. a force-removed record)
// while DeleteVolume is still expected to run for it.
//
// Attribution: a volume belongs to the PillarStore referenced by the
// PillarStorageClass that generated its StorageClass — never to every
// PillarStore sharing the pool (several PillarStores per pool is a normal
// configuration, so pool matching alone would pin unused stores forever).
// A PersistentVolume is attributed by its spec.storageClassName; a
// PillarVolumeState inherits the attribution of its same-named PV, or of the
// claim recorded in spec.claimRef when no PV exists.  A volume that cannot be
// attributed this way (a StorageClass no PillarStorageClass owns, no PV and no
// claim record) fails closed: it still blocks every PillarStore it lies in by
// location, and bindings whose store and protocol it matches.

// volumeHandleParts is the number of "/"-separated fields of a volume handle:
// "<agent>/<protocol-type>/<backend-type>/<agent-volume-id>", where the agent
// volume ID is itself "<pool>/<volume-name>" for pooled backends.
const volumeHandleParts = 4

// maxListedBlockers bounds how many blocker names a DeletionBlocked message
// lists before summarizing the rest as a count.
const maxListedBlockers = 10

// volumeRef identifies where a volume lives, as recorded by the CSI
// controller in the volume handle and the PillarVolumeState spec.
type volumeRef struct {
	agent    string
	protocol string
	backend  string
	// pool is the leading "<pool>/" segment of the agent volume ID: the ZFS
	// pool, LVM volume group, or PillarStore name the volume was carved from.
	// Empty when the agent volume ID carries no pool segment.
	pool string
}

// volumeRefFromVolumeState returns the location recorded in pvs.
func volumeRefFromVolumeState(pvs *pillarcsiv1alpha1.PillarVolumeState) volumeRef {
	return volumeRef{
		agent:    pvs.Spec.AgentRef,
		protocol: pvs.Spec.ProtocolType,
		backend:  pvs.Spec.BackendType,
		pool:     poolOfAgentVolumeID(pvs.Spec.AgentVolumeID),
	}
}

// volumeRefFromPV returns the location encoded in the volume handle of a PV
// provisioned by pillar-csi, and false for any other PV.
func volumeRefFromPV(pv *corev1.PersistentVolume) (volumeRef, bool) {
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != pillarCSIProvisioner {
		return volumeRef{}, false
	}
	parts := strings.SplitN(pv.Spec.CSI.VolumeHandle, "/", volumeHandleParts)
	if len(parts) != volumeHandleParts || parts[0] == "" {
		return volumeRef{}, false
	}
	return volumeRef{
		agent:    parts[0],
		protocol: parts[1],
		backend:  parts[2],
		pool:     poolOfAgentVolumeID(parts[3]),
	}, true
}

// poolOfAgentVolumeID returns the "<pool>" prefix of an agent volume ID
// "<pool>/<volume-name>", or "" when the ID carries no pool segment.
func poolOfAgentVolumeID(agentVolumeID string) string {
	pool, _, found := strings.Cut(agentVolumeID, "/")
	if !found {
		return ""
	}
	return pool
}

// storeVolumePool returns the pool segment that volumes provisioned from
// store carry in their agent volume ID.  It mirrors the store parameter the
// PillarStorageClass controller writes into the StorageClass: the ZFS pool,
// the LVM volume group, and otherwise the PillarStore name.
func storeVolumePool(store *pillarcsiv1alpha1.PillarStore) string {
	backend := store.Spec.Backend
	switch {
	case backend.ZFS != nil && backend.ZFS.Pool != "":
		return backend.ZFS.Pool
	case backend.LVM != nil && backend.LVM.VolumeGroup != "":
		return backend.LVM.VolumeGroup
	default:
		return store.Name
	}
}

// inStore reports whether ref lives in store: on its agent, in its backend
// type, and in its pool.
func (ref volumeRef) inStore(store *pillarcsiv1alpha1.PillarStore) bool {
	return ref.agent == store.Spec.AgentRef &&
		ref.backend == string(store.Spec.Backend.Type) &&
		ref.pool == storeVolumePool(store)
}

// provisionedThrough reports whether ref was provisioned through a
// PillarStorageClass binding store with protocol.
func (ref volumeRef) provisionedThrough(
	store *pillarcsiv1alpha1.PillarStore,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) bool {
	return ref.inStore(store) && ref.protocol == string(protocol.Spec.Type)
}

// storageClassStores maps each StorageClass name confirmed to be generated by
// a PillarStorageClass to the PillarStore that binding belongs to.  Two kinds
// of evidence count: a live StorageClass carries a controller owner reference
// to its binding (reconcileStorageClass sets it on create), and a binding's
// status.storageClassName is written only after that reconcile succeeded, so
// it still attributes volumes while the StorageClass is absent or was deleted.
// A binding's requested name (spec.storageClass.name) alone is not evidence:
// it can collide with a hand-written class the binding never created, and
// using it would attribute that class's volumes to the wrong store.  Claims
// for an absent class are dropped when they collide, so attribution fails
// closed instead of guessing.
func storageClassStores(
	bindings *pillarcsiv1alpha1.PillarStorageClassList,
	classes *storagev1.StorageClassList,
) map[string]string {
	bindingStore := make(map[string]string, len(bindings.Items))
	recorded := map[string]string{}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		bindingStore[binding.Name] = binding.Spec.StoreRef
		if binding.Status.StorageClassName != "" {
			recorded[binding.Name] = binding.Status.StorageClassName
		}
	}

	stores := make(map[string]string, len(classes.Items))
	ambiguous := map[string]bool{}
	add := func(scName, storeName string) {
		if scName == "" || ambiguous[scName] {
			return
		}
		if owner, seen := stores[scName]; seen && owner != storeName {
			delete(stores, scName)
			ambiguous[scName] = true
			return
		}
		stores[scName] = storeName
	}

	liveSC := make(map[string]bool, len(classes.Items))
	for i := range classes.Items {
		sc := &classes.Items[i]
		liveSC[sc.Name] = true
		owner := metav1.GetControllerOf(sc)
		if owner == nil || owner.Kind != "PillarStorageClass" ||
			owner.APIVersion != pillarcsiv1alpha1.GroupVersion.String() {
			continue
		}
		if store, ok := bindingStore[owner.Name]; ok {
			add(sc.Name, store)
		}
	}

	for bindingName, scName := range recorded {
		if liveSC[scName] {
			// A live StorageClass is attributed by its owner reference alone;
			// a stale status record must not override it.
			continue
		}
		add(scName, bindingStore[bindingName])
	}
	return stores
}

// volumeBlocksStore reports whether a volume whose StorageClass is scName
// blocks the deletion of store.  A StorageClass owned by a PillarStorageClass
// attributes the volume to that binding's store (spec.storeRef is immutable);
// any other name — empty, hand-written, or generated by a since-removed
// binding — leaves the volume unattributable, so it fails closed on the
// store's location match.
func volumeBlocksStore(
	scName string,
	ref volumeRef,
	store *pillarcsiv1alpha1.PillarStore,
	scStores map[string]string,
) bool {
	if owner, ok := scStores[scName]; ok {
		return owner == store.Name
	}
	return ref.inStore(store)
}

// pvcOfVolumeState returns the claim pvs was provisioned for, when the claim
// still exists and is the same object (its UID matches spec.claimRef).  A
// claim re-created under the same name is a different claim and does not
// count, nor does a record without namespace and name.
func pvcOfVolumeState(
	pvs *pillarcsiv1alpha1.PillarVolumeState,
	pvcByName map[string]*corev1.PersistentVolumeClaim,
) *corev1.PersistentVolumeClaim {
	ref := pvs.Spec.ClaimRef
	if ref == nil || ref.Namespace == "" || ref.Name == "" {
		return nil
	}
	pvc := pvcByName[ref.Namespace+"/"+ref.Name]
	if pvc == nil || string(pvc.UID) != ref.UID {
		return nil
	}
	return pvc
}

// volumeReferences names the PersistentVolumes and PillarVolumeStates that
// block a deletion.
type volumeReferences struct {
	pvs          []string
	volumeStates []string
}

// deletionBlockers collects the groups of objects that block a deletion, for
// the DeletionBlocked condition message.
type deletionBlockers []string

// add records names under kind, e.g. "PVC(s) [ns/a, ns/b]", listing at most
// maxListedBlockers names.
func (b *deletionBlockers) add(kind string, names []string) {
	if len(names) == 0 {
		return
	}
	listed := strings.Join(names, ", ")
	if len(names) > maxListedBlockers {
		listed = fmt.Sprintf("%s, ... and %d more",
			strings.Join(names[:maxListedBlockers], ", "), len(names)-maxListedBlockers)
	}
	*b = append(*b, fmt.Sprintf("%s [%s]", kind, listed))
}

// addVolumes records the PersistentVolumes and PillarVolumeStates of v.
func (b *deletionBlockers) addVolumes(v volumeReferences) {
	b.add("PersistentVolume(s)", v.pvs)
	b.add("PillarVolumeState(s)", v.volumeStates)
}

// message renders the DeletionBlocked message for a deletion of target.
func (b deletionBlockers) message(target string) string {
	return fmt.Sprintf("Deletion blocked: %s still reference %s; delete them first",
		strings.Join(b, " and "), target)
}

// A volumeStateMatch decides whether a PillarVolumeState blocks a deletion.
// It also receives the PV that shares the PillarVolumeState's name (or nil):
// both are named after the CSI volume, so that PV (when present) is the one
// provisioned for it.  The claim recorded in spec.claimRef is passed too (nil
// when none is recorded or the claim is gone), so a PillarVolumeState without
// a PV can still be attributed through the claim's StorageClass.
type volumeStateMatch func(
	pvs *pillarcsiv1alpha1.PillarVolumeState,
	pv *corev1.PersistentVolume,
	pvc *corev1.PersistentVolumeClaim,
) bool

// listVolumeReferences returns the PVs matching pvMatch and the
// PillarVolumeStates matching pvsMatch.
func listVolumeReferences(
	ctx context.Context,
	reader client.Reader,
	pvMatch func(*corev1.PersistentVolume) bool,
	pvsMatch volumeStateMatch,
) (volumeReferences, error) {
	var refs volumeReferences
	pvList := &corev1.PersistentVolumeList{}
	err := reader.List(ctx, pvList)
	if err != nil {
		return refs, fmt.Errorf("list PersistentVolumes: %w", err)
	}
	pvByName := make(map[string]*corev1.PersistentVolume, len(pvList.Items))
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		pvByName[pv.Name] = pv
		if pvMatch(pv) {
			refs.pvs = append(refs.pvs, pv.Name)
		}
	}

	pvcList := &corev1.PersistentVolumeClaimList{}
	err = reader.List(ctx, pvcList)
	if err != nil {
		return refs, fmt.Errorf("list PersistentVolumeClaims: %w", err)
	}
	pvcByName := make(map[string]*corev1.PersistentVolumeClaim, len(pvcList.Items))
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		pvcByName[pvc.Namespace+"/"+pvc.Name] = pvc
	}

	pvsList := &pillarcsiv1alpha1.PillarVolumeStateList{}
	err = reader.List(ctx, pvsList)
	if err != nil {
		return refs, fmt.Errorf("list PillarVolumeStates: %w", err)
	}
	for i := range pvsList.Items {
		pvs := &pvsList.Items[i]
		if pvsMatch(pvs, pvByName[pvs.Name], pvcOfVolumeState(pvs, pvcByName)) {
			refs.volumeStates = append(refs.volumeStates, pvs.Name)
		}
	}
	return refs, nil
}
