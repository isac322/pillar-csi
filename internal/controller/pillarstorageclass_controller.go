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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
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
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

const (
	// Finalizer added to every PillarStorageClass to prevent deletion
	// while PVCs still reference the generated StorageClass.
	pillarStorageClassFinalizer = "pillar-csi.bhyoo.com/storage-class-protection"

	// Requeue interval before re-checking whether blocking PVCs have been removed.
	requeueAfterBindingDeletionBlock = 10 * time.Second

	// Requeue interval before re-checking a binding whose pool or protocol
	// is not yet found or not yet Ready. Watches on PillarStore/PillarProtocol
	// will also re-enqueue the binding when those objects change, but the
	// periodic requeue acts as a safety net for cases where the watch event is missed.
	requeueAfterBindingNotReady = 15 * time.Second

	// CSI driver name used as the StorageClass provisioner.
	pillarCSIProvisioner = "pillar-csi.bhyoo.com"

	// Condition type constants for PillarStorageClass status.conditions.

	// PoolReady: set to True when the referenced PillarStore is in Ready state.
	conditionPoolReady = "StoreReady"

	// ProtocolValid: set to True when the referenced PillarProtocol exists
	// and its Ready condition is True.
	conditionProtocolValid = "ProtocolValid"

	// Compatible: set to True when the pool backend and protocol are
	// compatible (e.g. block-only backends are incompatible with NFS).
	conditionCompatible = "Compatible"

	// StorageClassCreated: set to True when the Kubernetes StorageClass
	// has been successfully created and is owned by this PillarStorageClass.
	conditionStorageClassCreated = "StorageClassCreated"

	// Ready: set to True when all other conditions pass and the binding
	// is fully operational (StorageClass available for PVC provisioning).
	conditionReady = "Ready"
)

// PillarStorageClassReconciler reconciles a PillarStorageClass object.
type PillarStorageClassReconciler struct {
	client.Client
	// APIReader reads from the API server, bypassing the informer cache.  It
	// is used where a stale cached PillarStorageClass would lose state; when
	// nil, Client is used.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  recorder.EventRecorder
}

type desiredStorageClass struct {
	params               map[string]string
	reclaimPolicy        corev1.PersistentVolumeReclaimPolicy
	volumeBindingMode    storagev1.VolumeBindingMode
	allowVolumeExpansion *bool
}

// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarStorageClass the reconciler:
//  1. Adds a finalizer on first creation (deletion protection).
//  2. On normal operation: validates that the referenced PillarStore and
//     PillarProtocol exist and are ready, checks backend/protocol compatibility,
//     creates/updates the owned Kubernetes StorageClass, and updates status
//     conditions (PoolReady, ProtocolValid, Compatible, StorageClassCreated, Ready).
//  3. On deletion: blocks until no PVCs reference the generated StorageClass,
//     then deletes the StorageClass and removes the finalizer to allow the
//     object to be garbage-collected.
//
//nolint:dupl // All four CRD controllers share identical Reconcile boilerplate; extraction requires reflection.
func (r *PillarStorageClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarStorageClass instance.
	binding := &pillarcsiv1alpha1.PillarStorageClass{}
	err := r.Get(ctx, req.NamespacedName, binding)
	if err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarStorageClass", "name", binding.Name, "deletionTimestamp", binding.DeletionTimestamp)

	// Branch: object is being deleted.
	if !binding.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, binding)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(binding, pillarStorageClassFinalizer) {
		log.Info("Adding finalizer to PillarStorageClass", "finalizer", pillarStorageClassFinalizer)
		controllerutil.AddFinalizer(binding, pillarStorageClassFinalizer)
		err := r.Update(ctx, binding)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		// Return after the update; controller-runtime will re-enqueue.
		return ctrl.Result{}, nil
	}

	// Normal reconcile path.
	return r.reconcileNormal(ctx, binding)
}

// storageClassNameFor returns the StorageClass name for the given binding.
// It uses spec.storageClass.name when set, falling back to the binding's own name.
func storageClassNameFor(binding *pillarcsiv1alpha1.PillarStorageClass) string {
	if binding.Spec.StorageClass.Name != "" {
		return binding.Spec.StorageClass.Name
	}
	return binding.Name
}

