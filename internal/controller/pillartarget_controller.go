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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agentclient"
)

const (
	// Finalizer added to every PillarTarget to prevent deletion
	// while PillarPool resources still reference it.
	pillarTargetFinalizer = "pillar-csi.bhyoo.com/pillar-target-protection"

	// Default gRPC port used when no port override is set.
	defaultAgentPort int32 = 9500

	// Label applied to the Kubernetes Node referenced by a
	// PillarTarget to mark it as a pillar-csi storage node.
	storageNodeLabel = "pillar-csi.bhyoo.com/storage-node"

	// Requeue interval before re-checking whether blocking PillarPools have been removed.
	requeueAfterTargetDeletionBlock = 10 * time.Second

	// Requeue interval for periodic agent connectivity re-checks.
	// Both the connected and disconnected cases requeue at this interval so
	// that transient failures are retried and live agents are re-verified.
	requeueAfterAgentHealthCheck = 30 * time.Second

	// Timeout for a single agent health-check RPC call.
	agentHealthCheckTimeout = 5 * time.Second

	// Timeout for a single agent GetCapabilities RPC call.
	agentCapabilitiesTimeout = 5 * time.Second
)

// PillarTargetReconciler reconciles a PillarTarget object.
type PillarTargetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Dialer is the gRPC connection manager used to verify agent connectivity.
	// When set, reconcileNormal and reconcileNodeRef will issue a live
	// HealthCheck RPC against the resolved agent address and reflect the result
	// in the AgentConnected and Ready status conditions.
	//
	// If Dialer is nil (e.g. in unit tests that do not exercise gRPC), the
	// reconciler sets AgentConnected=False with reason "DialerNotConfigured".
	//
	// The AgentConnected condition reason reflects the authentication level:
	//   Dialer.IsMTLS()==true  → reason "Authenticated" (mTLS handshake verified)
	//   Dialer.IsMTLS()==false → reason "Dialed"        (plain TCP, no TLS)
	// Use agentclient.NewManagerFromFiles or NewManagerWithTLSCredentials to
	// create a Dialer that enforces mTLS and reports IsMTLS()==true.
	Dialer agentclient.Dialer
}

// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarTarget the reconciler:
//  1. Adds a finalizer on first creation (deletion protection for future steps).
//  2. On normal operation: resolves the node IP from nodeRef (or uses the
//     external address directly) and updates the NodeExists status condition.
//  3. On deletion: removes the finalizer to allow garbage collection.
//
//nolint:dupl // All four CRD controllers share identical Reconcile boilerplate; extraction requires reflection.
func (r *PillarTargetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarTarget instance.
	target := &pillarcsiv1alpha1.PillarTarget{}
	err := r.Get(ctx, req.NamespacedName, target)
	if err != nil {
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
		err := r.Update(ctx, target)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, target)
}

// reconcileNormal handles the steady-state reconciliation of a PillarTarget
// that is not being deleted.  It resolves the agent address, updates the
// NodeExists status condition, performs a live gRPC HealthCheck via r.Dialer
// to set AgentConnected, and derives Ready accordingly.
func (r *PillarTargetReconciler) reconcileNormal(
	ctx context.Context,
	target *pillarcsiv1alpha1.PillarTarget,
) (ctrl.Result, error) {
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

		// AgentConnected: perform a live gRPC HealthCheck against the agent.
		connected := r.setAgentConnectedCondition(ctx, target, resolved)

		// Ready: True only when the agent is reachable and healthy.
		if connected {
			// Best-effort: populate AgentVersion / Capabilities / DiscoveredPools.
			r.populateCapabilitiesStatus(ctx, target, resolved)

			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				ObservedGeneration: target.Generation,
				Reason:             "AgentConnected",
				Message:            fmt.Sprintf("PillarTarget is ready: agent at %q is connected and healthy", resolved),
			})
		} else {
			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "AgentNotConnected",
				Message:            "PillarTarget is not ready: agent gRPC connection has not been established",
			})
		}

		err := r.Status().Update(ctx, target)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
		}
		// Requeue periodically to re-verify agent connectivity.
		return ctrl.Result{RequeueAfter: requeueAfterAgentHealthCheck}, nil

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
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: target.Generation,
			Reason:             "MissingSpec",
			Message:            "Neither spec.nodeRef nor spec.external is set; cannot determine agent address",
		})
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "MissingSpec",
			Message:            "PillarTarget spec is invalid: neither spec.nodeRef nor spec.external is set",
		})
		target.Status.ResolvedAddress = ""

		err := r.Status().Update(ctx, target)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
		}
		return ctrl.Result{}, nil
	}
}

