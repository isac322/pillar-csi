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
//  3. When the target is not ready, sets PoolDiscovered and BackendSupported to Unknown.
//  4. When the target is ready, evaluates PoolDiscovered from target.Status.DiscoveredPools
//     and BackendSupported from target.Status.Capabilities.Backends.
//  5. Sets Ready=True only when TargetReady, PoolDiscovered, and BackendSupported are all True.
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

		// When the target itself is not ready, pool discovery and backend
		// verification cannot be performed; mark both Unknown.
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "PoolDiscovered",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message:            fmt.Sprintf("Cannot discover pool: PillarTarget %q is not yet Ready", pool.Spec.TargetRef),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "BackendSupported",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message:            fmt.Sprintf("Cannot verify backend support: PillarTarget %q is not yet Ready", pool.Spec.TargetRef),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message:            fmt.Sprintf("PillarPool is not ready: PillarTarget %q is not yet Ready", pool.Spec.TargetRef),
		})

		if err := r.Status().Update(ctx, pool); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarPool status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Target is ready — evaluate pool discovery and backend support from
	// the target's reported status (populated by the target reconciler when
	// the agent gRPC connection is established).

	pdStatus, pdReason, pdMsg := evaluatePoolDiscovered(pool, target)
	log.Info("PoolDiscovered evaluation", "pool", pool.Name, "status", pdStatus, "reason", pdReason)
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "PoolDiscovered",
		Status:             pdStatus,
		ObservedGeneration: pool.Generation,
		Reason:             pdReason,
		Message:            pdMsg,
	})

	bsStatus, bsReason, bsMsg := evaluateBackendSupported(pool, target)
	log.Info("BackendSupported evaluation", "pool", pool.Name, "status", bsStatus, "reason", bsReason)
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "BackendSupported",
		Status:             bsStatus,
		ObservedGeneration: pool.Generation,
		Reason:             bsReason,
		Message:            bsMsg,
	})

	// Ready is True only when all three positive conditions hold.
	allReady := pdStatus == metav1.ConditionTrue && bsStatus == metav1.ConditionTrue
	if allReady {
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: pool.Generation,
			Reason:             "AllConditionsMet",
			Message:            "PillarPool is ready: target is reachable, pool is discovered, and backend is supported",
		})
	} else {
		// Compute a descriptive message listing which conditions are not True.
		var notReady []string
		if pdStatus != metav1.ConditionTrue {
			notReady = append(notReady, fmt.Sprintf("PoolDiscovered (%s: %s)", pdReason, pdMsg))
		}
		if bsStatus != metav1.ConditionTrue {
			notReady = append(notReady, fmt.Sprintf("BackendSupported (%s: %s)", bsReason, bsMsg))
		}
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "ConditionsNotMet",
			Message:            fmt.Sprintf("PillarPool is not ready: %s", strings.Join(notReady, "; ")),
		})
	}

	if err := r.Status().Update(ctx, pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarPool status: %w", err)
	}

	return ctrl.Result{}, nil
}

// evaluatePoolDiscovered checks whether the pool named in spec.backend is
// present in the target's status.discoveredPools list.
//
// When the target has not yet reported any discovered pools (i.e. agent gRPC
// has not yet been established), it returns Unknown so that the caller can
// distinguish "we haven't checked yet" from "pool is not there".
//
// For ZFS backends, the pool name is taken from spec.backend.zfs.pool.
// For backends that do not carry an explicit pool name (lvm-lv, dir), pool
// discovery is considered satisfied once the target reports any pools,
// because those backend types manage their own namespacing differently.
func evaluatePoolDiscovered(pool *pillarcsiv1alpha1.PillarPool, target *pillarcsiv1alpha1.PillarTarget) (metav1.ConditionStatus, string, string) {
	if len(target.Status.DiscoveredPools) == 0 {
		return metav1.ConditionUnknown, "WaitingForAgentData",
			fmt.Sprintf("PillarTarget %q has not yet reported any discovered pools; waiting for agent gRPC connection", pool.Spec.TargetRef)
	}

	// Determine the expected pool name from the backend spec.
	var expectedPoolName string
	switch pool.Spec.Backend.Type {
	case pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.BackendTypeZFSDataset:
		if pool.Spec.Backend.ZFS != nil && pool.Spec.Backend.ZFS.Pool != "" {
			expectedPoolName = pool.Spec.Backend.ZFS.Pool
		}
	}

	if expectedPoolName == "" {
		// Backend type does not carry an explicit pool name (e.g. lvm-lv, dir).
		// Treat as discovered once the target reports it is responsive.
		return metav1.ConditionTrue, "PoolDiscovered",
			fmt.Sprintf("Backend type %q does not require a named pool for discovery validation", pool.Spec.Backend.Type)
	}

	// Search for the expected pool in the target's discovered list.
	var discoveredNames []string
	for _, dp := range target.Status.DiscoveredPools {
		discoveredNames = append(discoveredNames, dp.Name)
		if dp.Name == expectedPoolName {
			return metav1.ConditionTrue, "PoolDiscovered",
				fmt.Sprintf("Pool %q was found in PillarTarget %q discovered pools", expectedPoolName, pool.Spec.TargetRef)
		}
	}

	return metav1.ConditionFalse, "PoolNotFound",
		fmt.Sprintf("Pool %q was not found in PillarTarget %q discovered pools (found: [%s])",
			expectedPoolName, pool.Spec.TargetRef, strings.Join(discoveredNames, ", "))
}

// evaluateBackendSupported checks whether the backend type declared in
// spec.backend.type is listed in the target's capabilities.backends.
//
// Returns Unknown when the target has not yet reported any capabilities —
// this happens before the agent gRPC connection is established.
func evaluateBackendSupported(pool *pillarcsiv1alpha1.PillarPool, target *pillarcsiv1alpha1.PillarTarget) (metav1.ConditionStatus, string, string) {
	if target.Status.Capabilities == nil || len(target.Status.Capabilities.Backends) == 0 {
		return metav1.ConditionUnknown, "WaitingForAgentData",
			fmt.Sprintf("PillarTarget %q has not yet reported agent capabilities; waiting for agent gRPC connection", pool.Spec.TargetRef)
	}

	backendType := string(pool.Spec.Backend.Type)
	for _, b := range target.Status.Capabilities.Backends {
		if b == backendType {
			return metav1.ConditionTrue, "BackendSupported",
				fmt.Sprintf("Backend type %q is supported by PillarTarget %q", backendType, pool.Spec.TargetRef)
		}
	}

	return metav1.ConditionFalse, "BackendNotSupported",
		fmt.Sprintf("Backend type %q is not in the supported backends list of PillarTarget %q (supported: [%s])",
			backendType, pool.Spec.TargetRef, strings.Join(target.Status.Capabilities.Backends, ", "))
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
