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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/lio"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// consumerKindForeignExport is the DeviceConsumer kind of a configured
// export of the device that is not this agent's own export of the volume.
const consumerKindForeignExport = "foreign_export"

// InspectVolume reports what the storage node observes about an LV and the
// agent's own record of it: the LV's identity, layout and filesystem
// signature, its local consumers, the agent's configured exports of the
// volume ID, and the durable fence mark including a pinned import source.
// It is strictly read-only and unfenced: it takes no fencing lock, never
// persists or creates anything (an absent mark or state dir is reported as
// absent, not created), never compares or admits a lifecycle and never
// infers ownership — the mark's volume UID is a recorded lifecycle only.
// A configured own export is reported in exports and is never evidence that
// the volume is idle.  A corrupt mark is Internal; a backend other than LVM,
// or one without LVInspector, is Unimplemented.
//
// On a recovery-configured agent (see server_recovery.go) a verified mTLS
// caller additionally receives a signed RecoverySnapshot attesting this
// exact observation, but only while the mark records a live lifecycle
// pinned to the LV the observation resolved; every other report is
// unsigned.
func (s *Server) InspectVolume(
	ctx context.Context,
	req *agentv1.InspectVolumeRequest,
) (*agentv1.InspectVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	s.setVolumeSpanAttributes(ctx, volumeID)
	if req.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM {
		return nil, status.Errorf(codes.Unimplemented,
			"InspectVolume %q: backend type %s is not supported; only BACKEND_TYPE_LVM",
			volumeID, req.GetBackendType())
	}
	b, err := s.backendForType(volumeID, req.GetBackendType())
	if err != nil {
		return nil, err
	}
	err = checkBackendType("InspectVolume", req.GetBackendType(), b.Type(), volumeID)
	if err != nil {
		return nil, err
	}
	inspector, ok := b.(backend.LVInspector)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"InspectVolume %q: backend %s cannot inspect LVs", volumeID, b.Type())
	}

	stored, exists, err := s.readFencingMark(volumeID)
	if err != nil {
		return nil, err
	}
	obs, err := inspector.InspectLV(ctx, volumeID)
	if err != nil {
		if refused, isRefused := errors.AsType[*backend.ImportRefusedError](err); isRefused {
			return nil, status.Errorf(codes.FailedPrecondition, "InspectVolume: %v", refused)
		}
		return nil, status.Errorf(codes.Internal, "InspectVolume %q: %v", volumeID, err)
	}
	exports, err := s.inspectOwnExports(volumeID)
	if err != nil {
		return nil, err
	}

	consumers := inspectConsumers(volumeID, obs)

	resp := &agentv1.InspectVolumeResponse{
		Lvm: &agentv1.LvmObservation{
			Identity: &agentv1.LvmSourceIdentity{
				VolumeGroup:       obs.Identity.VolumeGroup,
				LogicalVolume:     obs.Identity.LogicalVolume,
				VolumeGroupUuid:   obs.Identity.VolumeGroupUUID,
				LogicalVolumeUuid: obs.Identity.LogicalVolumeUUID,
			},
			LvAttr:         obs.Attr,
			Segtype:        obs.Segtype,
			PoolLv:         obs.PoolLV,
			SizeBytes:      obs.SizeBytes,
			Active:         obs.Active,
			DevicePath:     obs.DevicePath,
			DevMajorMinor:  obs.DevMajorMinor,
			Origin:         obs.Origin,
			ExclusiveClaim: obs.ExclusiveClaim,
		},
		FilesystemType:       obs.FSType,
		FilesystemUuid:       obs.FSUUID,
		FilesystemProbeState: obs.FSProbe,
		FilesystemProbeError: obs.FSProbeError,
		Consumers:            consumers,
		Fence:                fenceObservation(stored, exists),
		Exports:              exports,
	}
	// A recovery-configured agent attests its own observation with a signed
	// snapshot, but only when it can state the truth: a live pinned
	// lifecycle whose mark and the observed LV agree.  Absence means
	// unsigned, never an ownership claim.
	if snap, ok := s.recoverySnapshot(ctx, volumeID, stored, exists, obs, consumers, exports); ok {
		resp.Snapshot = snap
	}
	return resp, nil
}

// inspectConsumers lists the LV's local consumers followed by every observed
// export of the device that is not this agent's own export of volumeID.
func inspectConsumers(volumeID string, obs backend.LVObservation) []*agentv1.DeviceConsumer {
	ownIDs := ownTargetIDs(volumeID)
	consumers := make([]*agentv1.DeviceConsumer, 0, len(obs.Consumers)+len(obs.Exports))
	for _, c := range obs.Consumers {
		consumers = append(consumers, &agentv1.DeviceConsumer{Kind: c.Kind, Detail: c.Detail})
	}
	for _, e := range obs.Exports {
		if slices.Contains(ownIDs, e.Detail) {
			continue
		}
		consumers = append(consumers, &agentv1.DeviceConsumer{Kind: consumerKindForeignExport, Detail: e.Detail})
	}
	return consumers
}

