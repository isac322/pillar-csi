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
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

const (
	// Finalizer added to every PillarProtocol to prevent
	// deletion while PillarStorageClasss still reference it.
	pillarProtocolFinalizer = "pillar-csi.bhyoo.com/protocol-protection"

	// Requeue interval before re-checking whether blocking PillarStorageClasss have been removed.
	requeueAfterProtocolDeletionBlock = 10 * time.Second
)

// PillarProtocolReconciler reconciles a PillarProtocol object.
type PillarProtocolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarprotocols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarprotocols/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstores,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarProtocol the reconciler:
//  1. Adds a finalizer on first creation.
//  2. On normal operation: counts PillarStorageClass references and computes the
//     set of activeAgents (via Binding→Pool→Target chain), then updates status.
//  3. On deletion: blocks until no PillarStorageClasss reference this protocol,
//     then removes the finalizer to allow the object to be garbage-collected.
//
//nolint:dupl // All four CRD controllers share identical Reconcile boilerplate; extraction requires reflection.
func (r *PillarProtocolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarProtocol instance.
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	err := r.Get(ctx, req.NamespacedName, protocol)
	if err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarProtocol", "name", protocol.Name, "deletionTimestamp", protocol.DeletionTimestamp)

	// Branch: object is being deleted.
	if !protocol.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, protocol)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(protocol, pillarProtocolFinalizer) {
		log.Info("Adding finalizer to PillarProtocol", "finalizer", pillarProtocolFinalizer)
		controllerutil.AddFinalizer(protocol, pillarProtocolFinalizer)
		err := r.Update(ctx, protocol)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, protocol)
}

// reconcileNormal handles the steady-state reconciliation of a PillarProtocol
// that is not being deleted.
//
// It:
//  1. Lists all PillarStorageClasss that reference this protocol to compute storageClassCount.
//  2. For each referencing binding, looks up its PillarStore to collect the
//     pool's agentRef — building the deduplicated, sorted activeAgents list.
//  3. Writes storageClassCount, activeAgents, and the Ready condition to status.
func (r *PillarProtocolReconciler) reconcileNormal(
	ctx context.Context,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// List all PillarStorageClasss (cluster-scoped, no namespace filter).
	bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
	err := r.List(ctx, bindingList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarStorageClasss: %w", err)
	}

	// Count references to this protocol and collect the referenced pool names.
	var count int32
	poolNames := make(map[string]struct{})
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.ProtocolRef == protocol.Name {
			count++
			poolNames[bindingList.Items[i].Spec.StoreRef] = struct{}{}
		}
	}

	log.Info("PillarProtocol binding count", "name", protocol.Name, "count", count)

	// For each referenced pool, look up its agentRef to build activeAgents.
	// We use a set to deduplicate (multiple bindings may share the same target).
	targetSet := make(map[string]struct{})
	for poolName := range poolNames {
		pool := &pillarcsiv1alpha1.PillarStore{}
		poolErr := r.Get(ctx, types.NamespacedName{Name: poolName}, pool)
		if poolErr != nil {
			if client.IgnoreNotFound(poolErr) != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get PillarStore %q: %w", poolName, poolErr)
			}
			// Pool not found — binding may be in a degraded state; skip gracefully.
			log.V(1).Info("Referenced PillarStore not found; skipping for activeAgents computation",
				"protocol", protocol.Name, "pool", poolName)
			continue
		}
		if pool.Spec.AgentRef != "" {
			targetSet[pool.Spec.AgentRef] = struct{}{}
		}
	}

	// Convert the set to a sorted slice for deterministic output.
	activeAgents := make([]string, 0, len(targetSet))
	for t := range targetSet {
		activeAgents = append(activeAgents, t)
	}
	sort.Strings(activeAgents)

	log.Info("PillarProtocol active targets",
		"name", protocol.Name,
		"activeAgents", activeAgents,
		"count", len(activeAgents),
	)

	// Build updated status fields.
	protocol.Status.StorageClassCount = count
	protocol.Status.ActiveAgents = activeAgents

	meta.SetStatusCondition(&protocol.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: protocol.Generation,
		Reason:             "ProtocolConfigured",
		Message: fmt.Sprintf(
			"PillarProtocol is configured with type %q; referenced by %d binding(s) across %d active target(s)",
			protocol.Spec.Type, count, len(activeAgents),
		),
	})

	err = r.Status().Update(ctx, protocol)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarProtocol status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the deletion flow.  The finalizer is only removed
