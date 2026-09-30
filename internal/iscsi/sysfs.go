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

package iscsi

// The sysfs layout used (drivers/scsi/scsi_transport_iscsi.c, scsi_sysfs.c):
//
//	class/iscsi_transport/tcp/handle                     transport handle
//	class/iscsi_session/session<SID>/{targetname,initiatorname,state,tpgt,recovery_tmo}
//	class/iscsi_session/session<SID>/device -> .../host<H>/session<SID>
//	class/iscsi_session/session<SID>/device/target<H>:0:<T>/<H>:0:<T>:<L>/block/<sdX>/dev
//	class/iscsi_connection/connection<SID>:<CID>/{state,persistent_address,persistent_port,ping_tmo,recv_tmo}
//	class/scsi_host/host<H>/scan
//	class/scsi_device/<H:C:T:L>/device/rescan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Kernel session states (iscsi_session_state_names).
const (
	sessionStateLoggedIn = "LOGGED_IN"
	sessionStateFailed   = "FAILED"
	// Connection state connection_state_names[ISCSI_CONN_UP].
	connStateUp = "up"
)

type sysfsConn struct {
	CID               int
	State             string
	PersistentAddress string
	PersistentPort    int
	PingTmo           int
	RecvTmo           int
}

type sysfsSession struct {
	SID           int
	HostNo        int
	TargetName    string
	InitiatorName string
	State         string
	TPGT          int
	RecoveryTmo   int
	Conns         []sysfsConn
}

// portal returns the persistent portal of the first connection.
func (s *sysfsSession) portal() (Portal, bool) {
	for _, c := range s.Conns {
		if c.PersistentAddress != "" && c.PersistentPort != 0 {
			return Portal{Address: c.PersistentAddress, Port: c.PersistentPort}.normalized(), true
		}
	}
	return Portal{}, false
}

// healthy reports whether the kernel considers the session fully up.
func (s *sysfsSession) healthy() bool {
	if s.State != sessionStateLoggedIn || len(s.Conns) == 0 {
		return false
	}
	for _, c := range s.Conns {
		if c.State != connStateUp {
			return false
		}
	}
	return true
}

type sysfs struct {
	root string
}

func (f sysfs) path(elem ...string) string {
	return filepath.Join(append([]string{f.root}, elem...)...)
}

