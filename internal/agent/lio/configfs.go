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

// Package lio implements iSCSI target management through the Linux LIO
// target's configfs tree.  It never shells out to targetcli; every operation
// is a mkdir, rmdir, symlink or attribute write under
// <configfs>/target, the same way package nvmeof drives nvmet.
//
// Configfs layout of one export (abbreviated):
//
//	<root>/target/
//	  core/iblock_<HBAIndex>/<backstore>/     iblock backstore (one per volume)
//	    control                   "udev_path=<device>" (write-only; the device iblock opens)
//	    info                      read-back of control: "... UDEV PATH: <device> ..."
//	    udev_path                 informational copy of the device (as targetcli writes)
//	    enable                    "1" once configured (opens the bdev exclusively)
//	    wwn/vpd_unit_serial       stable T10 unit serial (see DeriveUnitSerial)
//	  iscsi/<target IQN>/tpgt_1/
//	    enable                    "1" to accept logins
//	    attrib/authentication     "0" (no CHAP)
//	    attrib/generate_node_acls "1" demo mode (ACL off) / "0" explicit ACLs
//	    attrib/cache_dynamic_acls, attrib/demo_mode_write_protect (demo mode)
//	    lun/lun_0/<link>          → core/iblock_<HBAIndex>/<backstore>
//	    np/<addr>:<port>          network portal on the family's wildcard address
//	                              ("0.0.0.0:<port>" or "[::]:<port>", see Target.Portal)
//	    acls/<initiator IQN>/lun_0/<link> → ../../lun/lun_0 (mapped LUN)
//
// Kernel modules: target_core_mod, target_core_iblock, iscsi_target_mod.
// The iscsi fabric directory is not created by loading the modules; the
// first mkdir of <root>/target/iscsi registers the fabric (and loads
// iscsi_target_mod on demand), so every operation creates it idempotently.
package lio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

const (
	// DefaultConfigfsRoot is the standard kernel configfs mount point.
	DefaultConfigfsRoot = "/sys/kernel/config"

	// DefaultPort is the IANA-assigned iSCSI port.
	DefaultPort int32 = 3260

	// OwnedIQNPrefix prefixes every target IQN the agent creates.  Targets
	// carrying it are owned by pillar-csi (see ListTargets); targets without
	// it are never touched.
	OwnedIQNPrefix = "iqn.2026-01.com.bhyoo.pillar-csi:"

	// MaxIQNLength is the longest iSCSI name LIO accepts (ISCSI_IQN_LEN
	// 224 including the terminating NUL).
	MaxIQNLength = 223

	// HBAIndex is the index of the single iblock HBA holding every
	// pillar-csi backstore (core/iblock_<HBAIndex>).  A fixed, otherwise
	// unlikely index keeps pillar-csi's backstores apart from HBAs created
	// by targetcli (which numbers from 0) and makes the backstore path a
	// pure function of the target IQN.  An HBA holds no resources of its
	// own, so it is created on demand and never removed.
	HBAIndex = 3260

	// TPGName is the only target portal group of every target.
	TPGName = "tpgt_1"

	// LUNName is the only LUN of every target (LUN 0).
	LUNName = "lun_0"

	// The symlink inside lun/lun_0 named by lunLinkName binds the LUN to
	// its backstore; LIO accepts any name.
	lunLinkName = "backstore"

	// The symlink inside an ACL's mapped LUN directory named by
	// mappedLUNLinkName points to tpgt_1/lun/lun_0.
	mappedLUNLinkName = "lun"

	// LIO prints wwn/vpd_unit_serial with unitSerialPrefix.
	unitSerialPrefix = "T10 VPD Unit Serial Number:"
)

// ErrTargetNotFound reports that the target IQN has no TPG in configfs, i.e.
// the volume is not exported.
var ErrTargetNotFound = errors.New("iSCSI target not found")

// ErrDeviceHeld reports that the backstore could not be enabled because its
// block device is held exclusively on the storage node (local attach).  It is
// the error nvmeof device claimers report, so one claimer serves both
// protocols.
var ErrDeviceHeld = nvmeof.ErrDeviceHeld

