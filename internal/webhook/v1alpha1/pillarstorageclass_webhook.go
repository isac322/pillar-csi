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

// Package v1alpha1 implements admission webhooks for the pillar-csi.bhyoo.com/v1alpha1 API group.
package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var pillarstorageclasslog = logf.Log.WithName("pillarstorageclass-resource")

// SetupPillarStorageClassWebhookWithManager registers the webhook for PillarStorageClass in the manager.
func SetupPillarStorageClassWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &pillarcsiv1alpha1.PillarStorageClass{}).
		WithValidator(&PillarStorageClassCustomValidator{Client: mgr.GetClient()}).
		WithDefaulter(&PillarStorageClassCustomDefaulter{Client: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-pillar-csi-bhyoo-com-v1alpha1-pillarstorageclass,mutating=true,failurePolicy=fail,sideEffects=None,groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses,verbs=create;update,versions=v1alpha1,name=mpillarstorageclass-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarStorageClassCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind PillarStorageClass when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PillarStorageClassCustomDefaulter struct {
	// Client is used to look up referenced PillarStore resources so that
	// allowVolumeExpansion can be derived from the pool's backend type.
	Client client.Client
}

var _ admission.Defaulter[*pillarcsiv1alpha1.PillarStorageClass] = &PillarStorageClassCustomDefaulter{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind PillarStorageClass.
func (d *PillarStorageClassCustomDefaulter) Default(
	ctx context.Context, pillarstorageclass *pillarcsiv1alpha1.PillarStorageClass,
) error {
	pillarstorageclasslog.Info("Defaulting for PillarStorageClass", "name", pillarstorageclass.GetName())

	// Auto-set allowVolumeExpansion from the referenced pool's backend type when
	// the user has not explicitly configured the field.
	if pillarstorageclass.Spec.StorageClass.AllowVolumeExpansion == nil {
		err := d.defaultAllowVolumeExpansion(ctx, pillarstorageclass)
		if err != nil {
			// The pool may not exist yet (e.g., created after the binding).
			// Skip silently rather than blocking admission – the controller will
			// reconcile the generated StorageClass once the pool becomes available.
			pillarstorageclasslog.V(1).Info("Skipping allowVolumeExpansion auto-detection",
				"reason", err.Error(),
				"storeRef", pillarstorageclass.Spec.StoreRef)
		}
	}

	return nil
}

// defaultAllowVolumeExpansion looks up the referenced PillarStore and writes
// spec.storageClass.allowVolumeExpansion based on the pool's backend type.
func (d *PillarStorageClassCustomDefaulter) defaultAllowVolumeExpansion(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarStorageClass,
) error {
	if d.Client == nil {
		return fmt.Errorf("defaulter client is nil, cannot look up PillarStore")
	}
	pool := &pillarcsiv1alpha1.PillarStore{}
	err := d.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.StoreRef}, pool)
	if err != nil {
		return fmt.Errorf("cannot look up PillarStore %q: %w", pb.Spec.StoreRef, err)
	}
	val := backendSupportsVolumeExpansion(pool.Spec.Backend.Type)
	pb.Spec.StorageClass.AllowVolumeExpansion = &val
	return nil
}

// backendSupportsVolumeExpansion returns true when the given backend type can
// resize volumes online. Block-device backends (zfs-zvol, lvm-lv) support
// expansion; filesystem/directory backends (zfs-dataset, dir) do not.
func backendSupportsVolumeExpansion(bt pillarcsiv1alpha1.BackendType) bool {
	switch bt {
	case pillarcsiv1alpha1.BackendTypeZFSZvol, pillarcsiv1alpha1.BackendTypeLVMLV:
		return true
	default: // zfs-dataset, dir, and any unknown future backend types
		return false
	}
}

// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-bhyoo-com-v1alpha1-pillarstorageclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.bhyoo.com,resources=pillarstorageclasses,verbs=create;update,versions=v1alpha1,name=vpillarstorageclass-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarStorageClassCustomValidator struct is responsible for validating the PillarStorageClass resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarStorageClassCustomValidator struct {
	// Client is used to look up referenced PillarStore and PillarProtocol resources in order
	// to verify that the backend type and protocol type are compatible.
	Client client.Client
}

