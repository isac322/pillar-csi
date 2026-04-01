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

var pillarpoollog = logf.Log.WithName("pillarpool-resource")

// SetupPillarPoolWebhookWithManager registers the webhook for PillarPool in the manager.
func SetupPillarPoolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &pillarcsiv1alpha1.PillarPool{}).
		WithValidator(&PillarPoolCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillarpool,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarpools,verbs=create;update,versions=v1alpha1,name=vpillarpool-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarPoolCustomValidator struct is responsible for validating the PillarPool resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarPoolCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ admission.Validator[*pillarcsiv1alpha1.PillarPool] = &PillarPoolCustomValidator{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type PillarPool.
func (*PillarPoolCustomValidator) ValidateCreate(
	_ context.Context, pillarpool *pillarcsiv1alpha1.PillarPool,
) (admission.Warnings, error) {
	pillarpoollog.Info("Validation for PillarPool upon creation", "name", pillarpool.GetName())

	return nil, validatePillarPoolSpec(pillarpool)
}

// validatePillarPoolSpec applies cross-field validation rules to a PillarPool spec.
// These rules enforce constraints that cannot be expressed in OpenAPI schema alone
// (e.g., requiring backend.lvm when backend.type == "lvm-lv").
func validatePillarPoolSpec(pool *pillarcsiv1alpha1.PillarPool) error {
	var allErrs field.ErrorList

	// When backend.type is lvm-lv, the backend.lvm section must be present and
	// must provide at minimum a non-empty volumeGroup so that the agent knows
	// which LVM Volume Group to use.
	if pool.Spec.Backend.Type == pillarcsiv1alpha1.BackendTypeLVMLV {
		if pool.Spec.Backend.LVM == nil {
			allErrs = append(allErrs, field.Required(
				field.NewPath("spec", "backend", "lvm"),
				fmt.Sprintf("spec.backend.lvm is required when spec.backend.type is %q", pillarcsiv1alpha1.BackendTypeLVMLV),
			))
		} else if pool.Spec.Backend.LVM.VolumeGroup == "" {
			allErrs = append(allErrs, field.Required(
				field.NewPath("spec", "backend", "lvm", "volumeGroup"),
				"spec.backend.lvm.volumeGroup must be non-empty when spec.backend.type is lvm-lv",
			))
		}
	}

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type PillarPool.
func (*PillarPoolCustomValidator) ValidateUpdate(
	_ context.Context, oldPool, newPool *pillarcsiv1alpha1.PillarPool,
) (admission.Warnings, error) {
	pillarpoollog.Info("Validation for PillarPool upon update", "name", newPool.GetName())

	var allErrs field.ErrorList

	// spec.targetRef is immutable: the pool is bound to a specific storage target at creation.
	// Moving a pool to a different target would change which physical storage is used,
	// invalidating all existing volumes provisioned from this pool.
	if oldPool.Spec.TargetRef != newPool.Spec.TargetRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "targetRef"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldPool.Spec.TargetRef, newPool.Spec.TargetRef),
		))
	}

	// spec.backend.type is immutable: changing the backend driver type (e.g. zfs-zvol → lvm-lv)
	// would silently break volumes that were provisioned using the original driver.
	if oldPool.Spec.Backend.Type != newPool.Spec.Backend.Type {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "backend", "type"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldPool.Spec.Backend.Type, newPool.Spec.Backend.Type),
		))
	}

	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type PillarPool.
func (*PillarPoolCustomValidator) ValidateDelete(
	_ context.Context, pillarpool *pillarcsiv1alpha1.PillarPool,
) (admission.Warnings, error) {
	pillarpoollog.Info("Validation for PillarPool upon deletion", "name", pillarpool.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
