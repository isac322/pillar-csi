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

package zfs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const (
	defaultDatasetMountRoot = "/var/lib/pillar-csi/agent/datasets"
	datasetPoolSetting      = "ZFS pool"
	datasetQuotaUnset       = "none"
	datasetMounted          = "yes"
	datasetFilesystem       = "filesystem"
	datasetFSType           = "zfs"
)

// DatasetBackend implements backend.VolumeBackend using ZFS filesystem datasets.
// Each volume is a mounted filesystem dataset with a managed refquota. The
// returned resource path is the durable mountpoint, not a block device.
type DatasetBackend struct {
	pool          string
	parentDataset string
	mountRoot     string
	exec          executor
	mountInfoPath string
}

// DatasetOption customizes a DatasetBackend.
type DatasetOption func(*DatasetBackend)

// WithDatasetMountRoot overrides the persistent root used for volume mountpoints.
func WithDatasetMountRoot(root string) DatasetOption {
	return func(b *DatasetBackend) {
		if root != "" {
			b.mountRoot = root
		}
	}
}

// WithDatasetMountInfoPath selects the namespace mount table used to verify
// the exact ZFS mount. It is primarily useful for isolated component tests.
func WithDatasetMountInfoPath(path string) DatasetOption {
	return func(b *DatasetBackend) { b.mountInfoPath = path }
}

// NewDataset creates a filesystem-dataset backend for one ZFS pool and layout.
// The mount root MUST be a persistent hostPath shared across agent restarts;
// an empty value uses the production agent-state location.
func NewDataset(pool, parentDataset, mountRoot string, opts ...DatasetOption) *DatasetBackend {
	if mountRoot == "" {
		mountRoot = defaultDatasetMountRoot
	}
	b := &DatasetBackend{
		pool:          pool,
		parentDataset: parentDataset,
		mountRoot:     filepath.Clean(mountRoot),
		exec:          osExecutor{},
		mountInfoPath: "/proc/self/mountinfo",
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// NewDatasetWithExecFn creates a dataset backend backed by fn instead of
// invoking zfs(8). It is intended for component tests.
func NewDatasetWithExecFn(
	pool, parentDataset, mountRoot string,
	fn func(context.Context, string, ...string) ([]byte, error),
	opts ...DatasetOption,
) *DatasetBackend {
	b := NewDataset(pool, parentDataset, mountRoot, opts...)
	b.exec = execFunc(fn)
	return b
}

var _ backend.VolumeBackend = (*DatasetBackend)(nil)
var _ backend.ProvisionedBytesReporter = (*DatasetBackend)(nil)

// Type reports the filesystem-dataset backend kind.
func (*DatasetBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
}

// Layout returns the configured parent dataset.
func (d *DatasetBackend) Layout() backend.Layout {
	return backend.Layout{ParentDataset: normalizeDataset(d.parentDataset)}
}

func (d *DatasetBackend) provisioningRoot() string {
	if d.parentDataset == "" {
		return d.pool
	}
	return filepath.ToSlash(filepath.Join(d.pool, filepath.FromSlash(normalizeDataset(d.parentDataset))))
}

func validDatasetPath(s string, allowEmpty bool) bool {
	if s == "" {
		return allowEmpty
	}
	if filepath.IsAbs(s) || strings.Contains(s, "\\") {
		return false
	}
	for component := range strings.SplitSeq(filepath.ToSlash(s), "/") {
		if !validVolumeLeaf(component) {
			return false
		}
	}
	return true
}

func validVolumeLeaf(s string) bool {
	if s == "" || s == "." || s == ".." || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !validVolumeCharacter(r) {
			return false
		}
	}
	return true
}

func validVolumeCharacter(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return r == '_' || r == '-' || r == '.' || r == ':'
	}
}

