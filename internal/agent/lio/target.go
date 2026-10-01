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

package lio

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// Target describes the iSCSI export of one volume: an iblock backstore on
// DevicePath, target IQN with TPG tpgt_1, LUN 0 and one network portal.
// It is a pure data struct; the methods drive configfs.
//
// Lifecycle and reachability.  A host can log in only while the TPG is
// enabled and a portal exists, so Prepare configures everything except the
// portal and the TPG enable, and Activate adds both.  A caller restoring many
// exports prepares all of them before activating any.
//
// Local attach.  LIO's iblock backstore holds an exclusive claim on its
// block device for as long as the backstore exists, so a volume attached
// directly on the storage node cannot coexist with it.  A locally attached
// export therefore keeps its target, TPG, portal and ACLs, but has its TPG
// disabled (terminating every session) and neither a LUN (no
// tpgt_1/lun/lun_0 directory, no mapped LUNs) nor a backstore.  Leaving
// local attach re-creates backstore, LUN and mapped LUNs; enabling the
// backstore is LIO's own exclusive open of the device, so it fails with
// EBUSY (ErrDeviceHeld) while the node still holds the device — there is no
// window in which both can own it.
type Target struct {
	// ConfigfsRoot is the configfs mount; "" means DefaultConfigfsRoot.
	ConfigfsRoot string

	// FS performs the configfs operations; nil means OSFS.
	FS FS

	// IQN is the target name (OwnedIQNPrefix + volume ID with "/" → ".").
	IQN string

	// DevicePath is the block device backing LUN 0.  Required unless
	// LocalAttach is set.
	DevicePath string

	// BindAddress is the advertised portal address.  Only its address
	// family is used: the portal listens on the wildcard address of that
	// family (see Portal).
	BindAddress string

	// Port is the TCP port of the portal.
	Port int32

	// ACLEnabled selects explicit node ACLs (generate_node_acls=0) instead
	// of demo mode, in which any initiator may log in.
	ACLEnabled bool

	// AllowedInitiators are initiator IQNs Prepare grants a node ACL.
	AllowedInitiators []string

	// UnitSerial is the T10 VPD unit serial of the backstore; "" selects
	// DeriveUnitSerial(IQN).
	UnitSerial string

	// LocalAttach puts the export in the local-attach state (see above).
	LocalAttach bool

	// DeviceClaimer probes that the device is not held before the backstore
	// is enabled, and verifies that it is free after the backstore was
	// removed for a local attach.  nil selects nvmeof.ClaimDeviceExclusively.
	DeviceClaimer DeviceClaimer

	// DiscardUnsupported, when set, is called with DevicePath when the
	// backing device does not support discard, so the backstore cannot
	// advertise thin provisioning (UNMAP) and the export is served without
	// it (see ensureThinProvisioning).
	DiscardUnsupported func(device string)

	// CHAP, when set, enables CHAP authentication: the TPG attribute
	// authentication is 1 and every node ACL carries these credentials
	// (see ensureACLAuth).  nil keeps authentication=0.  CHAP requires
	// ACLEnabled: the credentials live on explicit node ACLs.
	CHAP *CHAP
}

func (t *Target) cfs() configfs { return newConfigfs(t.FS, t.ConfigfsRoot) }

func (t *Target) targetDir() string        { return filepath.Join(t.cfs().iscsiDir(), t.IQN) }
func (t *Target) tpgDir() string           { return filepath.Join(t.targetDir(), TPGName) }
func (t *Target) lunDir() string           { return filepath.Join(t.tpgDir(), "lun", LUNName) }
func (t *Target) aclsDir() string          { return filepath.Join(t.tpgDir(), "acls") }
func (t *Target) npDir() string            { return filepath.Join(t.tpgDir(), "np") }
func (t *Target) attribDir() string        { return filepath.Join(t.tpgDir(), "attrib") }
func (t *Target) tpgEnable() string        { return filepath.Join(t.tpgDir(), "enable") }
func (t *Target) aclDir(iqn string) string { return filepath.Join(t.aclsDir(), iqn) }

