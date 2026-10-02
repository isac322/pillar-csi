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

	pillarcsiv1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

var pillarprotocollog = logf.Log.WithName("pillarprotocol-resource")

const nfsProtocolMember = string(pillarcsiv1alpha1.ProtocolIDNFS)

// protocolMember returns the name of the union member set in a protocol spec
// ("nvmeofTcp", "iscsi", or "nfs"), or "" when none is set.
func protocolMember(p pillarcsiv1alpha1.ProtocolSpec) string {
	switch {
	case p.NVMeOFTCP != nil:
		return "nvmeofTcp"
	case p.ISCSI != nil:
		return "iscsi"
	case p.NFS != nil:
		return nfsProtocolMember
	default:
		return ""
	}
}

// protocolMemberCount returns how many union members a protocol spec sets.
func protocolMemberCount(p pillarcsiv1alpha1.ProtocolSpec) int {
	n := 0
	if p.NVMeOFTCP != nil {
		n++
	}
	if p.ISCSI != nil {
		n++
	}
	if p.NFS != nil {
		n++
	}
	return n
}

// validateProtocolSpec re-checks spec.protocol with the same union rule and
// numeric domains as the CRD schema and the shared configdocs decoder, so the
// webhook never admits a protocol the agent or node would later reject.
func validateProtocolSpec(p pillarcsiv1alpha1.ProtocolSpec) error {
	protocolPath := field.NewPath("spec", "protocol")
	err := validateProtocolUnion(protocolPath, p)
	if err != nil {
		return err
	}

	memberPath := protocolPath.Child(protocolMember(p))
	var errs field.ErrorList
	switch {
	case p.NVMeOFTCP != nil:
		errs = validateNVMeOFTCP(memberPath, p.NVMeOFTCP)
	case p.ISCSI != nil:
		errs = validateISCSI(memberPath, p.ISCSI)
	case p.NFS != nil:
		errs = validateNFS(memberPath, p.NFS)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs.ToAggregate()
}

func validateProtocolUnion(protocolPath *field.Path, p pillarcsiv1alpha1.ProtocolSpec) error {
	const unionRule = "exactly one protocol member must be set (supported: nvmeofTcp, iscsi, nfs)"
	switch n := protocolMemberCount(p); {
	case n == 0:
		return field.ErrorList{field.Required(protocolPath, unionRule)}.ToAggregate()
	case n > 1:
		return field.ErrorList{field.Invalid(protocolPath, n, unionRule)}.ToAggregate()
	default:
		return nil
	}
}

func validateNVMeOFTCP(memberPath *field.Path, cfg *pillarcsiv1alpha1.NVMeOFTCPConfig) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs, validateInt32Range(memberPath, "port", cfg.Port, 1, 65535)...)
	errs = append(errs, validateOptionalInt32Range(memberPath, "maxQueueSize", cfg.MaxQueueSize, 16, 1024)...)
	errs = append(errs, validateOptionalInt32Range(
		memberPath, "inCapsuleDataSize", cfg.InCapsuleDataSize, 1024, math.MaxInt32,
	)...)
	if v := cfg.MaxDataTransferSize; v != nil && !pillarcsiv1alpha1.IsValidMaxDataTransferSize(int64(*v)) {
		errs = append(errs, field.Invalid(memberPath.Child("maxDataTransferSize"), *v,
			fmt.Sprintf("must be 0 (no limit) or a power of two from %d to %d",
				pillarcsiv1alpha1.MinMaxDataTransferSize, pillarcsiv1alpha1.MaxMaxDataTransferSize)))
	}
	errs = append(errs, validateOptionalInt32Range(memberPath, "ctrlLossTmo", cfg.CtrlLossTmo, 0, math.MaxInt32)...)
	errs = append(errs, validateOptionalInt32Range(memberPath, "reconnectDelay", cfg.ReconnectDelay, 0, math.MaxInt32)...)
	return errs
}

