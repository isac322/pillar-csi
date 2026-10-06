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

// Package devidle detects non-exclusive consumers of a block device that an
// O_EXCL claim cannot see: mounts recorded in /proc/self/mountinfo,
// device-mapper holders under /sys/dev/block, and configured LIO backstores
// or nvmet namespaces whose recorded device path resolves to the device.
//
// Everything is keyed on the kernel device number (st_rdev / "major:minor"),
// never on a path spelling: /dev/<vg>/<lv>, /dev/mapper/<vg>-<lv> and any
// other alias of the same device compare equal here.
//
// The typical call sequence is:
//
//	claim, _ := devidle.ClaimDevice(devPath)   // O_RDONLY|O_EXCL
//	err := devidle.Checker{}.Check(id, claim.Rdev())
//	releaseErr := claim.Release()
//
// ClaimDevice establishes the exclusive boundary; Check then fails on every
// remaining non-exclusive consumer.  Report returns the same findings as a
// list instead of refusing, for read-only observation (InspectLV) and for
// callers that classify export findings themselves.
//
// The scan is read-only: it only opens and reads procfs/sysfs/configfs
// attributes and stats recorded paths.  All roots are injectable through
// Roots so tests run against t.TempDir trees.
package devidle

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// Finding kinds reported by Checker.Check/Report.
const (
	// KindMount is a mountinfo entry whose device major:minor is the
	// device's.  Detail is the mount point.
	KindMount = "mount"
	// KindHolder is a device-mapper (or other stacked) device listed in the
	// device's sysfs holders directory.  Detail is the holder name.
	KindHolder = "holder"
	// KindExport is a configured LIO backstore or nvmet namespace whose
	// recorded device path resolves to the device.  TargetID carries the
	// export's target name (LIO IQN or nvmet NQN) when it can be resolved;
	// Protocol is "iscsi" or "nvmet".
	KindExport = "export"
)

// Export protocols reported on Finding.Protocol for KindExport findings.
const (
	ProtocolISCSI = "iscsi"
	ProtocolNVMe  = "nvmet"
)

// ErrDeviceHeld reports that a device already has an exclusive claimant: a
// mount, a device-mapper target, an export backstore, or another O_EXCL
// opener.  It is the same sentinel nvmeof.ErrDeviceHeld so callers compare
// one error value across the whole agent.
var ErrDeviceHeld = nvmeof.ErrDeviceHeld

// ErrUnsupported reports that a kernel-level device operation is not
// supported on this platform.  Package devidle is Linux-only; on other
// platforms every such operation returns this error instead of silently
// pretending a device is idle.
var ErrUnsupported = errors.New("devidle: device idle checks are unsupported on this platform")

// Roots locates the kernel trees devidle scans.  Empty fields fall back to
// the production defaults; tests point every field at a t.TempDir tree.
type Roots struct {
	// MountinfoPath is the mount table, defaulting to /proc/self/mountinfo.
	// This is the agent's mount namespace, not necessarily the host's.
	// Exclusive device claims and sysfs holders also detect mounts outside it.
	MountinfoPath string
	// SysDevBlock is the /sys/dev/block directory that holds one entry per
	// kernel device number, defaulting to /sys/dev/block.
	SysDevBlock string
	// ConfigfsRoot is the configfs mount root scanned for LIO backstores
	// (target/core) and nvmet namespaces (nvmet/), defaulting to
	// /sys/kernel/config.
	ConfigfsRoot string
}

func (r Roots) mountinfoPath() string {
	if r.MountinfoPath != "" {
		return r.MountinfoPath
	}
	return "/proc/self/mountinfo"
}

func (r Roots) sysDevBlock() string {
	if r.SysDevBlock != "" {
		return r.SysDevBlock
	}
	return "/sys/dev/block"
}

func (r Roots) configfsRoot() string {
	if r.ConfigfsRoot != "" {
		return r.ConfigfsRoot
	}
	return nvmeof.DefaultConfigfsRoot
}

// Finding is one observed consumer of a device.
type Finding struct {
	// Kind is KindMount, KindHolder or KindExport.
	Kind string
	// Detail is kind-specific: the mount point for KindMount, the holder
	// device name for KindHolder, and the configured device path (or
	// backstore directory) for KindExport.
	Detail string
	// TargetID is the export's target name — the LIO IQN or nvmet NQN —
	// when it can be resolved, empty otherwise.  Callers that know the
	// target ID their own export of the volume would use compare it
	// verbatim to distinguish "own configured export" from a foreign one.
	TargetID string
	// Protocol is ProtocolISCSI or ProtocolNVMe for KindExport findings.
	Protocol string
}