// DeviceClaimer takes an exclusive claim on a block device; see
// nvmeof.DeviceClaimer.
type DeviceClaimer = nvmeof.DeviceClaimer

// FS is the file-system view of configfs used by this package.  Production
// uses OSFS; tests use an emulation of the kernel's configfs side effects
// (see package liotest).
type FS interface {
	// Mkdir creates one directory; its parent must exist.  An existing
	// path fails with an error satisfying errors.Is(err, fs.ErrExist).
	Mkdir(path string) error
	// Rmdir removes one configfs item directory.
	Rmdir(path string) error
	// WriteFile writes content to an attribute.
	WriteFile(path, content string) error
	// ReadFile reads an attribute.
	ReadFile(path string) ([]byte, error)
	// Symlink creates newname pointing to oldname.
	Symlink(oldname, newname string) error
	// Readlink returns the target of the symlink at path.
	Readlink(path string) (string, error)
	// Unlink removes the symlink at path.
	Unlink(path string) error
	// Lstat returns file info without following symlinks.
	Lstat(path string) (fs.FileInfo, error)
	// ReadDir lists a directory.
	ReadDir(path string) ([]fs.DirEntry, error)
}

// OSFS is the production FS: plain system calls on the configfs mount.
type OSFS struct{}

var _ FS = OSFS{}

// Mkdir implements FS.
func (OSFS) Mkdir(path string) error { return os.Mkdir(path, 0o750) } //nolint:wrapcheck // wrapped by callers

// Rmdir implements FS.
func (OSFS) Rmdir(path string) error { return syscall.Rmdir(path) } //nolint:wrapcheck // wrapped by callers

// WriteFile implements FS.
func (OSFS) WriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600) //nolint:wrapcheck // wrapped by callers
}

// ReadFile implements FS.
func (OSFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec,wrapcheck // configfs path from validated IQNs; wrapped by callers
}

// Symlink implements FS.
func (OSFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname) //nolint:wrapcheck // wrapped by callers
}

// Readlink implements FS.
func (OSFS) Readlink(path string) (string, error) { return os.Readlink(path) } //nolint:wrapcheck // wrapped by callers

// Unlink implements FS.
func (OSFS) Unlink(path string) error { return syscall.Unlink(path) } //nolint:wrapcheck // wrapped by callers

// Lstat implements FS.
func (OSFS) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) } //nolint:wrapcheck // wrapped by callers

// ReadDir implements FS.
func (OSFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path) //nolint:wrapcheck // wrapped by callers
}

// configfs bundles an FS with the configfs root so helpers produce errors
// naming the operation and the path.
type configfs struct {
	fs   FS
	root string
}

func newConfigfs(fsys FS, root string) configfs {
	if fsys == nil {
		fsys = OSFS{}
	}
	if root == "" {
		root = DefaultConfigfsRoot
	}
	return configfs{fs: fsys, root: root}
}

func (c configfs) targetDir() string { return filepath.Join(c.root, "target") }
func (c configfs) iscsiDir() string  { return filepath.Join(c.root, "target", "iscsi") }
func (c configfs) hbaDir() string {
	return filepath.Join(c.root, "target", "core", fmt.Sprintf("iblock_%d", HBAIndex))
}

// exists reports whether path exists (without following symlinks).
func (c configfs) exists(path string) (bool, error) {
	_, err := c.fs.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("configfs stat %q: %w", path, err)
}

// ensureDir creates the configfs item at path unless it already exists.  Its
// parent must exist.
func (c configfs) ensureDir(path string) error {
	err := c.fs.Mkdir(path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("configfs mkdir %q: %w", path, err)
	}
	fi, statErr := c.fs.Lstat(path)
	if statErr != nil {
		return fmt.Errorf("configfs stat %q after EEXIST: %w", path, statErr)
	}
	if !fi.IsDir() {
		return fmt.Errorf("configfs mkdir %q: exists and is not a directory (mode=%s)", path, fi.Mode())
	}
	return nil
}

