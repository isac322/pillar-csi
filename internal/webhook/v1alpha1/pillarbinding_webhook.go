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

var pillarbindinglog = logf.Log.WithName("pillarbinding-resource")

// SetupPillarBindingWebhookWithManager registers the webhook for PillarBinding in the manager.
func SetupPillarBindingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &pillarcsiv1alpha1.PillarBinding{}).
		WithValidator(&PillarBindingCustomValidator{Client: mgr.GetClient()}).
		WithDefaulter(&PillarBindingCustomDefaulter{Client: mgr.GetClient()}).
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
	// Client is used to look up referenced PillarPool resources so that
	// allowVolumeExpansion can be derived from the pool's backend type.
	Client client.Client
}

var _ admission.Defaulter[*pillarcsiv1alpha1.PillarBinding] = &PillarBindingCustomDefaulter{}

// Default implements admission.Defaulter so a webhook will be registered for the Kind PillarBinding.
func (d *PillarBindingCustomDefaulter) Default(
	ctx context.Context, pillarbinding *pillarcsiv1alpha1.PillarBinding,
) error {
	pillarbindinglog.Info("Defaulting for PillarBinding", "name", pillarbinding.GetName())

	// Auto-set allowVolumeExpansion from the referenced pool's backend type when
	// the user has not explicitly configured the field.
	if pillarbinding.Spec.StorageClass.AllowVolumeExpansion == nil {
		err := d.defaultAllowVolumeExpansion(ctx, pillarbinding)
		if err != nil {
			// The pool may not exist yet (e.g., created after the binding).
			// Skip silently rather than blocking admission – the controller will
			// reconcile the generated StorageClass once the pool becomes available.
			pillarbindinglog.V(1).Info("Skipping allowVolumeExpansion auto-detection",
				"reason", err.Error(),
				"poolRef", pillarbinding.Spec.PoolRef)
		}
	}

	return nil
}

// defaultAllowVolumeExpansion looks up the referenced PillarPool and writes
// spec.storageClass.allowVolumeExpansion based on the pool's backend type.
func (d *PillarBindingCustomDefaulter) defaultAllowVolumeExpansion(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarBinding,
) error {
	if d.Client == nil {
		return fmt.Errorf("defaulter client is nil, cannot look up PillarPool")
	}
	pool := &pillarcsiv1alpha1.PillarPool{}
	err := d.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.PoolRef}, pool)
	if err != nil {
		return fmt.Errorf("cannot look up PillarPool %q: %w", pb.Spec.PoolRef, err)
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

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customize the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-pillar-csi-pillar-csi-bhyoo-com-v1alpha1-pillarbinding,mutating=false,failurePolicy=fail,sideEffects=None,groups=pillar-csi.pillar-csi.bhyoo.com,resources=pillarbindings,verbs=create;update,versions=v1alpha1,name=vpillarbinding-v1alpha1.kb.io,admissionReviewVersions=v1

// PillarBindingCustomValidator struct is responsible for validating the PillarBinding resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type PillarBindingCustomValidator struct {
	// Client is used to look up referenced PillarPool and PillarProtocol resources in order
	// to verify that the backend type and protocol type are compatible.
	Client client.Client
}

var _ admission.Validator[*pillarcsiv1alpha1.PillarBinding] = &PillarBindingCustomValidator{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type PillarBinding.
func (v *PillarBindingCustomValidator) ValidateCreate(
	ctx context.Context, pillarbinding *pillarcsiv1alpha1.PillarBinding,
) (admission.Warnings, error) {
	pillarbindinglog.Info("Validation for PillarBinding upon creation", "name", pillarbinding.GetName())

	err := v.validateCompatibility(ctx, pillarbinding)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type PillarBinding.
func (v *PillarBindingCustomValidator) ValidateUpdate(
	ctx context.Context, oldBinding, newBinding *pillarcsiv1alpha1.PillarBinding,
) (admission.Warnings, error) {
	pillarbindinglog.Info("Validation for PillarBinding upon update", "name", newBinding.GetName())

	var allErrs field.ErrorList

	// spec.poolRef is immutable: a binding owns a generated StorageClass that is tied to a
	// specific pool.  Changing poolRef mid-flight would silently redirect new PVC provisioning
	// to a different pool while leaving the StorageClass name unchanged, causing confusion.
	if oldBinding.Spec.PoolRef != newBinding.Spec.PoolRef {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "poolRef"),
			fmt.Sprintf("field is immutable; old value %q cannot be changed to %q",
				oldBinding.Spec.PoolRef, newBinding.Spec.PoolRef),
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
	// (poolRef and protocolRef are immutable, so this is only relevant when neither
	// changed — but we still need to guard against a cluster state change between
	// admission calls.)
	compatErr := v.validateCompatibility(ctx, newBinding)
	if compatErr != nil {
		return nil, compatErr
	}

	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type PillarBinding.
func (*PillarBindingCustomValidator) ValidateDelete(
	_ context.Context, pillarbinding *pillarcsiv1alpha1.PillarBinding,
) (admission.Warnings, error) {
	pillarbindinglog.Info("Validation for PillarBinding upon deletion", "name", pillarbinding.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}

// validateCompatibility checks that the backend type of the referenced
// PillarPool and the protocol type of the referenced PillarProtocol are a
// valid combination. If either referenced resource does not yet exist the
// check is skipped: the controller will detect and surface the mismatch via
// status conditions once both resources are available.
func (v *PillarBindingCustomValidator) validateCompatibility(
	ctx context.Context, pb *pillarcsiv1alpha1.PillarBinding,
) error {
	if v.Client == nil {
		return nil
	}

	pool := &pillarcsiv1alpha1.PillarPool{}
	poolErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.PoolRef}, pool)
	if poolErr != nil {
		// Pool not found yet — skip; controller reconciliation handles this case.
		pillarbindinglog.V(1).Info("Skipping compatibility check: cannot fetch pool",
			"poolRef", pb.Spec.PoolRef, "reason", poolErr.Error())
		return nil
	}

	protocol := &pillarcsiv1alpha1.PillarProtocol{}
	protoErr := v.Client.Get(ctx, types.NamespacedName{Name: pb.Spec.ProtocolRef}, protocol)
	if protoErr != nil {
		// Protocol not found yet — skip; controller reconciliation handles this case.
		pillarbindinglog.V(1).Info("Skipping compatibility check: cannot fetch protocol",
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
