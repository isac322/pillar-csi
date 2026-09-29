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
//   - zfs.ZfsBackend  — ZFS zvol backed by os/exec calls to zfs(8)
//   - lvm.Backend     — LVM logical volume backed by os/exec calls to lvm(8)
type VolumeBackend interface {
	// Create provisions a new block volume (zvol, LV, …) with at least
	// capacityBytes of usable storage.  On success it returns the host path to
	// the block device and the actual allocated size (which may exceed
	// capacityBytes due to backend rounding).
	//
	// params is the backend-specific oneof wrapper from the gRPC request.
	// Each backend implementation extracts the relevant sub-message
	// (e.g. params.GetZfs() for ZFS, params.GetLvm() for LVM).
	// Callers may pass nil when no backend-specific parameters are needed.
	//
	// Idempotent: if a volume with volumeID already exists and has compatible
	// parameters, Create MUST return the existing device path and size without
	// returning an error.
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

	// DevicePath returns the host filesystem path to the block device for
	// the given volumeID without touching the kernel or running any process.
	// The path follows the backend-specific convention:
	//   ZFS zvol  → /dev/zvol/<pool>[/<parentDataset>]/<name>
	//   LVM LV    → /dev/<vg>/<lv>
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
