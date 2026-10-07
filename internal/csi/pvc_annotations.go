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

// Pvc_annotations.go — decoding of the per-volume configuration documents a
// PersistentVolumeClaim carries as annotations (the highest-precedence layer
// of the effective-configuration resolution; see resolve.go).
//
// A PVC may carry three YAML documents, one per configuration axis, whose
// shapes are identical to the corresponding CRD subtrees (the per-volume
// tunable subset of them):
//
//	pillar-csi.bhyoo.com/backend:    zfs: {properties: {...}} | lvm: {provisioningMode: ...}
//	pillar-csi.bhyoo.com/protocol:   nvmeofTcp: {maxQueueSize, inCapsuleDataSize, maxDataTransferSize,
//	                                             ctrlLossTmo, reconnectDelay}
//	                                 | iscsi: {loginTimeout, replacementTimeout, noopOutInterval, noopOutTimeout}
//	pillar-csi.bhyoo.com/filesystem: {fsType, mkfsOptions, mountOptions}
//
// Structural fields (zfs.pool, lvm.volumeGroup, nvmeofTcp.port, iscsi.acl,
// …) and unknown fields are rejected with their full path by the shared
// decoder (internal/configdocs).  Every other annotation in the
// pillar-csi.bhyoo.com/ domain is rejected too, so a typo or a key of the
// removed flat vocabulary never silently falls back to the defaults.

import (
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/configdocs"
)

// pillarAnnotationDomain is the annotation-key prefix owned by pillar-csi.
const pillarAnnotationDomain = "pillar-csi.bhyoo.com/"

// pvcDocs holds the per-volume configuration documents decoded from a PVC's
// annotations.  A nil field means the PVC does not override that axis.
type pvcDocs struct {
	Backend    *v1alpha1.BackendOverrides
	Protocol   *v1alpha1.ProtocolOverrides
	Filesystem *v1alpha1.FilesystemConfig

	// ImportZvol is the raw value of the
	// v1alpha1.AnnotationImportZvol annotation: the full ZFS dataset to
	// adopt instead of provisioning a new volume.  Empty means a normal
	// create.  Its structural validation lives in CreateVolume, which has
	// the resolved store context the check needs.
	ImportZvol       string
	ImportDirectory  string
	ImportZFSDataset string

	// ImportLV is the raw value of the v1alpha1.AnnotationImportLV
	// annotation ("<vg>/<lv>:<vg_uuid>:<lv_uuid>") and ImportLVPolicy the
	// raw value of v1alpha1.AnnotationImportLVPolicy.  Empty means absent.
	// The grammar, the policy value and the store match are validated in
	// CreateVolume (see parseImportLV and resolveLVImportRequest).
	ImportLV       string
	ImportLVPolicy string
}

// decodePVCAnnotations decodes and validates the pillar-csi annotations of a
// PVC.  It returns an error for any structural, unknown or malformed field,
// and for any unknown pillar-csi.bhyoo.com/ annotation. Import selectors are
// mutually exclusive and a present selector must name a source. An absent or
// empty configuration document leaves its axis nil. The import-lv policy is
// only accepted next to an import-lv annotation.
func decodePVCAnnotations(annotations map[string]string) (pvcDocs, error) {
	var docs pvcDocs

	err := rejectUnknownPillarKeys("PVC annotation", annotations, map[string]bool{
		configdocs.BackendDocKey:            true,
		configdocs.ProtocolDocKey:           true,
		configdocs.FilesystemDocKey:         true,
		v1alpha1.AnnotationImportZvol:       true,
		v1alpha1.AnnotationImportDirectory:  true,
		v1alpha1.AnnotationImportZFSDataset: true,
		v1alpha1.AnnotationImportLV:         true,
		v1alpha1.AnnotationImportLVPolicy:   true,
	})
	if err != nil {
		return docs, err
	}

	backend, err := configdocs.DecodeBackendOverride(
		configdocs.BackendDocKey, annotations[configdocs.BackendDocKey])
	if err != nil {
		return docs, err //nolint:wrapcheck // decoder errors already carry the annotation key and path
	}
	protocol, err := configdocs.DecodeProtocolOverride(
		configdocs.ProtocolDocKey, annotations[configdocs.ProtocolDocKey])
	if err != nil {
		return docs, err //nolint:wrapcheck // decoder errors already carry the annotation key and path
	}
	filesystem, err := configdocs.DecodeFilesystemDoc(
		configdocs.FilesystemDocKey, annotations[configdocs.FilesystemDocKey])
	if err != nil {
		return docs, err //nolint:wrapcheck // decoder errors already carry the annotation key and path
	}

	docs.Backend, docs.Protocol, docs.Filesystem = backend, protocol, filesystem
	// Selectors are plain source names. Never turn malformed adoption intent
	// into a fresh dynamically provisioned volume.
	count := 0
	for _, selector := range []struct {
		key   string
		value *string
	}{
		{v1alpha1.AnnotationImportZvol, &docs.ImportZvol},
		{v1alpha1.AnnotationImportDirectory, &docs.ImportDirectory},
		{v1alpha1.AnnotationImportZFSDataset, &docs.ImportZFSDataset},
	} {
		raw, present := annotations[selector.key]
		if !present {
			continue
		}
		count++
		*selector.value = strings.TrimSpace(raw)
		if *selector.value == "" {
			return docs, fmt.Errorf("unsupported PVC annotation %q: value must name a source, got empty", selector.key)
		}
	}
	if count > 1 {
		return docs, fmt.Errorf("PVC import selectors %q, %q and %q are mutually exclusive",
			v1alpha1.AnnotationImportZvol, v1alpha1.AnnotationImportDirectory, v1alpha1.AnnotationImportZFSDataset)
	}
	err = decodeImportLV(annotations, &docs)
	return docs, err
}

