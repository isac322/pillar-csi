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

package csi

// LV import (issue #163): a PVC annotated with
// "pillar-csi.bhyoo.com/import-lv: <vg>/<lv>:<vg_uuid>:<lv_uuid>" asks
// CreateVolume to adopt an existing LVM logical volume instead of creating a
// new one.  The optional "pillar-csi.bhyoo.com/import-lv-policy" selects
// PreserveOriginal (the default) or Managed.
//
// The adopted source is pinned in spec.lvmSource before the first agent
// call and is authoritative for every retry: an annotation that later names
// another LV, other UUIDs or another policy is refused, never re-targeted.
// The reservation is keyed by the LV UUID (see reservationKey), so a renamed
// locator cannot open a second lifecycle on the same LV.
//
// PreserveOriginal lifecycles are never destroyed or resized: DeleteVolume
// and abandoned-attempt cleanup end them with ReleaseVolume only, expand is
// refused before any agent call, and the node is told through the
// VolumeContext to mount the existing filesystem without formatting,
// checking or resizing it.  Managed adoptions become normal volumes.

import (
	"context"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend/lvm"
)

// lvmUUIDRe is the LVM UUID text form (`lvs -o lv_uuid`): 32 alphanumerics
// grouped 6-4-4-4-4-4-6.  It equals the CRD validation pattern of
// spec.lvmSource.
var lvmUUIDRe = regexp.MustCompile(`^[A-Za-z0-9]{6}(-[A-Za-z0-9]{4}){5}-[A-Za-z0-9]{6}$`)

// lvImportFields is the number of ':'-separated fields of an import-lv value.
const lvImportFields = 3

// parseImportLV parses an import-lv annotation value and its policy into the
// source to pin.  The value must be exactly "<vg>/<lv>:<vg_uuid>:<lv_uuid>":
// both names must be legal LVM names (which never contain ':' or '/') and
// both UUIDs must be in LVM's text form.  An empty policy selects
// PreserveOriginal; any value other than the two policy names is refused.
// Every refusal is InvalidArgument.
func parseImportLV(value, policy string) (*v1alpha1.LVMSourceRef, error) {
	fields := strings.Split(value, ":")
	if len(fields) != lvImportFields {
		return nil, importInvalid(
			"%s must name an LVM logical volume as \"<vg>/<lv>:<vg_uuid>:<lv_uuid>\", got %q",
			v1alpha1.AnnotationImportLV, value)
	}
	vg, lv, found := strings.Cut(fields[0], "/")
	if !found || strings.Contains(lv, "/") {
		return nil, importInvalid(
			"%s locator %q must be exactly \"<vg>/<lv>\"", v1alpha1.AnnotationImportLV, fields[0])
	}
	err := lvm.ValidateVGName(vg)
	if err != nil {
		return nil, importInvalid("%s %q: %v", v1alpha1.AnnotationImportLV, value, err)
	}
	err = lvm.ValidateLVName(lv)
	if err != nil {
		return nil, importInvalid("%s %q: %v", v1alpha1.AnnotationImportLV, value, err)
	}
	for _, u := range []struct{ name, v string }{{"vg_uuid", fields[1]}, {"lv_uuid", fields[2]}} {
		if !lvmUUIDRe.MatchString(u.v) {
			return nil, importInvalid(
				"%s %q: %s %q is not an LVM UUID (read it with `lvs -o vg_uuid,lv_uuid`)",
				v1alpha1.AnnotationImportLV, value, u.name, u.v)
		}
	}
	var preserve bool
	switch policy {
	case "", v1alpha1.ImportLVPolicyPreserveOriginal:
		preserve = true
	case v1alpha1.ImportLVPolicyManaged:
		preserve = false
	default:
		return nil, importInvalid("%s must be %q or %q, got %q",
			v1alpha1.AnnotationImportLVPolicy,
			v1alpha1.ImportLVPolicyPreserveOriginal, v1alpha1.ImportLVPolicyManaged, policy)
	}
	return &v1alpha1.LVMSourceRef{
		VolumeGroup:       vg,
		LogicalVolume:     lv,
		VolumeGroupUUID:   fields[1],
		LogicalVolumeUUID: fields[2],
		PreserveOriginal:  preserve,
	}, nil
}

// lvmSourceLocator returns the "<vg>/<lv>" locator of src, which is also the
// agent volume ID of the adopted LV.
func lvmSourceLocator(src *v1alpha1.LVMSourceRef) string {
	return src.VolumeGroup + "/" + src.LogicalVolume
}

// lvmSourcePolicy names the adoption policy of src.
func lvmSourcePolicy(src *v1alpha1.LVMSourceRef) string {
	if src.PreserveOriginal {
		return v1alpha1.ImportLVPolicyPreserveOriginal
	}
	return v1alpha1.ImportLVPolicyManaged
}

