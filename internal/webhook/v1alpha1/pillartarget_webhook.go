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

var pillartargetlog = logf.Log.WithName("pillartarget-resource")

// SetupPillarTargetWebhookWithManager registers the webhook for PillarTarget in the manager.
func SetupPillarTargetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&pillarcsiv1alpha1.PillarTarget{}).
		WithValidator(&PillarTargetCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillartarget,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillartargets,verbs=create;update,versions=v1alpha1,name=vpillartarget-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarTargetCustomValidator struct is responsible for validating the PillarTarget resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarTargetCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &PillarTargetCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PillarTarget.
func (*PillarTargetCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillartarget, ok := obj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object but got %T", obj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon creation", "name", pillartarget.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PillarTarget.
func (*PillarTargetCustomValidator) ValidateUpdate(
	_ context.Context, oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	newTarget, ok := newObj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object for the newObj but got %T", newObj)
	}
	oldTarget, ok := oldObj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object for the oldObj but got %T", oldObj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon update", "name", newTarget.GetName())

	var allErrs field.ErrorList

	// spec.nodeRef / spec.external form a discriminated union that identifies
	// which physical agent this target connects to.  Switching between the two
	// variants (or changing the core identity within the same variant) is
	// equivalent to pointing the target at a completely different machine, which
	// would silently break all PillarPools and PillarBindings that depend on it.

	oldHasNodeRef := oldTarget.Spec.NodeRef != nil
	newHasNodeRef := newTarget.Spec.NodeRef != nil

	switch {
	case oldHasNodeRef != newHasNodeRef:
		// Discriminant switch: nodeRef ↔ external
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec"),
			"cannot switch between nodeRef and external after creation; "+
				"delete and recreate the PillarTarget instead",
		))
	case oldHasNodeRef:
		// Both old and new use nodeRef — the node name identifies the agent host.
		if oldTarget.Spec.NodeRef.Name != newTarget.Spec.NodeRef.Name {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "nodeRef", "name"),
				fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
					oldTarget.Spec.NodeRef.Name, newTarget.Spec.NodeRef.Name),
			))
		}
	case oldTarget.Spec.External != nil && newTarget.Spec.External != nil:
		// Both old and new use external — the address+port pair identifies the agent host.
		if oldTarget.Spec.External.Address != newTarget.Spec.External.Address {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "external", "address"),
				fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
					oldTarget.Spec.External.Address, newTarget.Spec.External.Address),
			))
		}
		if oldTarget.Spec.External.Port != newTarget.Spec.External.Port {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "external", "port"),
				fmt.Sprintf("field is immutable; old value %d cannot be changed to %d",
					oldTarget.Spec.External.Port, newTarget.Spec.External.Port),
			))
		}
	}

	if len(allErrs) > 0 {
		return nil, allErrs.ToAggregate()
	}
	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PillarTarget.
func (*PillarTargetCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillartarget, ok := obj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object but got %T", obj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon deletion", "name", pillartarget.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
