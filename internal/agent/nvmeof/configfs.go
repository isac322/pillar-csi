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
	"strings"
	"sync"
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

	// ACLEnabled controls whether host NQN-based access control is enforced.
	// When true the subsystem is created with attr_allow_any_host = 0, so only
	// explicitly allowed initiators can connect.
	// When false (the default) attr_allow_any_host = 1 is written, permitting
	// any initiator to connect without an ACL entry.
	ACLEnabled bool
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

// Configfs helper primitives.
//
// These wrappers keep the Apply/Remove logic easy to read and centralize
// error-message formatting.  They are unexported because callers outside this
// package never drive configfs directly.

// writeFileLocks serializes writes to the same configfs path within this
// process so immediate read-back verification observes the just-written value
// instead of a sibling goroutine's concurrent truncate/write cycle on regular
// test filesystems.
var writeFileLocks sync.Map

func writeFileLock(path string) *sync.Mutex {
	actual, _ := writeFileLocks.LoadOrStore(path, &sync.Mutex{})
	lock, ok := actual.(*sync.Mutex)
	if !ok {
		panic(fmt.Sprintf("writeFileLocks stored non-mutex for %q", path))
	}
	return lock
}

// writeFile writes content to the configfs pseudo-file at path, creating or
// truncating it.
// Configfs pseudo-files are single-valued: each write replaces the previous value.
//
// The function is intentionally simple — it does not retry — because configfs
// operations are synchronous kernel calls and transient errors are not expected.
func writeFile(path, content string) error {
	lock := writeFileLock(path)
	lock.Lock()
	defer lock.Unlock()

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		return fmt.Errorf("configfs write %q = %q: %w", path, content, err)
	}

	//nolint:gosec // G304: path is constructed from a configfs root under controller control.
	readback, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("configfs verify %q after write %q: %w", path, content, err)
	}

	want := strings.TrimSpace(content)
	got := strings.TrimSpace(string(readback))
	if got != want {
		return fmt.Errorf(
			"configfs verify %q after write %q: want %q, got %q (raw readback %q)",
			path,
			content,
			want,
			got,
			string(readback),
		)
	}

	return nil
}