func (d *DatasetBackend) resolve(volumeID string) (dataset, leaf string, err error) {
	if !validVolumeLeaf(d.pool) || !validDatasetPath(d.parentDataset, true) {
		return "", "", fmt.Errorf("zfs dataset: invalid configured pool/layout")
	}
	prefix := d.pool + "/"
	if !strings.HasPrefix(volumeID, prefix) {
		return "", "", &backend.LayoutMismatchError{
			VolumeID: volumeID, Setting: datasetPoolSetting, Requested: volumeID, Configured: d.pool,
		}
	}
	leaf = strings.TrimPrefix(volumeID, prefix)
	if !validVolumeLeaf(leaf) {
		return "", "", fmt.Errorf("zfs dataset: volume ID %q is not a single safe volume name", volumeID)
	}
	if d.parentDataset == "" {
		return d.pool + "/" + leaf, leaf, nil
	}
	return d.pool + "/" + normalizeDataset(d.parentDataset) + "/" + leaf, leaf, nil
}

func (d *DatasetBackend) mountPath(leaf string) string {
	if !filepath.IsAbs(d.mountRoot) {
		return ""
	}
	if !validVolumeLeaf(d.pool) || !validDatasetPath(d.parentDataset, true) || !validVolumeLeaf(leaf) {
		return ""
	}
	parts := []string{d.mountRoot, d.pool}
	if parent := normalizeDataset(d.parentDataset); parent != "" {
		parts = append(parts, filepath.FromSlash(parent))
	}
	parts = append(parts, leaf)
	candidate := filepath.Clean(filepath.Join(parts...))
	root := filepath.Clean(d.mountRoot)
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return candidate
}

func (d *DatasetBackend) validateParams(volumeID string, params *agentv1.BackendParams) error {
	if params == nil {
		return nil
	}
	zfsParams := params.GetZfs()
	if zfsParams == nil {
		return fmt.Errorf("zfs dataset backend: backend parameters do not select ZFS")
	}
	if zfsParams.GetPool() != "" && zfsParams.GetPool() != d.pool {
		return &backend.LayoutMismatchError{
			VolumeID: volumeID, Setting: datasetPoolSetting, Requested: zfsParams.GetPool(), Configured: d.pool,
		}
	}
	requested := normalizeDataset(zfsParams.GetParentDataset())
	configured := normalizeDataset(d.parentDataset)
	if requested != configured {
		return &backend.LayoutMismatchError{
			VolumeID: volumeID, Setting: "ZFS parent dataset", Requested: requested, Configured: configured,
		}
	}
	for key := range zfsParams.GetProperties() {
		switch strings.ToLower(key) {
		case "refquota", "mountpoint", "sharenfs":
			return fmt.Errorf("zfs dataset backend: property %q is managed and cannot be overridden", key)
		case "volsize", "volblocksize", "volmode", "volthreading", "sparse":
			return fmt.Errorf("zfs dataset backend: zvol-only property %q is not supported", key)
		}
	}
	return nil
}

type datasetState struct {
	typeName   string
	refquota   int64
	mountpoint string
	sharenfs   string
	mounted    string
}

func parseDatasetState(out []byte, dataset string) (datasetState, error) {
	var state datasetState
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		property, value, ok := strings.Cut(line, "\t")
		if !ok {
			return datasetState{}, fmt.Errorf("zfs: unexpected property output for dataset %q: %q", dataset, line)
		}
		err := state.setProperty(property, strings.TrimSpace(value), dataset)
		if err != nil {
			return datasetState{}, err
		}
	}
	if state.typeName == "" || state.mountpoint == "" || state.sharenfs == "" || state.mounted == "" {
		return datasetState{}, fmt.Errorf("zfs: incomplete properties for dataset %q", dataset)
	}
	return state, nil
}

func (s *datasetState) setProperty(property, value, dataset string) error {
	switch property {
	case "type":
		s.typeName = value
	case "refquota":
		quota, err := parseDatasetQuota(value, dataset)
		if err != nil {
			return err
		}
		// Unset properties do not replace an earlier numeric value.
		if value != datasetQuotaUnset && value != "-" {
			s.refquota = quota
		}
	case "mountpoint":
		s.mountpoint = value
	case "sharenfs":
		s.sharenfs = value
	case "mounted":
		s.mounted = value
	}
	return nil
}

