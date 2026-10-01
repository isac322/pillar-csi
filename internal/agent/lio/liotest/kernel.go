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

// Package liotest emulates the kernel side of the LIO configfs tree on a
// regular directory, for tests of package lio and of the agent's iSCSI
// handler.  It reproduces the behavior the agent relies on: objects are
// created by mkdir with their default groups and attribute files, rmdir
// removes them (and refuses while user-created children or links remain),
// "udev_path=" written to a backstore's control shows up in its info
// attribute as "UDEV PATH: <path>", a
// backstore can be enabled only with a device that is not held, its unit
// serial cannot change while a LUN uses it, its attrib/emulate_tpu accepts 1
// only once enabled and only for a device supporting discard, portal
// names must parse, attrib/authentication accepts only 0/1 (TPG) or
// -1/0/1 (node ACL), and a node ACL's auth attributes store values below
// 256 bytes verbatim (read back with one trailing newline), refuse empty
// writes, and keep the read-only authenticate_target in sync.
//
// It is test support only; production code uses lio.OSFS.
package liotest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// Kernel is an emulated LIO configfs rooted at Root.
type Kernel struct {
	// Root is the configfs root (the directory containing "target").
	Root string

	mu sync.Mutex
	// auto marks entries created by the emulated kernel (default groups and
	// attribute files), which rmdir removes together with their item.
	auto map[string]bool
	// held marks devices held exclusively outside LIO (local attach).
	held map[string]bool
	// failWrites injects write errors per attribute path.
	failWrites map[string]error
	// dropWrites makes writes per attribute path succeed without effect.
	dropWrites map[string]bool
	// noDiscard marks devices without discard support.
	noDiscard map[string]bool
}

var _ lio.FS = (*Kernel)(nil)

// New returns a Kernel with target_core_mod loaded: <root>/target/core
// exists, <root>/target/iscsi does not (the first mkdir registers it).
func New(root string) (*Kernel, error) {
	k := NewUnloaded(root)
	for _, dir := range []string{filepath.Join(root, "target"), filepath.Join(root, "target", "core")} {
		err := os.MkdirAll(dir, 0o750)
		if err != nil {
			return nil, err //nolint:wrapcheck // test support
		}
	}
	return k, nil
}

// NewUnloaded returns a Kernel whose root lacks <root>/target, i.e. the LIO
// modules are not loaded.
func NewUnloaded(root string) *Kernel {
	return &Kernel{
		Root:       root,
		auto:       map[string]bool{},
		held:       map[string]bool{},
		failWrites: map[string]error{},
		dropWrites: map[string]bool{},
		noDiscard:  map[string]bool{},
	}
}

// HoldDevice marks path as held exclusively by someone other than LIO.
func (k *Kernel) HoldDevice(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held[path] = true
}

// ReleaseDevice undoes HoldDevice.
func (k *Kernel) ReleaseDevice(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.held, path)
}

// FailWrite makes every later write to path fail with err (nil clears it).
func (k *Kernel) FailWrite(path string, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err == nil {
		delete(k.failWrites, path)
		return
	}
	k.failWrites[path] = err
}

// DropWrite makes every later write to path succeed without changing it, so
// only the read-back can detect it.
func (k *Kernel) DropWrite(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.dropWrites[path] = true
}

// DisableDiscard marks the block device path as not supporting discard:
// enabling thin provisioning (emulate_tpu=1) on a backstore of it fails
// with ENOSYS, as target_try_configure_unmap does in the kernel.
func (k *Kernel) DisableDiscard(path string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.noDiscard[path] = true
}

// Claimer is a DeviceClaimer consistent with the emulation: a device is held
// when HoldDevice marked it or an enabled backstore uses it.
func (k *Kernel) Claimer() lio.DeviceClaimer {
	return func(path string) (func() error, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.held[path] || k.claimedByBackstoreLocked(path) {
			return nil, &fs.PathError{Op: "exclusive open", Path: path, Err: nvmeof.ErrDeviceHeld}
		}
		return func() error { return nil }, nil
	}
}

var (
	reHBA    = regexp.MustCompile(`^[a-z]+_\d+$`)
	reTPG    = regexp.MustCompile(`^tpgt_\d+$`)
	reLUN    = regexp.MustCompile(`^lun_\d+$`)
	errnoErr = func(op, path string, errno syscall.Errno) error {
		return &fs.PathError{Op: op, Path: path, Err: errno}
	}
)

// kind classifies a configfs path.
type kind int

