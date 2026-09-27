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
	"fmt"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// validateFilesystemParams checks the filesystem settings of a CreateVolume
// request after the parameter merge, so that a setting that cannot take
// effect is rejected (InvalidArgument) instead of being silently dropped:
//
//   - paramFSType (only written by the PVC fs-override annotation) must be
//     "ext4" or "xfs";
//   - paramMkfsOptions must be a JSON string array and, for a Filesystem
//     volume, pass the mkfs allowlist of the type the node will format
//     (validateMkfsOptions);
//   - file protocols (NFS, SMB) have no node-side filesystem, so neither
//     setting is accepted for them;
//   - a raw block volume has no filesystem either, so a PVC fsType is
//     rejected, and so are mkfs options that differ from the StorageClass
//     value (i.e. come from the PVC).  Class-level mkfs options apply to the
//     class's filesystem volumes and are ignored for block volumes, exactly
//     like the class's csi.storage.k8s.io/fstype.
func validateFilesystemParams(
	params, scParams map[string]string,
	protocolType v1alpha1.ProtocolType,
	caps []*csi.VolumeCapability,
) error {
	fsType := params[paramFSType]
	if fsType != "" && fsType != defaultFsType && fsType != xfsFsType {
		return fmt.Errorf("unsupported %s %q: must be %q or %q", paramFSType, fsType, defaultFsType, xfsFsType)
	}
	mkfsRaw := params[paramMkfsOptions]
	mkfsOpts, err := decodeMkfsOptions(mkfsRaw)
	if err != nil {
		return err
	}
	if fsType == "" && mkfsRaw == "" {
		return nil
	}
	if isFileProtocol(protocolType) {
		return fmt.Errorf("protocol %q: filesystem settings (fsType, mkfsOptions) apply only to "+
			"block protocols whose volumes the node formats", protocolType)
	}
	for _, c := range caps {
		if mnt := c.GetMount(); mnt != nil {
			err = validateMkfsOptions(formatFsType(fsType, mnt), mkfsOpts)
			if err != nil {
				return err
			}
			continue
		}
		if fsType != "" {
			return fmt.Errorf("fsType %q cannot apply to a raw block volume (volumeMode: Block)", fsType)
		}
		if mkfsRaw != scParams[paramMkfsOptions] {
			return fmt.Errorf("mkfsOptions %s cannot apply to a raw block volume (volumeMode: Block)", mkfsRaw)
		}
	}
	return nil
}

// formatFsType returns the type NodeStageVolume formats a MOUNT volume with
// (see stageFilesystem): the PVC override, the capability fsType, then ext4.
func formatFsType(override string, mnt *csi.VolumeCapability_MountVolume) string {
	if override != "" {
		return override
	}
	if fsType := mnt.GetFsType(); fsType != "" {
		return fsType
	}
	return defaultFsType
}

// stageFilesystem resolves the filesystem type and mkfs options that
// NodeStageVolume uses for a MOUNT volume from its VolumeContext (written by
// CreateVolume) and VolumeCapability.
//
// The type is, in order of precedence: the per-PVC fs-override fsType
// (paramFSType), the capability's fsType (the PV's csi.fsType, taken from the
// StorageClass csi.storage.k8s.io/fstype), then ext4.  A PVC override has to
// win because the external-provisioner writes the PV's csi.fsType from the
// StorageClass only; the VolumeContext is the sole carrier of the per-PVC
// choice.  The mkfs options (paramMkfsOptions) are only used when the device
// carries no filesystem yet — see Mounter.FormatAndMount.
func stageFilesystem(
	volCtx map[string]string,
	volCap *csi.VolumeCapability,
) (fsType string, mkfsOptions []string, err error) {
	override := volCtx[paramFSType]
	if override != "" && override != defaultFsType && override != xfsFsType {
		return "", nil, fmt.Errorf("volume_context %s %q is unsupported: must be %q or %q",
			paramFSType, override, defaultFsType, xfsFsType)
	}
	fsType = formatFsType(override, volCap.GetMount())
	mkfsOptions, err = parseMkfsOptions(fsType, volCtx[paramMkfsOptions])
	if err != nil {
		return "", nil, fmt.Errorf("volume_context: %w", err)
	}
	return fsType, mkfsOptions, nil
}
