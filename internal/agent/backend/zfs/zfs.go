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
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"

	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

// defaultDevZvolBase is the production sysfs prefix under which the kernel
// exposes zvol block devices.  Individual ZfsBackend instances store their
// own copy of this value so that tests can override it per-instance without
// affecting any concurrently-running test goroutine.
const defaultDevZvolBase = "/dev/zvol"

// executor abstracts os/exec so that unit tests can inject a fake without
// running real ZFS commands.  The production path always uses osExecutor.
type executor interface {
	run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// osExecutor is the real executor that delegates to os/exec.
type osExecutor struct{}

func (osExecutor) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	//nolint:gosec,wrapcheck // G204: intentional; raw exit error returned with output.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// execFunc adapts a bare function to the executor interface, making it
// convenient to supply inline fakes in tests.
type execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f execFunc) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

// notExistError is returned by volsizeBytes when the ZFS dataset does not exist.
type notExistError struct {
	dataset string
}

func (e *notExistError) Error() string {
	return fmt.Sprintf("zfs: dataset %q does not exist", e.dataset)
}

// isNotExist reports whether err is (or wraps) a notExistError.
func isNotExist(err error) bool {
	var e *notExistError
	return errors.As(err, &e)
}

// isNotExistOutput returns true when ZFS command output contains the
// standard "dataset does not exist" message emitted by zfs(8).
func isNotExistOutput(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "dataset does not exist") ||
		strings.Contains(s, "does not exist")
}

// Backend implements backend.VolumeBackend using ZFS zvols.
//
// A single Backend instance is scoped to one ZFS pool and one optional
// parent dataset within that pool.  This mirrors the PillarPool CRD concept
// where each pool maps to exactly one backend instance on an agent.
//
// For backward compatibility the exported name ZfsBackend is kept as an alias.
type Backend struct {
	// pool is the ZFS pool name (e.g. "hot-data").
	pool string

	// parentDataset is the dataset path relative to pool under which zvols are
	// created (e.g. "k8s").  May be empty, in which case zvols are created
	// directly under the pool root.
	parentDataset string

	// exec is the executor used to run ZFS commands.  It defaults to
	// osExecutor{} and can be overridden in tests via SetBackendExec.
	// Storing the executor per-instance ensures that parallel tests using
	// different Backend values do not share state.
	exec executor

	// devZvolBase is the path prefix for zvol block devices.  It defaults to
	// defaultDevZvolBase ("/dev/zvol") and can be overridden per-instance in
	// tests via SetBackendDevZvolBase, keeping parallel tests isolated.
	devZvolBase string
}

// ZfsBackend is a type alias kept for API compatibility. Prefer Backend.
//
//nolint:revive // exported: ZfsBackend is the established public name for this type.
type ZfsBackend = Backend

// Verify at compile time that Backend satisfies the VolumeBackend interface.
var _ backend.VolumeBackend = (*Backend)(nil)

// New creates a Backend bound to the given pool and parentDataset.
// Neither argument is validated here; callers should supply values that have
// already been sanitized (no slashes in individual components, no empty pool).
func New(pool, parentDataset string) *Backend {
	return &Backend{
		pool:          pool,
		parentDataset: parentDataset,
		exec:          osExecutor{},
		devZvolBase:   defaultDevZvolBase,
	}
}

// NewWithExecFn creates a Backend that delegates all ZFS command execution to
// fn instead of running real zfs(8)/zpool(8) binaries.  This constructor is
// intended for use in component and integration tests that need to simulate
// ZFS command output without requiring a ZFS-capable host.
//
// Example:
//
//	b := zfs.NewWithExecFn("tank", "k8s", func(ctx context.Context, name string, args ...string) ([]byte, error) {
//	    return []byte("10737418240"), nil
//	})
func NewWithExecFn(
	pool, parentDataset string,
	fn func(ctx context.Context, name string, args ...string) ([]byte, error),
) *Backend {
	return &Backend{
		pool:          pool,
		parentDataset: parentDataset,
		exec:          execFunc(fn),
		devZvolBase:   defaultDevZvolBase,
	}
}

