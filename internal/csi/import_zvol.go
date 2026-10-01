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

// Zvol import (issue #146): a PVC annotated with
// "pillar-csi.bhyoo.com/import-zvol: <pool>/<parent>/<leaf>" asks CreateVolume
// to adopt an existing ZFS zvol — provisioned by another stack such as
// democratic-csi (zfs-generic-iscsi) or openebs zfs-localpv — instead of
// creating a new one.  The adopted zvol keeps its data; everything else about
// the volume is a normal pillar-csi lifecycle (NVMe/TCP export, node attach,
// expand, delete).
//
// The check split is fail-closed at both ends:
//
//   - Controller: annotation well-formed, backend is ZFS zvol, dataset pool
//     and parent match the store, leaf is a legal ZFS name, and the backend
//     volume ID ("<pool>/<leaf>") is not already owned by another
//     PillarVolumeState.
//   - Agent (ImportVolume RPC): the dataset exists, is a zvol, is large
//     enough, is not exported (LIO backstore / nvmet namespace), not mounted,
//     not dm-held, and not exclusively opened — i.e. the previous
//     provisioning stack was fully removed.
//
// A volume whose backend ID is "pool/<leaf>" (the imported zvol) is also found
// by the same scans, so every RPC that resolves the PillarVolumeState from a
// CSI volume ID — delete, publish, unpublish, expand — keeps working even
// though the leaf differs from the PillarVolumeState name.

