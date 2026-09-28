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
//	pillar-csi.bhyoo.com/protocol:   nvmeofTcp: {maxQueueSize, inCapsuleDataSize, ctrlLossTmo, reconnectDelay}
//	pillar-csi.bhyoo.com/filesystem: {fsType, mkfsOptions, mountOptions}
//
// Structural fields (zfs.pool, lvm.volumeGroup, nvmeofTcp.port, nvmeofTcp.acl,
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
}

// decodePVCAnnotations decodes and validates the pillar-csi annotations of a
// PVC.  It returns an error for any structural, unknown or malformed field,
// and for any pillar-csi.bhyoo.com/ annotation that is not one of the three
// document keys.  An absent or empty document leaves its axis nil.
func decodePVCAnnotations(annotations map[string]string) (pvcDocs, error) {
	var docs pvcDocs

	err := rejectUnknownPillarKeys("PVC annotation", annotations, map[string]bool{
		configdocs.BackendDocKey:    true,
		configdocs.ProtocolDocKey:   true,
		configdocs.FilesystemDocKey: true,
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
	return docs, nil
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