// BackstoreName returns the name of the target's iblock backstore: the IQN
// without OwnedIQNPrefix, i.e. the volume ID with "/" replaced by ".".
func (t *Target) BackstoreName() string {
	return BackstoreNameForIQN(t.IQN)
}

// BackstoreNameForIQN returns the backstore name of the target named iqn.
func BackstoreNameForIQN(iqn string) string {
	return strings.TrimPrefix(iqn, OwnedIQNPrefix)
}

// BackstoreDir returns the configfs directory of the target's backstore.
func (t *Target) BackstoreDir() string {
	return filepath.Join(t.cfs().hbaDir(), t.BackstoreName())
}

func (t *Target) claimer() DeviceClaimer {
	if t.DeviceClaimer != nil {
		return t.DeviceClaimer
	}
	return nvmeof.ClaimDeviceExclusively
}

// Portal returns the name of the target's network portal directory: the
// wildcard address of BindAddress's family and Port, "0.0.0.0:<port>" or
// "[::]:<port>".
//
// Like nvmet's addr_traddr, the portal is bound in the network namespace of
// the process writing configfs.  The advertised BindAddress (what initiators
// connect to) need not be assigned to an interface visible there, so the
// kernel listens on the wildcard; LIO shares one listening socket among all
// targets whose portal has the same address and port.
func (t *Target) Portal() (string, error) {
	ip := net.ParseIP(t.BindAddress)
	if ip == nil {
		return "", fmt.Errorf("iSCSI target %q: invalid bind address %q: want an IP literal", t.IQN, t.BindAddress)
	}
	if t.Port < 1 || t.Port > 65535 {
		return "", fmt.Errorf("iSCSI target %q: invalid port %d", t.IQN, t.Port)
	}
	if ip.To4() != nil {
		return FormatPortal("0.0.0.0", t.Port), nil
	}
	return FormatPortal("::", t.Port), nil
}

// FormatPortal returns the LIO network portal name of addr and port:
// "addr:port" for IPv4 and "[addr]:port" for IPv6.
func FormatPortal(addr string, port int32) string {
	return net.JoinHostPort(addr, strconv.Itoa(int(port)))
}

// ParsePortal splits a LIO network portal name into address and port.
func ParsePortal(name string) (host string, port int32, err error) {
	host, portText, err := net.SplitHostPort(name)
	if err != nil {
		return "", 0, fmt.Errorf("parse iSCSI portal %q: %w", name, err)
	}
	port64, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port64 < 1 || port64 > 65535 {
		return "", 0, fmt.Errorf("parse iSCSI portal %q: invalid port %q", name, portText)
	}
	return host, int32(port64), nil
}

// ValidateIQN checks that name is usable as a LIO target or node ACL name.
func ValidateIQN(name string) error {
	switch {
	case name == "":
		return errors.New("iSCSI name is empty")
	case len(name) > MaxIQNLength:
		return fmt.Errorf("iSCSI name %q is %d bytes, longer than %d", name, len(name), MaxIQNLength)
	case strings.ContainsAny(name, "/\x00") || name == "." || name == "..":
		return fmt.Errorf("iSCSI name %q is not a valid configfs directory name", name)
	}
	return nil
}

func (t *Target) validate() error {
	err := ValidateIQN(t.IQN)
	if err != nil {
		return fmt.Errorf("iSCSI target: %w", err)
	}
	if !t.LocalAttach && t.DevicePath == "" {
		return fmt.Errorf("iSCSI target %q: device path is required", t.IQN)
	}
	for _, iqn := range t.AllowedInitiators {
		err = ValidateIQN(iqn)
		if err != nil {
			return fmt.Errorf("iSCSI target %q: allowed initiator: %w", t.IQN, err)
		}
	}
	if t.CHAP != nil {
		if !t.ACLEnabled {
			return fmt.Errorf("iSCSI target %q: CHAP requires explicit node ACLs", t.IQN)
		}
		err = t.CHAP.Validate()
		if err != nil {
			return fmt.Errorf("iSCSI target %q: %w", t.IQN, err)
		}
	}
	_, err = t.Portal()
	return err
}

// Exists reports whether the target's TPG exists in configfs.
func (t *Target) Exists() (bool, error) {
	return t.cfs().exists(t.tpgDir())
}

