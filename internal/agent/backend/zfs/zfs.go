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

// Package zfs implements the VolumeBackend interface for ZFS zvol volumes.
// All ZFS operations are executed via os/exec calls to zfs(8) and zpool(8);
// no ZFS Go library is imported so that the agent binary carries zero CGO
// dependencies and can be cross-compiled easily.
//
// ZFS dataset naming convention used by this backend:
//
//	<pool>/<parentDataset>/<volumeName>   (parentDataset non-empty)
//	<pool>/<volumeName>                   (parentDataset empty)
//
// The corresponding block device appears at:
//
//	/dev/zvol/<pool>/<parentDataset>/<volumeName>
//	/dev/zvol/<pool>/<volumeName>
package zfs

import (
	"context"
	"fmt"
	"path"
	"strings"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"

	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

// devZvolBase is the sysfs prefix under which the kernel exposes zvol block
// devices.  It is a variable (not a constant) so that tests can override it
// without requiring root privileges or a real ZFS pool.  Use SetDevZvolBase
// from tests; never mutate it from production code.
var devZvolBase = "/dev/zvol"

// ZfsBackend implements backend.VolumeBackend using ZFS zvols.
//
// A single ZfsBackend instance is scoped to one ZFS pool and one optional
// parent dataset within that pool.  This mirrors the PillarPool CRD concept
// where each pool maps to exactly one backend instance on an agent.
type ZfsBackend struct {
	// pool is the ZFS pool name (e.g. "hot-data").
	pool string

	// parentDataset is the dataset path relative to pool under which zvols are
	// created (e.g. "k8s").  May be empty, in which case zvols are created
	// directly under the pool root.
	parentDataset string
}

// Verify at compile time that ZfsBackend satisfies the VolumeBackend interface.
var _ backend.VolumeBackend = (*ZfsBackend)(nil)

// New creates a ZfsBackend bound to the given pool and parentDataset.
// Neither argument is validated here; callers should supply values that have
// already been sanitised (no slashes in individual components, no empty pool).
func New(pool, parentDataset string) *ZfsBackend {
	return &ZfsBackend{
		pool:          pool,
		parentDataset: parentDataset,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Path helpers
// ─────────────────────────────────────────────────────────────────────────────

// datasetName returns the fully-qualified ZFS dataset name for a volume.
// volumeID is expected to be in the format "<pool>/<volume-name>" as used
// throughout the agent gRPC API; the pool prefix is stripped before appending
// the optional parentDataset component.
//
// Examples:
//
//	pool="hot-data" parentDataset="k8s"  volumeID="hot-data/pvc-abc"
//	  → "hot-data/k8s/pvc-abc"
//
//	pool="tank"      parentDataset=""     volumeID="tank/pvc-xyz"
//	  → "tank/pvc-xyz"
func (z *ZfsBackend) datasetName(volumeID string) string {
	// Strip the "<pool>/" prefix to obtain the bare volume name.
	volName := strings.TrimPrefix(volumeID, z.pool+"/")

	if z.parentDataset != "" {
		return path.Join(z.pool, z.parentDataset, volName)
	}
	return path.Join(z.pool, volName)
}

// DevicePath returns the host path to the zvol block device for volumeID.
// The path is constructed purely from pool/parentDataset/name metadata — no
// kernel or filesystem calls are made.
//
// The returned path follows the kernel convention:
//
//	/dev/zvol/<pool>/<parentDataset>/<volumeName>
func (z *ZfsBackend) DevicePath(volumeID string) string {
	ds := z.datasetName(volumeID)
	// devZvolBase is overridable for tests; path.Join handles the join cleanly.
	return path.Join(devZvolBase, ds)
}

// ─────────────────────────────────────────────────────────────────────────────
// VolumeBackend interface — stub implementations
// (full os/exec logic is added in subsequent sub-ACs)
// ─────────────────────────────────────────────────────────────────────────────

// Create provisions a ZFS zvol.  The stub returns an "not implemented" error;
// the real implementation (using `zfs create -V`) is added in Sub-AC 2b.
func (z *ZfsBackend) Create(_ context.Context, _ string, _ int64, _ *agentv1.ZfsVolumeParams) (string, int64, error) {
	return "", 0, fmt.Errorf("zfs: Create not yet implemented")
}

// Delete destroys a ZFS zvol.  Stub — real implementation in Sub-AC 2b.
func (z *ZfsBackend) Delete(_ context.Context, _ string) error {
	return fmt.Errorf("zfs: Delete not yet implemented")
}

// Expand resizes a ZFS zvol.  Stub — real implementation in Sub-AC 2b.
func (z *ZfsBackend) Expand(_ context.Context, _ string, _ int64) (int64, error) {
	return 0, fmt.Errorf("zfs: Expand not yet implemented")
}

// Capacity queries the pool for its total and available byte counts.
// Stub — real implementation in Sub-AC 2b.
func (z *ZfsBackend) Capacity(_ context.Context) (int64, int64, error) {
	return 0, 0, fmt.Errorf("zfs: Capacity not yet implemented")
}

// ListVolumes enumerates all zvols under the pool/parentDataset prefix.
// Stub — real implementation in Sub-AC 2b.
func (z *ZfsBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, fmt.Errorf("zfs: ListVolumes not yet implemented")
}
