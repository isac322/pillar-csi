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

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

const (
	// pillarBindingFinalizer is added to every PillarBinding to prevent deletion
	// while PVCs still reference the generated StorageClass.
	pillarBindingFinalizer = "pillar-csi.bhyoo.com/binding-protection"

	// requeueAfterBindingDeletionBlock is how long to wait before re-checking
	// whether blocking PVCs have been removed.
	requeueAfterBindingDeletionBlock = 10 * time.Second

	// requeueAfterBindingNotReady is how long to wait before re-checking a
	// binding whose pool or protocol is not yet found or not yet Ready.
	// Watches on PillarPool/PillarProtocol will also re-enqueue the binding
	// when those objects change, but the periodic requeue acts as a safety net
	// for cases where the watch event is missed.
	requeueAfterBindingNotReady = 15 * time.Second

	// pillarCSIProvisioner is the CSI driver name used as the StorageClass provisioner.
	pillarCSIProvisioner = "pillar-csi.bhyoo.com"

	// Condition type constants for PillarBinding status.conditions.

	// conditionPoolReady is True when the referenced PillarPool is in Ready state.
	conditionPoolReady = "PoolReady"

	// conditionProtocolValid is True when the referenced PillarProtocol exists
	// and its Ready condition is True.
	conditionProtocolValid = "ProtocolValid"

	// conditionCompatible is True when the pool backend and protocol are
	// compatible (e.g. block-only backends are incompatible with NFS).
	conditionCompatible = "Compatible"

	// conditionStorageClassCreated is True when the Kubernetes StorageClass
	// has been successfully created and is owned by this PillarBinding.
	conditionStorageClassCreated = "StorageClassCreated"

	// conditionReady is True when all other conditions pass and the binding
	// is fully operational (StorageClass available for PVC provisioning).
	conditionReady = "Ready"
)