import (
	"context"
	"fmt"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"

	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// importInvalid wraps an annotation/layout problem as InvalidArgument (the
// claim itself is malformed or contradicts the configuration); conflicts
// with live state use FailedPrecondition so the claim can be retried after
// the environment changes.
func importInvalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// resolveImportRequest decides whether this CreateVolume adopts an existing
// zvol and, when it does, returns the recorded source dataset and the leaf
// name the agent volume ID is built from.
//
// The annotation argument carries the claim's pillar-csi.bhyoo.com/import-zvol
// value ("" when absent).  For a retry of an existing lifecycle the recorded
// spec wins:
//   - Spec.ImportedFrom set, annotation equal or absent → keep importing;
//   - Spec.ImportedFrom set, annotation different → InvalidArgument;
//   - Spec.ImportedFrom unset, annotation present → InvalidArgument: a
//     volume is never re-interpreted as an import mid-lifecycle; the claim
//     must be re-created.
func (s *ControllerServer) resolveImportRequest(
	ctx context.Context,
	pvName string,
	pvExists bool,
	existingPV *v1alpha1.PillarVolumeState,
	resolved *v1alpha1.ResolvedVolumeConfig,
	annotation string,
	agentRef string,
) (importedFrom, leaf string, err error) {
	annotation = strings.TrimSpace(annotation)
	recorded := ""
	if pvExists {
		recorded = existingPV.Spec.ImportedFrom
	}
	switch {
	case recorded != "":
		if annotation != "" && annotation != recorded {
			return "", "", importInvalid(
				"%s: volume %q already imported %q; the import source cannot be changed",
				v1alpha1.AnnotationImportZvol, pvName, recorded)
		}
		leaf = existingPV.Spec.AgentVolumeID
		if idx := strings.LastIndex(leaf, "/"); idx >= 0 {
			leaf = leaf[idx+1:]
		}
		if leaf == "" {
			return "", "", status.Errorf(codes.Internal,
				"PillarVolumeState %q records import from %q but has no agentVolumeID",
				pvName, recorded)
		}
		return recorded, leaf, nil
	case annotation == "":
		return "", "", nil
	case pvExists:
		return "", "", importInvalid(
			"%s: volume %q was already provisioned without an import; "+
				"delete the PersistentVolumeClaim and re-create it to import a zvol",
			v1alpha1.AnnotationImportZvol, pvName)
	default:
		resolvedLeaf, resolveErr := resolveImportDataset(resolved, annotation)
		if resolveErr != nil {
			return "", "", resolveErr
		}
		leaf = resolvedLeaf
		agentVolID := resolved.Backend.PoolName() + "/" + leaf
		conflictErr := s.refuseImportConflict(ctx, pvName, agentRef, agentVolID, annotation)
		if conflictErr != nil {
			return "", "", conflictErr
		}
		return annotation, leaf, nil
	}
}

// importParts holds the split components of an import-zvol dataset name.
type importParts struct {
	pool   string
	parent string
	leaf   string
}

// parseImportZvol splits a "<pool>/<parent-dataset>/<leaf>" or
// "<pool>/<leaf>" annotation value into its parts, validating every component
// as a ZFS dataset name component.  The leaf may never contain "/"; the
// parent may be empty (a zvol directly under the pool) when the store
// declares no parentDataset.  The value must already be a canonical ZFS
// name: empty components ("a//b", a leading or trailing "/") are refused
// rather than collapsed, so the annotation names exactly one dataset.
func parseImportZvol(dataset string) (importParts, error) {
	var parts importParts
	dataset = strings.TrimSpace(dataset)
	if dataset == "" {
		return parts, importInvalid(
			"%s must name a ZFS dataset as \"<pool>/<dataset>\", got empty value",
			v1alpha1.AnnotationImportZvol)
	}
	first := strings.IndexByte(dataset, '/')
	last := strings.LastIndexByte(dataset, '/')
	if first <= 0 || last == len(dataset)-1 {
		return parts, importInvalid(
			"%s must name a ZFS dataset as \"<pool>[/<parent>]/<name>\", got %q",
			v1alpha1.AnnotationImportZvol, dataset)
	}
	parts.pool, parts.leaf = dataset[:first], dataset[last+1:]
	if first != last {
		parts.parent = dataset[first+1 : last]
	}
	poolErr := validateZFSComponent(parts.pool)
	if poolErr != nil {
		return importParts{}, importInvalid(
			"%s dataset %q: pool name: %v", v1alpha1.AnnotationImportZvol, dataset, poolErr)
	}
	comps := []string{parts.leaf}
	if first != last {
		comps = append(strings.Split(parts.parent, "/"), parts.leaf)
	}
	for _, comp := range comps {
		compErr := validateZFSComponent(comp)
		if compErr != nil {
			return importParts{}, importInvalid(
				"%s dataset %q: %v", v1alpha1.AnnotationImportZvol, dataset, compErr)
		}
	}
	return parts, nil
}

// validateZFSComponent accepts the portable subset of ZFS dataset name
// characters — ASCII letters, digits, and - _ . : — and rejects ".", "..",
// empty components, and anything containing "/", "@" (snapshot) or "#"
// (bookmark).
func validateZFSComponent(comp string) error {
	if comp == "" {
		return fmt.Errorf("empty dataset component")
	}
	if comp == "." || comp == ".." {
		return fmt.Errorf("dataset component %q is not a name", comp)
	}
	for _, r := range comp {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return fmt.Errorf("dataset component %q contains invalid character %q", comp, r)
	}
	return nil
}

// normalizeParentDataset canonicalises the store's parentDataset so that
// "k8s", "k8s/" and "/k8s" compare equal, as the agent's path.Join does.
func normalizeParentDataset(s string) string {
	c := path.Clean(strings.Trim(s, "/"))
	if c == "." {
		return ""
	}
	return c
}

// resolveImportDataset validates the annotation against the resolved backend
// and returns the leaf name that becomes the agent volume name.  The resolved
// backend is authoritative (a retry replays the recorded configuration), so a
// store whose spec changed mid-provisioning still resolves consistently.
func resolveImportDataset(
	resolved *v1alpha1.ResolvedVolumeConfig,
	dataset string,
) (leaf string, err error) {
	zfs := resolved.Backend.ZFS
	if zfs == nil || resolved.Backend.Kind() != v1alpha1.BackendIDZFSZvol {
		return "", importInvalid(
			"%s requires a zfs.zvol PillarStore backend, got backend %q; "+
				"other backends cannot adopt existing volumes",
			v1alpha1.AnnotationImportZvol, resolved.Backend.Kind())
	}
	parts, parseErr := parseImportZvol(dataset)
	if parseErr != nil {
		return "", parseErr
	}
	if parts.pool != zfs.Pool {
		return "", importInvalid(
			"%s dataset %q lives in pool %q but the PillarStore selects pool %q",
			v1alpha1.AnnotationImportZvol, dataset, parts.pool, zfs.Pool)
	}
	if parts.parent != normalizeParentDataset(zfs.ParentDataset) {
		return "", importInvalid(
			"%s dataset %q is not under the PillarStore's parent dataset %q",
			v1alpha1.AnnotationImportZvol, dataset, zfs.ParentDataset)
	}
	return parts.leaf, nil
}

// volumeStateNameForID resolves the PillarVolumeState name that owns the
// backend volume encoded in a CSI volume ID.  For ordinary volumes the leaf
// is the name itself; for an imported volume the leaf is the imported zvol's
// dataset name, so the owner is found by its spec.agentVolumeID.
// Returns "" when the volume ID does not parse or no PillarVolumeState owns
// it; callers treat that as "this driver never issued the ID" (NotFound for
// read paths, success for idempotent deletes).  A state owns the volume when
// its spec.volumeID equals the CSI volume ID (this also covers states written
// by older versions without spec.agentVolumeID) or, scoped to the same
// agent, its non-empty spec.agentVolumeID equals the ID's backend volume:
// agent volume IDs are only unique per storage node, so a same-named backend
// volume on another agent must never be matched.  A PillarVolumeState named
// after the leaf matching neither is never returned: it owns a different
// backend volume.
func (s *ControllerServer) volumeStateNameForID(ctx context.Context, volumeID string) (string, error) {
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return "", nil
	}
	agentName := parts[0]
	agentVolID := parts[3]
	leaf := agentVolID
	if idx := strings.LastIndex(agentVolID, "/"); idx >= 0 {
		leaf = agentVolID[idx+1:]
	}
	if leaf == "" {
		return "", nil
	}
	if s.k8sClient == nil {
		return leaf, nil
	}
	// Fast path: the PillarVolumeState name equals the leaf.  A single Get
	// avoids a list for every non-imported volume.
	probe, exists, err := s.readVolumeState(ctx, leaf)
	if err != nil {
		return "", status.Errorf(codes.Internal, "lookup PillarVolumeState %q: %v", leaf, err)
	}
	if exists && ownsVolume(probe, volumeID, agentName, agentVolID) {
		return leaf, nil
	}
	// Imported (or otherwise renamed) volume: find the owner by its IDs.
	// The list runs on the uncached apiReader like readVolumeState: an
	// informer-cache copy could miss a PillarVolumeState created moments
	// ago on another controller replica and wrongly report no owner.
	var list v1alpha1.PillarVolumeStateList
	listErr := s.uncachedReader().List(ctx, &list)
	if listErr != nil {
		return "", status.Errorf(codes.Internal, "list PillarVolumeStates: %v", listErr)
	}
	for i := range list.Items {
		if ownsVolume(&list.Items[i], volumeID, agentName, agentVolID) {
			return list.Items[i].Name, nil
		}
	}
	return "", nil // no lifecycle owns this backend volume
}

