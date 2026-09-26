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
//	    attr_serial                stable serial (see Identity)
//	    namespaces/<nsid>/
//	      device_path              path to the block device
//	      device_uuid              stable namespace UUID (see Identity)
//	      device_nguid             stable namespace NGUID (see Identity)
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
	"errors"
	"fmt"
	"net"
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

	// BindAddress is the publicly-advertised IP address of the NVMe-oF TCP
	// endpoint as seen by the initiator, e.g. "192.168.1.10".  Resolved by
	// the controller from PillarAgent and forwarded verbatim to the
	// initiator via ExportInfo.Address; the agent never resolves it itself.
	//
	// It is NOT written to nvmet's addr_traddr — that file holds the kernel
	// bind address, which must be reachable from the agent process's network
	// namespace.  In Kubernetes deployments the agent runs without host
	// networking, so the only address guaranteed to be bindable is the
	// wildcard "0.0.0.0".  See createPort for how this separation is
	// enforced.
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

	// Identity is the host-visible namespace and subsystem identity written
	// before the namespace is enabled.  The zero value selects
	// DeriveIdentity(SubsystemNQN, NamespaceID).
	Identity Identity
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

// triggerFile writes to a write-only configfs action attribute. Unlike
// writeFile, it cannot read the value back because the kernel exposes no show
// callback for action files such as revalidate_size.
func triggerFile(path, content string) error {
	lock := writeFileLock(path)
	lock.Lock()
	defer lock.Unlock()

	err := os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		return fmt.Errorf("configfs trigger %q = %q: %w", path, content, err)
	}
	return nil
}

