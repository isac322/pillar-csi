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
