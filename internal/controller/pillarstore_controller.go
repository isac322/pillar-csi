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
// pillar-csi CRDs (PillarAgent, PillarStore, PillarProtocol, PillarStorageClass).
package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
	// Finalizer added to every PillarStore to prevent deletion
	// while PillarStorageClass resources still reference it.
	pillarStoreFinalizer = "pillar-csi.bhyoo.com/store-protection"

	// Requeue interval before re-checking whether blocking PillarStorageClasss have been removed.
	requeueAfterPoolDeletionBlock = 10 * time.Second
)

// PillarStoreReconciler reconciles a PillarStore object.
type PillarStoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstores/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillaragents,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarStore the reconciler:
//  1. Adds a finalizer on first creation (deletion protection).
//  2. On normal operation: looks up the referenced PillarAgent, updates the
//     TargetReady status condition, and sets PoolDiscovered / BackendSupported.
//  3. On deletion: blocks until no PillarStorageClasss reference this pool, then
//     removes the finalizer to allow the object to be garbage-collected.
//
//nolint:dupl // All four CRD controllers share identical Reconcile boilerplate; extraction requires reflection.
func (r *PillarStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarStore instance.
	pool := &pillarcsiv1alpha1.PillarStore{}
	err := r.Get(ctx, req.NamespacedName, pool)
	if err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarStore", "name", pool.Name, "deletionTimestamp", pool.DeletionTimestamp)

	// Branch: object is being deleted.
	if !pool.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, pool)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(pool, pillarStoreFinalizer) {
		log.Info("Adding finalizer to PillarStore", "finalizer", pillarStoreFinalizer)
		controllerutil.AddFinalizer(pool, pillarStoreFinalizer)
		err := r.Update(ctx, pool)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, pool)
}

