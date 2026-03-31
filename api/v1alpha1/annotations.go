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

package v1alpha1

// Annotation keys written on a PersistentVolumeClaim to override per-volume
// storage parameters.
//
// Override hierarchy (lowest wins):
//
//	PillarPool (pool-level defaults)
//	  ↓ override
//	PillarProtocol (protocol defaults)
//	  ↓ override
//	PillarBinding (binding-level overrides — CRD typed schema)
//	  ↓ override
//	PVC annotation (volume-level overrides — only tuning parameters)
//
// Restriction: only tuning parameters may be overridden via PVC annotations.
// Structural reference fields (pool name, parentDataset, protocol type, port,
// ACL toggle, poolRef, protocolRef, targetRef) are rejected by the controller
// at CreateVolume time.  See [ForbiddenZFSAnnotationKeys],
// [ForbiddenNVMeOFTCPAnnotationKeys], and [ForbiddenISCSIAnnotationKeys].
const (
	// AnnotationBackendOverride is a PVC annotation whose YAML value is
	// decoded as [PVCBackendOverride].
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/backend-override: |
	//       zfs:
	//         properties:
	//           volblocksize: "8K"
	//           compression: zstd
	AnnotationBackendOverride = "pillar-csi.bhyoo.com/backend-override"

	// AnnotationProtocolOverride is a PVC annotation whose YAML value is
	// decoded as [PVCProtocolOverride].
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/protocol-override: |
	//       nvmeofTcp:
	//         maxQueueSize: 64
	//       iscsi:
	//         loginTimeout: 30
	AnnotationProtocolOverride = "pillar-csi.bhyoo.com/protocol-override"

	// AnnotationFSOverride is a PVC annotation whose YAML value is decoded
	// as [PVCFSOverride].  Only applicable to block protocols
	// (nvmeof-tcp, iscsi) combined with volumeMode: Filesystem.
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/fs-override: |
	//       fsType: xfs
	//       mkfsOptions: ["-K"]
	AnnotationFSOverride = "pillar-csi.bhyoo.com/fs-override"
)

// ─────────────────────────────────────────────────────────────────────────────
// PVC annotation value types
// ─────────────────────────────────────────────────────────────────────────────.

// PVCBackendOverride is the Go representation of the
// [AnnotationBackendOverride] annotation value.
//
// Re-uses [BackendOverrides] so that the set of tunable fields is defined
// in exactly one place.
type PVCBackendOverride = BackendOverrides

// PVCProtocolOverride is the Go representation of the
// [AnnotationProtocolOverride] annotation value.
//
// Re-uses [ProtocolOverrides] so that the set of tunable fields is defined
// in exactly one place.
type PVCProtocolOverride = ProtocolOverrides

// PVCFSOverride is the Go representation of the [AnnotationFSOverride]
// annotation value.  It controls the filesystem type and mkfs arguments used
// when the CSI node formats a newly provisioned block device.
//
// Applicable only for block protocols (nvmeof-tcp, iscsi) combined with
// volumeMode: Filesystem.  Ignored for NFS volumes.
type PVCFSOverride struct {
	// fsType is the filesystem to create on the block device.
	// Supported values: ext4, xfs. Defaults to the PillarProtocol fsType
	// (which itself defaults to ext4).
	// +optional
	// +kubebuilder:validation:Enum=ext4;xfs
	FSType string `json:"fsType,omitempty" yaml:"fsType,omitempty"`

	// mkfsOptions are additional arguments passed verbatim to mkfs when
	// formatting the device.  Each element is a separate shell token
	// (e.g. ["-K"] disables zero-initialization for xfs).
	// +optional
	MkfsOptions []string `json:"mkfsOptions,omitempty" yaml:"mkfsOptions,omitempty"`
}

// ─────────────────────────────────────────────────────────────────────────────
// Forbidden structural annotation key sets
// ─────────────────────────────────────────────────────────────────────────────
//
// The controller must reject a PVC annotation that attempts to change any of
// these structural reference fields.  Structural fields define topology,
// identity, or security anchors; they cannot be changed on a per-volume basis.
//
// Detection strategy: after decoding each annotation into a
// map[string]interface{}, verify that no top-level (or protocol-section) key
// belongs to the corresponding forbidden set.

