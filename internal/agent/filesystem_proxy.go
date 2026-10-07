package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// FilesystemProxyMounter owns the agent's deterministic filesystem proxy mount.
// The interface is intentionally small so export and teardown behavior can be
// tested without requiring mount privileges.
type FilesystemProxyMounter interface {
	Mount(source, target string) error
	Unmount(target string) error
	Mounted(target string) (bool, error)
}

// filesystemProxyFilesystemMounter is implemented by the native Linux
// mounter for an unmounted ZFS dataset. It is optional so the public test
// seam remains the frozen bind-mount interface above.
type filesystemProxyFilesystemMounter interface {
	MountFilesystem(source, target, fsType string) error
}

func (s *Server) proxyMounter() FilesystemProxyMounter {
	if s.filesystemMounter != nil {
		return s.filesystemMounter
	}
	return newFilesystemProxyMounter()
}

func (s *Server) ensureFilesystemProxy(
	ctx context.Context, volumeID string, pin backend.PinnedFilesystem, mark *fencingMark,
) (string, error) {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return "", fmt.Errorf("ensure filesystem proxy: %w", ctxErr)
	}
	if pin == nil || mark == nil {
		return "", errors.New("filesystem proxy requires a pinned filesystem and fencing mark")
	}
	adoption := pin.Adoption()
	namespaceErr := s.checkFilesystemSourceNamespace(adoption)
	if namespaceErr != nil {
		return "", namespaceErr
	}
	fenceID := backend.FilesystemFenceID(adoption)
	if fenceID == "" {
		return "", errors.New("filesystem proxy requires a valid native filesystem identity")
	}
	target, err := s.filesystemProxyPath(fenceID)
	if err != nil {
		return "", err
	}
	mounter := s.proxyMounter()
	created, mounted, err := s.prepareFilesystemProxyTarget(volumeID, fenceID, target, mark, mounter)
	if err != nil {
		return "", err
	}
	if mounted {
		verifyErr := pin.VerifyMount(ctx, target)
		if verifyErr != nil {
			return "", fmt.Errorf(
				"existing filesystem proxy for %q failed native identity verification: %w", volumeID, verifyErr,
			)
		}
		mark.FilesystemProxyClaimed = true
		return target, nil
	}
	mountErr := mountPinnedFilesystemProxy(mounter, pin, target)
	if mountErr != nil {
		return "", filesystemProxyTargetFailure("mount filesystem proxy", volumeID, target, created, mountErr)
	}
	verifyErr := verifyNewFilesystemProxyMount(ctx, volumeID, target, pin, mounter, created)
	if verifyErr != nil {
		return "", verifyErr
	}
	mark.FilesystemProxyClaimed = true
	return target, nil
}

func verifyNewFilesystemProxyMount(
	ctx context.Context, volumeID, target string, pin backend.PinnedFilesystem,
	mounter FilesystemProxyMounter, created bool,
) error {
	verifyErr := pin.VerifyMount(ctx, target)
	if verifyErr == nil {
		return nil
	}
	rollbackErr := mounter.Unmount(target)
	if rollbackErr == nil && created {
		rollbackErr = os.Remove(target)
	}
	if rollbackErr != nil {
		return fmt.Errorf("verify filesystem proxy for %q: %w (rollback: %w)", volumeID, verifyErr, rollbackErr)
	}
	return fmt.Errorf("verify filesystem proxy for %q: %w", volumeID, verifyErr)
}

