package agent

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

const (
	filesystemAdoptionKindDirectory  = "directory"
	filesystemAdoptionKindZFSDataset = "zfs-dataset"
)

// InspectImport resolves a source without taking ownership or writing a fence.
func (s *Server) InspectImport(
	ctx context.Context,
	req *agentv1.InspectImportRequest,
) (*agentv1.InspectImportResponse, error) {
	if req.GetPoolName() == "" || req.GetSource() == "" || req.GetRequiredBytes() <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "InspectImport requires pool, source and positive exact capacity")
	}
	b, err := s.backendForType(req.GetPoolName()+"/inspection", req.GetBackendType())
	if err != nil {
		return nil, err
	}
	checkErr := checkBackendType("InspectImport", req.GetBackendType(), b.Type(), req.GetPoolName())
	if checkErr != nil {
		return nil, checkErr
	}
	inspector, ok := b.(backend.VolumeInspector)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend cannot inspect existing filesystems")
	}
	layout := backend.Layout{ParentDataset: req.GetExpectedParentDataset(), HostRoot: req.GetExpectedHostRoot()}
	layoutErr := requireFilesystemLayout(b.Type(), layout)
	if layoutErr != nil {
		return nil, layoutErr
	}
	if layout != b.Layout() {
		return nil, status.Errorf(codes.FailedPrecondition, "filesystem inspection configured layout changed")
	}
	found, err := inspector.InspectImport(ctx, req.GetSource(), req.GetRequiredBytes(), layout)
	if err != nil {
		return nil, importVolumeError(err)
	}
	if found == nil || found.CapacityBytes != req.GetRequiredBytes() {
		return nil, status.Errorf(codes.FailedPrecondition, "inspection did not establish exact capacity")
	}
	_, fenceErr := filesystemFenceKey(found.Filesystem)
	if fenceErr != nil {
		return nil, fenceErr
	}
	namespaceErr := s.checkFilesystemSourceNamespace(found.Filesystem)
	if namespaceErr != nil {
		return nil, namespaceErr
	}
	ownerErr := s.checkLegacyFilesystemOwner(found.Filesystem)
	if ownerErr != nil {
		return nil, ownerErr
	}
	return &agentv1.InspectImportResponse{FilesystemAdoption: found.Filesystem, CapacityBytes: found.CapacityBytes}, nil
}

func filesystemFenceKey(a *agentv1.FilesystemAdoption) (string, error) {
	key := backend.FilesystemFenceID(a)
	if key == "" || a.GetCanonicalSource() == "" {
		return "", status.Errorf(codes.InvalidArgument, "invalid filesystem adoption descriptor")
	}
	switch a.GetKind() {
	case filesystemAdoptionKindDirectory:
		if !validDirectoryAdoption(a) {
			return "", status.Errorf(codes.InvalidArgument, "incomplete native directory identity")
		}
	case filesystemAdoptionKindZFSDataset:
		if !validZFSDatasetAdoption(a) {
			return "", status.Errorf(codes.InvalidArgument, "invalid native ZFS identity")
		}
	default:
		return "", status.Errorf(codes.InvalidArgument, "unknown filesystem adoption kind")
	}
	return key, nil
}

func validDirectoryAdoption(a *agentv1.FilesystemAdoption) bool {
	source := a.GetCanonicalSource()
	return filepath.IsAbs(source) &&
		filepath.Clean(source) == source &&
		a.GetFilesystemId() != "" &&
		a.GetInode() != 0 &&
		a.GetProjectId() != 0 &&
		(a.GetFilesystemType() == "ext4" || a.GetFilesystemType() == "xfs")
}

func validZFSDatasetAdoption(a *agentv1.FilesystemAdoption) bool {
	hostPath := a.GetHostPath()
	return a.GetFilesystemType() == "zfs" &&
		(hostPath == "" || (filepath.IsAbs(hostPath) && filepath.Clean(hostPath) == hostPath))
}

