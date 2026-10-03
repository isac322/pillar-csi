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
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/backend"
)

const zfsAdoptionProperties = "type,guid,mountpoint,mounted,canmount,readonly,refquota,quota,used," +
	"usedbydataset,usedbysnapshots,usedbychildren,usedbyrefreservation"

type zfsImportState struct {
	typeName                                                                         string
	guid, mountpoint, mounted, canmount, readonly                                    string
	refquota, quota, used, usedByDataset, usedBySnapshots, usedByChildren, usedByRef int64
}

type pinnedDataset struct {
	backend  *DatasetBackend
	adoption *agentv1.FilesystemAdoption
	capacity int64
	dataset  string
	file     *os.File
}

var _ backend.PinnedFilesystem = (*pinnedDataset)(nil)

func (p *pinnedDataset) Adoption() *agentv1.FilesystemAdoption { return p.adoption }
func (p *pinnedDataset) CapacityBytes() int64                  { return p.capacity }
func (p *pinnedDataset) MountSource() string {
	if p.file != nil {
		return "/proc/self/fd/" + strconv.Itoa(int(p.file.Fd()))
	}
	return p.dataset
}
func (p *pinnedDataset) VerifyMount(ctx context.Context, targetPath string) error {
	if p.backend == nil || p.dataset == "" || p.adoption == nil {
		return errors.New("zfs filesystem pin is invalid")
	}
	owned, err := p.backend.ownedProxyPath(p.adoption)
	if err != nil {
		return err
	}
	if targetPath != owned {
		return fmt.Errorf("zfs filesystem pin target %q is not the owned proxy %q", targetPath, owned)
	}
	err = p.backend.verifyPinnedDatasetMount(p.dataset, targetPath, p.file)
	if err != nil {
		return err
	}
	state, err := p.backend.readImportState(ctx, p.dataset)
	if err != nil {
		return err
	}
	if state.guid != p.adoption.GetResourceId() {
		return fmt.Errorf("zfs filesystem pin target %q changed GUID", targetPath)
	}
	_, err = p.backend.importMountPath(p.dataset, state, p.adoption)
	if err != nil {
		return err
	}
	_, err = p.backend.validateImportState(ctx, p.dataset, state, p.capacity)
	if err != nil {
		return err
	}
	return p.backend.verifyPinnedDatasetMount(p.dataset, targetPath, p.file)
}
func (p *pinnedDataset) Close() error {
	if p.file == nil {
		return nil
	}
	err := p.file.Close()
	p.file = nil
	if err != nil {
		return fmt.Errorf("close zfs filesystem pin: %w", err)
	}
	return nil
}

// InspectImport verifies an existing dataset's native identity, mounts, and exact
// effective quota without changing its properties or filesystem.
func (d *DatasetBackend) InspectImport(
	ctx context.Context, source string, requiredBytes int64, expectedLayout backend.Layout,
) (*backend.ImportInspection, error) {
	if requiredBytes <= 0 {
		return nil, fmt.Errorf("zfs filesystem import: exact capacity must be positive")
	}
	err := d.validateImportLayout(expectedLayout)
	if err != nil {
		return nil, err
	}
	dataset, err := d.validateImportSource(source)
	if err != nil {
		return nil, err
	}
	state, err := d.readImportState(ctx, dataset)
	if err != nil {
		return nil, d.refuseImport(dataset, reasonMissing, err.Error())
	}
	mountPath, err := d.importMountPath(dataset, state, nil)
	if err != nil {
		return nil, err
	}
	capacity, err := d.validateImportState(ctx, dataset, state, requiredBytes)
	if err != nil {
		return nil, err
	}
	adoption := adoptionFor(dataset, state)
	owned, ownedErr := d.ownedProxyPath(adoption)
	if mountPath != d.agentPath(state.mountpoint) || (ownedErr == nil && mountPath == owned) {
		adoption.HostPath = ""
	}
	return &backend.ImportInspection{Filesystem: adoption, CapacityBytes: capacity}, nil
}