// reconcileNodeRef fetches the referenced Kubernetes Node, resolves the agent
// IP according to the addressType and optional CIDR filter, then updates the
// NodeExists condition, resolvedAddress status fields, labels the node as a
// storage node, and performs a live gRPC HealthCheck to set AgentConnected /
// Ready conditions via r.Dialer.
//
//nolint:funlen // Multiple distinct sub-paths (not found / addr error / success) require unavoidable length.
func (r *PillarTargetReconciler) reconcileNodeRef(
	ctx context.Context,
	target *pillarcsiv1alpha1.PillarTarget,
) (ctrl.Result, error) {
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
			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "AgentConnected",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "NodeNotFound",
				Message: fmt.Sprintf(
					"Cannot connect to agent: Node %q was not found in the cluster", nodeRef.Name,
				),
			})
			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "NodeNotFound",
				Message: fmt.Sprintf(
					"PillarTarget is not ready: Node %q was not found in the cluster", nodeRef.Name,
				),
			})
			target.Status.ResolvedAddress = ""

			statusErr := r.Status().Update(ctx, target)
			if statusErr != nil {
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
		statusErr := r.Status().Update(ctx, target)
		if statusErr != nil {
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
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "AddressNotResolved",
			Message: fmt.Sprintf(
				"Cannot connect to agent: no resolvable address on Node %q", nodeRef.Name,
			),
		})
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "AddressNotResolved",
			Message: fmt.Sprintf(
				"PillarTarget is not ready: no resolvable address on Node %q", nodeRef.Name,
			),
		})
		target.Status.ResolvedAddress = ""

		statusErr := r.Status().Update(ctx, target)
		if statusErr != nil {
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

	// Label the node as a storage node (idempotent — only patch if label is absent).
	if node.Labels == nil || node.Labels[storageNodeLabel] != "true" {
		patch := client.MergeFrom(node.DeepCopy())
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		node.Labels[storageNodeLabel] = "true"
		patchErr := r.Patch(ctx, node, patch)
		if patchErr != nil {
			return ctrl.Result{}, fmt.Errorf(
				"failed to label node %q as storage node: %w", nodeRef.Name, patchErr,
			)
		}
		log.Info("Labeled node as storage node", "node", nodeRef.Name, "label", storageNodeLabel)
	}

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

	// AgentConnected: perform a live gRPC HealthCheck against the agent.
	connected := r.setAgentConnectedCondition(ctx, target, resolved)

	// Ready: True only when the agent is reachable and healthy.
	if connected {
		// Best-effort: populate AgentVersion / Capabilities / DiscoveredPools.
		r.populateCapabilitiesStatus(ctx, target, resolved)

		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: target.Generation,
			Reason:             "AgentConnected",
			Message: fmt.Sprintf(
				"PillarTarget is ready: agent at %q (node %q) is connected and healthy",
				resolved, nodeRef.Name,
			),
		})
	} else {
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "AgentNotConnected",
			Message:            "PillarTarget is not ready: agent gRPC connection has not been established",
		})
	}

	target.Status.ResolvedAddress = resolved

	err = r.Status().Update(ctx, target)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarTarget status: %w", err)
	}

	// Requeue periodically to re-verify agent connectivity.
	return ctrl.Result{RequeueAfter: requeueAfterAgentHealthCheck}, nil
}

