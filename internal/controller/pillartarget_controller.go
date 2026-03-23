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
	"net"

	corev1 "k8s.io/api/core/v1"
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
	// pillarTargetFinalizer is added to every PillarTarget to support
	// ordered deletion (e.g. blocked by referencing PillarPools in future steps).
	pillarTargetFinalizer = "pillar-csi.bhyoo.com/target-protection"

	// defaultAgentPort is the gRPC port used when no port override is set.
	defaultAgentPort int32 = 9500
)

// PillarTargetReconciler reconciles a PillarTarget object.
type PillarTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarTarget the reconciler:
//  1. Adds a finalizer on first creation (deletion protection for future steps).
//  2. On normal operation: resolves the node IP from nodeRef (or uses the
//     external address directly) and updates the NodeExists status condition.
//  3. On deletion: removes the finalizer to allow garbage collection.
func (r *PillarTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarTarget instance.
	target := &pillarcsiv1alpha1.PillarTarget{}
	if err := r.Get(ctx, req.NamespacedName, target); err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarTarget", "name", target.Name, "deletionTimestamp", target.DeletionTimestamp)

	// Branch: object is being deleted.
	if !target.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, target)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(target, pillarTargetFinalizer) {
		log.Info("Adding finalizer to PillarTarget", "finalizer", pillarTargetFinalizer)
		controllerutil.AddFinalizer(target, pillarTargetFinalizer)
		if err := r.Update(ctx, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, target)
}

// reconcileNormal handles the steady-state reconciliation of a PillarTarget
// that is not being deleted.  It resolves the agent address and updates the
// NodeExists status condition.
func (r *PillarTargetReconciler) reconcileNormal(ctx context.Context, target *pillarcsiv1alpha1.PillarTarget) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	switch {
	case target.Spec.NodeRef != nil:
		return r.reconcileNodeRef(ctx, target)

	case target.Spec.External != nil:
		// External mode: NodeExists is not applicable.
		resolved := fmt.Sprintf("%s:%d", target.Spec.External.Address, target.Spec.External.Port)
		log.Info("PillarTarget uses external address", "name", target.Name, "address", resolved)

		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "NodeExists",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: target.Generation,
			Reason:             "ExternalMode",
			Message:            "PillarTarget uses an external address; NodeExists condition does not apply",
		})
		target.Status.ResolvedAddress = resolved

		if err := r.Status().Update(ctx, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
		}
		return ctrl.Result{}, nil

	default:
		// Neither nodeRef nor external is set — webhook should prevent this,
		// but report it as Unknown so operators can see the misconfiguration.
		log.Info("PillarTarget has no nodeRef or external spec", "name", target.Name)

		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "NodeExists",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: target.Generation,
			Reason:             "MissingSpec",
			Message:            "Neither spec.nodeRef nor spec.external is set; exactly one must be provided",
		})
		target.Status.ResolvedAddress = ""

		if err := r.Status().Update(ctx, target); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
		}
		return ctrl.Result{}, nil
	}
}