// ImportFilesystem pins the existing dataset and rechecks its recorded contract.
func (d *DatasetBackend) ImportFilesystem(
	ctx context.Context, volumeID string, requiredBytes int64,
	expected *agentv1.FilesystemAdoption, expectedLayout backend.Layout,
) (backend.PinnedFilesystem, error) {
	if requiredBytes <= 0 {
		return nil, fmt.Errorf("zfs filesystem import %q: exact capacity must be positive", volumeID)
	}
	if expected == nil {
		return nil, fmt.Errorf("zfs filesystem import %q: adoption descriptor is required", volumeID)
	}
	err := d.validateImportLayout(expectedLayout)
	if err != nil {
		return nil, err
	}
	dataset, err := d.validateImportSource(expected.GetCanonicalSource())
	if err != nil {
		return nil, err
	}
	state, err := d.readImportState(ctx, dataset)
	if err != nil {
		return nil, d.refuseImport(volumeID, reasonMissing, err.Error())
	}
	if state.guid != expected.GetResourceId() {
		return nil, d.refuseImport(volumeID, reasonLayout, "recorded dataset GUID differs")
	}
	mountPath, err := d.importMountPath(dataset, state, expected)
	if err != nil {
		return nil, err
	}
	capacity, err := d.validateImportState(ctx, dataset, state, requiredBytes)
	if err != nil {
		return nil, err
	}
	adoption := adoptionFor(dataset, state)
	// An owned proxy never turns its current mountpoint into provenance.
	if expected.GetHostPath() == "" {
		adoption.HostPath = ""
	}
	if !sameFilesystemAdoption(expected, adoption) {
		return nil, d.refuseImport(volumeID, reasonLayout,
			"recorded filesystem identity, source path, or mount state differs")
	}
	pin := &pinnedDataset{backend: d, adoption: adoption, capacity: capacity, dataset: dataset}
	if mountPath == "" {
		return pin, nil
	}
	err = d.openDatasetPin(pin, volumeID, mountPath)
	if err == nil {
		err = d.recheckDatasetPin(ctx, pin, volumeID, mountPath)
	}
	if err != nil {
		return nil, fmt.Errorf("zfs filesystem import %q: %w", volumeID, errors.Join(err, pin.Close()))
	}
	return pin, nil
}

func (d *DatasetBackend) openDatasetPin(pin *pinnedDataset, volumeID, mountPath string) error {
	fd, err := syscall.Open(mountPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return d.refuseImport(volumeID, reasonInUse,
			fmt.Sprintf("open mounted filesystem %q: %v", mountPath, err))
	}
	pin.file = os.NewFile(uintptr(fd), mountPath)
	err = d.verifyPinnedDatasetMount(pin.dataset, mountPath, pin.file)
	if err != nil {
		return d.refuseImport(volumeID, reasonInUse, err.Error())
	}
	return nil
}

func (d *DatasetBackend) recheckDatasetPin(ctx context.Context, pin *pinnedDataset, volumeID, mountPath string) error {
	// Re-read native state after opening; path resolution must not create an
	// identity lease over a replaced dataset or a changed quota.
	current, err := d.readImportState(ctx, pin.dataset)
	if err != nil {
		return err
	}
	if current.guid != pin.adoption.GetResourceId() {
		return d.refuseImport(volumeID, reasonLayout, "dataset GUID changed while opening filesystem")
	}
	_, err = d.importMountPath(pin.dataset, current, pin.adoption)
	if err != nil {
		return err
	}
	_, err = d.validateImportState(ctx, pin.dataset, current, pin.capacity)
	if err != nil {
		return err
	}
	return d.verifyPinnedDatasetMount(pin.dataset, mountPath, pin.file)
}