func filesystemBackendType(a *agentv1.FilesystemAdoption) agentv1.BackendType {
	if a.GetKind() == filesystemAdoptionKindDirectory {
		return agentv1.BackendType_BACKEND_TYPE_DIRECTORY
	}
	return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
}

func requireFilesystemLayout(t agentv1.BackendType, layout backend.Layout) error {
	if t == agentv1.BackendType_BACKEND_TYPE_DIRECTORY && layout.HostRoot == "" {
		return status.Errorf(codes.InvalidArgument, "filesystem adoption requires the expected configured layout")
	}
	return nil
}

func filesystemLayout(a *agentv1.FilesystemAdoption, params *agentv1.BackendParams) (backend.Layout, error) {
	layout := backend.Layout{}
	switch a.GetKind() {
	case filesystemAdoptionKindDirectory:
		p := params.GetDirectory()
		if p == nil {
			return layout, status.Errorf(codes.InvalidArgument, "directory backend parameters required")
		}
		layout.HostRoot = p.GetHostRoot()
	case filesystemAdoptionKindZFSDataset:
		p := params.GetZfs()
		if p == nil {
			return layout, status.Errorf(codes.InvalidArgument, "ZFS backend parameters required")
		}
		layout.ParentDataset = p.GetParentDataset()
	}
	return layout, requireFilesystemLayout(filesystemBackendType(a), layout)
}

// filesystemOperation serializes every pool alias on the native resource key.
// Imports pin before recording ownership; subsequent mutations record the fence
// before touching only the agent-owned proxy or protocol state.
func (s *Server) filesystemOperation(
	ctx context.Context,
	volumeID string,
	a *agentv1.FilesystemAdoption,
	token *agentv1.FencingToken,
	op fenceOp,
	capacity int64,
	params *agentv1.BackendParams,
	importing bool,
	mutate func(backend.PinnedFilesystem, *fencingMark) error,
) (retErr error) {
	defer func() {
		if status.Code(retErr) == codes.Unknown {
			retErr = status.Errorf(codes.Internal, "filesystem operation %q: %v", volumeID, retErr)
		}
	}()

	source, err := s.filesystemOperationSource(a, volumeID, capacity)
	if err != nil {
		return err
	}
	if source.legacyID != "" && source.legacyID != source.key {
		unlockLegacy := s.lockFencing(source.legacyID)
		defer unlockLegacy()
	}
	unlock := s.lockFencing(source.key)
	defer unlock()

	admission, err := s.prepareFilesystemOperationAdmission(
		a, volumeID, source, token, op, importing,
	)
	if err != nil {
		return err
	}
	if admission.skip {
		return nil
	}

	next := admission.adm.next
	next.Filesystem = a
	next.FilesystemVolumeID = volumeID
	pin, layout, err := s.prepareFilesystemPin(
		ctx, volumeID, a, token, op, capacity, params, importing, source.pool, admission.stored, admission.exists,
	)
	if err != nil {
		return err
	}
	if pin != nil {
		defer func() {
			retErr = closeFilesystemOperationPin(pin, retErr)
		}()
		err := validateFilesystemPin(pin, a, capacity)
		if err != nil {
			return err
		}
	}
	if capacity > 0 {
		next.FilesystemCapacity = capacity
		next.FilesystemLayout = &layout
	}
	return s.commitFilesystemOperation(source.key, next, admission.stored, admission.adm.changed, importing, pin, mutate)
}

type filesystemOperationAdmission struct {
	adm    fenceAdmission
	stored fencingMark
	exists bool
	skip   bool
}

func (s *Server) prepareFilesystemOperationAdmission(
	a *agentv1.FilesystemAdoption,
	volumeID string,
	source filesystemSource,
	token *agentv1.FencingToken,
	op fenceOp,
	importing bool,
) (filesystemOperationAdmission, error) {
	if importing {
		err := s.checkLegacyFilesystemOwner(a)
		if err != nil {
			return filesystemOperationAdmission{}, err
		}
	}
	stored, exists, err := s.readFencingMark(source.key)
	if err != nil {
		return filesystemOperationAdmission{}, err
	}
	adm, err := admitFencingToken(source.key, token, op, stored, exists)
	if err != nil {
		return filesystemOperationAdmission{}, err
	}
	skip, err := filesystemOperationState(op, importing, token, stored, exists, volumeID, a)
	if err != nil {
		return filesystemOperationAdmission{}, err
	}
	return filesystemOperationAdmission{adm: adm, stored: stored, exists: exists, skip: skip}, nil
}

