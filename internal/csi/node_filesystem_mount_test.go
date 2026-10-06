//go:build linux

package csi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A durable agent proxy directory can survive the mount it used to contain.
// Exercise the public stage RPC with real paths and the production mounter:
// that directory must never be published as an empty replacement filesystem.
func TestNodeStageFilesystemRejectsMissingOwnedProxyMount(t *testing.T) {
	for _, kind := range []string{"missing", "empty-directory", "foreign-directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, proxy, target := rejectedFilesystemProxyPaths(t, kind)
			mounter := NewKubeMounter()
			registerFilesystemStageCleanup(t, target)
			node := NewNodeServer("storage-node", nil, mounter).
				WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(filepath.Join(root, "state"))
			const adoption = `{"kind":"zfs-dataset","canonicalSource":"tank/existing",` +
				`"resourceId":"12345","filesystemType":"zfs"}`
			req := &csipb.NodeStageVolumeRequest{
				VolumeId: "agent/nfs/zfs-dataset/tank/native/lifecycle", StagingTargetPath: target,
				VolumeCapability: &csipb.VolumeCapability{
					AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
					AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				},
				PublishContext: map[string]string{
					PublishContextKeyFilesystemAdoption:  adoption,
					PublishContextKeyFilesystemCapacity:  "1048576",
					PublishContextKeyFilesystemLayout:    `{"zfs":{"pool":"tank","parentDataset":""}}`,
					PublishContextKeyFilesystemLocalNode: "storage-node",
					PublishContextKeyFilesystemProxyPath: proxy,
				},
			}
			_, stageErr := node.NodeStageVolume(context.Background(), req)
			if status.Code(stageErr) != codes.FailedPrecondition {
				t.Fatalf("stage without an owned proxy mount = %v, want FailedPrecondition for kubelet retry", stageErr)
			}
			assertFilesystemStageRejected(t, node, req)
		})
	}
}

func rejectedFilesystemProxyPaths(t *testing.T, kind string) (root, proxy, target string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy = filepath.Join(root, "proxy")
	switch kind {
	case "empty-directory", "foreign-directory":
		if mkdirErr := os.Mkdir(proxy, 0o750); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if kind == "foreign-directory" {
			path := filepath.Join(proxy, "foreign-data")
			if writeErr := os.WriteFile(path, []byte("must remain private"), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
	case "symlink":
		if symlinkErr := os.Symlink(root, proxy); symlinkErr != nil {
			t.Fatal(symlinkErr)
		}
	}
	target = filepath.Join(root, "stage")
	if mkdirErr := os.Mkdir(target, 0o750); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	return root, proxy, target
}

func registerFilesystemStageCleanup(t *testing.T, target string) {
	t.Helper()
	t.Cleanup(func() {
		mounts, err := readMountInfoFile(procMountInfoPath)
		if err != nil {
			t.Errorf("read test cleanup mount table: %v", err)
			return
		}
		if _, mounted := findMount(mounts, target); mounted {
			if unmountErr := unix.Unmount(target, 0); unmountErr != nil {
				t.Errorf("cleanup test stage: %v", unmountErr)
			}
		}
	})
}

func assertFilesystemStageRejected(t *testing.T, node *NodeServer, req *csipb.NodeStageVolumeRequest) {
	t.Helper()
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		t.Fatal(err)
	}
	if mounted, ok := findMount(mounts, req.StagingTargetPath); ok {
		t.Fatalf("rejected stage exposed a mount: %+v", mounted)
	}
	if _, statErr := os.Stat(node.stateFilePath(req.VolumeId)); !os.IsNotExist(statErr) {
		t.Fatalf("rejected stage persisted a success record: %v", statErr)
	}
	entries, err := os.ReadDir(req.StagingTargetPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected stage exposed host directory contents: entries=%v, err=%v", entries, err)
	}
}