// mkdirAll creates path and all missing parent directories, mirroring
// os.MkdirAll but wrapping the error with the configfs context.
//
// Creating a directory in the nvmet configfs tree causes the kernel to
// instantiate the corresponding object (subsystem, namespace, host, port).
func mkdirAll(path string) error {
	err := os.MkdirAll(path, 0o750)
	if err != nil {
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
		linkErr := os.Symlink(oldname, newname)
		if linkErr != nil {
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
	err = os.Remove(path)
	if err != nil {
		return fmt.Errorf("configfs removeSymlink %q: %w", path, err)
	}
	return nil
}

// removeDir removes a single empty configfs directory at path.  The kernel
// destroys the associated object when the directory is removed.
// It is a no-op when path does not exist (idempotent).
func removeDir(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("configfs rmdir %q: %w", path, err)
	}
	return nil
}

// bestEffort accepts an error value and discards it.  It is used to silence
// errcheck for intentionally best-effort cleanup operations where failure is
// expected and acceptable (e.g. removing files on a regular filesystem in tests).
func bestEffort(_ error) {}

// Port ID allocation.
//
// NVMe-oF configfs port IDs are arbitrary uint16 values.
// We derive a stable ID from a hash of the bind address + port so that:
//   - the same (address, port) pair always gets the same port ID
//   - different pairs are unlikely to collide (FNV-1a hash mod 65535)
//   - the ID is reused across Apply calls (idempotent)

// stablePortID derives a deterministic configfs port ID from the bind address
// and TCP port.  The result is in [1, 65535] because the kernel rejects 0.
func stablePortID(addr string, port int32) uint32 {
	// FNV-1a 32-bit hash for simplicity — collisions are acceptable because
	// a single storage node typically has O(1) ports.
	var h uint32 = 2166136261
	for i := range len(addr) {
		h ^= uint32(addr[i])
		h *= 16777619
	}
	h ^= uint32(port) //nolint:gosec // G115: port in [1,65535]; overflow is intentional for FNV hash.
	h *= 16777619
	id := h%65535 + 1 // [1, 65535]
	return id
}

// Subsystem and namespace management functions.

// createSubsystem creates the configfs subsystem directory for this target and
// configures its host-access policy.  Specifically it:
//
//  1. Creates <nvmetRoot>/subsystems/<nqn>/ (the kernel instantiates the
//     NVMe subsystem object when the directory appears).
//  2. Writes "0" or "1" to attr_allow_any_host depending on ACLEnabled:
//     - ACLEnabled == false → "1" (any initiator may connect; no ACL check)
//     - ACLEnabled == true  → "0" (only explicitly allowed initiators)
//
// The operation is idempotent: if the directory already exists the mkdir is a
// no-op; configfs pseudo-files accept repeated identical writes.
func (t *NvmetTarget) createSubsystem() error {
	subDir := t.subsystemDir()
	err := mkdirAll(subDir)
	if err != nil {
		return fmt.Errorf("createSubsystem %q: %w", t.SubsystemNQN, err)
	}
	allowAnyHost := "1"
	if t.ACLEnabled {
		allowAnyHost = "0"
	}
	attrPath := filepath.Join(subDir, "attr_allow_any_host")
	err = writeFile(attrPath, allowAnyHost)
	if err != nil {
		return fmt.Errorf("createSubsystem %q: %w", t.SubsystemNQN, err)
	}
	return nil
}

// createNamespace creates the configfs namespace directory for this target and
// activates it against the backing block device.  Specifically it:
//
//  1. Creates <subsystemDir>/namespaces/<nsid>/ (the kernel instantiates the
//     namespace object when the directory appears).
//  2. Writes t.DevicePath to the device_path pseudo-file so the kernel knows
//     which block device backs this namespace.
//  3. Writes "1" to enable to activate the namespace; the kernel will begin
//     accepting I/O after this write.
//
// createNamespace must be called after createSubsystem because the namespace
// directory lives inside the subsystem directory.
//
// The operation is idempotent: repeated calls with the same parameters produce
// the same configfs state.
func (t *NvmetTarget) createNamespace() error {
	nsDir := t.namespaceDir()
	err := mkdirAll(nsDir)
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	devPath := filepath.Join(nsDir, "device_path")
	err = writeFile(devPath, t.DevicePath)
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	enablePath := filepath.Join(nsDir, "enable")
	err = writeFile(enablePath, "1")
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return nil
}

// ResizeNamespace toggles the namespace enable flag (0 → 1) to force the
// kernel to re-read the backing block device size.  Call this after expanding
// the backend volume (e.g. lvextend, zfs set volsize) so that NVMe-oF
// initiators see the new capacity without reconnecting.
func (t *NvmetTarget) ResizeNamespace() error {
	enablePath := filepath.Join(t.namespaceDir(), "enable")
	err := writeFile(enablePath, "0")
	if err != nil {
		return fmt.Errorf("ResizeNamespace %q ns=%d disable: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	err = writeFile(enablePath, "1")
	if err != nil {
		return fmt.Errorf("ResizeNamespace %q ns=%d re-enable: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return nil
}

// createPort creates the configfs port directory for this target's bind
// address and TCP port, then configures the transport attributes.
//
// The port ID is derived deterministically from (BindAddress, Port) via
// stablePortID so the same port directory is reused across calls.
//
// After the port directory is created the function writes:
//   - addr_trtype  = "tcp"
//   - addr_adrfam  = "ipv4"  (TODO: detect IPv6 from BindAddress)
//   - addr_traddr  = <BindAddress>
//   - addr_trsvcid = <Port>
//
// The operation is idempotent: repeated calls with the same parameters
// overwrite the pseudo-files with the same values.
func (t *NvmetTarget) createPort() (uint32, error) {
	port := t.Port
	if port == 0 {
		port = DefaultPort
	}
	portID := stablePortID(t.BindAddress, port)
	pDir := t.portDir(portID)

	err := mkdirAll(pDir)
	if err != nil {
		return 0, fmt.Errorf("createPort %s:%d: %w", t.BindAddress, port, err)
	}

	attrs := map[string]string{
		"addr_trtype":  "tcp",
		"addr_adrfam":  "ipv4",
		"addr_traddr":  t.BindAddress,
		"addr_trsvcid": fmt.Sprintf("%d", port),
	}
	for attr, val := range attrs {
		err = writeFile(filepath.Join(pDir, attr), val)
		if err != nil {
			return 0, fmt.Errorf("createPort %s:%d attr %s: %w", t.BindAddress, port, attr, err)
		}
	}
	return portID, nil
}

// linkSubsystemToPort creates a symlink in the port's subsystems/ directory
// that points to the subsystem directory, activating the subsystem on that
// port.  This is the last step in Apply — once the symlink exists, the kernel
// starts accepting NVMe-oF TCP connections.
func (t *NvmetTarget) linkSubsystemToPort(portID uint32) error {
	linkPath := t.portSubsystemLink(portID)
	target := t.subsystemDir()

	// Ensure the ports/<id>/subsystems/ parent directory exists.
	err := mkdirAll(filepath.Dir(linkPath))
	if err != nil {
		return fmt.Errorf("linkSubsystemToPort mkdir: %w", err)
	}

	return symlink(target, linkPath)
}

// Apply and Remove implement the full target lifecycle.

// Apply creates the complete NVMe-oF TCP target entry in configfs.  The steps
// are executed in dependency order:
//
//  1. Create subsystem (sets allow_any_host based on AllowedHosts)
//  2. Create namespace (device_path + enable)
//  3. Create port (transport attributes)
//  4. Link subsystem to port (activates the target)
//  5. If AllowedHosts is non-empty: create host entries and ACL symlinks,
//     then set attr_allow_any_host = 0
//
// Every step is idempotent, so Apply can be called repeatedly to converge to
// the desired state (useful for ReconcileState after reboot).
func (t *NvmetTarget) Apply() error {
	// 1. Subsystem — initially allow any host; tightened in step 5 if ACL needed.
	err := t.createSubsystem()
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}

	// 2. Namespace
	err = t.createNamespace()
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}

	// 3. Port
	portID, err := t.createPort()
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}

	// 4. Link subsystem → port
	err = t.linkSubsystemToPort(portID)
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}

	// 5. ACL: if AllowedHosts is set, add each host and disable allow_any_host.
	if len(t.AllowedHosts) > 0 {
		for _, host := range t.AllowedHosts {
			err = t.AllowHost(host)
			if err != nil {
				return fmt.Errorf("Apply: %w", err)
			}
		}
		attrPath := filepath.Join(t.subsystemDir(), "attr_allow_any_host")
		err = writeFile(attrPath, "0")
		if err != nil {
			return fmt.Errorf("Apply: disable allow_any_host: %w", err)
		}
	}

	return nil
}

// scanAndRemovePortLinks scans <nvmetRoot>/ports/ for any port entries and
// removes the subsystem symlink from each port's subsystems/ directory.
// If the ports directory does not exist, the function returns nil.
func (t *NvmetTarget) scanAndRemovePortLinks() error {
	portsDir := filepath.Join(t.nvmetRoot(), "ports")
	entries, err := os.ReadDir(portsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("scanAndRemovePortLinks: read ports dir: %w", err)
	}
	for _, entry := range entries {
		linkPath := filepath.Join(portsDir, entry.Name(), "subsystems", t.SubsystemNQN)
		err = removeSymlink(linkPath)
		if err != nil {
			return fmt.Errorf("scanAndRemovePortLinks: port %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// Remove tears down the NVMe-oF TCP target entry in configfs.  The steps are
// executed in reverse dependency order:
//
//  1. Unlink subsystem from all ports (scans ports/ directory)
//  2. Disable namespace (write "0" to enable)
//  3. Remove namespace directory
//  4. Remove allowed_hosts symlinks
//  5. Remove subsystem directory
//
// Every step is idempotent — Remove can be called on an already-removed target
// without error.
func (t *NvmetTarget) Remove() error {
	// 1. Unlink subsystem from all ports.
	err := t.scanAndRemovePortLinks()
	if err != nil {
		return fmt.Errorf("Remove: %w", err)
	}

	// 2-3. Disable + remove namespace.
	nsDir := t.namespaceDir()
	enablePath := filepath.Join(nsDir, "enable")
	// Write "0" to disable — ignore error if namespace doesn't exist.
	bestEffort(writeFile(enablePath, "0"))
	// On real configfs the kernel removes pseudo-files when the directory is
	// removed; on a regular filesystem (tests) we must clean them up manually.
	bestEffort(os.Remove(filepath.Join(nsDir, "device_path")))
	bestEffort(os.Remove(enablePath))
	err = removeDir(nsDir)
	if err != nil {
		return fmt.Errorf("Remove: namespace dir: %w", err)
	}

	// 4. Remove allowed_hosts symlinks.
	for _, host := range t.AllowedHosts {
		err = removeSymlink(t.allowedHostLink(host))
		if err != nil {
			return fmt.Errorf("Remove: allowed_host %q: %w", host, err)
		}
	}

	// 5. Remove subsystem directory.
	//    This may fail if the kernel requires all child directories to be
	//    removed first; the namespace was already removed in step 3.
	//    Clean up subsystem pseudo-files (tests only; kernel auto-removes).
	bestEffort(removeDir(filepath.Join(t.subsystemDir(), "allowed_hosts")))
	bestEffort(removeDir(filepath.Join(t.subsystemDir(), "namespaces")))
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "attr_allow_any_host")))
	err = removeDir(t.subsystemDir())
	if err != nil {
		return fmt.Errorf("Remove: subsystem dir: %w", err)
	}

	return nil
}

// ACL management functions.

// AllowHost grants the given host NQN access to this subsystem by:
//  1. Creating <nvmetRoot>/hosts/<hostNQN>/ directory (the kernel instantiates
//     the host object).
//  2. Creating a symlink at <subsystemDir>/allowed_hosts/<hostNQN> →
//     <nvmetRoot>/hosts/<hostNQN>.
//
// The operation is idempotent.
func (t *NvmetTarget) AllowHost(hostNQN string) error {
	hDir := t.hostDir(hostNQN)
	err := mkdirAll(hDir)
	if err != nil {
		return fmt.Errorf("AllowHost %q: create host dir: %w", hostNQN, err)
	}

	// Ensure the allowed_hosts parent directory exists.
	ahDir := filepath.Join(t.subsystemDir(), "allowed_hosts")
	err = mkdirAll(ahDir)
	if err != nil {
		return fmt.Errorf("AllowHost %q: create allowed_hosts dir: %w", hostNQN, err)
	}

	linkPath := t.allowedHostLink(hostNQN)
	err = symlink(hDir, linkPath)
	if err != nil {
		return fmt.Errorf("AllowHost %q: %w", hostNQN, err)
	}
	return nil
}

// DenyHost revokes the given host NQN's access to this subsystem by removing
// the allowed_hosts symlink.  The host directory under <nvmetRoot>/hosts/ is
// NOT removed because other subsystems may still reference it.
//
// The operation is idempotent.
func (t *NvmetTarget) DenyHost(hostNQN string) error {
	err := removeSymlink(t.allowedHostLink(hostNQN))
	if err != nil {
		return fmt.Errorf("DenyHost %q: %w", hostNQN, err)
	}
	return nil
}