// fenceObservation reports the mark verbatim; every field but exists is
// unset when there is no mark.
func fenceObservation(stored fencingMark, exists bool) *agentv1.FenceObservation {
	if !exists {
		return &agentv1.FenceObservation{}
	}
	observation := &agentv1.FenceObservation{
		Exists:           true,
		VolumeUid:        stored.VolumeUID,
		Generation:       stored.Generation,
		Ended:            stored.Ended,
		EndedUids:        slices.Clone(stored.EndedUIDs),
		PreserveOriginal: stored.PreserveOriginal,
		LvmSource:        stored.LVMSource.proto(),
	}
	if transfer := stored.Transfer; transfer != nil {
		observation.TransferAuthorizationDigest = transfer.AuthorizationDigest
		observation.TransferFromUid = transfer.FromUID
		observation.TransferFromGeneration = transfer.FromGeneration
		observation.TransferToUid = transfer.ToUID
		observation.TransferToGeneration = transfer.ToGeneration
	}
	return observation
}

// ownTargetIDs lists the target IDs this agent's own exports of volumeID
// use (NVMe-oF NQN, and the iSCSI IQN when it fits LIO's limit).
func ownTargetIDs(volumeID string) []string {
	var ids []string
	for _, protocol := range []agentv1.ProtocolType{
		agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	} {
		id, err := volumeTargetID(protocol, volumeID)
		if err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// inspectOwnExports reads the agent's own configured NVMe-oF and iSCSI
// exports of volumeID from configfs.  It only reads; a missing target
// directory means no export of that protocol.
func (s *Server) inspectOwnExports(volumeID string) ([]*agentv1.ExportObservation, error) {
	root := s.configfsRoot
	if root == "" {
		root = nvmeof.DefaultConfigfsRoot
	}
	var exports []*agentv1.ExportObservation

	nqn, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "InspectVolume %q: NVMe-oF target ID: %v", volumeID, err)
	}
	nvme, err := inspectNVMeExport(filepath.Join(root, "nvmet", "subsystems", nqn), nqn)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "InspectVolume %q: NVMe-oF export: %v", volumeID, err)
	}
	if nvme != nil {
		exports = append(exports, nvme)
	}

	iqn, iqnErr := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, volumeID)
	if iqnErr == nil {
		iscsi, err := inspectISCSIExport(filepath.Join(root, "target", "iscsi", iqn, lio.TPGName), iqn)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "InspectVolume %q: iSCSI export: %v", volumeID, err)
		}
		if iscsi != nil {
			exports = append(exports, iscsi)
		}
	}
	return exports, nil
}

// inspectNVMeExport observes the nvmet subsystem at dir: any enabled
// namespace, ACL enforcement (attr_allow_any_host "0") and allowed hosts.
func inspectNVMeExport(dir, nqn string) (*agentv1.ExportObservation, error) {
	present, err := dirExists(dir)
	if err != nil || !present {
		return nil, err
	}
	allowAny, err := readOptionalAttr(filepath.Join(dir, "attr_allow_any_host"))
	if err != nil {
		return nil, err
	}
	namespaces, err := listDir(filepath.Join(dir, "namespaces"))
	if err != nil {
		return nil, err
	}
	enabled := false
	for _, ns := range namespaces {
		value, readErr := readOptionalAttr(filepath.Join(dir, "namespaces", ns, "enable"))
		if readErr != nil {
			return nil, readErr
		}
		enabled = enabled || value == "1"
	}
	hosts, err := listDir(filepath.Join(dir, "allowed_hosts"))
	if err != nil {
		return nil, err
	}
	return &agentv1.ExportObservation{
		TargetId:         nqn,
		NamespaceEnabled: enabled,
		AclEnabled:       allowAny == "0",
		AllowedHosts:     hosts,
	}, nil
}

// inspectISCSIExport observes the LIO TPG at dir: TPG enable, explicit
// ACLs (generate_node_acls "0") and the initiator ACLs.
func inspectISCSIExport(dir, iqn string) (*agentv1.ExportObservation, error) {
	present, err := dirExists(dir)
	if err != nil || !present {
		return nil, err
	}
	enable, err := readOptionalAttr(filepath.Join(dir, "enable"))
	if err != nil {
		return nil, err
	}
	generate, err := readOptionalAttr(filepath.Join(dir, "attrib", "generate_node_acls"))
	if err != nil {
		return nil, err
	}
	hosts, err := listDir(filepath.Join(dir, "acls"))
	if err != nil {
		return nil, err
	}
	return &agentv1.ExportObservation{
		TargetId:         iqn,
		NamespaceEnabled: enable == "1",
		AclEnabled:       generate == "0",
		AllowedHosts:     hosts,
	}, nil
}

func dirExists(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %q: %w", dir, err)
	}
	return info.IsDir(), nil
}

// readOptionalAttr returns the trimmed attribute value, "" when absent.
func readOptionalAttr(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is built from the configured configfs root.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// listDir returns the sorted entry names of dir, nil when absent.
func listDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dir %q: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}