func (d *DatasetBackend) validateImportLayout(layout backend.Layout) error {
	configured := d.Layout()
	if normalizeDataset(layout.ParentDataset) != configured.ParentDataset || layout.HostRoot != "" {
		return &backend.LayoutMismatchError{
			Setting:    "ZFS filesystem layout",
			Requested:  fmt.Sprintf("parent=%q hostRoot=%q", layout.ParentDataset, layout.HostRoot),
			Configured: fmt.Sprintf("parent=%q hostRoot=%q", configured.ParentDataset, configured.HostRoot),
		}
	}
	return nil
}
func (d *DatasetBackend) validateImportSource(source string) (string, error) {
	if source == "" || !validDatasetPath(source, false) {
		return "", d.refuseImport(source, reasonLayout, "source is not a safe ZFS dataset fullname")
	}
	root := d.provisioningRoot()
	prefix := root + "/"
	if !strings.HasPrefix(source, prefix) {
		return "", d.refuseImport(source, reasonLayout, fmt.Sprintf("source is outside configured pool/parent %q", root))
	}
	if !validVolumeLeaf(strings.TrimPrefix(source, prefix)) {
		return "", d.refuseImport(source, reasonLayout, "source must be the direct child volume of the configured parent")
	}
	return source, nil
}

func (d *DatasetBackend) readImportState(ctx context.Context, dataset string) (zfsImportState, error) {
	out, err := d.exec.run(ctx, datasetFSType, zfsGetOperation, datasetMachineOutput,
		"-o", "property,value", zfsAdoptionProperties, dataset)
	if err != nil {
		if isNotExistOutput(out) {
			return zfsImportState{}, &notExistError{dataset: dataset}
		}
		return zfsImportState{}, fmt.Errorf("zfs get adoption properties %q: %w\n%s",
			dataset, err, strings.TrimSpace(string(out)))
	}
	var state zfsImportState
	seen := make(map[string]bool)
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		property, value, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			return zfsImportState{}, fmt.Errorf("zfs: unexpected adoption property output for %q: %q", dataset, line)
		}
		seen[property] = true
		err = state.setImportProperty(property, strings.TrimSpace(value), dataset)
		if err != nil {
			return zfsImportState{}, err
		}
	}
	for property := range strings.SplitSeq(zfsAdoptionProperties, ",") {
		if !seen[property] {
			return zfsImportState{}, fmt.Errorf("zfs: missing adoption property %q for dataset %q", property, dataset)
		}
	}
	return state, nil
}

func (s *zfsImportState) setImportProperty(property, value, dataset string) error {
	switch property {
	case datasetTypeProperty:
		s.typeName = value
	case datasetGUIDProperty:
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n == 0 {
			return fmt.Errorf("zfs: invalid GUID %q for dataset %q", value, dataset)
		}
		s.guid = strconv.FormatUint(n, 10)
	case datasetMountpointProperty:
		s.mountpoint = value
	case datasetMountedProperty:
		s.mounted = value
	case "canmount":
		s.canmount = value
	case "readonly":
		s.readonly = value
	default:
		return s.setImportQuotaProperty(property, value, dataset)
	}
	return nil
}

