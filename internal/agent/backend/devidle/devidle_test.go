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

package devidle_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isac322/pillar-csi/internal/agent/backend/devidle"
)

type tree struct {
	root string
	stat map[string]uint64
}

func newTree(t *testing.T) *tree {
	t.Helper()
	return &tree{root: t.TempDir(), stat: map[string]uint64{}}
}

func (tr *tree) checker() devidle.Checker {
	return devidle.Checker{
		Roots: devidle.Roots{
			MountinfoPath: filepath.Join(tr.root, "mountinfo"),
			SysDevBlock:   filepath.Join(tr.root, "sys", "dev", "block"),
			ConfigfsRoot:  filepath.Join(tr.root, "config"),
		},
		StatRdev: func(path string) (uint64, error) {
			if rdev, ok := tr.stat[path]; ok {
				return rdev, nil
			}
			return 0, &os.PathError{Op: "stat", Path: path, Err: os.ErrNotExist}
		},
	}
}

func (tr *tree) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(tr.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (tr *tree) symlink(t *testing.T, target, rel string) {
	t.Helper()
	path := filepath.Join(tr.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

var lvDev = devidle.EncodeLinuxDev(252, 0)

func TestLinuxDevEncoding(t *testing.T) {
	t.Parallel()
	for _, mm := range [][2]uint64{
		{252, 0}, {252, 5}, {8, 17}, {259, 1 << 19}, {4095, 255}, {4096, 256},
		{0, 0}, {0xffffffff, 0}, {0, 0xffffffff}, {0xffffffff, 0xffffffff},
		{0xfffff000, 0xff}, {0xfff, 0xffffff00},
	} {
		dev := devidle.EncodeLinuxDev(mm[0], mm[1])
		major, minor := devidle.DecodeLinuxDev(dev)
		if major != mm[0] || minor != mm[1] {
			t.Errorf("round trip %d:%d → %#x → %d:%d", mm[0], mm[1], dev, major, minor)
		}
	}
	// glibc makedev(252, 0) and makedev(8, 17) as st_rdev reports them.
	if got := devidle.EncodeLinuxDev(252, 0); got != 0xfc00 {
		t.Errorf("EncodeLinuxDev(252, 0) = %#x, want 0xfc00", got)
	}
	if got := devidle.EncodeLinuxDev(8, 17); got != 0x811 {
		t.Errorf("EncodeLinuxDev(8, 17) = %#x, want 0x811", got)
	}
	// High major and high minor bits occupy disjoint fields: the high major
	// bits must never leak into the minor (and vice versa).
	if got := devidle.EncodeLinuxDev(4096, 256); got != 0x100000100000 {
		t.Errorf("EncodeLinuxDev(4096, 256) = %#x, want 0x100000100000", got)
	}
	if major, minor := devidle.DecodeLinuxDev(0xfffff00000000000); major != 0xfffff000 || minor != 0 {
		t.Errorf("DecodeLinuxDev(0xfffff00000000000) = %d:%d, want %d:0", major, minor, uint64(0xfffff000))
	}
	if major, minor := devidle.DecodeLinuxDev(0x00000ffffff00000); major != 0 || minor != 0xffffff00 {
		t.Errorf("DecodeLinuxDev(0x00000ffffff00000) = %d:%d, want 0:%d", major, minor, uint64(0xffffff00))
	}
}

func TestReport_AbsentTreesObserveNothing(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	findings, err := tr.checker().Report(lvDev)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Report = (%+v, %v), want none", findings, err)
	}
	if err := tr.checker().Check("vg/lv", lvDev); err != nil {
		t.Fatalf("Check = %v, want nil", err)
	}
}

// Mounts match on mountinfo's major:minor, whatever path alias the source
// column uses; a source naming the LV's path on another device is not a
// consumer.
func TestReport_MountsByDeviceNumber(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write(t, "mountinfo",
		"22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+
			"36 22 252:0 / /srv/a\\040b rw - ext4 /dev/mapper/vg-lv rw\n"+
			"37 22 252:0 /sub /var/lib/bind rw - ext4 /dev/dm-0 rw\n"+
			"38 22 252:5 / /srv/other rw - ext4 /dev/vg/lv rw\n"+
			"malformed\n")
	findings, err := tr.checker().Report(lvDev)
	if err != nil {
		t.Fatal(err)
	}
	want := []devidle.Finding{
		{Kind: devidle.KindMount, Detail: "/srv/a b"},
		{Kind: devidle.KindMount, Detail: "/var/lib/bind"},
	}
	if !slices.Equal(findings, want) {
		t.Fatalf("Report = %+v, want %+v", findings, want)
	}
}

func TestReport_Holders(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write(t, "sys/dev/block/252:0/holders/dm-3", "")
	tr.write(t, "sys/dev/block/252:0/holders/dm-4", "")
	tr.write(t, "sys/dev/block/252:5/holders/dm-9", "")
	findings, err := tr.checker().Report(lvDev)
	if err != nil {
		t.Fatal(err)
	}
	want := []devidle.Finding{{Kind: devidle.KindHolder, Detail: "dm-3"}, {Kind: devidle.KindHolder, Detail: "dm-4"}}
	if !slices.Equal(findings, want) {
		t.Fatalf("Report = %+v, want %+v", findings, want)
	}
}

// A LIO backstore is matched by the device its configured path resolves to.
// The kernel's info attribute wins over the informational udev_path, and
// the target ID is the IQN whose LUN links the backstore.
func TestReport_LIOBackstores(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.stat["/dev/dm-0"] = lvDev
	tr.stat["/dev/vg/lv"] = lvDev
	tr.stat["/dev/sdb"] = devidle.EncodeLinuxDev(8, 16)
	// bound: info names the alias, the stale udev_path names another disk.
	tr.write(t, "config/target/core/iblock_0/bound/info",
		"Status: ACTIVATED  Max Queue Depth: 128\n        iBlock device: dm-0  UDEV PATH: /dev/dm-0  readonly: 0\n")
	tr.write(t, "config/target/core/iblock_0/bound/udev_path", "/dev/sdb\n")
	tr.symlink(t, "../../../../../core/iblock_0/bound",
		"config/target/iscsi/iqn.2003-01.org.example:own/tpgt_1/lun/lun_0/bs")
	// unbound: configured only through udev_path.
	tr.write(t, "config/target/core/iblock_1/unbound/udev_path", "/dev/vg/lv\n")
	// unrelated and vanished devices.
	tr.write(t, "config/target/core/iblock_1/gone/udev_path", "/dev/removed\n")
	// HBA attribute files sit beside backstores and are not backstores.
	tr.write(t, "config/target/core/iblock_0/hba_info", "HBA Index: 0 plugin: iblock version: v5.0\n")
	tr.write(t, "config/target/core/iblock_0/hba_mode", "0\n")
	findings, err := tr.checker().Report(lvDev)
	if err != nil {
		t.Fatal(err)
	}
	want := []devidle.Finding{
		{
			Kind: devidle.KindExport, Detail: "/dev/dm-0",
			TargetID: "iqn.2003-01.org.example:own", Protocol: devidle.ProtocolISCSI,
		},
		{Kind: devidle.KindExport, Detail: "/dev/vg/lv", Protocol: devidle.ProtocolISCSI},
	}
	if !slices.Equal(findings, want) {
		t.Fatalf("Report = %+v, want %+v", findings, want)
	}
}

func TestReport_NVMeNamespaces(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.stat["/dev/mapper/vg-lv"] = lvDev
	tr.stat["/dev/sdb"] = devidle.EncodeLinuxDev(8, 16)
	tr.write(t, "config/nvmet/subsystems/nqn.2014-08.org.example:foreign/namespaces/1/device_path", "/dev/mapper/vg-lv\n")
	tr.write(t, "config/nvmet/subsystems/nqn.2014-08.org.example:other/namespaces/1/device_path", "/dev/sdb\n")
	findings, err := tr.checker().Report(lvDev)
	if err != nil {
		t.Fatal(err)
	}
	want := []devidle.Finding{{
		Kind: devidle.KindExport, Detail: "/dev/mapper/vg-lv (namespace 1)",
		TargetID: "nqn.2014-08.org.example:foreign", Protocol: devidle.ProtocolNVMe,
	}}
	if !slices.Equal(findings, want) {
		t.Fatalf("Report = %+v, want %+v", findings, want)
	}
}

func TestCheck_ListsEveryFinding(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.write(t, "mountinfo", "36 22 252:0 / /srv/a rw - ext4 /dev/dm-0 rw\n")
	tr.write(t, "sys/dev/block/252:0/holders/dm-3", "")
	err := tr.checker().Check("vg/lv", lvDev)
	var inUse *devidle.InUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("Check = %v, want *InUseError", err)
	}
	if inUse.DeviceID != "vg/lv" || len(inUse.Findings) != 2 {
		t.Fatalf("InUseError = %+v, want both findings for vg/lv", inUse)
	}
}

// Probe failures are errors, never an empty (idle) report.
func TestReport_ProbeErrorsFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("unreadable mount table", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		if err := os.MkdirAll(filepath.Join(tr.root, "mountinfo"), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.checker().Report(lvDev); err == nil {
			t.Fatal("Report succeeded on an unreadable mount table")
		}
	})
	t.Run("holders is not a directory", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		tr.write(t, "sys/dev/block/252:0/holders", "")
		if _, err := tr.checker().Report(lvDev); err == nil {
			t.Fatal("Report succeeded on an unreadable holders directory")
		}
	})
	t.Run("recorded export path unstatable", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		tr.write(t, "config/target/core/iblock_0/bs/udev_path", "/dev/denied\n")
		c := tr.checker()
		c.StatRdev = func(string) (uint64, error) { return 0, os.ErrPermission }
		if err := c.Check("vg/lv", lvDev); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("Check = %v, want the stat error", err)
		}
	})
}

