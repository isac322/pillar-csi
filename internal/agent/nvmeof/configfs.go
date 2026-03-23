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

// Package nvmeof implements NVMe-oF TCP target management via direct configfs
// manipulation.  It never shells out to nvmetcli or nvme-cli; every operation
// is performed by writing to and symlinking within the nvmet configfs tree.
//
// Configfs layout (abbreviated):
//
//	<root>/nvmet/
//	  subsystems/<nqn>/
//	    attr_allow_any_host        "0" or "1"
//	    namespaces/<nsid>/
//	      device_path              path to the block device
//	      enable                   "1" to activate
//	    allowed_hosts/<host-nqn>/  → symlink to hosts/<host-nqn>
//	  hosts/<host-nqn>/
//	  ports/<portid>/
//	    addr_trtype                "tcp"
//	    addr_adrfam                "ipv4"
//	    addr_traddr                bind IP
//	    addr_trsvcid               port number
//	    subsystems/<nqn>/          → symlink to subsystems/<nqn>
package nvmeof

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultConfigfsRoot is the standard kernel configfs mount point used in
	// production.  Pass a different value to NvmetTarget.ConfigfsRoot in tests.
	DefaultConfigfsRoot = "/sys/kernel/config"

	// DefaultPort is the IANA-assigned port for NVMe-oF TCP.
	DefaultPort int32 = 4420
)

// NvmetTarget holds all the parameters required to describe one NVMe-oF TCP
// subsystem entry in configfs.  It is a pure data struct with no embedded
// state; the Apply / Remove methods (added in later sub-ACs) read these
// fields and drive configfs accordingly.
type NvmetTarget struct {
	// ConfigfsRoot is the root of the configfs mount.  Defaults to
	// DefaultConfigfsRoot (/sys/kernel/config) in production; override in
	// unit tests to a temporary directory.
	ConfigfsRoot string

	// SubsystemNQN is the NVMe Qualified Name that uniquely identifies the
	// subsystem, e.g. "nqn.2026-01.io.pillar-csi:pvc-abc123".
	// Must be non-empty.
	SubsystemNQN string

	// NamespaceID is the 1-based namespace identifier within the subsystem.
	// The kernel rejects 0; conventionally the first (and only) namespace is 1.
	NamespaceID uint32

	// DevicePath is the path to the block device that backs this namespace,
	// e.g. "/dev/zvol/tank/pvc-abc123".
	DevicePath string

	// BindAddress is the IP address on which the NVMe-oF TCP port will listen,
	// e.g. "192.168.1.10".  Resolved by the controller from PillarTarget and
	// passed in the MountVolume RPC call — never looked up by the agent itself.
	BindAddress string

	// Port is the TCP port number the target listens on (default: 4420).
	Port int32

	// AllowedHosts is the set of initiator NQNs that are granted access when
	// ACL enforcement is enabled (attr_allow_any_host == 0).
	// An empty slice means no initiator has been granted access yet.
	AllowedHosts []string
}

// nvmetRoot returns the path to the nvmet subtree within configfs, e.g.
// "/sys/kernel/config/nvmet".
func (t *NvmetTarget) nvmetRoot() string {
	root := t.ConfigfsRoot
	if root == "" {
		root = DefaultConfigfsRoot
	}
	return filepath.Join(root, "nvmet")
}

// subsystemDir returns the configfs directory for this subsystem.
func (t *NvmetTarget) subsystemDir() string {
	return filepath.Join(t.nvmetRoot(), "subsystems", t.SubsystemNQN)
}

// namespaceDir returns the configfs directory for this target's namespace.
func (t *NvmetTarget) namespaceDir() string {
	return filepath.Join(t.subsystemDir(), "namespaces", fmt.Sprintf("%d", t.NamespaceID))
}

// hostDir returns the configfs directory for the given host NQN.
func (t *NvmetTarget) hostDir(hostNQN string) string {
	return filepath.Join(t.nvmetRoot(), "hosts", hostNQN)
}