func validateFilesystemPin(
	pin backend.PinnedFilesystem,
	a *agentv1.FilesystemAdoption,
	capacity int64,
) error {
	if !proto.Equal(pin.Adoption(), a) || pin.CapacityBytes() != capacity {
		return status.Errorf(codes.FailedPrecondition, "pinned filesystem identity or exact capacity changed")
	}
	return nil
}

func closeFilesystemOperationPin(pin backend.PinnedFilesystem, retErr error) error {
	closeErr := pin.Close()
	if closeErr != nil {
		return status.Errorf(codes.Internal, "filesystem pin close: %v", errors.Join(retErr, closeErr))
	}
	return retErr
}

func (s *Server) commitFilesystemOperation(
	key string,
	next, stored fencingMark,
	changed, importing bool,
	pin backend.PinnedFilesystem,
	mutate func(backend.PinnedFilesystem, *fencingMark) error,
) error {
	if !importing {
		persistErr := s.persistFencingMark(key, next, changed || !proto.Equal(stored.Filesystem, next.Filesystem))
		if persistErr != nil {
			return persistErr
		}
	}
	if mutate != nil {
		mutateErr := mutate(pin, &next)
		if mutateErr != nil {
			return mutateErr
		}
	}
	return s.writeFencingMark(key, next)
}

type filesystemSource struct {
	key      string
	legacyID string
	pool     string
}

func (s *Server) filesystemOperationSource(
	a *agentv1.FilesystemAdoption,
	volumeID string,
	capacity int64,
) (filesystemSource, error) {
	key, err := filesystemFenceKey(a)
	if err != nil {
		return filesystemSource{}, err
	}
	if capacity > 0 {
		err = s.checkFilesystemSourceNamespace(a)
		if err != nil {
			return filesystemSource{}, err
		}
	}
	pool, err := poolFromVolumeID(volumeID)
	if err != nil {
		return filesystemSource{}, err
	}
	legacyID, err := legacyFilesystemVolumeID(a)
	if err != nil {
		return filesystemSource{}, err
	}
	return filesystemSource{key: key, legacyID: legacyID, pool: pool}, nil
}

func filesystemOperationState(
	op fenceOp,
	importing bool,
	token *agentv1.FencingToken,
	stored fencingMark,
	exists bool,
	volumeID string,
	a *agentv1.FilesystemAdoption,
) (bool, error) {
	if !importing && op == fenceGrant && (!exists || stored.Ended || stored.VolumeUID != token.GetVolumeUid()) {
		return false, status.Errorf(
			codes.FailedPrecondition,
			"filesystem lifecycle must be imported before publication or recovery",
		)
	}
	if op == fenceRevoke && (!exists || stored.Ended || stored.VolumeUID != token.GetVolumeUid()) {
		return true, nil
	}
	if exists && stored.VolumeUID == token.GetVolumeUid() &&
		(stored.Filesystem == nil || !proto.Equal(stored.Filesystem, a) || stored.FilesystemVolumeID != volumeID) {
		return false, status.Errorf(codes.FailedPrecondition, "filesystem descriptor differs from durable ownership")
	}
	return false, nil
}

