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
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// Configfs primitive operations (op label of
// pillar_csi_nvmet_configfs_errors_total).
const (
	configfsOpWrite   = "write"
	configfsOpTrigger = "trigger"
	configfsOpMkdir   = "mkdir"
	configfsOpSymlink = "symlink"
	configfsOpUnlink  = "unlink"
	configfsOpRmdir   = "rmdir"
)

// configfsErrors is M8, pillar_csi_nvmet_configfs_errors_total: one increment
// per failed configfs operation whose error is returned to the caller.
// Successes are not counted, and neither are best-effort teardown steps:
// those use plain os calls (see bestEffort), so tolerated misses like an
// EPERM rmdir of a kernel-managed default group or an ENOENT disable write
// on an already-removed namespace stay out of the metric.
var configfsErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pillar_csi_nvmet_configfs_errors_total",
	Help: "Errors returned to callers by nvmet configfs primitives, by operation and errno.",
}, []string{"op", "errno"})

// RegisterMetrics registers pillar_csi_nvmet_configfs_errors_total.
func RegisterMetrics(reg prometheus.Registerer) error {
	err := reg.Register(configfsErrors)
	if err != nil {
		return fmt.Errorf("register pillar_csi_nvmet_configfs_errors_total: %w", err)
	}
	return nil
}

