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

// Pvc_annotations.go — utility for parsing and validating PVC-level parameter
// overrides (Layer 4 of the 4-level merge hierarchy).
//
// The annotation format supports two styles:
//
//  1. Flat key-value (legacy / low-level):
//     annotation key: "pillar-csi.bhyoo.com/param.<param-key>"
//     annotation value: the param value
//     → maps directly to the param key after stripping the prefix.
//
//  2. Structured YAML (high-level, human-friendly):
//     annotation key: one of AnnotationBackendOverride,
//     AnnotationProtocolOverride, AnnotationFSOverride
//     annotation value: YAML document encoding backend, protocol, or FS params.
//
// Structural fields (pool names, parent datasets, port numbers, protocol type)
// cannot be overridden via PVC annotations; ParsePVCAnnotations returns an
// error if such a field is present so that CreateVolume can reject the request
// with InvalidArgument.
//
// See PRD §2.3 "파라미터 오버라이드 계층" for the full specification.

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// PVC annotation key constants
// ─────────────────────────────────────────────────────────────────────────────.

const (
	// AnnotationBackendOverride is the PVC annotation key for backend-level
	// parameter overrides. Its value is a YAML document.
	//
	// Supported sub-keys (Phase 1):
	//   zfs.properties.<name>: ZFS property override (e.g. compression, volblocksize)
	//
	// Blocked structural fields: zfs.pool, zfs.parentDataset
	//
	// Example:
	//   pillar-csi.bhyoo.com/backend-override: |
	//     zfs:
	//       properties:
	//         compression: zstd
	//         volblocksize: "16K"
	AnnotationBackendOverride = "pillar-csi.bhyoo.com/backend-override"

	// AnnotationProtocolOverride is the PVC annotation key for protocol-level
	// parameter overrides. Its value is a YAML document.
	//
	// Supported sub-keys (Phase 1):
	//   nvmeofTcp.maxQueueSize:      I/O queue depth (initiator queue_size)
	//   nvmeofTcp.inCapsuleDataSize: in-capsule data size in bytes (target
	//                                port setting shared by the port's volumes)
	//   nvmeofTcp.ctrlLossTmo:       controller loss timeout (seconds)
	//   nvmeofTcp.reconnectDelay:    reconnect attempt interval (seconds)
	//   iscsi.loginTimeout:        session login timeout (seconds)
	//   iscsi.replacementTimeout:  session replacement timeout (seconds)
	//   iscsi.nodeSessionTimeout:  node session timeout (seconds)
	//
	// Blocked structural fields: nvmeofTcp.port, iscsi.port
	//
	// Example:
	//   pillar-csi.bhyoo.com/protocol-override: |
	//     nvmeofTcp:
	//       maxQueueSize: 64
	//       ctrlLossTmo: 600
	AnnotationProtocolOverride = "pillar-csi.bhyoo.com/protocol-override"

	// AnnotationFSOverride is the PVC annotation key for filesystem-related
	// overrides. Its value is a YAML document.
	//
	// Supported sub-keys:
	//   fsType:      "ext4" or "xfs"
	//   mkfsOptions: list of extra mkfs arguments
	//
	// Example:
	//   pillar-csi.bhyoo.com/fs-override: |
	//     fsType: xfs
	//     mkfsOptions: ["-K"]
	AnnotationFSOverride = "pillar-csi.bhyoo.com/fs-override"
)

// ─────────────────────────────────────────────────────────────────────────────
// Protocol tuning parameter key constants (output of ParsePVCAnnotations)
// ─────────────────────────────────────────────────────────────────────────────.

