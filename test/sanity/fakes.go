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
	"os"
	"sync"
)

// fakeConnector pretends every NVMe-oF subsystem is reachable and reports a
// stable device path so NodeStageVolume succeeds without nvme-cli.
type fakeConnector struct {
	devicePath string
}

func (c *fakeConnector) Connect(_ context.Context, _, _, _ string) error { return nil }
func (c *fakeConnector) Disconnect(_ context.Context, _ string) error    { return nil }

func (c *fakeConnector) GetDevicePath(_ context.Context, _ string) (string, error) {
	return c.devicePath, nil
}

// fakeMounter records mount state in-memory and materializes the target
// directory on disk so the surrounding stat/statfs paths succeed.
type fakeMounter struct {
	mu      sync.Mutex
	mounted map[string]bool
}

func newFakeMounter() *fakeMounter {
	return &fakeMounter{mounted: map[string]bool{}}
}

func (m *fakeMounter) FormatAndMount(_, target, _ string, _ []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = true
	return nil
}

func (m *fakeMounter) Mount(_, target, _ string, _ []string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted[target] = true
	return nil
}

func (m *fakeMounter) Unmount(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mounted, target)
	return nil
}

func (m *fakeMounter) IsMounted(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted[target], nil
}

// fakeResizer is a no-op Resizer for NodeExpandVolume.
type fakeResizer struct{}

func (fakeResizer) ResizeFS(_, _ string) error { return nil }