func (t *Target) requireTPG() error {
	ok, err := t.Exists()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("iSCSI target %q: %w", t.IQN, ErrTargetNotFound)
	}
	return nil
}

// Prepare configures everything a host needs before the target becomes
// reachable, in dependency order: the iscsi fabric, target and TPG; then
// either backstore and LUN 0 (or, with LocalAttach, the local-attach state);
// the TPG attributes (authentication 1 with CHAP and 0 without, demo mode or
// explicit ACLs); the node ACLs of AllowedInitiators; LUN 0 mapped into every
// node ACL; and the CHAP credentials of every node ACL, so existing ACLs
// pick up changed credentials.  It neither creates the portal nor enables
// the TPG (see Activate), and it never disables an already enabled TPG
// unless LocalAttach asks for it.  Every step is idempotent.
func (t *Target) Prepare() error {
	err := t.validate()
	if err != nil {
		return err
	}
	c := t.cfs()
	err = c.ensureISCSIFabric()
	if err != nil {
		return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
	}
	for _, dir := range []string{t.targetDir(), t.tpgDir()} {
		err = c.ensureDir(dir)
		if err != nil {
			return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
		}
	}
	if t.LocalAttach {
		_, err = t.detachLUN()
	} else {
		err = t.attachLUN()
	}
	if err != nil {
		return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
	}
	err = t.ensureTPGAttribs()
	if err != nil {
		return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
	}
	for _, iqn := range t.AllowedInitiators {
		err = t.allowInitiator(iqn)
		if err != nil {
			return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
		}
	}
	acls, err := t.Initiators()
	if err != nil {
		return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
	}
	for _, iqn := range acls {
		err = t.ensureACLAuth(iqn)
		if err != nil {
			return fmt.Errorf("prepare iSCSI target %q: %w", t.IQN, err)
		}
	}
	return nil
}

// Activate makes a prepared target reachable: it creates the portal,
// removes any other portal of the TPG, and enables the TPG (or, with
// LocalAttach, keeps it disabled).  It refuses to enable a TPG whose LUN 0
// is not bound to the target's backstore.
func (t *Target) Activate() error {
	portal, err := t.Portal()
	if err != nil {
		return err
	}
	c := t.cfs()
	err = t.requireTPG()
	if err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	err = c.ensureDir(t.npDir())
	if err != nil {
		return fmt.Errorf("activate iSCSI target %q: %w", t.IQN, err)
	}
	err = c.ensureDir(filepath.Join(t.npDir(), portal))
	if err != nil {
		return fmt.Errorf("activate iSCSI target %q: %w", t.IQN, err)
	}
	portals, err := c.subdirs(t.npDir())
	if err != nil {
		return fmt.Errorf("activate iSCSI target %q: %w", t.IQN, err)
	}
	for _, other := range portals {
		if other == portal {
			continue
		}
		err = c.removeDir(filepath.Join(t.npDir(), other))
		if err != nil {
			return fmt.Errorf("activate iSCSI target %q: remove stale portal: %w", t.IQN, err)
		}
	}
	if t.LocalAttach {
		err = t.disableTPG()
		if err != nil {
			return fmt.Errorf("activate iSCSI target %q: %w", t.IQN, err)
		}
		return nil
	}
	bound, err := t.lunBound()
	if err != nil {
		return fmt.Errorf("activate iSCSI target %q: %w", t.IQN, err)
	}
	if !bound {
		return fmt.Errorf("activate iSCSI target %q: %s is not bound to backstore %s", t.IQN, t.lunDir(), t.BackstoreDir())
	}
	err = c.ensureAttr(t.tpgEnable(), "1")
	if err != nil {
		return fmt.Errorf("activate iSCSI target %q: enable TPG: %w", t.IQN, err)
	}
	return nil
}

// Apply is Prepare followed by Activate.
func (t *Target) Apply() error {
	err := t.Prepare()
	if err != nil {
		return err
	}
	return t.Activate()
}