// PillarBindingReconciler reconciles a PillarBinding object.
type PillarBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings/finalizers,verbs=update
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For PillarBinding the reconciler:
//  1. Adds a finalizer on first creation (deletion protection).
//  2. On normal operation: validates that the referenced PillarPool and
//     PillarProtocol exist and are ready, checks backend/protocol compatibility,
//     creates/updates the owned Kubernetes StorageClass, and updates status
//     conditions (PoolReady, ProtocolValid, Compatible, StorageClassCreated, Ready).
//  3. On deletion: blocks until no PVCs reference the generated StorageClass,
//     then deletes the StorageClass and removes the finalizer to allow the
//     object to be garbage-collected.
func (r *PillarBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the PillarBinding instance.
	binding := &pillarcsiv1alpha1.PillarBinding{}
	if err := r.Get(ctx, req.NamespacedName, binding); err != nil {
		// Not found — already deleted, nothing to do.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling PillarBinding", "name", binding.Name, "deletionTimestamp", binding.DeletionTimestamp)

	// Branch: object is being deleted.
	if !binding.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, binding)
	}

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(binding, pillarBindingFinalizer) {
		log.Info("Adding finalizer to PillarBinding", "finalizer", pillarBindingFinalizer)
		controllerutil.AddFinalizer(binding, pillarBindingFinalizer)
		if err := r.Update(ctx, binding); err != nil {
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
func storageClassNameFor(binding *pillarcsiv1alpha1.PillarBinding) string {
	if binding.Spec.StorageClass.Name != "" {
		return binding.Spec.StorageClass.Name
	}
	return binding.Name
}

// reconcileNormal handles the steady-state reconciliation of a PillarBinding
// that is not being deleted.
//
// It:
//  1. Looks up and validates the referenced PillarPool (PoolReady condition).
//  2. Looks up and validates the referenced PillarProtocol (ProtocolValid condition).
//  3. Checks that the pool backend and protocol are compatible (Compatible condition).
//  4. Creates or updates the owned StorageClass (StorageClassCreated condition).
//  5. Sets the top-level Ready condition and updates status.storageClassName.
func (r *PillarBindingReconciler) reconcileNormal(ctx context.Context, binding *pillarcsiv1alpha1.PillarBinding) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// --- 1. Validate PillarPool (PoolReady condition) ---
	pool := &pillarcsiv1alpha1.PillarPool{}
	poolErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.PoolRef}, pool)

	switch {
	case poolErr != nil && client.IgnoreNotFound(poolErr) == nil:
		log.Info("Referenced PillarPool not found", "binding", binding.Name, "pool", binding.Spec.PoolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolNotFound",
			Message:            fmt.Sprintf("PillarPool %q was not found in the cluster", binding.Spec.PoolRef),
		})
		setBindingNotReady(binding, "PoolNotFound", fmt.Sprintf("PillarPool %q was not found", binding.Spec.PoolRef))
		if err := r.Status().Update(ctx, binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
		}
		// Requeue: no watch will trigger until the pool is created.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil

	case poolErr != nil:
		log.Error(poolErr, "Failed to get referenced PillarPool", "binding", binding.Name, "pool", binding.Spec.PoolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolLookupError",
			Message:            fmt.Sprintf("Failed to look up PillarPool %q: %v", binding.Spec.PoolRef, poolErr),
		})
		if statusErr := r.Status().Update(ctx, binding); statusErr != nil {
			log.Error(statusErr, "Failed to update PillarBinding status after pool lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PillarPool %q: %w", binding.Spec.PoolRef, poolErr)
	}

	// Pool exists — check its Ready condition.
	poolReadyCond := meta.FindStatusCondition(pool.Status.Conditions, conditionReady)
	poolReady := poolReadyCond != nil && poolReadyCond.Status == metav1.ConditionTrue
	if poolReady {
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolReady",
			Message:            fmt.Sprintf("PillarPool %q is in Ready state", binding.Spec.PoolRef),
		})
	} else {
		msg := fmt.Sprintf("PillarPool %q exists but is not yet Ready", binding.Spec.PoolRef)
		if poolReadyCond != nil {
			msg = fmt.Sprintf("PillarPool %q is not Ready: %s", binding.Spec.PoolRef, poolReadyCond.Message)
		}
		log.Info("Referenced PillarPool is not Ready", "binding", binding.Name, "pool", binding.Spec.PoolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionPoolReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "PoolNotReady",
			Message:            msg,
		})
		setBindingNotReady(binding, "PoolNotReady", msg)
		if err := r.Status().Update(ctx, binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
		}
		// Requeue: pool watch will also re-enqueue when the pool becomes ready,
		// but the periodic requeue acts as a safety net.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil
	}

	// --- 2. Validate PillarProtocol (ProtocolValid condition) ---
	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protocolErr := r.Get(ctx, types.NamespacedName{Name: binding.Spec.ProtocolRef}, protocol)

	switch {
	case protocolErr != nil && client.IgnoreNotFound(protocolErr) == nil:
		log.Info("Referenced PillarProtocol not found", "binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolNotFound",
			Message:            fmt.Sprintf("PillarProtocol %q was not found in the cluster", binding.Spec.ProtocolRef),
		})
		setBindingNotReady(binding, "ProtocolNotFound", fmt.Sprintf("PillarProtocol %q was not found", binding.Spec.ProtocolRef))
		if err := r.Status().Update(ctx, binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
		}
		// Requeue: no watch will trigger until the protocol is created.
		return ctrl.Result{RequeueAfter: requeueAfterBindingNotReady}, nil

	case protocolErr != nil:
		log.Error(protocolErr, "Failed to get referenced PillarProtocol", "binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolLookupError",
			Message:            fmt.Sprintf("Failed to look up PillarProtocol %q: %v", binding.Spec.ProtocolRef, protocolErr),
		})
		if statusErr := r.Status().Update(ctx, binding); statusErr != nil {
			log.Error(statusErr, "Failed to update PillarBinding status after protocol lookup error")
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PillarProtocol %q: %w", binding.Spec.ProtocolRef, protocolErr)
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
			Message:            fmt.Sprintf("PillarProtocol %q is valid (type: %s)", binding.Spec.ProtocolRef, protocol.Spec.Type),
		})
	} else {
		msg := fmt.Sprintf("PillarProtocol %q exists but is not yet Ready", binding.Spec.ProtocolRef)
		if protocolReadyCond != nil {
			msg = fmt.Sprintf("PillarProtocol %q is not Ready: %s", binding.Spec.ProtocolRef, protocolReadyCond.Message)
		}
		log.Info("Referenced PillarProtocol is not Ready", "binding", binding.Name, "protocol", binding.Spec.ProtocolRef)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionProtocolValid,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "ProtocolNotReady",
			Message:            msg,
		})
		setBindingNotReady(binding, "ProtocolNotReady", msg)
		if err := r.Status().Update(ctx, binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
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
		if err := r.Status().Update(ctx, binding); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// --- 4. Create / update owned StorageClass (StorageClassCreated condition) ---
	scName := storageClassNameFor(binding)
	if err := r.reconcileStorageClass(ctx, binding, pool, protocol, scName); err != nil {
		log.Error(err, "Failed to reconcile StorageClass", "binding", binding.Name, "storageClass", scName)
		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionStorageClassCreated,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "StorageClassError",
			Message:            fmt.Sprintf("Failed to reconcile StorageClass %q: %v", scName, err),
		})
		setBindingNotReady(binding, "StorageClassError", fmt.Sprintf("StorageClass %q could not be created/updated", scName))
		if statusErr := r.Status().Update(ctx, binding); statusErr != nil {
			log.Error(statusErr, "Failed to update PillarBinding status after StorageClass error")
		}
		return ctrl.Result{}, err
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
			"PillarBinding is ready: StorageClass %q is available for provisioning (pool: %s, protocol: %s)",
			scName, binding.Spec.PoolRef, binding.Spec.ProtocolRef,
		),
	})

	if err := r.Status().Update(ctx, binding); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update PillarBinding status: %w", err)
	}

	log.Info("PillarBinding reconciled successfully",
		"name", binding.Name,
		"storageClass", scName,
		"pool", binding.Spec.PoolRef,
		"protocol", binding.Spec.ProtocolRef,
	)
	return ctrl.Result{}, nil
}