// reconcileNormal handles the steady-state reconciliation of a PillarStorageClass
// that is not being deleted.
//
// It:
//  1. Looks up and validates the referenced PillarStore (PoolReady condition).
//  2. Looks up and validates the referenced PillarProtocol (ProtocolValid condition).
//  3. Checks that the pool backend and protocol are compatible (Compatible condition).
//  4. Creates or updates the owned StorageClass (StorageClassCreated condition).
//  5. Sets the top-level Ready condition and updates status.storageClassName.
//
//nolint:funlen,gocognit,gocyclo // Validation, compatibility, and StorageClass paths drive the complexity.
func (r *PillarStorageClassReconciler) reconcileNormal(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// --- 1. Validate PillarStore (PoolReady condition) ---
	pool := &pillarcsiv1alpha1.PillarStore{}
	poolErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.StoreRef}, pool)

	//nolint:dupl // Pool and Protocol switch blocks are structurally identical; extraction would add indirection.
	switch {
	case poolErr != nil && client.IgnoreNotFound(poolErr) == nil:
		log.Info("Referenced PillarStore not found", "binding", binding.Name, "pool", binding.Spec.StoreRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolNotFound",
			Message:            fmt.Sprintf("PillarStore %q was not found in the cluster", binding.Spec.StoreRef),
		})
		setBindingNotReady(binding, "PoolNotFound", fmt.Sprintf("PillarStore %q was not found", binding.Spec.StoreRef))
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", statusErr)
		}
		// Requeue: no watch will trigger until the pool is created.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil

	case poolErr != nil:
		log.Error(poolErr, "Failed to get referenced PillarStore", "binding", binding.Name, "pool", binding.Spec.StoreRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolLookupError",
			Message:            fmt.Sprintf("Failed to look up PillarStore %q: %v", binding.Spec.StoreRef, poolErr),
		})
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			log.Error(statusErr, "Failed to update PillarStorageClass status after pool lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PillarStore %q: %w", binding.Spec.StoreRef, poolErr)
	}

	// Pool exists — check its Ready condition.
	poolReadyCond := meta.FindStatusCondition(pool.Status.Conditions, conditionReady)
	poolReady := poolReadyCond != nil && poolReadyCond.Status == metav1.ConditionTrue
	if poolReady {
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: binding.Generation,
			Reason:             "StoreReady",
			Message:            fmt.Sprintf("PillarStore %q is in Ready state", binding.Spec.StoreRef),
		})
	} else {
		msg := fmt.Sprintf("PillarStore %q exists but is not yet Ready", binding.Spec.StoreRef)
		if poolReadyCond != nil {
			msg = fmt.Sprintf("PillarStore %q is not Ready: %s", binding.Spec.StoreRef, poolReadyCond.Message)
		}
		log.Info("Referenced PillarStore is not Ready", "binding", binding.Name, "pool", binding.Spec.StoreRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolNotReady",
			Message:            msg,
		})
		setBindingNotReady(binding, "PoolNotReady", msg)
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", statusErr)
		}
		// Requeue: pool watch will also re-enqueue when the pool becomes ready,
		// but the periodic requeue acts as a safety net.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil
	}

	// --- 2. Validate PillarProtocol (ProtocolValid condition) ---
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protocolErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.ProtocolRef}, protocol)

	//nolint:dupl // Pool and Protocol switch blocks are structurally identical; extraction would add indirection.
	switch {
	case protocolErr != nil && client.IgnoreNotFound(protocolErr) == nil:
		log.Info("Referenced PillarProtocol not found",
			"binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolNotFound",
			Message: fmt.Sprintf(
				"PillarProtocol %q was not found in the cluster", binding.Spec.ProtocolRef,
			),
		})
		setBindingNotReady(
			binding, "ProtocolNotFound",
			fmt.Sprintf("PillarProtocol %q was not found", binding.Spec.ProtocolRef),
		)
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", statusErr)
		}
		// Requeue: no watch will trigger until the protocol is created.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil

	case protocolErr != nil:
		log.Error(protocolErr, "Failed to get referenced PillarProtocol",
			"binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolLookupError",
			Message: fmt.Sprintf(
				"Failed to look up PillarProtocol %q: %v", binding.Spec.ProtocolRef, protocolErr,
			),
		})
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			log.Error(statusErr, "Failed to update PillarStorageClass status after protocol lookup error")
		}
		return ctrl.Result{}, fmt.Errorf(
			"failed to get PillarProtocol %q: %w", binding.Spec.ProtocolRef, protocolErr,
		)
	}

	// Protocol exists — check its Ready condition.
	protocolReadyCond := meta.FindStatusCondition(protocol.Status.Conditions, conditionReady)
	protocolReady := protocolReadyCond != nil && protocolReadyCond.Status == metav1.ConditionTrue
	if protocolReady {
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolValid",
			Message: fmt.Sprintf(
				"PillarProtocol %q is valid (type: %s)", binding.Spec.ProtocolRef, protocol.Spec.Type,
			),
		})
	} else {
		msg := fmt.Sprintf("PillarProtocol %q exists but is not yet Ready", binding.Spec.ProtocolRef)
		if protocolReadyCond != nil {
			msg = fmt.Sprintf(
				"PillarProtocol %q is not Ready: %s", binding.Spec.ProtocolRef, protocolReadyCond.Message,
			)
		}
		log.Info("Referenced PillarProtocol is not Ready",
			"binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolNotReady",
			Message:            msg,
		})
		setBindingNotReady(binding, "ProtocolNotReady", msg)
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", statusErr)
		}
		// Requeue: protocol watch will also re-enqueue when the protocol becomes
		// ready, but the periodic requeue acts as a safety net.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil
	}

	// --- 3. Check backend/protocol compatibility (Compatible condition) ---
	compatMsg, compatible := evaluateCompatibility(pool, protocol)
	if compatible {
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionCompatible,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: binding.Generation,
			Reason:             "Compatible",
			Message: fmt.Sprintf(
				"Backend type %q and protocol type %q are compatible",
				pool.Spec.Backend.Type, protocol.Spec.Type,
			),
		})
	} else {
		log.Info("Backend and protocol are incompatible", "binding", binding.Name,
			"backend", pool.Spec.Backend.Type, "protocol", protocol.Spec.Type)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionCompatible,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "Incompatible",
			Message:            compatMsg,
		})
		setBindingNotReady(binding, "Incompatible", compatMsg)
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// --- 4. Create / update owned StorageClass (StorageClassCreated condition) ---
	scName := storageClassNameFor(binding)
	scErr := r.reconcileStorageClass(ctx, binding, pool, protocol, scName)
	if scErr != nil {
		log.Error(scErr, "Failed to reconcile StorageClass", "binding", binding.Name, "storageClass", scName)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionStorageClassCreated,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "StorageClassError",
			Message:            fmt.Sprintf("Failed to reconcile StorageClass %q: %v", scName, scErr),
		})
		setBindingNotReady(
			binding, "StorageClassError",
			fmt.Sprintf("StorageClass %q could not be created/updated", scName),
		)
		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			log.Error(statusErr, "Failed to update PillarStorageClass status after StorageClass error")
		}
		return ctrl.Result{}, scErr
	}

	meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
		Type:               conditionStorageClassCreated,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: binding.Generation,
		Reason:             "StorageClassCreated",
		Message:            fmt.Sprintf("StorageClass %q is present and owned by this binding", scName),
	})
	binding.Status.StorageClassName = scName

	// --- 5. Set top-level Ready condition ---
	meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: binding.Generation,
		Reason:             "AllConditionsMet",
		Message: fmt.Sprintf(
			"PillarStorageClass is ready: StorageClass %q is available for provisioning (pool: %s, protocol: %s)",
			scName, binding.Spec.StoreRef, binding.Spec.ProtocolRef,
		),
	})

	err := r.Status().Update(ctx, binding)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarStorageClass status: %w", err)
	}

	log.Info("PillarStorageClass reconciled successfully",
		"name", binding.Name,
		"storageClass", scName,
		"pool", binding.Spec.StoreRef,
		"protocol", binding.Spec.ProtocolRef,
	)
	return ctrl.Result{}, nil
}