// readFileTrimmed reads a configfs pseudo-file and returns the trimmed value.
// A missing file is reported as "" + nil so callers can distinguish "freshly
// created, no value yet" from a real I/O error.  This is the dual of
// writeFile and is used to make subsystem-level writes idempotent against
// configfs's "write-once-while-enabled" enforcement (EBUSY on device_path).
func readFileTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // configfs paths constructed from validated NQN/namespace inputs
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("configfs read %q: %w", path, err)
	}
	return strings.TrimRight(string(data), "\x00\n"), nil
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
// If newname already exists as a symlink that resolves to oldname the function
// returns nil (idempotent). Configfs may canonicalize an absolute target into
// a relative path, so comparing the raw readlink value is insufficient. Any
// other pre-existing path at newname is treated as an error to avoid silently
// overwriting unrelated configfs state.
func symlink(oldname, newname string) error {
	existing, err := os.Readlink(newname)
	switch {
	case err == nil:
		if resolvedLinkTarget(newname, existing) == resolvedLinkTarget(newname, oldname) {
			return nil
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

func resolvedLinkTarget(linkPath, target string) string {
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
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
//  2. Writes the final attr_allow_any_host value: "0" when ACLEnabled is set
//     or AllowedHosts is non-empty (only explicitly allowed initiators), "1"
//     otherwise (any initiator may connect).
//  3. Writes the identity serial to attr_serial (skipped when it already
//     holds that value, because nvmet locks it once a host discovered the
//     subsystem).
//
// The operation is idempotent: if the directory already exists the mkdir is a
// no-op; configfs pseudo-files accept repeated identical writes.
func (t *NvmetTarget) createSubsystem() error {
	id, err := t.desiredIdentity()
	if err != nil {
		return fmt.Errorf("createSubsystem: %w", err)
	}
	subDir := t.subsystemDir()
	err = mkdirAll(subDir)
	if err != nil {
		return fmt.Errorf("createSubsystem %q: %w", t.SubsystemNQN, err)
	}
	// nvmet refuses allowed_hosts links while attr_allow_any_host is 1, so
	// the final value is written before any host is added.
	allowAnyHost := "1"
	if t.ACLEnabled || len(t.AllowedHosts) > 0 {
		allowAnyHost = "0"
	}
	attrPath := filepath.Join(subDir, "attr_allow_any_host")
	err = writeFile(attrPath, allowAnyHost)
	if err != nil {
		return fmt.Errorf("createSubsystem %q: %w", t.SubsystemNQN, err)
	}
	err = ensureAttr(filepath.Join(subDir, "attr_serial"), id.Serial)
	if err != nil {
		return fmt.Errorf("createSubsystem %q serial: %w", t.SubsystemNQN, err)
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
//  3. Writes the identity to device_uuid and device_nguid.  nvmet otherwise
//     assigns a random UUID on every (re)creation, and a reconnecting host
//     that sees a different UUID drops the namespace.
//  4. Writes "1" to enable to activate the namespace; the kernel will begin
//     accepting I/O after this write.
//
// Call createNamespace after createSubsystem because the namespace directory
// lives inside the subsystem directory.
//
// An already-enabled namespace cannot change its identity (nvmet returns
// EBUSY) and must not be disabled, because connected hosts would lose it.  If
// its identity differs from the desired one, createNamespace returns an error
// instead of touching it; callers keep a live identity via LiveIdentity.
//
// The operation is idempotent: repeated calls with the same parameters produce
// the same configfs state.
func (t *NvmetTarget) createNamespace() error {
	id, err := t.desiredIdentity()
	if err != nil {
		return fmt.Errorf("createNamespace: %w", err)
	}
	nsDir := t.namespaceDir()
	err = mkdirAll(nsDir)
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}

	// The kernel rejects writes to device_path while the namespace is
	// enabled, returning EBUSY.  Apply is expected to be idempotent, so a
	// retry after a partially-successful previous Apply (e.g. port-symlink
	// failure leaving the namespace enabled) must not surface as an EBUSY
	// here.  Read the current value first and skip the write when it
	// already matches the desired device path.
	devPath := filepath.Join(nsDir, "device_path")
	current, readErr := readFileTrimmed(devPath)
	if readErr != nil {
		return fmt.Errorf("createNamespace %q ns=%d read device_path: %w",
			t.SubsystemNQN, t.NamespaceID, readErr)
	}
	if current != t.DevicePath {
		err = writeFile(devPath, t.DevicePath)
		if err != nil {
			return fmt.Errorf("createNamespace %q ns=%d: %w",
				t.SubsystemNQN, t.NamespaceID, err)
		}
	}

	enablePath := filepath.Join(nsDir, "enable")
	err = t.ensureNamespaceIdentity(nsDir, enablePath, id)
	if err != nil {
		return err
	}

	err = writeFile(enablePath, "1")
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	return nil
}

// ensureNamespaceIdentity writes id to a disabled namespace, or verifies that
// an enabled namespace already carries it.
func (t *NvmetTarget) ensureNamespaceIdentity(nsDir, enablePath string, id Identity) error {
	enabled, err := readAttr(enablePath)
	if err != nil {
		return fmt.Errorf("createNamespace %q ns=%d read enable: %w", t.SubsystemNQN, t.NamespaceID, err)
	}
	attrs := []struct{ name, want string }{
		{"device_uuid", id.UUID},
		{"device_nguid", id.NGUID},
	}
	for _, attr := range attrs {
		path := filepath.Join(nsDir, attr.name)
		if enabled != "1" {
			err = ensureAttr(path, attr.want)
			if err != nil {
				return fmt.Errorf("createNamespace %q ns=%d identity: %w", t.SubsystemNQN, t.NamespaceID, err)
			}
			continue
		}
		current, readErr := readAttr(path)
		if readErr != nil {
			return fmt.Errorf("createNamespace %q ns=%d identity: %w", t.SubsystemNQN, t.NamespaceID, readErr)
		}
		if current != attr.want {
			return fmt.Errorf(
				"createNamespace %q ns=%d: enabled namespace has %s %q, want %q; "+
					"refusing to disable a live namespace to change its identity",
				t.SubsystemNQN, t.NamespaceID, attr.name, current, attr.want)
		}
	}
	return nil
}

// ResizeNamespace asks the enabled NVMe target namespace to revalidate the
// backing block-device size and notify connected initiators with a namespace
// changed asynchronous event. It deliberately does not toggle enable: doing so
// unregisters the live namespace and leaves mounted clients holding a stale
// device node that fails with ENXIO during online filesystem expansion.
func (t *NvmetTarget) ResizeNamespace() error {
	nsDir := t.namespaceDir()
	_, statErr := os.Stat(nsDir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil // volume is not currently exported
		}
		return fmt.Errorf("ResizeNamespace %q ns=%d stat: %w", t.SubsystemNQN, t.NamespaceID, statErr)
	}

	revalidatePath := filepath.Join(nsDir, "revalidate_size")
	err := triggerFile(revalidatePath, "1")
	if err != nil {
		return fmt.Errorf(
			"ResizeNamespace %q ns=%d revalidate backing size: %w",
			t.SubsystemNQN,
			t.NamespaceID,
			err,
		)
	}
	return nil
}

// listenWildcard is the IPv4 kernel-side bind address written to nvmet's
// addr_traddr.  It is intentionally distinct from NvmetTarget.BindAddress:
// the latter is what the initiator connects to (a routable node IP) while
// addr_traddr is what the kernel binds inside the agent's network namespace.
// In pod-networked deployments the routable node IP is not present on any
// interface visible to the pod, so the kernel returns EADDRNOTAVAIL when the
// subsystem-to-port symlink is created.  Binding the wildcard accepts
// connections from every interface — including the host bridge — and lets
// initiators use the advertised BindAddress unchanged.
const listenWildcard = "0.0.0.0"

// listenWildcardV6 is the IPv6 counterpart of listenWildcard.  Same rationale
// as the IPv4 version: bind every interface so the published BindAddress
// (which may live on the host's external IPv6 interface) does not need to be
// reachable from inside the pod's network namespace.
const listenWildcardV6 = "::"

// createPort creates or converges the configfs port directory for this target's
// bind address and TCP port.
//
// The port ID is derived deterministically from (BindAddress, Port) via
// stablePortID so all subsystems advertised at the same endpoint share one
// listener.
//
// After the port directory is created the function ensures:
//   - addr_trtype  = "tcp"
//   - addr_adrfam  = "ipv4" or "ipv6" — derived from BindAddress
//   - addr_traddr  = "0.0.0.0" or "::" — kernel-side bind wildcard matching adrfam
//   - addr_trsvcid = <Port>
//
// BindAddress must be a valid IP literal (IPv4 dotted-quad or IPv6); an
// unparseable value is rejected up-front so the caller does not silently
// produce a port that the kernel will reject when the subsystem symlink is
// later created.
//
// The operation is idempotent. Once any subsystem is linked, Linux makes the
// listener attributes immutable and rejects even same-value writes. Existing
// matching values are therefore read and retained rather than rewritten.
func (t *NvmetTarget) createPort() (uint32, error) {
	port := t.Port
	if port == 0 {
		port = DefaultPort
	}
	ip := net.ParseIP(t.BindAddress)
	if ip == nil {
		return 0, fmt.Errorf("createPort: invalid BindAddress %q: not an IP literal", t.BindAddress)
	}
	adrfam, wildcard := "ipv4", listenWildcard
	if ip.To4() == nil {
		adrfam, wildcard = "ipv6", listenWildcardV6
	}
	portID := stablePortID(t.BindAddress, port)
	pDir := t.portDir(portID)
	portLock := writeFileLock(pDir)
	portLock.Lock()
	defer portLock.Unlock()

	err := mkdirAll(pDir)
	if err != nil {
		return 0, fmt.Errorf("createPort %s:%d: %w", t.BindAddress, port, err)
	}

	attrs := map[string]string{
		"addr_trtype":  "tcp",
		"addr_adrfam":  adrfam,
		"addr_traddr":  wildcard,
		"addr_trsvcid": fmt.Sprintf("%d", port),
	}
	for attr, val := range attrs {
		attrPath := filepath.Join(pDir, attr)
		current, readErr := readFileTrimmed(attrPath)
		if readErr != nil {
			return 0, fmt.Errorf("createPort %s:%d attr %s read: %w",
				t.BindAddress, port, attr, readErr)
		}
		if current == val {
			continue
		}
		err = writeFile(attrPath, val)
		if err != nil {
			return 0, fmt.Errorf("createPort %s:%d attr %s: %w", t.BindAddress, port, attr, err)
		}
	}
	return portID, nil
}

// linkSubsystemToPort creates a symlink in the port's subsystems/ directory
// that points to the subsystem directory, activating the subsystem on that
// port.  When it is the port's first subsystem the kernel starts listening
// (nvmet_port_subsys_allow_link → nvmet_enable_port), so from this moment
// hosts reach the subsystem, and every other subsystem sharing the port that
// is not linked yet answers connects with a do-not-retry rejection.
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

// Apply, Prepare, Link and Remove implement the full target lifecycle.
//
// Port ordering contract.  A reconnecting host must see either a refused TCP
// connection (retried until ctrl_loss_tmo) or a fully configured subsystem.
// The kernel target rejects the connect with the do-not-retry bit, and Linux
// hosts then delete the controller, when the subsystem is not linked to the
// listening port (Connect Invalid Data Parameter) or the host is not allowed
// (Connect Invalid Host); a namespace that is disabled or carries another
// identity is dropped by the host.  Therefore:
//
//   - a subsystem is linked to a port only after its ACL, namespace identity,
//     device and enable are in place (Prepare, then Link);
//   - a port starts listening only when every export that shares it is
//     prepared: a caller restoring several exports calls Prepare for all of
//     them first and Link for each only afterwards, with nothing slow
//     (device waits, durable writes) between the links;
//   - Remove unlinks the subsystem from its ports before tearing it down.

// PreparedTarget is a target whose subsystem, ACL, namespace and port are
// configured but which is not yet linked to its port, so no host can reach it.
type PreparedTarget struct {
	target   *NvmetTarget
	identity Identity
	portID   uint32
}

// Prepare configures everything a host needs before the subsystem becomes
// reachable, in dependency order:
//
//  1. Subsystem with its final attr_allow_any_host and serial.
//  2. Allowed hosts (nvmet accepts them only while attr_allow_any_host is 0).
//  3. Namespace: identity, device_path, then enable.
//  4. Port transport attributes (the port does not listen until a subsystem
//     is linked to it).
//
// It never links the subsystem to the port.  Every step is idempotent, and
// an already linked subsystem stays linked.
func (t *NvmetTarget) Prepare() (PreparedTarget, error) {
	id, err := t.desiredIdentity()
	if err != nil {
		return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
	}
	err = t.createSubsystem()
	if err != nil {
		return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
	}
	for _, host := range t.AllowedHosts {
		err = t.AllowHost(host)
		if err != nil {
			return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
		}
	}
	err = t.createNamespace()
	if err != nil {
		return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
	}
	portID, err := t.createPort()
	if err != nil {
		return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
	}
	return PreparedTarget{target: t, identity: id, portID: portID}, nil
}

// Link makes the prepared subsystem reachable on its port.  It first reads
// back the state Prepare established and refuses to link a subsystem that a
// host could not use: disabled namespace, other device or identity, or a
// missing ACL entry.
func (p PreparedTarget) Link() error {
	t := p.target
	if t == nil {
		return errors.New("Link: target was not prepared")
	}
	err := t.verifyPrepared(p.identity)
	if err != nil {
		return fmt.Errorf("Link %q: %w", t.SubsystemNQN, err)
	}
	err = t.linkSubsystemToPort(p.portID)
	if err != nil {
		return fmt.Errorf("Link %q: %w", t.SubsystemNQN, err)
	}
	return nil
}

// verifyPrepared checks the live configfs state against everything Prepare
// writes before the subsystem may be linked.
func (t *NvmetTarget) verifyPrepared(id Identity) error {
	nsDir := t.namespaceDir()
	allowAnyHost := "1"
	if t.ACLEnabled || len(t.AllowedHosts) > 0 {
		allowAnyHost = "0"
	}
	checks := []struct{ path, want string }{
		{filepath.Join(t.subsystemDir(), "attr_allow_any_host"), allowAnyHost},
		{filepath.Join(t.subsystemDir(), "attr_serial"), id.Serial},
		{filepath.Join(nsDir, "device_path"), t.DevicePath},
		{filepath.Join(nsDir, "device_uuid"), id.UUID},
		{filepath.Join(nsDir, "device_nguid"), id.NGUID},
		{filepath.Join(nsDir, "enable"), "1"},
	}
	for _, c := range checks {
		got, err := readAttr(c.path)
		if err != nil {
			return fmt.Errorf("verify prepared: %w", err)
		}
		if got != c.want {
			return fmt.Errorf("verify prepared: %s is %q, want %q", c.path, got, c.want)
		}
	}
	for _, host := range t.AllowedHosts {
		link := t.allowedHostLink(host)
		fi, err := os.Lstat(link)
		if err != nil {
			return fmt.Errorf("verify prepared: allowed host %q: %w", host, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("verify prepared: allowed host %q: %s is not a symlink", host, link)
		}
	}
	return nil
}

// Apply creates the complete NVMe-oF TCP target entry in configfs: Prepare,
// then Link.  Every step is idempotent, so Apply can be called repeatedly to
// converge to the desired state.  Apply alone is safe only when no other
// export sharing the port still waits to be applied; restoring several
// exports must Prepare all of them before Linking any.
func (t *NvmetTarget) Apply() error {
	prepared, err := t.Prepare()
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
	}
	err = prepared.Link()
	if err != nil {
		return fmt.Errorf("Apply: %w", err)
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
	bestEffort(os.Remove(filepath.Join(nsDir, "device_uuid")))
	bestEffort(os.Remove(filepath.Join(nsDir, "device_nguid")))
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
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "attr_serial")))
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

// RevokeHostsExcept removes every allowed_hosts entry of this subsystem whose
// host NQN is not in keep, making the subsystem ACL exactly keep (together
// with AllowHost for each member).  Only this subsystem's allowed_hosts links
// are touched; host directories under <nvmetRoot>/hosts/ and other subsystems
// are never modified.  A missing allowed_hosts directory means no host is
// allowed and is not an error.
func (t *NvmetTarget) RevokeHostsExcept(keep []string) error {
	ahDir := filepath.Join(t.subsystemDir(), "allowed_hosts")
	entries, err := os.ReadDir(ahDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("RevokeHostsExcept %q: read allowed_hosts: %w", t.SubsystemNQN, err)
	}
	wanted := make(map[string]struct{}, len(keep))
	for _, host := range keep {
		wanted[host] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := wanted[entry.Name()]; ok {
			continue
		}
		err = t.DenyHost(entry.Name())
		if err != nil {
			return fmt.Errorf("RevokeHostsExcept %q: %w", t.SubsystemNQN, err)
		}
	}
	return nil
}
