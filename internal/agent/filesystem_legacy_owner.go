package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hasActiveZFSFilesystemOwner gates block-backend metadata work. With no live
// filesystem adoption, old zvol operations issue no new physical ZFS queries.
// The durable descriptor, not a dataset/PVC naming convention, classifies the
// source. This scan retains no FD or native-identity cache between operations.
func (s *Server) hasActiveZFSFilesystemOwner(ctx context.Context) (active bool, retErr error) {
	root, err := os.OpenRoot(s.resolvedDrainStateDir())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, status.Errorf(codes.Internal, "open filesystem owner state: %v", err)
	}
	defer func() {
		closeErr := root.Close()
		if closeErr != nil {
			retErr = status.Errorf(codes.Internal, "close filesystem owner state: %v", errors.Join(retErr, closeErr))
		}
	}()
	dir, err := root.Open(fencingDirName)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, status.Errorf(codes.Internal, "open filesystem owner marks: %v", err)
	}
	entries, readErr := dir.ReadDir(-1)
	err = errors.Join(readErr, dir.Close())
	if err != nil {
		return false, status.Errorf(codes.Internal, "read filesystem owner marks: %v", err)
	}
	prefix := strings.TrimSuffix(fencingFilename("filesystem/"), fencingSuffix)
	for _, entry := range entries {
		ctxErr := ctx.Err()
		if ctxErr != nil {
			contextStatus := status.FromContextError(ctxErr)
			return false, status.Errorf(contextStatus.Code(), "%s", contextStatus.Message())
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), fencingSuffix) {
			continue
		}
		activeOwner, ownerErr := activeZFSFilesystemOwnerMark(root, entry.Name())
		if ownerErr != nil || activeOwner {
			return activeOwner, ownerErr
		}
	}
	return false, nil
}

func activeZFSFilesystemOwnerMark(root *os.Root, filename string) (bool, error) {
	data, err := root.ReadFile(path.Join(fencingDirName, filename))
	if err != nil {
		return false, status.Errorf(codes.Internal, "read filesystem owner mark: %v", err)
	}
	var mark fencingMark
	err = json.Unmarshal(data, &mark)
	if err != nil || mark.VolumeUID == "" {
		return false, status.Errorf(codes.Internal, "invalid filesystem owner mark: %v", err)
	}
	if mark.Filesystem == nil || mark.Ended || mark.Filesystem.GetKind() != filesystemAdoptionKindZFSDataset {
		return false, nil
	}
	key, err := filesystemFenceKey(mark.Filesystem)
	if err != nil || fencingFilename(key) != filename {
		return false, status.Errorf(codes.Internal, "filesystem owner mark has inconsistent native identity")
	}
	return true, nil
}