const (
	// NVMe-oF TCP tuning parameters.

	// ParamNVMeOFMaxQueueSize is the I/O queue depth for NVMe-oF TCP.
	// Corresponds to PillarProtocol.spec.nvmeofTcp.maxQueueSize.
	// Copied into the VolumeContext by CreateVolume and written as the
	// queue_size /dev/nvme-fabrics option at NodeStageVolume.
	paramNVMeOFMaxQueueSize = "pillar-csi.bhyoo.com/nvmeof-max-queue-size"

	// ParamNVMeOFInCapsuleDataSize is the in-capsule data size in bytes.
	// Corresponds to PillarProtocol.spec.nvmeofTcp.inCapsuleDataSize.
	// Sent to the agent in NvmeofTcpExportParams.in_capsule_data_size and
	// applied as the nvmet port's param_inline_data_size.
	paramNVMeOFInCapsuleDataSize = "pillar-csi.bhyoo.com/nvmeof-in-capsule-data-size"

	// ParamNVMeOFCtrlLossTmo is the controller loss timeout in seconds.
	// Corresponds to PillarProtocol.spec.nvmeofTcp.ctrlLossTmo.
	// Copied into the VolumeContext by CreateVolume and written as the
	// ctrl_loss_tmo /dev/nvme-fabrics option at NodeStageVolume.
	paramNVMeOFCtrlLossTmo = "pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo"

	// ParamNVMeOFReconnectDelay is the reconnect attempt interval in seconds.
	// Corresponds to PillarProtocol.spec.nvmeofTcp.reconnectDelay.
	// Copied into the VolumeContext by CreateVolume and written as the
	// reconnect_delay /dev/nvme-fabrics option at NodeStageVolume.
	paramNVMeOFReconnectDelay = "pillar-csi.bhyoo.com/nvmeof-reconnect-delay"

	// ISCSI tuning parameters.

	// ParamISCSILoginTimeout is the session login timeout in seconds.
	// Corresponds to PillarProtocol.spec.iscsi.loginTimeout.
	paramISCSILoginTimeout = "pillar-csi.bhyoo.com/iscsi-login-timeout"

	// ParamISCSIReplacementTimeout is the session replacement timeout in
	// seconds. Corresponds to PillarProtocol.spec.iscsi.replacementTimeout.
	paramISCSIReplacementTimeout = "pillar-csi.bhyoo.com/iscsi-replacement-timeout"

	// ParamISCSINodeSessionTimeout is the node session timeout in seconds.
	// Corresponds to PillarProtocol.spec.iscsi.nodeSessionTimeout.
	paramISCSINodeSessionTimeout = "pillar-csi.bhyoo.com/iscsi-node-session-timeout"

	// Filesystem parameters.

	// ParamFSType is the per-PVC filesystem type override (fs-override
	// fsType).  Valid values: "ext4", "xfs".  Copied into the VolumeContext
	// by CreateVolume; NodeStageVolume formats and mounts with it in place of
	// the PV's csi.fsType.
	paramFSType = "pillar-csi.bhyoo.com/fs-type"

	// ParamMkfsOptions is a JSON-encoded string array of extra mkfs arguments,
	// written by the StorageClass reconciler and the PVC fs-override.
	// Example value: `["-E","lazy_itable_init=0"]`.  Copied into the
	// VolumeContext by CreateVolume and used when NodeStageVolume formats a
	// blank device (see parseMkfsOptions).
	paramMkfsOptions = "pillar-csi.bhyoo.com/mkfs-options"
)

// ─────────────────────────────────────────────────────────────────────────────
// Blocked structural fields
// ─────────────────────────────────────────────────────────────────────────────.

// blockedZFSFields is the set of ZFS sub-fields that are structural (define
// which pool/dataset to use) and therefore cannot be overridden via PVC
// annotations.  Only tuning properties are allowed.
var blockedZFSFields = map[string]bool{
	"pool":          true,
	"parentDataset": true,
}

// blockedLVMFields is the set of LVM sub-fields that are structural (define
// which volume group/thin pool to use) and therefore cannot be overridden via
// PVC annotations.  Only the provisioning mode is allowed.
var blockedLVMFields = map[string]bool{
	"volumeGroup": true,
	"thinPool":    true,
}