// evaluateCompatibility delegates backend/protocol compatibility evaluation to
// the shared API helper so reconciliation and admission use the same matrix.
func evaluateCompatibility(
	pool *pillarcsiv1alpha1.PillarStore,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) (string, bool) {
	compat := pillarcsiv1alpha1.Compatible(pool.Spec.Backend.Type, protocol.Spec.Type)
	if compat.OK {
		return "", true
	}
	return compat.Message, false
}

// buildStorageClassParams constructs the StorageClass parameter map from the
// binding's pool, protocol, and optional overrides.
//
// The parameters encode all configuration that the CSI node/controller plugin
// needs to provision and attach volumes.  They follow the key convention:
//
//	pillar-csi.bhyoo.com/<parameter-name>
//
//nolint:gocognit,gocyclo // ZFS and protocol-specific param branches drive the complexity.
func buildStorageClassParams(
	binding *pillarcsiv1alpha1.PillarStorageClass,
	pool *pillarcsiv1alpha1.PillarStore,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) map[string]string {
	params := map[string]string{
		"pillar-csi.bhyoo.com/store":         binding.Spec.StoreRef,
		"pillar-csi.bhyoo.com/protocol":      binding.Spec.ProtocolRef,
		"pillar-csi.bhyoo.com/backend-type":  string(pool.Spec.Backend.Type),
		"pillar-csi.bhyoo.com/protocol-type": string(protocol.Spec.Type),
		"pillar-csi.bhyoo.com/agent":         pool.Spec.AgentRef,
	}

	// ZFS backend parameters: overwrite the generic pool reference with the
	// actual ZFS pool name so the CSI controller can construct agent volume
	// IDs and backend params without a separate zfs-pool parameter.
	if pool.Spec.Backend.ZFS != nil {
		if pool.Spec.Backend.ZFS.Pool != "" {
			params["pillar-csi.bhyoo.com/store"] = pool.Spec.Backend.ZFS.Pool
		}
		if pool.Spec.Backend.ZFS.ParentDataset != "" {
			params["pillar-csi.bhyoo.com/zfs-parent-dataset"] = pool.Spec.Backend.ZFS.ParentDataset
		}
	}

	// LVM backend parameters: overwrite the generic pool reference with the
	// actual LVM Volume Group name so the CSI controller can construct agent
	// volume IDs correctly (mirroring the ZFS pattern above).
	if pool.Spec.Backend.LVM != nil {
		if pool.Spec.Backend.LVM.VolumeGroup != "" {
			params["pillar-csi.bhyoo.com/store"] = pool.Spec.Backend.LVM.VolumeGroup
			params["pillar-csi.bhyoo.com/lvm-vg"] = pool.Spec.Backend.LVM.VolumeGroup
		}
		// Always emitted, even empty: the key declares the store's thin pool
		// ("" = none) so that the agent refuses CreateVolume when its
		// --backend thinpool disagrees, instead of silently using its own.
		params["pillar-csi.bhyoo.com/lvm-thin-pool"] = pool.Spec.Backend.LVM.ThinPool
	}

	// Protocol-specific parameters.
	switch protocol.Spec.Type {
	case pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP:
		if protocol.Spec.NVMeOFTCP != nil {
			params["pillar-csi.bhyoo.com/nvmeof-port"] = fmt.Sprintf("%d", protocol.Spec.NVMeOFTCP.Port)
			// Propagate ACL toggle so the CSI controller passes the correct
			// AclEnabled flag to agent.ExportVolume.  The default (true) is
			// emitted explicitly so that a future protocol update that flips
			// the field from false back to true is reflected in the StorageClass.
			if protocol.Spec.NVMeOFTCP.ACL {
				params["pillar-csi.bhyoo.com/acl-enabled"] = labelValueTrue
			} else {
				params["pillar-csi.bhyoo.com/acl-enabled"] = "false"
			}
			// Initiator reconnect tuning, applied by the node at fabrics
			// connect.  Emitted only when set (0 included) so an unset field
			// keeps the kernel defaults.
			if v := protocol.Spec.NVMeOFTCP.CtrlLossTmo; v != nil {
				params["pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo"] = fmt.Sprintf("%d", *v)
			}
			if v := protocol.Spec.NVMeOFTCP.ReconnectDelay; v != nil {
				params["pillar-csi.bhyoo.com/nvmeof-reconnect-delay"] = fmt.Sprintf("%d", *v)
			}
			addNVMeoFSizingParams(params, protocol.Spec.NVMeOFTCP, nvmeofTCPOverrides(binding))
		}
	case pillarcsiv1alpha1.ProtocolTypeISCSI:
		if protocol.Spec.ISCSI != nil {
			params["pillar-csi.bhyoo.com/iscsi-port"] = fmt.Sprintf("%d", protocol.Spec.ISCSI.Port)
			// Same ACL toggle for iSCSI (initiator IQN-based access control).
			if protocol.Spec.ISCSI.ACL {
				params["pillar-csi.bhyoo.com/acl-enabled"] = labelValueTrue
			} else {
				params["pillar-csi.bhyoo.com/acl-enabled"] = "false"
			}
		}
	case pillarcsiv1alpha1.ProtocolTypeNFS:
		if protocol.Spec.NFS != nil && protocol.Spec.NFS.Version != "" {
			params["pillar-csi.bhyoo.com/nfs-version"] = protocol.Spec.NFS.Version
		}
	}

	// fsType (block protocols only): binding override takes precedence,
	// then protocol-level default.
	if protocol.Spec.Type != pillarcsiv1alpha1.ProtocolTypeNFS {
		fsType := protocol.Spec.FSType
		if binding.Spec.Overrides != nil && binding.Spec.Overrides.FSType != "" {
			fsType = binding.Spec.Overrides.FSType
		}
		if fsType != "" {
			params["csi.storage.k8s.io/fstype"] = fsType
		}

		// mkfsOptions: binding override takes precedence, then protocol-level.
		// Encoded as a JSON string array — the same encoding the PVC
		// fs-override annotation produces — so every argv element survives
		// intact (e.g. a label containing spaces) on its way through the PV
		// VolumeContext to NodeStageVolume.
		mkfsOptions := protocol.Spec.MkfsOptions
		if binding.Spec.Overrides != nil && len(binding.Spec.Overrides.MkfsOptions) > 0 {
			mkfsOptions = binding.Spec.Overrides.MkfsOptions
		}
		if len(mkfsOptions) > 0 {
			encoded, err := json.Marshal(mkfsOptions)
			if err == nil { // json.Marshal of a []string cannot fail
				params["pillar-csi.bhyoo.com/mkfs-options"] = string(encoded)
			}
		}
	}

	return params
}

