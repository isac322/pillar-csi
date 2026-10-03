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

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

func (s *ControllerServer) effectiveDriverName() string {
	if s.driverName == "" {
		return v1alpha1.DefaultCSIDriver
	}
	return s.driverName
}

// The descriptor, not a separately mutable driver label, scopes a lifecycle.
func scopedDriverForVolume(pvs *v1alpha1.PillarVolumeState) string {
	if pvs != nil && pvs.Spec.FilesystemAdoption != nil {
		return v1alpha1.FileCSIDriver
	}
	return v1alpha1.DefaultCSIDriver
}

func (s *ControllerServer) validateVolumeDriver(pvs *v1alpha1.PillarVolumeState) error {
	if scopedDriverForVolume(pvs) != s.effectiveDriverName() {
		return status.Errorf(codes.NotFound, "volume belongs to CSI driver %q, not %q",
			scopedDriverForVolume(pvs), s.effectiveDriverName())
	}
	return nil
}

func filesystemAdoptionProto(a *v1alpha1.FilesystemAdoption) (*agentv1.FilesystemAdoption, error) {
	if a == nil {
		return nil, fmt.Errorf("filesystem adoption descriptor is required")
	}
	inode, err := v1alpha1.ParseFilesystemAdoptionInode(a.Inode)
	if err != nil && a.Inode != "" {
		return nil, fmt.Errorf("parse filesystem adoption inode: %w", err)
	}
	return &agentv1.FilesystemAdoption{
		Kind: string(a.Kind), CanonicalSource: a.CanonicalSource, ResourceId: a.ResourceID,
		HostPath: a.HostPath, FilesystemType: a.FilesystemType, FilesystemId: a.FilesystemID,
		Inode: inode, ProjectId: a.ProjectID,
	}, nil
}

func filesystemResourceID(a *v1alpha1.FilesystemAdoption) (string, error) {
	proto, err := filesystemAdoptionProto(a)
	if err != nil {
		return "", err
	}
	return backend.FilesystemFenceID(proto), nil
}

func filesystemImportLeaf(a *v1alpha1.FilesystemAdoption) (string, error) {
	resourceID, err := filesystemResourceID(a)
	if err != nil {
		return "", err
	}
	return "fs-" + strings.TrimPrefix(resourceID, "filesystem/"), nil
}

// Canonical identity must be validated before using it for a reservation or
// ownership fence. Unknown and partial descriptors never fall back to zvol IDs.
func validateFilesystemAdoption(a *v1alpha1.FilesystemAdoption, resolved *v1alpha1.ResolvedVolumeConfig) error {
	if a == nil || resolved == nil || a.ResourceID == "" || a.CanonicalSource == "" {
		return fmt.Errorf("missing filesystem adoption identity or resolved layout")
	}
	switch a.Kind {
	case v1alpha1.FilesystemAdoptionKindDirectory:
		return validateDirectoryFilesystemAdoption(a, resolved)
	case v1alpha1.FilesystemAdoptionKindZFSDataset:
		return validateZFSDatasetAdoption(a, resolved)
	default:
		return fmt.Errorf("unknown filesystem adoption kind %q", a.Kind)
	}
}

func validateDirectoryFilesystemAdoption(
	a *v1alpha1.FilesystemAdoption,
	resolved *v1alpha1.ResolvedVolumeConfig,
) error {
	d := resolved.Backend.Directory
	if d == nil || resolved.Backend.Kind() != v1alpha1.BackendIDDirectory || d.LogicalPool == "" {
		return fmt.Errorf("directory adoption requires a directory backend with a logical pool")
	}
	if !canonicalHostPath(d.HostRoot) || !canonicalHostPath(a.CanonicalSource) ||
		a.CanonicalSource == d.HostRoot ||
		!strings.HasPrefix(a.CanonicalSource, strings.TrimSuffix(d.HostRoot, "/")+"/") {
		return fmt.Errorf(
			"directory source %q must be a canonical directory strictly below hostRoot %q",
			a.CanonicalSource, d.HostRoot)
	}
	inode, err := v1alpha1.ParseFilesystemAdoptionInode(a.Inode)
	if err != nil {
		return fmt.Errorf(
			"directory descriptor must contain matching native filesystem UUID, inode and project identity")
	}
	if !directoryFilesystemIdentityMatches(a, inode) {
		return fmt.Errorf(
			"directory descriptor must contain matching native filesystem UUID, inode and project identity")
	}
	return nil
}

