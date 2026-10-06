package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"

	csisvc "github.com/isac322/pillar-csi/internal/csi"
)

func TestProbe_Node_RequiresNvmeFabrics(t *testing.T) {
	stateDir := t.TempDir()
	missingFabrics := filepath.Join(t.TempDir(), "nvme-fabrics")
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", nodeReadyFn(missingFabrics, stateDir))

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready without nvme-fabrics = true, want false")
	}
}

func TestProbe_Node_RequiresWritableStateDir(t *testing.T) {
	fabricsDevice := filepath.Join(t.TempDir(), "nvme-fabrics")
	if err := os.WriteFile(fabricsDevice, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write fake nvme-fabrics: %v", err)
	}
	stateDirFile := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(stateDirFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write non-directory state path: %v", err)
	}
	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", nodeReadyFn(fabricsDevice, stateDirFile))

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready with non-writable state dir = true, want false")
	}
}

func TestProbe_Node_CreatesMissingStateDir(t *testing.T) {
	fabricsDevice := filepath.Join(t.TempDir(), "nvme-fabrics")
	if err := os.WriteFile(fabricsDevice, []byte("ok"), 0o600); err != nil {
		t.Fatalf("write fake nvme-fabrics: %v", err)
	}
	parent := t.TempDir()
	missingStateDir := filepath.Join(parent, "never-created", "node")
	if _, statErr := os.Stat(missingStateDir); statErr == nil {
		t.Fatalf("test precondition violated: %q must not exist yet", missingStateDir)
	}

	server := csisvc.NewIdentityServerWithReadyFn(driverName, "test", nodeReadyFn(fabricsDevice, missingStateDir))
	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if !response.GetReady().GetValue() {
		t.Fatal("Probe must report ready on a fresh node by creating the state dir, got ready=false")
	}
	if _, statErr := os.Stat(missingStateDir); statErr != nil {
		t.Fatalf("state dir should have been auto-created: %v", statErr)
	}
}

type targetCheckingMounter struct {
	csisvc.Mounter
	mount func(source, target, fsType string, options []string) error
}

func (m *targetCheckingMounter) Mount(source, target, fsType string, options []string) error {
	return m.mount(source, target, fsType, options)
}

func TestMkdirMounter_NFSDirectoryTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "globalmount")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	const source = "192.0.2.8:/export/volume"
	options := []string{"nfsvers=4.2", "proto=tcp", "hard", "noatime"}
	wrapped := &targetCheckingMounter{
		mount: func(_, gotTarget, _ string, _ []string) error {
			st, err := os.Stat(gotTarget)
			if err != nil || !st.IsDir() {
				t.Fatalf("NFS mount target must be a directory: stat=%v, err=%v", st, err)
			}
			return syscall.EACCES
		},
	}

	err := (&mkdirMounter{wrapped: wrapped}).Mount(source, target, "nfs", options)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("NFS mount must reach the underlying mounter and retain its error: %v", err)
	}
}

func TestMkdirMounter_LocalBindTargetTypes(t *testing.T) {
	for _, sourceIsDir := range []bool{false, true} {
		name := "file"
		if sourceIsDir {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source")
			if sourceIsDir {
				if err := os.Mkdir(source, 0o750); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(source, []byte("staged-device"), 0o600); err != nil {
				t.Fatal(err)
			}
			assertLocalBindTarget(t, source, sourceIsDir)
		})
	}
}

func assertLocalBindTarget(t *testing.T, source string, sourceIsDir bool) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "publish", "target")
	wrapped := &targetCheckingMounter{
		mount: func(_, gotTarget, _ string, _ []string) error {
			st, err := os.Stat(gotTarget)
			if err != nil {
				t.Fatalf("bind target missing: %v", err)
			}
			if st.IsDir() != sourceIsDir || (!sourceIsDir && !st.Mode().IsRegular()) {
				t.Fatalf("bind target mode = %v, source directory = %t", st.Mode(), sourceIsDir)
			}
			return syscall.EACCES
		},
	}
	err := (&mkdirMounter{wrapped: wrapped}).Mount(source, target, "", []string{"bind"})
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("bind must preserve the underlying mount error: %v", err)
	}
}

// existingMounter records MountExisting calls of the wrapped Mounter and
// fails the test on any FormatAndMount: the preserve-original path must
// never reach the format path.
type existingMounter struct {
	csisvc.Mounter
	t     *testing.T
	calls []existingCall
	err   error
	check func(target string)
}

type existingCall struct {
	source, target, fsType string
	options                []string
}

func (m *existingMounter) MountExisting(
	_ context.Context, source, target, fsType string, options []string,
) error {
	if m.check != nil {
		m.check(target)
	}
	m.calls = append(m.calls, existingCall{source, target, fsType, options})
	return m.err
}

func (m *existingMounter) FormatAndMount(context.Context, string, string, string, []string, []string) error {
	m.t.Error("FormatAndMount reached through MountExisting")
	return nil
}

func TestMkdirMounter_MountExisting_CreatesTargetAndForwards(t *testing.T) {
	target := filepath.Join(t.TempDir(), "staging", "globalmount")
	wrapped := &existingMounter{t: t, err: syscall.EACCES}
	wrapped.check = func(gotTarget string) {
		st, err := os.Stat(gotTarget)
		if err != nil || !st.IsDir() {
			t.Fatalf("MountExisting target must exist as a directory before the forward: stat=%v, err=%v", st, err)
		}
	}
	options := []string{"noatime"}

	err := (&mkdirMounter{wrapped: wrapped}).MountExisting(t.Context(), "/dev/vg0/lv", target, "xfs", options)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("MountExisting must reach the wrapped mounter and retain its error: %v", err)
	}
	if len(wrapped.calls) != 1 {
		t.Fatalf("wrapped MountExisting calls = %d, want 1", len(wrapped.calls))
	}
	got := wrapped.calls[0]
	if got.source != "/dev/vg0/lv" || got.target != target || got.fsType != "xfs" ||
		len(got.options) != 1 || got.options[0] != "noatime" {
		t.Errorf("forwarded call = %+v, want identical arguments", got)
	}
}

func TestMkdirMounter_MountExisting_MkdirFailureNotForwarded(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := &existingMounter{t: t}

	target := filepath.Join(parent, "target")
	err := (&mkdirMounter{wrapped: wrapped}).MountExisting(t.Context(), "/dev/vg0/lv", target, "ext4", nil)
	if err == nil {
		t.Fatal("MountExisting under a regular-file parent succeeded, want the MkdirAll error")
	}
	if len(wrapped.calls) != 0 {
		t.Errorf("wrapped MountExisting called %d times after a failed MkdirAll, want 0", len(wrapped.calls))
	}
}
