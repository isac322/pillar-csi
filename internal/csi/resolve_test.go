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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// TestResolveFilesystem_GeneratedClassMountOptions verifies a generated class
// resolves an explicit mount list even when the live binding sets none, so
// the node applies no stale StorageClass mountOptions from the capability.
func TestResolveFilesystem_GeneratedClassMountOptions(t *testing.T) {
	t.Parallel()

	fs, err := resolveFilesystem(map[string]string{paramBinding: "b"}, nil, nil)
	if err != nil {
		t.Fatalf("resolveFilesystem: %v", err)
	}
	volCtx := map[string]string{}
	filesystemVolumeContext(fs, volCtx)
	flags, err := resolveMountFlags(volCtx, &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: []string{"noatime"}}},
	})
	if err != nil {
		t.Fatalf("resolveMountFlags: %v", err)
	}
	if len(flags) != 0 {
		t.Errorf("mount flags = %v, want none (stale StorageClass mountOptions)", flags)
	}

	hand, err := resolveFilesystem(map[string]string{paramStoreRef: "s", paramProtocolRef: "p"}, nil, nil)
	if err != nil {
		t.Fatalf("resolveFilesystem: %v", err)
	}
	if hand.MountOptions != nil {
		t.Errorf("hand-written class mountOptions = %v, want nil (StorageClass mountOptions apply)", *hand.MountOptions)
	}
}

// TestResolveFilesystem_FSTypeSources pins where the resolved fsType comes
// from: a generated class follows the live binding (its StorageClass fstype
// is only a possibly stale copy), a hand-written class uses its fstype
// parameter, and the PVC document wins over both.
func TestResolveFilesystem_FSTypeSources(t *testing.T) {
	t.Parallel()

	xfs := &v1alpha1.FilesystemConfig{FSType: xfsFsType}
	ext4 := &v1alpha1.FilesystemConfig{FSType: defaultFsType}
	tests := []struct {
		name     string
		scParams map[string]string
		classFS  *v1alpha1.FilesystemConfig
		pvcFS    *v1alpha1.FilesystemConfig
		want     string
	}{
		{
			name:     "generated class ignores stale fstype when binding has no filesystem",
			scParams: map[string]string{paramBinding: "b", paramFSTypeSC: xfsFsType},
			want:     defaultFsType,
		},
		{
			name:     "generated class follows live binding",
			scParams: map[string]string{paramBinding: "b", paramFSTypeSC: xfsFsType},
			classFS:  ext4,
			want:     defaultFsType,
		},
		{
			name:     "hand-written class uses fstype parameter",
			scParams: map[string]string{paramStoreRef: "s", paramProtocolRef: "p", paramFSTypeSC: xfsFsType},
			want:     xfsFsType,
		},
		{
			name:     "PVC document wins",
			scParams: map[string]string{paramBinding: "b"},
			classFS:  ext4,
			pvcFS:    xfs,
			want:     xfsFsType,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs, err := resolveFilesystem(tt.scParams, tt.classFS, tt.pvcFS)
			if err != nil {
				t.Fatalf("resolveFilesystem: %v", err)
			}
			if fs.FSType != tt.want {
				t.Errorf("fsType = %q, want %q", fs.FSType, tt.want)
			}
		})
	}
}
