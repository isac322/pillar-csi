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

package zfs_test

import (
	"testing"

	"github.com/bhyoo/pillar-csi/internal/agent/backend/zfs"
)

func TestDevicePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		pool          string
		parentDataset string
		volumeID      string
		want          string
	}{
		{
			name:          "pool with parent dataset",
			pool:          "hot-data",
			parentDataset: "k8s",
			volumeID:      "hot-data/pvc-abc123",
			want:          "/dev/zvol/hot-data/k8s/pvc-abc123",
		},
		{
			name:          "pool without parent dataset",
			pool:          "tank",
			parentDataset: "",
			volumeID:      "tank/pvc-xyz",
			want:          "/dev/zvol/tank/pvc-xyz",
		},
		{
			name:          "single-letter pool no parent",
			pool:          "z",
			parentDataset: "",
			volumeID:      "z/vol0",
			want:          "/dev/zvol/z/vol0",
		},
		{
			name:          "nested parent dataset",
			pool:          "data",
			parentDataset: "prod/k8s",
			volumeID:      "data/pvc-deep",
			want:          "/dev/zvol/data/prod/k8s/pvc-deep",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := zfs.New(tc.pool, tc.parentDataset)
			got := b.DevicePath(tc.volumeID)
			if got != tc.want {
				t.Errorf("DevicePath(%q) = %q; want %q", tc.volumeID, got, tc.want)
			}
		})
	}
}

// TestDevicePathCustomBase verifies that devZvolBase can be overridden by
// tests that want to exercise the path logic without /dev/zvol on disk.
func TestDevicePathCustomBase(t *testing.T) {
	t.Parallel()

	// Override the package-level base for this test.
	zfs.SetDevZvolBase(t, "/tmp/fakevol")

	b := zfs.New("pool", "ns")
	got := b.DevicePath("pool/pvc-1")
	want := "/tmp/fakevol/pool/ns/pvc-1"
	if got != want {
		t.Errorf("DevicePath with custom base = %q; want %q", got, want)
	}
}