// checkRecordedLVSource decides a retry of the lifecycle pvName against its
// recorded spec.lvmSource (nil for a lifecycle that is not an LV adoption).
// The record wins: an absent import-lv annotation keeps it, an annotation
// that differs in any identity field or in the policy is InvalidArgument (a
// source or policy is never re-targeted or downgraded), and an import-lv
// annotation on a lifecycle provisioned without one is InvalidArgument (a
// volume is never re-interpreted as an adoption mid-lifecycle).
func checkRecordedLVSource(pvName string, recorded *v1alpha1.LVMSourceRef, annotation, policy string) error {
	if annotation == "" {
		return nil
	}
	if recorded == nil {
		return importInvalid(
			"%s: volume %q was already provisioned without an LV import; "+
				"delete the PersistentVolumeClaim and re-create it to import an LV",
			v1alpha1.AnnotationImportLV, pvName)
	}
	want, err := parseImportLV(annotation, policy)
	if err != nil {
		return err
	}
	if *want != *recorded {
		return importInvalid(
			"%s: volume %q already adopted LV %s (vg_uuid %s, lv_uuid %s, policy %s); "+
				"the import source and policy cannot be changed",
			v1alpha1.AnnotationImportLV, pvName, lvmSourceLocator(recorded),
			recorded.VolumeGroupUUID, recorded.LogicalVolumeUUID, lvmSourcePolicy(recorded))
	}
	return nil
}

// resolveLVImportRequest decides whether this CreateVolume adopts an
// existing LV and, when it does, returns the source to pin and the agent
// volume ID ("<vg>/<lv>").  It mirrors resolveImportRequest: a retry of an
// existing lifecycle replays its recorded spec.lvmSource (see
// checkRecordedLVSource); a first attempt validates the annotation, requires
// an lvm-lv backend whose volume group is the LV's, and refuses an LV
// another lifecycle on the same agent already manages.  A nil source means a
// normal create.
func (s *ControllerServer) resolveLVImportRequest(
	ctx context.Context,
	pvName string,
	pvExists bool,
	existingPV *v1alpha1.PillarVolumeState,
	resolved *v1alpha1.ResolvedVolumeConfig,
	annotation, policy, agentRef string,
) (src *v1alpha1.LVMSourceRef, agentVolID string, err error) {
	if pvExists {
		recorded := existingPV.Spec.LVMSource
		err = checkRecordedLVSource(pvName, recorded, annotation, policy)
		if err != nil || recorded == nil {
			return nil, "", err
		}
		return recorded, lvmSourceLocator(recorded), nil
	}
	if annotation == "" {
		return nil, "", nil
	}
	src, err = parseImportLV(annotation, policy)
	if err != nil {
		return nil, "", err
	}
	lvmCfg := resolved.Backend.LVM
	if lvmCfg == nil || resolved.Backend.Kind() != v1alpha1.BackendIDLVMLV {
		return nil, "", importInvalid(
			"%s requires an lvm-lv PillarStore backend, got backend %q: "+
				"other backends cannot adopt existing LVs",
			v1alpha1.AnnotationImportLV, resolved.Backend.Kind())
	}
	if src.VolumeGroup != lvmCfg.VolumeGroup {
		return nil, "", importInvalid(
			"%s LV %q lives in volume group %q but the PillarStore selects volume group %q",
			v1alpha1.AnnotationImportLV, lvmSourceLocator(src), src.VolumeGroup, lvmCfg.VolumeGroup)
	}
	agentVolID = lvmSourceLocator(src)
	err = s.refuseLVImportConflict(ctx, pvName, agentRef, agentVolID, src)
	if err != nil {
		return nil, "", err
	}
	return src, agentVolID, nil
}

// refuseLVImportConflict refuses the adoption when another PillarVolumeState
// on the same agent already manages the LV: by its "<vg>/<lv>" agent volume
// ID, or by the LV UUID of a recorded adoption (the LV may have been renamed
// since).  The scan reads the API server directly; the UUID-keyed
// PillarVolumeReservation then closes the create-create race.
func (s *ControllerServer) refuseLVImportConflict(
	ctx context.Context,
	pvName, agentName, agentVolID string,
	src *v1alpha1.LVMSourceRef,
) error {
	var list v1alpha1.PillarVolumeStateList
	listErr := s.uncachedReader().List(ctx, &list)
	if listErr != nil {
		return status.Errorf(codes.Internal, "list PillarVolumeStates: %v", listErr)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name == pvName || item.Spec.AgentRef != agentName {
			continue
		}
		sameUUID := item.Spec.LVMSource != nil &&
			item.Spec.LVMSource.LogicalVolumeUUID == src.LogicalVolumeUUID
		if item.Spec.AgentVolumeID == agentVolID || sameUUID {
			return status.Errorf(codes.FailedPrecondition,
				"%s: LV %q (lv_uuid %s) is already managed by volume %q (PillarVolumeState %q); "+
					"delete that volume first",
				v1alpha1.AnnotationImportLV, agentVolID, src.LogicalVolumeUUID,
				item.Spec.VolumeID, item.Name)
		}
	}
	return nil
}