func (s *zfsImportState) setImportQuotaProperty(property, value, dataset string) error {
	var target *int64
	switch property {
	case datasetRefquotaProperty:
		target = &s.refquota
	case "quota":
		target = &s.quota
	case "used":
		target = &s.used
	case "usedbydataset":
		target = &s.usedByDataset
	case "usedbysnapshots":
		target = &s.usedBySnapshots
	case "usedbychildren":
		target = &s.usedByChildren
	case "usedbyrefreservation":
		target = &s.usedByRef
	default:
		return nil
	}
	quota, err := parseImportQuota(value, property, dataset)
	if err != nil {
		return err
	}
	*target = quota
	return nil
}
func parseImportQuota(value, property, dataset string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "none" || value == "-" {
		return 0, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n > math.MaxInt64 {
		return 0, fmt.Errorf("zfs: invalid or overflowing %s %q for dataset %q",
			property, value, dataset)
	}
	return int64(n), nil
}

func (d *DatasetBackend) validateImportState(
	ctx context.Context, dataset string, state zfsImportState, required int64,
) (int64, error) {
	if state.typeName != datasetFilesystem {
		return 0, d.refuseImport(dataset, reasonWrongType, fmt.Sprintf("dataset type is %q", state.typeName))
	}
	if state.canmount != "on" && state.canmount != "noauto" {
		return 0, d.refuseImport(dataset, reasonWrongType, fmt.Sprintf("canmount=%q is not adoptable", state.canmount))
	}
	if state.readonly != "on" && state.readonly != datasetPropertyOff {
		return 0, d.refuseImport(dataset, reasonWrongType,
			fmt.Sprintf("readonly=%q is not a ZFS compatibility value", state.readonly))
	}
	if state.mounted != datasetMounted && state.mounted != "no" {
		return 0, d.refuseImport(dataset, reasonWrongType, fmt.Sprintf("mounted=%q is not supported", state.mounted))
	}
	if state.refquota <= 0 && state.quota <= 0 {
		return 0, d.refuseImport(dataset, reasonTooSmall, "dataset has no finite effective quota")
	}
	capacity, err := d.effectiveQuota(ctx, dataset, state)
	if err != nil {
		return 0, d.refuseImport(dataset, reasonTooSmall, err.Error())
	}
	if capacity != required {
		return 0, &backend.ConflictError{VolumeID: dataset, ExistingBytes: capacity, RequestedBytes: required}
	}
	err = d.rejectSubordinateMountEscapes(ctx, dataset)
	if err != nil {
		return 0, err
	}
	return capacity, nil
}
func (d *DatasetBackend) effectiveQuota(ctx context.Context, dataset string, state zfsImportState) (int64, error) {
	capacity, err := ownDatasetQuota(state)
	if err != nil {
		return 0, err
	}
	ancestors := datasetAncestors(dataset)
	args := append(
		[]string{zfsGetOperation, datasetMachineOutput, "-o", datasetNamePropertyValueOutput, "quota,used"},
		ancestors...,
	)
	out, err := d.exec.run(ctx, datasetFSType, args...)
	if err != nil {
		return 0, fmt.Errorf("zfs get ancestor quotas %q: %w\n%s", dataset, err, strings.TrimSpace(string(out)))
	}
	props, err := parseDatasetProps(out)
	if err != nil {
		return 0, err
	}
	for _, ancestor := range ancestors[1:] {
		p, ok := props[ancestor]
		if !ok {
			return 0, fmt.Errorf("zfs: ancestor %q missing quota properties", ancestor)
		}
		err = validateAncestorQuota(ancestor, p, capacity)
		if err != nil {
			return 0, err
		}
	}
	// An enforced upper bound is not a pool-space reservation or a statement
	// that the entire bound is currently free.
	return capacity, nil
}

func ownDatasetQuota(state zfsImportState) (int64, error) {
	var accounted int64
	for _, used := range [...]int64{state.usedByDataset, state.usedBySnapshots, state.usedByChildren, state.usedByRef} {
		if used < 0 || accounted > math.MaxInt64-used {
			return 0, fmt.Errorf("zfs: dataset accounting overflows")
		}
		accounted += used
	}
	if accounted != state.used {
		return 0, fmt.Errorf("zfs: dataset used accounting is inconsistent")
	}
	other := accounted - state.usedByDataset
	if state.refquota <= 0 {
		if state.quota <= 0 {
			return 0, fmt.Errorf("zfs: dataset has no finite own quota")
		}
		if other != 0 {
			return 0, fmt.Errorf("zfs: own quota includes snapshots, descendants or refreservation")
		}
		return state.quota, nil
	}
	if state.quota <= 0 {
		return state.refquota, nil
	}
	// Refquota bounds live data. Aggregate quota also charges snapshots,
	// descendants and refreservation; never advertise a mixed binding scope.
	if state.quota < state.refquota {
		if other != 0 {
			return 0, fmt.Errorf("zfs: binding own quota has a mixed accounting scope")
		}
		return state.quota, nil
	}
	if other > state.quota-state.refquota {
		return 0, fmt.Errorf("zfs: aggregate quota constrains the own refquota")
	}
	return state.refquota, nil
}

func validateAncestorQuota(ancestor string, properties datasetProps, capacity int64) error {
	quota, err := properties.uint64("quota", ancestor)
	if err != nil {
		return err
	}
	used, err := properties.uint64("used", ancestor)
	if err != nil {
		return err
	}
	if quota > 0 && quota < capacity {
		return fmt.Errorf("zfs: ancestor %q quota is a smaller shared scope", ancestor)
	}
	if quota > 0 && used > quota {
		return fmt.Errorf("zfs: ancestor %q used exceeds quota", ancestor)
	}
	return nil
}
func (d *DatasetBackend) rejectSubordinateMountEscapes(ctx context.Context, dataset string) error {
	out, err := d.exec.run(ctx, datasetFSType, "list", datasetMachineOutput,
		"-r", "-t", datasetFilesystem, "-o", "name,mountpoint,mounted", dataset)
	if err != nil {
		return fmt.Errorf("zfs list subordinate mounts %q: %w\n%s", dataset, err, strings.TrimSpace(string(out)))
	}
	// A mounted child is a second native quota/identity scope, whether its
	// path escapes the root or hides a directory beneath it.
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(f) != 3 {
			return d.refuseImport(dataset, reasonInUse, "malformed subordinate mount output")
		}
		if f[0] != dataset && f[2] == datasetMounted {
			return d.refuseImport(dataset, reasonInUse, fmt.Sprintf("subordinate dataset %q is mounted", f[0]))
		}
	}
	return nil
}
func nativeFilesystemAdoption(dataset, guid string) *agentv1.FilesystemAdoption {
	return &agentv1.FilesystemAdoption{
		Kind: datasetAdoptionKind, CanonicalSource: dataset, ResourceId: guid, FilesystemType: datasetFSType,
	}
}