const (
	kindUnknown kind = iota
	kindFabric
	kindHBA
	kindBackstore
	kindTarget
	kindTPG
	kindLUN
	kindPortal
	kindACL
	kindMappedLUN
)

// dirACLs is the TPG default group holding node ACLs.
const dirACLs = "acls"

// classify returns the kind of the configfs object at path and the path's
// components relative to Root.
func (k *Kernel) classify(path string) (kd kind, parts []string) {
	rel, err := filepath.Rel(k.Root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return kindUnknown, nil
	}
	p := strings.Split(rel, string(filepath.Separator))
	if len(p) < 2 || p[0] != "target" {
		return kindUnknown, p
	}
	switch p[1] {
	case "core":
		return classifyCore(p), p
	case "iscsi":
		return classifyISCSI(p), p
	}
	return kindUnknown, p
}

// classifyCore classifies the components of a path under target/core.
func classifyCore(p []string) kind {
	switch {
	case len(p) == 3 && reHBA.MatchString(p[2]):
		return kindHBA
	case len(p) == 4 && reHBA.MatchString(p[2]):
		return kindBackstore
	}
	return kindUnknown
}

// classifyISCSI classifies the components of a path under target/iscsi.
func classifyISCSI(p []string) kind {
	switch {
	case len(p) == 2:
		return kindFabric
	case len(p) == 3:
		return kindTarget
	case len(p) == 4 && reTPG.MatchString(p[3]):
		return kindTPG
	case len(p) == 6 && p[4] == "lun" && reLUN.MatchString(p[5]):
		return kindLUN
	case len(p) == 6 && p[4] == "np":
		return kindPortal
	case len(p) == 6 && p[4] == dirACLs:
		return kindACL
	case len(p) == 7 && p[4] == dirACLs && reLUN.MatchString(p[6]):
		return kindMappedLUN
	}
	return kindUnknown
}

// Mkdir implements lio.FS.
func (k *Kernel) Mkdir(path string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	kd, p := k.classify(path)
	_, statErr := os.Lstat(path)
	if statErr == nil {
		return errnoErr("mkdir", path, syscall.EEXIST)
	}
	if kd == kindUnknown {
		return errnoErr("mkdir", path, syscall.EPERM)
	}
	if kd == kindPortal && !validPortalName(p[5]) {
		return errnoErr("mkdir", path, syscall.EINVAL)
	}
	err := os.Mkdir(path, 0o750)
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	k.populate(kd, path)
	return nil
}

// validPortalName reports whether the kernel accepts name as an np entry:
// it must parse, and IPv6 hosts must be bracketed.
func validPortalName(name string) bool {
	host, _, err := lio.ParsePortal(name)
	return err == nil && (!strings.Contains(host, ":") || strings.HasPrefix(name, "["))
}

// populate creates the default groups and attributes the kernel adds to a
// freshly made object of kind kd.
func (k *Kernel) populate(kd kind, path string) {
	switch kd {
	case kindFabric:
		k.autoFile(path, "lio_version", "Datera Inc. iSCSI Target (emulated)")
		k.autoDir(path, "discovery_auth")
	case kindHBA:
		k.autoFile(path, "hba_info", "")
	case kindBackstore:
		k.autoFile(path, "enable", "0")
		k.autoFile(path, "udev_path", "")
		k.autoFile(path, "control", "")
		k.autoFile(path, "info", iblockInfo(""))
		attrib := k.autoDir(path, "attrib")
		k.autoFile(attrib, "emulate_tpu", "0")
		wwn := k.autoDir(path, "wwn")
		k.autoFile(wwn, "vpd_unit_serial", "T10 VPD Unit Serial Number: ")
	case kindTPG:
		k.autoFile(path, "enable", "0")
		for _, d := range []string{"lun", "np", dirACLs, "param", "auth"} {
			k.autoDir(path, d)
		}
		attrib := k.autoDir(path, "attrib")
		k.autoFile(attrib, "authentication", "1")
		k.autoFile(attrib, "generate_node_acls", "0")
		k.autoFile(attrib, "cache_dynamic_acls", "0")
		k.autoFile(attrib, "demo_mode_write_protect", "1")
	case kindLUN:
		k.autoFile(path, "alua_tg_pt_gp", "default_tg_pt_gp")
		k.autoDir(path, "statistics")
	case kindACL:
		k.autoDir(path, "param")
		attrib := k.autoDir(path, "attrib")
		k.autoFile(attrib, "authentication", "-1")
		auth := k.autoDir(path, "auth")
		for _, name := range aclAuthStrings {
			k.autoFile(auth, name, "")
		}
		k.autoFile(auth, "authenticate_target", "0")
	case kindMappedLUN:
		k.autoFile(path, "write_protect", "0")
	case kindTarget, kindPortal, kindUnknown:
	}
}