func directoryFilesystemIdentityMatches(a *v1alpha1.FilesystemAdoption, inode uint64) bool {
	return a.HostPath == "" && (a.FilesystemType == defaultFsType || a.FilesystemType == xfsFsType) &&
		canonicalFilesystemUUID(a.FilesystemID) && inode != 0 && a.ProjectID != 0 &&
		a.ResourceID == a.FilesystemID+":"+strconv.FormatUint(inode, 10)
}

func validateZFSDatasetAdoption(
	a *v1alpha1.FilesystemAdoption,
	resolved *v1alpha1.ResolvedVolumeConfig,
) error {
	z := resolved.Backend.ZFS
	if z == nil || resolved.Backend.Kind() != v1alpha1.BackendIDZFSDataset {
		return fmt.Errorf("dataset adoption requires a zfs.dataset backend")
	}
	parts, err := parseImportZvol(a.CanonicalSource)
	if err != nil || parts.pool != z.Pool ||
		parts.parent != normalizeParentDataset(z.ParentDataset) {
		return fmt.Errorf(
			"dataset source %q does not match configured pool and parent dataset",
			a.CanonicalSource)
	}
	guid, err := strconv.ParseUint(a.ResourceID, 10, 64)
	if err != nil || guid == 0 || strconv.FormatUint(guid, 10) != a.ResourceID ||
		a.FilesystemType != zfsFsType || (a.HostPath != "" && !canonicalHostPath(a.HostPath)) ||
		a.FilesystemID != "" || a.Inode != "" || a.ProjectID != 0 {
		return fmt.Errorf(
			"dataset descriptor must contain canonical native GUID, optional canonical host path " +
				"and no directory identity")
	}
	return nil
}

func canonicalHostPath(value string) bool {
	return strings.HasPrefix(value, "/") && path.Clean(value) == value && !strings.ContainsRune(value, '\x00')
}

func canonicalFilesystemUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	nonzero := false
	for i := range value {
		c := value[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
		nonzero = nonzero || c != '0'
	}
	return nonzero
}

// Inspect uses only the configured agent and stored layout; a response cannot
// redirect routing to another pool, backend, agent or filesystem identity.
func (s *ControllerServer) inspectFilesystemImport(ctx context.Context, agentRef, source string,
	resolved *v1alpha1.ResolvedVolumeConfig, required int64) (*v1alpha1.FilesystemAdoption, error) {
	if required <= 0 || resolved == nil {
		return nil, importInvalid("filesystem adoption requires a positive exact capacity and resolved layout")
	}
	req := &agentv1.InspectImportRequest{PoolName: resolved.Backend.PoolName(), Source: source,
		BackendType: mapBackendType(string(resolved.Backend.Kind())), RequiredBytes: required}
	switch {
	case resolved.Backend.Directory != nil:
		if !canonicalHostPath(source) {
			return nil, importInvalid("import-directory requires a canonical absolute directory path")
		}
		req.ExpectedHostRoot = resolved.Backend.Directory.HostRoot
	case resolved.Backend.ZFS != nil && resolved.Backend.Kind() == v1alpha1.BackendIDZFSDataset:
		req.ExpectedParentDataset = resolved.Backend.ZFS.ParentDataset
	default:
		return nil, importInvalid("filesystem adoption requires a directory or zfs.dataset backend")
	}
	if req.PoolName == "" {
		return nil, importInvalid("filesystem adoption requires a configured backend pool")
	}
	target, err := s.getReadyAgent(ctx, agentRef)
	if err != nil {
		return nil, err
	}
	ctx = withAgentName(ctx, agentRef)
	agentClient, closer, err := s.dialAgent(ctx, target.Status.ResolvedAddress)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "dial PillarAgent %q: %v", agentRef, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled
	response, err := agentClient.InspectImport(ctx, req)
	if err != nil {
		grpcSt, _ := status.FromError(err)
		return nil, status.Errorf(
			grpcSt.Code(), "agent InspectImport(%q) failed: %v", source, err)
	}
	p := response.GetFilesystemAdoption()
	a := &v1alpha1.FilesystemAdoption{Kind: v1alpha1.FilesystemAdoptionKind(p.GetKind()),
		CanonicalSource: p.GetCanonicalSource(), ResourceID: p.GetResourceId(), HostPath: p.GetHostPath(),
		FilesystemType: p.GetFilesystemType(), FilesystemID: p.GetFilesystemId(),
		Inode: v1alpha1.FormatFilesystemAdoptionInode(p.GetInode()), ProjectID: p.GetProjectId()}
	err = validateFilesystemAdoption(a, resolved)
	if err != nil {
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"PillarAgent %q returned invalid filesystem identity: %v", agentRef, err)
	}
	if a.CanonicalSource != source {
		return nil, status.Errorf(codes.FailedPrecondition, "inspected filesystem source %q differs from requested source %q",
			a.CanonicalSource, source)
	}
	if response.GetCapacityBytes() != required {
		return nil, status.Errorf(codes.FailedPrecondition, "filesystem source %q exact quota is %d bytes, requested %d",
			a.CanonicalSource, response.GetCapacityBytes(), required)
	}
	return a, nil
}