func (s *Server) prepareFilesystemPin(
	ctx context.Context,
	volumeID string,
	a *agentv1.FilesystemAdoption,
	token *agentv1.FencingToken,
	op fenceOp,
	capacity int64,
	params *agentv1.BackendParams,
	importing bool,
	pool string,
	stored fencingMark,
	exists bool,
) (backend.PinnedFilesystem, backend.Layout, error) {
	if capacity <= 0 {
		if op == fenceGrant {
			return nil, backend.Layout{}, status.Errorf(codes.InvalidArgument, "positive exact filesystem capacity required")
		}
		return nil, backend.Layout{}, nil
	}
	layout, err := filesystemOperationLayout(a, token, capacity, params, stored, exists)
	if err != nil {
		return nil, backend.Layout{}, err
	}
	b, err := s.filesystemPinBackend(volumeID, a, layout)
	if err != nil {
		return nil, backend.Layout{}, err
	}
	err = inspectFilesystemImport(ctx, b, a, capacity, layout, params, importing, pool, token, stored, exists)
	if err != nil {
		return nil, backend.Layout{}, err
	}
	pin, err := importFilesystemPin(ctx, b, volumeID, capacity, a, layout)
	if err != nil {
		return nil, backend.Layout{}, err
	}
	return pin, layout, nil
}

func filesystemOperationLayout(
	a *agentv1.FilesystemAdoption,
	token *agentv1.FencingToken,
	capacity int64,
	params *agentv1.BackendParams,
	stored fencingMark,
	exists bool,
) (backend.Layout, error) {
	layout, err := filesystemLayout(a, params)
	if err != nil {
		return backend.Layout{}, err
	}
	if exists && stored.VolumeUID == token.GetVolumeUid() && !stored.Ended &&
		(stored.FilesystemCapacity != capacity || stored.FilesystemLayout == nil || *stored.FilesystemLayout != layout) {
		return backend.Layout{}, status.Errorf(
			codes.FailedPrecondition, "filesystem capacity or placement differs from durable import",
		)
	}
	return layout, nil
}

func inspectFilesystemImport(
	ctx context.Context,
	b backend.VolumeBackend,
	a *agentv1.FilesystemAdoption,
	capacity int64,
	layout backend.Layout,
	params *agentv1.BackendParams,
	importing bool,
	pool string,
	token *agentv1.FencingToken,
	stored fencingMark,
	exists bool,
) error {
	if a.GetKind() != filesystemAdoptionKindDirectory {
		return nil
	}
	if params.GetDirectory().GetLogicalPool() != pool {
		return status.Errorf(codes.InvalidArgument, "directory logical pool mismatch")
	}
	if importing && (!exists || stored.Ended || stored.VolumeUID != token.GetVolumeUid()) {
		return inspectFirstFilesystemClaim(ctx, b, a, capacity, layout)
	}
	return nil
}

func importFilesystemPin(
	ctx context.Context,
	b backend.VolumeBackend,
	volumeID string,
	capacity int64,
	a *agentv1.FilesystemAdoption,
	layout backend.Layout,
) (backend.PinnedFilesystem, error) {
	importer, ok := b.(backend.FilesystemImporter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "backend cannot pin existing filesystem")
	}
	pin, err := importer.ImportFilesystem(ctx, volumeID, capacity, a, layout)
	if err != nil {
		return nil, importVolumeError(err)
	}
	if pin == nil {
		return nil, status.Errorf(codes.Internal, "filesystem importer returned no pin")
	}
	return pin, nil
}

func (s *Server) filesystemPinBackend(
	volumeID string,
	a *agentv1.FilesystemAdoption,
	layout backend.Layout,
) (backend.VolumeBackend, error) {
	b, err := s.backendForType(volumeID, filesystemBackendType(a))
	if err != nil {
		return nil, err
	}
	if b.Type() != filesystemBackendType(a) {
		return nil, status.Errorf(codes.InvalidArgument, "filesystem backend does not match configured pool")
	}
	if layout != b.Layout() {
		return nil, status.Errorf(codes.FailedPrecondition, "filesystem configured layout changed")
	}
	return b, nil
}

func inspectFirstFilesystemClaim(
	ctx context.Context,
	b backend.VolumeBackend,
	a *agentv1.FilesystemAdoption,
	capacity int64,
	layout backend.Layout,
) error {
	inspector, ok := b.(backend.VolumeInspector)
	if !ok {
		return status.Errorf(codes.Unimplemented, "directory backend cannot prove first-claim quota scope")
	}
	inspected, err := inspector.InspectImport(ctx, a.GetCanonicalSource(), capacity, layout)
	if err != nil {
		return importVolumeError(err)
	}
	if inspected == nil || inspected.CapacityBytes != capacity || !proto.Equal(inspected.Filesystem, a) {
		return status.Errorf(codes.FailedPrecondition, "directory identity or quota scope changed before first claim")
	}
	return nil
}

