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

// errTrimUnsupportedOS reports that the platform has no FITRIM ioctl.
var errTrimUnsupportedOS = errors.New("periodic filesystem trim requires the Linux FITRIM ioctl")

// platformTrimFuncs always fails off Linux.  It is a variable so that
// StartTrimmer's error check is not a constant condition on this platform.
var platformTrimFuncs = func() (fitrimFunc, fsSizeFunc, error) {
	return nil, nil, fmt.Errorf("%w; running on %s", errTrimUnsupportedOS, runtime.GOOS)
}
