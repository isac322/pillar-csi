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
// iSCSI export state — the storage-node half of the local attach fence (LIO)
// ─────────────────────────────────────────────────────────────────────────────
//
// The iSCSI counterpart of nvmet_export_state.go.  While a volume is locally
// attached the agent keeps its LIO target without LUN 0: it removes
// <root>/iscsi/<targetIQN>/tpgt_1/lun/lun_0 (and the iblock backstore), and
// re-creating that LUN is how it re-enables the network export.  The node
// creates its exclusive device-mapper claim first and then checks that the
// LUN is gone, so the Dekker exclusion of the NVMe-oF fence carries over.

// DefaultLIOConfigfsRoot is the host configfs directory of the kernel LIO
// target core.  The node container sees it through the host /sys mount.
const DefaultLIOConfigfsRoot = "/sys/kernel/config/target"

// lioExportTPG and lioExportLUN name the single TPG and LUN the agent
// creates for every iSCSI target.
const (
	lioExportTPG = "tpgt_1"
	lioExportLUN = "lun_0"
)

// WithLIOConfigfsRoot replaces the LIO configfs root read by the local attach
// export check of iSCSI volumes and returns the receiver.  Tests point it at
// a temporary directory laid out like /sys/kernel/config/target.
func (n *NodeServer) WithLIOConfigfsRoot(root string) *NodeServer {
	n.lioRoot = root
	return n
}

// lioConfigfsRoot returns the configured LIO configfs root, defaulting to
// DefaultLIOConfigfsRoot.
func (n *NodeServer) lioConfigfsRoot() string {
	if n.lioRoot == "" {
		return DefaultLIOConfigfsRoot
	}
	return n.lioRoot
}

// enabledLIOLUNs returns the exported LUN directories ("lun_0") of the iSCSI
// target iqn under the LIO configfs root.
//
//   - root absent: the export state cannot be verified — error.
//   - target directory absent: no export can serve I/O — empty result.
//   - tpgt_1/lun/lun_0 present: the export is still active — ["lun_0"].
//   - tpgt_1/lun/lun_0 absent: the export is fenced — empty result.
//   - any other stat error is returned, never treated as fenced.
func enabledLIOLUNs(root, iqn string) ([]string, error) {
	if iqn == "" || iqn == "." || iqn == ".." || strings.ContainsRune(iqn, filepath.Separator) {
		return nil, fmt.Errorf("invalid iSCSI target IQN %q", iqn)
	}

	rootInfo, err := os.Stat(root) // #nosec G703 -- driver-configured configfs root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("LIO configfs %s is not available (target_core_mod not loaded or configfs "+
				"not mounted in the node container); cannot verify the export of target %s: %w", root, iqn, err)
		}
		return nil, fmt.Errorf("stat LIO configfs %s: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("LIO configfs %s is not a directory", root)
	}

	targetDir := filepath.Join(root, "iscsi", iqn)
	_, err = os.Stat(targetDir) // #nosec G703 -- IQN validated above to be a single path element
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat LIO target %s: %w", targetDir, err)
	}

	lunDir := filepath.Join(targetDir, lioExportTPG, "lun", lioExportLUN)
	_, err = os.Stat(lunDir) // #nosec G703 -- IQN validated above to be a single path element
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat LIO LUN %s: %w", lunDir, err)
	}
	return []string{lioExportLUN}, nil
}