// ensureTPGAttribs writes the TPG's access policy.  The authentication
// attribute is the TPG's CHAP switch (see chap.go); the per-ACL credentials
// are written by ensureACLAuth.
func (t *Target) ensureTPGAttribs() error {
	c := t.cfs()
	authentication := "0"
	if t.CHAP != nil {
		authentication = "1"
	}
	attrs := [][2]string{{"authentication", authentication}}
	if t.ACLEnabled {
		attrs = append(attrs, [2]string{"generate_node_acls", "0"})
	} else {
		attrs = append(attrs,
			[2]string{"generate_node_acls", "1"},
			[2]string{"cache_dynamic_acls", "1"},
			[2]string{"demo_mode_write_protect", "0"})
	}
	for _, kv := range attrs {
		err := c.ensureAttr(filepath.Join(t.attribDir(), kv[0]), kv[1])
		if err != nil {
			return fmt.Errorf("TPG attribute %s: %w", kv[0], err)
		}
	}
	return nil
}

// disableTPG writes enable=0 unless the TPG already reads 0.  Disabling a
// TPG terminates all of its sessions.
func (t *Target) disableTPG() error {
	err := t.cfs().ensureAttr(t.tpgEnable(), "0")
	if err != nil {
		return fmt.Errorf("disable TPG of %q: %w", t.IQN, err)
	}
	return nil
}

// attachLUN creates the backstore, binds LUN 0 to it and maps LUN 0 into
// every node ACL of the TPG.
func (t *Target) attachLUN() error {
	err := t.ensureBackstore()
	if err != nil {
		return err
	}
	err = t.ensureLUN()
	if err != nil {
		return err
	}
	acls, err := t.cfs().subdirs(t.aclsDir())
	if err != nil {
		return err
	}
	for _, iqn := range acls {
		err = t.ensureMappedLUN(iqn)
		if err != nil {
			return err
		}
	}
	return nil
}

// ensureBackstore creates and enables the iblock backstore on DevicePath,
// enables thin provisioning (also on an already enabled backstore, e.g. one
// restored by an older agent) and pins its unit serial.  An enabled
// backstore must already use DevicePath: LIO cannot change the device of a
// configured backstore.
func (t *Target) ensureBackstore() error {
	c := t.cfs()
	dir := t.BackstoreDir()
	for _, d := range []string{filepath.Dir(c.hbaDir()), c.hbaDir()} {
		err := c.ensureDir(d)
		if err != nil {
			return fmt.Errorf("create backstore %q: %w", dir, err)
		}
	}
	existed, err := c.exists(dir)
	if err != nil {
		return err
	}
	err = c.ensureDir(dir)
	if err != nil {
		return fmt.Errorf("create backstore: %w", err)
	}
	enabled, err := c.readAttr(filepath.Join(dir, "enable"))
	if err != nil {
		return err
	}
	if enabled == "1" {
		udevPath, readErr := c.backstoreDevice(dir)
		if readErr != nil {
			return readErr
		}
		if udevPath != t.DevicePath {
			return fmt.Errorf("backstore %q is configured for device %q, want %q", dir, udevPath, t.DevicePath)
		}
		err = t.ensureThinProvisioning(dir)
		if err != nil {
			return err
		}
		return t.ensureUnitSerial()
	}
	err = t.configureBackstore(dir)
	if err != nil {
		if !existed {
			rmErr := c.removeDir(dir)
			if rmErr != nil {
				return errors.Join(err, //nolint:wrapcheck // both operands are wrapped/annotated
					fmt.Errorf("roll back backstore: %w", rmErr))
			}
		}
		return err
	}
	return t.ensureUnitSerial()
}

