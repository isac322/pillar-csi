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
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			proxy := filepath.Join(root, "proxy")
			if kind != "missing" && kind != "symlink" {
				if err := os.Mkdir(proxy, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "foreign-directory" {
				if err := os.WriteFile(filepath.Join(proxy, "foreign-data"), []byte("must remain private"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink(root, proxy); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(root, "stage")
			if err := os.Mkdir(target, 0o750); err != nil {
				t.Fatal(err)
			}
			mounter := NewKubeMounter()
			t.Cleanup(func() {
				mounts, err := readMountInfoFile(procMountInfoPath)
				if err != nil {
					t.Errorf("read test cleanup mount table: %v", err)
					return
				}
				if _, mounted := findMount(mounts, target); mounted {
					if err := unix.Unmount(target, 0); err != nil {
						t.Errorf("cleanup test stage: %v", err)
					}
				}
			})
			node := NewNodeServer("storage-node", nil, mounter).
				WithDriverName("files.pillar-csi.bhyoo.com").WithStateDir(filepath.Join(root, "state"))
			req := &csipb.NodeStageVolumeRequest{
				VolumeId: "agent/nfs/zfs-dataset/tank/native/lifecycle", StagingTargetPath: target,
				VolumeCapability: &csipb.VolumeCapability{
					AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
					AccessMode: &csipb.VolumeCapability_AccessMode{Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
				},
				PublishContext: map[string]string{
					PublishContextKeyFilesystemAdoption:  `{"kind":"zfs-dataset","canonicalSource":"tank/existing","resourceId":"12345","filesystemType":"zfs"}`,
					PublishContextKeyFilesystemCapacity:  "1048576",
					PublishContextKeyFilesystemLayout:    `{"zfs":{"pool":"tank","parentDataset":""}}`,
					PublishContextKeyFilesystemLocalNode: "storage-node",
					PublishContextKeyFilesystemProxyPath: proxy,
				},
			}
			if _, err := node.NodeStageVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("stage without an owned proxy mount = %v, want FailedPrecondition for kubelet retry", err)
			}
			mounts, err := readMountInfoFile(procMountInfoPath)
			if err != nil {
				t.Fatal(err)
			}
			if mounted, ok := findMount(mounts, target); ok {
				t.Fatalf("rejected stage exposed a mount: %+v", mounted)
			}
			if _, err := os.Stat(node.stateFilePath(req.VolumeId)); !os.IsNotExist(err) {
				t.Fatalf("rejected stage persisted a success record: %v", err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected stage exposed host directory contents: entries=%v, err=%v", entries, err)
			}
		})
	}
}