func (s *Server) importFilesystem(
	ctx context.Context,
	req *agentv1.ImportVolumeRequest,
) (*agentv1.ImportVolumeResponse, error) {
	a := req.GetFilesystemAdoption()
	if req.GetBackendType() != filesystemBackendType(a) {
		return nil, status.Errorf(codes.InvalidArgument, "filesystem import backend type mismatch")
	}
	var size int64
	err := s.filesystemOperation(
		ctx,
		req.GetVolumeId(),
		a,
		req.GetFence(),
		fenceGrant,
		req.GetCapacityBytes(),
		req.GetBackendParams(),
		true,
		func(pin backend.PinnedFilesystem, _ *fencingMark) error {
			size = pin.CapacityBytes()
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return &agentv1.ImportVolumeResponse{DevicePath: backend.FilesystemMountSource(a), CapacityBytes: size}, nil
}

func (s *Server) exportFilesystem(
	ctx context.Context,
	params ExportParams,
	exact bool,
	clients []string,
) (*ExportResult, error) {
	if s.nfsManager == nil {
		return nil, status.Errorf(codes.Unimplemented, "NFS protocol is unavailable on this agent")
	}
	e, err := nfsProtocolSpec(params)
	if err != nil {
		return nil, err
	}
	canonicalClients, err := canonicalFilesystemClients(s.nfsManager, clients)
	if err != nil {
		return nil, err
	}
	var hostPath string
	err = s.filesystemOperation(
		ctx,
		params.VolumeID,
		params.FilesystemAdoption,
		params.Fence,
		fenceGrant,
		params.CapacityBytes,
		params.BackendParams,
		false,
		s.filesystemExportMutation(ctx, params, exact, canonicalClients, &e, &hostPath),
	)
	if err != nil {
		return nil, err
	}
	path, err := s.nfsManager.ExportPath(hostPath)
	if err != nil {
		return nil, nfsStatus("ExportVolume", params.VolumeID, err)
	}
	return &ExportResult{TargetID: path, VolumeRef: path, Address: e.BindAddress, Port: nfs.Port}, nil
}

func canonicalFilesystemClients(manager *nfs.Manager, clients []string) ([]string, error) {
	canonicalClients := make([]string, 0, len(clients))
	for _, client := range clients {
		validationErr := manager.ValidateClient(client)
		if validationErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s", validationErr.Error())
		}
		ip, parseErr := netip.ParseAddr(client)
		if parseErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s", parseErr.Error())
		}
		canonicalClients = append(canonicalClients, ip.Unmap().String())
	}
	slices.Sort(canonicalClients)
	return slices.Compact(canonicalClients), nil
}

func (s *Server) filesystemExportMutation(
	ctx context.Context,
	params ExportParams,
	exact bool,
	canonicalClients []string,
	e *nfs.Export,
	hostPath *string,
) func(backend.PinnedFilesystem, *fencingMark) error {
	return func(pin backend.PinnedFilesystem, mark *fencingMark) error {
		previouslyExported := mark.FilesystemExported
		var mountErr error
		*hostPath, mountErr = s.ensureFilesystemProxy(ctx, params.VolumeID, pin, mark)
		if mountErr != nil {
			return mountErr
		}
		e.Path = *hostPath
		e.SourceKey = backend.FilesystemFenceID(params.FilesystemAdoption)
		e.FenceUID = params.Fence.GetVolumeUid()
		desiredClients := filesystemExportDesiredClients(mark, exact, canonicalClients)
		// Grow the durable teardown bound before publication; shrink it only once
		// the manager has committed the exact new policy.
		mark.FilesystemOpen = mark.FilesystemOpen || !params.ACLEnabled
		mark.FilesystemExported = true
		writeErr := s.writeFencingMark(backend.FilesystemFenceID(params.FilesystemAdoption), *mark)
		if writeErr != nil {
			return writeErr
		}
		e.Clients = desiredClients
		// validateOwned/validateExport still enforce the dedicated manager root and mountpoint.
		putErr := s.nfsManager.Put(ctx, *e, exact, true)
		if putErr != nil {
			// A retry or recovery failure must not revoke a previously live export,
			// its peer clients, or the shared local publisher's proxy.
			if previouslyExported {
				return nfsStatus("ExportVolume", params.VolumeID, putErr)
			}
			return s.rollbackFilesystemExport(ctx, params, mark, putErr)
		}
		mark.FilesystemOpen = !params.ACLEnabled
		if !params.ACLEnabled {
			mark.FilesystemClients = nil
		} else if exact {
			mark.FilesystemClients = canonicalClients
		}
		return nil
	}
}