func parseDatasetQuota(value, dataset string) (int64, error) {
	if value == datasetQuotaUnset || value == "-" {
		return 0, nil
	}
	quota, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("zfs: invalid refquota %q for dataset %q: %w", value, dataset, err)
	}
	if quota < 0 {
		return 0, fmt.Errorf("zfs: invalid refquota %q for dataset %q", value, dataset)
	}
	return quota, nil
}

func (d *DatasetBackend) readState(ctx context.Context, dataset string) (datasetState, error) {
	out, err := d.exec.run(ctx, datasetFSType, "get", "-Hp", "-o", "property,value",
		"type,refquota,mountpoint,sharenfs,mounted", dataset)
	if err != nil {
		if isNotExistOutput(out) {
			return datasetState{}, &notExistError{dataset: dataset}
		}
		return datasetState{}, fmt.Errorf("zfs get dataset properties %q: %w\n%s",
			dataset, err, strings.TrimSpace(string(out)))
	}
	return parseDatasetState(out, dataset)
}

func validateDatasetState(dataset, mountpoint string, state datasetState, requested int64) error {
	if state.typeName != datasetFilesystem {
		return fmt.Errorf("zfs dataset %q has type %q, want filesystem", dataset, state.typeName)
	}
	if state.mountpoint != mountpoint {
		return fmt.Errorf("zfs dataset %q has mountpoint %q, want %q", dataset, state.mountpoint, mountpoint)
	}
	if state.sharenfs != "off" {
		return fmt.Errorf("zfs dataset %q has sharenfs=%q, want off", dataset, state.sharenfs)
	}
	if state.refquota <= 0 {
		return fmt.Errorf("zfs dataset %q must have a positive managed refquota", dataset)
	}
	if requested > 0 && state.refquota < requested {
		return &backend.ConflictError{VolumeID: dataset, ExistingBytes: state.refquota, RequestedBytes: requested}
	}
	return nil
}

func (d *DatasetBackend) ensureMounted(ctx context.Context, dataset, mountpoint string, state datasetState) error {
	expectedQuota := state.refquota
	err := validateDatasetMountPath(mountpoint)
	if err != nil {
		return err
	}
	if state.mounted != datasetMounted {
		err = d.prepareMountpoint(dataset, mountpoint)
		if err != nil {
			return err
		}
		out, runErr := d.exec.run(ctx, datasetFSType, "mount", dataset)
		if runErr != nil {
			current, readErr := d.readState(ctx, dataset)
			if readErr == nil && current.mounted == datasetMounted && current.mountpoint == mountpoint {
				err = validateDatasetState(dataset, mountpoint, current, expectedQuota)
				if err != nil {
					return err
				}
				return d.verifyMount(dataset, mountpoint)
			}
			return fmt.Errorf("zfs mount %q: %w\n%s", dataset, runErr, strings.TrimSpace(string(out)))
		}
	}
	current, err := d.readState(ctx, dataset)
	if err != nil {
		return err
	}
	err = validateDatasetState(dataset, mountpoint, current, expectedQuota)
	if err != nil {
		return err
	}
	if current.mounted != datasetMounted || current.mountpoint != mountpoint {
		return fmt.Errorf("zfs dataset %q is not mounted at %q", dataset, mountpoint)
	}
	return d.verifyMount(dataset, mountpoint)
}