// recordConfigfsError counts a non-nil error of the configfs primitive op.
func recordConfigfsError(op string, err error) {
	if err != nil {
		configfsErrors.WithLabelValues(op, telemetry.ConfigfsErrnoLabel(err)).Inc()
	}
}

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
	// wildcard "0.0.0.0".  See portSpec for how this separation is
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

	// InlineDataSize is the in-capsule data size in bytes the export
	// requires on its port (param_inline_data_size); at least
	// MinInlineDataSize.  nil accepts the port's value.  See
	// ensureInlineDataSizeLocked for the shared-port contract.
	InlineDataSize *int32

	// Identity is the host-visible namespace and subsystem identity written
	// before the namespace is enabled.  The zero value selects
	// DeriveIdentity(SubsystemNQN, NamespaceID).
	Identity Identity

	// LocalAttach keeps the namespace disabled (enable=0): the volume is
	// attached directly on the storage node, so no remote initiator may do
	// I/O through this export.  Prepare disables an enabled namespace and
	// Link verifies enable=0 instead of 1.
	LocalAttach bool

	// DeviceClaimer takes an exclusive claim on the backend device before a
	// disabled namespace is enabled; the claim is held across the enable
	// write.  nil selects ClaimDeviceExclusively.
	DeviceClaimer DeviceClaimer
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
func writeFile(path, content string) (err error) {
	defer func() { recordConfigfsError(configfsOpWrite, err) }()

	lock := writeFileLock(path)
	lock.Lock()
	defer lock.Unlock()

	err = os.WriteFile(path, []byte(content), 0o600)
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
func triggerFile(path, content string) (err error) {
	defer func() { recordConfigfsError(configfsOpTrigger, err) }()

	lock := writeFileLock(path)
	lock.Lock()
	defer lock.Unlock()

	err = os.WriteFile(path, []byte(content), 0o600)
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
	recordConfigfsError(configfsOpMkdir, err)
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
func symlink(oldname, newname string) (err error) {
	defer func() { recordConfigfsError(configfsOpSymlink, err) }()

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
func removeSymlink(path string) (err error) {
	defer func() { recordConfigfsError(configfsOpUnlink, err) }()

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
		recordConfigfsError(configfsOpRmdir, err)
		return fmt.Errorf("configfs rmdir %q: %w", path, err)
	}
	return nil
}

// removeDirVerified removes the directory at path like removeDir and reads
// back that the kernel actually destroyed it.
func removeDirVerified(path string) error {
	err := removeDir(path)
	if err != nil {
		return err
	}
	_, err = os.Lstat(path)
	if err == nil {
		return fmt.Errorf("configfs verify rmdir %q: still present after removal", path)
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("configfs verify rmdir %q: %w", path, err)
	}
	return nil
}

// bestEffort accepts an error value and discards it.  It is used to silence
// errcheck for intentionally best-effort cleanup operations where failure is
// expected and acceptable (e.g. removing pseudo-files on a regular filesystem
// in tests, the EPERM rmdir of kernel-managed default groups, or the ENOENT
// disable write of an already-removed namespace).  These call sites use plain
// os primitives, never the counting wrappers above, so tolerated failures are
// not counted by pillar_csi_nvmet_configfs_errors_total.
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
// brings it to its desired enable state against the backing block device.
// Specifically it:
//
//  1. Creates <subsystemDir>/namespaces/<nsid>/ (the kernel instantiates the
//     namespace object when the directory appears).
//  2. With LocalAttach, disables the namespace first (fencing every remote
//     session before anything else changes).
//  3. Writes t.DevicePath to the device_path pseudo-file so the kernel knows
//     which block device backs this namespace.
//  4. Writes the identity to device_uuid and device_nguid.  nvmet otherwise
//     assigns a random UUID on every (re)creation, and a reconnecting host
//     that sees a different UUID drops the namespace.
//  5. Without LocalAttach, enables the namespace through enableNamespace,
//     which refuses while the backend device is held exclusively on the
//     storage node; the kernel accepts I/O after this write.
//
// Call createNamespace after createSubsystem because the namespace directory
// lives inside the subsystem directory.
//
// An already-enabled namespace cannot change its identity (nvmet returns
// EBUSY) and, unless LocalAttach asks for it, must not be disabled, because
// connected hosts would lose it.  If
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

	if t.LocalAttach {
		err = t.disableNamespace()
		if err != nil {
			return fmt.Errorf("createNamespace: %w", err)
		}
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

	if t.LocalAttach {
		return nil
	}
	return t.enableNamespace()
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
//
// It is a no-op when the volume is not exported, and when the namespace is
// disabled (local attach): the kernel rejects revalidate_size on a disabled
// namespace, and it re-reads the device size when the namespace is enabled.
func (t *NvmetTarget) ResizeNamespace() error {
	exists, enabled, err := t.NamespaceEnabled()
	if err != nil {
		return fmt.Errorf("ResizeNamespace: %w", err)
	}
	if !exists || !enabled {
		return nil
	}

	revalidatePath := filepath.Join(t.namespaceDir(), "revalidate_size")
	err = triggerFile(revalidatePath, "1")
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

// portSpec is the configfs port a target is exported on: its stable ID and
// the listener attributes it must carry.
type portSpec struct {
	id    uint32
	attrs map[string]string
	// inlineDataSize is the param_inline_data_size the export requires, or
	// nil when it accepts the port's value (see ensureInlineDataSizeLocked).
	inlineDataSize *int32
	// desc names the endpoint in error messages ("<bind address>:<port>").
	desc string
}

// portSpec derives the port of this target from BindAddress and Port.
//
// The port ID is derived deterministically from (BindAddress, Port) via
// stablePortID so all subsystems advertised at the same endpoint share one
// listener.  The listener attributes are:
//   - addr_trtype  = "tcp"
//   - addr_adrfam  = "ipv4" or "ipv6" — derived from BindAddress
//   - addr_traddr  = "0.0.0.0" or "::" — kernel-side bind wildcard matching adrfam
//   - addr_trsvcid = <Port>
//
// BindAddress must be a valid IP literal (IPv4 dotted-quad or IPv6); an
// unparseable value is rejected up-front so the caller does not silently
// produce a port that the kernel will reject when the subsystem symlink is
// later created.
func (t *NvmetTarget) portSpec() (portSpec, error) {
	port := t.Port
	if port == 0 {
		port = DefaultPort
	}
	ip := net.ParseIP(t.BindAddress)
	if ip == nil {
		return portSpec{}, fmt.Errorf("port: invalid BindAddress %q: not an IP literal", t.BindAddress)
	}
	if v := t.InlineDataSize; v != nil && *v < MinInlineDataSize {
		return portSpec{}, fmt.Errorf("port: invalid InlineDataSize %d: must be at least %d", *v, MinInlineDataSize)
	}
	adrfam, wildcard := "ipv4", listenWildcard
	if ip.To4() == nil {
		adrfam, wildcard = "ipv6", listenWildcardV6
	}
	trsvcid := strconv.Itoa(int(port))
	return portSpec{
		id: stablePortID(t.BindAddress, port),
		attrs: map[string]string{
			"addr_trtype":  "tcp",
			"addr_adrfam":  adrfam,
			"addr_traddr":  wildcard,
			"addr_trsvcid": trsvcid,
		},
		inlineDataSize: t.InlineDataSize,
		desc:           net.JoinHostPort(t.BindAddress, trsvcid),
	}, nil
}

// ensurePortLocked creates the port directory of spec if needed and writes
// every listener attribute that does not hold its value yet.  The caller
// holds the port's writeFileLock, which serializes it with pruning the port.
//
// Once any subsystem is linked, Linux makes the listener attributes immutable
// and rejects even same-value writes.  Existing matching values are therefore
// read and retained rather than rewritten.
func (t *NvmetTarget) ensurePortLocked(spec portSpec) error {
	pDir := t.portDir(spec.id)
	err := mkdirAll(pDir)
	if err != nil {
		return fmt.Errorf("port %s: %w", spec.desc, err)
	}
	for attr, val := range spec.attrs {
		attrPath := filepath.Join(pDir, attr)
		current, readErr := readFileTrimmed(attrPath)
		if readErr != nil {
			return fmt.Errorf("port %s attr %s read: %w", spec.desc, attr, readErr)
		}
		if current == val {
			continue
		}
		err = writeFile(attrPath, val)
		if err != nil {
			return fmt.Errorf("port %s attr %s: %w", spec.desc, attr, err)
		}
	}
	return ensureInlineDataSizeLocked(pDir, spec)
}

// portInlineDataSizeAttr is the port attribute holding the in-capsule data
// size the target advertises (IOCCSZ) to hosts connecting through the port.
const portInlineDataSizeAttr = "param_inline_data_size"

// MinInlineDataSize is the smallest usable param_inline_data_size: Linux
// NVMe/TCP hosts always send fabrics commands in-capsule, including the
// 1024-byte Connect data (struct nvmf_connect_data), and nvmet_tcp rejects an
// in-capsule payload larger than the port's inline_data_size with a
// do-not-retry status, so a smaller value makes every connect fail.
const MinInlineDataSize = 1024

// transportDefaultInlineDataSize is the param_inline_data_size value that
// lets the transport choose (4 * PAGE_SIZE for TCP) when the port is enabled.
const transportDefaultInlineDataSize = "-1"

// ErrPortInlineDataSizeConflict reports that an export requires an in-capsule
// data size other than the one its shared, already active port uses.
var ErrPortInlineDataSizeConflict = errors.New("port in-capsule data size conflict")

// ensureInlineDataSizeLocked makes the port's param_inline_data_size satisfy
// spec.  The attribute belongs to the port, which every volume exported on
// the same address and port shares, and Linux accepts writes to it only
// while no subsystem is linked to the port (nvmet_is_port_enabled, EACCES
// otherwise).  Hence:
//
//   - port without linked subsystems: the requested value, or the transport
//     default when none is requested, is written and read back;
//   - port with linked subsystems: a requested value must equal the value
//     the port already uses, otherwise ErrPortInlineDataSizeConflict is
//     returned instead of silently exporting with the port's value; an
//     export without a requested value uses the port as is.
//
// A missing attribute (no value to reset) is left alone when nothing is
// requested.  The caller holds the port's writeFileLock.
func ensureInlineDataSizeLocked(pDir string, spec portSpec) error {
	attrPath := filepath.Join(pDir, portInlineDataSizeAttr)
	current, err := readFileTrimmed(attrPath)
	if err != nil {
		return fmt.Errorf("port %s attr %s read: %w", spec.desc, portInlineDataSizeAttr, err)
	}
	linked, err := portHasSubsystems(pDir)
	if err != nil {
		return fmt.Errorf("port %s: %w", spec.desc, err)
	}

	if linked {
		if spec.inlineDataSize == nil {
			return nil
		}
		want := strconv.Itoa(int(*spec.inlineDataSize))
		if current != want {
			return fmt.Errorf("port %s: %s is %q but the export requires %s; the in-capsule data "+
				"size is shared by every volume exported on this port and can change only "+
				"while no volume is exported on it: %w",
				spec.desc, portInlineDataSizeAttr, current, want, ErrPortInlineDataSizeConflict)
		}
		return nil
	}

	want := transportDefaultInlineDataSize
	if spec.inlineDataSize != nil {
		want = strconv.Itoa(int(*spec.inlineDataSize))
	} else if current == "" {
		return nil
	}
	if current == want {
		return nil
	}
	err = writeFile(attrPath, want)
	if err != nil {
		return fmt.Errorf("port %s attr %s: %w", spec.desc, portInlineDataSizeAttr, err)
	}
	return nil
}

// portHasSubsystems reports whether any subsystem is linked to the port at
// pDir, i.e. whether the kernel has enabled the port.
func portHasSubsystems(pDir string) (bool, error) {
	subsDir := filepath.Join(pDir, "subsystems")
	subs, err := os.ReadDir(subsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("configfs read %q: %w", subsDir, err)
	}
	return len(subs) > 0, nil
}

// linkSubsystemToPort creates a symlink in the port's subsystems/ directory
// that points to the subsystem directory, activating the subsystem on that
// port.  When it is the port's first subsystem the kernel starts listening
// (nvmet_port_subsys_allow_link → nvmet_enable_port), so from this moment
// hosts reach the subsystem, and every other subsystem sharing the port that
// is not linked yet answers connects with a do-not-retry rejection.
//
// Under the port lock, shared with pruning the port, it creates the port and
// its transport attributes if needed (the kernel refuses to enable a port
// without them) and then links.  A port therefore exists only while a
// subsystem is linked to it: when linking fails, a port left without any
// subsystem is pruned again.
func (t *NvmetTarget) linkSubsystemToPort(spec portSpec) error {
	pDir := t.portDir(spec.id)
	portLock := writeFileLock(pDir)
	portLock.Lock()
	defer portLock.Unlock()

	err := t.linkSubsystemToPortLocked(spec)
	if err != nil {
		return fmt.Errorf("linkSubsystemToPort: %w", errors.Join(err, prunePortLocked(pDir)))
	}
	return nil
}

func (t *NvmetTarget) linkSubsystemToPortLocked(spec portSpec) error {
	err := t.ensurePortLocked(spec)
	if err != nil {
		return err
	}
	linkPath := t.portSubsystemLink(spec.id)
	// Ensure the ports/<id>/subsystems/ parent directory exists.
	err = mkdirAll(filepath.Dir(linkPath))
	if err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return symlink(t.subsystemDir(), linkPath)
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
//   - Link creates the port and its transport attributes and links the
//     subsystem under the port lock; Prepare creates no port, so a port
//     exists only while a subsystem is linked to it;
//   - Remove unlinks the subsystem from its ports before tearing it down, and
//     under the same lock removes a port it was linked to once no subsystem
//     is linked any more.

// PreparedTarget is a target whose subsystem, ACL and namespace are
// configured but which is not yet linked to its port, so no host can reach it.
type PreparedTarget struct {
	target   *NvmetTarget
	identity Identity
	port     portSpec
}

// Prepare configures everything a host needs before the subsystem becomes
// reachable, in dependency order:
//
//  1. Subsystem with its final attr_allow_any_host and serial.
//  2. Allowed hosts (nvmet accepts them only while attr_allow_any_host is 0).
//  3. Namespace: identity, device_path, then the desired enable state
//     (disabled with LocalAttach; enabled only if the backend device is not
//     held exclusively on the storage node).
//
// It validates the port endpoint up front but neither creates the port nor
// links the subsystem to it; Link does both.  Every step is idempotent, and
// an already linked subsystem stays linked.
func (t *NvmetTarget) Prepare() (PreparedTarget, error) {
	port, err := t.portSpec()
	if err != nil {
		return PreparedTarget{}, fmt.Errorf("Prepare: %w", err)
	}
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
	return PreparedTarget{target: t, identity: id, port: port}, nil
}

// Link makes the prepared subsystem reachable on its port.  It first reads
// back the state Prepare established and refuses to link a subsystem whose
// namespace does not have the desired enable state (enabled, or disabled
// with LocalAttach), carries another device or identity, or lacks an ACL
// entry.
func (p PreparedTarget) Link() error {
	t := p.target
	if t == nil {
		return errors.New("Link: target was not prepared")
	}
	err := t.verifyPrepared(p.identity)
	if err != nil {
		return fmt.Errorf("Link %q: %w", t.SubsystemNQN, err)
	}
	err = t.linkSubsystemToPort(p.port)
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
		{filepath.Join(nsDir, "enable"), t.desiredEnable()},
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

// unlinkFromPorts unlinks the subsystem from every port under
// <nvmetRoot>/ports/ and prunes each port it was linked to once no subsystem
// is linked to it any more.  A missing ports directory is not an error.
func (t *NvmetTarget) unlinkFromPorts() error {
	portsDir := filepath.Join(t.nvmetRoot(), "ports")
	entries, err := os.ReadDir(portsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unlinkFromPorts: read ports dir: %w", err)
	}
	for _, entry := range entries {
		err = t.unlinkFromPort(filepath.Join(portsDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("unlinkFromPorts: port %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// unlinkFromPort removes the subsystem's link from the port at pDir and
// prunes the port if no subsystem is linked to it any more.  Ports the
// subsystem is not linked to are left alone.
//
// It runs under the port lock shared with Link, so a port is never pruned
// between Link creating it and linking to it.  Unlinking and pruning are one
// step: when pruning fails the link is restored, so that the error leaves the
// subsystem exported as before and a retry finds the port again.
func (t *NvmetTarget) unlinkFromPort(pDir string) error {
	portLock := writeFileLock(pDir)
	portLock.Lock()
	defer portLock.Unlock()

	linkPath := filepath.Join(pDir, "subsystems", t.SubsystemNQN)
	_, err := os.Lstat(linkPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("configfs stat %q: %w", linkPath, err)
	}
	err = removeSymlink(linkPath)
	if err != nil {
		return err
	}
	err = prunePortLocked(pDir)
	if err != nil {
		return fmt.Errorf("prune port: %w", errors.Join(err, restoreLink(t.subsystemDir(), linkPath)))
	}
	return nil
}

// restoreLink re-creates a link removed by an operation that failed later.
// Its parent is a default group on configfs, which survives a failed rmdir of
// its item; a regular filesystem (tests) may have lost it.
func restoreLink(oldname, newname string) error {
	err := mkdirAll(filepath.Dir(newname))
	if err == nil {
		err = symlink(oldname, newname)
	}
	if err != nil {
		return fmt.Errorf("restore %q after failure: %w", newname, err)
	}
	return nil
}

// portAttrs are the attributes ensurePortLocked writes to a port.
var portAttrs = []string{"addr_trtype", "addr_adrfam", "addr_traddr", "addr_trsvcid", portInlineDataSizeAttr}

// prunePortLocked removes the port directory at pDir when no subsystem is
// linked to it, and reads back that it is gone.  A port without subsystems
// does not listen, so removing it changes no connection.  The caller holds
// the port's writeFileLock.
func prunePortLocked(pDir string) error {
	_, err := os.Lstat(pDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("configfs stat %q: %w", pDir, err)
	}
	subsDir := filepath.Join(pDir, "subsystems")
	subs, err := os.ReadDir(subsDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("configfs read %q: %w", subsDir, err)
	}
	if len(subs) > 0 {
		return nil
	}
	// On real configfs the kernel removes pseudo-files and default groups
	// with the port directory; on a regular filesystem (tests) we must clean
	// them up manually.
	for _, attr := range portAttrs {
		bestEffort(os.Remove(filepath.Join(pDir, attr)))
	}
	bestEffort(os.Remove(subsDir))
	return removeDirVerified(pDir)
}

// Remove tears down the NVMe-oF TCP target entry in configfs.  The steps are
// executed in reverse dependency order:
//
//  1. Unlink subsystem from all ports (scans ports/ directory) and prune the
//     ports it was linked to that no subsystem is linked to any more
//  2. Disable namespace (write "0" to enable)
//  3. Remove namespace directory
//  4. Remove the allowed_hosts symlinks actually present and prune the host
//     entries no subsystem references any more
//  5. Remove subsystem directory
//
// Every step is idempotent — Remove can be called on an already-removed target
// without error.
func (t *NvmetTarget) Remove() error {
	// 1. Unlink subsystem from all ports.
	err := t.unlinkFromPorts()
	if err != nil {
		return fmt.Errorf("Remove: %w", err)
	}

	// 2-3. Disable + remove namespace.
	nsDir := t.namespaceDir()
	enablePath := filepath.Join(nsDir, "enable")
	// Write "0" to disable — ignore the error: on a retry or after partial
	// teardown the namespace (and its enable attribute) is already gone.
	bestEffort(os.WriteFile(enablePath, []byte("0"), 0o600))
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

	// 4. Revoke every host actually linked, not just t.AllowedHosts: hosts
	//    granted by AllowInitiator are not part of the target description,
	//    and Unexport knows only the subsystem NQN.
	err = t.RevokeHostsExcept(nil)
	if err != nil {
		return fmt.Errorf("Remove: %w", err)
	}

	// 5. Remove subsystem directory.
	//    This may fail if the kernel requires all child directories to be
	//    removed first; the namespace was already removed in step 3.
	//    Clean up subsystem pseudo-files (tests only; kernel auto-removes).
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "allowed_hosts")))
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "namespaces")))
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "attr_allow_any_host")))
	bestEffort(os.Remove(filepath.Join(t.subsystemDir(), "attr_serial")))
	err = removeDir(t.subsystemDir())
	if err != nil {
		return fmt.Errorf("Remove: subsystem dir: %w", err)
	}

	return nil
}

// ACL management functions.

// hostLock returns the lock that serializes granting hostNQN to any
// subsystem with pruning its host entry, so that a host entry is never
// removed between AllowHost creating it and linking to it.
func (t *NvmetTarget) hostLock(hostNQN string) *sync.Mutex {
	return writeFileLock(t.hostDir(hostNQN))
}

// AllowHost grants the given host NQN access to this subsystem by:
//  1. Creating <nvmetRoot>/hosts/<hostNQN>/ directory (the kernel instantiates
//     the host object).
//  2. Creating a symlink at <subsystemDir>/allowed_hosts/<hostNQN> →
//     <nvmetRoot>/hosts/<hostNQN>.
//
// Both steps run under the host lock shared with pruning.  The operation is
// idempotent.
func (t *NvmetTarget) AllowHost(hostNQN string) error {
	lock := t.hostLock(hostNQN)
	lock.Lock()
	defer lock.Unlock()

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
// the allowed_hosts symlink, then removes the host entry under
// <nvmetRoot>/hosts/ if no subsystem's allowed_hosts links to it any more.
// Both steps run under the host lock shared with AllowHost.  When removing
// the host entry fails the link is restored, so that the error leaves the ACL
// unchanged and a retry, which discovers hosts by their links, finds the
// host again.
//
// The operation is idempotent.
func (t *NvmetTarget) DenyHost(hostNQN string) error {
	lock := t.hostLock(hostNQN)
	lock.Lock()
	defer lock.Unlock()

	linkPath := t.allowedHostLink(hostNQN)
	_, statErr := os.Lstat(linkPath)
	linked := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("DenyHost %q: configfs stat %q: %w", hostNQN, linkPath, statErr)
	}
	err := removeSymlink(linkPath)
	if err != nil {
		return fmt.Errorf("DenyHost %q: %w", hostNQN, err)
	}
	err = t.pruneHostLocked(hostNQN)
	if err != nil {
		if linked {
			err = errors.Join(err, restoreLink(t.hostDir(hostNQN), linkPath))
		}
		return fmt.Errorf("DenyHost %q: %w", hostNQN, err)
	}
	return nil
}

// pruneHostLocked removes <nvmetRoot>/hosts/<hostNQN> and reads back that it
// is gone, unless it is absent or some subsystem's allowed_hosts still links
// to it.  The caller holds the host lock.
func (t *NvmetTarget) pruneHostLocked(hostNQN string) error {
	hDir := t.hostDir(hostNQN)
	_, err := os.Lstat(hDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("configfs stat %q: %w", hDir, err)
	}
	referenced, err := t.hostReferenced(hostNQN)
	if err != nil || referenced {
		return err
	}
	return removeDirVerified(hDir)
}

// hostReferenced reports whether any subsystem's allowed_hosts links to
// hostNQN.
func (t *NvmetTarget) hostReferenced(hostNQN string) (bool, error) {
	subsDir := filepath.Join(t.nvmetRoot(), "subsystems")
	entries, err := os.ReadDir(subsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("configfs read %q: %w", subsDir, err)
	}
	for _, entry := range entries {
		link := filepath.Join(subsDir, entry.Name(), "allowed_hosts", hostNQN)
		_, err = os.Lstat(link)
		if err == nil {
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("configfs stat %q: %w", link, err)
		}
	}
	return false, nil
}

// RevokeHostsExcept removes every allowed_hosts entry of this subsystem whose
// host NQN is not in keep, making the subsystem ACL exactly keep (together
// with AllowHost for each member).  Only this subsystem's allowed_hosts links
// are touched, and only the host entries it revoked are pruned (see DenyHost);
// other subsystems are never modified.  A missing allowed_hosts directory
// means no host is allowed and is not an error.
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
