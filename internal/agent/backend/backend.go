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

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
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

// VolumeBackend abstracts the storage-backend lifecycle for a single pool.
// All methods MUST be idempotent so that the controller can safely retry.
//
// Implementations:
//   - zfs.ZfsBackend  — ZFS zvol backed by os/exec calls to zfs(8) / zpool(8)
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

	// Capacity returns the total and available byte counts for the pool this
	// backend instance is bound to.
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
}
