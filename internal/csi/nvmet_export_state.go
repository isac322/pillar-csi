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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// NVMe-oF export state — the storage-node half of the local attach fence
// ─────────────────────────────────────────────────────────────────────────────
//
// A local stage and the agent re-enabling the network export exclude each
// other in Dekker style:
//
//   - node: create the exclusive device-mapper claim, THEN read the nvmet
//     namespace enable state of the volume's subsystem;
//   - agent: write enable=1, THEN probe the backend device for an exclusive
//     holder (and roll back to enable=0 when one is found).
//
// Both sides write before they read kernel state the other side writes, so at
// least one of them observes the other and backs off: the data is never
// served to remote initiators while the storage node has it mounted.

// DefaultNvmetConfigfsRoot is the host configfs directory of the kernel NVMe
// target.  The node container sees it through the host /sys mount.
const DefaultNvmetConfigfsRoot = "/sys/kernel/config/nvmet"

// WithNvmetConfigfsRoot replaces the nvmet configfs root read by the local
// attach export check and returns the receiver.  Tests point it at a
// temporary directory laid out like /sys/kernel/config/nvmet.
func (n *NodeServer) WithNvmetConfigfsRoot(root string) *NodeServer {
	n.nvmetRoot = root
	return n
}

// nvmetConfigfsRoot returns the configured nvmet configfs root, defaulting
// to DefaultNvmetConfigfsRoot.
func (n *NodeServer) nvmetConfigfsRoot() string {
	if n.nvmetRoot == "" {
		return DefaultNvmetConfigfsRoot
	}
	return n.nvmetRoot
}

// enabledNvmetNamespaces returns the IDs of the namespaces of NVMe-oF
// subsystem nqn whose enable attribute reads "1" under the nvmet configfs
// root.
//
//   - root absent: the export state cannot be verified — error.
//   - subsystem (or its namespaces directory) absent: no export can serve
//     I/O — empty result.  A later enable is re-checked by the agent.
//   - a namespace removed while scanning is skipped; any other read error is
//     returned.
func enabledNvmetNamespaces(root, nqn string) ([]string, error) {
	if nqn == "" || nqn == "." || nqn == ".." || strings.ContainsRune(nqn, filepath.Separator) {
		return nil, fmt.Errorf("invalid NVMe-oF subsystem NQN %q", nqn)
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("nvmet configfs %s is not available (nvmet not loaded or configfs not "+
				"mounted in the node container); cannot verify the export of subsystem %s: %w", root, nqn, err)
		}
		return nil, fmt.Errorf("stat nvmet configfs %s: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("nvmet configfs %s is not a directory", root)
	}

	nsRoot := filepath.Join(root, "subsystems", nqn, "namespaces")
	entries, err := os.ReadDir(nsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read nvmet namespaces %s: %w", nsRoot, err)
	}

	var enabled []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		enableFile := filepath.Join(nsRoot, e.Name(), "enable")
		raw, readErr := os.ReadFile(enableFile) //nolint:gosec // G304: configfs path under a fixed root
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				continue // namespace removed while scanning
			}
			return nil, fmt.Errorf("read %s: %w", enableFile, readErr)
		}
		if strings.TrimSpace(string(raw)) == "1" {
			enabled = append(enabled, e.Name())
		}
	}
	return enabled, nil
}