// nvmeofTCPOverrides returns the binding's NVMe-oF/TCP protocol overrides, or
// nil when it has none.
func nvmeofTCPOverrides(binding *pillarcsiv1alpha1.PillarStorageClass) *pillarcsiv1alpha1.NVMeOFTCPOverrides {
	if binding.Spec.Overrides == nil || binding.Spec.Overrides.Protocol == nil {
		return nil
	}
	return binding.Spec.Overrides.Protocol.NVMeOFTCP
}

// addNVMeoFSizingParams emits the queue depth (initiator queue_size at
// connect) and the in-capsule data size (target port
// param_inline_data_size).  A binding override wins over the protocol value;
// a value set on neither is omitted so the kernel default applies.
func addNVMeoFSizingParams(
	params map[string]string,
	protocol *pillarcsiv1alpha1.NVMeOFTCPConfig,
	overrides *pillarcsiv1alpha1.NVMeOFTCPOverrides,
) {
	maxQueueSize := protocol.MaxQueueSize
	inCapsuleDataSize := protocol.InCapsuleDataSize
	if overrides != nil {
		if overrides.MaxQueueSize != nil {
			maxQueueSize = overrides.MaxQueueSize
		}
		if overrides.InCapsuleDataSize != nil {
			inCapsuleDataSize = overrides.InCapsuleDataSize
		}
	}
	if maxQueueSize != nil {
		params["pillar-csi.bhyoo.com/nvmeof-max-queue-size"] = fmt.Sprintf("%d", *maxQueueSize)
	}
	if inCapsuleDataSize != nil {
		params["pillar-csi.bhyoo.com/nvmeof-in-capsule-data-size"] = fmt.Sprintf("%d", *inCapsuleDataSize)
	}
}

