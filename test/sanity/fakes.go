//go:build csi_sanity
// +build csi_sanity

package sanity

// fakes.go — in-memory implementations of the csi.Connector, csi.Mounter and
// csi.Resizer interfaces used by NodeServer.  csi-sanity exercises every
// node RPC; these fakes simulate the side effects without touching the
// kernel or the real filesystem block layer.
//
// FormatAndMount and Mount create the target path so that csi-sanity's
// NodeGetVolumeStats invocation can syscall.Statfs the directory.

import (
	"context"
	"fmt"
	"os"
	"sync"

	csidrv "github.com/isac322/pillar-csi/internal/csi"
)

// fakeConnector pretends every NVMe-oF subsystem is reachable and reports a
// stable device path so NodeStageVolume succeeds without nvme-cli.
type fakeConnector struct {
	devicePath string
}

func (c *fakeConnector) Connect(_ context.Context, _, _, _ string, _ csidrv.NVMeoFConnectOptions) error {
	return nil
}
func (c *fakeConnector) Disconnect(_ context.Context, _ string) error { return nil }

func (c *fakeConnector) GetDevicePath(_ context.Context, _ string) (string, error) {
	return c.devicePath, nil
}

// fakeMounter records mount state in-memory and materializes the target
// directory on disk so the surrounding stat/statfs paths succeed.
type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]bool
	source  map[string]string
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: map[string]bool{}, source: map[string]string{}}
}

func (m *fakeMounter) FormatAndMount(_ context.Context, source, target, _ string, _, _ []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = true
	m.source[target] = source
	return nil
}

func (m *fakeMounter) Mount(source, target, _ string, _ []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = true
	m.source[target] = source
	return nil
}

func (m *fakeMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, target)
	delete(m.source, target)
	return nil
}

// CheckMountReadable reports every path as readable: the fake never
// simulates a dead mount.
func (m *fakeMounter) CheckMountReadable(_ string) error {
	return nil
}

// CheckMountHealth reports every recorded mount as healthy: the fake never
// simulates a kernel-shutdown filesystem (issue #168).  An unmounted path
// is an inconclusive probe, matching the real mounter's contract.
func (m *fakeMounter) CheckMountHealth(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.mounted[target] {
		return fmt.Errorf("%q is not a mount point", target)
	}
	return nil
}

// HasOtherMounts reports no extra mounts: the fake records one mount per
// path and never creates bind mounts.
func (m *fakeMounter) HasOtherMounts(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.mounted[target] {
		return false, fmt.Errorf("%q is not a mount point", target)
	}
	return false, nil
}

// MountSource reports the recorded mount source, or errors for unmounted
// paths like the real mountinfo lookup.
func (m *fakeMounter) MountSource(target string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.mounted[target] {
		return "", fmt.Errorf("%q is not a mount point", target)
	}
	return m.source[target], nil
}

// MountEntryExists mirrors the mountinfo table: the fake records one entry
// per mount, so it equals the mounted map without any filesystem stat.
func (m *fakeMounter) MountEntryExists(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted[target], nil
}

// fakeResizer is a no-op Resizer for NodeExpandVolume.
type fakeResizer struct{}

func (fakeResizer) ResizeFS(_ context.Context, _, _ string) error { return nil }
