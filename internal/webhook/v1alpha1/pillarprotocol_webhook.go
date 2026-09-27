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
	"math"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

var pillarprotocollog = logf.Log.WithName("pillarprotocol-resource")

// SetupPillarProtocolWebhookWithManager registers the webhook for PillarProtocol in the manager.
func SetupPillarProtocolWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &pillarcsiv1alpha1.PillarProtocol{}).
		WithValidator(&PillarProtocolCustomValidator{}).
		Complete()
}

// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-bhyoo-com-v1alpha1-pillarprotocol,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.bhyoo.com,resources=pillarprotocols,verbs=create;update,versions=v1alpha1,name=vpillarprotocol-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarProtocolCustomValidator struct is responsible for validating the PillarProtocol resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarProtocolCustomValidator struct{}

var _ admission.Validator[*pillarcsiv1alpha1.PillarProtocol] = &PillarProtocolCustomValidator{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateCreate(
	_ context.Context, pillarprotocol *pillarcsiv1alpha1.PillarProtocol,
) (admission.Warnings, error) {
	pillarprotocollog.Info("Validation for PillarProtocol upon creation", "name", pillarprotocol.GetName())

	return nil, validateProtocolSpec(pillarprotocol.Spec.Protocol)
}

// protocolMember returns the name of the union member set in a protocol spec
// ("nvmeofTcp"), or "" when none is set.
func protocolMember(p pillarcsiv1alpha1.ProtocolSpec) string {
	if p.NVMeOFTCP != nil {
		return "nvmeofTcp"
	}
	return ""
}

// validateProtocolSpec re-checks spec.protocol with the same union rule and
// numeric domains as the CRD schema and the shared configdocs decoder, so the
// webhook never admits a protocol the agent or node would later reject.
func validateProtocolSpec(p pillarcsiv1alpha1.ProtocolSpec) error {
	protocolPath := field.NewPath("spec", "protocol")
	if protocolMember(p) == "" {
		return field.ErrorList{field.Required(protocolPath,
			"exactly one protocol member must be set (supported: nvmeofTcp)")}.ToAggregate()
	}

	var allErrs field.ErrorList
	cfg := p.NVMeOFTCP
	nvmePath := protocolPath.Child("nvmeofTcp")
	checkRange := func(name string, v int32, minimum, maximum int64) {
		if int64(v) < minimum || int64(v) > maximum {
			allErrs = append(allErrs, field.Invalid(nvmePath.Child(name), v,
				fmt.Sprintf("must be between %d and %d", minimum, maximum)))
		}
	}
	checkOptional := func(name string, v *int32, minimum, maximum int64) {
		if v != nil {
			checkRange(name, *v, minimum, maximum)
		}
	}
	checkRange("port", cfg.Port, 1, 65535)
	checkOptional("maxQueueSize", cfg.MaxQueueSize, 16, 1024)
	checkOptional("inCapsuleDataSize", cfg.InCapsuleDataSize, 1024, math.MaxInt32)
	checkOptional("ctrlLossTmo", cfg.CtrlLossTmo, 0, math.MaxInt32)
	checkOptional("reconnectDelay", cfg.ReconnectDelay, 0, math.MaxInt32)

	if len(allErrs) > 0 {
		return allErrs.ToAggregate()
	}
	return nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateUpdate(
	_ context.Context, oldProtocol, newProtocol *pillarcsiv1alpha1.PillarProtocol,
) (admission.Warnings, error) {
	pillarprotocollog.Info("Validation for PillarProtocol upon update", "name", newProtocol.GetName())

	// The protocol member is immutable: each protocol requires a distinct kernel subsystem
	// and configfs namespace. Switching members would orphan every volume exported via
	// the original protocol without any migration path.
	oldMember := protocolMember(oldProtocol.Spec.Protocol)
	newMember := protocolMember(newProtocol.Spec.Protocol)
	if oldMember != newMember {
		return nil, field.ErrorList{field.Forbidden(
			field.NewPath("spec", "protocol"),
			fmt.Sprintf("the protocol member is immutable; old member %q cannot be changed to %q",
				oldMember, newMember),
		)}.ToAggregate()
	}

	return nil, validateProtocolSpec(newProtocol.Spec.Protocol)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type PillarProtocol.
func (*PillarProtocolCustomValidator) ValidateDelete(
	_ context.Context, pillarprotocol *pillarcsiv1alpha1.PillarProtocol,
) (admission.Warnings, error) {
	pillarprotocollog.Info("Validation for PillarProtocol upon deletion", "name", pillarprotocol.GetName())

	return nil, nil
}
