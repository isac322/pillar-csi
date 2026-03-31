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

import "fmt"

// ─────────────────────────────────────────────────────────────────────────────
// Protocol type constants
// ─────────────────────────────────────────────────────────────────────────────

// Protocol type string constants used in nodeStageState.ProtocolType,
// AttachParams.ProtocolType, and handler map keys.
const (
	// ProtocolNVMeoFTCP identifies the NVMe-oF TCP transport protocol.
	protocolNVMeoFTCP = "nvmeof-tcp"
	protocolISCSI     = "iscsi"
	protocolNFS       = "nfs"
	protocolSMB       = "smb"
)

// ─────────────────────────────────────────────────────────────────────────────
// nodeStageState — discriminated union (RFC Section 5.5.1)
// ─────────────────────────────────────────────────────────────────────────────

// nodeStageState is the on-disk structure persisted during NodeStageVolume.
// It uses a discriminated union pattern (identical to the CRD protocol config
// approach) so that each storage protocol can store its own typed teardown
// parameters without sharing a generic map.
//
// Exactly one of NVMeoF, ISCSI, NFS, or SMB will be non-nil, identified by
// the ProtocolType tag.  This ensures that the fields required by each
// protocol's Detach() implementation are present and type-checked at compile
// time rather than discovered at runtime as missing map keys.
//
// Legacy format (before discriminated union): {"subsys_nqn": "nqn.…"}
// New format: {"protocol_type":"nvmeof-tcp","nvmeof":{"subsys_nqn":"nqn.…",…}}
// readStageState performs in-place migration from the old format.
type nodeStageState struct {
	// ProtocolType identifies which typed sub-struct is populated.
	// Known values: "nvmeof-tcp", "iscsi", "nfs", "smb".
	ProtocolType string `json:"protocol_type"`

	// NVMeoF holds NVMe-oF TCP teardown state.  Non-nil when ProtocolType == "nvmeof-tcp".
	NVMeoF *NVMeoFStageState `json:"nvmeof,omitempty"`

	// ISCSI holds iSCSI teardown state.  Non-nil when ProtocolType == "iscsi".
	ISCSI *ISCSIStageState `json:"iscsi,omitempty"`

	// NFS holds NFS unmount state.  Non-nil when ProtocolType == "nfs".
	NFS *NFSStageState `json:"nfs,omitempty"`

	// SMB holds SMB unmount state.  Non-nil when ProtocolType == "smb".
	SMB *SMBStageState `json:"smb,omitempty"`
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
}

// ISCSIStageState holds the iSCSI parameters needed to log out of an iSCSI
// session during NodeUnstageVolume.
type ISCSIStageState struct {
	// TargetIQN is the iSCSI Qualified Name of the target.
	TargetIQN string `json:"target_iqn"`

	// Portal is the iSCSI portal address in "ip:port" format (e.g. "192.168.1.10:3260").
	Portal string `json:"portal"`

	// LUN is the Logical Unit Number within the iSCSI target.
	LUN int `json:"lun"`
}

// NFSStageState holds the NFS parameters needed to unmount an NFS volume
// during NodeUnstageVolume.
type NFSStageState struct {
	// Server is the IP address or hostname of the NFS server.
	Server string `json:"server"`

	// ExportPath is the server-side export path (e.g. "/mnt/tank/pvc-abc123").
	ExportPath string `json:"export_path"`
}

// SMBStageState holds the SMB/CIFS parameters needed to unmount an SMB share
// during NodeUnstageVolume.
type SMBStageState struct {
	// Server is the IP address or hostname of the SMB server.
	Server string `json:"server"`

	// Share is the SMB share name (e.g. "pvc-abc123").
	Share string `json:"share"`
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
		ProtocolType: protocolNVMeoFTCP,
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
//   - "iscsi"      → nil (not yet implemented)
//   - "nfs"        → nil (not yet implemented; NFS detach is just unmount)
//   - "smb"        → nil (not yet implemented; SMB detach is just unmount)
//
// Returns nil with an error if the protocol type is unrecognized or the
// required sub-struct is absent.
func (s *nodeStageState) ToProtocolState() (ProtocolState, error) {
	if s == nil {
		return nil, fmt.Errorf("nil stage state")
	}
	switch s.ProtocolType {
	case protocolNVMeoFTCP:
		if s.NVMeoF == nil {
			return nil, fmt.Errorf("NVMe-oF stage state sub-struct is nil")
		}
		return &NVMeoFProtocolState{
			SubsysNQN: s.NVMeoF.SubsysNQN,
			Address:   s.NVMeoF.Address,
			Port:      s.NVMeoF.Port,
		}, nil
	case protocolISCSI, protocolNFS, protocolSMB:
		// Protocol states to be populated when those handlers are implemented.
		return nil, fmt.Errorf("protocol %q stage state conversion not yet implemented", s.ProtocolType)
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
//   - Other protocols: only ProtocolType is set; typed sub-structs are populated
//     when those handlers are implemented.
func stageStateFromAttachResult(
	protocolType, targetID, address, port string,
	attachResult *AttachResult,
) *nodeStageState {
	s := &nodeStageState{ProtocolType: protocolType}

	// NVMe-oF TCP: prefer state from AttachResult if it carries a concrete
	// NVMeoFProtocolState; fall back to VolumeContext fields for the legacy path.
	// iSCSI, NFS, SMB: sub-structs populated when those handlers are implemented.
	if protocolType == protocolNVMeoFTCP {
		subsysNQN := targetID
		trAddr := address
		trSvcID := port
		if attachResult != nil {
			if nvmeState, ok := attachResult.State.(*NVMeoFProtocolState); ok && nvmeState != nil {
				subsysNQN = nvmeState.SubsysNQN
				trAddr = nvmeState.Address
				trSvcID = nvmeState.Port
			}
		}
		s.NVMeoF = &NVMeoFStageState{
			SubsysNQN: subsysNQN,
			Address:   trAddr,
			Port:      trSvcID,
		}
	}

	return s
}