// InUseError is returned by Checker.Check when any consumer finding exists.
// Findings carries every finding, not just the first, so a refusal reports
// the complete consumer set.
type InUseError struct {
	// DeviceID identifies the checked device in messages (a volume ID).
	DeviceID string
	Findings []Finding
}

func (e *InUseError) Error() string {
	parts := make([]string, 0, len(e.Findings))
	for _, f := range e.Findings {
		switch {
		case f.Kind == KindExport && f.TargetID != "":
			parts = append(parts, fmt.Sprintf("%s export %q on %q", f.Protocol, f.TargetID, f.Detail))
		default:
			parts = append(parts, fmt.Sprintf("%s %q", f.Kind, f.Detail))
		}
	}
	return fmt.Sprintf("device %q is still in use: %s", e.DeviceID, strings.Join(parts, "; "))
}

// Checker locates the kernel trees and resolves recorded device paths to
// kernel device numbers.  The zero value is ready for production use.
type Checker struct {
	// Roots overrides the scanned kernel trees; the zero value uses the
	// production defaults.
	Roots Roots
	// StatRdev resolves a recorded device path (an LIO udev_path or an
	// nvmet device_path) to its kernel device number.  Nil uses the real
	// stat(2); tests inject a map lookup because t.TempDir trees contain
	// no device nodes.
	StatRdev func(path string) (uint64, error)
	// ReadDir lists one sysfs/configfs directory while enumerating holders
	// and LIO backstores and bindings.  Nil uses os.ReadDir; tests inject
	// EACCES/EIO on one path because permission bits cannot make a
	// directory unreadable to root.
	ReadDir func(path string) ([]os.DirEntry, error)
	// ReadFile reads one LIO backstore attribute (info, udev_path).  Nil
	// uses os.ReadFile; tests inject failures as for ReadDir.
	ReadFile func(path string) ([]byte, error)
}

func (c Checker) readDir(path string) ([]os.DirEntry, error) {
	if c.ReadDir != nil {
		return c.ReadDir(path)
	}
	return os.ReadDir(path) //nolint:wrapcheck // wrapped by callers
}

func (c Checker) readFile(path string) ([]byte, error) {
	if c.ReadFile != nil {
		return c.ReadFile(path)
	}
	return os.ReadFile(path) //nolint:gosec,wrapcheck // G304: configured configfs root; callers wrap.
}

// Check returns nil when no consumer of rdev is observed.  It returns an
// *InUseError listing every finding when any is, and a plain error when a
// kernel probe itself fails — an unverifiable device is never reported idle.
func (c Checker) Check(deviceID string, rdev uint64) error {
	findings, err := c.Report(rdev)
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return &InUseError{DeviceID: deviceID, Findings: findings}
	}
	return nil
}

// Report lists every consumer finding for the device with kernel number
// rdev, in deterministic order: mounts, holders, then exports.  A missing
// mountinfo file, sysfs tree or configfs tree contributes no findings; a
// present-but-unreadable one fails the report.
func (c Checker) Report(rdev uint64) ([]Finding, error) {
	major, minor := DecodeLinuxDev(rdev)
	var findings []Finding
	mounts, err := c.scanMountinfo(major, minor)
	if err != nil {
		return nil, err
	}
	findings = append(findings, mounts...)
	holders, err := c.scanHolders(major, minor)
	if err != nil {
		return nil, err
	}
	findings = append(findings, holders...)
	exports, err := c.scanExports(rdev)
	if err != nil {
		return nil, err
	}
	findings = append(findings, exports...)
	return findings, nil
}

// statRdev resolves path to its kernel device number.
func (c Checker) statRdev(path string) (uint64, error) {
	if c.StatRdev != nil {
		return c.StatRdev(path)
	}
	return statDevice(path)
}

// scanMountinfo reports one finding per mount whose device major:minor
// (mountinfo field 3) is the device's.  The mount source column is not
// compared: it records a path alias, and a renamed alias would let a real
// mount slip past a string comparison.
func (c Checker) scanMountinfo(major, minor uint64) ([]Finding, error) {
	path := c.Roots.mountinfoPath()
	f, err := os.Open(path) //nolint:gosec // G304: root is a configured kernel tree.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("devidle: open %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var findings []Finding
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		mj, mn, ok := parseMajorMinor(fields[2])
		if !ok || mj != major || mn != minor {
			continue
		}
		findings = append(findings, Finding{
			Kind:   KindMount,
			Detail: unescapeMountinfoField(fields[4]),
		})
	}
	err = scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("devidle: read %q: %w", path, err)
	}
	return findings, nil
}