// reconcileStorageClass creates or updates the StorageClass owned by this binding.
//
// A StorageClass is immutable apart from its metadata and allowVolumeExpansion:
// the API server rejects any update of provisioner, parameters, reclaimPolicy,
// volumeBindingMode, mountOptions or allowedTopologies.  Pool, protocol and
// binding edits change the parameters, so an in-place update cannot converge.
// When an immutable field differs from the desired state, the StorageClass is
// deleted and re-created with the desired spec (recreateStorageClass); only
// the mutable remainder goes through a regular update.  Existing
// PersistentVolumes are unaffected either way: a PV snapshots its parameters
// into spec.csi.volumeAttributes at provisioning time and refers to its class
// only by name.
//
// The owner reference makes deleting the PillarStorageClass cascade to the
// StorageClass once no PVCs are blocking deletion.
func (r *PillarStorageClassReconciler) reconcileStorageClass(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
	pool *pillarcsiv1alpha1.PillarStore,
	protocol *pillarcsiv1alpha1.PillarProtocol,
	scName string,
) error {
	log := logf.FromContext(ctx)
	desired := desiredStorageClassFor(binding, pool, protocol)

	existing := &storagev1.StorageClass{}
	getErr := r.Get(ctx, types.NamespacedName{Name: scName}, existing)
	switch {
	case getErr == nil && storageClassImmutableDrift(existing, desired):
		return r.recreateStorageClass(ctx, binding, existing, desired)
	case getErr != nil && !errors.IsNotFound(getErr):
		return fmt.Errorf("failed to get StorageClass %q: %w", scName, getErr)
	}

	// An absent StorageClass may be the gap of an interrupted re-create.  The
	// informer caches of StorageClasses and PillarStorageClasses advance
	// independently, so the cached binding may predate the carry-over record
	// written before the delete; read the record from the API server.
	checkpoint := binding
	if getErr != nil {
		var err error
		checkpoint, err = r.currentBinding(ctx, binding)
		if err != nil {
			return err
		}
	}
	carryOver, pending, err := pendingCarryOver(checkpoint)
	if err != nil {
		return err
	}

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sc, func() error {
		// A create completing an interrupted re-create restores the fields
		// recorded before the old StorageClass was deleted.
		if sc.ResourceVersion == "" && pending {
			carryOver.applyTo(sc)
		}

		// Set owner reference so that the StorageClass is garbage-collected
		// when the PillarStorageClass is deleted (after PVC blocking is resolved).
		setErr := controllerutil.SetControllerReference(binding, sc, r.Scheme)
		if setErr != nil {
			return fmt.Errorf("failed to set owner reference on StorageClass: %w", setErr)
		}

		if sc.ResourceVersion != "" && storageClassDrifted(sc, desired) {
			r.recordEvent(binding, "StorageClassReverted",
				"StorageClass %q drifted, reverting to PillarStorageClass spec", scName)
		}

		applyDesiredStorageClass(sc, desired)
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to create or update StorageClass %q: %w", scName, err)
	}
	log.Info("StorageClass reconciled", "name", scName, "operation", op)

	if pending {
		// The StorageClass exists again, so the record has served its purpose.
		return r.recordCarryOver(ctx, checkpoint, nil)
	}
	return nil
}

// currentBinding returns binding as stored in the API server: binding itself
// when it is current, else the fresher copy.
func (r *PillarStorageClassReconciler) currentBinding(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
) (*pillarcsiv1alpha1.PillarStorageClass, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &pillarcsiv1alpha1.PillarStorageClass{}
	err := reader.Get(ctx, types.NamespacedName{Name: binding.Name}, current)
	if err != nil {
		return nil, fmt.Errorf("read PillarStorageClass %q from the API server: %w", binding.Name, err)
	}
	if current.ResourceVersion == binding.ResourceVersion {
		return binding, nil
	}
	return current, nil
}

// storageClassCarryOverAnnotation holds, on the PillarStorageClass, the
// StorageClass fields a re-create carries over.  It is written before the old
// StorageClass is deleted and removed once the replacement exists, so that a
// failed create or a controller restart in between cannot lose them: the next
// reconcile creates the StorageClass from the recorded fields.
const storageClassCarryOverAnnotation = "pillar-csi.bhyoo.com/storage-class-carry-over"

// storageClassCarryOver is the part of a StorageClass that this controller
// does not manage and a re-create keeps.
type storageClassCarryOver struct {
	Labels            map[string]string             `json:"labels,omitempty"`
	Annotations       map[string]string             `json:"annotations,omitempty"`
	MountOptions      []string                      `json:"mountOptions,omitempty"`
	AllowedTopologies []corev1.TopologySelectorTerm `json:"allowedTopologies,omitempty"`
}

func carryOverFrom(sc *storagev1.StorageClass) *storageClassCarryOver {
	return &storageClassCarryOver{
		Labels:            sc.Labels,
		Annotations:       sc.Annotations,
		MountOptions:      sc.MountOptions,
		AllowedTopologies: sc.AllowedTopologies,
	}
}

