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

package zfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

func datasetFixture(t *testing.T) (root, mountpoint, mountinfo string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mountpoint = filepath.Join(root, "tank", "k8s", "vol1")
	//nolint:gosec // G301: private temp fixture models a mounted filesystem root with client traversal access.
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatal(err)
	}
	mountinfo = filepath.Join(root, "mountinfo")
	if err := os.WriteFile(mountinfo, []byte(datasetMountLine(mountpoint)), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, mountpoint, mountinfo
}

func datasetOutput(mountpoint, dtype, mounted string, quota int64) []byte {
	return []byte(fmt.Sprintf("type\t%s\nrefquota\t%d\nmountpoint\t%s\nsharenfs\toff\nmounted\t%s\n",
		dtype, quota, mountpoint, mounted))
}

func datasetMountLine(mountpoint string) string {
	return "100 1 0:10 / " + mountpoint + " rw - zfs tank/k8s/vol1 rw\n"
}

func requireDatasetMode(t *testing.T, mountpoint string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("dataset path %q permissions = %04o, want %04o", mountpoint, info.Mode().Perm(), want)
	}
}

func TestDatasetCreateRetryPreservesExistingFilesystem(t *testing.T) {
	root, mountpoint, mountinfo := datasetFixture(t)
	var gets, creates int
	b := NewDatasetWithExecFn("tank", "k8s", root, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "get":
			gets++
			if gets == 1 {
				return []byte("dataset does not exist"), errors.New("missing")
			}
			return datasetOutput(mountpoint, "filesystem", "yes", 1<<30), nil
		case "create":
			creates++
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected operation %s", args[0])
		}
	}, WithDatasetMountInfoPath(mountinfo))
	params := &agentv1.BackendParams{Params: &agentv1.BackendParams_Zfs{
		Zfs: &agentv1.ZfsVolumeParams{Pool: "tank", ParentDataset: "k8s"},
	}}
	for i := range 2 {
		path, capacity, err := b.Create(context.Background(), "tank/vol1", 1<<30, params)
		if err != nil || path != mountpoint || capacity != 1<<30 {
			t.Fatalf("Create retry %d = %q/%d/%v, want %q/%d/nil", i, path, capacity, err, mountpoint, 1<<30)
		}
	}
	if creates != 1 {
		t.Fatalf("retry recreated volume: create count = %d", creates)
	}
	_, _, err := b.Create(context.Background(), "tank/vol1", 2<<30, params)
	conflict, isConflict := errors.AsType[*backend.ConflictError](err)
	if !isConflict || conflict == nil {
		t.Fatalf("larger Create retry error = %v, want ConflictError", err)
	}
}

func TestDatasetCreateRecoversMountAndRejectsStackedForeignMount(t *testing.T) {
	root, mountpoint, mountinfo := datasetFixture(t)
	mounted := "no"
	b := NewDatasetWithExecFn("tank", "k8s", root, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "mount" {
			mounted = "yes"
			return nil, nil
		}
		if args[0] == "get" {
			return datasetOutput(mountpoint, "filesystem", mounted, 1024), nil
		}
		return nil, fmt.Errorf("unexpected mutation %s", args[0])
	}, WithDatasetMountInfoPath(mountinfo))
	if _, _, err := b.Create(context.Background(), "tank/vol1", 1024, nil); err != nil {
		t.Fatalf("recover mount: %v", err)
	}
	stackedMounts := datasetMountLine(mountpoint) + "101 1 0:11 / " + mountpoint + " rw - ext4 /dev/foreign rw\n"
	if err := os.WriteFile(mountinfo, []byte(stackedMounts), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Create(context.Background(), "tank/vol1", 1024, nil); err == nil {
		t.Fatal("Create accepted foreign mount stacked over its dataset")
	}
}

func TestDatasetRejectsUnsafeIdentityAndPropertyRequests(t *testing.T) {
	b := NewDatasetWithExecFn("tank", "k8s", "/state/datasets", func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unsafe request reached executor")
		return nil, nil
	})
	for _, volumeID := range []string{"tank/../escape", "other/vol1", "tank/a/b", "tank/vol1\n", "tank/vol@1"} {
		if path := b.DevicePath(volumeID); path != "" {
			t.Errorf("DevicePath(%q) = %q", volumeID, path)
		}
		if _, _, err := b.Create(context.Background(), volumeID, 1, nil); err == nil {
			t.Errorf("Create(%q) accepted unsafe identity", volumeID)
		}
	}
	for _, key := range []string{"mountpoint", "refquota", "sharenfs", "volsize", "volblocksize"} {
		params := &agentv1.BackendParams{Params: &agentv1.BackendParams_Zfs{
			Zfs: &agentv1.ZfsVolumeParams{Pool: "tank", ParentDataset: "k8s", Properties: map[string]string{key: "1"}},
		}}
		if _, _, err := b.Create(context.Background(), "tank/vol1", 1, params); err == nil {
			t.Errorf("Create accepted managed/zvol property %q", key)
		}
	}
	wrong := &agentv1.BackendParams{Params: &agentv1.BackendParams_Lvm{Lvm: &agentv1.LvmVolumeParams{}}}
	if _, _, err := b.Create(context.Background(), "tank/vol1", 1, wrong); err == nil {
		t.Fatal("Create accepted LVM parameters")
	}
}