// reconcileNormal handles the steady-state reconciliation of a PillarStore that
// is not being deleted.
//
// It:
//  1. Looks up the PillarAgent named in spec.agentRef.
//  2. Sets TargetReady based on whether the target exists and its Ready condition is True.
//  3. When the target is not ready, sets PoolDiscovered and BackendSupported to Unknown.
//  4. When the target is ready, evaluates PoolDiscovered from target.Status.DiscoveredPools
//     and BackendSupported from target.Status.Capabilities.Backends.
//  5. Sets Ready=True only when TargetReady, PoolDiscovered, and BackendSupported are all True.
//
//nolint:funlen,gocognit,gocyclo // Multiple code paths (target-not-found, not-ready, all-ready) update many conditions.
func (r *PillarStoreReconciler) reconcileNormal(
	ctx context.Context,
	pool *pillarcsiv1alpha1.PillarStore,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Look up the referenced PillarAgent.
	target := &pillarcsiv1alpha1.PillarAgent{}
	err := r.Get(ctx, types.NamespacedName{Name: pool.Spec.AgentRef}, target)

	switch {
	case err != nil && client.IgnoreNotFound(err) == nil:
		// Target does not exist.
		log.Info("Referenced PillarAgent not found", "pool", pool.Name, "target", pool.Spec.AgentRef)

		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "AgentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            fmt.Sprintf("PillarAgent %q was not found in the cluster", pool.Spec.AgentRef),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "PoolDiscovered",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            "Cannot discover pool: referenced PillarAgent does not exist",
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "BackendSupported",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message:            "Cannot verify backend support: referenced PillarAgent does not exist",
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotFound",
			Message: fmt.Sprintf(
				"PillarStore is not ready: PillarAgent %q was not found", pool.Spec.AgentRef,
			),
		})

		statusErr := r.Status().Update(ctx, pool)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStore status: %w", statusErr)
		}
		// No requeue — a PillarAgent watch will trigger reconcile when the target appears.
		return ctrl.Result{}, nil

	case err != nil:
		// Transient API error — report Unknown and let the controller requeue.
		log.Error(err, "Failed to get referenced PillarAgent", "pool", pool.Name, "target", pool.Spec.AgentRef)

		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "AgentReady",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetLookupError",
			Message: fmt.Sprintf(
				"Failed to look up PillarAgent %q: %v", pool.Spec.AgentRef, err,
			),
		})
		statusErr := r.Status().Update(ctx, pool)
		if statusErr != nil {
			log.Error(statusErr, "Failed to update PillarStore status after target lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PillarAgent %q: %w", pool.Spec.AgentRef, err)
	}

	// Target exists — check if it is being deleted before evaluating readiness.
	if !target.DeletionTimestamp.IsZero() {
		log.Info("Referenced PillarAgent is being deleted; marking pool TargetReady=False",
			"pool", pool.Name, "target", pool.Spec.AgentRef)
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "AgentReady",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetDeleting",
			Message: fmt.Sprintf(
				"PillarAgent %q is being deleted", pool.Spec.AgentRef,
			),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "PoolDiscovered",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetDeleting",
			Message: fmt.Sprintf(
				"Cannot discover pool: PillarAgent %q is being deleted", pool.Spec.AgentRef,
			),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "BackendSupported",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetDeleting",
			Message: fmt.Sprintf(
				"Cannot verify backend support: PillarAgent %q is being deleted", pool.Spec.AgentRef,
			),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetDeleting",
			Message: fmt.Sprintf(
				"PillarStore is not ready: PillarAgent %q is being deleted", pool.Spec.AgentRef,
			),
		})
		statusErr := r.Status().Update(ctx, pool)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStore status: %w", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// Target exists — check whether it reports Ready=True.
	targetReadyCond := meta.FindStatusCondition(target.Status.Conditions, "Ready")
	targetReady := targetReadyCond != nil && targetReadyCond.Status == metav1.ConditionTrue

	if targetReady {
		log.Info("Referenced PillarAgent is Ready", "pool", pool.Name, "target", pool.Spec.AgentRef)
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "AgentReady",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: pool.Generation,
			Reason:             "AgentReady",
			Message: fmt.Sprintf(
				"PillarAgent %q is present and in Ready state (address: %q)",
				pool.Spec.AgentRef, target.Status.ResolvedAddress,
			),
		})
	} else {
		msg := fmt.Sprintf("PillarAgent %q exists but is not yet Ready", pool.Spec.AgentRef)
		if targetReadyCond != nil {
			msg = fmt.Sprintf(
				"PillarAgent %q is not Ready: %s",
				pool.Spec.AgentRef, targetReadyCond.Message,
			)
		}
		log.Info("Referenced PillarAgent is not Ready", "pool", pool.Name, "target", pool.Spec.AgentRef)
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "AgentReady",
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
			Message: fmt.Sprintf(
				"Cannot discover pool: PillarAgent %q is not yet Ready", pool.Spec.AgentRef,
			),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "BackendSupported",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message: fmt.Sprintf(
				"Cannot verify backend support: PillarAgent %q is not yet Ready", pool.Spec.AgentRef,
			),
		})
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: pool.Generation,
			Reason:             "TargetNotReady",
			Message: fmt.Sprintf(
				"PillarStore is not ready: PillarAgent %q is not yet Ready", pool.Spec.AgentRef,
			),
		})

		statusErr := r.Status().Update(ctx, pool)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStore status: %w", statusErr)
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

	// --- Capacity sync from agent data ---
	// When the pool is confirmed discovered we attempt to pull capacity
	// (Total, Available, Used) from the matching DiscoveredPool entry that
	// the target reconciler populates once the agent gRPC connection is
	// established.  If the pool is not discovered (or its state is unknown)
	// we clear any stale capacity so callers get an accurate picture.
	if pdStatus == metav1.ConditionTrue {
		if synced := syncCapacityFromTarget(pool, target); synced {
			log.Info("Synced capacity from DiscoveredPool",
				"pool", pool.Name,
				"total", pool.Status.Capacity.Total,
				"available", pool.Status.Capacity.Available,
				"used", pool.Status.Capacity.Used,
			)
		} else {
			log.V(1).Info("Pool is discovered but no capacity data available yet", "pool", pool.Name)
		}
	} else {
		// Pool not yet discovered or state is unknown — clear stale capacity.
		pool.Status.Capacity = nil
	}

	// --- Top-level Ready condition ---
	// Ready is True only when TargetReady, PoolDiscovered, and BackendSupported
	// are all True.  TargetReady is already confirmed True at this point in the
	// code (the function returns early above when it is False), so we only need
	// to check the two remaining conditions explicitly.
	allReady := pdStatus == metav1.ConditionTrue && bsStatus == metav1.ConditionTrue
	if allReady {
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: pool.Generation,
			Reason:             "AllConditionsMet",
			Message:            "PillarStore is ready: target is reachable, pool is discovered, and backend is supported",
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
			Message:            fmt.Sprintf("PillarStore is not ready: %s", strings.Join(notReady, "; ")),
		})
	}

	err = r.Status().Update(ctx, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarStore status: %w", err)
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
func evaluatePoolDiscovered(
	pool *pillarcsiv1alpha1.PillarStore,
	target *pillarcsiv1alpha1.PillarAgent,
) (status metav1.ConditionStatus, reason, message string) {
	if len(target.Status.DiscoveredPools) == 0 {
		return metav1.ConditionUnknown, "WaitingForAgentData",
			fmt.Sprintf(
				"PillarAgent %q has not yet reported any discovered pools; waiting for agent gRPC connection",
				pool.Spec.AgentRef,
			)
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
			fmt.Sprintf(
				"Backend type %q does not require a named pool for discovery validation",
				pool.Spec.Backend.Type,
			)
	}

	// Search for the expected pool in the target's discovered list.
	var discoveredNames []string
	for _, dp := range target.Status.DiscoveredPools {
		discoveredNames = append(discoveredNames, dp.Name)
		if dp.Name == expectedPoolName {
			return metav1.ConditionTrue, "PoolDiscovered",
				fmt.Sprintf(
					"Pool %q was found in PillarAgent %q discovered pools",
					expectedPoolName, pool.Spec.AgentRef,
				)
		}
	}

	return metav1.ConditionFalse, "PoolNotFound",
		fmt.Sprintf("Pool %q was not found in PillarAgent %q discovered pools (found: [%s])",
			expectedPoolName, pool.Spec.AgentRef, strings.Join(discoveredNames, ", "))
}

// evaluateBackendSupported checks whether the backend type declared in
// spec.backend.type is listed in the target's capabilities.backends.
//
// Returns Unknown when the target has not yet reported any capabilities —
// this happens before the agent gRPC connection is established.
func evaluateBackendSupported(
	pool *pillarcsiv1alpha1.PillarStore,
	target *pillarcsiv1alpha1.PillarAgent,
) (status metav1.ConditionStatus, reason, message string) {
	if target.Status.Capabilities == nil || len(target.Status.Capabilities.Backends) == 0 {
		return metav1.ConditionUnknown, "WaitingForAgentData",
			fmt.Sprintf(
				"PillarAgent %q has not yet reported agent capabilities; waiting for agent gRPC connection",
				pool.Spec.AgentRef,
			)
	}

	backendType := string(pool.Spec.Backend.Type)
	if slices.Contains(target.Status.Capabilities.Backends, backendType) {
		return metav1.ConditionTrue, "BackendSupported",
			fmt.Sprintf(
				"Backend type %q is supported by PillarAgent %q", backendType, pool.Spec.AgentRef,
			)
	}

	return metav1.ConditionFalse, "BackendNotSupported",
		fmt.Sprintf("Backend type %q is not in the supported backends list of PillarAgent %q (supported: [%s])",
			backendType, pool.Spec.AgentRef, strings.Join(target.Status.Capabilities.Backends, ", "))
}

// syncCapacityFromTarget reads capacity data for this pool from the matching
// entry in target.Status.DiscoveredPools and writes it to pool.Status.Capacity.
//
// Matching logic:
//   - ZFS backends (zfs-zvol, zfs-dataset): match by pool name (spec.backend.zfs.pool).
//   - Other backends (lvm-lv, dir): no named pool — match the first DiscoveredPool entry.
//
// The function computes Used = Total − Available when both quantities are present,
// clamping the result at zero to protect against corrupted agent data.
//
// Returns true when capacity fields were updated, false when no matching entry
// or no capacity data was found (both Total and Available are nil).
func syncCapacityFromTarget(
	pool *pillarcsiv1alpha1.PillarStore,
	target *pillarcsiv1alpha1.PillarAgent,
) bool {
	if len(target.Status.DiscoveredPools) == 0 {
		return false
	}

	// Resolve the pool name we expect to match (ZFS uses named pools; other
	// backend types do not carry an explicit pool name).
	var expectedName string
	switch pool.Spec.Backend.Type {
	case pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.BackendTypeZFSDataset:
		if pool.Spec.Backend.ZFS != nil {
			expectedName = pool.Spec.Backend.ZFS.Pool
		}
	}

	// Walk the discovered pool list and find the first matching entry.
	var found *pillarcsiv1alpha1.DiscoveredPool
	for i := range target.Status.DiscoveredPools {
		dp := &target.Status.DiscoveredPools[i]
		if expectedName == "" || dp.Name == expectedName {
			found = dp
			break
		}
	}

	if found == nil {
		return false
	}

	// Require at least one capacity field; an entry with no capacity data
	// (Total == nil && Available == nil) carries no actionable information.
	if found.Total == nil && found.Available == nil {
		return false
	}

	poolCap := &pillarcsiv1alpha1.StoreCapacity{
		Total:     found.Total,
		Available: found.Available,
	}

	// Compute Used = Total − Available when both values are present.
	if found.Total != nil && found.Available != nil {
		used := found.Total.DeepCopy()
		used.Sub(*found.Available)

		// Guard against negative Used that can arise from corrupted agent data
		// (e.g. Available > Total due to a reporting race).
		zero := resource.MustParse("0")
		if used.Cmp(zero) < 0 {
			used = zero
		}
		poolCap.Used = &used
	}

	pool.Status.Capacity = poolCap
	return true
}

// reconcileDelete handles the deletion flow.  The finalizer is only removed
// once no PillarStorageClasss reference this PillarStore.
func (r *PillarStoreReconciler) reconcileDelete(
	ctx context.Context,
	pool *pillarcsiv1alpha1.PillarStore,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(pool, pillarStoreFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarStore is being deleted — checking for referencing PillarStorageClasss", "name", pool.Name)

	// List all PillarStorageClasss (cluster-scoped) and find those that reference this pool.
	bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
	err := r.List(ctx, bindingList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarStorageClasss: %w", err)
	}

	var referencingNames []string
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.StoreRef == pool.Name {
			referencingNames = append(referencingNames, bindingList.Items[i].Name)
		}
	}

	if len(referencingNames) > 0 {
		// Deletion is blocked — log the reason and requeue.
		msg := fmt.Sprintf(
			"Deletion blocked: PillarStorageClass(s) [%s] still reference this pool; delete them first",
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

		statusErr := r.Status().Update(ctx, pool)
		if statusErr != nil {
			// Log but don't fail — the important thing is to requeue.
			log.Error(statusErr, "Failed to update status while deletion is blocked")
		}

		// Requeue after a short delay so we re-check once the operator has had
		// a chance to remove the blocking PillarStorageClasss.
		return ctrl.Result{RequeueAfter: requeueAfterPoolDeletionBlock}, nil
	}

	// No referencing bindings — safe to remove the finalizer.
	log.Info("No PillarStorageClasss reference this pool; removing finalizer", "name", pool.Name)
	controllerutil.RemoveFinalizer(pool, pillarStoreFinalizer)
	err = r.Update(ctx, pool)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarStore: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// The controller watches:
//   - PillarStore (primary resource)
//   - PillarAgent: re-enqueues pools referencing a target whenever the target
//     changes — so TargetReady condition stays current.
//   - PillarStorageClass: re-enqueues the PillarStore named in a binding's storeRef
//     whenever a binding is deleted — so deletion-blocking is lifted promptly.
func (r *PillarStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapTargetToPools returns reconcile Requests for every PillarStore whose
	// spec.agentRef matches the PillarAgent that just changed.
	mapTargetToPools := func(ctx context.Context, obj client.Object) []reconcile.Request {
		target, ok := obj.(*pillarcsiv1alpha1.PillarAgent)
		if !ok {
			return nil
		}

		poolList := &pillarcsiv1alpha1.PillarStoreList{}
		err := mgr.GetClient().List(ctx, poolList)
		if err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range poolList.Items {
			if poolList.Items[i].Spec.AgentRef == target.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: poolList.Items[i].Name},
				})
			}
		}
		return requests
	}

	// mapBindingToPool re-enqueues the PillarStore referenced by a changed
	// PillarStorageClass.  This ensures that when the last blocking binding is
	// deleted the pool's finalizer is removed promptly (instead of waiting for
	// the RequeueAfter timer).
	mapBindingToPool := func(_ context.Context, obj client.Object) []reconcile.Request {
		binding, ok := obj.(*pillarcsiv1alpha1.PillarStorageClass)
		if !ok || binding.Spec.StoreRef == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: binding.Spec.StoreRef}},
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarStore{}).
		// Re-enqueue PillarStores whenever the referenced PillarAgent changes.
		Watches(
			&pillarcsiv1alpha1.PillarAgent{},
			handler.EnqueueRequestsFromMapFunc(mapTargetToPools),
		).
		// Re-enqueue a PillarStore when any of its referencing PillarStorageClasss
		// change (e.g. deletion) so deletion-blocking is lifted quickly.
		Watches(
			&pillarcsiv1alpha1.PillarStorageClass{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToPool),
		).
		Named("pillarstore").
		Complete(r)
}