func (s *ControllerServer) revalidateFilesystemImport(ctx context.Context, pvs *v1alpha1.PillarVolumeState) error {
	if pvs == nil || pvs.Spec.FilesystemAdoption == nil {
		return importInvalid("filesystem revalidation requires a recorded adoption")
	}
	if pvs.Spec.ImportedFrom != "" {
		return status.Errorf(codes.FailedPrecondition, "recorded filesystem adoption also declares a zvol import")
	}
	var err error
	err = s.validateVolumeDriver(pvs)
	if err != nil {
		return err
	}
	err = validateFilesystemAdoption(pvs.Spec.FilesystemAdoption, pvs.Spec.Resolved)
	if err != nil {
		return status.Errorf(
			codes.FailedPrecondition, "recorded filesystem identity is invalid: %v", err)
	}
	actual, err := s.inspectFilesystemImport(ctx, pvs.Spec.AgentRef, pvs.Spec.FilesystemAdoption.CanonicalSource,
		pvs.Spec.Resolved, pvs.Spec.CapacityBytes)
	if err != nil {
		return err
	}
	if *actual != *pvs.Spec.FilesystemAdoption {
		return status.Errorf(codes.FailedPrecondition, "filesystem source %q no longer matches its recorded native identity",
			pvs.Spec.FilesystemAdoption.CanonicalSource)
	}
	return nil
}

func (s *ControllerServer) resolveFilesystemImportRequest(
	ctx context.Context,
	pvName string,
	pvExists bool,
	existing *v1alpha1.PillarVolumeState,
	resolved *v1alpha1.ResolvedVolumeConfig,
	directory, dataset, agentRef string,
	required int64,
) (*v1alpha1.FilesystemAdoption, string, error) {
	directory, dataset = strings.TrimSpace(directory), strings.TrimSpace(dataset)
	recorded := existing != nil && existing.Spec.FilesystemAdoption != nil
	if s.effectiveDriverName() != v1alpha1.FileCSIDriver {
		return resolveNonFileFilesystemImport(directory, dataset, recorded)
	}
	if directory != "" && dataset != "" {
		return nil, "", importInvalid("filesystem import selectors are mutually exclusive")
	}
	if pvExists {
		return s.resolveRecordedFilesystemImport(
			ctx, pvName, existing, directory, dataset, required)
	}
	return s.resolveNewFilesystemImport(
		ctx, pvName, resolved, directory, dataset, agentRef, required)
}

