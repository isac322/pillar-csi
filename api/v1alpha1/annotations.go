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

	// AnnotationImportZvol is a PVC annotation whose plain-string value names
	// an existing ZFS dataset (zvol) to adopt instead of provisioning a new
	// volume, e.g. "hot-data/k8s/pvc-abc123" from a democratic-csi or openebs
	// zfs-localpv installation.  Unlike the document keys above it is not a
	// YAML document and is not valid as a StorageClass parameter.
	//
	// The import is fail-closed: CreateVolume refuses the claim when the
	// dataset does not exist, is not a zvol, does not resolve under the
	// PillarStore's pool and parentDataset, is already tracked by a
	// PillarVolumeState or named by another PVC's import annotation, is
	// smaller than the requested capacity, or is still in use on the storage
	// node (LIO backstore, nvmet namespace, mount, or exclusive open).
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/import-zvol: "hot-data/k8s/pvc-0d5201a5-…"
	AnnotationImportZvol = "pillar-csi.bhyoo.com/import-zvol"

	// AnnotationImportDirectory names an existing host directory to adopt
	// without changing its contents, ownership, or existing project quota.
	// It requires the file CSI driver and is mutually exclusive with the
	// other import selectors. It is not a StorageClass parameter.
	AnnotationImportDirectory = "pillar-csi.bhyoo.com/import-directory"

	// AnnotationImportZFSDataset names an existing ZFS filesystem dataset
	// to adopt without changing its properties or destroying it on deletion.
	// It requires the file CSI driver and is mutually exclusive with the
	// other import selectors. It is not a StorageClass parameter.
	AnnotationImportZFSDataset = "pillar-csi.bhyoo.com/import-zfs-dataset"

	// AnnotationImportLV is a PVC annotation that names an existing LVM
	// logical volume to adopt instead of provisioning a new volume.  The
	// plain-string value is "<vg>/<lv>:<vg_uuid>:<lv_uuid>"; all four parts
	// are required (read the UUIDs with `lvs -o vg_uuid,lv_uuid`).  It is
	// mutually exclusive with the other import selectors ([AnnotationImportZvol],
	// [AnnotationImportDirectory], [AnnotationImportZFSDataset]) on the same
	// claim and is not valid as a StorageClass parameter.
	//
	// Example:
	//   annotations:
	//     pillar-csi.bhyoo.com/import-lv: "data-vg/legacy:<vg_uuid>:<lv_uuid>"
	AnnotationImportLV = "pillar-csi.bhyoo.com/import-lv"

	// AnnotationImportLVPolicy is an optional PVC annotation selecting the
	// adoption policy of an [AnnotationImportLV] claim: one of
	// [ImportLVPolicyPreserveOriginal] (the default when absent) or
	// [ImportLVPolicyManaged].  Any other value is refused.
	AnnotationImportLVPolicy = "pillar-csi.bhyoo.com/import-lv-policy"

	// ImportLVPolicyPreserveOriginal keeps the adopted LV's data intact:
	// DeleteVolume only releases it, expansion is refused, and the node
	// never formats, fscks or resizes it.
	ImportLVPolicyPreserveOriginal = "PreserveOriginal"

	// ImportLVPolicyManaged turns the adopted LV into a normal managed
	// volume that DeleteVolume removes and ExpandVolume may grow.
	ImportLVPolicyManaged = "Managed"
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
