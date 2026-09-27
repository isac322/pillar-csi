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

// Abandoned provisioning attempts.
//
// CreateVolume creates the PillarVolumeState before any agent call, so a
// failed attempt leaves one behind.  While the claim exists the provisioner
// retries CreateVolume with the same name and the record makes that retry
// idempotent.  Once the claim is deleted before any PersistentVolume was
// created, the provisioner stops retrying and, having no PersistentVolume,
// never calls DeleteVolume: nothing else ends that lifecycle.
//
// The phase does not tell whether the storage node holds anything: the agent
// may have created the backend resource (and even the export) of an attempt
// whose later CRD write or response was lost.  An abandoned lifecycle is
// therefore always ended by the same fenced teardown as DeleteVolume
// (UnexportVolume and DeleteVolume, both idempotent) before its record is
// removed, never by deleting the record alone.

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// provisionerVolumePrefix is external-provisioner's default volume name
// prefix; the volume, the PersistentVolume and the PillarVolumeState are named
// "<prefix>-<claim UID>".
const provisionerVolumePrefix = "pvc-"

// claimRefFromParams returns the claim identity external-provisioner passes
// with --extra-create-metadata, or nil when the UID is absent: only the UID
// pins one claim.
func claimRefFromParams(params map[string]string) *v1alpha1.VolumeClaimRef {
	uid := params[paramPVCUIDMeta]
	if uid == "" {
		return nil
	}
	return &v1alpha1.VolumeClaimRef{
		UID:       uid,
		Namespace: params[paramPVCNamespaceMeta],
		Name:      params[paramPVCNameMeta],
	}
}

// volumeClaimUID returns the UID of the claim pvs was provisioned for: the
// recorded claimRef, else the UID in a default "pvc-<claim UID>" name.  It
// returns "" when the claim cannot be identified; such a volume is never
// treated as abandoned.
func volumeClaimUID(pvs *v1alpha1.PillarVolumeState) types.UID {
	if ref := pvs.Spec.ClaimRef; ref != nil && ref.UID != "" {
		return types.UID(ref.UID)
	}
	suffix, ok := strings.CutPrefix(pvs.Name, provisionerVolumePrefix)
	if !ok {
		return ""
	}
	_, err := uuid.Parse(suffix)
	if err != nil {
		return ""
	}
	return types.UID(suffix)
}

// ReapAbandonedVolume ends the lifecycle of the named PillarVolumeState when
// its provisioning was abandoned: the volume is attributed to a claim, that
// claim no longer exists (or is terminating), and no PersistentVolume refers
// to the volume.  It reports whether the lifecycle was ended.
//
// Every other volume is left alone and (false, nil) is returned: a volume
// with a PersistentVolume is deleted through CSI DeleteVolume when that
// PersistentVolume is released, a live claim may still retry CreateVolume,
// and a volume whose claim cannot be identified cannot be proven abandoned.
//
// The teardown runs under the volume lock and commits status.deleting with a
// fresh fencing generation first, so a CreateVolume retry still in flight is
// either refused (it re-reads deleting) or rejected by the agent as stale.  A
// failure keeps the record marked deleting and is returned so the caller
// retries; the retry repeats the idempotent steps with the same token.  A
// volume still recorded as published is refused (FailedPrecondition) and kept.
func (s *ControllerServer) ReapAbandonedVolume(ctx context.Context, pvsName string) (bool, error) {
	pvs, found, err := s.readVolumeState(ctx, pvsName)
	if err != nil || !found {
		return false, err
	}
	volumeID := pvs.Spec.VolumeID
	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// Decide on the current record: another holder of the lock may have
	// deleted or re-created it.
	pvs, found, err = s.readVolumeState(ctx, pvsName)
	if err != nil || !found {
		return false, err
	}
	abandoned, err := s.provisioningAbandoned(ctx, pvs)
	if err != nil || !abandoned {
		return false, err
	}

	pvs, fence, err := s.markVolumeDeleting(ctx, pvsName, volumeID)
	if err != nil {
		return false, err
	}
	if pvs == nil {
		return false, nil
	}

	err = s.teardownMarkedVolume(ctx, volumeTeardown{
		volumeID:     volumeID,
		pvName:       pvsName,
		uid:          pvs.UID,
		targetName:   pvs.Spec.AgentRef,
		protocolType: mapProtocolType(pvs.Spec.ProtocolType),
		backendType:  mapBackendType(pvs.Spec.BackendType),
		agentVolID:   pvs.Spec.AgentVolumeID,
		fence:        fence,
	})
	if err != nil {
		return false, fmt.Errorf("tear down abandoned volume %q: %w", volumeID, err)
	}
	return true, nil
}

// provisioningAbandoned reports whether pvs belongs to a claim that no longer
// exists and no PersistentVolume refers to it.  It reads uncached through
// apiReader: a stale informer cache could miss a just-created
// PersistentVolume and tear down a volume in use.
func (s *ControllerServer) provisioningAbandoned(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
) (bool, error) {
	claimUID := volumeClaimUID(pvs)
	if claimUID == "" {
		return false, nil
	}

	hasPV, err := s.persistentVolumeExists(ctx, pvs)
	if err != nil || hasPV {
		return false, err
	}

	claims := &corev1.PersistentVolumeClaimList{}
	err = s.apiReader.List(ctx, claims)
	if err != nil {
		return false, fmt.Errorf("list PersistentVolumeClaims for volume %q: %w", pvs.Spec.VolumeID, err)
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		// A terminating claim is never provisioned again: the provisioner
		// skips claims being deleted.
		if claim.UID == claimUID && claim.DeletionTimestamp.IsZero() {
			return false, nil
		}
	}
	return true, nil
}

// persistentVolumeExists reports whether a PersistentVolume refers to the
// volume: one named after it (external-provisioner names the
// PersistentVolume after the volume) or any whose CSI volume handle is its
// volume ID (a statically created PersistentVolume).
func (s *ControllerServer) persistentVolumeExists(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
) (bool, error) {
	pv := &corev1.PersistentVolume{}
	err := s.apiReader.Get(ctx, types.NamespacedName{Name: pvs.Name}, pv)
	if err == nil {
		return true, nil
	}
	if ctrlclient.IgnoreNotFound(err) != nil {
		return false, fmt.Errorf("get PersistentVolume %q: %w", pvs.Name, err)
	}

	pvList := &corev1.PersistentVolumeList{}
	err = s.apiReader.List(ctx, pvList)
	if err != nil {
		return false, fmt.Errorf("list PersistentVolumes for volume %q: %w", pvs.Spec.VolumeID, err)
	}
	for i := range pvList.Items {
		src := pvList.Items[i].Spec.CSI
		if src != nil && src.Driver == s.driverName && src.VolumeHandle == pvs.Spec.VolumeID {
			return true, nil
		}
	}
	return false, nil
}