// decodeImportLV reads the import-lv annotation pair into docs.  Like
// import-zvol, a present-but-empty value is malformed rather than absent:
// an intended adoption must never silently become a fresh empty volume, and
// an empty policy must never silently select the default.
func decodeImportLV(annotations map[string]string, docs *pvcDocs) error {
	raw, present := annotations[v1alpha1.AnnotationImportLV]
	docs.ImportLV = strings.TrimSpace(raw)
	if present && docs.ImportLV == "" {
		return fmt.Errorf("unsupported PVC annotation %q: value must name an LVM logical "+
			"volume as \"<vg>/<lv>:<vg_uuid>:<lv_uuid>\", got empty", v1alpha1.AnnotationImportLV)
	}
	rawPolicy, policyPresent := annotations[v1alpha1.AnnotationImportLVPolicy]
	docs.ImportLVPolicy = strings.TrimSpace(rawPolicy)
	if policyPresent && docs.ImportLVPolicy == "" {
		return fmt.Errorf("unsupported PVC annotation %q: value must be %q or %q, got empty",
			v1alpha1.AnnotationImportLVPolicy,
			v1alpha1.ImportLVPolicyPreserveOriginal, v1alpha1.ImportLVPolicyManaged)
	}
	if policyPresent && !present {
		return fmt.Errorf("unsupported PVC annotation %q: it selects the policy of an %q "+
			"adoption and is invalid without it", v1alpha1.AnnotationImportLVPolicy, v1alpha1.AnnotationImportLV)
	}
	if present && (docs.ImportZvol != "" || docs.ImportDirectory != "" || docs.ImportZFSDataset != "") {
		return fmt.Errorf("PVC annotations %q, %q, %q and %q are mutually exclusive: a claim adopts "+
			"at most one existing volume", v1alpha1.AnnotationImportZvol, v1alpha1.AnnotationImportDirectory,
			v1alpha1.AnnotationImportZFSDataset, v1alpha1.AnnotationImportLV)
	}
	return nil
}

// rejectUnknownPillarKeys returns an error naming every key in the
// pillar-csi.bhyoo.com/ domain that is not in allowed.  The kind argument
// names the input ("PVC annotation", "StorageClass parameter") in the
// message, which also lists the accepted document keys.
func rejectUnknownPillarKeys(kind string, m map[string]string, allowed map[string]bool) error {
	var unknown []string
	for k := range m {
		if strings.HasPrefix(k, pillarAnnotationDomain) && !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	allowedKeys := make([]string, 0, len(allowed))
	for k := range allowed {
		allowedKeys = append(allowedKeys, k)
	}
	sort.Strings(allowedKeys)

	return fmt.Errorf("unsupported %s %q: pillar-csi settings are YAML documents under %s",
		kind, unknown[0], strings.Join(allowedKeys, ", "))
}