// allowedHostLink returns the path of the symlink that grants hostNQN access
// to the subsystem.
func (t *NvmetTarget) allowedHostLink(hostNQN string) string {
	return filepath.Join(t.subsystemDir(), "allowed_hosts", hostNQN)
}

// portDir returns the configfs directory for the TCP port whose address
// matches this target.  Port IDs are arbitrary; we derive a stable one from
// the bind address and port number so the same port is reused across calls.
// (Actual port-ID selection logic is deferred to Apply.)
func (t *NvmetTarget) portDir(portID uint32) string {
	return filepath.Join(t.nvmetRoot(), "ports", fmt.Sprintf("%d", portID))
}

// portSubsystemLink returns the path of the symlink that attaches the
// subsystem to the port.
func (t *NvmetTarget) portSubsystemLink(portID uint32) string {
	return filepath.Join(t.portDir(portID), "subsystems", t.SubsystemNQN)
}

// ─────────────────────────────────────────────────────────────────────────────
// configfs helper primitives
//
// These wrappers keep the Apply/Remove logic easy to read and centralise
// error-message formatting.  They are unexported because callers outside this
// package never drive configfs directly.
// ─────────────────────────────────────────────────────────────────────────────

// writeFile writes content to the configfs pseudo-file at path, creating or
// truncating it.  configfs pseudo-files are single-valued: each write
// replaces the previous value.
//
// The function is intentionally simple — it does not retry — because configfs
// operations are synchronous kernel calls and transient errors are not expected.
func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("configfs write %q = %q: %w", path, content, err)
	}
	return nil
}

// mkdirAll creates path and all missing parent directories, mirroring
// os.MkdirAll but wrapping the error with the configfs context.
//
// Creating a directory in the nvmet configfs tree causes the kernel to
// instantiate the corresponding object (subsystem, namespace, host, port).
func mkdirAll(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("configfs mkdir %q: %w", path, err)
	}
	return nil
}

// symlink creates a symbolic link newname → oldname.  In the nvmet configfs
// tree, symlinks are used to:
//   - attach a subsystem to a port   (ports/<id>/subsystems/<nqn> → ../../subsystems/<nqn>)
//   - grant a host access to a sub   (subsystems/<nqn>/allowed_hosts/<host> → ../../../hosts/<host>)
//
// If newname already exists as a symlink pointing to oldname the function
// returns nil (idempotent).  Any other pre-existing path at newname is
// treated as an error to avoid silently overwriting unrelated configfs state.
func symlink(oldname, newname string) error {
	existing, err := os.Readlink(newname)
	switch {
	case err == nil:
		// newname exists and is a symlink.
		if existing == oldname {
			return nil // already correct — idempotent success
		}
		return fmt.Errorf("configfs symlink %q → %q: already points to %q", newname, oldname, existing)
	case os.IsNotExist(err):
		// newname does not exist — create it.
		if linkErr := os.Symlink(oldname, newname); linkErr != nil {
			return fmt.Errorf("configfs symlink %q → %q: %w", newname, oldname, linkErr)
		}
		return nil
	default:
		// Readlink returned a non-ENOENT error (e.g. permission denied).
		return fmt.Errorf("configfs symlink check %q: %w", newname, err)
	}
}

// removeSymlink removes the symbolic link at path.  It is a no-op (idempotent)
// when path does not exist.  Returns an error if path exists but is not a
// symlink, to prevent accidental removal of real configfs directories.
func removeSymlink(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil // already gone — idempotent success
	}
	if err != nil {
		return fmt.Errorf("configfs symlink stat %q: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("configfs removeSymlink %q: not a symlink (mode=%s)", path, fi.Mode())
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("configfs removeSymlink %q: %w", path, err)
	}
	return nil
}

// removeDir removes a single empty configfs directory at path.  The kernel
// destroys the associated object when the directory is removed.
// It is a no-op when path does not exist (idempotent).
func removeDir(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("configfs rmdir %q: %w", path, err)
	}
	return nil
}
