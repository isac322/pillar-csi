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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

// listVolumeReferences returns the PVs matching pvMatch and the
// PillarVolumeStates matching pvsMatch.  The pvsMatch predicate also receives
// the PV that shares the PillarVolumeState's name, or nil: both are named
// after the CSI volume, so that PV (when present) is the one provisioned for
// it.
func listVolumeReferences(
	ctx context.Context,
	reader client.Reader,
	pvMatch func(*corev1.PersistentVolume) bool,
	pvsMatch func(pvs *pillarcsiv1alpha1.PillarVolumeState, pv *corev1.PersistentVolume) bool,
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

	pvsList := &pillarcsiv1alpha1.PillarVolumeStateList{}
	err = reader.List(ctx, pvsList)
	if err != nil {
		return refs, fmt.Errorf("list PillarVolumeStates: %w", err)
	}
	for i := range pvsList.Items {
		pvs := &pvsList.Items[i]
		if pvsMatch(pvs, pvByName[pvs.Name]) {
			refs.volumeStates = append(refs.volumeStates, pvs.Name)
		}
	}
	return refs, nil
}
