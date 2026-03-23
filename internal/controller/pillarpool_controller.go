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

// Package controller implements the Kubernetes reconciliation loops for
// pillar-csi CRDs (PillarTarget, PillarPool, PillarProtocol, PillarBinding).
package controller

import (
	"context"
	"fmt"
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
	// pillarPoolFinalizer is added to every PillarPool to prevent deletion
	// while PillarBinding resources still reference it.
	pillarPoolFinalizer = "pillar-csi.bhyoo.com/pool-protection"

	// requeueAfterPoolDeletionBlock is how long to wait before re-checking
	// whether blocking PillarBindings have been removed.
	requeueAfterPoolDeletionBlock = 10 * time.Second
)

// PillarPoolReconciler reconciles a PillarPool object.
type PillarPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarPool the reconciler:
//  1. Adds a finalizer on first creation (deletion protection).
//  2. On normal operation: looks up the referenced PillarTarget, updates the
//     TargetReady status condition, and sets PoolDiscovered / BackendSupported
//     (stubbed False — awaiting gRPC agent integration in a later task).
//  3. On deletion: blocks until no PillarBindings reference this pool, then
//     removes the finalizer to allow the object to be garbage-collected.
func (r *PillarPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarPool instance.
	pool := &pillarcsiv1alpha1.PillarPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarPool", "name", pool.Name, "deletionTimestamp", pool.DeletionTimestamp)

	// Branch: object is being deleted.
	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, pool)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(pool, pillarPoolFinalizer) {
		log.Info("Adding finalizer to PillarPool", "finalizer", pillarPoolFinalizer)
		controllerutil.AddFinalizer(pool, pillarPoolFinalizer)
		if err := r.Update(ctx, pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, pool)
}

// reconcileNormal handles the steady-state reconciliation of a PillarPool that
// is not being deleted.
//
// It:
//  1. Looks up the PillarTarget named in spec.targetRef.
//  2. Sets TargetReady based on whether the target exists and its Ready condition is True.
//  3. Sets PoolDiscovered and BackendSupported to False (stubbed — gRPC not yet wired).
//  4. Sets Ready to False because PoolDiscovered / BackendSupported are stubbed.
func (r *PillarPoolReconciler) reconcileNormal(ctx context.Context, pool *pillarcsiv1alpha1.PillarPool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Look up the referenced PillarTarget.
	target := &pillarcsiv1alpha1.PillarTarget{}
	err := r.Get(ctx, types.NamespacedName{Name: pool.Spec.TargetRef}, target)

	switch {
	case err != nil && client.IgnoreNotFound(err) == nil:
		// Target does not exist.
		log.Info("Referenced PillarTarget not found", "pool", pool.Name, "target", pool.Spec.TargetRef)

		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "TargetReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            fmt.Sprintf("PillarTarget %q was not found in the cluster", pool.Spec.TargetRef),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "PoolDiscovered",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            "Cannot discover pool: referenced PillarTarget does not exist",
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "BackendSupported",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            "Cannot verify backend support: referenced PillarTarget does not exist",
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            fmt.Sprintf("PillarPool is not ready: PillarTarget %q was not found", pool.Spec.TargetRef),
		})

		if statusErr := r.Status().Update(ctx, pool); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarPool status: %w", statusErr)
		}
		// No requeue — a PillarTarget watch will trigger reconcile when the target appears.
		return ctrl.Result{}, nil

	case err != nil:
		// Transient API error — report Unknown and let the controller requeue.
		log.Error(err, "Failed to get referenced PillarTarget", "pool", pool.Name, "target", pool.Spec.TargetRef)

		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "TargetReady",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetLookupError",
			Message:            fmt.Sprintf("Failed to look up PillarTarget %q: %v", pool.Spec.TargetRef, err),
		})
		if statusErr := r.Status().Update(ctx, pool); statusErr != nil {
			log.Error(statusErr, "Failed to update PillarPool status after target lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PillarTarget %q: %w", pool.Spec.TargetRef, err)
	}

	// Target exists — check whether it reports Ready=True.
	targetReadyCond := meta.FindStatusCondition(target.Status.Conditions, "Ready")
	targetReady := targetReadyCond != nil && targetReadyCond.Status == metav1.ConditionTrue

	if targetReady {
		log.Info("Referenced PillarTarget is Ready", "pool", pool.Name, "target", pool.Spec.TargetRef)
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "TargetReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetReady",
			Message: fmt.Sprintf(
				"PillarTarget %q is present and in Ready state (address: %q)",
				pool.Spec.TargetRef, target.Status.ResolvedAddress,
			),
		})
	} else {
		msg := fmt.Sprintf("PillarTarget %q exists but is not yet Ready", pool.Spec.TargetRef)
		if targetReadyCond != nil {
			msg = fmt.Sprintf(
				"PillarTarget %q is not Ready: %s",
				pool.Spec.TargetRef, targetReadyCond.Message,
			)
		}
		log.Info("Referenced PillarTarget is not Ready", "pool", pool.Name, "target", pool.Spec.TargetRef)
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "TargetReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message:            msg,
		})
	}

	// PoolDiscovered: stubbed False until gRPC agent integration (Task 3).
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "PoolDiscovered",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pool.Generation,
		Reason:             "AgentConnectionNotImplemented",
		Message:            "Pool discovery via agent gRPC is not yet implemented; will be enabled in a future task",
	})

	// BackendSupported: stubbed False until gRPC agent integration (Task 3).
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "BackendSupported",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pool.Generation,
		Reason:             "AgentConnectionNotImplemented",
		Message:            fmt.Sprintf("Backend support verification for type %q is not yet implemented; will be enabled in a future task", pool.Spec.Backend.Type),
	})

	// Ready: False because PoolDiscovered and BackendSupported are still stubbed.
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: pool.Generation,
		Reason:             "AgentConnectionNotImplemented",
		Message:            "PillarPool is not ready: pool discovery and backend verification require agent gRPC connection (not yet implemented)",
	})

	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarPool status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the deletion flow.  The finalizer is only removed
