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

import "context"

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolHandler — transport abstraction (RFC Section 5.4.2)
// ─────────────────────────────────────────────────────────────────────────────

// ProtocolHandler abstracts transport-level operations for different storage
// protocols. Each protocol (NVMe-oF TCP, iSCSI, NFS, SMB) provides its own
// implementation. The three layers of the node runtime are:
//
//  1. ProtocolHandler (Layer 1): transport/session setup and teardown.
//  2. VolumePresenter (Layer 2): convert Layer 1 output to workload-accessible form.
//  3. CSI node orchestration (Layer 3): NodeStage/Unstage/Publish/Unpublish/Expand.
type ProtocolHandler interface {
	// Attach establishes the transport connection and returns either:
	//   - Block protocols (NVMe-oF, iSCSI): the local block device path
	//     (e.g. /dev/nvme0n1 or /dev/disk/by-path/ip-<ip>:3260-iscsi-<iqn>-lun-<lun>)
	//     via AttachResult.DevicePath.
	//   - File protocols (NFS, SMB): the mount source string
	//     (e.g. "192.168.1.10:/export/vol1" or "//192.168.1.10/share")
	//     via AttachResult.MountSource.
	//
	// The returned AttachResult indicates which presentation path Layer 3 should
	// follow (block vs. file).
	Attach(ctx context.Context, params AttachParams) (*AttachResult, error)

	// Detach tears down the transport connection using the state previously
	// returned by Attach. Implementations must be idempotent: detaching an
	// already-disconnected target must succeed without error.
	Detach(ctx context.Context, state ProtocolState) error

	// Rescan triggers a device or share rescan after online volume expansion.
	//   - NVMe-oF: echo 1 > /sys/class/nvme-ns/<ns>/rescan_controller
	//   - iSCSI:   iscsiadm -m session --rescan
	//   - NFS/SMB: no-op (server-side resize is transparent to the client).
	Rescan(ctx context.Context, state ProtocolState) error
}

// ─────────────────────────────────────────────────────────────────────────────
// AttachParams — protocol-agnostic input to ProtocolHandler.Attach
// ─────────────────────────────────────────────────────────────────────────────

// AttachParams is the protocol-agnostic input to ProtocolHandler.Attach.
// All fields are strings so that the same struct can carry parameters for any
// protocol; protocol handlers interpret fields according to their own semantics.
type AttachParams struct {
	// ProtocolType identifies the storage protocol.
	// Known values: "nvmeof-tcp", "iscsi", "nfs", "smb".
	ProtocolType string

	// ConnectionID is the protocol-specific identifier for the transport target:
	//   - NVMe-oF TCP: subsystem NQN (e.g. "nqn.2024-01.com.example:storage:vol1")
	//   - iSCSI:       target IQN   (e.g. "iqn.2024-01.com.example:storage:vol1")
	//   - NFS:         server IP    (mount source is derived together with VolumeRef)
	//   - SMB:         server IP    (UNC path is derived together with VolumeRef)
	ConnectionID string

	// Address is the IP address (or hostname) of the storage target.
	Address string

	// Port is the TCP/IP port of the storage target encoded as a decimal string,
	// e.g. "4420" for NVMe-oF or "3260" for iSCSI.
	// May be empty for file protocols.
	Port string

	// VolumeRef is the volume-level identifier within the target:
	//   - NVMe-oF: NVM namespace ID  (if needed; often implied by the NQN)
	//   - iSCSI:   LUN number as a decimal string
	//   - NFS:     export path       (e.g. "/mnt/tank/pvc-abc123")
	//   - SMB:     share name        (e.g. "pvc-abc123")
	VolumeRef string

	// Extra carries protocol-specific parameters that do not fit the common
	// fields above. Examples:
	//   - NFS version:        Extra["nfs.version"] = "4.2"
	//   - iSCSI CHAP user:    Extra["chap.username"] = "..."
	//   - SMB subdirectory:   Extra["smb.subdir"] = "data"
	Extra map[string]string
}

// ─────────────────────────────────────────────────────────────────────────────
// AttachResult — protocol-agnostic output of ProtocolHandler.Attach
// ─────────────────────────────────────────────────────────────────────────────

// AttachResult is the protocol-agnostic output of ProtocolHandler.Attach.
// Exactly one of DevicePath or MountSource will be set, indicating whether
// Layer 3 should follow the block presentation path or the file presentation
// path.
type AttachResult struct {
	// DevicePath is set for block protocols (NVMe-oF, iSCSI).
	// It is the local block device node, e.g.:
	//   - NVMe-oF: "/dev/nvme0n1"
	//   - iSCSI:   "/dev/disk/by-path/ip-192.168.1.10:3260-iscsi-iqn.…-lun-0"
	// Empty for file protocols.
	DevicePath string

	// MountSource is set for file protocols (NFS, SMB).
	// It is the source argument to the mount(2) syscall, e.g.:
	//   - NFS: "192.168.1.10:/export/pvc-abc123"
	//   - SMB: "//192.168.1.10/pvc-abc123"
	// Empty for block protocols.
	MountSource string

	// State is the opaque per-protocol state needed by Detach and Rescan.
	// It is persisted in nodeStageState and restored on node restart so that
	// Detach can disconnect the volume even after a reboot.
	State ProtocolState
}

// ─────────────────────────────────────────────────────────────────────────────
// ProtocolState — opaque per-protocol disconnect state
// ─────────────────────────────────────────────────────────────────────────────

// ProtocolState is the opaque, per-protocol state that ProtocolHandler.Detach
// and ProtocolHandler.Rescan need to identify and tear down a transport
// connection. Each protocol handler defines its own concrete implementation
// (e.g. *NVMeoFStageState, *ISCSIStageState) and type-asserts the received
// value to its own type.
//
// ProtocolState values are serialized into nodeStageState JSON on disk and
// reconstructed on node restart to support Detach after a reboot.
type ProtocolState interface {
	// ProtocolType returns the protocol identifier for this state, matching
	// AttachParams.ProtocolType (e.g. "nvmeof-tcp", "iscsi", "nfs", "smb").
	// Used by nodeStageState serialization to select the correct typed sub-struct.
	ProtocolType() string
}
