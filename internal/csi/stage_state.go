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

package csi

import (
	"fmt"
	"time"

	"github.com/isac322/pillar-csi/internal/iscsi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Protocol type constants
// ─────────────────────────────────────────────────────────────────────────────

// Protocol type string constants used in nodeStageState.ProtocolType,
// AttachParams.ProtocolType, and handler map keys.
// Exported so that cmd/node can reference them for handler registration.
const (
	// ProtocolNVMeoFTCP identifies the NVMe-oF TCP transport protocol.
	ProtocolNVMeoFTCP = "nvmeof-tcp"

	// ProtocolISCSI identifies the iSCSI (TCP) transport protocol.
	ProtocolISCSI = "iscsi"

	// ProtocolNFS identifies the NFSv4 client protocol.
	ProtocolNFS = "nfs"
)

// CSI access-type string constants persisted in nodeStageState.AccessType so
// that NodeUnstageVolume can pick the correct mount/bind target without a
// VolumeCapability (which the CO does not send on the unstage RPC).
const (
	// AccessTypeFilesystem corresponds to VolumeCapability_Mount: NodeStageVolume
	// formats and mounts the device at stagingTargetPath itself.
	AccessTypeFilesystem = "filesystem"

	// AccessTypeBlock corresponds to VolumeCapability_Block: NodeStageVolume
	// bind-mounts the raw device onto a regular file inside stagingTargetPath
	// (see blockStagingDevicePath).
	AccessTypeBlock = "block"
)

// ─────────────────────────────────────────────────────────────────────────────
// nodeStageState — discriminated union (RFC Section 5.5.1)
// ─────────────────────────────────────────────────────────────────────────────

// nodeStageState is the on-disk structure persisted during NodeStageVolume.
// It uses a discriminated union pattern (identical to the CRD protocol config
// approach) so that each storage protocol can store its own typed teardown
// parameters without sharing a generic map.
//
// The sub-struct matching the ProtocolType tag is non-nil (NVMe-oF TCP, iSCSI,
// and NFS). This ensures that the fields required by each protocol's Detach()
// implementation are present and type-checked at compile time rather than
// discovered at runtime as missing map keys.
//
// Legacy format (before discriminated union): {"subsys_nqn": "nqn.…"}
// New format: {"protocol_type":"nvmeof-tcp","nvmeof":{"subsys_nqn":"nqn.…",…}}
// readStageState performs in-place migration from the old format.
type nodeStageState struct {
	// Known values: "nvmeof-tcp", "iscsi", "nfs".
	ProtocolType string `json:"protocol_type"`

	// AccessType records whether NodeStageVolume staged the volume in
	// Filesystem or Block mode.  NodeUnstageVolume relies on this to pick
	// the matching unmount target without re-receiving the original
	// VolumeCapability.  Legacy state files written before this field
	// existed deserialize to the empty string, which readStageState
	// migrates to AccessTypeFilesystem (the only mode shipped historically).
	AccessType string `json:"access_type,omitempty"`

	// FsType is the filesystem type NodeStageVolume formatted (if the device
	// was blank) and mounted a Filesystem-mode volume with.  It can differ
	// from the PV's csi.fsType when a filesystem document chose the type, and
	// NodeExpandVolume — which receives no VolumeContext — reads it to pick
	// the matching resize tool.  Empty for Block mode and for state files
	// written before the field existed.
	FsType string `json:"fs_type,omitempty"`

	// DevicePath is the block device NodeStageVolume mounted at
	// StagingPath (the protocol-attach device, or the device-mapper path of
	// a local attach).  NodePublishVolume re-mounts it when a kernel-shutdown
	// staged filesystem was dropped and must be repaired in place; records
	// written before the field existed decode empty and fall back to the
	// staged mount's mountinfo source.  Empty for NFS.
	DevicePath string `json:"device_path,omitempty"`

	// NVMeoF holds NVMe-oF TCP teardown state.  Non-nil when ProtocolType == "nvmeof-tcp"
	// and the volume was staged through the protocol handler.
	NVMeoF *NVMeoFStageState `json:"nvmeof,omitempty"`

	// ISCSI holds iSCSI teardown state.  Non-nil when ProtocolType == "iscsi"
	// and the volume was staged through the protocol handler.
	ISCSI *ISCSIStageState `json:"iscsi,omitempty"`

	// NFS holds the exact mount source needed to validate idempotent restage and
	// to type-check teardown after a process restart.
	NFS *NFSStageState `json:"nfs,omitempty"`

	// AttachMode records how NodeStageVolume attached the device:
	// AttachModeLocal for a direct attach on the storage node, empty for a
	// protocol attach.  State files written before local attach existed
	// carry no field and therefore decode as a protocol attach.
	AttachMode string `json:"attach_mode,omitempty"`

	// Local holds the device-mapper claim of a local attach.  Non-nil when
	// AttachMode == AttachModeLocal.
	Local *LocalStageState `json:"local,omitempty"`

	// VolumeID is the CSI volume ID the record belongs to.  The state file
	// name replaces "/" with "_", which cannot be inverted, so the periodic
	// trim loop and the NVMe-oF transfer limit reconciler read the ID from
	// here to take the volume's lock.  Empty in records written before the
	// field existed (see kubeletStagingTarget).
	VolumeID string `json:"volume_id,omitempty"`

	// StagingPath is the staging_target_path NodeStageVolume mounted the
	// volume at.  The periodic trim loop trims this path — never a publish
	// path.  Empty in records written before the field existed.
	StagingPath string `json:"staging_path,omitempty"`

	// StagedAt is when the record was first written by NodeStageVolume.
	// The periodic trim schedules the first trim a random delay after it,
	// so writeStageState preserves the recorded time across every later
	// rewrite of the same record.  Nil in records written before the field
	// existed; the trim loop uses the state file's modification time for
	// them and the next rewrite backfills the field from it.
	StagedAt *time.Time `json:"staged_at,omitempty"`

	// PeriodicTrim is the resolved periodicTrim setting of a Filesystem-mode
	// volume; false opts the volume out of the node's periodic trim.  Nil
	// (unset, or a record written before the field existed) follows the
	// node setting.
	PeriodicTrim *bool `json:"periodic_trim,omitempty"`

	// LastTrim is the start time of the last periodic trim attempt,
	// whatever its outcome.  The next attempt is due one trim interval
	// later.  Nil until the first attempt.
	LastTrim *time.Time `json:"last_trim,omitempty"`

	// PreserveOriginal pins a volume adopted from a pre-existing LV whose
	// data must never be rewritten (issue #163).  NodeStageVolume sets it
	// when the VolumeContext carries VolumeContextKeyPreserveOriginal="true"
	// and never clears it: once true the record stays true for the life of
	// the stage record, so restages after a plugin restart or without the
	// key keep mounting the existing filesystem instead of formatting,
	// fscking or resizing the device.  NodeExpandVolume refuses preserved
	// volumes and the staged-mount repair re-mounts them through
	// MountExisting.
	PreserveOriginal bool `json:"preserve_original,omitempty"`
}

// isLocalAttach reports whether the volume was staged by a local attach.
func (s *nodeStageState) isLocalAttach() bool {
	return s != nil && s.AttachMode == AttachModeLocal
}

// LocalStageState holds what NodeUnstageVolume and NodeExpandVolume need to
// manage the device-mapper target of a local attach.
type LocalStageState struct {
	// DMName is the device-mapper device name (see LocalDMName).
	DMName string `json:"dm_name"`

	// BackingDevice is the backend block device (zvol or logical volume)
	// the linear target maps.
	BackingDevice string `json:"backing_device"`
}

// localStageState builds the stage state of a local attach.  ProtocolType
// is kept so the record still names the volume's transport, but unstage and
// expand dispatch on AttachMode and never reach the protocol handler.
func localStageState(protocolType, accessType, dmName, backingDevice string) *nodeStageState {
	return &nodeStageState{
		ProtocolType: protocolType,
		AccessType:   accessType,
		AttachMode:   AttachModeLocal,
		Local:        &LocalStageState{DMName: dmName, BackingDevice: backingDevice},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Protocol-specific stage state sub-structs
// ─────────────────────────────────────────────────────────────────────────────

// NVMeoFStageState holds the NVMe-oF TCP parameters needed to disconnect an
// NVMe-oF session during NodeUnstageVolume or after a node reboot.
type NVMeoFStageState struct {
	// SubsysNQN is the NVMe Qualified Name of the connected subsystem.
	// Required for connector.Disconnect and sysfs rescan operations.
	SubsysNQN string `json:"subsys_nqn"`

	// Address is the IP address of the NVMe-oF TCP target.
	Address string `json:"address"`

	// Port is the TCP port of the NVMe-oF TCP target (e.g. "4420").
	Port string `json:"port"`

	// MaxDataTransferSize is the volume's max data transfer size in bytes
	// (0: no limit) that NodeStageVolume applied to the namespace devices
	// when the target advertised no MDTS.  The transfer limit reconciler
	// (StartNVMeoFTransferLimitReconciler) keeps re-applying it while the
	// volume stays staged.  Nil in records written before the field
	// existed, which resolve to v1alpha1.DefaultMaxDataTransferSize.
	MaxDataTransferSize *int32 `json:"max_data_transfer_size,omitempty"`
}

// ISCSIStageState holds the iSCSI parameters needed to log out of an iSCSI
// session during NodeUnstageVolume or after a node reboot, and to rescan its
// LUN during NodeExpandVolume.
type ISCSIStageState struct {
	// TargetIQN is the iSCSI Qualified Name of the logged-in target.
	TargetIQN string `json:"target_iqn"`

	// Address is the IP address of the iSCSI target portal.
	Address string `json:"address"`

	// Port is the TCP port of the iSCSI target portal (e.g. "3260").
	Port string `json:"port"`

	// LUN is the logical unit number of the volume within the target.
	LUN int `json:"lun"`

	// LoginTimeoutSeconds is the login timeout the session was staged
	// with.  The initiator keeps it only in memory, so RestoreProtocolSessions
	// re-applies it to the session adopted after a pillar-node restart.
	// Zero (records written before the field existed) means unknown: the
	// initiator default applies.
	LoginTimeoutSeconds int `json:"login_timeout_seconds,omitempty"`

	// CHAP holds the credentials the session logged in with (absent for
	// AuthMethod=None).  The initiator keeps them only in memory and
	// kubelet does not repeat NodeStageVolume for a mounted volume, so
	// RestoreProtocolSessions re-applies them to the session adopted after
	// a pillar-node restart, for its next re-login.  Like open-iscsi's
	// node records, the stage state file is therefore written mode 0600
	// in a 0700 directory (see writeStageState); never log it.
	CHAP *ISCSIStageCHAP `json:"chap,omitempty"`
}

// NFSStageState holds the durable NFS mount identity.
type NFSStageState struct {
	Address     string `json:"address"`
	ExportPath  string `json:"export_path"`
	Port        string `json:"port"`
	Version     string `json:"version"`
	MountSource string `json:"mount_source"`
}

// ISCSIStageCHAP is the persisted form of iscsi.CHAPCredentials.
type ISCSIStageCHAP struct {
	Username       string `json:"username"`
	Secret         string `json:"secret"`
	MutualUsername string `json:"mutual_username,omitempty"`
	MutualSecret   string `json:"mutual_secret,omitempty"`
}

// String redacts the credentials, so a stage state formatted by mistake
// leaks nothing.
func (ISCSIStageCHAP) String() string {
	return "CHAP{secrets redacted}"
}

// GoString redacts the credentials for %#v.
func (c ISCSIStageCHAP) GoString() string {
	return c.String()
}

func iscsiStageCHAP(c *iscsi.CHAPCredentials) *ISCSIStageCHAP {
	if c == nil {
		return nil
	}
	return &ISCSIStageCHAP{
		Username: c.Username, Secret: c.Secret, MutualUsername: c.MutualUsername, MutualSecret: c.MutualSecret,
	}
}

func (c *ISCSIStageCHAP) credentials() *iscsi.CHAPCredentials {
	if c == nil {
		return nil
	}
	return &iscsi.CHAPCredentials{
		Username: c.Username, Secret: c.Secret, MutualUsername: c.MutualUsername, MutualSecret: c.MutualSecret,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Legacy format detection and in-place migration (RFC §5.5.2)
// ─────────────────────────────────────────────────────────────────────────────

// legacyNodeStageState represents the Phase 1 (pre-discriminated-union) on-disk
// format that was written by NodeStageVolume before Phase 2 of the
// multi-protocol driver foundation RFC.
//
// Phase 1 state files contain only a "subsys_nqn" field:
//
//	{"subsys_nqn":"nqn.2024-01.com.example:vol1"}
//
// When readStageState encounters a JSON file without a "protocol_type" field, it
// unmarshals into this struct to recover the NQN and calls migrateFromLegacy to
// produce the Phase 2 format.  The migrated state is then written back to disk
// so subsequent reads and node restarts use the discriminated union path.
type legacyNodeStageState struct {
	// SubsysNQN is the NVMe Qualified Name present in all Phase 1 state files.
	SubsysNQN string `json:"subsys_nqn"`

	// ProtocolType is absent in Phase 1 files; its zero value ("") is used by
	// isLegacyFormat to distinguish old from new files.
	ProtocolType string `json:"protocol_type"`
}

// isLegacyFormat returns true when raw represents a Phase 1 state file.
//
// A Phase 1 file has a non-empty SubsysNQN and no ProtocolType.
func isLegacyFormat(raw *legacyNodeStageState) bool {
	return raw != nil && raw.ProtocolType == "" && raw.SubsysNQN != ""
}

// migrateFromLegacy converts a Phase 1 state into the Phase 2 discriminated
// union format.
//
// The protocol type is assumed to be "nvmeof-tcp" because Phase 1 only
// supported NVMe-oF TCP.  Address and Port are not present in Phase 1 state
// files; they are left empty.  This is safe because NVMeoFTCPHandler.Detach
// only requires SubsysNQN to disconnect a session.
func migrateFromLegacy(raw *legacyNodeStageState) *nodeStageState {
	return &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		// Phase 1 only ever staged Filesystem-mode volumes (Block-mode bind
		// landed in Phase 3 alongside this field), so existing on-disk state
		// is unambiguously filesystem and gets the matching AccessType.
		AccessType: AccessTypeFilesystem,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: raw.SubsysNQN,
			// Address and Port are unavailable from Phase 1 files; Detach only
			// needs SubsysNQN for NVMe-oF TCP session teardown.
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ToProtocolState — derive a ProtocolState for ProtocolHandler.Detach/Rescan
// ─────────────────────────────────────────────────────────────────────────────

// ToProtocolState converts the persisted nodeStageState into a runtime
// ProtocolState value suitable for passing to ProtocolHandler.Detach and
// ProtocolHandler.Rescan.
//
// The mapping is:
//   - "nvmeof-tcp" → *NVMeoFProtocolState  (defined in nvmeof_tcp_handler.go)
//   - "iscsi"      → *ISCSIProtocolState   (defined in iscsi_handler.go)
//
// Returns nil with an error if the protocol type is unrecognized or the
// required sub-struct is absent.
func (s *nodeStageState) ToProtocolState() (ProtocolState, error) {
	if s == nil {
		return nil, fmt.Errorf("nil stage state")
	}
	switch s.ProtocolType {
	case ProtocolNVMeoFTCP:
		if s.NVMeoF == nil {
			return nil, fmt.Errorf("NVMe-oF stage state sub-struct is nil")
		}
		return &NVMeoFProtocolState{
			SubsysNQN: s.NVMeoF.SubsysNQN,
			Address:   s.NVMeoF.Address,
			Port:      s.NVMeoF.Port,
		}, nil
	case ProtocolISCSI:
		if s.ISCSI == nil {
			return nil, fmt.Errorf("iSCSI stage state sub-struct is nil")
		}
		return &ISCSIProtocolState{
			TargetIQN:    s.ISCSI.TargetIQN,
			Address:      s.ISCSI.Address,
			Port:         s.ISCSI.Port,
			LUN:          s.ISCSI.LUN,
			LoginTimeout: time.Duration(s.ISCSI.LoginTimeoutSeconds) * time.Second,
			CHAP:         s.ISCSI.CHAP.credentials(),
		}, nil
	case ProtocolNFS:
		if s.NFS == nil {
			return nil, fmt.Errorf("NFS stage state sub-struct is nil")
		}
		return &NFSProtocolState{
			Address: s.NFS.Address, ExportPath: s.NFS.ExportPath,
			Port: s.NFS.Port, Version: s.NFS.Version, MountSource: s.NFS.MountSource,
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized protocol type %q in persisted stage state", s.ProtocolType)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// stageStateFromAttachResult — build nodeStageState from handler.Attach output
// ─────────────────────────────────────────────────────────────────────────────

// stageStateFromAttachResult constructs the on-disk nodeStageState from the
// parameters used to call ProtocolHandler.Attach and the resulting AttachResult.
//
// This is the inverse of ToProtocolState: given the protocol type and the Attach
// inputs/outputs it produces the typed sub-struct that NodeUnstageVolume needs
// to call Detach after a node reboot.
//
// Mapping:
//   - "nvmeof-tcp": uses targetID (NQN), address, port from VolumeContext.
//     Falls back to NVMeoFProtocolState values from attachResult.State if
//     the result carries a concrete *NVMeoFProtocolState.
//   - "iscsi": uses targetID (IQN), address, port from VolumeContext and
//     LUN 0; the *ISCSIProtocolState from attachResult.State wins when present.
//   - "nfs": uses the typed NFSProtocolState from AttachResult.State so the
//     exact mount source survives a process restart.
func stageStateFromAttachResult(
	protocolType, accessType, targetID, address, port string,
	attachResult *AttachResult,
) *nodeStageState {
	s := &nodeStageState{ProtocolType: protocolType, AccessType: accessType}
	switch protocolType {
	case ProtocolNVMeoFTCP:
		s.NVMeoF = nvmeofStageState(targetID, address, port, attachResult)
	case ProtocolISCSI:
		s.ISCSI = iscsiStageState(targetID, address, port, attachResult)
	case ProtocolNFS:
		s.NFS = nfsStageState(targetID, address, port, attachResult)
	}
	return s
}

func nvmeofStageState(targetID, address, port string, attachResult *AttachResult) *NVMeoFStageState {
	state := &NVMeoFStageState{SubsysNQN: targetID, Address: address, Port: port}
	if attachResult == nil {
		return state
	}
	nvmeState, ok := attachResult.State.(*NVMeoFProtocolState)
	if !ok || nvmeState == nil {
		return state
	}
	state.SubsysNQN = nvmeState.SubsysNQN
	state.Address = nvmeState.Address
	state.Port = nvmeState.Port
	return state
}

func iscsiStageState(targetID, address, port string, attachResult *AttachResult) *ISCSIStageState {
	state := &ISCSIStageState{TargetIQN: targetID, Address: address, Port: port}
	if attachResult == nil {
		return state
	}
	iscsiState, ok := attachResult.State.(*ISCSIProtocolState)
	if !ok || iscsiState == nil {
		return state
	}
	return &ISCSIStageState{
		TargetIQN:           iscsiState.TargetIQN,
		Address:             iscsiState.Address,
		Port:                iscsiState.Port,
		LUN:                 iscsiState.LUN,
		LoginTimeoutSeconds: int(iscsiState.LoginTimeout / time.Second),
		CHAP:                iscsiStageCHAP(iscsiState.CHAP),
	}
}

func nfsStageState(targetID, address, port string, attachResult *AttachResult) *NFSStageState {
	if attachResult != nil {
		nfsState, ok := attachResult.State.(*NFSProtocolState)
		if ok && nfsState != nil {
			return &NFSStageState{
				Address: nfsState.Address, ExportPath: nfsState.ExportPath,
				Port: nfsState.Port, Version: nfsState.Version,
				MountSource: nfsState.MountSource,
			}
		}
		if attachResult.MountSource != "" {
			return &NFSStageState{
				Address: address, Port: port, Version: defaultNFSVersion,
				MountSource: attachResult.MountSource,
			}
		}
	}
	return &NFSStageState{
		Address: address, Port: port, Version: defaultNFSVersion,
		MountSource: address + ":" + targetID,
	}
}