const foreignIQN = "iqn.2003-01.org.example:foreign"

// foreignDisabledLIO builds a configured but disabled foreign backstore on
// the LV, bound to a foreign target's LUN.  Such a backstore holds no
// exclusive claim, so O_EXCL succeeds and only the configfs scan sees it.
// The lun_0 directory carries attribute files and a statistics group beside
// the backstore symlink, as the kernel creates them.
func foreignDisabledLIO(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t)
	tr.stat["/dev/vg/lv"] = lvDev
	tr.write(t, "config/target/core/iblock_0/hba_info", "HBA Index: 0 plugin: iblock version: v5.0\n")
	tr.write(t, "config/target/core/iblock_0/foreign/udev_path", "/dev/vg/lv\n")
	tr.write(t, "config/target/core/iblock_0/foreign/enable", "0\n")
	tr.write(t, "config/target/iscsi/lio_version", "Datera Inc. iSCSI Target v4.1.0\n")
	lun := "config/target/iscsi/" + foreignIQN + "/tpgt_1/lun/lun_0"
	tr.symlink(t, "../../../../../core/iblock_0/foreign", lun+"/bs")
	tr.write(t, lun+"/alua_tg_pt_gp", "default_tg_pt_gp\n")
	if err := os.MkdirAll(filepath.Join(tr.root, lun, "statistics", "scsi_port"), 0o750); err != nil {
		t.Fatal(err)
	}
	return tr
}