func TestDatasetExpandNeverShrinksAndChecksReadback(t *testing.T) {
	quota := int64(2048)
	sets := 0
	b := NewDatasetWithExecFn("tank", "k8s", "/state/datasets",
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "set" {
				sets++
				return nil, nil // Intentionally stale refquota readback.
			}
			return datasetOutput("/state/datasets/tank/k8s/vol1", "filesystem", "yes", quota), nil
		})
	if got, err := b.Expand(context.Background(), "tank/vol1", 1024); err != nil || got != 2048 || sets != 0 {
		t.Fatalf("smaller expand = %d/%v, mutation count %d", got, err, sets)
	}
	if _, err := b.Expand(context.Background(), "tank/vol1", 4096); err == nil {
		t.Fatalf("stale quota readback accepted: %v", err)
	}
	quota = 4096
	if got, err := b.Expand(context.Background(), "tank/vol1", 4096); err != nil || got != 4096 {
		t.Fatalf("completed expand retry = %d/%v", got, err)
	}
}

func TestDatasetDeleteRefusesZvolAndPropagatesBusy(t *testing.T) {
	dtype := "volume"
	var destroys int
	busyErr := errors.New("busy")
	b := NewDatasetWithExecFn("tank", "k8s", "/state/datasets",
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "destroy" {
				destroys++
				return []byte("dataset is busy"), busyErr
			}
			return datasetOutput("/state/datasets/tank/k8s/vol1", dtype, "yes", 1024), nil
		})
	if err := b.Delete(context.Background(), "tank/vol1"); err == nil || destroys != 0 {
		t.Fatalf("wrong-type Delete = %v, destroys %d", err, destroys)
	}
	dtype = "filesystem"
	if err := b.Delete(context.Background(), "tank/vol1"); !errors.Is(err, busyErr) {
		t.Fatalf("busy destroy error = %v", err)
	}
}

func TestDatasetListOmitsParentsChildrenAndForeignFilesystems(t *testing.T) {
	b := NewDatasetWithExecFn("tank", "k8s", "/state/datasets",
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("tank/k8s\tnone\ntank/k8s/vol1\t1024\ntank/k8s/vol1/child\t512\n" +
					"tank/k8s/foreign\t1024\ntank/k8s/plain\tnone\n"), nil
			}
			if args[len(args)-1] == "tank/k8s/foreign" {
				return datasetOutput("/foreign", "filesystem", "yes", 1024), nil
			}
			return datasetOutput("/state/datasets/tank/k8s/vol1", "filesystem", "yes", 1024), nil
		})
	volumes, err := b.ListVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 || volumes[0].GetVolumeId() != "tank/vol1" ||
		volumes[0].GetCapacityBytes() != 1024 || volumes[0].GetDevicePath() != "/state/datasets/tank/k8s/vol1" {
		t.Fatalf("ListVolumes = %v, want only owned quota-backed vol1", volumes)
	}
}

func TestDatasetCreateRejectsSymlinkMountPath(t *testing.T) {
	root, mountpoint, _ := datasetFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(filepath.Dir(mountpoint), "linked")); err != nil {
		t.Fatal(err)
	}
	b := NewDatasetWithExecFn("tank", "k8s", root, func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("symlink mountpoint reached ZFS mutation")
		return nil, nil
	})
	if _, _, err := b.Create(context.Background(), "tank/linked", 1024, nil); err == nil {
		t.Fatal("Create accepted a symlink escape from the mount root")
	}
}

func TestDatasetCreateProtectsBackingStubWithoutChangingMountedRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ZFS backing directories must be root-owned")
	}
	root, mountpoint, mountinfo := datasetFixture(t)
	if err := os.WriteFile(mountinfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	created := false
	b := NewDatasetWithExecFn("tank", "k8s", root, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "get":
			if !created {
				return []byte("dataset does not exist"), errors.New("missing")
			}
			return datasetOutput(mountpoint, "filesystem", "yes", 1024), nil
		case "create":
			requireDatasetMode(t, mountpoint, 0)
			// Represent the newly mounted dataset root: it has independent
			// permissions and must not be chmod'd by later Create retries.
			//nolint:gosec // G302: models an independently mounted root; retries must preserve its actual mode0755.
			if err := os.Chmod(mountpoint, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(mountinfo, []byte(datasetMountLine(mountpoint)), 0o600); err != nil {
				t.Fatal(err)
			}
			created = true
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected operation %s", args[0])
		}
	}, WithDatasetMountInfoPath(mountinfo))
	for range 2 {
		if _, _, err := b.Create(context.Background(), "tank/vol1", 1024, nil); err != nil {
			t.Fatal(err)
		}
		requireDatasetMode(t, mountpoint, 0o755)
	}
}

func TestDatasetRecoveryRefusesNonemptyBackingDirectory(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ZFS backing directories must be root-owned")
	}
	root, mountpoint, mountinfo := datasetFixture(t)
	if err := os.WriteFile(mountinfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(mountpoint, "unexpected-data")
	if err := os.WriteFile(marker, []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewDatasetWithExecFn("tank", "k8s", root, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] != "get" {
			t.Fatal("nonempty backing directory reached ZFS mount/create")
		}
		return datasetOutput(mountpoint, "filesystem", "no", 1024), nil
	}, WithDatasetMountInfoPath(mountinfo))
	if _, _, err := b.Create(context.Background(), "tank/vol1", 1024, nil); err == nil {
		t.Fatalf("nonempty backing directory error = %v", err)
	}
	//nolint:gosec // G304: marker is created above inside this test's private temp fixture.
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "must survive" {
		t.Fatalf("backing directory data was changed: %q/%v", data, err)
	}
}