func resolveNonFileFilesystemImport(
	directory, dataset string,
	recorded bool,
) (*v1alpha1.FilesystemAdoption, string, error) {
	if directory != "" || dataset != "" || recorded {
		return nil, "", importInvalid(
			"filesystem adoption requires CSI driver %q", v1alpha1.FileCSIDriver)
	}
	return nil, "", nil
}

func (s *ControllerServer) resolveRecordedFilesystemImport(
	ctx context.Context,
	pvName string,
	existing *v1alpha1.PillarVolumeState,
	directory, dataset string,
	required int64,
) (*v1alpha1.FilesystemAdoption, string, error) {
	if existing == nil || existing.Spec.FilesystemAdoption == nil ||
		existing.Spec.ImportedFrom != "" {
		return nil, "", importInvalid(
			"files CSI driver requires a recorded filesystem adoption; an existing lifecycle cannot change kind")
	}
	a := existing.Spec.FilesystemAdoption
	if (directory != "" &&
		(a.Kind != v1alpha1.FilesystemAdoptionKindDirectory || directory != a.CanonicalSource)) ||
		(dataset != "" &&
			(a.Kind != v1alpha1.FilesystemAdoptionKindZFSDataset || dataset != a.CanonicalSource)) {
		return nil, "", importInvalid(
			"volume %q already adopted %q; filesystem kind and source cannot change",
			pvName, a.CanonicalSource)
	}
	if required != existing.Spec.CapacityBytes {
		return nil, "", importInvalid(
			"volume %q filesystem adoption exact capacity cannot change", pvName)
	}
	err := s.revalidateFilesystemImport(ctx, existing)
	if err != nil {
		return nil, "", err
	}
	prefix := existing.Spec.Resolved.Backend.PoolName() + "/"
	if !strings.HasPrefix(existing.Spec.AgentVolumeID, prefix) {
		return nil, "", status.Errorf(
			codes.FailedPrecondition, "volume %q has invalid recorded agentVolumeID", pvName)
	}
	leaf := strings.TrimPrefix(existing.Spec.AgentVolumeID, prefix)
	expectedLeaf, err := filesystemImportLeaf(a)
	if err != nil {
		return nil, "", status.Errorf(
			codes.FailedPrecondition,
			"volume %q has invalid recorded filesystem identity: %v", pvName, err)
	}
	if leaf != expectedLeaf {
		return nil, "", status.Errorf(
			codes.FailedPrecondition,
			"volume %q recorded volume leaf does not match native identity", pvName)
	}
	return a.DeepCopy(), leaf, nil
}

func (s *ControllerServer) resolveNewFilesystemImport(
	ctx context.Context,
	pvName string,
	resolved *v1alpha1.ResolvedVolumeConfig,
	directory, dataset, agentRef string,
	required int64,
) (*v1alpha1.FilesystemAdoption, string, error) {
	source := directory
	kind := v1alpha1.FilesystemAdoptionKindDirectory
	if dataset != "" {
		source, kind = dataset, v1alpha1.FilesystemAdoptionKindZFSDataset
	}
	if source == "" {
		return nil, "", importInvalid(
			"files CSI driver requires import-directory or import-zfs-dataset; " +
				"dynamic creation and zvol imports are not supported")
	}
	if resolved == nil ||
		(kind == v1alpha1.FilesystemAdoptionKindDirectory &&
			resolved.Backend.Kind() != v1alpha1.BackendIDDirectory) ||
		(kind == v1alpha1.FilesystemAdoptionKindZFSDataset &&
			resolved.Backend.Kind() != v1alpha1.BackendIDZFSDataset) {
		return nil, "", importInvalid(
			"filesystem selector kind %q does not match configured backend", kind)
	}
	a, err := s.inspectFilesystemImport(ctx, agentRef, source, resolved, required)
	if err != nil {
		return nil, "", err
	}
	if a.Kind != kind {
		return nil, "", status.Errorf(
			codes.FailedPrecondition, "inspected filesystem kind does not match selector")
	}
	err = s.refuseFilesystemImportConflict(ctx, pvName, agentRef, a)
	if err != nil {
		return nil, "", err
	}
	leaf, err := filesystemImportLeaf(a)
	if err != nil {
		return nil, "", status.Errorf(
			codes.FailedPrecondition, "inspected filesystem identity is invalid: %v", err)
	}
	return a, leaf, nil
}