func (f sysfs) sessionDir(sid int) string {
	return f.path("class", "iscsi_session", "session"+strconv.Itoa(sid))
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // sysfs path built from kernel ids
	if err != nil {
		return "", fmt.Errorf("read sysfs attribute: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// readOptionalInt returns -1 when the attribute is absent or empty
// (attributes whose value was never set read as "" on some kernels).
func readOptionalInt(path string) (int, error) {
	s, err := readTrimmed(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && s == "") {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return n, nil
}

// transportHandle reads the iscsi_tcp transport handle.
func (f sysfs) transportHandle() (uint64, error) {
	p := f.path("class", "iscsi_transport", "tcp", "handle")
	s, err := readTrimmed(p)
	if err != nil {
		return 0, fmt.Errorf("read iscsi_tcp transport handle %s (is the iscsi_tcp module loaded?): %w", p, err)
	}
	h, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse iscsi_tcp transport handle %s=%q: %w", p, s, err)
	}
	return h, nil
}

var hostDirRE = regexp.MustCompile(`^host(\d+)$`)

// hostNoOf resolves the SCSI host a session hangs off.
func (f sysfs) hostNoOf(sid int) (int, error) {
	link := filepath.Join(f.sessionDir(sid), "device")
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return 0, fmt.Errorf("resolve %s: %w", link, err)
	}
	parts := strings.Split(filepath.ToSlash(resolved), "/")
	for _, part := range slices.Backward(parts) {
		m := hostDirRE.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		hostNo, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("parse SCSI host number in %s: %w", resolved, err)
		}
		return hostNo, nil
	}
	return 0, fmt.Errorf("resolve SCSI host of session %d: no host<N> component in %s", sid, resolved)
}

// stringAttr and intAttr name a sysfs attribute and where to store it.
type stringAttr struct {
	name string
	dst  *string
}

type intAttr struct {
	name string
	dst  *int
}

// readAttrs reads string attributes, then optional integer attributes, of
// dir.  Errors are prefixed with the attribute name.
func readAttrs(dir string, strs []stringAttr, ints []intAttr) error {
	for _, a := range strs {
		v, err := readTrimmed(filepath.Join(dir, a.name))
		if err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
		*a.dst = v
	}
	for _, a := range ints {
		v, err := readOptionalInt(filepath.Join(dir, a.name))
		if err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
		*a.dst = v
	}
	return nil
}

// readSession reads one session.  It returns an error wrapping
// fs.ErrNotExist when the session disappeared.
func (f sysfs) readSession(sid int) (*sysfsSession, error) {
	s := &sysfsSession{SID: sid}
	err := readAttrs(f.sessionDir(sid),
		[]stringAttr{
			{"targetname", &s.TargetName},
			{"initiatorname", &s.InitiatorName},
			{"state", &s.State},
		},
		[]intAttr{
			{"tpgt", &s.TPGT},
			{"recovery_tmo", &s.RecoveryTmo},
		})
	if err != nil {
		return nil, fmt.Errorf("read session %d %w", sid, err)
	}
	s.HostNo, err = f.hostNoOf(sid)
	if err != nil {
		return nil, err
	}
	s.Conns, err = f.readConns(sid)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (f sysfs) readConns(sid int) ([]sysfsConn, error) {
	pattern := f.path("class", "iscsi_connection", fmt.Sprintf("connection%d:*", sid))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("list connections of session %d: %w", sid, err)
	}
	sort.Strings(matches)
	conns := make([]sysfsConn, 0, len(matches))
	prefix := fmt.Sprintf("connection%d:", sid)
	for _, dir := range matches {
		cid, convErr := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), prefix))
		if convErr != nil {
			continue // e.g. connection1:x from a different sid prefix match
		}
		c := sysfsConn{CID: cid}
		err = readAttrs(dir,
			[]stringAttr{
				{"state", &c.State},
				{"persistent_address", &c.PersistentAddress},
			},
			[]intAttr{
				{"persistent_port", &c.PersistentPort},
				{"ping_tmo", &c.PingTmo},
				{"recv_tmo", &c.RecvTmo},
			})
		if err != nil {
			return nil, fmt.Errorf("read connection %d:%d %w", sid, cid, err)
		}
		conns = append(conns, c)
	}
	return conns, nil
}

var sessionDirRE = regexp.MustCompile(`^session(\d+)$`)

// listSessions reads every iSCSI session in sysfs.  Sessions that vanish
// while being read are skipped.
func (f sysfs) listSessions() ([]*sysfsSession, error) {
	dir := f.path("class", "iscsi_session")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list iSCSI sessions in %s: %w", dir, err)
	}
	var out []*sysfsSession
	for _, e := range entries {
		m := sessionDirRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		sid, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			return nil, fmt.Errorf("parse session id of %s: %w", e.Name(), convErr)
		}
		s, readErr := f.readSession(sid)
		if errors.Is(readErr, fs.ErrNotExist) {
			_, statErr := os.Stat(f.sessionDir(sid))
			if errors.Is(statErr, fs.ErrNotExist) {
				continue // destroyed concurrently
			}
		}
		if readErr != nil {
			return nil, readErr
		}
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].SID < out[b].SID })
	return out, nil
}

func (f sysfs) sessionExists(sid int) (bool, error) {
	_, err := os.Stat(f.sessionDir(sid))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat session %d: %w", sid, err)
	}
	return true, nil
}

// lunDevice is one SCSI device of a session.
type lunDevice struct {
	HCTL  string // "H:C:T:L"
	LUN   int
	Block string // "sdX", empty until the block device is registered
}

var hctlRE = regexp.MustCompile(`^(\d+):(\d+):(\d+):(\d+)$`)

