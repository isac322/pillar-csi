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

// Import adopts an existing ZFS zvol — provisioned by another stack such as
// democratic-csi (zfs-generic-iscsi) or openebs zfs-localpv — into a pillar-csi
// volume lifecycle.  It backs the agent ImportVolume RPC (see
// backend.VolumeImporter).
//
// Import is deliberately fail-closed and strictly read-only: it never creates,
// renames, resizes, or formats anything.  Every refusal is a
// *backend.ImportRefusedError, which the gRPC layer maps to FailedPrecondition:
//
//   - "missing" / "wrong type": the resolved dataset does not exist or is not
//     a volume (zvol);
//   - "too small": the zvol's volsize is below the requested capacity (import
//     never resizes; expanding an imported zvol afterwards goes through the
//     normal Expand path);
//   - "in use": the zvol's block device is still claimed on the storage node —
//     a LIO backstore udev_path, an nvmet namespace device_path, a mount in
//     /proc/self/mounts, device-mapper holders in sysfs, or an exclusive open
//     (O_EXCL) failing with EBUSY.  Adopting a zvol that another export or
//     local consumer still uses would silently double-publish the device.
//   - "layout": the resolved volume name escapes this backend's configured
//     parentDataset (a shallow safety net for a caller that names a zvol
//     outside the PillarStore's boundary).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/isac322/pillar-csi/internal/agent/backend"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// Refusal reason codes recorded on ImportRefusedError so operators can filter
// the refusal class without parsing the detail text.
const (
	reasonMissing   = "missing"
	reasonWrongType = "wrong type"
	reasonTooSmall  = "too small"
	reasonInUse     = "in use"
	reasonLayout    = "layout"
)

// Compile-time check that Backend implements the optional import contract.
var _ backend.VolumeImporter = (*Backend)(nil)

// Import verifies that volumeID names an existing, idle zvol inside this
// backend's layout and returns its device path and size without modifying it.
//
// The expectedDataset argument is the controller's record of the dataset that must be
// adopted (the import annotation value).  When it is set, the resolved
// datasetName must equal it exactly: the request's volumeID carries only the
// pool and the leaf name, so an agent whose parentDataset differs from the
// store's would otherwise adopt a different dataset than the claim names.
func (z *Backend) Import(
	ctx context.Context,
	volumeID string,
	capacityBytes int64,
	expectedDataset string,
) (devicePath string, sizeBytes int64, err error) {
	ds := z.datasetName(volumeID)
	refuse := func(reason, detail string) (string, int64, error) {
		return "", 0, &backend.ImportRefusedError{VolumeID: volumeID, Reason: reason, Detail: detail}
	}

	// The resolved dataset is authoritative: a caller-declared name that
	// differs from it means the two sides disagree on where the volume
	// lives, and adopting anyway could seize a dataset the claim never
	// named.  Refuse before any existence or in-use probe.
	if expectedDataset != "" && ds != expectedDataset {
		return refuse(reasonLayout, fmt.Sprintf(
			"expected dataset %q does not match the resolved dataset %q "+
				"(backend layout %q under pool %q); align the PillarStore "+
				"pool/parentDataset with the agent backend config",
			expectedDataset, ds, z.parentDataset, z.pool))
	}

	// The leaf must be a single dataset component inside this backend's
	// layout.  datasetName() path.Joins it under pool/parentDataset, so a
	// name containing "/" or ".." could resolve to a dataset outside the
	// pool boundary — refuse instead of adopting another pool's zvol.
	volName := strings.TrimPrefix(volumeID, z.pool+"/")
	if volName != filepath.Base(volName) {
		return "", 0, &backend.ImportRefusedError{
			VolumeID: volumeID,
			Reason:   reasonLayout,
			Detail: fmt.Sprintf("volume name %q is not a single dataset component "+
				"inside pool %q layout %q", volName, z.pool, z.parentDataset),
		}
	}
	// The dataset must exist and be a zvol.  A plain filesystem dataset or a
	// snapshot named by the annotation must never be adopted as a block device.
	dtype, volsize, err := z.datasetProps(ctx, ds)
	switch {
	case err == nil:
		// Continue.
	case isNotExist(err):
		return refuse(reasonMissing, fmt.Sprintf("dataset %q does not exist", ds))
	default:
		return "", 0, fmt.Errorf("zfs: import %q: query dataset %q: %w", volumeID, ds, err)
	}
	if dtype != "volume" {
		return refuse(reasonWrongType,
			fmt.Sprintf("dataset %q has type %q, only ZFS volumes (zvols) can be imported", ds, dtype))
	}
	if volsize < capacityBytes {
		return refuse(reasonTooSmall,
			fmt.Sprintf("dataset %q is %d bytes, requested capacity is %d bytes; "+
				"import never resizes", ds, volsize, capacityBytes))
	}

	devPath := z.DevicePath(volumeID)
	err = z.assertDeviceIdle(volumeID, ds, devPath)
	if err != nil {
		return "", 0, err
	}
	return devPath, volsize, nil
}