func filesystemExportDesiredClients(mark *fencingMark, exact bool, canonicalClients []string) []string {
	if !exact {
		return mark.FilesystemClients
	}
	for _, client := range canonicalClients {
		if !slices.Contains(mark.FilesystemClients, client) {
			mark.FilesystemClients = append(mark.FilesystemClients, client)
		}
	}
	return canonicalClients
}

func (s *Server) rollbackFilesystemExport(
	ctx context.Context,
	params ExportParams,
	mark *fencingMark,
	putErr error,
) error {
	removeErr := s.nfsManager.Remove(context.WithoutCancel(ctx), params.VolumeID)
	if removeErr != nil {
		return status.Errorf(codes.Internal, "filesystem export rollback: %v", errors.Join(putErr, removeErr))
	}
	if !mark.LocalAttached {
		proxyErr := s.removeFilesystemProxy(
			context.WithoutCancel(ctx),
			params.VolumeID,
			params.FilesystemAdoption,
			mark,
		)
		if proxyErr != nil {
			return status.Errorf(codes.Internal, "filesystem export rollback: %v", errors.Join(putErr, proxyErr))
		}
	}
	mark.FilesystemClients = nil
	mark.FilesystemOpen = false
	mark.FilesystemExported = false
	stateErr := s.writeFencingMark(backend.FilesystemFenceID(params.FilesystemAdoption), *mark)
	return status.Errorf(codes.Internal, "filesystem export rollback: %v", errors.Join(putErr, stateErr))
}

func (s *Server) unexportFilesystem(
	ctx context.Context,
	volumeID string,
	a *agentv1.FilesystemAdoption,
	token *agentv1.FencingToken,
) error {
	return s.filesystemOperation(
		ctx,
		volumeID,
		a,
		token,
		fenceRevoke,
		0,
		nil,
		false,
		func(_ backend.PinnedFilesystem, mark *fencingMark) error {
			if mark.FilesystemExported && s.nfsManager == nil {
				return status.Errorf(codes.Unavailable, "owned NFS runtime required to withdraw recorded filesystem export")
			}
			if s.nfsManager != nil {
				removeErr := s.nfsManager.Remove(ctx, volumeID)
				if removeErr != nil {
					return nfsStatus("UnexportVolume", volumeID, removeErr)
				}
			}
			// Preserve creation evidence until the owned proxy was removed. A local
			// publisher keeps that proxy independently of network admission.
			if !mark.LocalAttached {
				proxyErr := s.removeFilesystemProxy(ctx, volumeID, a, mark)
				if proxyErr != nil {
					return proxyErr
				}
			}
			mark.FilesystemClients = nil
			mark.FilesystemOpen = false
			mark.FilesystemExported = false
			return nil
		},
	)
}