// evaluateCompatibility checks whether the pool's backend type and the
// protocol type are a valid combination.
//
// Compatibility rules:
//
//  1. NFS (file protocol) is incompatible with block-only backends
//     (zfs-zvol, lvm-lv) because those backends produce raw block devices
//     that cannot be exported via NFS.
//
//  2. Block protocols (nvmeof-tcp, iscsi) are incompatible with file-only
//     backends (zfs-dataset, dir) because those backends expose filesystems
//     or directories, not raw block devices that can be served as NVMe or
//     iSCSI LUNs.
//
//  3. All other combinations are considered compatible:
//     - zfs-zvol  / lvm-lv  + nvmeof-tcp / iscsi  → block ↔ block  ✓
//     - zfs-dataset / dir   + nfs                  → file  ↔ file   ✓
//
// Returns a message describing the incompatibility (or an empty string when
// compatible) and a boolean indicating whether the pair is compatible.
func evaluateCompatibility(pool *pillarcsiv1alpha1.PillarPool, protocol *pillarcsiv1alpha1.PillarProtocol) (string, bool) {
	backend := pool.Spec.Backend.Type
	proto := protocol.Spec.Type

	// Rule 1: Block-only backends cannot be exported over NFS.
	if proto == pillarcsiv1alpha1.ProtocolTypeNFS {
		switch backend {
		case pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.BackendTypeLVMLV:
			return fmt.Sprintf(
				"Backend type %q produces raw block devices and cannot be exported via NFS (protocol %q); "+
					"use zfs-dataset or dir for NFS, or switch to a block protocol (nvmeof-tcp, iscsi)",
				backend, proto,
			), false
		}
	}

	// Rule 2: File-only backends cannot be exposed as block devices via
	// NVMeOF-TCP or iSCSI.
	if proto == pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP || proto == pillarcsiv1alpha1.ProtocolTypeISCSI {
		switch backend {
		case pillarcsiv1alpha1.BackendTypeZFSDataset, pillarcsiv1alpha1.BackendTypeDir:
			return fmt.Sprintf(
				"Backend type %q is a filesystem-based backend and cannot be exposed as a block device via "+
					"protocol %q; use zfs-zvol or lvm-lv for block protocols, or switch to NFS",
				backend, proto,
			), false
		}
	}

	return "", true
}

