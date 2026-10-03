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

package csi

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// validateFilesystemConfig checks the resolved filesystem configuration of a
// CreateVolume request, so that a setting that cannot take effect is
// rejected (InvalidArgument) instead of being silently dropped:
//
//   - for a Filesystem volume the mkfs options must pass the allowlist of
//     the resolved fsType (validateMkfsOptions);
//   - a raw block volume has no filesystem, so a per-volume filesystem
//     document (pvcFS, the PVC annotation) is rejected for it.  Class-level
//     filesystem settings apply to the class's Filesystem volumes and are
//     ignored for Block volumes, exactly like csi.storage.k8s.io/fstype.
func validateFilesystemConfig(
	fs *v1alpha1.FilesystemConfig,
	pvcFS *v1alpha1.FilesystemConfig,
	caps []*csi.VolumeCapability,
) error {
	for _, c := range caps {
		if c.GetMount() != nil {
			if fs.FSType != ProtocolNFS {
				err := validateMkfsOptions(fs.FSType, derefList(fs.MkfsOptions))
				if err != nil {
					return err
				}
			}
			continue
		}
		if pvcFS != nil {
			return fmt.Errorf("%s cannot apply to a raw block volume (volumeMode: Block)",
				v1alpha1.AnnotationFilesystemDoc)
		}
	}
	return nil
}

// filesystemVolumeContext returns the VolumeContext entries that carry the
// resolved filesystem configuration to NodeStageVolume / NodePublishVolume:
// the fsType, the mkfs options (JSON array, when any) and the mount options
// (JSON array, when the resolved configuration sets them — an explicit empty
// list is written so the node clears the StorageClass options).
func filesystemVolumeContext(fs *v1alpha1.FilesystemConfig, volCtx map[string]string) {
	if fs == nil {
		return
	}
	volCtx[paramFSType] = fs.FSType
	if opts := derefList(fs.MkfsOptions); len(opts) > 0 {
		volCtx[paramMkfsOptions] = mustJSON(opts)
	}
	if fs.MountOptions != nil {
		volCtx[paramMountOptions] = mustJSON(*fs.MountOptions)
	}
	if fs.PeriodicTrim != nil {
		volCtx[paramPeriodicTrim] = strconv.FormatBool(*fs.PeriodicTrim)
	}
}

// stageFilesystem resolves the filesystem type, mkfs options and mount flags
// that NodeStageVolume uses for a MOUNT volume from its VolumeContext
// (written by CreateVolume) and VolumeCapability.
//
// The type is, in order of precedence: the resolved fsType (paramFSType),
// the capability's fsType (the PV's csi.fsType, taken from the StorageClass
// csi.storage.k8s.io/fstype), then ext4.  The resolved value has to win
// because the external-provisioner writes the PV's csi.fsType from the
// StorageClass only; the VolumeContext is the sole carrier of a per-PVC
// choice.  The mkfs options are only used when the device carries no
// filesystem yet — see Mounter.FormatAndMount.
//
// The mount flags are the resolved mount options (paramMountOptions) when the
// VolumeContext carries them, otherwise the capability's mount flags (the
// PV's mountOptions, taken from the StorageClass).
func stageFilesystem(
	volCtx map[string]string,
	volCap *csi.VolumeCapability,
) (stagedFilesystem, error) {
	override := volCtx[paramFSType]
	if override != "" && override != defaultFsType && override != xfsFsType {
		return stagedFilesystem{}, fmt.Errorf("volume_context %s %q is unsupported: must be %q or %q",
			paramFSType, override, defaultFsType, xfsFsType)
	}
	fsType := formatFsType(override, volCap.GetMount())
	mkfsOptions, err := parseMkfsOptions(fsType, volCtx[paramMkfsOptions])
	if err != nil {
		return stagedFilesystem{}, fmt.Errorf("volume_context: %w", err)
	}
	mountFlags, err := resolveMountFlags(volCtx, volCap)
	if err != nil {
		return stagedFilesystem{}, err
	}
	periodicTrim, err := parsePeriodicTrim(volCtx)
	if err != nil {
		return stagedFilesystem{}, err
	}
	return stagedFilesystem{
		fsType:       fsType,
		mkfsOptions:  mkfsOptions,
		mountFlags:   mountFlags,
		PeriodicTrim: periodicTrim,
	}, nil
}

// stagedFilesystem is the filesystem configuration NodeStageVolume applies
// to a MOUNT volume.
type stagedFilesystem struct {
	fsType      string
	mkfsOptions []string
	mountFlags  []string
	// PeriodicTrim is the resolved periodicTrim setting, nil when no layer
	// set it (the node setting applies).
	PeriodicTrim *bool
}

// parsePeriodicTrim returns the resolved periodicTrim setting from the
// VolumeContext, or nil when the key is absent.  Only "true" and "false"
// are accepted — the values filesystemVolumeContext writes.
func parsePeriodicTrim(volCtx map[string]string) (*bool, error) {
	raw, ok := volCtx[paramPeriodicTrim]
	if !ok {
		return nil, nil //nolint:nilnil // absent key means "not set"
	}
	var v bool
	switch raw {
	case topologyValueTrue:
		v = true
	case "false":
	default:
		return nil, fmt.Errorf("volume_context %s %q is unsupported: must be %q or %q",
			paramPeriodicTrim, raw, topologyValueTrue, "false")
	}
	return &v, nil
}

// resolveMountFlags returns the resolved mount options from the VolumeContext
// when present, otherwise the capability's mount flags.
func resolveMountFlags(volCtx map[string]string, volCap *csi.VolumeCapability) ([]string, error) {
	raw, ok := volCtx[paramMountOptions]
	if !ok {
		return volCap.GetMount().GetMountFlags(), nil
	}
	var flags []string
	err := json.Unmarshal([]byte(raw), &flags)
	if err != nil {
		return nil, fmt.Errorf("volume_context: decode %s %q as a JSON string array: %w",
			paramMountOptions, raw, err)
	}
	return flags, nil
}

// stagedMountReadOnly reports whether the resolved mount flags mount the
// filesystem read-only ("ro").  The caller needs that answer before running
// the write-based health probe (CheckMountHealth): EROFS on a requested
// read-only mount is the expected outcome, not evidence of a dead
// filesystem, so probing a mount staged with "ro" would misclassify it as
// ErrMountUnhealthy (issue #168).
func stagedMountReadOnly(volCtx map[string]string, volCap *csi.VolumeCapability) (bool, error) {
	flags, err := resolveMountFlags(volCtx, volCap)
	if err != nil {
		return false, err
	}
	return slices.Contains(flags, "ro"), nil
}

// formatFsType returns the type NodeStageVolume formats a MOUNT volume with
// (see stageFilesystem): the resolved override, the capability fsType, then ext4.
func formatFsType(override string, mnt *csi.VolumeCapability_MountVolume) string {
	if override != "" {
		return override
	}
	if fsType := mnt.GetFsType(); fsType != "" {
		return fsType
	}
	return defaultFsType
}

// derefList returns the list a *[]string points to, or nil.
func derefList(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// mustJSON encodes a string slice as a JSON array.  Marshaling a []string
// cannot fail.
func mustJSON(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal string slice: %v", err))
	}
	return string(b)
}