func adoptionFor(dataset string, state zfsImportState) *agentv1.FilesystemAdoption {
	adoption := nativeFilesystemAdoption(dataset, state.guid)
	if state.mounted == datasetMounted {
		adoption.HostPath = state.mountpoint
	}
	return adoption
}

func sameFilesystemAdoption(want, got *agentv1.FilesystemAdoption) bool {
	return want.GetKind() == got.GetKind() &&
		want.GetCanonicalSource() == got.GetCanonicalSource() &&
		want.GetResourceId() == got.GetResourceId() &&
		want.GetHostPath() == got.GetHostPath() &&
		want.GetFilesystemType() == got.GetFilesystemType() &&
		want.GetFilesystemId() == "" && want.GetInode() == 0 && want.GetProjectId() == 0
}

func (*DatasetBackend) refuseImport(volumeID, reason, detail string) error {
	return &backend.ImportRefusedError{VolumeID: volumeID, Reason: reason, Detail: detail}
}
func (d *DatasetBackend) agentPath(hostPath string) string {
	if d.hostRootPrefix == "" || hostPath == "" {
		return hostPath
	}
	r, h := filepath.Clean(d.hostRootPrefix), filepath.Clean(hostPath)
	if !filepath.IsAbs(r) || !filepath.IsAbs(h) {
		return ""
	}
	return filepath.Join(r, strings.TrimPrefix(h, string(filepath.Separator)))
}

type adoptionMount struct {
	id, device, root, path, fsType, source string
}

