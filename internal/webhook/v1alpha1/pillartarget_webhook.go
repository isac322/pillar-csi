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
var pillartargetlog = logf.Log.WithName("pillartarget-resource")

// SetupPillarTargetWebhookWithManager registers the webhook for PillarTarget in the manager.
func SetupPillarTargetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&pillarcsiv1alpha1.PillarTarget{}).
		WithValidator(&PillarTargetCustomValidator{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
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
func (v *PillarTargetCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillartarget, ok := obj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object but got %T", obj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon creation", "name", pillartarget.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type PillarTarget.
func (v *PillarTargetCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	pillartarget, ok := newObj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object for the newObj but got %T", newObj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon update", "name", pillartarget.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type PillarTarget.
func (v *PillarTargetCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	pillartarget, ok := obj.(*pillarcsiv1alpha1.PillarTarget)
	if !ok {
		return nil, fmt.Errorf("expected a PillarTarget object but got %T", obj)
	}
	pillartargetlog.Info("Validation for PillarTarget upon deletion", "name", pillartarget.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