func (s *Server) claimFilesystemProxyTarget(volumeID, fenceID, target string, mark *fencingMark) (bool, error) {
	_, statErr := os.Lstat(target)
	if errors.Is(statErr, os.ErrNotExist) {
		claimErr := s.persistFilesystemProxyClaim(fenceID, mark)
		return false, claimErr
	}
	if statErr != nil {
		return false, fmt.Errorf("inspect filesystem proxy target for %q: %w", volumeID, statErr)
	}
	info, infoErr := os.Lstat(target)
	if infoErr != nil {
		return false, fmt.Errorf("inspect filesystem proxy target for %q: %w", volumeID, infoErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("filesystem proxy target for %q is not a safe directory", volumeID)
	}
	return true, nil
}

func (s *Server) prepareFilesystemProxyTarget(
	volumeID, fenceID, target string, mark *fencingMark, mounter FilesystemProxyMounter,
) (created, mounted bool, err error) {
	ownsTarget := mark.FilesystemProxyClaimed || mark.LocalAttached || mark.FilesystemExported
	targetExists, claimErr := s.claimFilesystemProxyTarget(volumeID, fenceID, target, mark)
	if claimErr != nil {
		return false, false, claimErr
	}
	created, targetErr := ensureProxyTarget(target)
	if targetErr != nil {
		return false, false, targetErr
	}
	mounted, mountErr := mounter.Mounted(target)
	if mountErr != nil {
		return false, false, filesystemProxyTargetFailure(
			"check existing filesystem proxy", volumeID, target, created, mountErr,
		)
	}
	if targetExists && !ownsTarget {
		return false, false, rejectUnownedFilesystemProxyTarget(volumeID, target, mounted)
	}
	if !mounted && !created {
		empty, emptyErr := directoryEmpty(target)
		if emptyErr != nil {
			return false, false, fmt.Errorf("inspect filesystem proxy target for %q: %w", volumeID, emptyErr)
		}
		if !empty {
			return false, false, fmt.Errorf("filesystem proxy target for %q is not an empty unmounted directory", volumeID)
		}
	}
	return created, mounted, nil
}

func rejectUnownedFilesystemProxyTarget(volumeID, target string, mounted bool) error {
	if mounted {
		return fmt.Errorf("filesystem proxy target for %q is mounted without durable ownership", volumeID)
	}
	empty, err := directoryEmpty(target)
	if err != nil {
		return fmt.Errorf("inspect unowned filesystem proxy target for %q: %w", volumeID, err)
	}
	if empty {
		return fmt.Errorf("filesystem proxy target for %q exists without durable ownership", volumeID)
	}
	return fmt.Errorf("filesystem proxy target for %q is not an empty unmounted directory", volumeID)
}

func filesystemProxyTargetFailure(action, volumeID, target string, created bool, cause error) error {
	if created {
		removeErr := os.Remove(target)
		if removeErr != nil {
			return fmt.Errorf("%s for %q: %w (remove target: %w)", action, volumeID, cause, removeErr)
		}
	}
	return fmt.Errorf("%s for %q: %w", action, volumeID, cause)
}

func mountPinnedFilesystemProxy(mounter FilesystemProxyMounter, pin backend.PinnedFilesystem, target string) error {
	adoption := pin.Adoption()
	if adoption.GetKind() == filesystemAdoptionKindZFSDataset && adoption.GetHostPath() == "" {
		native, ok := mounter.(filesystemProxyFilesystemMounter)
		switch {
		case !ok:
			return errors.New("filesystem proxy mounter cannot mount an unmounted ZFS dataset")
		case adoption.GetCanonicalSource() == "":
			return errors.New("unmounted ZFS filesystem adoption has no native dataset source")
		default:
			mountErr := native.MountFilesystem(adoption.GetCanonicalSource(), target, "zfs")
			if mountErr != nil {
				return fmt.Errorf("mount native ZFS filesystem proxy: %w", mountErr)
			}
			return nil
		}
	}
	source := pin.MountSource()
	sourceErr := validatePinnedSource(source)
	if sourceErr != nil {
		return sourceErr
	}
	mountErr := mounter.Mount(source, target)
	if mountErr != nil {
		return fmt.Errorf("bind mount pinned filesystem proxy: %w", mountErr)
	}
	return nil
}

func (s *Server) persistFilesystemProxyClaim(fenceID string, mark *fencingMark) error {
	if mark == nil {
		return errors.New("filesystem proxy claim requires a fencing mark")
	}
	mark.FilesystemProxyClaimed = true
	writeErr := s.writeFencingMark(fenceID, *mark)
	if writeErr != nil {
		mark.FilesystemProxyClaimed = false
		return fmt.Errorf("persist filesystem proxy claim: %w", writeErr)
	}
	return nil
}

func (s *Server) removeFilesystemProxy(
	ctx context.Context, volumeID string, adoption *agentv1.FilesystemAdoption, mark *fencingMark,
) error {
	ctxErr := ctx.Err()
	if ctxErr != nil {
		return fmt.Errorf("remove filesystem proxy: %w", ctxErr)
	}
	namespaceErr := s.checkFilesystemSourceNamespace(adoption)
	if namespaceErr != nil {
		return namespaceErr
	}
	if mark == nil {
		return errors.New("filesystem proxy removal requires a fencing mark")
	}
	fenceID := backend.FilesystemFenceID(adoption)
	if fenceID == "" {
		return errors.New("filesystem proxy removal requires a valid native filesystem identity")
	}
	target, err := s.filesystemProxyPath(fenceID)
	if err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		mark.FilesystemProxyClaimed = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect filesystem proxy target for %q: %w", volumeID, err)
	}
	targetErr := validateFilesystemProxyRemovalTarget(volumeID, info, mark)
	if targetErr != nil {
		return targetErr
	}
	mounter := s.proxyMounter()
	mounted, err := mounter.Mounted(target)
	if err != nil {
		return fmt.Errorf("check filesystem proxy mount for %q: %w", volumeID, err)
	}
	if mounted {
		unmountErr := mounter.Unmount(target)
		if unmountErr != nil {
			return fmt.Errorf("unmount filesystem proxy for %q: %w", volumeID, unmountErr)
		}
	}
	removeErr := os.Remove(target)
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove filesystem proxy target for %q: %w", volumeID, removeErr)
	}
	mark.FilesystemProxyClaimed = false
	return nil
}