// configureBackstore points a not yet enabled backstore at DevicePath,
// enables it and enables thin provisioning, which LIO accepts only once the
// backstore is enabled.  The claim probe refuses early while the node holds
// the device; the enable write itself is LIO's exclusive open, and its EBUSY
// is reported as ErrDeviceHeld too.
func (t *Target) configureBackstore(dir string) error {
	c := t.cfs()
	release, err := t.claimer()(t.DevicePath)
	if err != nil {
		if errors.Is(err, ErrDeviceHeld) {
			return fmt.Errorf("enable backstore %q: device %s is still held on the storage node "+
				"(local attach in use): %w", dir, t.DevicePath, err)
		}
		return fmt.Errorf("enable backstore %q: claim device %s: %w", dir, t.DevicePath, err)
	}
	err = release()
	if err != nil {
		return fmt.Errorf("enable backstore %q: release claim probe on %s: %w", dir, t.DevicePath, err)
	}

	// iblock opens the device named by control's udev_path (reported back
	// in info as "UDEV PATH: <path>"); the separate udev_path attribute is
	// informational and is written as targetcli does, so ListTargets and
	// operators see the device.
	control := filepath.Join(dir, "control")
	value := "udev_path=" + t.DevicePath
	err = c.fs.WriteFile(control, value)
	if err != nil {
		return fmt.Errorf("configfs write %q = %q: %w", control, value, err)
	}
	infoPath := filepath.Join(dir, "info")
	info, err := c.readAttr(infoPath)
	if err != nil {
		return err
	}
	if got := infoUdevPath(info); got != t.DevicePath {
		return fmt.Errorf("configfs verify %q after write %q: %s reports UDEV PATH %q (info %q)",
			control, value, infoPath, got, info)
	}
	err = c.writeAttr(filepath.Join(dir, "udev_path"), t.DevicePath)
	if err != nil {
		return err
	}

	enable := filepath.Join(dir, "enable")
	err = c.fs.WriteFile(enable, "1")
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("configfs write %q = 1 (device %s): %w: %w", enable, t.DevicePath, ErrDeviceHeld, err)
		}
		return fmt.Errorf("configfs write %q = 1 (device %s): %w", enable, t.DevicePath, err)
	}
	got, err := c.readAttr(enable)
	if err != nil {
		return err
	}
	if got != "1" {
		return fmt.Errorf("configfs verify %q after write 1: got %q", enable, got)
	}
	return t.ensureThinProvisioning(dir)
}