// buildStorageClassParams constructs the StorageClass parameter map from the
// binding's pool, protocol, and optional overrides.
//
// The parameters encode all configuration that the CSI node/controller plugin
// needs to provision and attach volumes.  They follow the key convention:
//
//	pillar-csi.bhyoo.com/<parameter-name>
func buildStorageClassParams(
	binding *pillarcsiv1alpha1.PillarBinding,
	pool *pillarcsiv1alpha1.PillarPool,
	protocol *pillarcsiv1alpha1.PillarProtocol,
) map[string]string {
	params := map[string]string{
		"pillar-csi.bhyoo.com/pool":          binding.Spec.PoolRef,
		"pillar-csi.bhyoo.com/protocol":      binding.Spec.ProtocolRef,
		"pillar-csi.bhyoo.com/backend-type":  string(pool.Spec.Backend.Type),
		"pillar-csi.bhyoo.com/protocol-type": string(protocol.Spec.Type),
		"pillar-csi.bhyoo.com/target":        pool.Spec.TargetRef,
	}

	// ZFS backend parameters.
	if pool.Spec.Backend.ZFS != nil {
		if pool.Spec.Backend.ZFS.Pool != "" {
			params["pillar-csi.bhyoo.com/zfs-pool"] = pool.Spec.Backend.ZFS.Pool
		}
		if pool.Spec.Backend.ZFS.ParentDataset != "" {
			params["pillar-csi.bhyoo.com/zfs-parent-dataset"] = pool.Spec.Backend.ZFS.ParentDataset
		}
	}

	// Protocol-specific parameters.
	switch protocol.Spec.Type {
	case pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP:
		if protocol.Spec.NVMeOFTCP != nil {
			params["pillar-csi.bhyoo.com/nvmeof-port"] = fmt.Sprintf("%d", protocol.Spec.NVMeOFTCP.Port)
		}
	case pillarcsiv1alpha1.ProtocolTypeISCSI:
		if protocol.Spec.ISCSI != nil {
			params["pillar-csi.bhyoo.com/iscsi-port"] = fmt.Sprintf("%d", protocol.Spec.ISCSI.Port)
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
		mkfsOptions := protocol.Spec.MkfsOptions
		if binding.Spec.Overrides != nil && len(binding.Spec.Overrides.MkfsOptions) > 0 {
			mkfsOptions = binding.Spec.Overrides.MkfsOptions
		}
		if len(mkfsOptions) > 0 {
			params["pillar-csi.bhyoo.com/mkfs-options"] = strings.Join(mkfsOptions, " ")
		}
	}

	return params
}

// reconcileStorageClass creates or updates the StorageClass owned by this binding.
//
// It uses controllerutil.CreateOrUpdate to apply a desired StorageClass spec,
// and sets an owner reference so that deleting the PillarBinding cascades to
// the StorageClass (once no PVCs are blocking deletion).
func (r *PillarBindingReconciler) reconcileStorageClass(
	ctx context.Context,
	binding *pillarcsiv1alpha1.PillarBinding,
	pool *pillarcsiv1alpha1.PillarPool,
	protocol *pillarcsiv1alpha1.PillarProtocol,
	scName string,
) error {
	log := logf.FromContext(ctx)

	// Build the desired ReclaimPolicy.
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	if binding.Spec.StorageClass.ReclaimPolicy == pillarcsiv1alpha1.ReclaimPolicyRetain {
		reclaimPolicy = corev1.PersistentVolumeReclaimRetain
	}

	// Build the desired VolumeBindingMode.
	volumeBindingMode := storagev1.VolumeBindingImmediate
	if binding.Spec.StorageClass.VolumeBindingMode == pillarcsiv1alpha1.VolumeBindingWaitForFirstConsumer {
		volumeBindingMode = storagev1.VolumeBindingWaitForFirstConsumer
	}

	params := buildStorageClassParams(binding, pool, protocol)

	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, sc, func() error {
		// Set owner reference so that the StorageClass is garbage-collected
		// when the PillarBinding is deleted (after PVC blocking is resolved).
		if err := controllerutil.SetControllerReference(binding, sc, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on StorageClass: %w", err)
		}

		sc.Provisioner = pillarCSIProvisioner
		sc.Parameters = params
		sc.ReclaimPolicy = &reclaimPolicy
		sc.VolumeBindingMode = &volumeBindingMode

		// AllowVolumeExpansion: use spec value when explicitly set.
		if binding.Spec.StorageClass.AllowVolumeExpansion != nil {
			sc.AllowVolumeExpansion = binding.Spec.StorageClass.AllowVolumeExpansion
		} else {
			// Default: allow expansion for block backends (zvol, lvm), not for NFS.
			defaultAllow := protocol.Spec.Type != pillarcsiv1alpha1.ProtocolTypeNFS
			sc.AllowVolumeExpansion = &defaultAllow
		}

		return nil
	})
	if err != nil {
		return err
	}

	log.Info("StorageClass reconciled", "name", scName, "operation", op)
	return nil
}

// setBindingNotReady is a helper that sets the top-level Ready condition to
// False with the given reason and message.
func setBindingNotReady(binding *pillarcsiv1alpha1.PillarBinding, reason, message string) {
	meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: binding.Generation,
		Reason:             reason,
		Message:            fmt.Sprintf("PillarBinding is not ready: %s", message),
	})
}