// once no PillarStorageClasss reference this PillarProtocol.
func (r *PillarProtocolReconciler) reconcileDelete(
	ctx context.Context,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(protocol, pillarProtocolFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarProtocol is being deleted — checking for referencing PillarStorageClasss", "name", protocol.Name)

	// List all PillarStorageClasss and find those that reference this protocol.
	bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
	err := r.List(ctx, bindingList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarStorageClasss: %w", err)
	}

	var referencingNames []string
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.ProtocolRef == protocol.Name {
			referencingNames = append(referencingNames, bindingList.Items[i].Name)
		}
	}

	if len(referencingNames) > 0 {
		// Deletion is blocked — update status and requeue.
		msg := fmt.Sprintf(
			"Deletion blocked: PillarStorageClass(s) [%s] still reference this protocol; delete them first",
			strings.Join(referencingNames, ", "),
		)
		log.Info(msg, "name", protocol.Name)

		//nolint:gosec // StorageClassCount is bounded by available cluster resources and cannot realistically overflow int32.
		protocol.Status.StorageClassCount = int32(len(referencingNames))
		meta.SetStatusCondition(&protocol.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: protocol.Generation,
			Reason:             "DeletionBlocked",
			Message:            msg,
		})

		statusErr := r.Status().Update(ctx, protocol)
		if statusErr != nil {
			// Log but don't fail — the important thing is to requeue.
			log.Error(statusErr, "Failed to update status while deletion is blocked")
		}

		return ctrl.Result{RequeueAfter: requeueAfterProtocolDeletionBlock}, nil
	}

	// No referencing bindings — safe to remove the finalizer.
	log.Info("No PillarStorageClasss reference this protocol; removing finalizer", "name", protocol.Name)
	controllerutil.RemoveFinalizer(protocol, pillarProtocolFinalizer)
	err = r.Update(ctx, protocol)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarProtocol: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// The controller watches:
//   - PillarProtocol (primary resource)
//   - PillarStorageClass: re-enqueues the referenced PillarProtocol whenever a
//     binding is created, updated, or deleted — so that storageClassCount and the
//     deletion-gate stay consistent.
//   - PillarStore: re-enqueues the PillarProtocol(s) reachable via Pool→Binding→Protocol
//     whenever a pool changes — so that activeAgents stays consistent.
func (r *PillarProtocolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapBindingToProtocol extracts the protocolRef from a PillarStorageClass and
	// returns a reconcile.Request for the referenced PillarProtocol.
	mapBindingToProtocol := func(_ context.Context, obj client.Object) []reconcile.Request {
		binding, ok := obj.(*pillarcsiv1alpha1.PillarStorageClass)
		if !ok || binding.Spec.ProtocolRef == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: binding.Spec.ProtocolRef}},
		}
	}

	// mapPoolToProtocol: when a PillarStore changes, find all PillarStorageClasss
	// that reference it, then collect the distinct set of protocolRefs and
	// enqueue each for reconciliation so activeAgents is recomputed.
	mapPoolToProtocol := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pool, ok := obj.(*pillarcsiv1alpha1.PillarStore)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
		listErr := r.List(ctx, bindingList)
		if listErr != nil {
			// Cannot propagate error from a watch handler; log and return empty.
			logf.FromContext(ctx).Error(listErr,
				"Failed to list PillarStorageClasss while mapping PillarStore event to PillarProtocol",
				"pool", pool.Name)
			return nil
		}

		protocolSet := make(map[string]struct{})
		for i := range bindingList.Items {
			b := &bindingList.Items[i]
			if b.Spec.StoreRef == pool.Name && b.Spec.ProtocolRef != "" {
				protocolSet[b.Spec.ProtocolRef] = struct{}{}
			}
		}

		reqs := make([]reconcile.Request, 0, len(protocolSet))
		for name := range protocolSet {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name},
			})
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarProtocol{}).
		// Re-enqueue the protocol whenever a referencing binding changes.
		Watches(
			&pillarcsiv1alpha1.PillarStorageClass{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToProtocol),
		).
		// Re-enqueue protocol(s) whenever a PillarStore changes (activeAgents may change).
		Watches(
			&pillarcsiv1alpha1.PillarStore{},
			handler.EnqueueRequestsFromMapFunc(mapPoolToProtocol),
		).
		Named("pillarprotocol").
		Complete(r)
}
