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
	// deletion while PillarBindings still reference it.
	pillarProtocolFinalizer = "pillar-csi.bhyoo.com/protocol-protection"

	// Requeue interval before re-checking whether blocking PillarBindings have been removed.
	requeueAfterProtocolDeletionBlock = 10 * time.Second
)

// PillarProtocolReconciler reconciles a PillarProtocol object.
type PillarProtocolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarprotocols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarprotocols/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarProtocol the reconciler:
//  1. Adds a finalizer on first creation.
//  2. On normal operation: counts PillarBinding references and computes the
//     set of activeTargets (via Binding→Pool→Target chain), then updates status.
//  3. On deletion: blocks until no PillarBindings reference this protocol,
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
//  1. Lists all PillarBindings that reference this protocol to compute bindingCount.
//  2. For each referencing binding, looks up its PillarPool to collect the
//     pool's targetRef — building the deduplicated, sorted activeTargets list.
//  3. Writes bindingCount, activeTargets, and the Ready condition to status.
func (r *PillarProtocolReconciler) reconcileNormal(
	ctx context.Context,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// List all PillarBindings (cluster-scoped, no namespace filter).
	bindingList := &pillarcsiv1alpha1.PillarBindingList{}
	err := r.List(ctx, bindingList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarBindings: %w", err)
	}

	// Count references to this protocol and collect the referenced pool names.
	var count int32
	poolNames := make(map[string]struct{})
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.ProtocolRef == protocol.Name {
			count++
			poolNames[bindingList.Items[i].Spec.PoolRef] = struct{}{}
		}
	}

	log.Info("PillarProtocol binding count", "name", protocol.Name, "count", count)

	// For each referenced pool, look up its targetRef to build activeTargets.
	// We use a set to deduplicate (multiple bindings may share the same target).
	targetSet := make(map[string]struct{})
	for poolName := range poolNames {
		pool := &pillarcsiv1alpha1.PillarPool{}
		poolErr := r.Get(ctx, types.NamespacedName{Name: poolName}, pool)
		if poolErr != nil {
			if client.IgnoreNotFound(poolErr) != nil {
				return ctrl.Result{}, fmt.Errorf("failed to get PillarPool %q: %w", poolName, poolErr)
			}
			// Pool not found — binding may be in a degraded state; skip gracefully.
			log.V(1).Info("Referenced PillarPool not found; skipping for activeTargets computation",
				"protocol", protocol.Name, "pool", poolName)
			continue
		}
		if pool.Spec.TargetRef != "" {
			targetSet[pool.Spec.TargetRef] = struct{}{}
		}
	}

	// Convert the set to a sorted slice for deterministic output.
	activeTargets := make([]string, 0, len(targetSet))
	for t := range targetSet {
		activeTargets = append(activeTargets, t)
	}
	sort.Strings(activeTargets)

	log.Info("PillarProtocol active targets",
		"name", protocol.Name,
		"activeTargets", activeTargets,
		"count", len(activeTargets),
	)

	// Build updated status fields.
	protocol.Status.BindingCount = count
	protocol.Status.ActiveTargets = activeTargets

	meta.SetStatusCondition(&protocol.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: protocol.Generation,
		Reason:             "ProtocolConfigured",
		Message: fmt.Sprintf(
			"PillarProtocol is configured with type %q; referenced by %d binding(s) across %d active target(s)",
			protocol.Spec.Type, count, len(activeTargets),
		),
	})

	err = r.Status().Update(ctx, protocol)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarProtocol status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the deletion flow.  The finalizer is only removed
// once no PillarBindings reference this PillarProtocol.
func (r *PillarProtocolReconciler) reconcileDelete(
	ctx context.Context,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(protocol, pillarProtocolFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarProtocol is being deleted — checking for referencing PillarBindings", "name", protocol.Name)

	// List all PillarBindings and find those that reference this protocol.
	bindingList := &pillarcsiv1alpha1.PillarBindingList{}
	err := r.List(ctx, bindingList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarBindings: %w", err)
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
			"Deletion blocked: PillarBinding(s) [%s] still reference this protocol; delete them first",
			strings.Join(referencingNames, ", "),
		)
		log.Info(msg, "name", protocol.Name)

		//nolint:gosec // BindingCount is bounded by available cluster resources and cannot realistically overflow int32.
		protocol.Status.BindingCount = int32(len(referencingNames))
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
	log.Info("No PillarBindings reference this protocol; removing finalizer", "name", protocol.Name)
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
//   - PillarBinding: re-enqueues the referenced PillarProtocol whenever a
//     binding is created, updated, or deleted — so that bindingCount and the
//     deletion-gate stay consistent.
//   - PillarPool: re-enqueues the PillarProtocol(s) reachable via Pool→Binding→Protocol
//     whenever a pool changes — so that activeTargets stays consistent.
func (r *PillarProtocolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapBindingToProtocol extracts the protocolRef from a PillarBinding and
	// returns a reconcile.Request for the referenced PillarProtocol.
	mapBindingToProtocol := func(_ context.Context, obj client.Object) []reconcile.Request {
		binding, ok := obj.(*pillarcsiv1alpha1.PillarBinding)
		if !ok || binding.Spec.ProtocolRef == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: binding.Spec.ProtocolRef}},
		}
	}

	// mapPoolToProtocol: when a PillarPool changes, find all PillarBindings
	// that reference it, then collect the distinct set of protocolRefs and
	// enqueue each for reconciliation so activeTargets is recomputed.
	mapPoolToProtocol := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pool, ok := obj.(*pillarcsiv1alpha1.PillarPool)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarBindingList{}
		listErr := r.List(ctx, bindingList)
		if listErr != nil {
			// Cannot propagate error from a watch handler; log and return empty.
			logf.FromContext(ctx).Error(listErr,
				"Failed to list PillarBindings while mapping PillarPool event to PillarProtocol",
				"pool", pool.Name)
			return nil
		}

		protocolSet := make(map[string]struct{})
		for i := range bindingList.Items {
			b := &bindingList.Items[i]
			if b.Spec.PoolRef == pool.Name && b.Spec.ProtocolRef != "" {
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
			&pillarcsiv1alpha1.PillarBinding{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToProtocol),
		).
		// Re-enqueue protocol(s) whenever a PillarPool changes (activeTargets may change).
		Watches(
			&pillarcsiv1alpha1.PillarPool{},
			handler.EnqueueRequestsFromMapFunc(mapPoolToProtocol),
		).
		Named("pillarprotocol").
		Complete(r)
}
