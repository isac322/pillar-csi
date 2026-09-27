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
// storage parameters.  Each annotation's value is a YAML document whose
// shape is identical to the corresponding CRD subtree — the per-volume
// tunable subset of it; structural fields (pool, volumeGroup, thinPool,
// port, acl, …) are rejected with their path.
//
// The same keys are valid as parameters on a hand-written StorageClass
// (parameters are string scalars, so the same YAML document is the value).
//
// Override hierarchy (lowest wins):
//
//	PillarStore / PillarProtocol (infrastructure defaults)
//	  ↓ override
//	PillarStorageClass (binding-level overrides — CRD typed schema)
//	  ↓ override
//	StorageClass parameter documents (hand-written classes only)
//	  ↓ override
//	PVC annotation (volume-level overrides — only tuning parameters)
const (
	// AnnotationBackendDoc is a PVC annotation whose YAML value decodes as
	// [BackendOverrides].  The selected member must match the store's
	// backend.
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/backend: |
	//       zfs:
	//         properties:
	//           volblocksize: "8K"
	//           compression: zstd
	AnnotationBackendDoc = "pillar-csi.bhyoo.com/backend"

	// AnnotationProtocolDoc is a PVC annotation whose YAML value decodes as
	// [ProtocolOverrides].  The selected member must match the referenced
	// protocol.
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/protocol: |
	//       nvmeofTcp:
	//         maxQueueSize: 64
	AnnotationProtocolDoc = "pillar-csi.bhyoo.com/protocol"

	// AnnotationFilesystemDoc is a PVC annotation whose YAML value decodes as
	// [FilesystemConfig].  Applicable only for volumeMode: Filesystem
	// volumes.
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/filesystem: |
	//       fsType: xfs
	//       mkfsOptions: ["-K"]
	AnnotationFilesystemDoc = "pillar-csi.bhyoo.com/filesystem"
)

// PVCBackendOverride is the Go representation of the [AnnotationBackendDoc]
// annotation value.
//
// Re-uses [BackendOverrides] so that the set of tunable fields is defined
// in exactly one place.
type PVCBackendOverride = BackendOverrides

// PVCProtocolOverride is the Go representation of the
// [AnnotationProtocolDoc] annotation value.
//
// Re-uses [ProtocolOverrides] so that the set of tunable fields is defined
// in exactly one place.
type PVCProtocolOverride = ProtocolOverrides

// PVCFilesystemOverride is the Go representation of the
// [AnnotationFilesystemDoc] annotation value.  It controls the filesystem
// type, mkfs arguments and mount options used when the CSI node formats and
// mounts a newly provisioned block device.
type PVCFilesystemOverride = FilesystemConfig