var foreignFinding = devidle.Finding{
	Kind: devidle.KindExport, Detail: "/dev/vg/lv", TargetID: foreignIQN, Protocol: devidle.ProtocolISCSI,
}

// failOn makes the real os.ReadDir fail with cause on exactly one path.
// Permission bits cannot make a directory unreadable to root, so an
// injected listing error is the only deterministic EACCES/EIO under every
// uid; every other directory is still listed for real.
func failOn(path string, cause error) func(string) ([]os.DirEntry, error) {
	return func(p string) ([]os.DirEntry, error) {
		if p == path {
			return nil, &os.PathError{Op: "open", Path: p, Err: cause}
		}
		return os.ReadDir(p)
	}
}

var errInjectedIO = errors.New("input/output error")

// assertFailClosed requires Report to fail without findings and Check to
// fail with that error rather than an idle nil or an *InUseError.
func assertFailClosed(t *testing.T, c devidle.Checker, cause error, path string) {
	t.Helper()
	findings, err := c.Report(lvDev)
	if err == nil || findings != nil {
		t.Fatalf("Report = (%+v, %v), want an error and no findings", findings, err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Report error %v does not wrap %v", err, cause)
	}
	for _, want := range []string{path, "252:0"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Report error %q lacks %q", err, want)
		}
	}
	err = c.Check("vg/lv", lvDev)
	var inUse *devidle.InUseError
	if !errors.Is(err, cause) || errors.As(err, &inUse) {
		t.Fatalf("Check = %v, want the %v probe error", err, cause)
	}
}

