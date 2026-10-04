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
	"fmt"
	"runtime"
)

// CheckMountHealth is only implemented on Linux, the node plugin's platform:
// the probe relies on Linux O_TMPFILE semantics.  Returning an error rather
// than nil keeps the "no silent success" contract — callers on non-Linux
// platforms (tests aside) cannot claim health.
func (*KubeMounter) CheckMountHealth(target string) error {
	return fmt.Errorf("CheckMountHealth %s: unsupported on %s", target, runtime.GOOS)
}

// HasOtherMounts is only implemented on Linux: it parses /proc/self/mountinfo.
func (*KubeMounter) HasOtherMounts(target string) (bool, error) {
	return false, fmt.Errorf("HasOtherMounts %s: unsupported on %s", target, runtime.GOOS)
}

// MountSource is only implemented on Linux: it parses /proc/self/mountinfo.
func (*KubeMounter) MountSource(target string) (string, error) {
	return "", fmt.Errorf("MountSource %s: unsupported on %s", target, runtime.GOOS)
}

// MountEntryExists is only implemented on Linux: it parses /proc/self/mountinfo.
func (*KubeMounter) MountEntryExists(target string) (bool, error) {
	return false, fmt.Errorf("MountEntryExists %s: unsupported on %s", target, runtime.GOOS)
}
