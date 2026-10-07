package agent

import (
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// checkFilesystemSourceNamespace reserves only the adoption proxy subtree.
// A broad allow-root may still admit unrelated sources. An unmounted ZFS
// source has no original host mount path and remains eligible for an owned
// native mount without changing its durable descriptor.
func (s *Server) checkFilesystemSourceNamespace(a *agentv1.FilesystemAdoption) error {
	key, err := filesystemFenceKey(a)
	if err != nil {
		return err
	}
	target, err := s.filesystemProxyPath(key)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "filesystem proxy namespace is unavailable: %v", err)
	}
	source := backend.FilesystemMountSource(a)
	if source == "" && a.GetKind() == filesystemAdoptionKindZFSDataset {
		return nil
	}
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return status.Error(codes.InvalidArgument, "filesystem source host path must be canonical and absolute")
	}
	namespace := filepath.Dir(target)
	if filesystemPathContains(namespace, source) || filesystemPathContains(source, namespace) {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return status.Error(codes.FailedPrecondition, "filesystem source overlaps the reserved adoption proxy namespace")
	}
	return nil
}

func filesystemPathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil &&
		!filepath.IsAbs(relative) &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