// An unreadable directory anywhere in the LIO trees fails the scan: a
// listing error must never read as "no backstore" or "no binding".
func TestReport_LIOEnumerationErrorsFailClosed(t *testing.T) {
	t.Parallel()
	t.Run("fixture is found when readable", func(t *testing.T) {
		t.Parallel()
		findings, err := foreignDisabledLIO(t).checker().Report(lvDev)
		if err != nil || !slices.Equal(findings, []devidle.Finding{foreignFinding}) {
			t.Fatalf("Report = (%+v, %v), want [%+v]", findings, err, foreignFinding)
		}
	})
	lun := "target/iscsi/" + foreignIQN + "/tpgt_1/lun"
	for _, tc := range []struct {
		name  string
		dir   string
		cause error
	}{
		{"target/core EACCES", "target/core", os.ErrPermission},
		{"HBA EIO", "target/core/iblock_0", errInjectedIO},
		{"iscsi fabric EACCES", "target/iscsi", os.ErrPermission},
		{"iSCSI target EIO", "target/iscsi/" + foreignIQN, errInjectedIO},
		{"LUN group EACCES", lun, os.ErrPermission},
		{"LUN EIO", lun + "/lun_0", errInjectedIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := foreignDisabledLIO(t)
			path := filepath.Join(tr.root, "config", filepath.FromSlash(tc.dir))
			c := tr.checker()
			c.ReadDir = failOn(path, tc.cause)
			assertFailClosed(t, c, tc.cause, path)
		})
	}
	for _, tc := range []struct {
		name  string
		attr  string
		cause error
	}{
		{"info EACCES", "info", os.ErrPermission},
		{"udev_path EIO", "udev_path", errInjectedIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := foreignDisabledLIO(t)
			path := filepath.Join(tr.root, "config", "target", "core", "iblock_0", "foreign", tc.attr)
			c := tr.checker()
			c.ReadFile = func(p string) ([]byte, error) {
				if p == path {
					return nil, &os.PathError{Op: "open", Path: p, Err: tc.cause}
				}
				return os.ReadFile(p) //nolint:gosec // G304: test fixture path.
			}
			assertFailClosed(t, c, tc.cause, path)
		})
	}
	t.Run("holders EIO", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		tr.write(t, "sys/dev/block/252:0/holders/dm-3", "")
		path := filepath.Join(tr.root, "sys", "dev", "block", "252:0", "holders")
		c := tr.checker()
		c.ReadDir = failOn(path, errInjectedIO)
		if _, err := c.Report(lvDev); !errors.Is(err, errInjectedIO) {
			t.Fatalf("Report = %v, want the holders listing error", err)
		}
	})
}

// Only ENOENT is empty: absent optional subtrees and entries removed
// between listings contribute nothing, while the rest of the tree is
// still scanned.
func TestReport_LIOAbsentSubtreesObserveNothing(t *testing.T) {
	t.Parallel()
	t.Run("empty and partial trees", func(t *testing.T) {
		t.Parallel()
		tr := newTree(t)
		for _, dir := range []string{
			"config/target/core/iblock_0",
			"config/target/iscsi/" + foreignIQN + "/tpgt_1",
			"config/target/iscsi/iqn.2003-01.org.example:nolun/tpgt_1/lun/lun_0",
		} {
			if err := os.MkdirAll(filepath.Join(tr.root, filepath.FromSlash(dir)), 0o750); err != nil {
				t.Fatal(err)
			}
		}
		findings, err := tr.checker().Report(lvDev)
		if err != nil || len(findings) != 0 {
			t.Fatalf("Report = (%+v, %v), want none", findings, err)
		}
	})
	t.Run("vanished HBA is skipped, others still scanned", func(t *testing.T) {
		t.Parallel()
		tr := foreignDisabledLIO(t)
		tr.write(t, "config/target/core/iblock_1/gone/udev_path", "/dev/vg/lv\n")
		c := tr.checker()
		c.ReadDir = failOn(filepath.Join(tr.root, "config", "target", "core", "iblock_1"), os.ErrNotExist)
		findings, err := c.Report(lvDev)
		if err != nil || !slices.Equal(findings, []devidle.Finding{foreignFinding}) {
			t.Fatalf("Report = (%+v, %v), want [%+v]", findings, err, foreignFinding)
		}
	})
}
