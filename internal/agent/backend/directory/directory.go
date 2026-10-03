/*
Copyright 2026.

Licensed under the Apache License, Version 2.0.
*/

// Package directory adopts existing host directories without provisioning or
// mutating them. All identity and quota checks are read-only.
package directory

import (
	"context"
	"fmt"
	"path/filepath"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

// Option configures a directory backend.
type Option func(*Backend)

// WithHostRootPrefix translates recorded host paths into a node/container path
// for syscalls. Descriptors and layouts always retain the unprefixed host path.
func WithHostRootPrefix(prefix string) Option {
	return func(b *Backend) {
		b.hostRootPrefix = filepath.Clean(prefix)
		if prefix == "" {
			b.hostRootPrefix = ""
		}
	}
}

// WithMountInfoPath overrides the mountinfo source, primarily for isolated
// component tests. Production uses /proc/self/mountinfo.
func WithMountInfoPath(path string) Option {
	return func(b *Backend) {
		if path != "" {
			b.mountInfoPath = path
		}
	}
}

// Backend implements the read-only existing-directory backend.
type Backend struct {
	logicalPool    string
	hostRoot       string
	hostRootPrefix string
	mountInfoPath  string
}

// New creates a backend for one logical pool and trusted host allow-root.
func New(logicalPool, hostRoot string, opts ...Option) *Backend {
	b := &Backend{
		logicalPool:   logicalPool,
		hostRoot:      filepath.Clean(hostRoot),
		mountInfoPath: "/proc/self/mountinfo",
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

var (
	_ backend.VolumeBackend      = (*Backend)(nil)
	_ backend.VolumeInspector    = (*Backend)(nil)
	_ backend.FilesystemImporter = (*Backend)(nil)
)

// Type reports that this backend adopts existing directories.
func (*Backend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_DIRECTORY
}

// Layout reports the configured host-root layout.
func (b *Backend) Layout() backend.Layout { return backend.Layout{HostRoot: b.hostRoot} }

// Create is intentionally refused: adopting a directory never creates an
// empty directory or changes an existing source.
func (b *Backend) Create(
	_ context.Context,
	volumeID string,
	_ int64,
	_ *agentv1.BackendParams,
) (path string, sizeBytes int64, err error) {
	return "", 0, fmt.Errorf(
		"directory backend %q refuses Create for %q: existing sources are read-only",
		b.logicalPool,
		volumeID,
	)
}

// Delete is intentionally refused: the original directory is always preserved.
func (b *Backend) Delete(_ context.Context, volumeID string) error {
	return fmt.Errorf("directory backend %q refuses Delete for %q: original source is preserved", b.logicalPool, volumeID)
}

// Expand is intentionally refused: quota and filesystem expansion are outside
// the adoption contract and must not mutate the source.
func (b *Backend) Expand(_ context.Context, volumeID string, requestedBytes int64) (int64, error) {
	return 0, fmt.Errorf(
		"directory backend %q refuses Expand for %q (%d bytes): adopted quota is read-only",
		b.logicalPool,
		volumeID,
		requestedBytes,
	)
}

// Capacity returns the underlying filesystem's statfs capacity. It is a
// topology/capacity observation only; it does not represent an adoptable quota.
func (b *Backend) Capacity(ctx context.Context) (totalBytes, availableBytes int64, err error) {
	return capacity(ctx, b.sysPath(b.hostRoot))
}

// ListVolumes has no provisioned resources to enumerate.
func (*Backend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) { return nil, nil }

// DevicePath cannot resolve an adopted resource from a volume ID without the
// persisted descriptor, so callers must use FilesystemMountSource instead.
func (*Backend) DevicePath(string) string { return "" }

// InspectImport validates an existing directory and reports its native identity
// and exact effective project-quota capacity without mutating the source.
func (b *Backend) InspectImport(
	ctx context.Context,
	source string,
	requiredBytes int64,
	expectedLayout backend.Layout,
) (*backend.ImportInspection, error) {
	err := b.validateLayout(expectedLayout)
	if err != nil {
		return nil, err
	}
	// A read-only inspection keeps ownership of its descriptor until inspect
	// closes it; only ImportFilesystem requests a transferred pin.
	result := b.inspectResult(ctx, source, requiredBytes, nil, false)
	if result.err != nil {
		return nil, fmt.Errorf("directory backend inspect %q: %w", source, result.err)
	}
	return result.inspection, nil
}

// ImportFilesystem validates and pins an existing directory for one operation.
// The returned pin owns its descriptor until Close is called.
func (b *Backend) ImportFilesystem(
	ctx context.Context,
	volumeID string,
	requiredBytes int64,
	expected *agentv1.FilesystemAdoption,
	expectedLayout backend.Layout,
) (backend.PinnedFilesystem, error) {
	if expected == nil {
		return nil, fmt.Errorf("directory backend import %q: nil filesystem adoption descriptor", volumeID)
	}
	err := b.validateLayout(expectedLayout)
	if err != nil {
		return nil, err
	}
	result := b.inspectResult(ctx, expected.GetCanonicalSource(), requiredBytes, expected, true)
	if result.err != nil {
		return nil, fmt.Errorf("directory backend import %q: %w", volumeID, result.err)
	}
	return makePinned(result.pin, result.inspection.Filesystem, result.inspection.CapacityBytes, b)
}

type inspectionResult struct {
	inspection *backend.ImportInspection
	pin        *dirPin
	err        error
}

func (b *Backend) inspectResult(
	ctx context.Context,
	source string,
	requiredBytes int64,
	expected *agentv1.FilesystemAdoption,
	pin bool,
) inspectionResult {
	var result inspectionResult
	result.inspection, result.pin, result.err = b.inspect(ctx, source, requiredBytes, expected, pin)
	return result
}

func (b *Backend) validateLayout(layout backend.Layout) error {
	configured := filepath.Clean(b.hostRoot)
	requested := filepath.Clean(layout.HostRoot)
	if requested == "." {
		requested = ""
	}
	if requested != "" && requested != configured {
		return &backend.LayoutMismatchError{Setting: "directory host root", Requested: requested, Configured: configured}
	}
	if configured == "." || !filepath.IsAbs(configured) {
		return fmt.Errorf("directory backend: configured host root %q is not absolute", b.hostRoot)
	}
	return nil
}

func (b *Backend) sysPath(hostPath string) string {
	if b.hostRootPrefix == "" {
		return hostPath
	}
	return filepath.Join(b.hostRootPrefix, filepath.FromSlash(hostPath))
}