// reconcileNodeRef fetches the referenced Kubernetes Node, resolves the agent
// IP according to the addressType and optional CIDR filter, then updates the
// NodeExists condition and resolvedAddress status fields.
func (r *PillarTargetReconciler) reconcileNodeRef(ctx context.Context, target *pillarcsiv1alpha1.PillarTarget) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	nodeRef := target.Spec.NodeRef

	// Look up the referenced node.
	node := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: nodeRef.Name}, node)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			// Node does not exist in the cluster.
			log.Info("Referenced node not found", "name", target.Name, "node", nodeRef.Name)

			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "NodeExists",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "NodeNotFound",
				Message:            fmt.Sprintf("Node %q was not found in the cluster", nodeRef.Name),
			})
			target.Status.ResolvedAddress = ""

			if statusErr := r.Status().Update(ctx, target); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", statusErr)
			}
			// No requeue — a Node watch will trigger reconcile when the node appears.
			return ctrl.Result{}, nil
		}
		// Transient API error — report Unknown and let the controller requeue.
		log.Error(err, "Failed to get referenced node", "node", nodeRef.Name)

		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "NodeExists",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: target.Generation,
			Reason:             "NodeLookupError",
			Message:            fmt.Sprintf("Failed to look up Node %q: %v", nodeRef.Name, err),
		})
		if statusErr := r.Status().Update(ctx, target); statusErr != nil {
			log.Error(statusErr, "Failed to update PillarTarget status after node lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get node %q: %w", nodeRef.Name, err)
	}

	// Node exists — resolve IP address.
	ip, resolveErr := resolveNodeAddress(node, nodeRef)
	if resolveErr != nil {
		log.Info("Node exists but address resolution failed",
			"name", target.Name, "node", nodeRef.Name, "error", resolveErr)

		// Node exists but address is unresolvable — report NodeExists=True
		// but clear resolvedAddress so downstream controllers don't proceed.
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "NodeExists",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: target.Generation,
			Reason:             "AddressNotResolved",
			Message: fmt.Sprintf(
				"Node %q exists but no matching address found (type=%q, selector=%q): %v",
				nodeRef.Name, nodeRef.AddressType, nodeRef.AddressSelector, resolveErr,
			),
		})
		target.Status.ResolvedAddress = ""

		if statusErr := r.Status().Update(ctx, target); statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// Address resolved successfully.
	port := defaultAgentPort
	if nodeRef.Port != nil {
		port = *nodeRef.Port
	}
	resolved := fmt.Sprintf("%s:%d", ip, port)

	log.Info("PillarTarget node address resolved",
		"name", target.Name, "node", nodeRef.Name, "address", resolved)

	meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
		Type:               "NodeExists",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: target.Generation,
		Reason:             "NodeFound",
		Message: fmt.Sprintf(
			"Node %q is present in the cluster; resolved agent address %q",
			nodeRef.Name, resolved,
		),
	})
	target.Status.ResolvedAddress = resolved

	if err := r.Status().Update(ctx, target); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileDelete handles the deletion flow.  Currently the finalizer is
// removed immediately; future steps will add blocking logic (e.g. wait until
// all referencing PillarPools are gone).
func (r *PillarTargetReconciler) reconcileDelete(ctx context.Context, target *pillarcsiv1alpha1.PillarTarget) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(target, pillarTargetFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarTarget is being deleted; removing finalizer", "name", target.Name)

	controllerutil.RemoveFinalizer(target, pillarTargetFinalizer)
	if err := r.Update(ctx, target); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarTarget: %w", err)
	}

	return ctrl.Result{}, nil
}

// resolveNodeAddress picks an IP address from node.Status.Addresses according
// to the address type and optional CIDR filter defined in nodeRef.
// Returns an error if no matching address is found.
func resolveNodeAddress(node *corev1.Node, nodeRef *pillarcsiv1alpha1.NodeRefSpec) (string, error) {
	addressType := corev1.NodeAddressType(nodeRef.AddressType)
	if addressType == "" {
		addressType = corev1.NodeInternalIP
	}

	// Parse CIDR filter if provided.
	var cidr *net.IPNet
	if nodeRef.AddressSelector != "" {
		_, parsed, err := net.ParseCIDR(nodeRef.AddressSelector)
		if err != nil {
			return "", fmt.Errorf("invalid addressSelector CIDR %q: %w", nodeRef.AddressSelector, err)
		}
		cidr = parsed
	}

	for _, addr := range node.Status.Addresses {
		if addr.Type != addressType {
			continue
		}
		ip := net.ParseIP(addr.Address)
		if ip == nil {
			// Not a bare IP (could be a hostname); skip for now.
			continue
		}
		if cidr != nil && !cidr.Contains(ip) {
			continue
		}
		return addr.Address, nil
	}

	if cidr != nil {
		return "", fmt.Errorf(
			"no %q address on node %q within CIDR %q",
			addressType, node.Name, nodeRef.AddressSelector,
		)
	}
	return "", fmt.Errorf("no %q address found on node %q", addressType, node.Name)
}

// SetupWithManager sets up the controller with the Manager.
// It also watches Node objects and re-enqueues any PillarTarget that references
// a changed node — so that NodeExists and resolvedAddress stay current when
// nodes are added, updated, or removed.
func (r *PillarTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapNodeToTargets returns reconcile Requests for every PillarTarget whose
	// spec.nodeRef.name matches the node that just changed.
	mapNodeToTargets := func(ctx context.Context, obj client.Object) []reconcile.Request {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return nil
		}

		targetList := &pillarcsiv1alpha1.PillarTargetList{}
		if err := mgr.GetClient().List(ctx, targetList); err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range targetList.Items {
			t := &targetList.Items[i]
			if t.Spec.NodeRef != nil && t.Spec.NodeRef.Name == node.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: t.Name},
				})
			}
		}
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarTarget{}).
		// Re-enqueue PillarTargets whenever the referenced Node changes.
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(mapNodeToTargets),
		).
		Named("pillartarget").
		Complete(r)
}