// ensureThinProvisioning makes the enabled backstore at dir advertise SCSI
// thin provisioning, so initiators see UNMAP (discard) support and fstrim or
// "-o discard" on the node return freed blocks to the backing device.  LIO
// defaults attrib/emulate_tpu to 0; it is written only when it differs and
// read back.
//
// The kernel accepts 1 only when the backing device supports discard:
// iblock reads the discard limits of its block device when the backstore is
// enabled, and otherwise emulate_tpu_store fails with ENOSYS from
// target_try_configure_unmap (drivers/target/target_core_configfs.c).  That
// ENOSYS leaves the attribute at 0, is reported through DiscardUnsupported,
// and the backstore is served without UNMAP; any other error is returned.
func (t *Target) ensureThinProvisioning(dir string) error {
	c := t.cfs()
	path := filepath.Join(dir, "attrib", "emulate_tpu")
	got, err := c.readAttr(path)
	if err != nil {
		return err
	}
	if got == "1" {
		return nil
	}
	err = c.fs.WriteFile(path, "1")
	if errors.Is(err, syscall.ENOSYS) {
		if t.DiscardUnsupported != nil {
			t.DiscardUnsupported(t.DevicePath)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("configfs write %q = 1 (device %s): %w", path, t.DevicePath, err)
	}
	got, err = c.readAttr(path)
	if err != nil {
		return fmt.Errorf("configfs verify %q after write 1: %w", path, err)
	}
	if got != "1" {
		return fmt.Errorf("configfs verify %q after write 1: got %q", path, got)
	}
	return nil
}

// infoUdevPath extracts the device of "UDEV PATH: <path>" from an iblock
// backstore's info attribute; "" when absent.
func infoUdevPath(info string) string {
	_, rest, ok := strings.Cut(info, "UDEV PATH: ")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// backstoreDevice returns the device an iblock backstore at dir opens, as
// reported by its info attribute.
func (c configfs) backstoreDevice(dir string) (string, error) {
	info, err := c.readAttr(filepath.Join(dir, "info"))
	if err != nil {
		return "", err
	}
	return infoUdevPath(info), nil
}

// ensureUnitSerial pins the backstore's T10 unit serial.  LIO derives the
// NAA WWN of VPD page 0x83 from it, and initiators identify the disk by it,
// so it must be identical every time the backstore is re-created.  LIO
// rejects the write while a LUN uses the backstore; a differing serial on an
// exported backstore is therefore an error rather than a silent change.
func (t *Target) ensureUnitSerial() error {
	c := t.cfs()
	want := t.UnitSerial
	if want == "" {
		want = DeriveUnitSerial(t.IQN)
	}
	path := filepath.Join(t.BackstoreDir(), "wwn", "vpd_unit_serial")
	got, err := t.readUnitSerial()
	if err != nil {
		return err
	}
	if got == want {
		return nil
	}
	bound, err := t.lunBound()
	if err != nil {
		return err
	}
	if bound {
		return fmt.Errorf("backstore %q has unit serial %q, want %q, and cannot change it while exported",
			t.BackstoreDir(), got, want)
	}
	err = c.fs.WriteFile(path, want)
	if err != nil {
		return fmt.Errorf("configfs write %q = %q: %w", path, want, err)
	}
	got, err = t.readUnitSerial()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("configfs verify %q after write %q: got %q", path, want, got)
	}
	return nil
}

func (t *Target) readUnitSerial() (string, error) {
	raw, err := t.cfs().readAttr(filepath.Join(t.BackstoreDir(), "wwn", "vpd_unit_serial"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, unitSerialPrefix)), nil
}

// lunBound reports whether LUN 0 exists and links to the target's backstore.
func (t *Target) lunBound() (bool, error) {
	links, err := t.cfs().symlinks(t.lunDir())
	if err != nil {
		return false, err
	}
	for _, target := range links {
		if target == t.BackstoreDir() {
			return true, nil
		}
	}
	return false, nil
}

// ensureLUN creates LUN 0 and binds it to the backstore.  A LUN bound to
// another backstore is an error.
func (t *Target) ensureLUN() error {
	c := t.cfs()
	for _, dir := range []string{filepath.Dir(t.lunDir()), t.lunDir()} {
		err := c.ensureDir(dir)
		if err != nil {
			return fmt.Errorf("create LUN: %w", err)
		}
	}
	links, err := c.symlinks(t.lunDir())
	if err != nil {
		return err
	}
	for name, target := range links {
		if target != t.BackstoreDir() {
			return fmt.Errorf("LUN %q is bound to %q (link %s), want %q", t.lunDir(), target, name, t.BackstoreDir())
		}
		return nil
	}
	return c.ensureSymlink(t.BackstoreDir(), filepath.Join(t.lunDir(), lunLinkName))
}

// ensureMappedLUN maps LUN 0 into the node ACL of iqn as its LUN 0.
func (t *Target) ensureMappedLUN(iqn string) error {
	c := t.cfs()
	mapped := filepath.Join(t.aclDir(iqn), LUNName)
	err := c.ensureDir(mapped)
	if err != nil {
		return fmt.Errorf("map LUN for initiator %q: %w", iqn, err)
	}
	return c.ensureSymlink(t.lunDir(), filepath.Join(mapped, mappedLUNLinkName))
}

// removeMappedLUNs removes every mapped LUN of the node ACL at aclDir.
func (t *Target) removeMappedLUNs(aclDir string) error {
	c := t.cfs()
	dirs, err := c.subdirs(aclDir)
	if err != nil {
		return err
	}
	for _, name := range dirs {
		if !strings.HasPrefix(name, "lun_") {
			continue
		}
		mapped := filepath.Join(aclDir, name)
		err = c.removeSymlinks(mapped)
		if err != nil {
			return err
		}
		err = c.removeDir(mapped)
		if err != nil {
			return err
		}
	}
	return nil
}

// removeLUNs unbinds and removes every LUN of the TPG.
func (t *Target) removeLUNs() error {
	c := t.cfs()
	lunRoot := filepath.Dir(t.lunDir())
	luns, err := c.subdirs(lunRoot)
	if err != nil {
		return err
	}
	for _, name := range luns {
		dir := filepath.Join(lunRoot, name)
		err = c.removeSymlinks(dir)
		if err != nil {
			return err
		}
		err = c.removeDir(dir)
		if err != nil {
			return err
		}
	}
	return nil
}

// detachLUN brings an existing target into the local-attach state: TPG
// disabled, no mapped LUNs, no LUN, no backstore.  It returns the device the
// removed backstore used ("" when there was no backstore).
func (t *Target) detachLUN() (string, error) {
	c := t.cfs()
	ok, err := c.exists(t.tpgDir())
	if err != nil {
		return "", err
	}
	if ok {
		err = t.disableTPG()
		if err != nil {
			return "", err
		}
		acls, listErr := c.subdirs(t.aclsDir())
		if listErr != nil {
			return "", listErr
		}
		for _, iqn := range acls {
			err = t.removeMappedLUNs(t.aclDir(iqn))
			if err != nil {
				return "", err
			}
		}
		err = t.removeLUNs()
		if err != nil {
			return "", err
		}
	}
	dir := t.BackstoreDir()
	ok, err = c.exists(dir)
	if err != nil || !ok {
		return "", err
	}
	devicePath, err := c.backstoreDevice(dir)
	if err != nil {
		return "", err
	}
	err = c.removeDir(dir)
	if err != nil {
		return "", fmt.Errorf("remove backstore: %w", err)
	}
	return devicePath, nil
}

// EnterLocalAttach fences the export for a direct attach on the storage
// node: it disables the TPG (terminating every session), removes the mapped
// LUNs, LUN 0 and the backstore, and verifies with an exclusive claim probe
// that the device is no longer held.  It returns the device path the removed
// backstore used, or DevicePath when the export already was in the
// local-attach state.  A missing target returns ErrTargetNotFound.
func (t *Target) EnterLocalAttach() (string, error) {
	err := t.requireTPG()
	if err != nil {
		return "", fmt.Errorf("EnterLocalAttach: %w", err)
	}
	removedDevice, err := t.detachLUN()
	if err != nil {
		return "", fmt.Errorf("EnterLocalAttach %q: %w", t.IQN, err)
	}
	if removedDevice == "" {
		return t.DevicePath, nil
	}
	release, err := t.claimer()(removedDevice)
	if err != nil {
		return "", fmt.Errorf("EnterLocalAttach %q: device %s still claimed after removing the backstore: %w",
			t.IQN, removedDevice, err)
	}
	err = release()
	if err != nil {
		return "", fmt.Errorf("EnterLocalAttach %q: release claim probe on %s: %w", t.IQN, removedDevice, err)
	}
	return removedDevice, nil
}

// LeaveLocalAttach returns a locally attached export to serving remote
// initiators: it re-creates the backstore on DevicePath (ErrDeviceHeld while
// the node still holds the device), LUN 0 and the mapped LUNs, and enables
// the TPG.  Portal, TPG attributes and ACLs are left as they are.  A missing
// target returns ErrTargetNotFound.
func (t *Target) LeaveLocalAttach() error {
	err := t.requireTPG()
	if err != nil {
		return fmt.Errorf("LeaveLocalAttach: %w", err)
	}
	if t.DevicePath == "" {
		return fmt.Errorf("LeaveLocalAttach %q: device path is required", t.IQN)
	}
	err = t.attachLUN()
	if err != nil {
		return fmt.Errorf("LeaveLocalAttach %q: %w", t.IQN, err)
	}
	err = t.cfs().ensureAttr(t.tpgEnable(), "1")
	if err != nil {
		return fmt.Errorf("LeaveLocalAttach %q: enable TPG: %w", t.IQN, err)
	}
	return nil
}

// Remove tears the export down in reverse dependency order: disable the TPG
// (terminating sessions), remove node ACLs with their mapped LUNs, portals,
// LUNs, the TPG, the target and finally the backstore.  Missing pieces are
// skipped; every removal is read back.
func (t *Target) Remove() error {
	err := ValidateIQN(t.IQN)
	if err != nil {
		return fmt.Errorf("remove iSCSI target: %w", err)
	}
	c := t.cfs()
	ok, err := c.exists(t.tpgDir())
	if err != nil {
		return fmt.Errorf("remove iSCSI target %q: %w", t.IQN, err)
	}
	if ok {
		err = t.removeTPG()
		if err != nil {
			return fmt.Errorf("remove iSCSI target %q: %w", t.IQN, err)
		}
	}
	err = c.removeDir(t.targetDir())
	if err != nil {
		return fmt.Errorf("remove iSCSI target %q: %w", t.IQN, err)
	}
	err = c.removeDir(t.BackstoreDir())
	if err != nil {
		return fmt.Errorf("remove iSCSI target %q: backstore: %w", t.IQN, err)
	}
	return nil
}

func (t *Target) removeTPG() error {
	c := t.cfs()
	err := t.disableTPG()
	if err != nil {
		return err
	}
	acls, err := c.subdirs(t.aclsDir())
	if err != nil {
		return err
	}
	for _, iqn := range acls {
		err = t.removeACL(iqn)
		if err != nil {
			return err
		}
	}
	portals, err := c.subdirs(t.npDir())
	if err != nil {
		return err
	}
	for _, portal := range portals {
		err = c.removeDir(filepath.Join(t.npDir(), portal))
		if err != nil {
			return err
		}
	}
	err = t.removeLUNs()
	if err != nil {
		return err
	}
	return c.removeDir(t.tpgDir())
}

// AllowInitiator creates the node ACL of iqn, writes CHAP's credentials to
// it (also when it already exists, so changed credentials apply) and, while
// LUN 0 exists, maps it.  It does not change the TPG's authentication
// switch.  A missing target returns ErrTargetNotFound.
func (t *Target) AllowInitiator(iqn string) error {
	err := ValidateIQN(iqn)
	if err != nil {
		return fmt.Errorf("AllowInitiator %q: %w", t.IQN, err)
	}
	if t.CHAP != nil {
		err = t.CHAP.Validate()
		if err != nil {
			return fmt.Errorf("AllowInitiator %q: %w", t.IQN, err)
		}
	}
	err = t.requireTPG()
	if err != nil {
		return fmt.Errorf("AllowInitiator: %w", err)
	}
	err = t.allowInitiator(iqn)
	if err != nil {
		return fmt.Errorf("AllowInitiator %q: %w", t.IQN, err)
	}
	return nil
}

func (t *Target) allowInitiator(iqn string) error {
	c := t.cfs()
	for _, dir := range []string{t.aclsDir(), t.aclDir(iqn)} {
		err := c.ensureDir(dir)
		if err != nil {
			return fmt.Errorf("create node ACL %q: %w", iqn, err)
		}
	}
	err := t.ensureACLAuth(iqn)
	if err != nil {
		return err
	}
	ok, err := c.exists(t.lunDir())
	if err != nil || !ok {
		return err
	}
	return t.ensureMappedLUN(iqn)
}

// DenyInitiator removes the node ACL of iqn with its mapped LUNs; LIO
// terminates the initiator's sessions when its ACL is removed.  A missing
// target or ACL is not an error.
func (t *Target) DenyInitiator(iqn string) error {
	err := ValidateIQN(iqn)
	if err != nil {
		return fmt.Errorf("DenyInitiator %q: %w", t.IQN, err)
	}
	err = t.removeACL(iqn)
	if err != nil {
		return fmt.Errorf("DenyInitiator %q: %w", t.IQN, err)
	}
	return nil
}

func (t *Target) removeACL(iqn string) error {
	dir := t.aclDir(iqn)
	err := t.removeMappedLUNs(dir)
	if err != nil {
		return err
	}
	return t.cfs().removeDir(dir)
}

// Initiators returns the initiator IQNs with a node ACL, sorted.
func (t *Target) Initiators() ([]string, error) {
	acls, err := t.cfs().subdirs(t.aclsDir())
	if err != nil {
		return nil, err
	}
	slices.Sort(acls)
	return acls, nil
}

// RevokeInitiatorsExcept removes every node ACL whose initiator is not in
// keep.
func (t *Target) RevokeInitiatorsExcept(keep []string) error {
	acls, err := t.Initiators()
	if err != nil {
		return fmt.Errorf("RevokeInitiatorsExcept %q: %w", t.IQN, err)
	}
	for _, iqn := range acls {
		if slices.Contains(keep, iqn) {
			continue
		}
		err = t.removeACL(iqn)
		if err != nil {
			return fmt.Errorf("RevokeInitiatorsExcept %q: %w", t.IQN, err)
		}
	}
	return nil
}