func (d *DatasetBackend) verifyMount(dataset, mountpoint string) error {
	err := validateDatasetMountPath(mountpoint)
	if err != nil {
		return err
	}
	info, err := os.Stat(mountpoint)
	if err != nil {
		return fmt.Errorf("zfs dataset %q stat mountpoint: %w", dataset, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("zfs dataset %q mountpoint %q is not a directory", dataset, mountpoint)
	}
	fsType, source, err := d.mountAt(mountpoint)
	if err != nil {
		return err
	}
	if fsType != datasetFSType || source != dataset {
		return fmt.Errorf("zfs dataset %q is not the ZFS mount at %q in this namespace", dataset, mountpoint)
	}
	return nil
}

func (d *DatasetBackend) mountAt(mountpoint string) (fsType, source string, err error) {
	file, err := os.Open(d.mountInfoPath)
	if err != nil {
		return "", "", fmt.Errorf("zfs dataset read namespace mount table: %w", err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // read-only file
	scanner := bufio.NewScanner(file)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := -1
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+3 {
			return "", "", fmt.Errorf("zfs dataset: malformed mountinfo line %q", scanner.Text())
		}
		if unescape.Replace(fields[4]) == mountpoint {
			// Last entry wins when another mount is stacked over this path.
			fsType, source = fields[sep+1], unescape.Replace(fields[sep+2])
		}
	}
	err = scanner.Err()
	if err != nil {
		return "", "", fmt.Errorf("zfs dataset read mountinfo: %w", err)
	}
	return fsType, source, nil
}

// prepareMountpoint protects the otherwise visible backing directory below an
// NFSv4 pseudoroot. Never chmod a mounted dataset's root or unmount a live
// filesystem to retrofit an older backing directory: its hidden permissions
// cannot be safely inspected until the dataset is normally unmounted.
func (d *DatasetBackend) prepareMountpoint(dataset, mountpoint string) error {
	err := validateDatasetMountPath(mountpoint)
	if err != nil {
		return err
	}
	mounted, err := d.mountpointOccupied(dataset, mountpoint)
	if err != nil || mounted {
		return err
	}
	err = d.prepareBackingPath(dataset, mountpoint)
	if err != nil {
		return err
	}
	// Bind permission updates to this directory inode, not a pathname that
	// a concurrent mount could replace with the filesystem's own root.
	//nolint:gosec // G304: validated managed mountpoint, no symlink following.
	dir, err := os.OpenFile(mountpoint, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("zfs dataset %q open backing directory: %w", dataset, err)
	}
	defer func() { _ = dir.Close() }() //nolint:errcheck // read-only directory
	err = validateBackingDirectory(dir, dataset)
	if err != nil {
		return err
	}
	mounted, err = d.mountpointOccupied(dataset, mountpoint)
	if err != nil || mounted {
		return err
	}
	return protectBackingDirectory(dir, dataset)
}

func (d *DatasetBackend) mountpointOccupied(dataset, mountpoint string) (bool, error) {
	fsType, source, err := d.mountAt(mountpoint)
	if err != nil {
		return false, err
	}
	if fsType == "" {
		return false, nil
	}
	if fsType == datasetFSType && source == dataset {
		return true, nil
	}
	return false, fmt.Errorf("zfs dataset %q mountpoint %q is occupied by %s %q", dataset, mountpoint, fsType, source)
}

func (d *DatasetBackend) prepareBackingPath(dataset, mountpoint string) error {
	root, err := os.Stat(d.mountRoot)
	if err != nil {
		return fmt.Errorf("zfs dataset persistent mount root %q: %w", d.mountRoot, err)
	}
	if !root.IsDir() {
		return fmt.Errorf("zfs dataset persistent mount root %q is not a directory", d.mountRoot)
	}
	//nolint:gosec // G301: NFS LOOKUP requires client execute access; these parents contain only protected stubs.
	err = os.MkdirAll(filepath.Dir(mountpoint), 0o755)
	if err != nil {
		return fmt.Errorf("zfs dataset %q create traversal directories: %w", dataset, err)
	}
	err = os.Mkdir(mountpoint, 0)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("zfs dataset %q create backing directory: %w", dataset, err)
	}
	return nil
}

func validateBackingDirectory(dir *os.File, dataset string) error {
	info, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("zfs dataset %q stat backing directory: %w", dataset, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("zfs dataset %q backing directory must be root-owned", dataset)
	}
	_, err = dir.ReadDir(1)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("zfs dataset %q inspect backing directory: %w", dataset, err)
	}
	return fmt.Errorf("zfs dataset %q backing directory must be empty", dataset)
}

func protectBackingDirectory(dir *os.File, dataset string) error {
	err := dir.Chmod(0)
	if err != nil {
		return fmt.Errorf("zfs dataset %q protect backing directory: %w", dataset, err)
	}
	info, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("zfs dataset %q backing permissions readback: %w", dataset, err)
	}
	if info.Mode().Perm() != 0 {
		return fmt.Errorf("zfs dataset %q backing directory permissions readback is %04o, want 0000",
			dataset, info.Mode().Perm())
	}
	return nil
}