// blockedNVMeOFFields is the set of NVMe-oF TCP sub-fields that are
// structural and cannot be overridden via PVC annotations.
var blockedNVMeOFFields = map[string]bool{
	"port": true,
}

// blockedISCSIFields is the set of iSCSI sub-fields that are structural
// and cannot be overridden via PVC annotations.
var blockedISCSIFields = map[string]bool{
	"port": true,
}

// paramKeyPrefix is the namespace of the pillar-csi CreateVolume parameter
// keys; a flat "param.<name>" annotation overrides key paramKeyPrefix+<name>.
const paramKeyPrefix = "pillar-csi.bhyoo.com/"

// blockedFlatParams are the parameter names (without paramKeyPrefix) that
// route or secure a volume — which PillarStorageClass, store, agent, pool,
// dataset, port or access control it uses — and therefore cannot be set by
// a flat "param.<name>" PVC annotation.
var blockedFlatParams = map[string]bool{
	"storage-class":      true,
	"store":              true,
	"protocol":           true,
	"backend-type":       true,
	"protocol-type":      true,
	"agent":              true,
	"zfs-parent-dataset": true,
	"lvm-vg":             true,
	"lvm-thin-pool":      true,
	"nvmeof-port":        true,
	"iscsi-port":         true,
	"nfs-version":        true,
	"acl-enabled":        true,
}

// ─────────────────────────────────────────────────────────────────────────────
// ParsePVCAnnotations
// ─────────────────────────────────────────────────────────────────────────────.