// datasetProps returns the type and volsize of dataset via a single
// `zfs get -Hp` call.  A missing dataset is reported as *notExistError.
func (z *Backend) datasetProps(ctx context.Context, dataset string) (dtype string, volsize int64, err error) {
	out, runErr := z.exec.run(
		ctx,
		"zfs",
		zfsGetOperation,
		datasetMachineOutput,
		"-o",
		"property,value",
		"type,volsize",
		dataset,
	)
	if runErr != nil {
		if isNotExistOutput(out) {
			return "", 0, &notExistError{dataset: dataset}
		}
		return "", 0, fmt.Errorf("zfs get type,volsize %q: %w\n%s",
			dataset, runErr, strings.TrimSpace(string(out)))
	}
	var volsizeRaw string
	for line := range strings.Lines(string(out)) {
		prop, val, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found {
			continue
		}
		switch prop {
		case datasetTypeProperty:
			dtype = strings.TrimSpace(val)
		case "volsize":
			volsizeRaw = strings.TrimSpace(val)
		}
	}
	if dtype == "" {
		return "", 0, fmt.Errorf("zfs get type,volsize %q: unexpected output %q",
			dataset, strings.TrimSpace(string(out)))
	}
	// Only volumes carry a volsize; other dataset types report "-", which the
	// caller rejects on the type check without parsing.
	if volsizeRaw != "" && volsizeRaw != "-" {
		n, parseErr := strconv.ParseInt(volsizeRaw, 10, 64)
		if parseErr != nil {
			return "", 0, fmt.Errorf("zfs: parsing volsize output %q for dataset %q: %w",
				volsizeRaw, dataset, parseErr)
		}
		volsize = n
	}
	return dtype, volsize, nil
}

// assertDeviceIdle refuses adoption when the zvol's block device is still
// claimed by another consumer on the storage node.  Checks run cheap-first:
// exported-device scans (LIO backstores, nvmet namespaces), then kernel usage
// (mounts, sysfs holders), then an exclusive-open probe.
func (z *Backend) assertDeviceIdle(volumeID, dataset, devPath string) error {
	devName, err := zvolDeviceName(volumeID, devPath)
	if err != nil {
		return err
	}
	// Candidate names for this device as recorded by kernel/export layers:
	// the /dev/zvol path, its resolved /dev/zdN form, and the bare kernel
	// device name — LIO writes the /dev/zvol path into udev_path while nvmet
	// and mount tables may record either form.
	candidates := deviceCandidates(dataset, devPath, devName)
	err = z.checkLIOBackstores(volumeID, candidates)
	if err != nil {
		return err
	}
	err = z.checkNVMeNamespaces(volumeID, candidates)
	if err != nil {
		return err
	}
	err = z.checkMounts(volumeID, candidates)
	if err != nil {
		return err
	}
	err = z.checkHolders(volumeID, devName)
	if err != nil {
		return err
	}
	return z.checkExclusiveClaim(volumeID, devPath)
}

// zvolDeviceName resolves the zvol's kernel device name (zd<N>) by following
// the /dev/zvol symlink.  The zvol was proven to exist via zfs get, so a
// missing block device means udev has not settled yet; that is a transient
// refusal (FailedPrecondition), not an Internal error.
func zvolDeviceName(volumeID, devPath string) (string, error) {
	target, err := os.Readlink(devPath)
	if err != nil {
		return "", &backend.ImportRefusedError{
			VolumeID: volumeID,
			Reason:   reasonMissing,
			Detail:   fmt.Sprintf("block device %q is not present on the storage node: %v", devPath, err),
		}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(devPath), target)
	}
	return filepath.Base(filepath.Clean(target)), nil
}

// sameDevice reports whether recorded names the zvol device.  The recorded
// value may be an absolute /dev/zvol path, a resolved /dev/zdN path, or a
// bare kernel name.
func sameDevice(recorded string, candidates map[string]struct{}) bool {
	if _, ok := candidates[filepath.Clean(recorded)]; ok {
		return true
	}
	base := filepath.Base(recorded)
	_, ok := candidates["zd:"+base]
	return ok
}

// deviceCandidates builds the lookup set for this zvol: the effective
// devZvolBase path, the canonical /dev/zvol path (LIO's udev_path records the
// canonical spelling even when the backend runs against a fake devZvolBase in
// tests), the resolved /dev/zdN path, and a "zd:<name>" entry so kernel
// device names match any path spelling.
func deviceCandidates(dataset, devPath, devName string) map[string]struct{} {
	return map[string]struct{}{
		filepath.Clean(devPath): {},
		"/dev/zvol/" + dataset:  {},
		"/dev/" + devName:       {},
		"zd:" + devName:         {},
	}
}