// parseMajorMinor parses a "major:minor" mountinfo field.
func parseMajorMinor(field string) (major, minor uint64, ok bool) {
	mj, mn, found := strings.Cut(field, ":")
	if !found {
		return 0, 0, false
	}
	major, err1 := strconv.ParseUint(mj, 10, 64)
	minor, err2 := strconv.ParseUint(mn, 10, 64)
	return major, minor, err1 == nil && err2 == nil
}

// unescapeMountinfoField reverses the octal escapes mountinfo applies to
// space, tab, newline and backslash (\040 \011 \012 \134).
func unescapeMountinfoField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ { // explicit: the body skips escape digits via i += 3
		if s[i] == '\\' && i+3 < len(s) &&
			s[i+1] >= '0' && s[i+1] <= '3' &&
			s[i+2] >= '0' && s[i+2] <= '7' &&
			s[i+3] >= '0' && s[i+3] <= '7' {
			v := (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
			b.WriteByte(v)
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// scanHolders reports one finding per entry of
// <SysDevBlock>/<major>:<minor>/holders — the stacked devices (device-mapper
// maps, multipath, md) holding the checked device open.
func (c Checker) scanHolders(major, minor uint64) ([]Finding, error) {
	dir := filepath.Join(c.Roots.sysDevBlock(), fmt.Sprintf("%d:%d", major, minor), "holders")
	entries, err := c.readDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("devidle: read sysfs holders %q: %w", dir, err)
	}
	findings := make([]Finding, 0, len(entries))
	for _, e := range entries {
		findings = append(findings, Finding{Kind: KindHolder, Detail: e.Name()})
	}
	return findings, nil
}

// scanExports reports one finding per configured export whose recorded
// device path resolves to rdev: LIO backstores under target/core and nvmet
// namespaces under nvmet/subsystems.  A configured path that no longer
// resolves (stat ENOENT — the device vanished) contributes nothing.
func (c Checker) scanExports(rdev uint64) ([]Finding, error) {
	lio, err := c.scanLIOExports(rdev)
	if err != nil {
		return nil, err
	}
	nvmet, err := c.scanNVMeExports(rdev)
	if err != nil {
		return nil, err
	}
	return append(lio, nvmet...), nil
}

// scanLIOExports scans <configfs>/target/core/<hba>/<backstore>/ for
// backstores whose configured device resolves to rdev.  Each backstore's
// target ID is the IQN of the iSCSI target whose LUN binds it, resolved
// through the target/iscsi/*/tpgt_*/lun/lun_* symlinks; an unbound
// backstore has no target ID.
//
// Every level is enumerated with a checked ReadDir, never filepath.Glob:
// Glob drops directory read errors, and an unreadable target/core or HBA
// directory could hide a configured-but-disabled backstore, which holds no
// exclusive claim on the device.  Only ENOENT — the tree is absent, or an
// entry vanished between listing and reading it — is empty; any other
// failure fails the scan so the device is never reported idle.
func (c Checker) scanLIOExports(rdev uint64) ([]Finding, error) {
	major, minor := DecodeLinuxDev(rdev)
	bindings, err := c.lioBackstoreBindings()
	if err != nil {
		return nil, fmt.Errorf("devidle: resolve LIO bindings for device %d:%d: %w", major, minor, err)
	}
	coreDir := filepath.Join(c.Roots.configfsRoot(), "target", "core")
	hbas, err := c.readDirIfPresent(coreDir)
	if err != nil {
		return nil, fmt.Errorf("devidle: list LIO HBAs %q for device %d:%d: %w", coreDir, major, minor, err)
	}
	var findings []Finding
	for _, hba := range hbas {
		// target/core also holds attribute files; only directories are
		// HBAs.  os.ReadDir lstats entries whose type readdir did not
		// report, so an entry type is never guessed.
		if !hba.IsDir() {
			continue
		}
		found, err := c.scanLIOHBA(filepath.Join(coreDir, hba.Name()), rdev, bindings)
		if err != nil {
			return nil, err
		}
		findings = append(findings, found...)
	}
	return findings, nil
}

// readDirIfPresent lists dir with the checked readDir.  A missing directory
// (never created, or removed between listings) yields no entries; any other
// failure is returned unwrapped for the caller to annotate.
func (c Checker) readDirIfPresent(dir string) ([]os.DirEntry, error) {
	entries, err := c.readDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return entries, nil
}

// scanLIOHBA reports one finding per backstore of the HBA directory hbaDir
// whose recorded device resolves to rdev.  A vanished HBA contributes nothing.
func (c Checker) scanLIOHBA(hbaDir string, rdev uint64, bindings map[string]string) ([]Finding, error) {
	major, minor := DecodeLinuxDev(rdev)
	stores, err := c.readDirIfPresent(hbaDir)
	if err != nil {
		return nil, fmt.Errorf("devidle: list LIO backstores %q for device %d:%d: %w", hbaDir, major, minor, err)
	}
	var findings []Finding
	for _, store := range stores {
		// target/core/<hba>/ also holds attribute files (hba_info,
		// hba_mode); only directories are backstores.
		if !store.IsDir() {
			continue
		}
		dir := filepath.Join(hbaDir, store.Name())
		recorded, err := c.backstoreDevice(dir)
		if err != nil {
			return nil, fmt.Errorf("devidle: LIO backstore %q for device %d:%d: %w", dir, major, minor, err)
		}
		if recorded == "" {
			continue // no device configured on this backstore
		}
		match, err := c.recordedIsDevice(recorded, rdev)
		if err != nil {
			return nil, err
		}
		if !match {
			continue
		}
		findings = append(findings, Finding{
			Kind:     KindExport,
			Detail:   recorded,
			TargetID: bindings[dir],
			Protocol: ProtocolISCSI,
		})
	}
	return findings, nil
}

// backstoreDevice returns the device a backstore is configured with: the
// kernel-authoritative "UDEV PATH: <path>" of its info attribute, falling
// back to the informational udev_path attribute (what targetcli and this
// agent write) when info names none, e.g. in hand-built fixtures.  A
// present-but-unreadable attribute is an error: the backstore cannot be
// cleared, so the device is not proven idle.
func (c Checker) backstoreDevice(dir string) (string, error) {
	infoPath := filepath.Join(dir, "info")
	info, err := c.readFile(infoPath)
	switch {
	case err == nil:
		if _, rest, ok := strings.Cut(string(info), "UDEV PATH: "); ok {
			if fields := strings.Fields(rest); len(fields) > 0 && fields[0] != "None" {
				return fields[0], nil
			}
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("devidle: read %q: %w", infoPath, err)
	}
	udevPath := filepath.Join(dir, "udev_path")
	data, err := c.readFile(udevPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("devidle: read %q: %w", udevPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// lioBackstoreBindings maps each backstore directory under target/core to
// the IQN of the iSCSI target binding it, by following the
// target/iscsi/<iqn>/tpgt_*/lun/lun_*/* symlinks.  Each level is listed
// with a checked ReadDir: a missing directory (no iscsi fabric, a TPG with
// no lun group, an entry removed between listings) yields no bindings, any
// other listing or readlink failure is an error.
func (c Checker) lioBackstoreBindings() (map[string]string, error) {
	bindings := map[string]string{}
	iscsiDir := filepath.Join(c.Roots.configfsRoot(), "target", "iscsi")
	iqns, err := c.readDirIfPresent(iscsiDir)
	if err != nil {
		return nil, fmt.Errorf("devidle: list iSCSI targets %q: %w", iscsiDir, err)
	}
	for _, iqn := range iqns {
		// target/iscsi also holds fabric attribute files (discovery_auth,
		// lio_version); only directories are targets.
		if !iqn.IsDir() {
			continue
		}
		err = c.bindTargetLUNs(bindings, filepath.Join(iscsiDir, iqn.Name()), iqn.Name())
		if err != nil {
			return nil, err
		}
	}
	return bindings, nil
}

// bindTargetLUNs records in bindings every backstore linked from a
// tpgt_*/lun/lun_* directory of the iSCSI target directory iqnDir, keeping
// the first IQN recorded for a backstore.
func (c Checker) bindTargetLUNs(bindings map[string]string, iqnDir, iqn string) error {
	tpgs, err := c.readDirIfPresent(iqnDir)
	if err != nil {
		return fmt.Errorf("devidle: list iSCSI TPGs %q: %w", iqnDir, err)
	}
	for _, tpg := range tpgs {
		if !tpg.IsDir() || !strings.HasPrefix(tpg.Name(), "tpgt_") {
			continue
		}
		lunGroup := filepath.Join(iqnDir, tpg.Name(), "lun")
		luns, err := c.readDirIfPresent(lunGroup)
		if err != nil {
			return fmt.Errorf("devidle: list iSCSI LUNs %q: %w", lunGroup, err)
		}
		for _, lun := range luns {
			if !lun.IsDir() || !strings.HasPrefix(lun.Name(), "lun_") {
				continue
			}
			err = c.bindLUNLinks(bindings, filepath.Join(lunGroup, lun.Name()), iqn)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// bindLUNLinks records in bindings the backstore each symlink of the LUN
// directory lunDir points to, unless another IQN already binds it.
func (c Checker) bindLUNLinks(bindings map[string]string, lunDir, iqn string) error {
	links, err := c.readDirIfPresent(lunDir)
	if err != nil {
		return fmt.Errorf("devidle: list iSCSI LUN %q: %w", lunDir, err)
	}
	for _, l := range links {
		// lun_<n> also holds attribute files and a statistics
		// group beside the backstore symlink.
		if l.Type()&os.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(lunDir, l.Name())
		target, err := os.Readlink(link)
		if err != nil {
			if os.IsNotExist(err) {
				continue // link vanished between listing and Readlink
			}
			return fmt.Errorf("devidle: readlink %q: %w", link, err)
		}
		bsDir := target
		if !filepath.IsAbs(bsDir) {
			bsDir = filepath.Join(lunDir, bsDir)
		}
		bsDir = filepath.Clean(bsDir)
		if _, bound := bindings[bsDir]; !bound {
			bindings[bsDir] = iqn
		}
	}
	return nil
}

// scanNVMeExports reports one finding per nvmet namespace whose device_path
// resolves to rdev.  The target ID is the subsystem NQN.
func (c Checker) scanNVMeExports(rdev uint64) ([]Finding, error) {
	subs, err := nvmeof.ListExports(c.Roots.configfsRoot())
	if err != nil {
		return nil, fmt.Errorf("devidle: list nvmet exports: %w", err)
	}
	var findings []Finding
	for _, sub := range subs {
		for nsid, recorded := range sub.NamespaceDevicePaths {
			match, err := c.recordedIsDevice(recorded, rdev)
			if err != nil {
				return nil, err
			}
			if !match {
				continue
			}
			findings = append(findings, Finding{
				Kind:     KindExport,
				Detail:   fmt.Sprintf("%s (namespace %d)", recorded, nsid),
				TargetID: sub.NQN,
				Protocol: ProtocolNVMe,
			})
		}
	}
	return findings, nil
}

// recordedIsDevice reports whether the recorded path exists and resolves to
// rdev.  A recorded path that no longer exists is not a finding: the kernel
// dropped the device it named.  Any other stat error fails the check —
// the export cannot be cleared, so the device is not proven idle.
func (c Checker) recordedIsDevice(recorded string, rdev uint64) (bool, error) {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false, nil
	}
	got, err := c.statRdev(recorded)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("devidle: stat recorded device %q: %w", recorded, err)
	}
	return got == rdev, nil
}

// ClaimDevice opens path O_RDONLY|O_EXCL|O_CLOEXEC and keeps the descriptor
// open until Release.  While held, the kernel refuses competing exclusive
// claims on the device: mounts, device-mapper table loads, export backstore
// enables and other O_EXCL opens.
//
// The returned claim carries the device's kernel number (st_rdev of the
// held descriptor), so the caller compares the device it actually claimed
// against the device the metadata named rather than trusting the path.
//
// EBUSY is reported as ErrDeviceHeld.  A non-Linux build refuses with
// ErrUnsupported — there is no mocked fallback.
func ClaimDevice(path string) (Claim, error) {
	return claimDevice(path)
}

// Claim is a held exclusive device open.  Release closes the descriptor;
// its error is real and must be surfaced by the caller.
type Claim interface {
	// Rdev is the kernel device number of the held descriptor.
	Rdev() uint64
	// Release drops the exclusive claim.
	Release() error
}

// DecodeLinuxDev decodes a Linux kernel device number (st_rdev / the dev
// field of stat) into major and minor numbers, matching glibc's
// gnu_dev_major/gnu_dev_minor: major is dev bits 8-19 plus bits 44-63,
// minor is dev bits 0-7 plus bits 20-43.  Each number is 32 bits wide.
func DecodeLinuxDev(dev uint64) (major, minor uint64) {
	major = (dev>>8)&0x00000fff | (dev>>32)&0xfffff000
	minor = dev&0x000000ff | (dev>>12)&0xffffff00
	return major, minor
}

// EncodeLinuxDev is the inverse of DecodeLinuxDev: the Linux kernel device
// number (glibc gnu_dev_makedev) of major:minor, as stat(2) reports it in
// st_rdev.  Bits above the 32-bit major/minor range are dropped, as glibc
// does.
func EncodeLinuxDev(major, minor uint64) uint64 {
	return (major&0x00000fff)<<8 | (major&0xfffff000)<<32 |
		minor&0x000000ff | (minor&0xffffff00)<<12
}