func validateDatasetMountPath(mountpoint string) error {
	current := filepath.Clean(mountpoint)
	for {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("zfs dataset: inspect mount path %q: %w", current, err)
		}
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return fmt.Errorf("zfs dataset: mount path component %q is not a real directory", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return nil
}

// Create provisions or recovers an owned filesystem without altering existing contents.
func (d *DatasetBackend) Create(
	ctx context.Context, volumeID string, capacityBytes int64, params *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, createErr error) {
	if capacityBytes <= 0 {
		return "", 0, fmt.Errorf("zfs dataset: capacity must be positive, got %d", capacityBytes)
	}
	err := d.validateParams(volumeID, params)
	if err != nil {
		return "", 0, err
	}
	dataset, leaf, err := d.resolve(volumeID)
	if err != nil {
		return "", 0, err
	}
	mountpoint := d.mountPath(leaf)
	if mountpoint == "" {
		return "", 0, fmt.Errorf("zfs dataset: unsafe mount path for volume %q", volumeID)
	}
	err = validateDatasetMountPath(mountpoint)
	if err != nil {
		return "", 0, err
	}
	state, err := d.readState(ctx, dataset)
	if err == nil {
		return d.adoptDataset(ctx, dataset, mountpoint, capacityBytes, state)
	}
	if !isNotExist(err) {
		return "", 0, err
	}
	err = d.prepareMountpoint(dataset, mountpoint)
	if err != nil {
		return "", 0, err
	}
	return d.createDataset(ctx, volumeID, dataset, mountpoint, capacityBytes, params)
}

func datasetCreateArgs(dataset, mountpoint string, capacityBytes int64, params *agentv1.BackendParams) []string {
	properties := params.GetZfs().GetProperties()
	args := make([]string, 0, 8+2*len(properties))
	args = append(args, "create", "-o", "mountpoint="+mountpoint, "-o", "sharenfs=off",
		"-o", "refquota="+strconv.FormatInt(capacityBytes, 10))
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "-o", key+"="+properties[key])
	}
	return append(args, dataset)
}

func (d *DatasetBackend) createDataset(
	ctx context.Context, volumeID, dataset, mountpoint string, capacityBytes int64, params *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, createErr error) {
	out, runErr := d.exec.run(ctx, datasetFSType, datasetCreateArgs(dataset, mountpoint, capacityBytes, params)...)
	if runErr != nil {
		// A concurrent creator may have won the race. Re-read and adopt only
		// when all identity and safety properties match exactly.
		state, readErr := d.readState(ctx, dataset)
		if readErr == nil {
			return d.adoptDataset(ctx, dataset, mountpoint, capacityBytes, state)
		}
		if isOutOfSpaceOutput(out) {
			return "", 0, &backend.InsufficientCapacityError{
				VolumeID: volumeID, RequestedBytes: capacityBytes, Err: fmt.Errorf("zfs create %q: %w", dataset, runErr),
			}
		}
		return "", 0, fmt.Errorf("zfs create dataset %q: %w\n%s", dataset, runErr, strings.TrimSpace(string(out)))
	}
	state, err := d.readState(ctx, dataset)
	if err != nil {
		return "", 0, fmt.Errorf("zfs create dataset %q readback: %w", dataset, err)
	}
	return d.adoptDataset(ctx, dataset, mountpoint, capacityBytes, state)
}

func (d *DatasetBackend) adoptDataset(
	ctx context.Context, dataset, mountpoint string, capacityBytes int64, state datasetState,
) (devicePath string, allocatedBytes int64, createErr error) {
	err := validateDatasetState(dataset, mountpoint, state, capacityBytes)
	if err != nil {
		return "", 0, err
	}
	err = d.ensureMounted(ctx, dataset, mountpoint, state)
	if err != nil {
		return "", 0, err
	}
	return mountpoint, state.refquota, nil
}