func validateISCSI(memberPath *field.Path, cfg *pillarcsiv1alpha1.ISCSIConfig) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs, validateInt32Range(memberPath, "port", cfg.Port, 1, 65535)...)
	errs = append(errs, validateOptionalInt32Range(memberPath, "loginTimeout", cfg.LoginTimeout, 1, math.MaxInt32)...)
	errs = append(errs, validateOptionalInt32Range(
		memberPath, "replacementTimeout", cfg.ReplacementTimeout, 0, math.MaxInt32,
	)...)
	errs = append(errs, validateOptionalInt32Range(
		memberPath, "noopOutInterval", cfg.NoopOutInterval, 0, math.MaxInt32,
	)...)
	errs = append(errs, validateOptionalInt32Range(memberPath, "noopOutTimeout", cfg.NoopOutTimeout, 0, math.MaxInt32)...)
	return append(errs, validateISCSIAuth(memberPath, cfg)...)
}

func validateNFS(memberPath *field.Path, cfg *pillarcsiv1alpha1.NFSConfig) field.ErrorList {
	var errs field.ErrorList
	if cfg.Version != "" && cfg.Version != "4.2" {
		errs = append(errs, field.NotSupported(memberPath.Child("version"), cfg.Version, []string{"4.2"}))
	}
	if cfg.Port != 0 && cfg.Port != 2049 {
		errs = append(errs, field.Invalid(memberPath.Child("port"), cfg.Port, "must be 2049"))
	}
	switch cfg.Squash {
	case "", pillarcsiv1alpha1.NFSSquashRoot, pillarcsiv1alpha1.NFSSquashNone, pillarcsiv1alpha1.NFSSquashAll:
	default:
		errs = append(errs, field.NotSupported(memberPath.Child("squash"), cfg.Squash,
			[]string{
				string(pillarcsiv1alpha1.NFSSquashRoot),
				string(pillarcsiv1alpha1.NFSSquashNone),
				string(pillarcsiv1alpha1.NFSSquashAll),
			}))
	}
	return errs
}

func validateInt32Range(memberPath *field.Path, name string, value int32, minimum, maximum int64) field.ErrorList {
	if int64(value) < minimum || int64(value) > maximum {
		return field.ErrorList{field.Invalid(memberPath.Child(name), value,
			fmt.Sprintf("must be between %d and %d", minimum, maximum))}
	}
	return nil
}

func validateOptionalInt32Range(
	memberPath *field.Path, name string, value *int32, minimum, maximum int64,
) field.ErrorList {
	if value == nil {
		return nil
	}
	return validateInt32Range(memberPath, name, *value, minimum, maximum)
}

// validateISCSIAuth mirrors the ISCSIConfig auth CEL rules: CHAP and
// MutualCHAP name their Secret and run with ACLs, because the credentials
// are set on the per-initiator ACLs.
func validateISCSIAuth(memberPath *field.Path, cfg *pillarcsiv1alpha1.ISCSIConfig) field.ErrorList {
	if cfg.Auth == nil {
		return nil
	}
	authPath := memberPath.Child("auth")
	switch cfg.Auth.Method {
	case "", pillarcsiv1alpha1.ISCSIAuthMethodNone:
		return nil
	case pillarcsiv1alpha1.ISCSIAuthMethodCHAP, pillarcsiv1alpha1.ISCSIAuthMethodMutualCHAP:
	default:
		return field.ErrorList{field.NotSupported(authPath.Child("method"), cfg.Auth.Method, []string{
			string(pillarcsiv1alpha1.ISCSIAuthMethodNone),
			string(pillarcsiv1alpha1.ISCSIAuthMethodCHAP),
			string(pillarcsiv1alpha1.ISCSIAuthMethodMutualCHAP),
		})}
	}
	var errs field.ErrorList
	if cfg.Auth.SecretRef == nil || cfg.Auth.SecretRef.Name == "" {
		errs = append(errs, field.Required(authPath.Child("secretRef"),
			"auth.secretRef is required when auth.method is CHAP or MutualCHAP"))
	}
	if !cfg.ACL {
		errs = append(errs, field.Invalid(memberPath.Child("acl"), cfg.ACL,
			"auth.method CHAP and MutualCHAP require acl: true"))
	}
	return errs
}

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
