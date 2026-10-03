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
	"context"
	"fmt"
	"strconv"
	"strings"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

type nativeDatasetIdentity struct {
	typeName, guid string
	exists         bool
}

// readNativeDatasetIdentity reads only immutable native identity. It never
// checks quota, mounts, ownership, or performs a source mutation. Exists is
// false for a missing dataset.
func readNativeDatasetIdentity(
	ctx context.Context, exec executor, dataset string,
) (nativeDatasetIdentity, error) {
	out, runErr := exec.run(
		ctx,
		datasetFSType,
		zfsGetOperation,
		datasetMachineOutput,
		"-o",
		"property,value",
		"type,guid",
		dataset,
	)
	if runErr != nil {
		if isNotExistOutput(out) {
			return nativeDatasetIdentity{}, nil
		}
		return nativeDatasetIdentity{}, fmt.Errorf("zfs get native identity %q: %w\n%s",
			dataset, runErr, strings.TrimSpace(string(out)))
	}
	identity := nativeDatasetIdentity{exists: true}
	seenType, seenGUID := false, false
	for line := range strings.Lines(strings.TrimSpace(string(out))) {
		property, value, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			return identity, fmt.Errorf("zfs: unexpected native identity output for %q: %q", dataset, line)
		}
		switch property {
		case datasetTypeProperty:
			identity.typeName, seenType = strings.TrimSpace(value), true
		case datasetGUIDProperty:
			n, parseErr := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if parseErr != nil || n == 0 {
				return identity, fmt.Errorf("zfs: invalid GUID %q for dataset %q", value, dataset)
			}
			identity.guid, seenGUID = strconv.FormatUint(n, 10), true
		}
	}
	if !seenType || !seenGUID {
		return identity, fmt.Errorf("zfs dataset %q has incomplete native identity", dataset)
	}
	return identity, nil
}

// ExistingFilesystemIdentity returns the native identity at a legacy zvol
// backend's resolved dataset name. A missing zvol or an actual zvol returns
// nil, nil; a filesystem at that same name is returned so callers can prevent
// typed-zvol lifecycle operations from destroying an adopted filesystem.
func (z *Backend) ExistingFilesystemIdentity(
	ctx context.Context, volumeID string,
) (*agentv1.FilesystemAdoption, error) {
	dataset := z.datasetName(volumeID)
	identity, err := readNativeDatasetIdentity(ctx, z.exec, dataset)
	if err != nil || !identity.exists || identity.typeName != datasetFilesystem {
		return nil, err
	}
	return nativeFilesystemAdoption(dataset, identity.guid), nil
}