// ForbiddenZFSAnnotationKeys is the set of JSON/YAML field names that are
// structural within the ZFS backend section of [AnnotationBackendOverride].
//
// These correspond to [ZFSBackendConfig] fields that identify the pool
// topology and cannot be overridden per-volume:
//   - "pool"          — ZFS pool name
//   - "parentDataset" — parent dataset path under which volumes are created
//
// Note: the "properties" map itself is allowed; this set governs only the
// sibling keys of "properties" within the "zfs" section.
var ForbiddenZFSAnnotationKeys = map[string]struct{}{
	"pool":          {},
	"parentDataset": {},
}

// ForbiddenLVMAnnotationKeys is the set of JSON/YAML field names that are
// structural within the LVM backend section of [AnnotationBackendOverride].
//
// These correspond to [LVMBackendConfig] fields that identify the volume
// group / thin pool topology and cannot be overridden per-volume:
//   - "volumeGroup" — LVM Volume Group name
//   - "thinPool"    — thin pool LV name within the VG
var ForbiddenLVMAnnotationKeys = map[string]struct{}{
	"volumeGroup": {},
	"thinPool":    {},
}

// ForbiddenNVMeOFTCPAnnotationKeys is the set of JSON/YAML field names that
// are structural within the nvmeofTcp section of [AnnotationProtocolOverride].
//
// These correspond to [NVMeOFTCPConfig] fields that are infrastructure/security
// anchors and cannot be changed per-volume:
//   - "port" — target TCP port; changing it would route to a different listener
//   - "acl"  — ACL on/off is a security policy; must not be per-volume
var ForbiddenNVMeOFTCPAnnotationKeys = map[string]struct{}{
	"port": {},
	"acl":  {},
}

// ForbiddenISCSIAnnotationKeys is the set of JSON/YAML field names that are
// structural within the iscsi section of [AnnotationProtocolOverride].
//
// These correspond to [ISCSIConfig] fields that are infrastructure/security
// anchors:
//   - "port" — target TCP port
//   - "acl"  — ACL on/off is a security policy; must not be per-volume
var ForbiddenISCSIAnnotationKeys = map[string]struct{}{
	"port": {},
	"acl":  {},
}

// ─────────────────────────────────────────────────────────────────────────────
// Supported override key documentation
// ─────────────────────────────────────────────────────────────────────────────.

// SupportedZFSAnnotationKeys enumerates the JSON/YAML field names that are
// supported within the zfs section of [AnnotationBackendOverride].
//
// Currently the only supported key is "properties": a map of arbitrary ZFS
// dataset/zvol properties (e.g. compression, volblocksize, recordsize).
//
// This set is used in error messages to guide users toward valid overrides.
var SupportedZFSAnnotationKeys = map[string]struct{}{
	"properties": {},
}

// SupportedLVMAnnotationKeys enumerates the JSON/YAML field names that are
// supported within the lvm section of [AnnotationBackendOverride].
//
// Currently the only supported key is "provisioningMode": selects between
// "linear" (fully-allocated) and "thin" (thin-provisioned) LV creation.
var SupportedLVMAnnotationKeys = map[string]struct{}{
	"provisioningMode": {},
}

// SupportedNVMeOFTCPAnnotationKeys enumerates the JSON/YAML field names that
// are supported within the nvmeofTcp section of [AnnotationProtocolOverride].
var SupportedNVMeOFTCPAnnotationKeys = map[string]struct{}{
	"maxQueueSize":      {},
	"inCapsuleDataSize": {},
	"ctrlLossTmo":       {},
	"reconnectDelay":    {},
}

// SupportedISCSIAnnotationKeys enumerates the JSON/YAML field names that are
// supported within the iscsi section of [AnnotationProtocolOverride].
var SupportedISCSIAnnotationKeys = map[string]struct{}{
	"loginTimeout":       {},
	"replacementTimeout": {},
	"nodeSessionTimeout": {},
}
