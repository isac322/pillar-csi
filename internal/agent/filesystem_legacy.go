package agent

import (
	"context"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// Adopted ZFS sources are strict direct children of the configured parent.
// Their native leaf therefore maps to the pre-existing managed dataset fence.
func legacyFilesystemVolumeID(a *agentv1.FilesystemAdoption) (string, error) {
	if a.GetKind() != filesystemAdoptionKindZFSDataset {
		return "", nil
	}
	source := a.GetCanonicalSource()
	parts := strings.Split(source, "/")
	if len(parts) < 2 || parts[0] == "" || path.Clean(source) != source || strings.HasPrefix(source, "/") {
		return "", status.Errorf(codes.InvalidArgument, "invalid native dataset source")
	}
	return parts[0] + "/" + parts[len(parts)-1], nil
}

func (s *Server) checkLegacyFilesystemOwner(a *agentv1.FilesystemAdoption) error {
	volumeID, err := legacyFilesystemVolumeID(a)
	if err != nil || volumeID == "" {
		return err
	}
	mark, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		return err
	}
	if exists && !mark.Ended && mark.Filesystem == nil && !mark.LegacyRevokeOnly {
		return status.Errorf(codes.FailedPrecondition, "existing filesystem is owned by a managed dataset lifecycle")
	}
	return nil
}

type existingFilesystemIdentity interface {
	ExistingFilesystemIdentity(context.Context, string) (*agentv1.FilesystemAdoption, error)
}

// guardLegacyFilesystem runs under the legacy name fence before its mark is
// advanced. A refused old operation cannot leave a phantom managed owner that
// prevents the existing native owner from retrying its import.
func (s *Server) guardLegacyFilesystem(ctx context.Context, b backend.VolumeBackend, volumeID string) error {
	if b == nil {
		return nil
	}
	switch b.Type() {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET:
	case agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL:
		active, err := s.hasActiveZFSFilesystemOwner(ctx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
	default:
		return nil
	}
	inspector, ok := b.(existingFilesystemIdentity)
	if !ok {
		return status.Errorf(codes.Unimplemented, "managed filesystem backend cannot resolve existing native identity")
	}
	identity, err := inspector.ExistingFilesystemIdentity(ctx, volumeID)
	if err != nil {
		return importVolumeError(err)
	}
	if identity == nil {
		return nil
	}
	key, err := filesystemFenceKey(identity)
	if err != nil {
		return err
	}
	mark, exists, err := s.readFencingMark(key)
	if err != nil {
		return err
	}
	if exists && !mark.Ended && mark.Filesystem != nil {
		return status.Errorf(
			codes.FailedPrecondition, "managed dataset operation conflicts with an active adopted native owner",
		)
	}
	return nil
}

func (s *Server) fencedLegacyFilesystem(
	ctx context.Context,
	volumeID string,
	token *agentv1.FencingToken,
	op fenceOp,
	mutate func() error,
) error {
	b, err := s.backendForType(volumeID, agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET)
	if err != nil {
		return err
	}
	return s.fencedBackend(ctx, volumeID, token, op, b, mutate)
}