func (s *Server) changeFilesystemClient(
	ctx context.Context,
	req *agentv1.AllowInitiatorRequest,
) error {
	if s.nfsManager == nil {
		return status.Errorf(codes.Unimplemented, "NFS protocol unavailable")
	}
	validationErr := s.nfsManager.ValidateClient(req.GetInitiatorId())
	if validationErr != nil {
		return status.Errorf(codes.InvalidArgument, "%s", validationErr.Error())
	}
	client, parseErr := netip.ParseAddr(req.GetInitiatorId())
	if parseErr != nil {
		return status.Errorf(codes.InvalidArgument, "%s", parseErr.Error())
	}
	canonicalClient := client.Unmap().String()
	return s.filesystemOperation(
		ctx,
		req.GetVolumeId(),
		req.GetFilesystemAdoption(),
		req.GetFence(),
		fenceGrant,
		req.GetCapacityBytes(),
		req.GetBackendParams(),
		false,
		func(pin backend.PinnedFilesystem, mark *fencingMark) error {
			if !mark.FilesystemExported {
				return status.Errorf(codes.FailedPrecondition, "filesystem is not exported for remote publication")
			}
			_, proxyErr := s.ensureFilesystemProxy(ctx, req.GetVolumeId(), pin, mark)
			if proxyErr != nil {
				return proxyErr
			}
			if !mark.FilesystemOpen && !slices.Contains(mark.FilesystemClients, canonicalClient) {
				mark.FilesystemClients = append(slices.Clone(mark.FilesystemClients), canonicalClient)
			}
			writeErr := s.writeFencingMark(backend.FilesystemFenceID(req.GetFilesystemAdoption()), *mark)
			if writeErr != nil {
				return writeErr
			}
			return nfsStatus(
				"AllowInitiator",
				req.GetVolumeId(),
				s.nfsManager.ChangeClient(ctx, req.GetVolumeId(), req.GetInitiatorId(), true),
			)
		},
	)
}

func (s *Server) denyFilesystemClient(
	ctx context.Context,
	req *agentv1.DenyInitiatorRequest,
) error {
	client, parseErr := netip.ParseAddr(req.GetInitiatorId())
	if parseErr != nil {
		return status.Errorf(codes.InvalidArgument, "filesystem initiator must be a numeric address")
	}
	canonicalClient := client.Unmap().String()
	return s.filesystemOperation(
		ctx,
		req.GetVolumeId(),
		req.GetFilesystemAdoption(),
		req.GetFence(),
		fenceRevoke,
		0,
		nil,
		false,
		func(_ backend.PinnedFilesystem, mark *fencingMark) error {
			if s.nfsManager == nil {
				return status.Errorf(codes.Unimplemented, "NFS protocol unavailable")
			}
			changeErr := s.nfsManager.ChangeClient(ctx, req.GetVolumeId(), req.GetInitiatorId(), false)
			if changeErr != nil {
				return nfsStatus("DenyInitiator", req.GetVolumeId(), changeErr)
			}
			mark.FilesystemClients = slices.DeleteFunc(
				slices.Clone(mark.FilesystemClients),
				func(client string) bool {
					return client == canonicalClient
				},
			)
			return nil
		},
	)
}

