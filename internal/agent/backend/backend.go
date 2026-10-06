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

// Package backend defines the VolumeBackend interface that every storage-backend
// plugin (ZFS, LVM, …) must implement.  The interface is intentionally thin:
// it only covers the operations that the gRPC AgentService RPCs need; all
// protocol-level concerns (NVMe-oF, iSCSI, …) live in a separate "protocol"
// layer.
package backend

import (
	"context"
	"fmt"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ConflictError is returned by VolumeBackend.Create when a volume with the
// given ID already exists but was created with incompatible parameters (e.g.
// a different capacity).  Callers should map this to gRPC codes.AlreadyExists.
type ConflictError struct {
	VolumeID       string
	ExistingBytes  int64
	RequestedBytes int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf(
		"volume %q already exists with capacity %d bytes, requested %d bytes",
		e.VolumeID, e.ExistingBytes, e.RequestedBytes,
	)
}

// InsufficientCapacityError is returned by VolumeBackend.Create and
// VolumeBackend.Expand when the storage pool (ZFS pool / dataset quota, LVM VG
// or thin pool) cannot hold the requested size.  Callers should map it to gRPC
// codes.ResourceExhausted, the code the CSI spec prescribes for insufficient
// capacity on CreateVolume and ControllerExpandVolume.
//
// Err is the underlying command failure, kept for its diagnostic output.
type InsufficientCapacityError struct {
	VolumeID       string
	RequestedBytes int64
	Err            error
}

func (e *InsufficientCapacityError) Error() string {
	return fmt.Sprintf("insufficient capacity for volume %q (%d bytes requested): %v",
		e.VolumeID, e.RequestedBytes, e.Err)
}

func (e *InsufficientCapacityError) Unwrap() error { return e.Err }

// LayoutMismatchError is returned by VolumeBackend.Create when the request
// declares a volume location (ZFS parent dataset, LVM thin pool) that differs
// from the one this backend was started with (the agent's --config file).
// The backend never relocates a volume to match the request: every other RPC
// (Delete, Expand, Export, ListVolumes) derives the location from the
// backend's own configuration, so a volume created elsewhere would be lost to
// them.  Callers should map this to gRPC codes.FailedPrecondition.
type LayoutMismatchError struct {
	VolumeID string
	// Setting names the disagreeing setting, e.g. "parent dataset".
	Setting    string
	Requested  string
	Configured string
}

func (e *LayoutMismatchError) Error() string {
	return fmt.Sprintf(
		"volume %q: requested %s %q does not match the agent backend's configured %s %q; "+
			"align the PillarStore spec with the agent config file backends entry (chart agent.backends)",
		e.VolumeID, e.Setting, e.Requested, e.Setting, e.Configured,
	)
}

// Layout describes where a backend creates new volumes inside its pool.  The
// agent reports it in GetCapabilities so the controller can compare it with
// the PillarStore spec.
type Layout struct {
	// ParentDataset is the ZFS dataset, relative to the pool, under which
	// volumes are created; empty means the pool root dataset.
	ParentDataset string
	// ThinPool is the LVM thin pool LV thin volumes are created in; empty
	// means the backend has no thin pool.
	ThinPool string
}

// VolumeBackend abstracts the storage-backend lifecycle for a single pool.
// All methods MUST be idempotent so that the controller can safely retry.
//
// Implementations:
//   - zfs.DatasetBackend — ZFS filesystem dataset backed by os/exec calls to zfs(8)
//   - zfs.Backend  — ZFS zvol backed by os/exec calls to zfs(8)
//   - lvm.Backend  — LVM logical volume backed by os/exec calls to lvm(8)
type VolumeBackend interface {
	// Create provisions a new storage volume (filesystem dataset, zvol, LV,
	// …) with at least capacityBytes of usable storage. On success it returns
	// the host path to the mounted directory or block device and the actual
	// capacity (which may exceed capacityBytes due to backend rounding).
	//
	// params is the backend-specific oneof wrapper from the gRPC request.
	// Each backend implementation extracts the relevant sub-message
	// (e.g. params.GetZfs() for ZFS, params.GetLvm() for LVM).
	// Callers may pass nil when no backend-specific parameters are needed.
	//
	// Idempotent: if a volume with volumeID already exists and has compatible
	// parameters, Create MUST return the existing device path and size without
	// returning an error.
	//
	// Filesystem datasets protect the empty, root-owned backing directory
	// before mounting. Recovery MUST NOT chmod an already-mounted filesystem
	// root, whose permissions and contents belong to the volume.
	Create(
		ctx context.Context,
		volumeID string,
		capacityBytes int64,
		params *agentv1.BackendParams,
	) (devicePath string, allocatedBytes int64, err error)

	// Delete destroys the backend storage resource identified by volumeID.
	//
	// Idempotent: if volumeID does not exist, Delete MUST return nil.
	Delete(ctx context.Context, volumeID string) error

	// Expand grows the backend storage resource to at least requestedBytes.
	// It returns the actual size after the operation.
	Expand(ctx context.Context, volumeID string, requestedBytes int64) (allocatedBytes int64, err error)

	// Capacity returns the total and available byte counts of the storage
	// boundary in which this backend instance creates new volumes (ZFS: the
	// parent dataset, or the pool root dataset; LVM: the VG, or the thin pool).
	// availableBytes is what can still be allocated to new volumes there;
	// totalBytes - availableBytes is the space already consumed there.
	Capacity(ctx context.Context) (totalBytes int64, availableBytes int64, err error)

	// ListVolumes returns metadata for all volumes currently present in the pool.
	ListVolumes(ctx context.Context) ([]*agentv1.VolumeInfo, error)

	// DevicePath returns the host filesystem path to the storage resource for
	// the given volumeID without touching the kernel or running a process.
	//   ZFS dataset → persistent mounted filesystem path
	//   ZFS zvol    → /dev/zvol/<pool>[/<parentDataset>]/<name>
	//   LVM LV      → /dev/<vg>/<lv>
	DevicePath(volumeID string) string

	// Type returns the agentv1.BackendType enum value that identifies this
	// backend implementation.  It is used by GetCapabilities to advertise
	// supported backend types and by collectPoolInfo to tag each pool's
	// PoolInfo record with its actual backend type, making both RPCs
	// backend-agnostic.
	Type() agentv1.BackendType

	// Layout returns where this backend creates new volumes inside its pool
	// (ZFS parent dataset, LVM thin pool), as configured at agent start.
	Layout() Layout
}

// CapacityDetails is one capacity probe of a pool: the same total and
// available bytes as [VolumeBackend.Capacity], plus the thin pool's metadata
// usage when the backend provisions from a thin pool.
type CapacityDetails struct {
	TotalBytes     int64
	AvailableBytes int64
	// ThinMetadataUsedRatio is the used fraction (0-1) of the thin pool's
	// metadata LV; meaningful only when HasThinMetadata is true.
	ThinMetadataUsedRatio float64
	HasThinMetadata       bool
}

// CapacityDetailer is implemented by backends that report more than
// [VolumeBackend.Capacity] in the same probe.  Metrics use it instead of
// Capacity when available.
type CapacityDetailer interface {
	CapacityDetails(ctx context.Context) (CapacityDetails, error)
}

// ProvisionedBytesReporter is implemented by backends that can overcommit
// their pool.  ProvisionedBytes returns the summed virtual size of the
// volumes in the pool; ok is false when the backend's configuration cannot
// overcommit (e.g. linear LVM), so there is nothing to report.
type ProvisionedBytesReporter interface {
	ProvisionedBytes(ctx context.Context) (bytes int64, ok bool, err error)
}

// VolumeImporter is implemented by backends that can adopt an already-existing
// storage resource — created by another provisioning stack, e.g. democratic-csi
// or openebs zfs-localpv — into a pillar-csi volume lifecycle.  It backs the
// agent ImportVolume RPC; backends that cannot import return gRPC
// codes.Unimplemented from the RPC handler.
// Import is strictly read-only on the backend: unlike Create it MUST NOT
// create, rename, resize, or format anything.  Implementations refuse with
// ImportRefusedError when:
//   - expectedDataset is non-empty and the volumeID's resolved dataset does
//     not equal it exactly — the request names only pool and leaf, so an
//     agent whose configured layout differs from the controller's must never
//     adopt the dataset its own layout resolves to;
//   - the resolved volumeID does not name an existing resource of the
//     backend's volume type inside its configured Layout;
//   - the resource is still in use on the storage node (a target backstore
//     udev_path, an nvmet namespace device_path, a mount, or an exclusive
//     device claim);
//   - the resource is smaller than capacityBytes.
//
// Idempotent: repeated calls with the same volumeID and compatible capacity
// return the same device path and size.
type VolumeImporter interface {
	// Import returns the host path to the adopted resource and its current
	// size in bytes.  expectedDataset is the full dataset name the caller
	// expects volumeID to resolve to; empty means unchecked (older callers).
	Import(
		ctx context.Context,
		volumeID string,
		capacityBytes int64,
		expectedDataset string,
	) (devicePath string, sizeBytes int64, err error)
}

// ImportRefusedError is returned by VolumeImporter.Import, LVImporter.ImportLV
// and LVVerifier.VerifyLV when the resource cannot safely be adopted or no
// longer matches its pinned identity.  Callers should map it to gRPC
// codes.FailedPrecondition.  Reason names the refusal class for operator
// diagnosis ("missing", "wrong type", "in use", "too small", "layout",
// "inactive", [ImportRefusedReasonIdentity]).
type ImportRefusedError struct {
	VolumeID string
	Reason   string
	// Detail explains the concrete observation (e.g. "mounted at /data",
	// "nvmet subsystem nqn.… owns /dev/zd0").
	Detail string
}

func (e *ImportRefusedError) Error() string {
	return fmt.Sprintf("import of volume %q refused: %s: %s",
		e.VolumeID, e.Reason, e.Detail)
}

// ImportRefusedReasonIdentity is the ImportRefusedError reason for an LV whose
// VG/LV UUIDs or kernel device number differ from the expected identity: the
// name now resolves to a different (renamed, recreated or aliased) LV.
const ImportRefusedReasonIdentity = "identity"

// LVMIdentity names a pre-existing LVM logical volume by name and by its
// stable UUIDs (`lvs -o vg_name,lv_name,vg_uuid,lv_uuid`).
type LVMIdentity struct {
	VolumeGroup       string
	LogicalVolume     string
	VolumeGroupUUID   string
	LogicalVolumeUUID string
}

// LVImporter is implemented by LVM backends that can adopt an existing LV
// into a pillar-csi volume lifecycle.  It backs the agent ImportVolume RPC
// for BACKEND_TYPE_LVM; VolumeImporter stays the ZFS contract.
//
// ImportLV is strictly read-only: it never activates, creates, renames,
// resizes or formats anything.  It refuses with ImportRefusedError when the
// LV does not resolve inside the backend's VG and layout, does not match
// want in every field, is a snapshot/pool/mirror/raid/origin/virtual LV, is
// inactive, is smaller than capacityBytes, or is in use on the storage node.
//
// Idempotent: repeated calls with the same arguments return the same device
// path and size.
type LVImporter interface {
	ImportLV(
		ctx context.Context,
		volumeID string,
		capacityBytes int64,
		want LVMIdentity,
	) (devicePath string, sizeBytes int64, err error)
}

// LVVerifier is implemented by LVM backends that can re-verify that volumeID
// still resolves to the LV pinned at import.  It is read-only and refuses with
// ImportRefusedError: reason "missing" when the LV is gone and
// [ImportRefusedReasonIdentity] when any of the four identity fields differ.
type LVVerifier interface {
	VerifyLV(ctx context.Context, volumeID string, want LVMIdentity) error
}

// LVInspector is implemented by LVM backends that can report a read-only
// observation of an LV.  InspectLV never runs lvchange/vgchange, never mounts
// and writes nothing; its only side effect is one transient O_RDONLY|O_EXCL
// open, closed immediately with no data read, that yields ExclusiveClaim.
type LVInspector interface {
	InspectLV(ctx context.Context, volumeID string) (LVObservation, error)
}

// Exclusive-claim observations reported in LVObservation.ExclusiveClaim.
const (
	// ExclusiveClaimFree means the transient O_EXCL open succeeded.
	ExclusiveClaimFree = "free"
	// ExclusiveClaimBusy means the transient O_EXCL open failed with EBUSY.
	ExclusiveClaimBusy = "busy"
	// ExclusiveClaimUnknown means any other open error, or the LV is inactive.
	ExclusiveClaimUnknown = "unknown"
)

// Filesystem-signature probe outcomes reported in LVObservation.FSProbe.
const (
	// FSProbeDetected means blkid -p reported a well-formed record naming
	// the device with a filesystem TYPE; FSType/FSUUID carry it.
	FSProbeDetected = "detected"
	// FSProbeUnknown means the probe did not establish what the device
	// holds: blkid -p exited 2 with no output (util-linux reports a silent
	// low-level read/probe failure the same way as "nothing found"),
	// reported only a partition table or other TYPE-less records, or the
	// LV is inactive so no probe ran.  It is never evidence that the device
	// is blank or unformatted, and it is not a consumer observation.
	FSProbeUnknown = "unknown"
)

// LVObservation is what InspectLV observed about an LV.  It is an
// observation only and carries no ownership claim.
type LVObservation struct {
	// Identity is the observed name and UUIDs.
	Identity LVMIdentity
	// Attr is lvs lv_attr.
	Attr string
	// Segtype is lvs segtype (e.g. "linear", "thin").
	Segtype string
	// PoolLV is lvs pool_lv; empty for non-thin LVs.
	PoolLV string
	// Origin is lvs origin; non-empty for snapshots.
	Origin string
	// DevicePath is the LV's host device path.
	DevicePath string
	// DevMajorMinor is the kernel device number "<major>:<minor>"; empty
	// when inactive.
	DevMajorMinor string
	// FSType and FSUUID come from blkid -p and are set only when FSProbe is
	// FSProbeDetected.  Empty never means "no signature" by itself.
	FSType string
	FSUUID string
	// FSProbe is FSProbeDetected or FSProbeUnknown.
	FSProbe string
	// FSProbeError explains an FSProbeUnknown outcome (exit status and
	// output, or why no probe ran); empty when FSProbe is FSProbeDetected.
	FSProbeError string
	// SizeBytes is the LV size in bytes.
	SizeBytes int64
	// Active reports lv_attr[4] == 'a'.
	Active bool
	// ExclusiveClaim is one of ExclusiveClaimFree, ExclusiveClaimBusy or
	// ExclusiveClaimUnknown.
	ExclusiveClaim string
	// Consumers lists the observed local consumers of the device: mounts
	// and stacked-device holders.  It never lists exclusive opens or
	// configured exports; ExclusiveClaim and Exports report those.
	Consumers []DeviceConsumer
	// Exports lists every configured export whose recorded device resolves
	// to this LV — a LIO backstore or an nvmet namespace — whether it is
	// the agent's own export of this volume or a foreign one.  Kind is
	// "export" and Detail is the export's target ID verbatim (LIO IQN or
	// nvmet NQN) when it can be resolved, otherwise a description of the
	// backstore or namespace.  An export is configuration, not proof of a
	// live session: callers compare Detail against the target ID their own
	// export of this volume would use to distinguish own from foreign.
	Exports []DeviceConsumer
}

// DeviceConsumer is one observed local consumer of a block device.  Kind is
// one of "mount", "holder", "exclusive_open" or "foreign_export"; Detail is
// kind-specific (mount point, holder device, export target id, ...).
type DeviceConsumer struct {
	Kind   string
	Detail string
}
