//go:build linux

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
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fitrimIoctl is FITRIM, _IOWR('X', 121, struct fstrim_range); x/sys/unix
// does not define it.
const fitrimIoctl = 0xc0185879

// fstrimRange is struct fstrim_range of <linux/fs.h>.
type fstrimRange struct {
	Start  uint64
	Len    uint64
	Minlen uint64
}

func platformTrimFuncs() (fitrimFunc, fsSizeFunc, error) {
	return linuxFITrim, linuxFSSize, nil
}

// linuxFITrim opens the directory path and issues FITRIM for
// [start, start+length) with minlen 0.  The opened directory must be on
// device major:minor — the mount the caller verified — so a mount replaced
// between the check and the open is never trimmed.
func linuxFITrim(path string, major, minor uint32, start, length uint64) (uint64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }() //nolint:errcheck // read-only directory fd

	var st unix.Stat_t
	err = unix.Fstat(fd, &st)
	if err != nil {
		return 0, fmt.Errorf("stat %q: %w", path, err)
	}
	if want := unix.Mkdev(major, minor); st.Dev != want {
		return 0, fmt.Errorf("%w: %q is on device %d:%d, the verified mount is %d:%d",
			errTrimUnverified, path, unix.Major(st.Dev), unix.Minor(st.Dev), major, minor)
	}

	r := fstrimRange{Start: start, Len: length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fitrimIoctl,
		uintptr(unsafe.Pointer(&r))) //nolint:gosec // G103: FITRIM takes a struct fstrim_range pointer
	if errno != 0 {
		return 0, fmt.Errorf("ioctl FITRIM on %q: %w", path, errno)
	}
	// The kernel replaces Len with the number of bytes it trimmed.
	return r.Len, nil
}

// linuxFSSize returns the size of the filesystem mounted at path.
func linuxFSSize(path string) (uint64, error) {
	var st unix.Statfs_t
	err := unix.Statfs(path, &st)
	if err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return st.Blocks * uint64(st.Bsize), nil //nolint:gosec // G115: Bsize is a positive block size
}