func validateFilesystemProxyRemovalTarget(volumeID string, info os.FileInfo, mark *fencingMark) error {
	if !mark.FilesystemProxyClaimed && !mark.LocalAttached && !mark.FilesystemExported {
		return fmt.Errorf("filesystem proxy target for %q has no durable ownership", volumeID)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("filesystem proxy target for %q is a symlink", volumeID)
	}
	if !info.IsDir() {
		return fmt.Errorf("filesystem proxy target for %q is not a directory", volumeID)
	}
	return nil
}

func (s *Server) filesystemProxyPath(fenceID string) (string, error) {
	if !validFilesystemProxyFenceKey(fenceID) {
		return "", errors.New("invalid filesystem proxy fence key")
	}
	root := s.filesystemProxyRoot
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return "", errors.New("filesystem proxy root must be a dedicated absolute clean directory")
	}
	rootErr := rejectSymlinkPath(root, true)
	if rootErr != nil {
		return "", fmt.Errorf("invalid filesystem proxy root: %w", rootErr)
	}
	target := filepath.Join(root, fenceID)
	parentErr := rejectSymlinkPath(filepath.Dir(target), true)
	if parentErr != nil {
		return "", fmt.Errorf("invalid filesystem proxy parent: %w", parentErr)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("filesystem proxy target escapes its root")
	}
	return target, nil
}