// legacyZFSDatasetSource resolves the native dataset path managed by a
// pre-filesystem-adoption lifecycle. The resolved backend and agent volume ID
// are durable, so this remains usable even when the source is not mounted.
func legacyZFSDatasetSource(pvs *v1alpha1.PillarVolumeState) (source string, isLegacyDataset bool, err error) {
	if pvs == nil ||
		pvs.Spec.FilesystemAdoption != nil ||
		pvs.Spec.ImportedFrom != "" ||
		pvs.Spec.BackendType != string(v1alpha1.BackendIDZFSDataset) {
		return "", false, nil
	}
	resolved := pvs.Spec.Resolved
	if resolved == nil || resolved.Backend.Kind() != v1alpha1.BackendIDZFSDataset ||
		resolved.Backend.ZFS == nil {
		return "", false, status.Errorf(
			codes.FailedPrecondition,
			"legacy zfs-dataset volume %q has incomplete durable dataset identity",
			pvs.Name)
	}
	zfs := resolved.Backend.ZFS
	prefix := zfs.Pool + "/"
	if zfs.Pool == "" || !strings.HasPrefix(pvs.Spec.AgentVolumeID, prefix) {
		return "", false, status.Errorf(
			codes.FailedPrecondition,
			"legacy zfs-dataset volume %q has invalid durable agent volume ID",
			pvs.Name)
	}
	leaf := strings.TrimPrefix(pvs.Spec.AgentVolumeID, prefix)
	if leaf == "" || strings.Contains(leaf, "/") {
		return "", false, status.Errorf(
			codes.FailedPrecondition,
			"legacy zfs-dataset volume %q has invalid durable dataset leaf",
			pvs.Name)
	}
	source = zfs.Pool
	if zfs.ParentDataset != "" {
		source += "/" + zfs.ParentDataset
	}
	return source + "/" + leaf, true, nil
}

func (s *ControllerServer) refuseFilesystemImportConflict(ctx context.Context, pvName, agentRef string,
	a *v1alpha1.FilesystemAdoption) error {
	var list v1alpha1.PillarVolumeStateList
	err := s.uncachedReader().List(ctx, &list)
	if err != nil {
		return status.Errorf(
			codes.Internal, "list PillarVolumeStates for filesystem conflict: %v", err)
	}
	resourceID, err := filesystemResourceID(a)
	if err != nil {
		return status.Errorf(
			codes.FailedPrecondition, "invalid filesystem identity: %v", err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.Name == pvName || item.Spec.AgentRef != agentRef {
			continue
		}
		err = refuseFilesystemVolumeConflict(item, a, resourceID)
		if err != nil {
			return err
		}
	}
	return nil
}

func refuseFilesystemVolumeConflict(
	item *v1alpha1.PillarVolumeState, a *v1alpha1.FilesystemAdoption, resourceID string,
) error {
	if item.Spec.FilesystemAdoption != nil {
		itemResourceID, err := filesystemResourceID(item.Spec.FilesystemAdoption)
		if err != nil {
			return status.Errorf(
				codes.FailedPrecondition,
				"volume %q has invalid filesystem identity: %v", item.Name, err)
		}
		if itemResourceID == resourceID {
			return status.Errorf(
				codes.FailedPrecondition,
				"filesystem source %q is already managed by volume %q (PillarVolumeState %q)",
				a.CanonicalSource, item.Spec.VolumeID, item.Name)
		}
		return nil
	}
	if a.Kind != v1alpha1.FilesystemAdoptionKindZFSDataset {
		return nil
	}
	legacySource, isLegacyDataset, err := legacyZFSDatasetSource(item)
	if err != nil {
		return err
	}
	if isLegacyDataset && legacySource == a.CanonicalSource {
		return status.Errorf(
			codes.FailedPrecondition,
			"filesystem source %q is already managed by legacy volume %q (PillarVolumeState %q)",
			a.CanonicalSource, item.Spec.VolumeID, item.Name)
	}
	return nil
}
