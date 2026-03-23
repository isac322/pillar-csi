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
	// pillarProtocolFinalizer is added to every PillarProtocol to prevent
	// deletion while PillarBindings still reference it.
	pillarProtocolFinalizer = "pillar-csi.bhyoo.com/protocol-protection"

	// requeueAfterProtocolDeletionBlock is how long to wait before re-checking
	// whether blocking PillarBindings have been removed.
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarProtocol the reconciler:
//  1. Adds a finalizer on first creation.
//  2. On normal operation: counts PillarBinding references and updates status.
//  3. On deletion: blocks until no PillarBindings reference this protocol,
//     then removes the finalizer to allow the object to be garbage-collected.
func (r *PillarProtocolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarProtocol instance.
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	if err := r.Get(ctx, req.NamespacedName, protocol); err != nil {
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
		if err := r.Update(ctx, protocol); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, protocol)
}

// reconcileNormal handles the steady-state reconciliation of a PillarProtocol
// that is not being deleted.  It counts PillarBinding references and updates
// the status accordingly.
func (r *PillarProtocolReconciler) reconcileNormal(ctx context.Context, protocol *pillarcsiv1alpha1.PillarProtocol) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// List all PillarBindings (cluster-scoped, no namespace filter).
	bindingList := &pillarcsiv1alpha1.PillarBindingList{}
	if err := r.List(ctx, bindingList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarBindings: %w", err)
	}

	// Count references to this protocol.
	var count int32
	for i := range bindingList.Items {
		if bindingList.Items[i].Spec.ProtocolRef == protocol.Name {
			count++
		}
	}

	log.Info("PillarProtocol binding count", "name", protocol.Name, "count", count)

	// Build updated status.
	protocol.Status.BindingCount = count

	meta.SetStatusCondition(&protocol.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: protocol.Generation,
		Reason:             "ProtocolConfigured",
		Message: fmt.Sprintf(
			"PillarProtocol is configured with type %q and referenced by %d binding(s)",
			protocol.Spec.Type, count,
		),
	})

	if err := r.Status().Update(ctx, protocol); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarProtocol status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the deletion flow.  The finalizer is only removed
// once no PillarBindings reference this PillarProtocol.
func (r *PillarProtocolReconciler) reconcileDelete(ctx context.Context, protocol *pillarcsiv1alpha1.PillarProtocol) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(protocol, pillarProtocolFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarProtocol is being deleted — checking for referencing PillarBindings", "name", protocol.Name)

	// List all PillarBindings and find those that reference this protocol.
	bindingList := &pillarcsiv1alpha1.PillarBindingList{}
	if err := r.List(ctx, bindingList); err != nil {
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

		protocol.Status.BindingCount = int32(len(referencingNames))
		meta.SetStatusCondition(&protocol.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: protocol.Generation,
			Reason:             "DeletionBlocked",
			Message:            msg,
		})

		if err := r.Status().Update(ctx, protocol); err != nil {
			// Log but don't fail — the important thing is to requeue.
			log.Error(err, "Failed to update status while deletion is blocked")
		}

		return ctrl.Result{RequeueAfter: requeueAfterProtocolDeletionBlock}, nil
	}

	// No referencing bindings — safe to remove the finalizer.
	log.Info("No PillarBindings reference this protocol; removing finalizer", "name", protocol.Name)
	controllerutil.RemoveFinalizer(protocol, pillarProtocolFinalizer)
	if err := r.Update(ctx, protocol); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarProtocol: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
// It also watches PillarBinding objects and re-enqueues the referenced
// PillarProtocol whenever a binding is created, updated, or deleted — so that
// the protocol's bindingCount and deletion-gate stay consistent.
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarProtocol{}).
		// Re-enqueue the protocol whenever a referencing binding changes.
		Watches(
			&pillarcsiv1alpha1.PillarBinding{},
			handler.EnqueueRequestsFromMapFunc(mapBindingToProtocol),
		).
		Named("pillarprotocol").
		Complete(r)
}