// Delete destroys only an owned quota-managed filesystem dataset.
func (d *DatasetBackend) Delete(ctx context.Context, volumeID string) error {
	dataset, leaf, err := d.resolve(volumeID)
	if err != nil {
		return err
	}
	state, err := d.readState(ctx, dataset)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	err = validateDatasetState(dataset, d.mountPath(leaf), state, 0)
	if err != nil {
		return err
	}
	out, runErr := d.exec.run(ctx, datasetFSType, "destroy", dataset)
	if runErr != nil {
		if isNotExistOutput(out) {
			return nil
		}
		return fmt.Errorf("zfs destroy dataset %q: %w\n%s", dataset, runErr, strings.TrimSpace(string(out)))
	}
	return nil
}

// Expand grows the managed refquota and verifies the applied capacity.
func (d *DatasetBackend) Expand(ctx context.Context, volumeID string, requestedBytes int64) (int64, error) {
	if requestedBytes <= 0 {
		return 0, fmt.Errorf("zfs dataset: requested capacity must be positive, got %d", requestedBytes)
	}
	dataset, leaf, err := d.resolve(volumeID)
	if err != nil {
		return 0, err
	}
	state, err := d.readState(ctx, dataset)
	if err != nil {
		if isNotExist(err) {
			return 0, fmt.Errorf("zfs dataset %q does not exist", dataset)
		}
		return 0, err
	}
	err = validateDatasetState(dataset, d.mountPath(leaf), state, 0)
	if err != nil {
		return 0, err
	}
	if state.refquota >= requestedBytes {
		return state.refquota, nil
	}
	out, runErr := d.exec.run(ctx, datasetFSType, "set", "refquota="+strconv.FormatInt(requestedBytes, 10), dataset)
	if runErr != nil {
		if isOutOfSpaceOutput(out) {
			return 0, &backend.InsufficientCapacityError{
				VolumeID: volumeID, RequestedBytes: requestedBytes,
				Err: fmt.Errorf("zfs set refquota %q: %w", dataset, runErr),
			}
		}
		return 0, fmt.Errorf("zfs set refquota %q: %w\n%s", dataset, runErr, strings.TrimSpace(string(out)))
	}
	state, err = d.readState(ctx, dataset)
	if err != nil {
		return 0, fmt.Errorf("zfs expand dataset %q readback: %w", dataset, err)
	}
	err = validateDatasetState(dataset, d.mountPath(leaf), state, 0)
	if err != nil {
		return 0, err
	}
	if state.refquota < requestedBytes {
		return 0, fmt.Errorf("zfs dataset %q refquota readback %d below requested %d",
			dataset, state.refquota, requestedBytes)
	}
	return state.refquota, nil
}

// Capacity reports capacity at the configured pool or parent-dataset boundary.
func (d *DatasetBackend) Capacity(ctx context.Context) (totalBytes, availableBytes int64, capacityErr error) {
	root := d.provisioningRoot()
	if !validDatasetPath(d.pool, false) || !validDatasetPath(d.parentDataset, true) {
		return 0, 0, fmt.Errorf("zfs dataset: invalid configured pool/layout")
	}
	ancestors := datasetAncestors(root)
	args := append([]string{"get", "-Hp", "-o", "name,property,value",
		"available,used,usedbyrefreservation,refquota,quota,reservation"}, ancestors...)
	out, err := d.exec.run(ctx, datasetFSType, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("zfs get capacity properties %s: %w\n%s", root, err, strings.TrimSpace(string(out)))
	}
	props, err := parseDatasetProps(out)
	if err != nil {
		return 0, 0, err
	}
	rootProps, ok := props[root]
	if !ok {
		return 0, 0, fmt.Errorf("zfs: no properties returned for dataset %q", root)
	}
	used, err := rootProps.uint64("used", root)
	if err != nil {
		return 0, 0, err
	}
	available, err := dirSpaceAvailable(root, props)
	if err != nil {
		return 0, 0, err
	}
	if used > math.MaxInt64-available {
		return 0, 0, fmt.Errorf("zfs: capacity of dataset %q overflows int64", root)
	}
	return used + available, available, nil
}

