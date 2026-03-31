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
	"os"
	"strings"
)

// hostNQNFile is the standard path where the Linux NVMe driver stores the
// host NQN assigned to this machine.  It is written by nvme-cli on first
// boot (or by the OS vendor) and is stable across reboots.
const hostNQNFile = "/etc/nvme/hostnqn"

// ReadHostNQN reads the NVMe host NQN for this node from the well-known
// system file (/etc/nvme/hostnqn).  The returned string is trimmed of
// whitespace.
//
// Returns an error if the file cannot be read or is empty after trimming,
// which typically means the nvme-cli package has not been installed or the
// NVMe kernel module has never been loaded on this node.
func ReadHostNQN() (string, error) {
	return readHostNQNFrom(hostNQNFile)
}

// readHostNQNFrom is the testable inner implementation that accepts an
// arbitrary file path instead of the hard-coded system path.
func readHostNQNFrom(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read host NQN from %s: %w", path, err)
	}
	nqn := strings.TrimSpace(string(data))
	if nqn == "" {
		return "", fmt.Errorf("host NQN file %s is empty", path)
	}
	return nqn, nil
}
