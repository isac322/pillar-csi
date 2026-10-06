//go:build !linux

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
	"context"
	"fmt"
	"runtime"
)

// FormatAndMount is only implemented on Linux, the node plugin's platform:
// k8s.io/utils/mount cannot detect existing filesystems elsewhere.
func (*KubeMounter) FormatAndMount(_ context.Context, source, target, fsType string, _, _ []string) error {
	return fmt.Errorf("FormatAndMount %s → %s as %s: unsupported on %s", source, target, fsType, runtime.GOOS)
}

// MountExisting is only implemented on Linux for the same reason as
// FormatAndMount: probing the existing filesystem signature needs blkid.
func (*KubeMounter) MountExisting(_ context.Context, source, target, fsType string, _ []string) error {
	return fmt.Errorf("MountExisting %s → %s as %s: unsupported on %s", source, target, fsType, runtime.GOOS)
}