// ListVolumes reports only owned direct-child filesystem datasets with managed quotas.
func (d *DatasetBackend) ListVolumes(ctx context.Context) ([]*agentv1.VolumeInfo, error) {
	root := d.provisioningRoot()
	if !validDatasetPath(d.pool, false) || !validDatasetPath(d.parentDataset, true) {
		return nil, fmt.Errorf("zfs dataset: invalid configured pool/layout")
	}
	out, err := d.exec.run(ctx, datasetFSType, "list", "-Hp", "-t", datasetFilesystem, "-o", "name,refquota", "-r", root)
	if err != nil {
		if isNotExistOutput(out) {
			return []*agentv1.VolumeInfo{}, nil
		}
		return nil, fmt.Errorf("zfs list filesystems %q: %w\n%s", root, err, strings.TrimSpace(string(out)))
	}
	volumes := make([]*agentv1.VolumeInfo, 0)
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		dataset, leaf, parseErr := datasetListEntry(strings.TrimSpace(line), root)
		if parseErr != nil {
			return nil, parseErr
		}
		if leaf == "" {
			continue
		}
		volumes, err = d.appendListedVolume(ctx, volumes, dataset, leaf)
		if err != nil {
			return nil, err
		}
	}
	return volumes, nil
}

func datasetListEntry(line, root string) (dataset, leaf string, err error) {
	if line == "" {
		return "", "", nil
	}
	dataset, quotaRaw, ok := strings.Cut(line, "\t")
	if !ok {
		return "", "", fmt.Errorf("zfs: unexpected filesystem list output %q", line)
	}
	if dataset == root {
		return "", "", nil
	}
	prefix := root + "/"
	if !strings.HasPrefix(dataset, prefix) {
		return "", "", fmt.Errorf("zfs: list returned dataset outside configured root: %q", dataset)
	}
	leaf = strings.TrimPrefix(dataset, prefix)
	if !validVolumeLeaf(leaf) {
		// Recursive child datasets are not volumes owned by this backend.
		return "", "", nil
	}
	quota, err := parseDatasetQuota(strings.TrimSpace(quotaRaw), dataset)
	if err != nil {
		return "", "", err
	}
	if quota == 0 {
		// Plain parent/foreign datasets are not quota-managed volumes.
		return "", "", nil
	}
	return dataset, leaf, nil
}

func (d *DatasetBackend) appendListedVolume(
	ctx context.Context, volumes []*agentv1.VolumeInfo, dataset, leaf string,
) ([]*agentv1.VolumeInfo, error) {
	mountpoint := d.mountPath(leaf)
	if mountpoint == "" {
		return nil, fmt.Errorf("zfs: unsafe mount path for listed dataset %q", dataset)
	}
	state, err := d.readState(ctx, dataset)
	if err != nil {
		return nil, err
	}
	if state.mountpoint != mountpoint || state.sharenfs != "off" {
		return volumes, nil
	}
	err = validateDatasetState(dataset, mountpoint, state, 0)
	if err != nil {
		return nil, err
	}
	return append(volumes, &agentv1.VolumeInfo{
		VolumeId: d.pool + "/" + leaf, CapacityBytes: state.refquota, DevicePath: mountpoint,
	}), nil
}

// ProvisionedBytes sums the managed refquotas of owned filesystem datasets.
func (d *DatasetBackend) ProvisionedBytes(ctx context.Context) (
	allocatedBytes int64,
	complete bool,
	provisionedErr error,
) {
	volumes, err := d.ListVolumes(ctx)
	if err != nil {
		return 0, false, err
	}
	var total int64
	for _, volume := range volumes {
		if total > math.MaxInt64-volume.GetCapacityBytes() {
			return 0, false, fmt.Errorf("zfs dataset: provisioned capacity overflows int64")
		}
		total += volume.GetCapacityBytes()
	}
	return total, true, nil
}

// DevicePath returns the stable mounted filesystem path for volumeID. Invalid
// IDs return an empty path rather than a traversal-capable path.
func (d *DatasetBackend) DevicePath(volumeID string) string {
	_, leaf, err := d.resolve(volumeID)
	if err != nil {
		return ""
	}
	return d.mountPath(leaf)
}