// setAgentConnectedCondition performs a live gRPC HealthCheck against the
// agent at address and updates the AgentConnected status condition on target.
//
// The reason reflects both connectivity and authentication level:
//   - "Authenticated"     – mTLS handshake succeeded and agent reports healthy.
//   - "Dialed"            – TCP reachable (no mTLS) and agent reports healthy.
//   - "TLSHandshakeFailed"– mTLS is configured but the TLS handshake failed.
//   - "HealthCheckFailed" – transport error (TCP or other) prevented the RPC.
//   - "AgentUnhealthy"    – agent is reachable but reports degraded status.
//   - "DialerNotConfigured"– no Dialer is wired up (dev/test only).
//
// It returns true when the agent is reachable and reports healthy status, and
// false otherwise (including when r.Dialer is nil).
//
// The caller is responsible for subsequently setting the Ready condition and
// persisting the status update.
func (r *PillarTargetReconciler) setAgentConnectedCondition(
	ctx context.Context,
	target *pillarcsiv1alpha1.PillarTarget,
	address string,
) bool {
	log := logf.FromContext(ctx)

	if r.Dialer == nil {
		// No dialer configured — set condition False with an informative reason
		// instead of the old opaque stub message.
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "DialerNotConfigured",
			Message:            "No gRPC dialer is configured for this reconciler; agent connectivity cannot be verified",
		})
		return false
	}

	// Use a short-lived context for the health-check RPC so a slow or
	// unreachable agent does not block the reconcile loop indefinitely.
	hcCtx, hcCancel := context.WithTimeout(ctx, agentHealthCheckTimeout)
	defer hcCancel()

	resp, err := r.Dialer.HealthCheck(hcCtx, address)
	if err != nil {
		log.Info("Agent health check failed", "address", address, "error", err)
		// Distinguish TLS handshake failures from plain transport errors so
		// operators know whether to investigate certificate configuration.
		if r.Dialer.IsMTLS() && isTLSHandshakeError(err) {
			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "AgentConnected",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "TLSHandshakeFailed",
				Message: fmt.Sprintf(
					"mTLS handshake to agent at %q failed; verify certificate chain and CA: %v",
					address, err,
				),
			})
		} else {
			meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
				Type:               "AgentConnected",
				Status:             metav1.ConditionFalse,
				ObservedGeneration: target.Generation,
				Reason:             "HealthCheckFailed",
				Message:            fmt.Sprintf("Agent health check at %q failed: %v", address, err),
			})
		}
		return false
	}

	if !resp.Healthy {
		log.Info("Agent is reachable but reports unhealthy status", "address", address)
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: target.Generation,
			Reason:             "AgentUnhealthy",
			Message:            fmt.Sprintf("Agent at %q is reachable but reports unhealthy status", address),
		})
		return false
	}

	// Health check succeeded.  Reflect the authentication level in the reason
	// so operators can distinguish a plain TCP dial from a verified mTLS session.
	if r.Dialer.IsMTLS() {
		log.Info("Agent authenticated via mTLS and reports healthy", "address", address)
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: target.Generation,
			Reason:             "Authenticated",
			Message: fmt.Sprintf(
				"Agent at %q is authenticated via mTLS and reports healthy status", address,
			),
		})
	} else {
		log.Info("Agent reachable via plaintext TCP and reports healthy", "address", address)
		meta.SetStatusCondition(&target.Status.Conditions, metav1.Condition{
			Type:               "AgentConnected",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: target.Generation,
			Reason:             "Dialed",
			Message: fmt.Sprintf(
				"Agent at %q is reachable via plaintext TCP and reports healthy status", address,
			),
		})
	}
	return true
}

// populateCapabilitiesStatus calls GetCapabilities on the agent at address
// and writes the result to target.Status.AgentVersion, .Capabilities, and
// .DiscoveredPools.
//
// This is a best-effort operation: if GetCapabilities fails (e.g. the agent
// does not yet implement the RPC or a transient error occurs) the error is
// logged and the status fields are left unchanged.  The caller should not
// treat this failure as a reconcile error.
func (r *PillarTargetReconciler) populateCapabilitiesStatus(
	ctx context.Context,
	target *pillarcsiv1alpha1.PillarTarget,
	address string,
) {
	log := logf.FromContext(ctx)

	if r.Dialer == nil {
		return
	}

	capCtx, capCancel := context.WithTimeout(ctx, agentCapabilitiesTimeout)
	defer capCancel()

	resp, err := r.Dialer.GetCapabilities(capCtx, address)
	if err != nil {
		log.Info("GetCapabilities RPC failed; capabilities status not updated",
			"address", address, "error", err)
		return
	}

	target.Status.AgentVersion = resp.GetAgentVersion()
	target.Status.Capabilities = buildAgentCapabilities(resp)
	target.Status.DiscoveredPools = buildDiscoveredPools(resp.GetDiscoveredPools())
}

