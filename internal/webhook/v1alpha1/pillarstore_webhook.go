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

package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var pillarstorelog = logf.Log.WithName("pillarstore-resource")

// SetupPillarStoreWebhookWithManager registers the webhook for PillarStore in the manager.
func SetupPillarStoreWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &pillarcsiv1alpha1.PillarStore{}).
		WithValidator(&PillarStoreCustomValidator{}).
		Complete()
}

// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-bhyoo-com-v1alpha1-pillarstore,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.bhyoo.com,resources=pillarstores,verbs=create;update,versions=v1alpha1,name=vpillarstore-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarStoreCustomValidator struct is responsible for validating the PillarStore resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarStoreCustomValidator struct{}

var _ admission.Validator[*pillarcsiv1alpha1.PillarStore] = &PillarStoreCustomValidator{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type PillarStore.
func (*PillarStoreCustomValidator) ValidateCreate(
	_ context.Context, pillarstore *pillarcsiv1alpha1.PillarStore,
) (admission.Warnings, error) {
	pillarstorelog.Info("Validation for PillarStore upon creation", "name", pillarstore.GetName())

	return nil, validatePillarStoreSpec(pillarstore)
}

// validatePillarStoreSpec re-checks the backend union of a PillarStore spec.
// The CRD schema (CEL exactly-one rule, MinLength on pool/volumeGroup)
// already enforces these constraints at the API server; the webhook repeats
// them so a schema that failed to install can never admit a store without a
// usable backend.
func validatePillarStoreSpec(store *pillarcsiv1alpha1.PillarStore) error {
	var allErrs field.ErrorList
	backendPath := field.NewPath("spec", "backend")
	backend := store.Spec.Backend

	switch {
	case backend.ZFS != nil && backend.LVM != nil:
		allErrs = append(allErrs, field.Invalid(backendPath, "zfs, lvm",
			"exactly one of zfs or lvm must be set"))
	case backend.Kind() == "":
		allErrs = append(allErrs, field.Required(backendPath,
			"exactly one of zfs or lvm must be set"))
	case backend.ZFS != nil && backend.ZFS.Pool == "":
		allErrs = append(allErrs, field.Required(backendPath.Child("zfs", "pool"),
			"the ZFS pool name must be non-empty"))
	case backend.LVM != nil && backend.LVM.VolumeGroup == "":
		allErrs = append(allErrs, field.Required(backendPath.Child("lvm", "volumeGroup"),
			"the LVM volume group name must be non-empty"))
	}

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}

// backendMember returns the name of the union member set in a backend spec
// ("zfs" or "lvm"), or "" when none is set.
func backendMember(b pillarcsiv1alpha1.BackendSpec) string {
	switch {
	case b.ZFS != nil:
		return "zfs"
	case b.LVM != nil:
		return "lvm"
	default:
		return ""
	}
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type PillarStore.
func (*PillarStoreCustomValidator) ValidateUpdate(
	_ context.Context, oldStore, newStore *pillarcsiv1alpha1.PillarStore,
) (admission.Warnings, error) {
	pillarstorelog.Info("Validation for PillarStore upon update", "name", newStore.GetName())

	var allErrs field.ErrorList

	// spec.agentRef is immutable: the store is bound to a specific storage agent at creation.
	// Moving a store to a different agent would change which physical storage is used,
	// invalidating all existing volumes provisioned from this store.
	if oldStore.Spec.AgentRef != newStore.Spec.AgentRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "agentRef"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldStore.Spec.AgentRef, newStore.Spec.AgentRef),
		))
	}

	// The backend member (zfs or lvm) is immutable: switching backends would silently
	// break volumes that were provisioned by the original backend.
	oldMember := backendMember(oldStore.Spec.Backend)
	newMember := backendMember(newStore.Spec.Backend)
	if oldMember != newMember {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "backend"),
			fmt.Sprintf("the backend member is immutable; old member %q cannot be changed to %q",
				oldMember, newMember),
		))
	}

	// The physical pool identity is immutable: it is embedded in the volume IDs of every
	// volume provisioned from this store.
	oldZFS, newZFS := oldStore.Spec.Backend.ZFS, newStore.Spec.Backend.ZFS
	if oldZFS != nil && newZFS != nil && oldZFS.Pool != newZFS.Pool {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "backend", "zfs", "pool"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldZFS.Pool, newZFS.Pool),
		))
	}
	oldLVM, newLVM := oldStore.Spec.Backend.LVM, newStore.Spec.Backend.LVM
	if oldLVM != nil && newLVM != nil && oldLVM.VolumeGroup != newLVM.VolumeGroup {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "backend", "lvm", "volumeGroup"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldLVM.VolumeGroup, newLVM.VolumeGroup),
		))
	}

	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, validatePillarStoreSpec(newStore)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type PillarStore.
func (*PillarStoreCustomValidator) ValidateDelete(
	_ context.Context, pillarstore *pillarcsiv1alpha1.PillarStore,
) (admission.Warnings, error) {
	pillarstorelog.Info("Validation for PillarStore upon deletion", "name", pillarstore.GetName())

	return nil, nil
}