// once no PillarBindings reference this PillarPool.
func (r *PillarPoolReconciler) reconcileDelete(ctx context.Context, pool *pillarcsiv1alpha1.PillarPool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(pool, pillarPoolFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarPool is being deleted — checking for referencing PillarBindings", "name", pool.Name)

	// List all PillarBindings (cluster-scoped) and find those that reference this pool.
	bindingList := &pillarcsiv1alpha1.PillarBindingList{}
	if err := r.List(ctx, bindingList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarBindings: %w", err)
	}

	var referencingNames []string
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.PoolRef == pool.Name {
			referencingNames = append(referencingNames, bindingList.Items[i].Name)
		}
	}

	if len(referencingNames) > 0 {
		// Deletion is blocked — log the reason and requeue.
		msg := fmt.Sprintf(
			"Deletion blocked: PillarBinding(s) [%s] still reference this pool; delete them first",
			strings.Join(referencingNames, ", "),
		)
		log.Info(msg, "name", pool.Name)

		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "DeletionBlocked",
			Message:            msg,
		})

		if statusErr := r.Status().Update(ctx, pool); statusErr != nil {
			// Log but don't fail — the important thing is to requeue.
			log.Error(statusErr, "Failed to update status while deletion is blocked")
		}

		// Requeue after a short delay so we re-check once the operator has had
		// a chance to remove the blocking PillarBindings.
		return ctrl.Result{RequeueAfter: requeueAfterPoolDeletionBlock}, nil
	}

	// No referencing bindings — safe to remove the finalizer.
	log.Info("No PillarBindings reference this pool; removing finalizer", "name", pool.Name)
	controllerutil.RemoveFinalizer(pool, pillarPoolFinalizer)
	if err := r.Update(ctx, pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarPool: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// The controller watches:
//   - PillarPool (primary resource)
//   - PillarTarget: re-enqueues pools referencing a target whenever the target
//     changes — so TargetReady condition stays current.
//   - PillarBinding: re-enqueues the PillarPool named in a binding's poolRef
//     whenever a binding is deleted — so deletion-blocking is lifted promptly.
func (r *PillarPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapTargetToPools returns reconcile Requests for every PillarPool whose
	// spec.targetRef matches the PillarTarget that just changed.
	mapTargetToPools := func(ctx context.Context, obj client.Object) []reconcile.Request {
		target, ok := obj.(*pillarcsiv1alpha1.PillarTarget)
		if !ok {
			return nil
		}

		poolList := &pillarcsiv1alpha1.PillarPoolList{}
		if err := mgr.GetClient().List(ctx, poolList); err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range poolList.Items {
			if poolList.Items[i].Spec.TargetRef == target.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: poolList.Items[i].Name},
				})
			}
		}
		return requests
	}

	// mapBindingToPool re-enqueues the PillarPool referenced by a changed
	// PillarBinding.  This ensures that when the last blocking binding is
	// deleted the pool's finalizer is removed promptly (instead of waiting for
	// the RequeueAfter timer).
	mapBindingToPool := func(_ context.Context, obj client.Object) []reconcile.Request {
		binding, ok := obj.(*pillarcsiv1alpha1.PillarBinding)
		if !ok || binding.Spec.PoolRef == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: binding.Spec.PoolRef}},
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarPool{}).
		// Re-enqueue PillarPools whenever the referenced PillarTarget changes.
		Watches(
			&pillarcsiv1alpha1.PillarTarget{},
			handler.EnqueueRequestsFromMapFunc(mapTargetToPools),
		).
		// Re-enqueue a PillarPool when any of its referencing PillarBindings
		// change (e.g. deletion) so deletion-blocking is lifted quickly.
		Watches(
			&pillarcsiv1alpha1.PillarBinding{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToPool),
		).
		Named("pillarpool").
		Complete(r)
}