// buildAgentCapabilities converts a GetCapabilitiesResponse into the
// pillarcsiv1alpha1.AgentCapabilities status struct.
func buildAgentCapabilities(resp *agentv1.GetCapabilitiesResponse) *pillarcsiv1alpha1.AgentCapabilities {
	backends := make([]string, 0, len(resp.GetSupportedBackends()))
	for _, bt := range resp.GetSupportedBackends() {
		backends = append(backends, backendTypeToString(bt))
	}
	protocols := make([]string, 0, len(resp.GetSupportedProtocols()))
	for _, pt := range resp.GetSupportedProtocols() {
		protocols = append(protocols, protocolTypeToString(pt))
	}
	return &pillarcsiv1alpha1.AgentCapabilities{
		Backends:  backends,
		Protocols: protocols,
	}
}

// buildDiscoveredPools converts a slice of PoolInfo protos to the
// pillarcsiv1alpha1.DiscoveredPool status slice.
func buildDiscoveredPools(pools []*agentv1.PoolInfo) []pillarcsiv1alpha1.DiscoveredPool {
	result := make([]pillarcsiv1alpha1.DiscoveredPool, 0, len(pools))
	for _, p := range pools {
		dp := pillarcsiv1alpha1.DiscoveredPool{
			Name: p.GetName(),
			Type: backendTypeToString(p.GetBackendType()),
		}
		if total := p.GetTotalBytes(); total > 0 {
			q := resource.NewQuantity(total, resource.BinarySI)
			dp.Total = q
		}
		if avail := p.GetAvailableBytes(); avail > 0 {
			q := resource.NewQuantity(avail, resource.BinarySI)
			dp.Available = q
		}
		result = append(result, dp)
	}
	return result
}

// backendTypeToString converts an agentv1.BackendType proto enum to the
// lowercase kebab-case string used in pillar-csi API types (e.g. "zfs-zvol").
func backendTypeToString(bt agentv1.BackendType) string {
	switch bt {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:
		return "zfs-zvol"
	case agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET:
		return "zfs-dataset"
	case agentv1.BackendType_BACKEND_TYPE_LVM:
		return "lvm-lv"
	case agentv1.BackendType_BACKEND_TYPE_BLOCK_DEVICE:
		return "block-device"
	case agentv1.BackendType_BACKEND_TYPE_DIRECTORY:
		return "dir"
	default:
		return strings.ToLower(bt.String())
	}
}

// protocolTypeToString converts an agentv1.ProtocolType proto enum to the
// lowercase kebab-case string used in pillar-csi API types (e.g. "nvmeof-tcp").
func protocolTypeToString(pt agentv1.ProtocolType) string {
	switch pt {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		return "nvmeof-tcp"
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		return "iscsi"
	case agentv1.ProtocolType_PROTOCOL_TYPE_NFS:
		return "nfs"
	case agentv1.ProtocolType_PROTOCOL_TYPE_SMB:
		return "smb"
	default:
		return strings.ToLower(pt.String())
	}
}

// isTLSHandshakeError returns true when err appears to originate from a TLS
// handshake failure (wrong CA, expired certificate, missing client cert, etc.).
// The detection is best-effort and relies on the gRPC transport wrapping the
// underlying crypto/tls error message.
func isTLSHandshakeError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "tls:") ||
		strings.Contains(errStr, "authentication handshake failed") ||
		strings.Contains(errStr, "certificate verify") ||
		strings.Contains(errStr, "x509:")
}