// uncachedReader returns the reader that bypasses any informer cache; it
// falls back to the write client when no dedicated reader was injected
// (unit tests), mirroring NewControllerServerWithDialer's default.
func (s *ControllerServer) uncachedReader() ctrlclient.Reader {
	if s.apiReader != nil {
		return s.apiReader
	}
	return s.k8sClient
}

// ownsVolume reports whether pvs is the lifecycle record of the CSI volume
// volumeID whose backend volume is agentVolID on the agent agentName.  The
// exact spec.volumeID match also owns the volume for states written before
// spec.agentVolumeID existed; the agentVolumeID match is scoped to the same
// agent because backend volume IDs are only unique per storage node.
func ownsVolume(pvs *v1alpha1.PillarVolumeState, volumeID, agentName, agentVolID string) bool {
	return pvs.Spec.VolumeID == volumeID ||
		(pvs.Spec.AgentVolumeID != "" && pvs.Spec.AgentVolumeID == agentVolID &&
			pvs.Spec.AgentRef == agentName)
}

// mustVolumeState resolves the owning PillarVolumeState for a CSI volume ID
// and returns it, or a NotFound error when no state owns the volume.  It is
// the read path every post-create RPC uses to find its lifecycle record.
func (s *ControllerServer) mustVolumeState(
	ctx context.Context,
	volumeID string,
) (pvName string, pvs *v1alpha1.PillarVolumeState, err error) {
	pvName, nameErr := s.volumeStateNameForID(ctx, volumeID)
	if nameErr != nil {
		return "", nil, nameErr
	}
	if pvName == "" {
		return "", nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	pvs, exists, readErr := s.readVolumeState(ctx, pvName)
	if readErr != nil {
		return "", nil, status.Errorf(codes.Internal, "%v", readErr)
	}
	if !exists {
		return "", nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	return pvName, pvs, nil
}

// refuseImportConflict refuses the import when the backend volume ID
// "<pool>/<leaf>" the imported zvol maps to is already owned by a different
// PillarVolumeState on the same agent.  Two lifecycles must never manage one
// backend device: either side could then export or delete what the other one
// serves.  Ownership is scoped to the agent (backend volume IDs are only
// unique per storage node) and the scan reads the API server directly, so a
// state committed moments ago on another replica is still seen — the atomic
// PillarVolumeReservation then closes the remaining create-create race.
func (s *ControllerServer) refuseImportConflict(
	ctx context.Context,
	pvName, agentName, agentVolID, dataset string,
) error {
	var list v1alpha1.PillarVolumeStateList
	listErr := s.uncachedReader().List(ctx, &list)
	if listErr != nil {
		return status.Errorf(codes.Internal, "list PillarVolumeStates: %v", listErr)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name != pvName && item.Spec.AgentRef == agentName &&
			item.Spec.AgentVolumeID == agentVolID {
			return status.Errorf(codes.FailedPrecondition,
				"%s: zvol %q is already managed by volume %q (PillarVolumeState %q); "+
					"delete that volume first",
				v1alpha1.AnnotationImportZvol, dataset, item.Spec.VolumeID, item.Name)
		}
	}
	return nil
}

// refuseAdoptionCollision refuses a normal create when its backend volume ID
// is already owned by a different PillarVolumeState on the same agent — which
// can only happen when that state adopted the zvol via an import annotation
// whose leaf is this create's volume name.  Without the check the create
// would silently adopt the imported zvol and both lifecycles would manage
// one device.
func (s *ControllerServer) refuseAdoptionCollision(
	ctx context.Context,
	pvName, agentName, agentVolID string,
) error {
	var list v1alpha1.PillarVolumeStateList
	listErr := s.uncachedReader().List(ctx, &list)
	if listErr != nil {
		return status.Errorf(codes.Internal, "list PillarVolumeStates: %v", listErr)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name != pvName && item.Spec.AgentRef == agentName &&
			item.Spec.AgentVolumeID == agentVolID {
			return status.Errorf(codes.FailedPrecondition,
				"backend volume %q is already managed by volume %q (PillarVolumeState %q, "+
					"imported from %q); choose a different claim name or delete that volume first",
				agentVolID, item.Spec.VolumeID, item.Name, item.Spec.ImportedFrom)
		}
	}
	return nil
}

// importBackend adopts the existing zvol via agent.ImportVolume under the
// same fencing/partial-state discipline as createBackend (see
// commitBackendVolume).  It returns the adopted device path and its existing
// size.
func (s *ControllerServer) importBackend(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName, volumeID string,
	uid types.UID,
	req *agentv1.ImportVolumeRequest,
	exportSpec *v1alpha1.VolumeExportSpec,
) (devicePath string, capacity int64, err error) {
	return s.commitBackendVolume(ctx, pvName, volumeID, uid, exportSpec,
		func(fence *agentv1.FencingToken) (string, int64, error) {
			req.Fence = fence
			resp, callErr := agentClient.ImportVolume(ctx, req)
			if callErr != nil {
				grpcSt, _ := status.FromError(callErr)
				return "", 0, status.Errorf(grpcSt.Code(),
					"agent ImportVolume(%q) failed: %v", req.GetVolumeId(), callErr)
			}
			capacity = req.GetCapacityBytes()
			if allocated := resp.GetCapacityBytes(); allocated != 0 {
				capacity = allocated
			}
			return resp.GetDevicePath(), capacity, nil
		})
}
