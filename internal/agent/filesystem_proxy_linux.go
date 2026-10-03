//go:build linux

package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type nativeFilesystemProxyMounter struct{}

type filesystemMountInfo struct {
	mounted  bool
	writable bool
}

func newFilesystemProxyMounter() FilesystemProxyMounter {
	return nativeFilesystemProxyMounter{}
}

// Mount performs a native bind mount. The source is expected to be a
// pin-backed /proc/self/fd path, never a re-resolved adoption descriptor.
func (nativeFilesystemProxyMounter) Mount(source, target string) error {
	err := unix.Mount(source, target, "", unix.MS_BIND, "")
	if err != nil {
		return fmt.Errorf("bind mount %s → %s: %w", source, target, err)
	}
	// A bind mount inherits the source's per-mount read-only flag. Remounting
	// the owned bind clears only that flag on the proxy, not on the source.
	err = unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT, "")
	if err != nil {
		return cleanupNativeProxyMount(
			target,
			fmt.Errorf("remount writable bind %s → %s: %w", source, target, err),
		)
	}
	info, err := readFilesystemMountInfo(target)
	if err != nil {
		return cleanupNativeProxyMount(target, fmt.Errorf("validate bind mount %s: %w", target, err))
	}
	if !info.mounted || !info.writable {
		return cleanupNativeProxyMount(
			target,
			fmt.Errorf("validate bind mount %s: resulting mount is not writable", target),
		)
	}
	return nil
}

func cleanupNativeProxyMount(target string, cause error) error {
	err := unix.Unmount(target, 0)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("unmount partial bind %s: %w", target, err))
	}
	return cause
}

// MountFilesystem mounts a native filesystem source (currently ZFS) directly
// at target. It is used only when an adopted dataset has no existing host
// mount, so binding a path would incorrectly mutate or depend on a pathname.
func (nativeFilesystemProxyMounter) MountFilesystem(source, target, fsType string) error {
	err := unix.Mount(source, target, fsType, 0, "")
	if err != nil {
		return fmt.Errorf("mount %s filesystem %s → %s: %w", fsType, source, target, err)
	}
	return nil
}

func (nativeFilesystemProxyMounter) Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if err != nil {
		return fmt.Errorf("unmount %s: %w", target, err)
	}
	return nil
}

func (nativeFilesystemProxyMounter) Mounted(target string) (bool, error) {
	_, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lstat mount target %s: %w", target, err)
	}
	info, err := readFilesystemMountInfo(target)
	if err != nil {
		return false, err
	}
	return info.mounted, nil
}

func readFilesystemMountInfo(target string) (info filesystemMountInfo, retErr error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return filesystemMountInfo{}, fmt.Errorf("open mountinfo: %w", err)
	}
	defer func() {
		closeErr := file.Close()
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close mountinfo: %w", closeErr))
		}
	}()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), " - ", 2)
		if len(fields) != 2 {
			continue
		}
		pre := strings.Fields(fields[0])
		if len(pre) < 6 {
			continue
		}
		mountPoint, err := unescapeMountInfoPath(pre[4])
		if err != nil || mountPoint != target {
			continue
		}
		return filesystemMountInfo{
			mounted:  true,
			writable: mountInfoOptionsWritable(pre[5]),
		}, nil
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		return filesystemMountInfo{}, fmt.Errorf("read mountinfo: %w", scanErr)
	}
	return filesystemMountInfo{}, nil
}

func mountInfoOptionsWritable(options string) bool {
	for option := range strings.SplitSeq(options, ",") {
		if option == "rw" {
			return true
		}
	}
	return false
}

func unescapeMountInfoPath(value string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			b.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", errors.New("truncated mountinfo escape")
		}
		code, err := strconv.ParseUint(value[i+1:i+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("parse mountinfo escape %q: %w", value[i:i+4], err)
		}
		b.WriteByte(byte(code))
		i += 3
	}
	return b.String(), nil
}
