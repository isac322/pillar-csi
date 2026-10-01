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
	"errors"
	"fmt"
	"runtime"
)

// errNVMeAdminUnsupportedOS reports that the platform has no NVMe admin
// passthrough ioctl.
var errNVMeAdminUnsupportedOS = errors.New("reading the NVMe controller MDTS requires the Linux NVMe admin ioctl")

// ReadNVMeControllerMDTS always fails off Linux.
func ReadNVMeControllerMDTS(ctrl string) (uint8, error) {
	return 0, fmt.Errorf("%w: controller %s on %s", errNVMeAdminUnsupportedOS, ctrl, runtime.GOOS)
}
