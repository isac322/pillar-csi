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

package nvmeof

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Host-visible identity of an exported namespace.
//
// A Linux NVMe host caches the namespace identifiers (UUID, NGUID, EUI-64,
// CSI) it saw when it first scanned a namespace.  After a controller
// reconnect it rescans and, in nvme_validate_ns, compares the freshly
// reported identifiers with the cached ones (nvme_ns_ids_equal).  Any
// difference is logged as "identifiers changed for nsid N" and the namespace
// path is removed, failing all queued I/O — the filesystem on top shuts down.
//
// The nvmet driver assigns a random device_uuid (and a zero device_nguid) to
// every newly created configfs namespace and a random attr_serial to every
// new subsystem.  Because configfs does not survive a storage-node reboot,
// the agent recreates these objects on every re-export, so the identity must
// be written explicitly and must be a pure function of data that survives the
// reboot.

// maxSerialLen is NVMET_SN_MAX_SIZE: the Identify Controller serial number
// field is 20 bytes of printable ASCII.
const maxSerialLen = 20

// zeroNGUID is how nvmet reports an unset device_nguid.
const zeroNGUID = "00000000-0000-0000-0000-000000000000"

// identityNamespace scopes the name-based (RFC 4122 version 5) UUIDs derived
// for pillar-csi namespaces so they never collide with UUIDs derived for the
// same names by other software.  It must never change: every exported
// namespace's identity depends on it.
var identityNamespace = uuid.MustParse("3c6e0b8a-6a0f-5c1d-9f5e-8d2b7a4e1c90")

// Identity is the set of identifiers an initiator caches for an exported
// namespace and compares after reconnecting.
type Identity struct {
	// UUID is the namespace device_uuid, in canonical lowercase
	// 8-4-4-4-12 form.
	UUID string `json:"uuid"`
	// NGUID is the namespace device_nguid, in the same textual form nvmet
	// reports it (canonical UUID layout).  All zeros means "not reported".
	NGUID string `json:"nguid"`
	// Serial is the subsystem attr_serial reported in Identify Controller.
	Serial string `json:"serial"`
}

// IsZero reports whether no identifier is set.
func (id Identity) IsZero() bool {
	return id == Identity{}
}

// Validate checks that every identifier is set and in the form nvmet accepts
// and reports back, so a later read-back comparison is exact.
func (id Identity) Validate() error {
	u, err := uuid.Parse(id.UUID)
	if err != nil || u.String() != id.UUID {
		return fmt.Errorf("invalid namespace uuid %q: want canonical lowercase UUID", id.UUID)
	}
	g, err := uuid.Parse(id.NGUID)
	if err != nil || g.String() != id.NGUID {
		return fmt.Errorf("invalid namespace nguid %q: want canonical lowercase 16-byte hex", id.NGUID)
	}
	if id.Serial == "" || len(id.Serial) > maxSerialLen {
		return fmt.Errorf("invalid subsystem serial %q: want 1-%d characters", id.Serial, maxSerialLen)
	}
	for i := range len(id.Serial) {
		c := id.Serial[i]
		if c <= ' ' || c > '~' {
			return fmt.Errorf("invalid subsystem serial %q: byte %d is not printable ASCII", id.Serial, i)
		}
	}
	return nil
}

// DeriveIdentity returns the deterministic identity of namespace nsid in the
// subsystem named subsystemNQN.  The agent derives the NQN from the volume ID,
// so the identity is identical across agent restarts, storage-node reboots,
// loss of all agent state, and controller-driven re-exports.
func DeriveIdentity(subsystemNQN string, nsid uint32) Identity {
	nsName := subsystemNQN + "/namespaces/" + strconv.FormatUint(uint64(nsid), 10)
	serialSum := sha256.Sum256([]byte("serial:" + subsystemNQN))
	return Identity{
		UUID:   uuid.NewSHA1(identityNamespace, []byte("uuid:"+nsName)).String(),
		NGUID:  uuid.NewSHA1(identityNamespace, []byte("nguid:"+nsName)).String(),
		Serial: hex.EncodeToString(serialSum[:maxSerialLen/2]),
	}
}

// desiredIdentity returns t.Identity, or the derived identity when it is
// unset.
func (t *NvmetTarget) desiredIdentity() (Identity, error) {
	if t.Identity.IsZero() {
		return DeriveIdentity(t.SubsystemNQN, t.NamespaceID), nil
	}
	err := t.Identity.Validate()
	if err != nil {
		return Identity{}, fmt.Errorf("target %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return t.Identity, nil
}

// LiveIdentity returns the identifiers already fixed by kernel objects that a
// host may have identified, so a re-applied export can keep them instead of
// replacing them under connected hosts:
//
//   - Serial is set when the subsystem directory exists (nvmet refuses to
//     change the serial of a subsystem a host has discovered).
//   - UUID and NGUID are set when the namespace exists and is enabled (nvmet
//     refuses to change them while enabled, and connected hosts cached them).
//
// Identifiers of objects that do not exist, or of a disabled namespace, are
// returned empty: Apply may still (re)write them.
func (t *NvmetTarget) LiveIdentity() (Identity, error) {
	var live Identity
	serial, err := readAttr(filepath.Join(t.subsystemDir(), "attr_serial"))
	if err != nil {
		return Identity{}, fmt.Errorf("LiveIdentity %q: %w", t.SubsystemNQN, err)
	}
	live.Serial = serial

	enabled, err := readAttr(filepath.Join(t.namespaceDir(), "enable"))
	if err != nil {
		return Identity{}, fmt.Errorf("LiveIdentity %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	if enabled != "1" {
		return live, nil
	}
	live.UUID, err = readAttr(filepath.Join(t.namespaceDir(), "device_uuid"))
	if err != nil {
		return Identity{}, fmt.Errorf("LiveIdentity %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	live.NGUID, err = readAttr(filepath.Join(t.namespaceDir(), "device_nguid"))
	if err != nil {
		return Identity{}, fmt.Errorf("LiveIdentity %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return live, nil
}

// readAttr reads a configfs attribute, trimming the newline and the space
// padding nvmet adds to fixed-width fields such as attr_serial.  A missing
// attribute reads as "".
func readAttr(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // configfs paths constructed from validated NQN/namespace inputs
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("configfs read %q: %w", path, err)
	}
	return strings.TrimSpace(strings.TrimRight(string(data), "\x00")), nil
}

// ensureAttr writes want to a configfs attribute unless it already holds that
// value.  Skipping identical writes matters for attributes nvmet locks once
// they are in use (device_uuid and device_nguid while the namespace is
// enabled, attr_serial once a host discovered the subsystem): an unchanged
// value must not fail a re-apply.  A differing value is written and verified
// by read-back; if nvmet rejects the write, the error is returned.
func ensureAttr(path, want string) error {
	current, err := readAttr(path)
	if err != nil {
		return err
	}
	if current == want {
		return nil
	}
	return writeFile(path, want)
}