var _ admission.Validator[*pillarcsiv1alpha1.PillarStorageClass] = &PillarStorageClassCustomValidator{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type PillarStorageClass.
func (v *PillarStorageClassCustomValidator) ValidateCreate(
	ctx context.Context, pillarstorageclass *pillarcsiv1alpha1.PillarStorageClass,
) (admission.Warnings, error) {
	pillarstorageclasslog.Info("Validation for PillarStorageClass upon creation", "name", pillarstorageclass.GetName())

	err := v.validateCompatibility(ctx, pillarstorageclass)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type PillarStorageClass.
func (v *PillarStorageClassCustomValidator) ValidateUpdate(
	ctx context.Context, oldBinding, newBinding *pillarcsiv1alpha1.PillarStorageClass,
) (admission.Warnings, error) {
	pillarstorageclasslog.Info("Validation for PillarStorageClass upon update", "name", newBinding.GetName())

	var allErrs field.ErrorList

	// spec.storeRef is immutable: a binding owns a generated StorageClass that is tied to a
	// specific pool.  Changing storeRef mid-flight would silently redirect new PVC provisioning
	// to a different pool while leaving the StorageClass name unchanged, causing confusion.
	if oldBinding.Spec.StoreRef != newBinding.Spec.StoreRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "storeRef"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldBinding.Spec.StoreRef, newBinding.Spec.StoreRef),
		))
	}

	// spec.protocolRef is immutable: the binding's StorageClass encodes a specific network
	// protocol path.  Changing protocolRef would silently alter the access mode and
	// connectivity for all PVCs already provisioned through this binding.
	if oldBinding.Spec.ProtocolRef != newBinding.Spec.ProtocolRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "protocolRef"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldBinding.Spec.ProtocolRef, newBinding.Spec.ProtocolRef),
		))
	}

	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}

	// Also verify that the new binding's backend-protocol combination is still valid.
	// (storeRef and protocolRef are immutable, so this is only relevant when neither
	// changed — but we still need to guard against a cluster state change between
	// admission calls.)
	compatErr := v.validateCompatibility(ctx, newBinding)
	if compatErr != nil {
		return nil, compatErr
	}

	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type PillarStorageClass.
func (*PillarStorageClassCustomValidator) ValidateDelete(
	_ context.Context, pillarstorageclass *pillarcsiv1alpha1.PillarStorageClass,
) (admission.Warnings, error) {
	pillarstorageclasslog.Info("Validation for PillarStorageClass upon deletion", "name", pillarstorageclass.GetName())

	return nil, nil
}

// validateCompatibility checks that the backend type of the referenced
// PillarStore and the protocol type of the referenced PillarProtocol are a
// valid combination. If either referenced resource does not yet exist the
// check is skipped: the controller will detect and surface the mismatch via
// status conditions once both resources are available.
func (v *PillarStorageClassCustomValidator) validateCompatibility(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarStorageClass,
) error {
	if v.Client == nil {
		return nil
	}

	pool := &pillarcsiv1alpha1.PillarStore{}
	poolErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.StoreRef}, pool)
	if poolErr != nil {
		// Pool not found yet — skip; controller reconciliation handles this case.
		pillarstorageclasslog.V(1).Info("Skipping compatibility check: cannot fetch pool",
			"storeRef", pb.Spec.StoreRef, "reason", poolErr.Error())
		return nil
	}

	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protoErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.ProtocolRef}, protocol)
	if protoErr != nil {
		// Protocol not found yet — skip; controller reconciliation handles this case.
		pillarstorageclasslog.V(1).Info("Skipping compatibility check: cannot fetch protocol",
			"protocolRef", pb.Spec.ProtocolRef, "reason", protoErr.Error())
		return nil
	}

	backendType := pool.Spec.Backend.Type
	protocolType := protocol.Spec.Type
	compat := pillarcsiv1alpha1.Compatible(backendType, protocolType)
	if !compat.OK {
		return field.Invalid(
			field.NewPath("spec", "protocolRef"),
			pb.Spec.ProtocolRef,
			compat.Message,
		)
	}

	return nil
}