// ensureISCSIFabric creates <root>/target/iscsi, which registers the iscsi
// fabric with the target core.  <root>/target must exist (target_core_mod
// loaded).
func (c configfs) ensureISCSIFabric() error {
	ok, err := c.exists(c.targetDir())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("configfs %q: %w (is target_core_mod loaded?)", c.targetDir(), fs.ErrNotExist)
	}
	return c.ensureDir(c.iscsiDir())
}

// readAttr reads an attribute and trims the trailing newline/NULs and space
// padding.  A missing attribute reads as "".
func (c configfs) readAttr(path string) (string, error) {
	data, err := c.fs.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("configfs read %q: %w", path, err)
	}
	return strings.TrimSpace(strings.TrimRight(string(data), "\x00")), nil
}

// writeAttr writes value to the attribute at path and reads it back.
func (c configfs) writeAttr(path, value string) error {
	err := c.fs.WriteFile(path, value)
	if err != nil {
		return fmt.Errorf("configfs write %q = %q: %w", path, value, err)
	}
	got, err := c.readAttr(path)
	if err != nil {
		return fmt.Errorf("configfs verify %q after write %q: %w", path, value, err)
	}
	if got != value {
		return fmt.Errorf("configfs verify %q after write %q: got %q", path, value, got)
	}
	return nil
}

// ensureAttr writes value unless the attribute already holds it.
func (c configfs) ensureAttr(path, value string) error {
	got, err := c.readAttr(path)
	if err != nil {
		return err
	}
	if got == value {
		return nil
	}
	return c.writeAttr(path, value)
}

// resolvedLinkTarget returns target made absolute relative to linkPath.
func resolvedLinkTarget(linkPath, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
}

// ensureSymlink creates newname → oldname unless newname already resolves to
// oldname.  A link to anything else is an error.
func (c configfs) ensureSymlink(oldname, newname string) error {
	existing, err := c.fs.Readlink(newname)
	switch {
	case err == nil:
		if resolvedLinkTarget(newname, existing) == filepath.Clean(oldname) {
			return nil
		}
		return fmt.Errorf("configfs symlink %q → %q: already points to %q", newname, oldname, existing)
	case errors.Is(err, fs.ErrNotExist):
		linkErr := c.fs.Symlink(oldname, newname)
		if linkErr != nil {
			return fmt.Errorf("configfs symlink %q → %q: %w", newname, oldname, linkErr)
		}
		return nil
	default:
		return fmt.Errorf("configfs readlink %q: %w", newname, err)
	}
}

// symlinks returns the names and resolved targets of the symlinks directly
// inside dir.  A missing dir has none.
func (c configfs) symlinks(dir string) (map[string]string, error) {
	entries, err := c.fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil //nolint:nilnil // a missing dir has no symlinks; a nil map reads as empty
		}
		return nil, fmt.Errorf("configfs readdir %q: %w", dir, err)
	}
	links := make(map[string]string)
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		p := filepath.Join(dir, e.Name())
		target, err := c.fs.Readlink(p)
		if err != nil {
			return nil, fmt.Errorf("configfs readlink %q: %w", p, err)
		}
		links[e.Name()] = resolvedLinkTarget(p, target)
	}
	return links, nil
}

// subdirs returns the names of the directories directly inside dir.  A
// missing dir has none.
func (c configfs) subdirs(dir string) ([]string, error) {
	entries, err := c.fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("configfs readdir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// removeSymlinks unlinks every symlink directly inside dir.
func (c configfs) removeSymlinks(dir string) error {
	links, err := c.symlinks(dir)
	if err != nil {
		return err
	}
	for name := range links {
		p := filepath.Join(dir, name)
		err = c.fs.Unlink(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("configfs unlink %q: %w", p, err)
		}
	}
	return nil
}

// removeDir removes the configfs item at path and reads back that it is
// gone.  A missing path is not an error.
func (c configfs) removeDir(path string) error {
	err := c.fs.Rmdir(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("configfs rmdir %q: %w", path, err)
	}
	ok, err := c.exists(path)
	if err != nil {
		return fmt.Errorf("configfs verify rmdir %q: %w", path, err)
	}
	if ok {
		return fmt.Errorf("configfs verify rmdir %q: still present after removal", path)
	}
	return nil
}