func (k *Kernel) autoFile(dir, name, content string) {
	p := filepath.Join(dir, name)
	err := os.WriteFile(p, []byte(content+"\n"), 0o600)
	if err != nil {
		panic(err)
	}
	k.auto[p] = true
}

func (k *Kernel) autoDir(dir, name string) string {
	p := filepath.Join(dir, name)
	err := os.Mkdir(p, 0o750)
	if err != nil {
		panic(err)
	}
	k.auto[p] = true
	return p
}

// Rmdir implements lio.FS.
func (k *Kernel) Rmdir(path string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	fi, err := os.Lstat(path)
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	if !fi.IsDir() {
		return errnoErr("rmdir", path, syscall.ENOTDIR)
	}
	if k.auto[path] {
		return errnoErr("rmdir", path, syscall.EPERM)
	}
	if !k.onlyAutoChildren(path) {
		return errnoErr("rmdir", path, syscall.ENOTEMPTY)
	}
	if k.linkedTo(path) {
		return errnoErr("rmdir", path, syscall.EBUSY)
	}
	err = os.RemoveAll(path)
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	for p := range k.auto {
		if strings.HasPrefix(p, path+string(filepath.Separator)) {
			delete(k.auto, p)
		}
	}
	return nil
}

func (k *Kernel) onlyAutoChildren(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !k.auto[p] {
			return false
		}
		if e.IsDir() && !k.onlyAutoChildren(p) {
			return false
		}
	}
	return true
}

// linkedTo reports whether any symlink under target/iscsi resolves to path.
func (k *Kernel) linkedTo(path string) bool {
	found := false
	walkErr := filepath.WalkDir(filepath.Join(k.Root, "target", "iscsi"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil //nolint:nilerr // best-effort scan of the emulated tree
		}
		dest, linkErr := os.Readlink(p)
		if linkErr == nil && resolve(p, dest) == path {
			found = true
		}
		return nil
	})
	if walkErr != nil {
		// The callback never fails, so WalkDir cannot either.
		panic(walkErr)
	}
	return found
}

func resolve(link, dest string) string {
	if filepath.IsAbs(dest) {
		return filepath.Clean(dest)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(link), dest))
}

// iblockInfo is the info attribute of an iblock backstore whose control
// received udev_path=udev ("" for none).
func iblockInfo(udev string) string {
	info := "Status: DEACTIVATED  Max Queue Depth: 0  SectorSize: 512  HwMaxSectors: 1024\n" +
		"        iBlock device: None"
	if udev != "" {
		info += "  UDEV PATH: " + udev
	}
	return info + "  readonly: 0"
}

// infoDevice returns the device control configured for the backstore dir.
func infoDevice(dir string) string {
	_, rest, ok := strings.Cut(readTrim(filepath.Join(dir, "info")), "UDEV PATH: ")
	if !ok {
		return ""
	}
	return strings.Fields(rest)[0]
}

func (k *Kernel) claimedByBackstoreLocked(device string) bool {
	matches, err := filepath.Glob(filepath.Join(k.Root, "target", "core", "*", "*", "enable"))
	if err != nil {
		// Only ErrBadPattern is possible, and the pattern is constant.
		panic(err)
	}
	for _, m := range matches {
		if readTrim(m) == "1" && infoDevice(filepath.Dir(m)) == device {
			return true
		}
	}
	return false
}

// WriteFile implements lio.FS.
func (k *Kernel) WriteFile(path, content string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	done, err := k.injectedWriteLocked(path)
	if done {
		return err
	}
	if k.isACLAuthAttr(path) {
		return writeACLAuth(path, content)
	}
	return k.writeAttrLocked(path, strings.TrimSpace(content))
}

// aclAuthStrings are the writable string attributes of a node ACL's auth
// group (lio_target_nacl_auth_attrs).
var aclAuthStrings = []string{"userid", "password", "userid_mutual", "password_mutual"}

// isACLAuthAttr reports whether path is an attribute of a node ACL's auth
// group.
func (k *Kernel) isACLAuthAttr(path string) bool {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "auth" {
		return false
	}
	kd, _ := k.classify(filepath.Dir(dir))
	return kd == kindACL
}

