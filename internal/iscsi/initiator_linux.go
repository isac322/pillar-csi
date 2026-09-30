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

package iscsi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// NewInitiator creates an initiator.  Call Start before any other method.
func NewInitiator(opts Options) (*Initiator, error) {
	opts = opts.withDefaults()
	return newInitiator(opts, deps{
		openKernel: func(handle uint64) (kernelConn, error) {
			return openNetlink(opts.NetlinkNetnsPath, handle, opts.Logger.WithName("iscsi-netlink"))
		},
		dial:     dialTCP,
		devNodes: mknodDevNodes{},
	})
}

// Available returns nil when the kernel software iSCSI transport
// (iscsi_tcp) is registered.
func Available(sysfsRoot string) error {
	if sysfsRoot == "" {
		sysfsRoot = DefaultSysfsRoot
	}
	p := filepath.Join(sysfsRoot, "class", "iscsi_transport", "tcp")
	_, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("iSCSI initiator unavailable: %s (load kernel module iscsi_tcp): %w", p, err)
	}
	return nil
}

type fileEndpoint struct{ f *os.File }

func (e fileEndpoint) fd() uintptr { return e.f.Fd() }

func (e fileEndpoint) Close() error { return e.f.Close() } //nolint:wrapcheck // wrapped by callers

// dialTCP connects to the portal and detaches the socket from the Go
// runtime: the duplicate descriptor from File() is owned by the kernel
// iSCSI connection after BIND_CONN, and closing the original net.Conn does
// not shut the socket down because the duplicate still references it.
func dialTCP(ctx context.Context, p Portal) (endpoint, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", p.String())
	if err != nil {
		return nil, fmt.Errorf("connect to portal %s: %w", p, err)
	}
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("connect to portal %s: %w", p,
			errors.Join(fmt.Errorf("unexpected connection type %T", c), c.Close()))
	}
	f, err := tc.File()
	closeErr := tc.Close()
	if err != nil {
		return nil, fmt.Errorf("detach socket for portal %s: %w", p, err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("detach socket for portal %s: close original descriptor: %w", p,
			errors.Join(closeErr, f.Close()))
	}
	return fileEndpoint{f: f}, nil
}

// mknodDevNodes creates block device nodes.  Nested-container nodes (Kind)
// do not get devtmpfs entries for devices that appear after the container
// started, so the node is created from the sysfs major:minor.
type mknodDevNodes struct{}

func isBlockDevice(st *unix.Stat_t, dev uint64) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFBLK && st.Rdev == dev
}

func (mknodDevNodes) ensure(path string, major, minor uint32) (bool, error) {
	want := unix.Mkdev(major, minor)
	var st unix.Stat_t
	err := unix.Stat(path, &st)
	switch {
	case err == nil:
		if isBlockDevice(&st, want) {
			return false, nil
		}
		// Stale node from a previous device with the same name.
		rmErr := os.Remove(path)
		if rmErr != nil {
			return false, fmt.Errorf("remove stale node %s: %w", path, rmErr)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	// Mkdev packs a 32-bit major and minor into the kernel's dev_t layout,
	// which mknod(2) takes as an int.
	err = unix.Mknod(path, unix.S_IFBLK|0o660, int(want)) //nolint:gosec // G115: dev_t from 32-bit major/minor.
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EEXIST) {
		// devtmpfs/udev created it concurrently; verify.
		statErr := unix.Stat(path, &st)
		if statErr == nil && isBlockDevice(&st, want) {
			return false, nil
		}
	}
	return false, fmt.Errorf("mknod %s: %w", path, err)
}

func (mknodDevNodes) usable(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENOMEDIUM):
		return fmt.Sprintf("block device %s cannot be opened yet: %v", path, err), nil
	case err != nil:
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	size, seekErr := unix.Seek(fd, 0, io.SeekEnd)
	closeErr := unix.Close(fd)
	if seekErr != nil {
		return "", fmt.Errorf("read capacity of %s: %w", path, seekErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", path, closeErr)
	}
	if size == 0 {
		return "block device " + path + " reports zero capacity", nil
	}
	return "", nil
}

// flush fsyncs the block device: the kernel writes back the device's page
// cache and, for a disk with a volatile write cache, sends SYNCHRONIZE
// CACHE.  A device that cannot be opened is reported through missing.
func (mknodDevNodes) flush(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENXIO), errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENOMEDIUM):
		return fmt.Sprintf("block device %s cannot be opened: %v", path, err), nil
	case err != nil:
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	syncErr := unix.Fsync(fd)
	closeErr := unix.Close(fd)
	if syncErr != nil {
		return "", fmt.Errorf("fsync %s: %w", path, syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close %s: %w", path, closeErr)
	}
	return "", nil
}