// reconcileDelete handles the deletion flow for PillarBinding.
//
// Deletion is blocked until no PVCs reference the generated StorageClass
// (to prevent orphaned volumes).  Once the StorageClass is no longer in use,
// the finalizer is removed and Kubernetes can garbage-collect the object.
func (r *PillarBindingReconciler) reconcileDelete(ctx context.Context, binding *pillarcsiv1alpha1.PillarBinding) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If our finalizer is not present (e.g. stripped manually), nothing to do.
	if !controllerutil.ContainsFinalizer(binding, pillarBindingFinalizer) {
		return ctrl.Result{}, nil
	}

	log.Info("PillarBinding is being deleted — checking for PVCs that reference the StorageClass", "name", binding.Name)

	// Determine the StorageClass name from status (it was set during creation)
	// or fall back to computing it from spec.
	scName := binding.Status.StorageClassName
	if scName == "" {
		scName = storageClassNameFor(binding)
	}

	// List all PVCs cluster-wide and find those that reference this StorageClass.
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list PersistentVolumeClaims: %w", err)
	}

	var blockingPVCs []string
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == scName {
			blockingPVCs = append(blockingPVCs, fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
		}
	}

	if len(blockingPVCs) > 0 {
		msg := fmt.Sprintf(
			"Deletion blocked: PVC(s) [%s] still reference StorageClass %q; delete them first",
			strings.Join(blockingPVCs, ", "), scName,
		)
		log.Info(msg, "name", binding.Name)

		meta.SetStatusCondition(&binding.Status.Conditions, metav1.Condition{
			Type:               conditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: binding.Generation,
			Reason:             "DeletionBlocked",
			Message:            msg,
		})

		if statusErr := r.Status().Update(ctx, binding); statusErr != nil {
			log.Error(statusErr, "Failed to update status while deletion is blocked")
		}

		return ctrl.Result{RequeueAfter: requeueAfterBindingDeletionBlock}, nil
	}

	// No blocking PVCs — delete the owned StorageClass if it still exists.
	sc := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: scName}, sc); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get StorageClass %q: %w", scName, err)
		}
		// StorageClass already gone — nothing more to clean up.
	} else {
		// StorageClass exists — delete it explicitly (ownerRef GC may not fire
		// instantly, and we want deterministic cleanup in the deletion path).
		log.Info("Deleting owned StorageClass", "name", scName)
		if err := r.Delete(ctx, sc); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to delete StorageClass %q: %w", scName, err)
		}
	}

	// Safe to remove the finalizer.
	log.Info("No PVCs reference StorageClass; removing finalizer", "binding", binding.Name, "storageClass", scName)
	controllerutil.RemoveFinalizer(binding, pillarBindingFinalizer)
	if err := r.Update(ctx, binding); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to remove finalizer from PillarBinding: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
//
// The controller watches:
//   - PillarBinding (primary resource).
//   - PillarPool: re-enqueues bindings that reference a pool whenever the
//     pool changes — so that the PoolReady condition stays current.
//   - PillarProtocol: re-enqueues bindings that reference a protocol whenever
//     the protocol changes — so that the ProtocolValid condition stays current.
//   - StorageClass: re-enqueues the owner PillarBinding when the managed
//     StorageClass is modified or deleted (drift detection / self-healing).
func (r *PillarBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapPoolToBindings returns reconcile Requests for every PillarBinding
	// whose spec.poolRef matches the PillarPool that just changed.
	mapPoolToBindings := func(ctx context.Context, obj client.Object) []reconcile.Request {
		pool, ok := obj.(*pillarcsiv1alpha1.PillarPool)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarBindingList{}
		if err := mgr.GetClient().List(ctx, bindingList); err != nil {
			return nil
		}

		var requests []reconcile.Request
		for i := range bindingList.Items {
			if bindingList.Items[i].Spec.PoolRef == pool.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: bindingList.Items[i].Name},
				})
			}
		}
		return requests
	}

	// mapProtocolToBindings returns reconcile Requests for every PillarBinding
	// whose spec.protocolRef matches the PillarProtocol that just changed.
	mapProtocolToBindings := func(ctx context.Context, obj client.Object) []reconcile.Request {
		protocol, ok := obj.(*pillarcsiv1alpha1.PillarProtocol)
		if !ok {
			return nil
		}

		bindingList := &pillarcsiv1alpha1.PillarBindingList{}
		if err := mgr.GetClient().List(ctx, bindingList); err != nil {
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&pillarcsiv1alpha1.PillarBinding{}).
		// Re-enqueue bindings whenever the referenced PillarPool changes.
		Watches(
			&pillarcsiv1alpha1.PillarPool{},
			handler.EnqueueRequestsFromMapFunc(mapPoolToBindings),
		).
		// Re-enqueue bindings whenever the referenced PillarProtocol changes.
		Watches(
			&pillarcsiv1alpha1.PillarProtocol{},
			handler.EnqueueRequestsFromMapFunc(mapProtocolToBindings),
		).
		// Automatically re-enqueue the owning PillarBinding when its managed
		// StorageClass is modified externally (self-healing).
		Owns(&storagev1.StorageClass{}).
		Named("pillarbinding").
		Complete(r)
}
