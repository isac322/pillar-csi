package agent

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

// ValidateFilesystemExport is the read-only NFS activation guard. Legacy
// provisioned exports have no native ownership hint and keep their existing
// recovery path. Adopted exports must prove their durable lifecycle, exact
// quota, placement, and mounted native identity before any admission is added.
// It deliberately does not acquire a fencing lock: Put invokes the guard while
// the calling RPC already holds that lock. Marks are atomically replaced.
func (s *Server) ValidateFilesystemExport(ctx context.Context, e nfs.Export) (retErr error) {
	if e.SourceKey == "" && e.FenceUID == "" {
		return nil
	}
	if e.SourceKey == "" || e.FenceUID == "" {
		return status.Errorf(codes.FailedPrecondition, "incomplete adopted NFS ownership hint")
	}
	contextErr := ctx.Err()
	if contextErr != nil {
		contextStatus := status.FromContextError(contextErr)
		return status.Errorf(contextStatus.Code(), "%s", contextStatus.Message())
	}
	stored, exists, err := s.readFencingMark(e.SourceKey)
	if err != nil {
		return err
	}
	key, err := validateFilesystemExportMark(e, stored, exists)
	if err != nil {
		return err
	}
	expectedPath, pathErr := s.filesystemProxyPath(key)
	if pathErr != nil {
		return pathErr
	}
	if e.Path != expectedPath {
		return status.Errorf(codes.FailedPrecondition, "adopted NFS export is not the canonical owned proxy")
	}
	err = s.checkFilesystemSourceNamespace(stored.Filesystem)
	if err != nil {
		return err
	}
	// The NFS manager's durable desired policy is authoritative for clients and
	// ACL mode. The native mark's publication fields are conservative teardown
	// bounds, not a second admission policy; checking them here would race a
	// narrowed exact reconcile against another export's activation.
	pin, err := s.pinFilesystemExport(ctx, e, stored)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := pin.Close()
		if closeErr != nil {
			retErr = status.Errorf(codes.Internal, "adopted NFS pin close for %q: %v", e.VolumeID, errors.Join(retErr, closeErr))
		}
	}()
	return verifyFilesystemExportPin(ctx, e, stored, pin)
}

func (s *Server) pinFilesystemExport(
	ctx context.Context,
	e nfs.Export,
	stored fencingMark,
) (backend.PinnedFilesystem, error) {
	b, err := s.backendForType(e.VolumeID, filesystemBackendType(stored.Filesystem))
	if err != nil {
		return nil, err
	}
	if b.Type() != filesystemBackendType(stored.Filesystem) || b.Layout() != *stored.FilesystemLayout {
		return nil, status.Errorf(codes.FailedPrecondition, "adopted NFS backend placement changed")
	}
	importer, ok := b.(backend.FilesystemImporter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "adopted NFS backend cannot pin native filesystem")
	}
	pin, err := importer.ImportFilesystem(
		ctx, e.VolumeID, stored.FilesystemCapacity, stored.Filesystem, *stored.FilesystemLayout,
	)
	if err != nil {
		return nil, importVolumeError(err)
	}
	if pin == nil {
		return nil, status.Errorf(codes.Internal, "adopted NFS importer returned no native pin")
	}
	return pin, nil
}

func validateFilesystemExportMark(e nfs.Export, stored fencingMark, exists bool) (string, error) {
	if !exists || stored.Ended || stored.VolumeUID != e.FenceUID ||
		stored.FilesystemVolumeID != e.VolumeID || !stored.FilesystemExported {
		return "", status.Errorf(codes.FailedPrecondition, "adopted NFS export no longer owns its native resource")
	}
	key, err := filesystemFenceKey(stored.Filesystem)
	if err != nil || key != e.SourceKey || stored.FilesystemCapacity <= 0 || stored.FilesystemLayout == nil {
		return "", status.Errorf(codes.FailedPrecondition,
			"adopted NFS export has invalid durable native identity or quota")
	}
	return key, nil
}

func verifyFilesystemExportPin(
	ctx context.Context, e nfs.Export, stored fencingMark, pin backend.PinnedFilesystem,
) error {
	if !proto.Equal(pin.Adoption(), stored.Filesystem) || pin.CapacityBytes() != stored.FilesystemCapacity {
		return status.Errorf(codes.FailedPrecondition, "adopted NFS native identity or exact capacity changed")
	}
	err := pin.VerifyMount(ctx, e.Path)
	if err != nil {
		return importVolumeError(err)
	}
	return nil
}