func (d *DatasetBackend) adoptionMounts() (mounts []adoptionMount, resultErr error) {
	file, err := os.Open(d.mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("zfs adoption read mountinfo: %w", err)
	}
	defer func() {
		closeErr := file.Close()
		if closeErr != nil {
			resultErr = fmt.Errorf("read and close zfs adoption mountinfo: %w", errors.Join(resultErr, closeErr))
		}
	}()
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
		if sep < 6 || len(fields) < sep+4 {
			return nil, errors.New("zfs adoption malformed mountinfo")
		}
		mounts = append(mounts, adoptionMount{
			id: fields[0], device: fields[2], root: unescape.Replace(fields[3]),
			path: unescape.Replace(fields[4]), fsType: fields[sep+1], source: unescape.Replace(fields[sep+2]),
		})
	}
	err = scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("scan zfs adoption mountinfo: %w", err)
	}
	return mounts, nil
}

func (d *DatasetBackend) ownedProxyPath(adoption *agentv1.FilesystemAdoption) (string, error) {
	root := d.filesystemProxyRoot
	fence := backend.FilesystemFenceID(adoption)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || fence == "" {
		return "", errors.New("zfs adoption requires a dedicated absolute agent-owned filesystem proxy root")
	}
	return filepath.Join(root, fence), nil
}

func (d *DatasetBackend) importMountPath(
	dataset string, state zfsImportState, expected *agentv1.FilesystemAdoption,
) (string, error) {
	mounts, err := d.adoptionMounts()
	if err != nil {
		return "", d.refuseImport(dataset, reasonInUse, err.Error())
	}
	if state.mounted == "no" {
		return "", d.validateUnmountedDataset(dataset, state.mountpoint, mounts)
	}
	path, err := d.resolveMountedImportPath(dataset, state, expected, mounts)
	if err != nil {
		return "", err
	}
	err = d.verifyPinnedDatasetMount(dataset, path, nil)
	if err != nil {
		return "", d.refuseImport(dataset, reasonInUse, err.Error())
	}
	return path, nil
}

func (d *DatasetBackend) resolveMountedImportPath(
	dataset string, state zfsImportState, expected *agentv1.FilesystemAdoption,
	mounts []adoptionMount,
) (string, error) {
	adoption := expected
	if adoption == nil {
		adoption = nativeFilesystemAdoption(dataset, state.guid)
	}
	owned, ownedErr := d.ownedProxyPath(adoption)
	nativePath := d.agentPath(state.mountpoint)
	ownedOnly := expected != nil && expected.GetHostPath() == ""
	if expected == nil && ownedErr == nil && nativePath == owned {
		ownedOnly = true
	}
	if !ownedOnly && validNativeMountpoint(state.mountpoint) {
		path := findNativeDatasetMount(dataset, nativePath, mounts)
		if path != "" {
			return path, nil
		}
	}
	if expected != nil && !ownedOnly {
		return "", d.refuseImport(dataset, reasonInUse, "original native host mount is absent")
	}
	return d.ownedDatasetMount(dataset, owned, ownedErr, mounts)
}