func validFilesystemProxyFenceKey(fenceID string) bool {
	leaf, hasPrefix := strings.CutPrefix(fenceID, "filesystem/")
	if !hasPrefix || len(leaf) != 64 || strings.ContainsAny(leaf, `/\`) {
		return false
	}
	for _, c := range leaf {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func ensureProxyTarget(target string) (bool, error) {
	parent := filepath.Dir(target)
	parentErr := ensureDirectoryPath(parent)
	if parentErr != nil {
		return false, parentErr
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		createErr := os.Mkdir(target, filesystemProxyTargetMode)
		if createErr != nil {
			return false, fmt.Errorf("create filesystem proxy target: %w", createErr)
		}
		modeErr := finishCreatedProxyDirectory(target, filesystemProxyTargetMode)
		if modeErr != nil {
			return false, modeErr
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect filesystem proxy target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("filesystem proxy target must not be a symlink")
	}
	if !info.IsDir() {
		return false, errors.New("filesystem proxy target must be a directory")
	}
	return false, nil
}

func ensureDirectoryPath(path string) error {
	pathErr := rejectSymlinkPath(path, true)
	if pathErr != nil {
		return pathErr
	}
	clean := filepath.Clean(path)
	current := string(filepath.Separator)
	for part := range strings.SplitSeq(strings.TrimPrefix(clean, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			//nolint:gosec // G301: NFS clients need execute traversal; mode is re-applied below past umask.
			createErr := os.Mkdir(current, 0o755)
			if createErr != nil {
				return fmt.Errorf("create filesystem proxy directory %q: %w", current, createErr)
			}
			modeErr := finishCreatedProxyDirectory(current, filesystemProxyParentMode)
			if modeErr != nil {
				return modeErr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect filesystem proxy directory %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("filesystem proxy path component %q is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("filesystem proxy path component %q is not a directory", current)
		}
	}
	return nil
}

const (
	// Driver-created intermediates use this mode so NFS clients, including
	// root-squashed ones, can traverse them. They contain only protected mount
	// targets.
	filesystemProxyParentMode os.FileMode = 0o755
	// The unmounted proxy target uses this private mode.
	filesystemProxyTargetMode os.FileMode = 0o750
)

// finishCreatedProxyDirectory applies mode to a directory this agent just
// created, because mkdir(2) is subject to the process umask. Pre-existing
// directories are never passed here, so foreign modes stay untouched. On
// failure the empty directory is removed so a retry recreates it instead of
// adopting a directory with the wrong mode.
func finishCreatedProxyDirectory(path string, mode os.FileMode) error {
	modeErr := setCreatedProxyDirectoryMode(path, mode)
	if modeErr == nil {
		return nil
	}
	removeErr := os.Remove(path)
	if removeErr != nil {
		return fmt.Errorf("%w; remove created filesystem proxy directory %q: %w", modeErr, path, removeErr)
	}
	return modeErr
}

func setCreatedProxyDirectoryMode(path string, mode os.FileMode) (err error) {
	//nolint:gosec // G304: validated driver-owned proxy path; O_NOFOLLOW rejects symlinks.
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open created filesystem proxy directory %q: %w", path, err)
	}
	defer func() {
		closeErr := dir.Close()
		if closeErr != nil {
			err = fmt.Errorf("%w; close created filesystem proxy directory %q: %w", err, path, closeErr)
		}
	}()
	chmodErr := dir.Chmod(mode)
	if chmodErr != nil {
		return fmt.Errorf("chmod created filesystem proxy directory %q: %w", path, chmodErr)
	}
	info, statErr := dir.Stat()
	if statErr != nil {
		return fmt.Errorf("inspect created filesystem proxy directory %q: %w", path, statErr)
	}
	if !info.IsDir() || info.Mode().Perm() != mode {
		return fmt.Errorf("created filesystem proxy directory %q mode = %v, want directory %04o", path, info.Mode(), mode)
	}
	return nil
}

func rejectSymlinkPath(path string, allowMissing bool) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return errors.New("path must be absolute")
	}
	current := string(filepath.Separator)
	for part := range strings.SplitSeq(strings.TrimPrefix(clean, current), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if allowMissing {
				return nil
			}
			return fmt.Errorf("path component %q does not exist", current)
		}
		if err != nil {
			return fmt.Errorf("inspect path component %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

func directoryEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, fmt.Errorf("read directory %q: %w", path, err)
	}
	return len(entries) == 0, nil
}

func validatePinnedSource(source string) error {
	if source == "" {
		return errors.New("pinned filesystem has no mount source")
	}
	if descriptor, hasPrefix := strings.CutPrefix(source, "/proc/self/fd/"); hasPrefix {
		fd, err := strconv.Atoi(descriptor)
		if err != nil || fd < 0 {
			return errors.New("pinned filesystem mount source has an invalid file descriptor")
		}
		_, statErr := os.Stat(source)
		if statErr != nil {
			return fmt.Errorf("pinned filesystem file descriptor is not open: %w", statErr)
		}
		return nil
	}
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return errors.New("pinned filesystem mount source must be an absolute clean path")
	}
	return rejectSymlinkPath(source, false)
}