// ParsePVCAnnotations parses and validates pillar-csi PVC annotations into a
// flat parameter override map that can be merged (highest-priority) into the
// StorageClass parameter map at CreateVolume time.
//
// It processes three annotation types:
//   - AnnotationBackendOverride  — backend (ZFS, LVM, …) tuning properties
//   - AnnotationProtocolOverride — protocol (NVMe-oF, iSCSI, …) tuning params
//   - AnnotationFSOverride       — filesystem type and mkfs options
//
// In addition, any annotation "pillar-csi.bhyoo.com/param.<name>" with a
// non-empty value sets parameter "pillar-csi.bhyoo.com/<name>" (legacy /
// low-level override path); structural names (blockedFlatParams) are an error.
//
// ParsePVCAnnotations returns an error if any annotation attempts to override
// a blocked structural field (e.g. zfs.pool, lvm.volumeGroup, nvmeofTcp.port).  All other
// errors (YAML parse errors, type mismatches) are also returned so that the
// caller can surface them as InvalidArgument failures.
//
// The returned map is safe to mutate; it is never nil even when annotations is
// nil or empty.
func ParsePVCAnnotations(annotations map[string]string) (map[string]string, error) {
	result := make(map[string]string)

	// ── Flat param.* overrides (legacy, low-level path) ───────────────────
	// Example: "pillar-csi.bhyoo.com/param.zfs-prop.compression" = "lz4"
	// → result["pillar-csi.bhyoo.com/zfs-prop.compression"] = "lz4"
	// An empty value is not an override: the lower layers stay in effect.
	for k, v := range annotations {
		name, ok := strings.CutPrefix(k, pvcAnnotationParamPrefix)
		if !ok || name == "" || v == "" {
			continue
		}
		if blockedFlatParams[name] {
			return nil, fmt.Errorf("%s: %q is a structural parameter and cannot be overridden via PVC annotation",
				k, name)
		}
		result[paramKeyPrefix+name] = v
	}

	// ── Structured YAML: backend-override ─────────────────────────────────
	if yamlStr, ok := annotations[AnnotationBackendOverride]; ok && yamlStr != "" {
		err := parseBackendOverride(yamlStr, result)
		if err != nil {
			return nil, fmt.Errorf("%s annotation: %w", AnnotationBackendOverride, err)
		}
	}

	// ── Structured YAML: protocol-override ────────────────────────────────
	if yamlStr, ok := annotations[AnnotationProtocolOverride]; ok && yamlStr != "" {
		err := parseProtocolOverride(yamlStr, result)
		if err != nil {
			return nil, fmt.Errorf("%s annotation: %w", AnnotationProtocolOverride, err)
		}
	}

	// ── Structured YAML: fs-override ──────────────────────────────────────
	if yamlStr, ok := annotations[AnnotationFSOverride]; ok && yamlStr != "" {
		err := parseFSOverride(yamlStr, result)
		if err != nil {
			return nil, fmt.Errorf("%s annotation: %w", AnnotationFSOverride, err)
		}
	}

	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal parsers
// ─────────────────────────────────────────────────────────────────────────────.

// parseBackendOverride parses the backend-override YAML annotation into out.
//
// Supported YAML structure:
//
//	zfs:
//	  properties:
//	    <name>: <value>
//	lvm:
//	  provisioningMode: linear | thin
//
// Returns an error if a blocked structural field is present or the YAML is
// malformed.
func parseBackendOverride(yamlStr string, out map[string]string) error {
	var raw map[string]any
	err := yaml.Unmarshal([]byte(yamlStr), &raw)
	if err != nil {
		return fmt.Errorf("YAML parse error: %w", err)
	}
	if raw == nil {
		return nil // empty document
	}

	if zfsRaw, ok := raw["zfs"]; ok {
		zfsErr := parseZFSBackendOverride(zfsRaw, out)
		if zfsErr != nil {
			return zfsErr
		}
	}

	if lvmRaw, ok := raw["lvm"]; ok {
		lvmErr := parseLVMBackendOverride(lvmRaw, out)
		if lvmErr != nil {
			return lvmErr
		}
	}

	return nil
}

// parseZFSBackendOverride handles the "zfs" section of backend-override.
func parseZFSBackendOverride(zfsRaw any, out map[string]string) error {
	zfsMap, ok := zfsRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("zfs: expected a map, got %T", zfsRaw)
	}

	for field := range zfsMap {
		if blockedZFSFields[field] {
			return fmt.Errorf("zfs.%s is a structural parameter and cannot be overridden via PVC annotation", field)
		}
	}

	if propsRaw, ok := zfsMap["properties"]; ok {
		propsMap, ok := propsRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("zfs.properties: expected a map, got %T", propsRaw)
		}
		for k, v := range propsMap {
			out[paramZFSPropPrefix+k] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}

// parseLVMBackendOverride handles the "lvm" section of backend-override.
func parseLVMBackendOverride(lvmRaw any, out map[string]string) error {
	lvmMap, ok := lvmRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("lvm: expected a map, got %T", lvmRaw)
	}

	for field := range lvmMap {
		if blockedLVMFields[field] {
			return fmt.Errorf("lvm.%s is a structural parameter and cannot be overridden via PVC annotation", field)
		}
	}

	if modeRaw, ok := lvmMap["provisioningMode"]; ok {
		out[paramLVMMode] = fmt.Sprintf("%v", modeRaw)
	}
	return nil
}