// lvImportSubject names an LV adoption in reservation refusals.
func lvImportSubject(src *v1alpha1.LVMSourceRef) reservationSubject {
	return reservationSubject{
		annotation: v1alpha1.AnnotationImportLV,
		noun:       "LV",
		source:     lvmSourceLocator(src),
	}
}

// lvImportRequest builds the agent ImportVolume request adopting src: the
// agent verifies the pinned identity and pins the policy in its durable mark.
// The request never names a dataset, so the agent's ZFS path cannot run.
func lvImportRequest(agentVolID string, capacityBytes int64, src *v1alpha1.LVMSourceRef) *agentv1.ImportVolumeRequest {
	return &agentv1.ImportVolumeRequest{
		VolumeId:      agentVolID,
		CapacityBytes: capacityBytes,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_LVM,
		ExpectedLvmSource: &agentv1.LvmSourceIdentity{
			VolumeGroup:       src.VolumeGroup,
			LogicalVolume:     src.LogicalVolume,
			VolumeGroupUuid:   src.VolumeGroupUUID,
			LogicalVolumeUuid: src.LogicalVolumeUUID,
		},
		PreserveOriginal: new(src.PreserveOriginal),
	}
}

// preservesOriginal reports whether pvs adopted an LV under PreserveOriginal.
func preservesOriginal(pvs *v1alpha1.PillarVolumeState) bool {
	return pvs.Spec.LVMSource != nil && pvs.Spec.LVMSource.PreserveOriginal
}

// addPreserveVolumeContext adds VolumeContextKeyPreserveOriginal="true" for a
// preserving adoption, from the authoritative recorded spec: the node then
// mounts the existing filesystem and never formats, fscks or resizes it.
func addPreserveVolumeContext(pvs *v1alpha1.PillarVolumeState, volCtx map[string]string) {
	if preservesOriginal(pvs) {
		volCtx[VolumeContextKeyPreserveOriginal] = annotationValueTrue
	}
}

// refusePreservedExpand refuses ControllerExpandVolume of a PreserveOriginal
// adoption.  It runs before any token write or agent dial: resizing would
// rewrite pre-existing data the policy promises to keep untouched.
func refusePreservedExpand(pvs *v1alpha1.PillarVolumeState, volumeID string) error {
	if !preservesOriginal(pvs) {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition,
		"cannot expand volume %q: it adopted LV %q under policy %s, which never resizes the original",
		volumeID, lvmSourceLocator(pvs.Spec.LVMSource), v1alpha1.ImportLVPolicyPreserveOriginal)
}

// refuseRecordedLVDrift is the import-lv check for a CreateVolume retry
// answered from a Ready record (see CreateVolume): that fast path never
// resolves the configuration, so the claim's import annotations are read
// here.  An LV adoption refuses an import-zvol annotation, and the import-lv
// pair is decided by checkRecordedLVSource.  A claim that cannot be read as
// named (no claim metadata, or the claim is gone) carries no annotation to
// compare.  Lifecycles that are not LV adoptions keep their import-zvol
// behavior exactly as it was.
func (s *ControllerServer) refuseRecordedLVDrift(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
	scParams map[string]string,
) error {
	pvcName, pvcNamespace := scParams[paramPVCNameMeta], scParams[paramPVCNamespaceMeta]
	if pvcName == "" || pvcNamespace == "" {
		return nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	err := s.uncachedReader().Get(ctx, types.NamespacedName{Namespace: pvcNamespace, Name: pvcName}, pvc)
	switch {
	case k8serrors.IsNotFound(err):
		return nil
	case err != nil:
		return status.Errorf(codes.Internal,
			"get PersistentVolumeClaim %s/%s: %v", pvcNamespace, pvcName, err)
	}
	if _, zvol := pvc.Annotations[v1alpha1.AnnotationImportZvol]; zvol && pvs.Spec.LVMSource != nil {
		return importInvalid(
			"%s: volume %q adopted LV %s; the import source cannot be changed",
			v1alpha1.AnnotationImportZvol, pvs.Name, lvmSourceLocator(pvs.Spec.LVMSource))
	}
	return checkRecordedLVSource(pvs.Name, pvs.Spec.LVMSource,
		strings.TrimSpace(pvc.Annotations[v1alpha1.AnnotationImportLV]),
		strings.TrimSpace(pvc.Annotations[v1alpha1.AnnotationImportLVPolicy]))
}