func (c *storageClassCarryOver) applyTo(sc *storagev1.StorageClass) {
	sc.Labels = c.Labels
	sc.Annotations = c.Annotations
	sc.MountOptions = c.MountOptions
	sc.AllowedTopologies = c.AllowedTopologies
}

// pendingCarryOver returns the carry-over recorded on binding by an
// interrupted re-create, and whether there is one.
func pendingCarryOver(binding *pillarcsiv1alpha1.PillarStorageClass) (*storageClassCarryOver, bool, error) {
	raw, ok := binding.Annotations[storageClassCarryOverAnnotation]
	if !ok {
		return &storageClassCarryOver{}, false, nil
	}
	carryOver := &storageClassCarryOver{}
	err := json.Unmarshal([]byte(raw), carryOver)
	if err != nil {
		return nil, false, fmt.Errorf("decode annotation %s on PillarStorageClass %q: %w",
			storageClassCarryOverAnnotation, binding.Name, err)
	}
	return carryOver, true, nil
}

// recordCarryOver durably records carryOver on binding, or removes the record
// when carryOver is nil.
func (r *PillarStorageClassReconciler) recordCarryOver(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
	carryOver *storageClassCarryOver,
) error {
	if carryOver == nil {
		if _, ok := binding.Annotations[storageClassCarryOverAnnotation]; !ok {
			return nil
		}
		delete(binding.Annotations, storageClassCarryOverAnnotation)
	} else {
		raw, err := json.Marshal(carryOver)
		if err != nil {
			return fmt.Errorf("encode StorageClass carry-over for PillarStorageClass %q: %w", binding.Name, err)
		}
		if binding.Annotations == nil {
			binding.Annotations = map[string]string{}
		}
		binding.Annotations[storageClassCarryOverAnnotation] = string(raw)
	}
	err := r.Update(ctx, binding)
	if err != nil {
		return fmt.Errorf("update annotation %s on PillarStorageClass %q: %w",
			storageClassCarryOverAnnotation, binding.Name, err)
	}
	return nil
}

// recreateStorageClass replaces existing, whose immutable fields differ from
// desired, with a StorageClass built from desired.  Metadata and the
// immutable fields this controller does not manage (mountOptions,
// allowedTopologies) are carried over so the replacement differs only in the
// managed spec.  A StorageClass that another object controls is never
// deleted.
//
// The carry-over is recorded on the binding before the delete, so the
// replacement is complete even when the create fails or the controller
// restarts in between: the next reconcile finds the StorageClass absent and
// creates it from the record.  StorageClasses carry no finalizers, so the
// delete completes synchronously and the name is free for the create.
func (r *PillarStorageClassReconciler) recreateStorageClass(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
	existing *storagev1.StorageClass,
	desired desiredStorageClass,
) error {
	owner := metav1.GetControllerOf(existing)
	if owner != nil && owner.UID != binding.UID {
		return fmt.Errorf(
			"recreate StorageClass %q: controlled by %s %q, not by this PillarStorageClass",
			existing.Name, owner.Kind, owner.Name)
	}
	if !existing.DeletionTimestamp.IsZero() {
		return fmt.Errorf("recreate StorageClass %q: previous StorageClass is still being deleted", existing.Name)
	}

	carryOver := carryOverFrom(existing)
	replacement := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: existing.Name}}
	carryOver.applyTo(replacement)
	applyDesiredStorageClass(replacement, desired)
	err := controllerutil.SetControllerReference(binding, replacement, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set owner reference on StorageClass: %w", err)
	}

	err = r.recordCarryOver(ctx, binding, carryOver)
	if err != nil {
		return err
	}

	// Preconditions pin the delete to the object whose drift was observed,
	// so a concurrent writer is never replaced blindly.
	err = r.Delete(ctx, existing, client.Preconditions{
		UID:             &existing.UID,
		ResourceVersion: &existing.ResourceVersion,
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete StorageClass %q for recreation: %w", existing.Name, err)
	}
	err = r.Create(ctx, replacement)
	if err != nil {
		return fmt.Errorf("create StorageClass %q after deleting it for recreation: %w", existing.Name, err)
	}

	r.recordEvent(binding, "StorageClassRecreated",
		"StorageClass %q re-created because immutable fields changed; existing volumes keep their parameters",
		existing.Name)
	logf.FromContext(ctx).Info("StorageClass re-created with the desired immutable fields", "name", existing.Name)
	return r.recordCarryOver(ctx, binding, nil)
}

// desiredStorageClassFor computes the managed StorageClass fields for binding.
func desiredStorageClassFor(
	binding *pillarcsiv1alpha1.PillarStorageClass,
	pool *pillarcsiv1alpha1.PillarStore,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) desiredStorageClass {
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	if binding.Spec.StorageClass.ReclaimPolicy == pillarcsiv1alpha1.ReclaimPolicyRetain {
		reclaimPolicy = corev1.PersistentVolumeReclaimRetain
	}

	volumeBindingMode := storagev1.VolumeBindingImmediate
	if binding.Spec.StorageClass.VolumeBindingMode == pillarcsiv1alpha1.VolumeBindingWaitForFirstConsumer {
		volumeBindingMode = storagev1.VolumeBindingWaitForFirstConsumer
	}

	allowVolumeExpansion := binding.Spec.StorageClass.AllowVolumeExpansion
	if allowVolumeExpansion == nil {
		defaultAllow := protocol.Spec.Type != pillarcsiv1alpha1.ProtocolTypeNFS
		allowVolumeExpansion = &defaultAllow
	}
	return desiredStorageClass{
		params:               buildStorageClassParams(binding, pool, protocol),
		reclaimPolicy:        reclaimPolicy,
		volumeBindingMode:    volumeBindingMode,
		allowVolumeExpansion: allowVolumeExpansion,
	}
}