func (s *Server) localFilesystem(
	ctx context.Context,
	req *agentv1.SetLocalAttachRequest,
) (*agentv1.SetLocalAttachResponse, error) {
	var hostPath string
	op := fenceRevoke
	capacity := int64(0)
	if req.GetLocal() {
		op = fenceGrant
		capacity = req.GetCapacityBytes()
	}
	err := s.filesystemOperation(
		ctx,
		req.GetVolumeId(),
		req.GetFilesystemAdoption(),
		req.GetFence(),
		op,
		capacity,
		req.GetBackendParams(),
		false,
		func(pin backend.PinnedFilesystem, mark *fencingMark) error {
			if req.GetLocal() {
				var proxyErr error
				hostPath, proxyErr = s.ensureFilesystemProxy(ctx, req.GetVolumeId(), pin, mark)
				if proxyErr != nil {
					return proxyErr
				}
			}
			if !req.GetLocal() && !mark.FilesystemExported {
				removeErr := s.removeFilesystemProxy(ctx, req.GetVolumeId(), req.GetFilesystemAdoption(), mark)
				if removeErr != nil {
					return removeErr
				}
			}
			mark.LocalAttached = req.GetLocal()
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return &agentv1.SetLocalAttachResponse{DevicePath: hostPath}, nil
}

func (s *Server) releaseFilesystem(
	ctx context.Context,
	volumeID string,
	a *agentv1.FilesystemAdoption,
	token *agentv1.FencingToken,
	deleting bool,
) error {
	key, err := filesystemFenceKey(a)
	if err != nil {
		return err
	}
	for {
		owned, err := s.retireFence(ctx, key, token, false)
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		err = s.filesystemOperation(
			ctx,
			volumeID,
			a,
			token,
			fenceRevoke,
			0,
			nil,
			false,
			s.releaseFilesystemMutation(ctx, volumeID, a, deleting),
		)
		if err != nil {
			return err
		}
		owned, err = s.retireFence(ctx, key, token, true)
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
	}
}

func (s *Server) releaseFilesystemMutation(
	ctx context.Context,
	volumeID string,
	a *agentv1.FilesystemAdoption,
	deleting bool,
) func(backend.PinnedFilesystem, *fencingMark) error {
	return func(_ backend.PinnedFilesystem, mark *fencingMark) error {
		if mark.LocalAttached || (deleting && (mark.FilesystemOpen || len(mark.FilesystemClients) != 0)) {
			return status.Errorf(codes.FailedPrecondition, "cannot retire a published adopted filesystem")
		}
		if mark.FilesystemExported && s.nfsManager == nil {
			return status.Errorf(codes.Unavailable, "owned NFS runtime required to retire recorded filesystem export")
		}
		if s.nfsManager != nil {
			removeErr := s.nfsManager.Remove(ctx, volumeID)
			if removeErr != nil {
				return nfsStatus("ReleaseVolume", volumeID, removeErr)
			}
		}
		proxyErr := s.removeFilesystemProxy(ctx, volumeID, a, mark)
		if proxyErr != nil {
			return proxyErr
		}
		mark.LocalAttached = false
		mark.FilesystemClients = nil
		mark.FilesystemOpen = false
		mark.FilesystemExported = false
		return nil
	}
}

func (s *Server) reconcileFilesystem(
	ctx context.Context,
	vol *agentv1.VolumeDesiredState,
) error {
	if vol.GetBackendType() != filesystemBackendType(vol.GetFilesystemAdoption()) {
		return status.Errorf(codes.InvalidArgument, "filesystem reconcile backend type mismatch")
	}
	// Reject incompatible protocol specifications before rebuilding any state.
	for _, export := range vol.GetExports() {
		if export.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NFS {
			return status.Errorf(codes.InvalidArgument, "filesystem reconcile supports only NFS exports")
		}
		params := ExportParams{
			VolumeID:       vol.GetVolumeId(),
			ProtocolParams: export.GetExportParams(),
			ACLEnabled:     export.GetAclEnabled(),
		}
		_, specErr := nfsProtocolSpec(params)
		if specErr != nil {
			return specErr
		}
	}
	err := s.filesystemOperation(
		ctx,
		vol.GetVolumeId(),
		vol.GetFilesystemAdoption(),
		vol.GetFence(),
		fenceGrant,
		vol.GetCapacityBytes(),
		vol.GetBackendParams(),
		false,
		func(pin backend.PinnedFilesystem, mark *fencingMark) error {
			if mark.LocalAttached || len(vol.GetExports()) > 0 {
				_, proxyErr := s.ensureFilesystemProxy(ctx, vol.GetVolumeId(), pin, mark)
				return proxyErr
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	for _, export := range vol.GetExports() {
		_, err = s.exportFilesystem(ctx, ExportParams{
			VolumeID:           vol.GetVolumeId(),
			ProtocolParams:     export.GetExportParams(),
			ACLEnabled:         export.GetAclEnabled(),
			Fence:              vol.GetFence(),
			FilesystemAdoption: vol.GetFilesystemAdoption(),
			CapacityBytes:      vol.GetCapacityBytes(),
			BackendParams:      vol.GetBackendParams(),
		}, true, export.GetAllowedInitiators())
		if err != nil {
			return err
		}
	}
	return nil
}