// checkLIOBackstores refuses the import when a LIO target backstore still
// points at the zvol device.  Democratic-csi's zfs-generic-iscsi driver leaves
// exactly this trace under configfs target/core: each backstore's udev_path
// records the device it exports ("/dev/zvol/<dataset>").
func (z *Backend) checkLIOBackstores(volumeID string, candidates map[string]struct{}) error {
	coreDir := filepath.Join(z.configfsRoot, "target", "core")
	pattern := filepath.Join(coreDir, "*", "*", "udev_path")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("zfs: import %q: glob %q: %w", volumeID, pattern, err)
	}
	for _, udevPath := range matches {
		//nolint:gosec // G304: udevPath comes from a filepath.Glob under the
		// fixed configfs target/core tree, not from request input.
		data, readErr := os.ReadFile(udevPath)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // backstore vanished between Glob and ReadFile
			}
			return fmt.Errorf("zfs: import %q: read %q: %w", volumeID, udevPath, readErr)
		}
		recorded := strings.TrimSpace(string(data))
		if sameDevice(recorded, candidates) {
			return &backend.ImportRefusedError{
				VolumeID: volumeID,
				Reason:   reasonInUse,
				Detail: fmt.Sprintf("LIO backstore %q still points at %q; "+
					"remove the old driver's export (targetcli delete) first",
					strings.TrimPrefix(udevPath, z.configfsRoot), recorded),
			}
		}
	}
	return nil
}

// checkNVMeNamespaces refuses the import when an nvmet subsystem exports the
// zvol device.  This covers the case where pillar-csi itself — or another
// provisioning stack — already exports the device: adopting it would place two
// volume lifecycles on one device and double-publish it.
func (z *Backend) checkNVMeNamespaces(volumeID string, candidates map[string]struct{}) error {
	subs, err := nvmeof.ListExports(z.configfsRoot)
	if err != nil {
		return fmt.Errorf("zfs: import %q: list nvmet exports: %w", volumeID, err)
	}
	for _, sub := range subs {
		for nsid, dev := range sub.NamespaceDevicePaths {
			if sameDevice(dev, candidates) {
				return &backend.ImportRefusedError{
					VolumeID: volumeID,
					Reason:   reasonInUse,
					Detail: fmt.Sprintf("nvmet subsystem %q namespace %d still exports %q; "+
						"remove the export first", sub.NQN, nsid, dev),
				}
			}
		}
	}
	return nil
}

// checkMounts refuses the import when the zvol device is mounted anywhere in
// this mount namespace.  The agent runs privileged with the host's mount
// table, so this catches openebs-zfs-localpv-style local mounts on the storage
// host.
func (z *Backend) checkMounts(volumeID string, candidates map[string]struct{}) error {
	f, err := os.Open(z.mountsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No mount table (tests); other checks still apply.
		}
		return fmt.Errorf("zfs: import %q: open %q: %w", volumeID, z.mountsPath, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if sameDevice(fields[0], candidates) {
			return &backend.ImportRefusedError{
				VolumeID: volumeID,
				Reason:   reasonInUse,
				Detail:   fmt.Sprintf("device %q is mounted at %q; unmount it first", fields[0], fields[1]),
			}
		}
	}
	err = scanner.Err()
	if err != nil {
		return fmt.Errorf("zfs: import %q: read %q: %w", volumeID, z.mountsPath, err)
	}
	return nil
}

// checkHolders refuses the import when the zvol device (or any of its
// partitions) has device-mapper holders in sysfs — the signature of a multipath
// map, an mdadm/LVM assembly, or a stratis pool sitting on the zvol.
func (z *Backend) checkHolders(volumeID, devName string) error {
	blockDir := filepath.Join(z.sysBlockRoot, devName)
	if dirHasEntries(filepath.Join(blockDir, "holders")) {
		return heldBy(volumeID, devName)
	}
	// Partitions of the zvol (zdNpM) inherit the base name as prefix.
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // sysfs view absent (tests); exclusive probe still applies
		}
		return fmt.Errorf("zfs: import %q: read sysfs %q: %w", volumeID, blockDir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), devName) || e.Name() == devName {
			continue
		}
		if dirHasEntries(filepath.Join(blockDir, e.Name(), "holders")) {
			return heldBy(volumeID, e.Name())
		}
	}
	return nil
}

func dirHasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func heldBy(volumeID, heldName string) error {
	return &backend.ImportRefusedError{
		VolumeID: volumeID,
		Reason:   reasonInUse,
		Detail: fmt.Sprintf("device %q has device-mapper holders in sysfs; "+
			"release the holding device first", heldName),
	}
}

// checkExclusiveClaim takes and immediately releases an O_EXCL claim on the
// zvol device.  This is the catch-all check: mounted filesystems, openebs-style
// local consumers, and leftover dm devices all hold a zvol exclusively, so a
// device that passes the export scans above but fails this probe is still in
// use and the import is refused.
func (z *Backend) checkExclusiveClaim(volumeID, devPath string) error {
	release, err := z.claimDevice(devPath)
	if err != nil {
		if errors.Is(err, nvmeof.ErrDeviceHeld) {
			return &backend.ImportRefusedError{
				VolumeID: volumeID,
				Reason:   reasonInUse,
				Detail: fmt.Sprintf("device %q is held exclusively on the storage node "+
					"(mounted, exported, or opened by another consumer)", devPath),
			}
		}
		return fmt.Errorf("zfs: import %q: exclusive-open probe of %q: %w", volumeID, devPath, err)
	}
	err = release()
	if err != nil {
		return fmt.Errorf("zfs: import %q: release exclusive probe of %q: %w", volumeID, devPath, err)
	}
	return nil
}