// applyDesiredStorageClass writes the managed fields of desired into sc.
func applyDesiredStorageClass(sc *storagev1.StorageClass, desired desiredStorageClass) {
	sc.Provisioner = pillarCSIProvisioner
	sc.Parameters = desired.params
	sc.ReclaimPolicy = &desired.reclaimPolicy
	sc.VolumeBindingMode = &desired.volumeBindingMode
	sc.AllowVolumeExpansion = desired.allowVolumeExpansion
}

// storageClassImmutableDrift reports whether a managed field that the API
// server refuses to update differs from desired.
func storageClassImmutableDrift(sc *storagev1.StorageClass, desired desiredStorageClass) bool {
	return sc.Provisioner != pillarCSIProvisioner ||
		!equality.Semantic.DeepEqual(sc.Parameters, desired.params) ||
		!equality.Semantic.DeepEqual(sc.ReclaimPolicy, &desired.reclaimPolicy) ||
		!equality.Semantic.DeepEqual(sc.VolumeBindingMode, &desired.volumeBindingMode)
}

func storageClassDrifted(sc *storagev1.StorageClass, desired desiredStorageClass) bool {
	return storageClassImmutableDrift(sc, desired) ||
		!equality.Semantic.DeepEqual(sc.AllowVolumeExpansion, desired.allowVolumeExpansion)
}

// recordEvent emits a Normal event on binding when a recorder is configured.
func (r *PillarStorageClassReconciler) recordEvent(
	binding *pillarcsiv1alpha1.PillarStorageClass,
	reason, messageFmt string,
	args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(binding, nil, corev1.EventTypeNormal, reason, "Reconcile", messageFmt, args...)
}

// setBindingNotReady is a helper that sets the top-level Ready condition to
// False with the given reason and message.
func setBindingNotReady(binding *pillarcsiv1alpha1.PillarStorageClass, reason, message string) {
	meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: binding.Generation,
		Reason:             reason,
		Message:            fmt.Sprintf("PillarStorageClass is not ready: %s", message),
	})
}

// reconcileDelete handles the deletion flow for PillarStorageClass.
//
// Deletion is blocked while any PVC, PersistentVolume or PillarVolumeState of
// the generated StorageClass remains: DeleteVolume of those volumes still
// relies on the PillarStore and PillarAgent this binding keeps alive.  Once
// nothing references it, the StorageClass is deleted and the finalizer is
// removed so Kubernetes can garbage-collect the object.
func (r *PillarStorageClassReconciler) reconcileDelete(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(binding, pillarStorageClassFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarStorageClass is being deleted — checking for volumes of the StorageClass",
		"name", binding.Name)

	// Determine the StorageClass name from status (it was set during creation)
	// or fall back to computing it from spec.
	scName := binding.Status.StorageClassName
	if scName == "" {
		scName = storageClassNameFor(binding)
	}

	blockingPVCs, err := r.pvcsOfStorageClass(ctx, scName)
	if err != nil {
		return ctrl.Result{}, err
	}
	volumes, err := r.bindingVolumeReferences(ctx, binding, scName)
	if err != nil {
		return ctrl.Result{}, err
	}

	var blockers deletionBlockers
	blockers.add("PVC(s)", blockingPVCs)
	blockers.addVolumes(volumes)
	if len(blockers) > 0 {
		msg := blockers.message(fmt.Sprintf("StorageClass %q", scName))
		log.Info(msg, "name", binding.Name)

		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "DeletionBlocked",
			Message:            msg,
		})

		statusErr := r.Status().Update(ctx, binding)
		if statusErr != nil {
			log.Error(statusErr, "Failed to update status while deletion is blocked")
		}

		return ctrl.Result{RequeueAfter: requeueAfterBindingDeletionBlock}, nil
	}

	// Nothing references the StorageClass — delete the owned StorageClass if it still exists.
	sc := &storagev1.StorageClass{}
	getErr := r.Get(ctx, types.NamespacedName{Name: scName}, sc)
	if getErr != nil {
		if !errors.IsNotFound(getErr) {
			return ctrl.Result{}, fmt.Errorf("failed to get StorageClass %q: %w", scName, getErr)
		}
		// StorageClass already gone — nothing more to clean up.
	} else {
		// StorageClass exists — delete it explicitly (ownerRef GC may not fire
		// instantly, and we want deterministic cleanup in the deletion path).
		log.Info("Deleting owned StorageClass", "name", scName)
		delErr := r.Delete(ctx, sc)
		if delErr != nil && !errors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("failed to delete StorageClass %q: %w", scName, delErr)
		}
	}

	// Safe to remove the finalizer.
	log.Info("No PVCs or volumes reference StorageClass; removing finalizer",
		"binding", binding.Name, "storageClass", scName)
	controllerutil.RemoveFinalizer(binding, pillarStorageClassFinalizer)
	err = r.Update(ctx, binding)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarStorageClass: %w", err)
	}

	return ctrl.Result{}, nil
}