// writeACLAuth handles a write to a node ACL's auth attribute like
// __DEF_NACL_AUTH_STR: a value of 256 bytes or more is EINVAL, the bytes up
// to the first NUL are stored verbatim (no trimming), and
// authenticate_target (read-only) becomes 1 while both mutual values are
// set, a value starting with "NULL" counting as unset.
func writeACLAuth(path, content string) error {
	name := filepath.Base(path)
	if !slices.Contains(aclAuthStrings, name) {
		return errnoErr("write", path, syscall.EACCES)
	}
	if content == "" {
		// configfs_write_iter: an empty write never reaches the store.
		return errnoErr("write", path, syscall.EFAULT)
	}
	if len(content) >= 256 {
		return errnoErr("write", path, syscall.EINVAL)
	}
	value, _, _ := strings.Cut(content, "\x00")
	err := os.WriteFile(path, []byte(value+"\n"), 0o600)
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	dir := filepath.Dir(path)
	isSet := func(n string) bool {
		data, readErr := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // test tree
		v := strings.TrimSuffix(string(data), "\n")
		return readErr == nil && v != "" && !strings.HasPrefix(v, "NULL")
	}
	authenticate := "0"
	if isSet("userid_mutual") && isSet("password_mutual") {
		authenticate = "1"
	}
	return put(filepath.Join(dir, "authenticate_target"), authenticate)
}

// injectedWriteLocked applies FailWrite and DropWrite and the checks every
// attribute write passes, and reports whether the write ends there with
// err.  The caller must hold k.mu.
func (k *Kernel) injectedWriteLocked(path string) (done bool, err error) {
	failErr := k.failWrites[path]
	if failErr != nil {
		return true, &fs.PathError{Op: "write", Path: path, Err: failErr}
	}
	if k.dropWrites[path] {
		return true, nil
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return true, err //nolint:wrapcheck // emulated syscall
	}
	if fi.IsDir() {
		return true, errnoErr("write", path, syscall.EISDIR)
	}
	return false, nil
}

// writeAttrLocked stores value in the attribute at path with the side
// effects and checks of its kind; k.mu must be held.
func (k *Kernel) writeAttrLocked(path, value string) error {
	dir, name := filepath.Dir(path), filepath.Base(path)
	done, err := k.writeAttrHandoffLocked(dir, name, path, value)
	if done {
		return err
	}
	return put(path, value)
}

// writeAttrHandoffLocked dispatches writes with side effects and checks
// beyond storing the value and reports whether the write ends there; the
// caller must hold k.mu.
func (k *Kernel) writeAttrHandoffLocked(dir, name, path, value string) (bool, error) {
	dirKind, _ := k.classify(dir)
	parent := filepath.Base(dir)
	switch {
	case dirKind == kindBackstore && name == "control":
		return true, writeBackstoreControl(path, value)
	case dirKind == kindBackstore && name == "enable":
		return true, k.writeBackstoreEnableLocked(path, value)
	case name == "emulate_tpu" && parent == "attrib" && k.isBackstore(filepath.Dir(dir)):
		return true, k.writeEmulateTPULocked(path, value)
	case name == "vpd_unit_serial" && parent == "wwn":
		return true, k.writeUnitSerialLocked(dir, path, value)
	case name == "enable" && dirKind == kindTPG:
		return true, putBool(path, value)
	case name == "authentication" && parent == "attrib":
		return true, k.writeAuthenticationLocked(path, value)
	}
	return false, nil
}

// writeUnitSerialLocked handles a write to a backstore's wwn/vpd_unit_serial;
// k.mu must be held.
func (k *Kernel) writeUnitSerialLocked(wwnDir, path, value string) error {
	if k.linkedTo(filepath.Dir(wwnDir)) {
		return errnoErr("write", path, syscall.EINVAL)
	}
	return put(path, "T10 VPD Unit Serial Number: "+value)
}

// putBool writes value, which must be "0" or "1", to the attribute at path.
func putBool(path, value string) error {
	if value != "0" && value != "1" {
		return errnoErr("write", path, syscall.EINVAL)
	}
	return put(path, value)
}

// writeAuthenticationLocked handles a write to attrib/authentication: the
// TPG's accepts 0 or 1 (iscsit_ta_authentication), a node ACL's also -1
// (inherit, iscsi_nacl_attrib_authentication_store); k.mu must be held.
func (k *Kernel) writeAuthenticationLocked(path, value string) error {
	owner, _ := k.classify(filepath.Dir(filepath.Dir(path)))
	switch {
	case owner == kindTPG && (value == "0" || value == "1"):
	case owner == kindACL && (value == "0" || value == "1" || value == "-1"):
	default:
		return errnoErr("write", path, syscall.EINVAL)
	}
	return put(path, value)
}