// reconcileDelete handles the deletion flow.
//
// It first checks whether any PillarPool resources still reference this
// PillarTarget via spec.targetRef.  If any do, deletion is blocked and the
// reconciler requeues until they are removed.  Only once no references remain
// does it clean up the storage-node label on the referenced Node and release
// the finalizer.
//
//nolint:gocognit // Deletion flow branches across pool-reference check, node-label cleanup, and multiple error paths.
func (r *PillarTargetReconciler) reconcileDelete(
	ctx context.Context,
	target *pillarcsiv1alpha1.PillarTarget,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(target, pillarTargetFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarTarget is being deleted — checking for referencing PillarPools", "name", target.Name)

	// List all PillarPools (cluster-scoped) and find those that reference this target.
	poolList := &pillarcsiv1alpha1.PillarPoolList{}
	err := r.List(ctx, poolList)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PillarPools: %w", err)
	}

	var referencingPools []string
	for i := range poolList.Items {
		if poolList.Items[i].Spec.TargetRef == target.Name {
			referencingPools = append(referencingPools, poolList.Items[i].Name)
		}
	}

	if len(referencingPools) > 0 {
		// Deletion is blocked — log the reason and requeue.
		msg := fmt.Sprintf(
			"Deletion blocked: PillarPool(s) [%s] still reference this target; delete them first",
			strings.Join(referencingPools, ", "),
		)
		log.Info(msg, "name", target.Name)

		// Requeue after a short delay so we re-check once the operator has had
		// a chance to remove the blocking PillarPools.
		return ctrl.Result{RequeueAfter: requeueAfterTargetDeletionBlock}, nil
	}

	// No remaining PillarPool references — proceed with cleanup.

	// Remove the storage-node label from the referenced Kubernetes Node.
	if target.Spec.NodeRef != nil {
		node := &corev1.Node{}
		nodeErr := r.Get(ctx, types.NamespacedName{Name: target.Spec.NodeRef.Name}, node)
		switch {
		case nodeErr == nil:
			if node.Labels != nil {
				if _, hasLabel := node.Labels[storageNodeLabel]; hasLabel {
					patch := client.MergeFrom(node.DeepCopy())
					delete(node.Labels, storageNodeLabel)
					patchErr := r.Patch(ctx, node, patch)
					if patchErr != nil {
						return ctrl.Result{}, fmt.Errorf(
							"failed to remove storage-node label from Node %q: %w",
							target.Spec.NodeRef.Name, patchErr,
						)
					}
					log.Info("Removed storage-node label from node during deletion",
						"node", target.Spec.NodeRef.Name, "label", storageNodeLabel)
				}
			}
		case client.IgnoreNotFound(nodeErr) == nil:
			// Node already gone — nothing to clean up.
			log.Info("Referenced node not found during deletion cleanup; skipping label removal",
				"name", target.Name, "node", target.Spec.NodeRef.Name)
		default:
			// Transient error — requeue so we retry label removal.
			return ctrl.Result{}, fmt.Errorf(
				"failed to get node %q for label cleanup: %w",
				target.Spec.NodeRef.Name, nodeErr,
			)
		}
	}

	log.Info("PillarTarget deletion unblocked; removing finalizer", "name", target.Name)

	controllerutil.RemoveFinalizer(target, pillarTargetFinalizer)
	err = r.Update(ctx, target)
	if err != nil {
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
//
// In addition to the primary PillarTarget watch it registers two secondary
// watches:
//   - Node objects: re-enqueues any PillarTarget whose spec.nodeRef.name
//     matches the changed node so that NodeExists / resolvedAddress stay
//     current when nodes appear, change, or disappear.
//   - PillarPool objects: re-enqueues the PillarTarget named in the pool's
//     spec.targetRef so that a deletion-blocked target is promptly unblocked
//     when the last referencing pool is removed.
func (r *PillarTargetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapNodeToTargets returns reconcile Requests for every PillarTarget whose
	// spec.nodeRef.name matches the node that just changed.
	mapNodeToTargets := func(ctx context.Context, obj client.Object) []reconcile.Request {
		node, ok := obj.(*corev1.Node)
		if !ok {
			return nil
		}

		targetList := &pillarcsiv1alpha1.PillarTargetList{}
		err := mgr.GetClient().List(ctx, targetList)
		if err != nil {
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

	// mapPoolToTarget re-enqueues the PillarTarget referenced by a changed
	// PillarPool.  This ensures that when the last blocking pool is deleted the
	// target's finalizer is removed promptly (instead of waiting for the
	// RequeueAfter timer).
	mapPoolToTarget := func(_ context.Context, obj client.Object) []reconcile.Request {
		pool, ok := obj.(*pillarcsiv1alpha1.PillarPool)
		if !ok {
			return nil
		}
		if pool.Spec.TargetRef == "" {
			return nil
		}
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Name: pool.Spec.TargetRef}},
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarTarget{}).
		// Re-enqueue PillarTargets whenever the referenced Node changes.
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(mapNodeToTargets),
		).
		// Re-enqueue a PillarTarget when any of its referencing PillarPools
		// change (e.g. deletion) so deletion-blocking is lifted quickly.
		Watches(
			&pillarcsiv1alpha1.PillarPool{},
			handler.EnqueueRequestsFromMapFunc(mapPoolToTarget),
		).
		Named("pillartarget").
		Complete(r)
}
