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

package zfs_test

// Import tests.  Every test builds a ZfsBackend on fake host roots (tmpdirs
// for /dev/zvol, configfs, /sys/block, and a fake mounts file) plus a fake
// executor, so no real ZFS pool or kernel state is needed.  The fake device
// is a symlink <devZvolBase>/<pool>/<parent>/<name> -> <devZvolBase>/../zdN,
// mirroring the production /dev/zvol layout.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/backend/zfs"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

const (
	importVolID = "hot-data/pvc-abc"
	importDS    = "hot-data/k8s/pvc-abc"
	importDevN  = "zd16"
)

// importFixture holds a backend on fake host roots.
type importFixture struct {
	b           *zfs.ZfsBackend
	exec        *fakeExec
	devZvolBase string
	configfs    string
	sysBlock    string
	mounts      string
	devPath     string // <devZvolBase>/hot-data/k8s/pvc-abc
}

func newImportFixture(t *testing.T, zfsGetOut []byte, zfsGetErr error) *importFixture {
	t.Helper()
	root := t.TempDir()
	fx := &importFixture{
		devZvolBase: filepath.Join(root, "dev-zvol"),
		configfs:    filepath.Join(root, "configfs"),
		sysBlock:    filepath.Join(root, "sys-block"),
		mounts:      filepath.Join(root, "proc-mounts"),
	}
	fx.devPath = filepath.Join(fx.devZvolBase, importDS)
	for _, d := range []string{
		fx.devZvolBase, fx.configfs, fx.sysBlock,
		filepath.Dir(fx.devPath),
	} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %q: %v", d, err)
		}
	}
	if err := os.WriteFile(fx.mounts, []byte(""), 0o600); err != nil {
		t.Fatalf("write mounts: %v", err)
	}
	// The fake zvol device: a dangling-free symlink so os.Readlink succeeds.
	target := filepath.Join(root, "dev", importDevN)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatalf("mkdir dev: %v", err)
	}
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("write device node stub: %v", err)
	}
	if err := os.Symlink(target, fx.devPath); err != nil {
		t.Fatalf("symlink zvol: %v", err)
	}
	// /sys/block/zd16 with an empty holders dir.
	if err := os.MkdirAll(filepath.Join(fx.sysBlock, importDevN, "holders"), 0o750); err != nil {
		t.Fatalf("mkdir sysblock: %v", err)
	}

	fx.exec = newFake(t, fakeResponse{out: zfsGetOut, err: zfsGetErr})
	fx.b = zfs.NewWithExecFn("hot-data", "k8s", fx.exec.exec())
	zfs.SetBackendDevZvolBase(t, fx.b, fx.devZvolBase)
	zfs.SetBackendHostRoots(t, fx.b, fx.configfs, fx.sysBlock, fx.mounts)
	zfs.SetBackendClaimDevice(t, fx.b, func(string) (func() error, error) {
		return func() error { return nil }, nil
	})
	return fx
}

func zfsGetProps(dtype, volsize string) []byte {
	return []byte(fmt.Sprintf("type\t%s\nvolsize\t%s\n", dtype, volsize))
}

func TestImport_Success(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)

	devPath, size, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if devPath != fx.devPath {
		t.Errorf("devicePath = %q, want %q", devPath, fx.devPath)
	}
	if size != 2147483648 {
		t.Errorf("size = %d, want existing volsize", size)
	}
	for _, c := range fx.exec.calls {
		if len(c.args) > 0 && c.args[0] == "create" {
			t.Fatalf("import must never create; saw %v", c.args)
		}
	}
}

func TestImport_Missing(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t,
		[]byte("cannot open 'hot-data/k8s/pvc-abc': dataset does not exist"),
		errors.New("exit status 1"))

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Import error = %v, want ImportRefusedError", err)
	}
	if refused.Reason != "missing" {
		t.Errorf("reason = %q, want missing", refused.Reason)
	}
}

func TestImport_WrongType(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("filesystem", "-"), nil)

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "wrong type" {
		t.Fatalf("Import error = %v, want wrong-type refusal", err)
	}
}

func TestImport_TooSmall(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "1073741824"), nil)

	_, _, err := fx.b.Import(context.Background(), importVolID, 2<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "too small" {
		t.Fatalf("Import error = %v, want too-small refusal", err)
	}
}

func TestImport_LayoutEscape(t *testing.T) {
	t.Parallel()
	// "hot-data/../sneaky" would resolve outside the backend's layout.
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)

	_, _, err := fx.b.Import(context.Background(), "hot-data/../../nas/x", 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "layout" {
		t.Fatalf("Import error = %v, want layout refusal", err)
	}
	if len(fx.exec.calls) != 0 {
		t.Errorf("import ran zfs commands before refusing a bad volumeID: %v", fx.exec.calls)
	}
}

func TestImport_LIOBackstoreInUse(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	// Democratic-csi leaves target/core/iblock_<n>/<name>/udev_path pointing
	// at the zvol.
	udev := filepath.Join(fx.configfs, "target", "core", "iblock_0", "pvc-abc", "udev_path")
	if err := os.MkdirAll(filepath.Dir(udev), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(udev, []byte("/dev/zvol/"+importDS+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "in use" {
		t.Fatalf("Import error = %v, want in-use refusal", err)
	}
	if !strings.Contains(refused.Detail, "LIO") {
		t.Errorf("detail = %q, want LIO backstore mention", refused.Detail)
	}
}

func TestImport_NVMeNamespaceInUse(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	// An nvmet subsystem whose namespace exports this device — e.g. pillar-csi
	// already owns it.
	nsDir := filepath.Join(fx.configfs, "nvmet", "subsystems", "nqn.2026-01.test:vol", "namespaces", "1")
	if err := os.MkdirAll(nsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "device_path"), []byte(fx.devPath), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "in use" {
		t.Fatalf("Import error = %v, want in-use refusal", err)
	}
	if !strings.Contains(refused.Detail, "nvmet") {
		t.Errorf("detail = %q, want nvmet mention", refused.Detail)
	}
}

func TestImport_MountedRefused(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	if err := os.WriteFile(fx.mounts,
		[]byte("/dev/"+importDevN+" /var/lib/openebs xfs rw,relatime 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "in use" {
		t.Fatalf("Import error = %v, want in-use refusal", err)
	}
}

func TestImport_SysfsHoldersRefused(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	if err := os.MkdirAll(filepath.Join(fx.sysBlock, importDevN, "holders", "dm-0"), 0o750); err != nil {
		t.Fatal(err)
	}

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "in use" {
		t.Fatalf("Import error = %v, want in-use refusal", err)
	}
}

func TestImport_ExclusiveClaimRefused(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	zfs.SetBackendClaimDevice(t, fx.b, func(string) (func() error, error) {
		return nil, nvmeof.ErrDeviceHeld
	})

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "in use" {
		t.Fatalf("Import error = %v, want in-use refusal", err)
	}
}

func TestImport_MissingDeviceNode(t *testing.T) {
	t.Parallel()
	fx := newImportFixture(t, zfsGetProps("volume", "2147483648"), nil)
	if err := os.Remove(fx.devPath); err != nil {
		t.Fatal(err)
	}

	_, _, err := fx.b.Import(context.Background(), importVolID, 1<<30)
	var refused *backend.ImportRefusedError
	if !errors.As(err, &refused) || refused.Reason != "missing" {
		t.Fatalf("Import error = %v, want missing-device refusal", err)
	}
}
