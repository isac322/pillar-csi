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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var pillarprotocollog = logf.Log.WithName("pillarprotocol-resource")

// SetupPillarProtocolWebhookWithManager registers the webhook for PillarProtocol in the manager.
func SetupPillarProtocolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&pillarcsiv1alpha1.PillarProtocol{}).
		WithValidator(&PillarProtocolCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillarprotocol,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=create;update,versions=v1alpha1,name=vpillarprotocol-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarProtocolCustomValidator struct is responsible for validating the PillarProtocol resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarProtocolCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &PillarProtocolCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateCreate(
	_ context.Context, obj runtime.Object,
) (admission.Warnings, error) {
	pillarprotocol, ok := obj.(*pillarcsiv1alpha1.PillarProtocol)
	if !ok {
		return nil, fmt.Errorf("expected a PillarProtocol object but got %T", obj)
	}
	pillarprotocollog.Info("Validation for PillarProtocol upon creation", "name", pillarprotocol.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateUpdate(
	_ context.Context, oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	newProtocol, ok := newObj.(*pillarcsiv1alpha1.PillarProtocol)
	if !ok {
		return nil, fmt.Errorf("expected a PillarProtocol object for the newObj but got %T", newObj)
	}
	oldProtocol, ok := oldObj.(*pillarcsiv1alpha1.PillarProtocol)
	if !ok {
		return nil, fmt.Errorf("expected a PillarProtocol object for the oldObj but got %T", oldObj)
	}
	pillarprotocollog.Info("Validation for PillarProtocol upon update", "name", newProtocol.GetName())

	var allErrs field.ErrorList

	// spec.type is immutable: each protocol type requires a distinct kernel subsystem and configfs
	// namespace (NVMe-oF vs iSCSI vs NFS). Allowing type changes would orphan all volumes that were
	// exported via the original protocol without any migration path.
	if oldProtocol.Spec.Type != newProtocol.Spec.Type {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "type"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldProtocol.Spec.Type, newProtocol.Spec.Type),
		))
	}

	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateDelete(
	_ context.Context, obj runtime.Object,
) (admission.Warnings, error) {
	pillarprotocol, ok := obj.(*pillarcsiv1alpha1.PillarProtocol)
	if !ok {
		return nil, fmt.Errorf("expected a PillarProtocol object but got %T", obj)
	}
	pillarprotocollog.Info("Validation for PillarProtocol upon deletion", "name", pillarprotocol.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
