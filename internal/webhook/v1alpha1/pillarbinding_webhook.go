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
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var pillarbindinglog = logf.Log.WithName("pillarbinding-resource")

// SetupPillarBindingWebhookWithManager registers the webhook for PillarBinding in the manager.
func SetupPillarBindingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&pillarcsiv1alpha1.PillarBinding{}).
		WithValidator(&PillarBindingCustomValidator{}).
		WithDefaulter(&PillarBindingCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillarbinding,mutating=true,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=create;update,versions=v1alpha1,name=mpillarbinding-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarBindingCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind PillarBinding when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type PillarBindingCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

var _ webhook.CustomDefaulter = &PillarBindingCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind PillarBinding.
func (d *PillarBindingCustomDefaulter) Default(_ context.Context, obj runtime.Object) error {
	pillarbinding, ok := obj.(*pillarcsiv1alpha1.PillarBinding)

	if !ok {
		return fmt.Errorf("expected an PillarBinding object but got %T", obj)
	}
	pillarbindinglog.Info("Defaulting for PillarBinding", "name", pillarbinding.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillarbinding,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=create;update,versions=v1alpha1,name=vpillarbinding-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarBindingCustomValidator struct is responsible for validating the PillarBinding resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarBindingCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

var _ webhook.CustomValidator = &PillarBindingCustomValidator{}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type PillarBinding.
func (v *PillarBindingCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillarbinding, ok := obj.(*pillarcsiv1alpha1.PillarBinding)
	if !ok {
		return nil, fmt.Errorf("expected a PillarBinding object but got %T", obj)
	}
	pillarbindinglog.Info("Validation for PillarBinding upon creation", "name", pillarbinding.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PillarBinding.
func (v *PillarBindingCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	pillarbinding, ok := newObj.(*pillarcsiv1alpha1.PillarBinding)
	if !ok {
		return nil, fmt.Errorf("expected a PillarBinding object for the newObj but got %T", newObj)
	}
	pillarbindinglog.Info("Validation for PillarBinding upon update", "name", pillarbinding.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PillarBinding.
func (v *PillarBindingCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillarbinding, ok := obj.(*pillarcsiv1alpha1.PillarBinding)
	if !ok {
		return nil, fmt.Errorf("expected a PillarBinding object but got %T", obj)
	}
	pillarbindinglog.Info("Validation for PillarBinding upon deletion", "name", pillarbinding.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