// sessionLUNs lists the SCSI devices attached below a session.
func (f sysfs) sessionLUNs(sid int) ([]lunDevice, error) {
	pattern := filepath.Join(f.sessionDir(sid), "device", "target*", "*")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("list LUNs of session %d: %w", sid, err)
	}
	var out []lunDevice
	for _, dir := range matches {
		name := filepath.Base(dir)
		m := hctlRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		lun, convErr := strconv.Atoi(m[4])
		if convErr != nil {
			return nil, fmt.Errorf("parse LUN of SCSI device %s: %w", name, convErr)
		}
		d := lunDevice{HCTL: name, LUN: lun}
		blocks, err := os.ReadDir(filepath.Join(dir, "block"))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("list block devices of SCSI device %s: %w", name, err)
		case len(blocks) > 0:
			d.Block = blocks[0].Name()
		}
		out = append(out, d)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].LUN < out[b].LUN })
	return out, nil
}

// blockDevNumber reads major:minor of a block device registered under an
// HCTL directory of a session.
func (f sysfs) blockDevNumber(sid int, d lunDevice) (major, minor uint32, err error) {
	pattern := filepath.Join(f.sessionDir(sid), "device", "target*", d.HCTL, "block", d.Block, "dev")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return 0, 0, fmt.Errorf("find dev attribute of %s (%s): %w", d.Block, d.HCTL, fs.ErrNotExist)
	}
	s, err := readTrimmed(matches[0])
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", matches[0], err)
	}
	return parseDevNumber(s)
}

func parseDevNumber(s string) (major, minor uint32, err error) {
	maj, minStr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("parse device number %q: want MAJOR:MINOR", s)
	}
	ma, err := strconv.ParseUint(maj, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse device number %q: %w", s, err)
	}
	mi, err := strconv.ParseUint(minStr, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse device number %q: %w", s, err)
	}
	return uint32(ma), uint32(mi), nil
}

// scsiDeviceState reads the SCSI device state ("running", "offline", ...).
func (f sysfs) scsiDeviceState(hctl string) (string, error) {
	return readTrimmed(f.path("class", "scsi_device", hctl, "device", "state"))
}

func (sysfs) writeAttr(path, value string) error {
	fh, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // sysfs attribute
	if err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, err)
	}
	_, err = fh.WriteString(value)
	if err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, errors.Join(err, fh.Close()))
	}
	err = fh.Close()
	if err != nil {
		return fmt.Errorf("write %q to %s: %w", value, path, err)
	}
	return nil
}

// scanHost asks the SCSI midlayer to scan one LUN on a host.  The software
// iSCSI host has exactly one channel/target, so channel and id are
// wildcards.
func (f sysfs) scanHost(hostNo, lun int) error {
	return f.writeAttr(f.path("class", "scsi_host", "host"+strconv.Itoa(hostNo), "scan"), "- - "+strconv.Itoa(lun))
}

// rescanDevice re-reads a SCSI device's capacity (online resize).
func (f sysfs) rescanDevice(hctl string) error {
	return f.writeAttr(f.path("class", "scsi_device", hctl, "device", "rescan"), "1")
}

// deleteDevice removes a SCSI device through its sysfs "delete" attribute.
// The kernel removes the device synchronously and, when the disk reports a
// volatile write cache, sends SYNCHRONIZE CACHE on the way out.  Page-cache
// write-back of a disk still held open fails once removal started, so the
// caller fsyncs the block device first.  On a failed session the write
// blocks until the session's replacement timeout expires and the I/O fails.
// Open holders do not block removal: their later I/O fails with ENXIO.
func (f sysfs) deleteDevice(hctl string) error {
	return f.writeAttr(f.path("class", "scsi_device", hctl, "device", "delete"), "1")
}

// scsiDeviceExists reports whether the SCSI device is still registered.
func (f sysfs) scsiDeviceExists(hctl string) (bool, error) {
	_, err := os.Stat(f.path("class", "scsi_device", hctl))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat SCSI device %s: %w", hctl, err)
	}
	return true, nil
}