// datasetName returns the fully-qualified ZFS dataset name for a volume.
// VolumeID is expected to be in the format "<pool>/<volume-name>" as used
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
func (z *Backend) datasetName(volumeID string) string {
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
func (z *Backend) DevicePath(volumeID string) string {
	ds := z.datasetName(volumeID)
	return path.Join(z.devZvolBase, ds)
}

// Type returns BACKEND_TYPE_ZFS_ZVOL, identifying this backend as a ZFS zvol
// implementation.  It satisfies the backend.VolumeBackend interface so that
// callers (e.g. GetCapabilities) can report supported backend types
// dynamically without hardcoding ZFS-specific values.
func (*Backend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

// volsizeBytes queries ZFS for the current volsize of dataset in bytes.
// It returns a *notExistError when the dataset does not exist at all, which
// allows callers to distinguish "missing" from other errors.
func (z *Backend) volsizeBytes(ctx context.Context, dataset string) (int64, error) {
	out, err := z.exec.run(ctx, "zfs", "get", "-Hp", "-o", "value", "volsize", dataset)
	if err != nil {
		if isNotExistOutput(out) {
			return 0, &notExistError{dataset: dataset}
		}
		return 0, fmt.Errorf("zfs get volsize %q: %w\n%s", dataset, err, strings.TrimSpace(string(out)))
	}

	sizeStr := strings.TrimSpace(string(out))
	size, parseErr := strconv.ParseInt(sizeStr, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("zfs: parsing volsize output %q for dataset %q: %w", sizeStr, dataset, parseErr)
	}
	return size, nil
}

// Create provisions a ZFS zvol with at least capacityBytes of storage.
//
// If a zvol with this volumeID already exists, Create returns its current
// device path and size without error (idempotent).  If it does not exist,
// `zfs create -V <size>` is called, optionally with per-volume ZFS properties
// from params.Properties.
//
// The actual allocated size is read back after creation and returned; ZFS may
// round the requested size up to the next volblocksize boundary.
func (z *Backend) Create(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	params *agentv1.ZfsVolumeParams,
) (devicePath string, allocatedBytes int64, err error) {
	ds := z.datasetName(volumeID)

	// If the zvol already exists, check that the requested size is compatible.
	// An identical (or compatible) request is returned as-is (idempotent).
	// A request for a different capacity is rejected with ConflictError so
	// that the caller can surface gRPC codes.AlreadyExists with detail.
	existing, err := z.volsizeBytes(ctx, ds)
	if err == nil {
		if existing != capacityBytes {
			return "", existing, &backend.ConflictError{
				VolumeID:       volumeID,
				ExistingBytes:  existing,
				RequestedBytes: capacityBytes,
			}
		}
		return z.DevicePath(volumeID), existing, nil
	}
	if !isNotExist(err) {
		return "", 0, fmt.Errorf("zfs: pre-create existence check for %q: %w", ds, err)
	}

	// -V <size> sets the volsize property; ZFS accepts raw byte integers.
	args := []string{"create", "-V", strconv.FormatInt(capacityBytes, 10)}
	if params != nil {
		// Append arbitrary ZFS properties from the gRPC request.  Unknown
		// property names are forwarded verbatim; zfs(8) will reject them.
		for k, v := range params.GetProperties() {
			args = append(args, "-o", k+"="+v)
		}
	}
	args = append(args, ds)

	out, runErr := z.exec.run(ctx, "zfs", args...)
	if runErr != nil {
		return "", 0, fmt.Errorf("zfs create -V %d %s: %w\n%s",
			capacityBytes, ds, runErr, strings.TrimSpace(string(out)))
	}

	// ZFS rounds volsize up to the nearest volblocksize boundary, so the
	// returned size may be larger than what was requested.
	allocated, err := z.volsizeBytes(ctx, ds)
	if err != nil {
		return "", 0, fmt.Errorf("zfs: reading volsize after create of %q: %w", ds, err)
	}

	return z.DevicePath(volumeID), allocated, nil
}

// Delete destroys the ZFS zvol identified by volumeID.
//
// If the zvol does not exist, Delete returns nil (idempotent).
// A dataset that still has active NVMe-oF or iSCSI exports MUST be unexported
// before calling Delete; otherwise `zfs destroy` will fail with "dataset is
// busy" and an error is returned.
func (z *Backend) Delete(ctx context.Context, volumeID string) error {
	ds := z.datasetName(volumeID)

	out, err := z.exec.run(ctx, "zfs", "destroy", ds)
	if err != nil {
		// `zfs destroy` on a non-existent dataset prints "dataset does not exist"
		// and exits non-zero.  Treat this as a success so the operation is
		// idempotent.
		if isNotExistOutput(out) {
			return nil
		}
		return fmt.Errorf("zfs destroy %s: %w\n%s", ds, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Expand grows the ZFS zvol to at least requestedBytes.
//
// `zfs set volsize=<size>` is called; ZFS enforces that volsize can only
// increase (shrinking is rejected).  The actual size after the operation is
// read back and returned — it may be larger than requestedBytes due to
// volblocksize rounding.
func (z *Backend) Expand(ctx context.Context, volumeID string, requestedBytes int64) (int64, error) {
	ds := z.datasetName(volumeID)

	volsizeArg := "volsize=" + strconv.FormatInt(requestedBytes, 10)
	out, err := z.exec.run(ctx, "zfs", "set", volsizeArg, ds)
	if err != nil {
		return 0, fmt.Errorf("zfs set %s %s: %w\n%s",
			volsizeArg, ds, err, strings.TrimSpace(string(out)))
	}

	// Read back the actual size after rounding.
	actual, err := z.volsizeBytes(ctx, ds)
	if err != nil {
		return 0, fmt.Errorf("zfs: reading volsize after expand of %q: %w", ds, err)
	}
	return actual, nil
}

// Capacity queries the pool for its total and available byte counts.
//
// It runs `zpool list -Hp -o size,free <pool>` which produces a single
// tab-separated line of the form:
//
//	<totalBytes>\t<freeBytes>\n
//
// The -H flag suppresses headers and the -p flag requests exact byte values
// instead of human-readable abbreviations.
func (z *Backend) Capacity(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	out, err := z.exec.run(ctx, "zpool", "list", "-Hp", "-o", "size,free", z.pool)
	if err != nil {
		return 0, 0, fmt.Errorf("zpool list %s: %w\n%s", z.pool, err, strings.TrimSpace(string(out)))
	}

	line := strings.TrimSpace(string(out))
	parts := strings.Split(line, "\t")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("zfs: unexpected zpool list output for pool %q: %q", z.pool, line)
	}

	var parseErr error
	totalBytes, parseErr = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if parseErr != nil {
		return 0, 0, fmt.Errorf("zfs: parsing pool size %q: %w", parts[0], parseErr)
	}

	availableBytes, parseErr = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if parseErr != nil {
		return 0, 0, fmt.Errorf("zfs: parsing pool free space %q: %w", parts[1], parseErr)
	}

	return totalBytes, availableBytes, nil
}

// ListVolumes enumerates all zvols under the pool/parentDataset prefix.
//
// It runs `zfs list -Hp -t volume -o name,volsize -r <listRoot>` which emits
// one tab-separated line per zvol:
//
//	<datasetName>\t<volsizeBytes>
//
// The -H flag suppresses the header, -p requests exact byte counts, -t volume
// restricts output to zvols only, and -r makes the listing recursive.
//
// If the parent dataset does not exist the function returns an empty slice
// (not an error) so that the caller can treat a freshly-created pool as having
// zero volumes rather than failing.
func (z *Backend) ListVolumes(ctx context.Context) ([]*agentv1.VolumeInfo, error) {
	// Build the root dataset to list under (pool or pool/parentDataset).
	listRoot := z.pool
	if z.parentDataset != "" {
		listRoot = path.Join(z.pool, z.parentDataset)
	}

	out, err := z.exec.run(ctx, "zfs", "list", "-Hp", "-t", "volume", "-o", "name,volsize", "-r", listRoot)
	if err != nil {
		// Treat "dataset does not exist" as an empty list so the operation is
		// safe to call on a pool that has not yet had any volumes created.
		if isNotExistOutput(out) {
			return []*agentv1.VolumeInfo{}, nil
		}
		return nil, fmt.Errorf("zfs list -r %s: %w\n%s", listRoot, err, strings.TrimSpace(string(out)))
	}

	// datasetPrefix is the prefix portion we strip from each returned dataset
	// name to recover the bare volume name, e.g. "tank/k8s/" or "tank/".
	datasetPrefix := z.pool + "/"
	if z.parentDataset != "" {
		datasetPrefix = z.pool + "/" + z.parentDataset + "/"
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		// No zvols exist under the root — return an empty (non-nil) slice.
		return []*agentv1.VolumeInfo{}, nil
	}

	lines := strings.Split(trimmed, "\n")
	volumes := make([]*agentv1.VolumeInfo, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("zfs: unexpected zfs list output line %q", line)
		}

		dsName := parts[0]
		sizeStr := strings.TrimSpace(parts[1])

		// Reconstruct volumeID: strip the pool/parentDataset prefix then
		// re-prepend just the pool, giving "<pool>/<volumeName>".
		volName := strings.TrimPrefix(dsName, datasetPrefix)
		volumeID := z.pool + "/" + volName

		sizeBytes, parseErr := strconv.ParseInt(sizeStr, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("zfs: parsing volsize %q for dataset %q: %w", sizeStr, dsName, parseErr)
		}

		volumes = append(volumes, &agentv1.VolumeInfo{
			VolumeId:      volumeID,
			CapacityBytes: sizeBytes,
			DevicePath:    z.DevicePath(volumeID),
		})
	}

	return volumes, nil
}
