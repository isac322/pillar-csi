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
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// parseMkfsOptions decodes the paramMkfsOptions value — a JSON array of
// strings, one mkfs argv element each — and validates every element with
// validateMkfsOptions.  An empty raw value yields no options.
//
// The same encoding is written by the PillarStorageClass reconciler
// (PillarProtocol.spec.mkfsOptions / PillarStorageClass.spec.overrides.mkfsOptions)
// and by the PVC fs-override annotation, travels unchanged in the PV
// VolumeContext, and is decoded again by NodeStageVolume.
func parseMkfsOptions(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	var opts []string
	err := json.Unmarshal([]byte(raw), &opts)
	if err != nil {
		return nil, fmt.Errorf("decode %s %q as a JSON string array: %w", paramMkfsOptions, raw, err)
	}
	err = validateMkfsOptions(opts)
	if err != nil {
		return nil, err
	}
	return opts, nil
}

// validateMkfsOptions rejects mkfs arguments that could make mkfs touch
// anything other than the volume being formatted.
//
// The arguments are passed to mkfs as separate argv elements (no shell), so
// quoting and command injection are not possible.  However, mkfs accepts
// options that name other files or devices — an external journal or log
// device (mke2fs -J device=, mkfs.xfs -l logdev= / -r rtdev=), a directory
// or prototype file to copy into the new filesystem (mke2fs -d, mkfs.xfs -p),
// an undo file to write (mke2fs -z) — and mkfs runs as root on the node with
// the host /dev visible.  Because PVC annotations are writable by namespace
// users, any element containing a path separator or a parent-directory
// reference is rejected, and mkfs is run from an empty working directory
// (see KubeMounter) so a bare relative name cannot resolve to a node file.
func validateMkfsOptions(opts []string) error {
	for i, opt := range opts {
		switch {
		case strings.TrimSpace(opt) == "":
			return fmt.Errorf("mkfs option %d is empty", i)
		case strings.ContainsRune(opt, 0):
			return fmt.Errorf("mkfs option %d %q contains a NUL byte", i, opt)
		case strings.Contains(opt, "/"):
			return fmt.Errorf("mkfs option %d %q must not contain a path separator: "+
				"mkfs options may not reference files or devices", i, opt)
		case strings.Contains(opt, ".."):
			return fmt.Errorf("mkfs option %d %q must not contain a parent-directory reference", i, opt)
		}
	}
	return nil
}

// validateFilesystemParams checks the filesystem settings of a CreateVolume
// request after the parameter merge, so that a setting that cannot take
// effect is rejected (InvalidArgument) instead of being silently dropped:
//
//   - paramFSType (only written by the PVC fs-override annotation) must be
//     "ext4" or "xfs";
//   - paramMkfsOptions must be a valid JSON string array (parseMkfsOptions);
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
	_, err := parseMkfsOptions(mkfsRaw)
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
		if c.GetBlock() == nil {
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
	fsType = volCtx[paramFSType]
	switch fsType {
	case "":
		fsType = volCap.GetMount().GetFsType()
		if fsType == "" {
			fsType = defaultFsType
		}
	case defaultFsType, xfsFsType:
	default:
		return "", nil, fmt.Errorf("volume_context %s %q is unsupported: must be %q or %q",
			paramFSType, fsType, defaultFsType, xfsFsType)
	}
	mkfsOptions, err = parseMkfsOptions(volCtx[paramMkfsOptions])
	if err != nil {
		return "", nil, fmt.Errorf("volume_context: %w", err)
	}
	return fsType, mkfsOptions, nil
}