func validNativeMountpoint(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func findNativeDatasetMount(dataset, path string, mounts []adoptionMount) string {
	for _, mount := range mounts {
		if mount.path == path && mount.fsType == datasetFSType && mount.source == dataset && mount.root == "/" {
			return path
		}
	}
	return ""
}

func (d *DatasetBackend) validateUnmountedDataset(dataset, mountpoint string, mounts []adoptionMount) error {
	if mountpoint != "legacy" && !validNativeMountpoint(mountpoint) {
		return d.refuseImport(dataset, reasonLayout, "unmounted dataset has invalid mountpoint")
	}
	for _, mount := range mounts {
		if mount.fsType == datasetFSType && mount.source == dataset {
			return d.refuseImport(dataset, reasonInUse, "unmounted dataset has a namespace mount")
		}
	}
	return nil
}

func (d *DatasetBackend) ownedDatasetMount(
	dataset, owned string, ownedErr error, mounts []adoptionMount,
) (string, error) {
	if ownedErr != nil {
		return "", d.refuseImport(dataset, reasonInUse, ownedErr.Error())
	}
	path := ""
	for _, mount := range mounts {
		if mount.fsType != datasetFSType || mount.source != dataset {
			continue
		}
		if mount.path != owned || mount.root != "/" {
			return "", d.refuseImport(dataset, reasonInUse, "unmounted adoption has an unexpected native mount")
		}
		path = owned
	}
	if path == "" {
		return "", d.refuseImport(dataset, reasonInUse, "mounted adoption has no owned proxy mount")
	}
	return path, nil
}

func datasetMountRoot(dataset, path string, mounts []adoptionMount) (adoptionMount, error) {
	var match adoptionMount
	for _, mount := range mounts {
		if mount.path == path {
			if match.id != "" {
				return adoptionMount{}, errors.New("zfs adoption mount root has stacked mounts")
			}
			match = mount
		}
		if strings.HasPrefix(mount.path, path+string(filepath.Separator)) {
			return adoptionMount{}, fmt.Errorf("zfs adoption has a subordinate mount at %q", mount.path)
		}
	}
	if match.fsType != datasetFSType || match.source != dataset || match.root != "/" {
		return adoptionMount{}, fmt.Errorf("zfs adoption %q is not the native dataset root mount at %q", dataset, path)
	}
	return match, nil
}

// Stat_t uses unsigned dev_t on Linux and signed dev_t on Darwin. Keep the
// Linux native device width intact while checking the signed conversion.
func datasetDeviceNumber[T ~int32 | ~uint64](device T) (uint64, error) {
	if device < 0 {
		return 0, errors.New("zfs adoption mount root device cannot be represented")
	}
	return uint64(device), nil
}

func verifyDatasetPinIdentity(path, device string, file *os.File) error {
	var target syscall.Stat_t
	err := syscall.Stat(path, &target)
	if err != nil {
		return fmt.Errorf("stat zfs adoption mount root %q: %w", path, err)
	}
	if target.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return errors.New("zfs adoption mount root is not a directory")
	}
	nativeDevice, err := datasetDeviceNumber(target.Dev)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%d:%d", unix.Major(nativeDevice), unix.Minor(nativeDevice)) != device {
		return errors.New("zfs adoption mountinfo device differs from mount root")
	}
	if file == nil {
		return nil
	}
	var pinned syscall.Stat_t
	err = syscall.Fstat(int(file.Fd()), &pinned)
	if err != nil {
		return fmt.Errorf("stat opened zfs adoption pin: %w", err)
	}
	if pinned.Dev != target.Dev || pinned.Ino != target.Ino {
		return errors.New("zfs adoption opened pin differs from mount root")
	}
	return nil
}

func (d *DatasetBackend) verifyPinnedDatasetMount(dataset, path string, file *os.File) error {
	err := validateDatasetMountPath(path)
	if err != nil {
		return err
	}
	mounts, err := d.adoptionMounts()
	if err != nil {
		return err
	}
	match, err := datasetMountRoot(dataset, path, mounts)
	if err != nil {
		return err
	}
	err = verifyDatasetPinIdentity(path, match.device, file)
	if err != nil {
		return err
	}
	current, err := d.adoptionMounts()
	if err != nil {
		return err
	}
	after, err := datasetMountRoot(dataset, path, current)
	if err != nil {
		return err
	}
	if match != after {
		return errors.New("zfs adoption mount changed during pin verification")
	}
	return nil
}

// ExistingFilesystemIdentity returns the native identity of a configured
// legacy dataset without adopting it. Missing datasets return (nil, nil).
func (d *DatasetBackend) ExistingFilesystemIdentity(
	ctx context.Context, volumeID string,
) (*agentv1.FilesystemAdoption, error) {
	dataset, _, err := d.resolve(volumeID)
	if err != nil {
		return nil, err
	}
	identity, err := readNativeDatasetIdentity(ctx, d.exec, dataset)
	if err != nil || !identity.exists {
		return nil, err
	}
	if identity.typeName != datasetFilesystem {
		return nil, fmt.Errorf("zfs dataset %q has type %q, want filesystem", dataset, identity.typeName)
	}
	return nativeFilesystemAdoption(dataset, identity.guid), nil
}