// parseProtocolOverride parses the protocol-override YAML annotation into out.
//
// Supported YAML structures (Phase 1):
//
//	nvmeofTcp:
//	  maxQueueSize:      <int>
//	  inCapsuleDataSize: <int>
//	  ctrlLossTmo:       <int>
//	  reconnectDelay:    <int>
//
//	iscsi:
//	  loginTimeout:        <int>
//	  replacementTimeout:  <int>
//	  nodeSessionTimeout:  <int>
//
// Returns an error if a blocked structural field is present or the YAML is
// malformed.
func parseProtocolOverride(yamlStr string, out map[string]string) error { //nolint:gocognit,gocyclo // branching
	var raw map[string]any
	err := yaml.Unmarshal([]byte(yamlStr), &raw)
	if err != nil {
		return fmt.Errorf("YAML parse error: %w", err)
	}
	if raw == nil {
		return nil
	}

	// ── NVMe-oF TCP ────────────────────────────────────────────────────────
	if nvmeofRaw, ok := raw["nvmeofTcp"]; ok {
		nvmeofMap, ok := nvmeofRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("nvmeofTcp: expected a map, got %T", nvmeofRaw)
		}

		for field := range nvmeofMap {
			if blockedNVMeOFFields[field] {
				return fmt.Errorf("nvmeofTcp.%s is a structural parameter and cannot be overridden via PVC annotation", field)
			}
		}

		if v, ok := nvmeofMap["maxQueueSize"]; ok {
			out[paramNVMeOFMaxQueueSize] = fmt.Sprintf("%v", v)
		}
		if v, ok := nvmeofMap["inCapsuleDataSize"]; ok {
			out[paramNVMeOFInCapsuleDataSize] = fmt.Sprintf("%v", v)
		}
		if v, ok := nvmeofMap["ctrlLossTmo"]; ok {
			out[paramNVMeOFCtrlLossTmo] = fmt.Sprintf("%v", v)
		}
		if v, ok := nvmeofMap["reconnectDelay"]; ok {
			out[paramNVMeOFReconnectDelay] = fmt.Sprintf("%v", v)
		}
	}

	// ── iSCSI ──────────────────────────────────────────────────────────────
	if iscsiRaw, ok := raw["iscsi"]; ok {
		iscsiMap, ok := iscsiRaw.(map[string]any)
		if !ok {
			return fmt.Errorf("iscsi: expected a map, got %T", iscsiRaw)
		}

		for field := range iscsiMap {
			if blockedISCSIFields[field] {
				return fmt.Errorf("iscsi.%s is a structural parameter and cannot be overridden via PVC annotation", field)
			}
		}

		if v, ok := iscsiMap["loginTimeout"]; ok {
			out[paramISCSILoginTimeout] = fmt.Sprintf("%v", v)
		}
		if v, ok := iscsiMap["replacementTimeout"]; ok {
			out[paramISCSIReplacementTimeout] = fmt.Sprintf("%v", v)
		}
		if v, ok := iscsiMap["nodeSessionTimeout"]; ok {
			out[paramISCSINodeSessionTimeout] = fmt.Sprintf("%v", v)
		}
	}

	return nil
}

// parseFSOverride parses the fs-override YAML annotation into out.
//
// Supported YAML structure:
//
//	fsType: ext4 | xfs
//	mkfsOptions:
//	  - <option>
//	  - …
//
// Returns an error if fsType is not one of the supported values or the YAML
// is malformed.
func parseFSOverride(yamlStr string, out map[string]string) error {
	var raw map[string]any
	err := yaml.Unmarshal([]byte(yamlStr), &raw)
	if err != nil {
		return fmt.Errorf("YAML parse error: %w", err)
	}
	if raw == nil {
		return nil
	}

	if fsTypeRaw, ok := raw["fsType"]; ok {
		fsType, ok := fsTypeRaw.(string)
		if !ok {
			return fmt.Errorf("fsType: expected a string, got %T", fsTypeRaw)
		}
		if fsType != defaultFsType && fsType != xfsFsType {
			return fmt.Errorf("unsupported fsType %q: must be \"ext4\" or \"xfs\"", fsType)
		}
		out[paramFSType] = fsType
	}

	if mkfsRaw, ok := raw["mkfsOptions"]; ok {
		mkfsList, ok := mkfsRaw.([]any)
		if !ok {
			return fmt.Errorf("mkfsOptions: expected a list, got %T", mkfsRaw)
		}
		opts := make([]string, 0, len(mkfsList))
		for i, opt := range mkfsList {
			s, ok := opt.(string)
			if !ok {
				return fmt.Errorf("mkfsOptions[%d]: expected a string, got %T", i, opt)
			}
			opts = append(opts, s)
		}
		jsonBytes, err := json.Marshal(opts)
		if err != nil {
			// json.Marshal of []string never fails in practice, but handle it.
			return fmt.Errorf("mkfsOptions: JSON encode error: %w", err)
		}
		out[paramMkfsOptions] = string(jsonBytes)
	}

	return nil
}