// writeBackstoreControl handles a write to a backstore's control attribute.
func writeBackstoreControl(path, value string) error {
	dir := filepath.Dir(path)
	udev, ok := strings.CutPrefix(value, "udev_path=")
	if !ok {
		return errnoErr("write", path, syscall.EINVAL)
	}
	if readTrim(filepath.Join(dir, "enable")) == "1" {
		return errnoErr("write", path, syscall.EEXIST)
	}
	return put(filepath.Join(dir, "info"), iblockInfo(udev))
}

// writeBackstoreEnableLocked handles a write to a backstore's enable
// attribute; k.mu must be held.
func (k *Kernel) writeBackstoreEnableLocked(path, value string) error {
	udev := infoDevice(filepath.Dir(path))
	switch {
	case value != "1" || udev == "":
		return errnoErr("write", path, syscall.EINVAL)
	case readTrim(path) == "1":
		return errnoErr("write", path, syscall.EEXIST)
	case k.held[udev] || k.claimedByBackstoreLocked(udev):
		return errnoErr("write", path, syscall.EBUSY)
	}
	return put(path, "1")
}

func (k *Kernel) isBackstore(path string) bool {
	kd, _ := k.classify(path)
	return kd == kindBackstore
}

// writeEmulateTPULocked handles a write to a backstore's attrib/emulate_tpu
// like emulate_tpu_store: enabling it configures UNMAP from the backing
// device, which requires an enabled backstore (ENODEV) on a device with
// discard support (ENOSYS); k.mu must be held.
func (k *Kernel) writeEmulateTPULocked(path, value string) error {
	if value != "0" && value != "1" {
		return errnoErr("write", path, syscall.EINVAL)
	}
	if value == "1" {
		dir := filepath.Dir(filepath.Dir(path))
		switch {
		case readTrim(filepath.Join(dir, "enable")) != "1":
			return errnoErr("write", path, syscall.ENODEV)
		case k.noDiscard[infoDevice(dir)]:
			return errnoErr("write", path, syscall.ENOSYS)
		}
	}
	return put(path, value)
}

func put(path, value string) error {
	return os.WriteFile(path, []byte(value+"\n"), 0o600) //nolint:wrapcheck // emulated syscall
}

// readTrim returns the trimmed content of path, or "" when it cannot be read
// (an absent attribute reads as unset, matching the emulated kernel).
func readTrim(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // test tree
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ReadFile implements lio.FS.
func (k *Kernel) ReadFile(path string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if filepath.Base(path) == "control" {
		return nil, errnoErr("read", path, syscall.EACCES)
	}
	return os.ReadFile(path) //nolint:gosec,wrapcheck // test tree
}

// Symlink implements lio.FS.
func (k *Kernel) Symlink(oldname, newname string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	parentKind, _ := k.classify(filepath.Dir(newname))
	targetKind, _ := k.classify(oldname)
	switch {
	case parentKind == kindLUN && targetKind == kindBackstore:
		if readTrim(filepath.Join(oldname, "enable")) != "1" {
			return errnoErr("symlink", newname, syscall.ENODEV)
		}
	case parentKind == kindMappedLUN && targetKind == kindLUN:
		_, statErr := os.Stat(oldname)
		if statErr != nil {
			return errnoErr("symlink", newname, syscall.ENOENT)
		}
	default:
		return errnoErr("symlink", newname, syscall.EPERM)
	}
	links, err := os.ReadDir(filepath.Dir(newname))
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	for _, e := range links {
		if e.Type()&fs.ModeSymlink != 0 {
			return errnoErr("symlink", newname, syscall.EEXIST)
		}
	}
	return os.Symlink(oldname, newname) //nolint:wrapcheck // emulated syscall
}

// Readlink implements lio.FS.
func (*Kernel) Readlink(path string) (string, error) {
	return os.Readlink(path) //nolint:wrapcheck // emulated syscall
}

// Unlink implements lio.FS.
func (k *Kernel) Unlink(path string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	fi, err := os.Lstat(path)
	if err != nil {
		return err //nolint:wrapcheck // emulated syscall
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return errnoErr("unlink", path, syscall.EPERM)
	}
	return os.Remove(path) //nolint:wrapcheck // emulated syscall
}

// Lstat implements lio.FS.
func (*Kernel) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path) //nolint:wrapcheck // emulated syscall
}

// ReadDir implements lio.FS.
func (*Kernel) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path) //nolint:wrapcheck // emulated syscall
}

// Exists reports whether path exists; a test convenience.
func (*Kernel) Exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
