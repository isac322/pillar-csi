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

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	pillarv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend/lvm"
	"github.com/bhyoo/pillar-csi/internal/configdocs"
)

// agentConfigBackendsKey is the only top-level key of the agent config file.
const agentConfigBackendsKey = "backends"

// loadAgentConfig reads the agent's backend placement config file (--config)
// and returns its validated backend entries.  The file has the same shape as
// the chart's agent.backends value:
//
//	backends:
//	  - zfs: {volumeType: zvol, pool: hot-data, parentDataset: k8s}
//	  - lvm: {volumeGroup: data-vg, thinPool: thin0}
//
// Each entry is decoded by configdocs.DecodeBackendSpec, the decoder shared
// with PillarStore.spec.backend, so the keys, enums and defaults are the same.
func loadAgentConfig(path string) ([]pillarv1alpha1.BackendSpec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read agent config: %w", err)
	}
	return parseAgentConfig(path, data)
}

// parseAgentConfig decodes and validates an agent config document.  The
// source argument names the document in error messages (the config file path).  Decoding is
// strict: an unknown key anywhere is rejected with its path, and at least one
// backend is required.
func parseAgentConfig(source string, data []byte) ([]pillarv1alpha1.BackendSpec, error) {
	// yaml.v3 rejects duplicate mapping keys instead of keeping the last one.
	var doc map[string]any
	err := configdocs.UnmarshalYAML(data, &doc)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid YAML: %w", source, err)
	}
	for key := range doc {
		if key != agentConfigBackendsKey {
			return nil, fmt.Errorf("%s: unknown field %q (supported: %s)", source, key, agentConfigBackendsKey)
		}
	}

	rawBackends, ok := doc[agentConfigBackendsKey]
	if !ok || rawBackends == nil {
		return nil, fmt.Errorf("%s: backends: at least one backend is required", source)
	}
	entries, ok := rawBackends.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: backends: expected a list, got %T", source, rawBackends)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: backends: at least one backend is required", source)
	}

	specs := make([]pillarv1alpha1.BackendSpec, 0, len(entries))
	for i, entry := range entries {
		entrySource := fmt.Sprintf("%s: backends[%d]", source, i)
		raw, err := yaml.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: re-encode entry: %w", entrySource, err)
		}
		spec, err := configdocs.DecodeBackendSpec(entrySource, string(raw))
		if err != nil {
			return nil, err
		}
		if spec == nil {
			return nil, fmt.Errorf("%s: entry is empty; exactly one of lvm, zfs must be set", entrySource)
		}
		err = validateAgentBackend(entrySource, *spec)
		if err != nil {
			return nil, err
		}
		specs = append(specs, *spec)
	}
	return specs, nil
}

// validateAgentBackend applies the agent-side checks the shared decoder does
// not know about: fields that have no meaning at agent level are refused
// instead of being silently ignored, and placement values the backend would
// misinterpret are rejected at startup.
func validateAgentBackend(source string, spec pillarv1alpha1.BackendSpec) error {
	switch {
	case spec.ZFS != nil:
		if spec.ZFS.Pool == "" {
			return fmt.Errorf("%s: zfs.pool is required", source)
		}
		if len(spec.ZFS.Properties) > 0 {
			return fmt.Errorf("%s: zfs.properties are per-volume settings and are not used by the agent; "+
				"set them on the PillarStore, PillarStorageClass overrides or the PVC backend document", source)
		}
		// A "." or ".." component would place volumes elsewhere (e.g.
		// parentDataset "../k8s" creates in pool "k8s") while the reported
		// layout claims otherwise (issue #113).
		if spec.ZFS.ParentDataset != "" {
			for comp := range strings.SplitSeq(spec.ZFS.ParentDataset, "/") {
				if comp == "." || comp == ".." {
					return fmt.Errorf("%s: zfs.parentDataset %q must be a dataset path inside pool %q "+
						"(no %q components)", source, spec.ZFS.ParentDataset, spec.ZFS.Pool, comp)
				}
			}
		}
	case spec.LVM != nil:
		err := lvm.ValidateVGName(spec.LVM.VolumeGroup)
		if err != nil {
			return fmt.Errorf("%s: lvm.volumeGroup: %w", source, err)
		}
		if spec.LVM.ProvisioningMode == pillarv1alpha1.LVMProvisioningModeThin && spec.LVM.ThinPool == "" {
			return fmt.Errorf("%s: lvm.provisioningMode %q requires lvm.thinPool", source,
				pillarv1alpha1.LVMProvisioningModeThin)
		}
	default:
		return errors.New(source + ": exactly one of lvm, zfs must be set")
	}
	return nil
}
