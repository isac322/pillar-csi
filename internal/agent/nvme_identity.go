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

package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// NVMe namespace identity contract.
//
// Connected hosts drop a namespace whose identifiers change across a
// reconnect, so every export must present the same identity each time the
// agent recreates its configfs objects (after a storage-node reboot, or any
// other loss of the kernel target state).
//
//   - The identity of a volume is nvmeof.DeriveIdentity of its subsystem NQN,
//     a pure function of the volume ID.  It needs no stored state.
//   - An identity already held by live kernel objects is never replaced: the
//     hosts connected to them cached it, and nvmet cannot change it without
//     disabling the namespace.  Exports created before identities were pinned
//     (pillar-csi 0.2.0 and earlier) carry kernel-random identifiers; the
//     agent adopts them as they are.
//   - An identity that differs from the derived one is persisted per volume
//     lifecycle in the agent state directory (a hostPath that survives
//     reboots), so re-creating the export later reproduces it exactly.  The
//     record is dropped when the export is removed or the volume ID starts a
//     new lifecycle, after which the derived identity applies.

// identityDirName is the agent state subdirectory holding identity records.
const identityDirName = "nvmet-identity"

// identityRecord is the durable identity of one volume's NVMe export.
type identityRecord struct {
	// VolumeUID is the lifecycle (PillarVolumeState UID) the identity
	// belongs to.
	VolumeUID string `json:"volumeUID"`
	// Identity is the identity the export must be recreated with.
	Identity nvmeof.Identity `json:"identity"`
}

func identityRecordName(volumeID string) string {
	return path.Join(identityDirName, strings.TrimSuffix(fencingFilename(volumeID), fencingSuffix)+".json")
}

// resolveNVMeIdentity returns the identity target must be applied with and
// makes it durable when it cannot be re-derived.  It must run inside the
// volume's fenced section, before target.Apply.
func (s *Server) resolveNVMeIdentity(
	volumeID string,
	fence *agentv1.FencingToken,
	target *nvmeof.NvmetTarget,
) (nvmeof.Identity, error) {
	derived := nvmeof.DeriveIdentity(target.SubsystemNQN, target.NamespaceID)
	record, exists, err := s.readIdentityRecord(volumeID)
	if err != nil {
		return nvmeof.Identity{}, err
	}

	want := derived
	if exists && record.VolumeUID == fence.GetVolumeUid() {
		want = record.Identity
	}

	live, err := target.LiveIdentity()
	if err != nil {
		return nvmeof.Identity{}, status.Errorf(codes.Internal,
			"read live NVMe identity of volume %q: %v", volumeID, err)
	}
	if live.Serial != "" {
		want.Serial = live.Serial
	}
	if live.UUID != "" {
		want.UUID = live.UUID
	}
	if live.NGUID != "" {
		want.NGUID = live.NGUID
	}
	err = want.Validate()
	if err != nil {
		return nvmeof.Identity{}, status.Errorf(codes.Internal,
			"NVMe identity of volume %q: %v", volumeID, err)
	}

	switch {
	case want == derived && exists:
		err = s.removeIdentityRecord(volumeID)
	case want != derived && (!exists || record != identityRecord{VolumeUID: fence.GetVolumeUid(), Identity: want}):
		err = s.writeIdentityRecord(volumeID, identityRecord{VolumeUID: fence.GetVolumeUid(), Identity: want})
	}
	if err != nil {
		return nvmeof.Identity{}, err
	}
	return want, nil
}

// readIdentityRecord returns the stored identity record of volumeID.  A
// corrupt record is an error: guessing an identity could drop the namespace
// from connected hosts.
func (s *Server) readIdentityRecord(volumeID string) (identityRecord, bool, error) {
	stateDir := s.resolvedDrainStateDir()
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return identityRecord{}, false, nil
		}
		return identityRecord{}, false, status.Errorf(codes.Internal, "open agent state dir %q: %v", stateDir, err)
	}
	defer root.Close() //nolint:errcheck // read-only handle

	name := identityRecordName(volumeID)
	data, err := root.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return identityRecord{}, false, nil
		}
		return identityRecord{}, false, status.Errorf(codes.Internal, "read NVMe identity record %q: %v", name, err)
	}
	var record identityRecord
	err = json.Unmarshal(data, &record)
	if err == nil && record.VolumeUID == "" {
		err = errors.New("missing volumeUID")
	}
	if err == nil {
		err = record.Identity.Validate()
	}
	if err != nil {
		return identityRecord{}, false, status.Errorf(codes.Internal, "corrupt NVMe identity record %q: %v", name, err)
	}
	return record, true, nil
}

// writeIdentityRecord atomically replaces the identity record of volumeID.
func (s *Server) writeIdentityRecord(volumeID string, record identityRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return status.Errorf(codes.Internal, "encode NVMe identity record for %q: %v", volumeID, err)
	}
	root, err := s.openStateRoot(identityDirName)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // sync errors are returned below

	name := identityRecordName(volumeID)
	tmp := name + ".tmp"
	err = writeFileSynced(root, tmp, data)
	if err != nil {
		return status.Errorf(codes.Internal, "write NVMe identity record %q: %v", tmp, err)
	}
	err = root.Rename(tmp, name)
	if err != nil {
		return status.Errorf(codes.Internal, "rename NVMe identity record %q: %v", name, err)
	}
	return syncStateDirs(root, identityDirName)
}

// removeIdentityRecord deletes the identity record of volumeID, if any.
func (s *Server) removeIdentityRecord(volumeID string) error {
	stateDir := s.resolvedDrainStateDir()
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return status.Errorf(codes.Internal, "open agent state dir %q: %v", stateDir, err)
	}
	defer root.Close() //nolint:errcheck // sync errors are returned below

	name := identityRecordName(volumeID)
	err = root.Remove(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return status.Errorf(codes.Internal, "remove NVMe identity record %q: %v", name, err)
	}
	return syncStateDirs(root, identityDirName)
}
