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
	// allowVolumeExpansion can be derived from the store's backend member.
	Client client.Client
}

var _ admission.Defaulter[*pillarcsiv1alpha1.PillarStorageClass] = &PillarStorageClassCustomDefaulter{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind PillarStorageClass.
func (d *PillarStorageClassCustomDefaulter) Default(
	ctx context.Context, pillarstorageclass *pillarcsiv1alpha1.PillarStorageClass,
) error {
	pillarstorageclasslog.Info("Defaulting for PillarStorageClass", "name", pillarstorageclass.GetName())

	// Auto-set allowVolumeExpansion from the referenced store's backend member when
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
// spec.storageClass.allowVolumeExpansion based on the store's backend member.
func (d *PillarStorageClassCustomDefaulter) defaultAllowVolumeExpansion(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarStorageClass,
) error {
	if d.Client == nil {
		return fmt.Errorf("defaulter client is nil, cannot look up PillarStore")
	}
	store := &pillarcsiv1alpha1.PillarStore{}
	err := d.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.StoreRef}, store)
	if err != nil {
		return fmt.Errorf("cannot look up PillarStore %q: %w", pb.Spec.StoreRef, err)
	}
	val := backendSupportsVolumeExpansion(store.Spec.Backend.Kind())
	pb.Spec.StorageClass.AllowVolumeExpansion = &val
	return nil
}

// backendSupportsVolumeExpansion returns true when the given backend can
// resize volumes online: every block-device backend (zfs zvol, lvm LV) can.
func backendSupportsVolumeExpansion(id pillarcsiv1alpha1.BackendID) bool {
	return pillarcsiv1alpha1.CategoryOf(id) == pillarcsiv1alpha1.BackendCategoryBlock
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
	// to verify backend/protocol compatibility and override member matches.
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

	// The generated StorageClass name is immutable: PVCs and PVs reference their class
	// by name, and a rename would leave the old StorageClass orphaned (its owner reference
	// only cascades on deletion of this binding) while new PVCs silently switch classes.
	// The name defaults to the binding name, so setting or clearing it is also a rename.
	oldSCName := effectiveStorageClassName(oldBinding)
	newSCName := effectiveStorageClassName(newBinding)
	if oldSCName != newSCName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "storageClass", "name"),
			fmt.Sprintf("the generated StorageClass name is immutable; old value %q cannot be changed to %q",
				oldSCName, newSCName),
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

// effectiveStorageClassName returns the name of the StorageClass generated for
// pb: spec.storageClass.name, defaulting to the binding's own name.  It mirrors
// storageClassNameFor in the PillarStorageClass controller.
func effectiveStorageClassName(pb *pillarcsiv1alpha1.PillarStorageClass) string {
	if pb.Spec.StorageClass.Name != "" {
		return pb.Spec.StorageClass.Name
	}
	return pb.Name
}

// validateCompatibility checks the binding against the live PillarStore and
// PillarProtocol it references:
//   - the backend and protocol must be a compatible combination;
//   - spec.overrides.backend must select the same member (zfs/lvm) as the
//     store's backend, and spec.overrides.protocol the same member
//     (nvmeofTcp) as the protocol — an override for another member could
//     never apply and is rejected again at CreateVolume.
//
// If either referenced resource does not yet exist the checks that need it
// are skipped: the controller detects and surfaces the problem via status
// conditions once both resources are available, and CreateVolume re-checks
// the overrides.
func (v *PillarStorageClassCustomValidator) validateCompatibility(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarStorageClass,
) error {
	if v.Client == nil {
		return nil
	}

	var allErrs field.ErrorList
	overridesPath := field.NewPath("spec", "overrides")

	store := &pillarcsiv1alpha1.PillarStore{}
	storeErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.StoreRef}, store)
	if storeErr != nil {
		// Store not found yet — skip; controller reconciliation handles this case.
		pillarstorageclasslog.V(1).Info("Skipping store-dependent checks: cannot fetch store",
			"storeRef", pb.Spec.StoreRef, "reason", storeErr.Error())
	} else if pb.Spec.Overrides != nil && pb.Spec.Overrides.Backend != nil {
		overrideMember := pb.Spec.Overrides.Backend.Kind()
		storeMember := backendMember(store.Spec.Backend)
		if overrideMember != storeMember {
			allErrs = append(allErrs, field.Invalid(
				overridesPath.Child("backend"), overrideMember,
				fmt.Sprintf("backend override member %q does not match the %q backend of PillarStore %q",
					overrideMember, storeMember, pb.Spec.StoreRef),
			))
		}
	}

	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protoErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.ProtocolRef}, protocol)
	if protoErr != nil {
		// Protocol not found yet — skip; controller reconciliation handles this case.
		pillarstorageclasslog.V(1).Info("Skipping protocol-dependent checks: cannot fetch protocol",
			"protocolRef", pb.Spec.ProtocolRef, "reason", protoErr.Error())
	} else if pb.Spec.Overrides != nil && pb.Spec.Overrides.Protocol != nil {
		overrideMember := pb.Spec.Overrides.Protocol.Kind()
		protocolMemberName := protocolMember(protocol.Spec.Protocol)
		if overrideMember != protocolMemberName {
			allErrs = append(allErrs, field.Invalid(
				overridesPath.Child("protocol"), overrideMember,
				fmt.Sprintf("protocol override member %q does not match the %q protocol of PillarProtocol %q",
					overrideMember, protocolMemberName, pb.Spec.ProtocolRef),
			))
		}
	}

	if storeErr == nil && protoErr == nil {
		compat := pillarcsiv1alpha1.Compatible(store.Spec.Backend, protocol.Spec.Protocol)
		if !compat.OK {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec", "protocolRef"),
				pb.Spec.ProtocolRef,
				compat.Message,
			))
		}
	}

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}