// pvcsOfStorageClass returns "namespace/name" of every PVC that requests
// StorageClass scName.
func (r *PillarStorageClassReconciler) pvcsOfStorageClass(ctx context.Context, scName string) ([]string, error) {
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := r.List(ctx, pvcList)
	if err != nil {
		return nil, fmt.Errorf("failed to list PersistentVolumeClaims: %w", err)
	}
	var names []string
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == scName {
			names = append(names, pvc.Namespace+"/"+pvc.Name)
		}
	}
	return names, nil
}

// bindingVolumeReferences returns the PersistentVolumes of StorageClass
// scName and the PillarVolumeStates of volumes provisioned through binding.
//
// A PillarVolumeState records no StorageClass.  When its PV (the PV of the
// same name) exists, that PV's StorageClass decides.  Without a PV, a volume
// that lives in the binding's PillarStore and uses its protocol is attributed
// to the binding; if either object is already gone this fallback cannot
// attribute it, and the PillarStore and PillarAgent guards keep holding the
// volume's storage node instead.
func (r *PillarStorageClassReconciler) bindingVolumeReferences(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarStorageClass,
	scName string,
) (volumeReferences, error) {
	store := &pillarcsiv1alpha1.PillarStore{}
	storeErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.StoreRef}, store)
	if storeErr != nil && !errors.IsNotFound(storeErr) {
		return volumeReferences{}, fmt.Errorf("get PillarStore %q: %w", binding.Spec.StoreRef, storeErr)
	}
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protocolErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.ProtocolRef}, protocol)
	if protocolErr != nil && !errors.IsNotFound(protocolErr) {
		return volumeReferences{}, fmt.Errorf("get PillarProtocol %q: %w", binding.Spec.ProtocolRef, protocolErr)
	}
	sourceKnown := storeErr == nil && protocolErr == nil

	return listVolumeReferences(ctx, r.Client,
		func(pv *corev1.PersistentVolume) bool {
			return pv.Spec.StorageClassName == scName
		},
		func(pvs *pillarcsiv1alpha1.PillarVolumeState, pv *corev1.PersistentVolume) bool {
			if pv != nil {
				return pv.Spec.StorageClassName == scName
			}
			return sourceKnown && volumeRefFromVolumeState(pvs).provisionedThrough(store, protocol)
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
//
// The controller watches:
//   - PillarStorageClass (primary resource).
//   - PillarStore: re-enqueues bindings that reference a pool whenever the
//     pool changes — so that the PoolReady condition stays current.
//   - PillarProtocol: re-enqueues bindings that reference a protocol whenever
//     the protocol changes — so that the ProtocolValid condition stays current.
//   - StorageClass: re-enqueues the owner PillarStorageClass when the managed
//     StorageClass is modified or deleted (drift detection / self-healing).
func (r *PillarStorageClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapPoolToBindings returns reconcile Requests for every PillarStorageClass
	// whose spec.storeRef matches the PillarStore that just changed.
	mapPoolToBindings := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pool, ok := obj.(*pillarcsiv1alpha1.PillarStore)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
		err := mgr.GetClient().List(ctx, bindingList)
		if err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range bindingList.Items {
			if bindingList.Items[i].Spec.StoreRef == pool.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: bindingList.Items[i].Name},
				})
			}
		}
		return requests
	}

	// mapProtocolToBindings returns reconcile Requests for every PillarStorageClass
	// whose spec.protocolRef matches the PillarProtocol that just changed.
	mapProtocolToBindings := func(ctx context.Context, obj client.Object) []reconcile.Request {
		protocol, ok := obj.(*pillarcsiv1alpha1.PillarProtocol)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarStorageClassList{}
		err := mgr.GetClient().List(ctx, bindingList)
		if err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range bindingList.Items {
			if bindingList.Items[i].Spec.ProtocolRef == protocol.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: bindingList.Items[i].Name},
				})
			}
		}
		return requests
	}

	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarStorageClass{}).
		// Re-enqueue bindings whenever the referenced PillarStore changes.
		Watches(
			&pillarcsiv1alpha1.PillarStore{},
			handler.EnqueueRequestsFromMapFunc(mapPoolToBindings),
		).
		// Re-enqueue bindings whenever the referenced PillarProtocol changes.
		Watches(
			&pillarcsiv1alpha1.PillarProtocol{},
			handler.EnqueueRequestsFromMapFunc(mapProtocolToBindings),
		).
		// Automatically re-enqueue the owning PillarStorageClass when its managed
		// StorageClass is modified externally (self-healing).
		Owns(&storagev1.StorageClass{}).
		Named("pillarstorageclass")
	// Re-check a blocked PillarStorageClass deletion once one of its volumes is gone.
	return watchVolumeDeletions(bldr, mgr.GetClient(), func() client.ObjectList {
		return &pillarcsiv1alpha1.PillarStorageClassList{}
	}).Complete(r)
}
