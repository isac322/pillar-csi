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

package nvmeof

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ExportedSubsystem represents a single NVMe-oF subsystem as read from the
// kernel configfs tree.  It reflects actual kernel state, not
// controller-desired state.
//
// Populated by ListExports.
type ExportedSubsystem struct {
	// NQN is the NVMe Qualified Name of the subsystem, read from the
	// attr_name pseudo-file.  Falls back to the directory name when attr_name
	// is absent (e.g. in test environments that simulate configfs with a
	// plain tmpdir).
	NQN string

	// NamespaceDevicePaths maps each namespace ID (1-based uint32) to the
	// backing block-device path read from
	// namespaces/<nsid>/device_path.
	NamespaceDevicePaths map[uint32]string

	// AllowedHosts is the sorted list of initiator NQNs that have been
	// granted access to this subsystem.  Each entry corresponds to a symlink
	// (or directory) under allowed_hosts/ — the entry name is the host NQN.
	// Nil when the allowed_hosts directory is absent or empty.
	AllowedHosts []string
}

// ListExports scans the kernel configfs nvmet subsystems tree and returns one
// ExportedSubsystem per subsystem directory found.  It reads the following
// configfs attributes for each subsystem:
//
//   - attr_name            → ExportedSubsystem.NQN
//   - namespaces/<id>/device_path → ExportedSubsystem.NamespaceDevicePaths
//   - allowed_hosts/*      → ExportedSubsystem.AllowedHosts
//
// If configfsRoot is empty, DefaultConfigfsRoot (/sys/kernel/config) is used.
// If the subsystems directory does not exist (nvmet not loaded or no
// subsystems configured), a nil slice and a nil error are returned.
//
// The returned slice is sorted by NQN for deterministic ordering.
func ListExports(configfsRoot string) ([]ExportedSubsystem, error) {
	if configfsRoot == "" {
		configfsRoot = DefaultConfigfsRoot
	}
	subsDir := filepath.Join(configfsRoot, "nvmet", "subsystems")

	entries, err := os.ReadDir(subsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ListExports: read subsystems dir %q: %w", subsDir, err)
	}

	var result []ExportedSubsystem
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		subPath := filepath.Join(subsDir, entry.Name())
		sub, scanErr := scanSubsystem(subPath, entry.Name())
		if scanErr != nil {
			return nil, fmt.Errorf("ListExports: subsystem %q: %w", entry.Name(), scanErr)
		}
		result = append(result, sub)
	}

	// Sort by NQN so callers get a stable, deterministic order.
	sort.Slice(result, func(i, j int) bool {
		return result[i].NQN < result[j].NQN
	})
	return result, nil
}

// scanSubsystem reads the configfs attributes of one subsystem directory and
// returns a populated ExportedSubsystem.
func scanSubsystem(subPath, dirName string) (ExportedSubsystem, error) {
	nqn, err := readAttrName(subPath, dirName)
	if err != nil {
		return ExportedSubsystem{}, err
	}

	nsPaths, err := readNamespaceDevicePaths(subPath)
	if err != nil {
		return ExportedSubsystem{}, fmt.Errorf("namespaces: %w", err)
	}

	hosts, err := readAllowedHosts(subPath)
	if err != nil {
		return ExportedSubsystem{}, fmt.Errorf("allowed_hosts: %w", err)
	}

	return ExportedSubsystem{
		NQN:                  nqn,
		NamespaceDevicePaths: nsPaths,
		AllowedHosts:         hosts,
	}, nil
}

// readAttrName reads the NQN from the attr_name pseudo-file in subPath.
// If the file does not exist (e.g. in a test tmpdir that mimics configfs
// without creating every pseudo-file), dirName is used as the NQN instead —
// which is identical to the NQN on the real kernel because the kernel names
// the directory after the NQN.
func readAttrName(subPath, dirName string) (string, error) {
	attrFile := filepath.Join(subPath, "attr_name")
	data, err := os.ReadFile(attrFile) //nolint:gosec // G304: path is constructed from a configfs root under our control.
	if err != nil {
		if os.IsNotExist(err) {
			return dirName, nil
		}
		return "", fmt.Errorf("read attr_name: %w", err)
	}
	// Configfs pseudo-files often include a trailing newline; strip it.
	return strings.TrimRight(string(data), "\n"), nil
}

// readNamespaceDevicePaths scans the namespaces/ subdirectory of subPath and
// returns a map from namespace ID to device_path content.  Entries whose
// names are not valid uint32 integers are silently skipped (defensive against
// unexpected pseudo-files).  Returns nil when the namespaces directory is
// absent.
func readNamespaceDevicePaths(subPath string) (map[uint32]string, error) {
	nsDir := filepath.Join(subPath, "namespaces")
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[uint32]string{}, nil
		}
		return nil, fmt.Errorf("read namespaces dir: %w", err)
	}

	result := make(map[uint32]string, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		nsErr := readOneNamespace(nsDir, entry.Name(), result)
		if nsErr != nil {
			return nil, nsErr
		}
	}
	return result, nil
}

// readOneNamespace reads the device_path for a single namespace entry and
// inserts it into result.  Non-numeric entry names and missing device_path
// files are silently skipped.
func readOneNamespace(nsDir, entryName string, result map[uint32]string) error {
	nsid64, parseErr := strconv.ParseUint(entryName, 10, 32)
	if parseErr != nil {
		// Non-numeric directory name — skip silently.
		return nil //nolint:nilerr // intentional: non-numeric names are not namespace IDs.
	}
	devPathFile := filepath.Join(nsDir, entryName, "device_path")
	data, readErr := os.ReadFile(devPathFile) //nolint:gosec // G304: path is within configfs root.
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil // namespace dir exists but device_path not yet written
		}
		return fmt.Errorf("namespace %s device_path: %w", entryName, readErr)
	}
	// ParseUint with bitSize=32 guarantees nsid64 fits in uint32 (no overflow).
	result[uint32(nsid64)] = strings.TrimRight(string(data), "\n")
	return nil
}

// readAllowedHosts scans the allowed_hosts/ subdirectory of subPath and
// returns a sorted slice of host NQNs derived from the entry names (each
// entry is a symlink named after the initiator NQN).  Returns nil when the
// allowed_hosts directory is absent or empty.
func readAllowedHosts(subPath string) ([]string, error) {
	ahDir := filepath.Join(subPath, "allowed_hosts")
	entries, err := os.ReadDir(ahDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read allowed_hosts dir: %w", err)
	}

	if len(entries) == 0 {
		return nil, nil
	}

	hosts := make([]string, 0, len(entries))
	for _, entry := range entries {
		hosts = append(hosts, entry.Name())
	}
	// Sort for deterministic output.
	sort.Strings(hosts)
	return hosts, nil
}
