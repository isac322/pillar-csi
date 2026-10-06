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

// Package csi implements the CSI (Container Storage Interface) gRPC services
// for pillar-csi: the ControllerServer (runs in the Kubernetes controller pod)
// and the NodeServer (runs as a DaemonSet on every storage consumer node).
//
// Both services accept injectable interfaces for all privileged or
// kernel-dependent operations so that they can be unit- and e2e-tested
// without root privileges, real NVMe-oF kernel modules, or a live cluster.
package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/runtimepaths"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// ─────────────────────────────────────────────────────────────────────────────
// VolumeContext keys
// ─────────────────────────────────────────────────────────────────────────────.

// VolumeContext keys set by the controller in CreateVolume and stored in the
// PersistentVolume spec.csi.volumeAttributes map.  The CO propagates these
// to NodeStageVolume so the node knows how to connect to the storage target.
const (
	// VolumeContextKeyTargetID is the protocol-agnostic target identifier set
	// by the controller in CreateVolume.  Corresponds to ExportInfo.TargetId
	// returned by the agent; for NVMe-oF TCP it is the subsystem NVMe
	// Qualified Name (NQN), e.g. "nqn.2024-01.com.example:vol1"; for iSCSI it
	// is the target IQN.
	VolumeContextKeyTargetID = "target_id"

	// VolumeContextKeyAddress is the IP address (or hostname) of the storage
	// target.  Corresponds to ExportInfo.Address.
	VolumeContextKeyAddress = "address"

	// VolumeContextKeyPort is the TCP port of the storage target encoded as a
	// decimal string (e.g. "4420").  Derived from ExportInfo.Port.
	VolumeContextKeyPort = "port"

	// VolumeContextKeyProtocolType is the storage protocol routing token set
	// by the controller in CreateVolume.  It is used by NodeStageVolume to
	// dispatch the volume to the correct ProtocolHandler.
	// Known values: "nvmeof-tcp", "iscsi".
	VolumeContextKeyProtocolType = "pillar-csi.bhyoo.com/protocol-type"
)

// ─────────────────────────────────────────────────────────────────────────────
// Staging constants and state types
// ─────────────────────────────────────────────────────────────────────────────.

// deviceWaitTimeout is the maximum time NodeStageVolume waits for the NVMe
// block device to appear after a successful Connect call.
const deviceWaitTimeout = 30 * time.Second

// devicePollInterval is the sleep between successive GetDevicePath polls.
const devicePollInterval = 500 * time.Millisecond

// defaultFsType is the filesystem type used by NodeStageVolume when the
// VolumeCapability does not specify an explicit fsType.
const defaultFsType = "ext4"

// xfsFsType is the filesystem type string for XFS.
const xfsFsType = "xfs"

// defaultStateDir is the directory used to persist per-volume staging state
// when NodeServer is created via NewNodeServer.
const defaultStateDir = "/var/lib/pillar-csi/node"

// blockStagingDeviceFile is the regular-file name that NodeStageVolume creates
// inside the kubelet-supplied stagingTargetPath directory for Block-mode
// volumes and bind-mounts the raw NVMe-oF/iSCSI device onto.
//
// Kubelet pre-creates stagingTargetPath as a DIRECTORY for both Filesystem
// and Block-mode CSI volumes (kubelet csi_block_mapper.SetUpDevice calls
// os.MkdirAll(stagingPath, 0o750) before issuing NodeStageVolume).  The
// Linux kernel rejects mount --bind when source is a block device and
// target is a directory (errno surfaces as mount(8) exit 32,
// EXT_SOURCEMOUNTREJECTED), so the plugin must bind onto a regular file
// instead.  Placing that file inside the kubelet-created directory keeps
// the stage artifact scoped to the volume's staging path and lets
// NodeUnstageVolume rely on os.Remove + the kubelet's eventual rmdir for
// cleanup.  NodePublishVolume then bind-mounts the same regular file onto
// the per-pod publish path.
const blockStagingDeviceFile = "device"

// blockStagingDevicePath returns the regular-file path inside the kubelet-
// created stagingTargetPath that NodeStageVolume Block-mode binds the raw
// NVMe-oF/iSCSI device onto.  See blockStagingDeviceFile for the kernel
// rationale.
func blockStagingDevicePath(stagingTargetPath string) string {
	return filepath.Join(stagingTargetPath, blockStagingDeviceFile)
}

// stageBindTarget returns the filesystem path that NodeStageVolume mounts
// (Filesystem mode) or bind-mounts (Block mode) for the given capability,
// and that NodePublishVolume binds onto the per-pod target_path.  Block
// mode resolves to blockStagingDevicePath; Filesystem mode resolves to
// stagingTargetPath itself.
func stageBindTarget(stagingTargetPath string, volCap *csi.VolumeCapability) string {
	if volCap != nil && volCap.GetBlock() != nil {
		return blockStagingDevicePath(stagingTargetPath)
	}
	return stagingTargetPath
}

// NodeStageState discriminated union is defined in stage_state.go.

// Connector is the interface for NVMe-oF connect/disconnect operations.
// A real implementation issues nvme-cli commands or writes to /sys/class/nvme-fabrics.
// A test implementation returns pre-programmed responses without touching the kernel.
type Connector interface {
	// Connect establishes an NVMe-oF TCP connection to the given subsystem NQN
	// at the given transport address and service ID (port), applying the
	// optional fabrics tuning in opts (nil fields keep the kernel defaults).
	// Implementations must be idempotent: connecting to an already-connected
	// subsystem must succeed without error.
	Connect(ctx context.Context, subsysNQN, trAddr, trSvcID string, opts NVMeoFConnectOptions) error

	// Disconnect tears down the NVMe-oF connection to the given subsystem NQN.
	// Implementations must be idempotent: disconnecting an NQN that is not
	// currently connected must succeed without error.
	Disconnect(ctx context.Context, subsysNQN string) error

	// GetDevicePath returns the /dev/nvmeXnY block-device path for the given
	// subsystem NQN after a successful Connect.  Returns ("", nil) if the
	// device is not yet visible; callers should poll until it appears or a
	// deadline is exceeded.
	GetDevicePath(ctx context.Context, subsysNQN string) (string, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// connectorProtocolHandlerAdapter
// ─────────────────────────────────────────────────────────────────────────────

// connectorProtocolHandlerAdapter adapts the legacy Connector interface to the
// ProtocolHandler interface (RFC §5.4.2).  It is used internally by the
// Connector-based constructors (NewNodeServerWithStateDir,
// NewNodeServerWithStateMachine) so that NodeStageVolume and NodeUnstageVolume
// can use the new handlers map uniformly without requiring every existing unit
// test to migrate away from the legacy Connector mocks.
//
// Attach calls connector.Connect followed by a poll of connector.GetDevicePath,
// mirroring the behavior previously embedded in NodeStageVolume.
// Detach calls connector.Disconnect with the NQN from the NVMeoFProtocolState.
// Rescan is a no-op because the legacy Connector has no rescan API.
type connectorProtocolHandlerAdapter struct {
	conn         Connector
	pollTimeout  time.Duration
	pollInterval time.Duration
}

// Ensure connectorProtocolHandlerAdapter satisfies ProtocolHandler at compile time.
var _ ProtocolHandler = (*connectorProtocolHandlerAdapter)(nil)

// Attach implements ProtocolHandler.Attach for the legacy Connector.
// It calls conn.Connect then polls conn.GetDevicePath until the device appears
// or the deadline is exceeded.
func (a *connectorProtocolHandlerAdapter) Attach(ctx context.Context, params AttachParams) (*AttachResult, error) {
	connectOpts, optsErr := ParseNVMeoFConnectOptions(params.Extra)
	if optsErr != nil {
		return nil, fmt.Errorf("connect: %w", optsErr)
	}
	connErr := a.conn.Connect(ctx, params.ConnectionID, params.Address, params.Port, connectOpts)
	if connErr != nil {
		return nil, fmt.Errorf("connect: %w", connErr)
	}

	// Poll for the block device path.
	pollCtx, pollCancel := context.WithTimeout(ctx, a.pollTimeout)
	defer pollCancel()

	for {
		devPath, devErr := a.conn.GetDevicePath(pollCtx, params.ConnectionID)
		if devErr != nil {
			return nil, fmt.Errorf("get device path: %w", devErr)
		}
		if devPath != "" {
			return &AttachResult{
				DevicePath: devPath,
				State: &NVMeoFProtocolState{
					SubsysNQN: params.ConnectionID,
					Address:   params.Address,
					Port:      params.Port,
				},
			}, nil
		}
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("timed out waiting for block device: %w", pollCtx.Err())
		case <-time.After(a.pollInterval):
			// next iteration
		}
	}
}

// Detach implements ProtocolHandler.Detach for the legacy Connector.
func (a *connectorProtocolHandlerAdapter) Detach(ctx context.Context, state ProtocolState) error {
	nvmeState, ok := state.(*NVMeoFProtocolState)
	if !ok || nvmeState == nil {
		return fmt.Errorf("connectorProtocolHandlerAdapter Detach: expected *NVMeoFProtocolState, got %T", state)
	}
	err := a.conn.Disconnect(ctx, nvmeState.SubsysNQN)
	if err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}
	return nil
}

// Rescan implements ProtocolHandler.Rescan for the legacy Connector.
// The legacy Connector has no rescan API so this is always a no-op.
func (*connectorProtocolHandlerAdapter) Rescan(_ context.Context, _ ProtocolState) error {
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// handlersFromConnector — bridge for Connector-based constructors
// ─────────────────────────────────────────────────────────────────────────────

// handlersFromConnector wraps conn in a connectorProtocolHandlerAdapter keyed
// as "nvmeof-tcp" and returns the resulting handler map.  A nil connector
// produces a nil map (no handlers registered), which is safe for constructors
// that pass nil for state-only test servers that never call NodeStageVolume.
func handlersFromConnector(conn Connector) map[string]ProtocolHandler {
	if conn == nil {
		return nil
	}
	return map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: &connectorProtocolHandlerAdapter{
			conn:         conn,
			pollTimeout:  deviceWaitTimeout,
			pollInterval: devicePollInterval,
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// resolveProtocolType — extract protocol type for handler dispatch
// ─────────────────────────────────────────────────────────────────────────────

// knownProtocolTypes is the set of valid protocol-type string values recognized
// by this driver.  It is used by resolveProtocolType to validate protocol-type
// strings extracted from volumeID path components (which could be any string).
var knownProtocolTypes = map[string]struct{}{
	ProtocolNVMeoFTCP: {},
	ProtocolNFS:       {},
}

// resolveProtocolType derives the storage protocol type for the given volume.
//
// Resolution order (first non-empty, valid value wins):
//  1. VolumeContext["pillar-csi.bhyoo.com/protocol-type"] — set by the
//     controller in CreateVolume for all newly provisioned volumes.
//  2. volumeID path component — the volumeID format used by this driver is
//     "<target-name>/<protocol-type>/<backend-type>/<agent-vol-id>"; when the
//     second slash-separated component is a known protocol type it is used.
//  3. Default: "nvmeof-tcp" — backward-compatible fallback for volumes
//     provisioned before Phase 2 that do not carry a protocol-type in their
//     VolumeContext, and for unit tests that use simplified volumeID strings.
func resolveProtocolType(volumeID string, volCtx map[string]string) string {
	// 1. Explicit VolumeContext key (preferred; always present for new volumes).
	if pt := volCtx[VolumeContextKeyProtocolType]; pt != "" {
		return pt
	}

	// 2. Parse from volumeID: <target>/<protocol-type>/<backend-type>/<agent-vol-id>
	// SplitN with limit 4 keeps the last segment intact (it may contain slashes).
	parts := strings.SplitN(volumeID, "/", 4)
	if len(parts) >= 2 {
		candidate := parts[1]
		if _, known := knownProtocolTypes[candidate]; known {
			return candidate
		}
	}

	// 3. Backward-compatible default.
	return ProtocolNVMeoFTCP
}

// Mounter is the interface for filesystem-level mount/unmount operations.
// A real implementation shells out to mount(8) / umount(8) or uses the
// kubernetes.io/utils/mount package.  A test implementation records calls
// without touching the filesystem.
type Mounter interface {
	// FormatAndMount formats the block device at source with the given
	// filesystem type and mkfs arguments (formatOptions) if — and only if —
	// it carries no filesystem yet, then mounts it at target.  An existing
	// filesystem is never reformatted; formatOptions are ignored for it.
	// fsType must be a kernel-supported filesystem name, e.g. "ext4" or "xfs".
	// options are passed verbatim as -o flags to mount(8); formatOptions are
	// separate mkfs argv elements (no shell).
	FormatAndMount(ctx context.Context, source, target, fsType string, options, formatOptions []string) error

	// Mount performs a plain mount of source at target with the given type and
	// options.  Callers use this for bind mounts (source already formatted).
	Mount(source, target, fsType string, options []string) error

	// Unmount unmounts the path at target.
	// Implementations must be idempotent: unmounting a path that is not
	// currently mounted must succeed without error, and a target whose
	// mount probe reports a corrupted mount (e.g. stat EIO on an aborted
	// filesystem after its backing device disappeared) must still be
	// unmounted.  Any other probe or unmount failure must be returned to
	// the caller — cleanup callers must never treat a failed probe as
	// "not mounted".
	Unmount(target string) error

	// CheckMountHealth verifies that the filesystem mounted at target can
	// still serve writes.  MountEntryExists only proves a mount exists; a
	// mounted filesystem whose kernel instance was shut down (XFS forced
	// shutdown, ext4 abort) keeps its mount entry while every real operation
	// fails.  Returns nil for a live filesystem, an error wrapping
	// ErrMountUnhealthy for a dead one, and any other error when the probe
	// itself fails.
	//
	// The probe is a metadata write: a mount that was deliberately mounted
	// read-only ("ro" mount flag or a read-only bind) answers it with
	// EROFS, which CheckMountHealth cannot tell apart from the ext4
	// remount-ro abort signature — both report ErrMountUnhealthy.  Callers
	// must not probe mounts whose configuration is read-only by request;
	// they use CheckMountReadable.
	CheckMountHealth(target string) error

	// CheckMountReadable is the non-writing liveness probe for mounts that
	// must not be write-probed (staged read-only, NFS, raw block binds): it
	// stats target and returns nil when the kernel answers, an error
	// wrapping ErrMountUnhealthy for a dead mount (EIO from a shut-down
	// filesystem, ESTALE/ENOTCONN from a lost server), and any other error
	// when the probe itself fails.  It cannot detect an ext4 remount-ro
	// abort — stat keeps working there — which is why writable mounts use
	// CheckMountHealth instead.
	CheckMountReadable(target string) error

	// HasOtherMounts reports whether the filesystem mounted at target is
	// referenced by at least one more mount in this mount namespace (pod
	// bind mounts of the staging path).  When it returns true, unmounting
	// target does not detach the filesystem — a fresh mount would silently
	// re-attach the same superblock.  An error is inconclusive.
	HasOtherMounts(target string) (bool, error)

	// MountSource returns the mountinfo source of the filesystem mounted at
	// target — the device path NodePublishVolume needs to re-mount a staged
	// filesystem whose stage record predates the DevicePath field.  An error
	// is inconclusive.
	MountSource(target string) (string, error)

	// MountEntryExists reports whether target has a mount table entry,
	// consulting only the mount namespace (proc mountinfo on Linux).  It is
	// the only "is it mounted" query: it never stats the mounted
	// filesystem, so a kernel-shutdown mount — whose stat(2) fails with EIO
	// (issue #175) — still answers true and reaches the health probe and
	// repair.  A stat-based check would wedge every caller on exactly the
	// mounts that need repair.  Callers that only want to remove a mount
	// call the idempotent Unmount directly.  An error means the mount table
	// is unreadable and is inconclusive.
	MountEntryExists(target string) (bool, error)
}

// NodeServer implements csi.NodeServer.  It handles the per-node portion of
// the CSI lifecycle: connecting volumes via ProtocolHandler, formatting
// filesystems, and bind-mounting into pod target paths.
//
// All privileged operations are delegated to the injectable ProtocolHandler
// and Mounter interfaces so that the server can be tested without kernel
// modules or root privileges.
//
// When sm is non-nil the NodeServer consults the VolumeStateMachine before
// executing any operation and returns gRPC FailedPrecondition for out-of-order
// requests.  When sm is nil (the default) no state-machine validation is
// performed and the existing file-based idempotency logic is the sole guard —
// this preserves backward compatibility with unit tests that do not exercise
// the full controller→node ordering path.
type NodeServer struct {
	csi.UnimplementedNodeServer

	// nodeID is the unique identifier for this Kubernetes node.  It is
	// included in NodeGetInfo responses and used for topology key labeling.
	nodeID string

	// handlers maps protocol-type string (e.g. "nvmeof-tcp") to the
	// corresponding ProtocolHandler implementation.  NodeStageVolume dispatches
	// to the handler matched by the volume's protocol type and calls Attach.
	// NodeUnstageVolume calls Detach with the persisted stage state.
	//
	// Backward-compatible constructors (NewNodeServerWithStateDir,
	// NewNodeServerWithStateMachine) wrap the legacy Connector argument in a
	// connectorProtocolHandlerAdapter keyed as "nvmeof-tcp".
	handlers map[string]ProtocolHandler

	// mounter performs filesystem format and bind-mount operations.
	mounter Mounter

	// stateDir is the directory in which per-volume staging state files are
	// persisted.  Each staged volume has a JSON file named after its (sanitized)
	// volumeID in this directory.  NodeUnstageVolume reads the file to recover
	// the subsystem NQN without requiring the VolumeContext that was available
	// during NodeStageVolume.
	//
	// Defaults to /var/lib/pillar-csi/node; override via NewNodeServerWithStateDir
	// for testing.
	stateDir string

	// sm is an optional VolumeStateMachine shared with the ControllerServer.
	// When non-nil every node operation validates the volume's current
	// lifecycle state before executing privileged work, rejecting out-of-order
	// RPCs with FailedPrecondition.
	sm *VolumeStateMachine

	// driverName scopes this node instance to one CSI identity.  Empty keeps
	// the historical block driver identity; file adoption requires the
	// explicitly configured files driver and never silently downgrades.
	driverName string

	// filesystemHostRoot is the host mount prefix visible inside the node
	// container.  It is used only for explicitly trusted source paths.
	filesystemHostRoot string

	// resizer performs online filesystem expand operations in NodeExpandVolume.
	// When nil, NodeExpandVolume falls back to the default exec-based Resizer
	// (resize2fs for ext4, xfs_growfs for xfs).  Override in tests via
	// WithResizer to inject a mock without requiring real resize tools.
	resizer Resizer
	// fileStatsFn revalidates an adopted filesystem's native identity and
	// exact recorded capacity through the controller/agent connection.
	fileStatsFn func(context.Context, string, *FileStageState) (*csi.NodeGetVolumeStatsResponse, error)

	// statFn is the function used by NodeGetVolumeStats to stat the volume
	// path and determine whether it is a block device or a filesystem mount.
	// When nil, os.Stat is used.  Override in tests to simulate block devices
	// without requiring root privileges or a real kernel block device.
	statFn func(string) (os.FileInfo, error)

	// blockDeviceSizeFn is the function used by NodeGetVolumeStats to read
	// the total capacity of a raw block device via the BLKGETSIZE64 ioctl.
	// When nil, linuxBlockDeviceSize is used.  Override in tests to return a
	// synthetic size without opening a real block device.
	blockDeviceSizeFn func(string) (int64, error)

	// topologyProber determines which storage protocols are available on this
	// node and is consulted by NodeGetInfo to build AccessibleTopology.
	// When nil, the default sysfsProber is used (checks kernel modules,
	// system binaries, and config files on the real host).
	// Override in tests via WithTopologyProber to inject a mock prober.
	topologyProber ProtocolProber

	// dm manages the device-mapper linear targets that hold the backend
	// device of a local attach (see devicemapper.go).  When nil,
	// NewExecDeviceMapper is used.  Override via WithDeviceMapper.
	dm DeviceMapper

	// nvmetRoot is the nvmet configfs root the local attach export check
	// reads (see nvmet_export_state.go).  When empty,
	// DefaultNvmetConfigfsRoot is used.  Override via WithNvmetConfigfsRoot.
	nvmetRoot string

	// lioRoot is the LIO configfs root the local attach export check of
	// iSCSI volumes reads (see lio_export_state.go).  When empty,
	// DefaultLIOConfigfsRoot is used.  Override via WithLIOConfigfsRoot.
	lioRoot string

	// dmTargetPresentFn reports whether a device-mapper target exists; it
	// gates the orphan-claim cleanup of NodeUnstageVolume without a stage
	// state file.  When nil, the target is looked up in sysfs.  Override in
	// tests.
	dmTargetPresentFn func(name string) (bool, error)

	// volumeLocks serializes, per volume ID, NodeStageVolume,
	// NodeUnstageVolume, NodePublishVolume, NodeUnpublishVolume,
	// NodeExpandVolume and every periodic trim chunk (see trim.go).  Publish
	// and unpublish join the same lock because the stage repair path drops
	// and re-mounts the staged filesystem a bind mount references (issue
	// #168).  The zero value is ready to use.
	volumeLocks volumeLockSet

	// pageSize is the memory page size NodeStageVolume checks an NVMe-oF
	// max data transfer size against (see CheckNVMeoFTransferSizePageSize).
	// Zero selects os.Getpagesize(); tests set it directly.
	pageSize int
}

// workerPageSize returns n.pageSize, or the page size of this node.
func (n *NodeServer) workerPageSize() int {
	if n.pageSize > 0 {
		return n.pageSize
	}
	return os.Getpagesize()
}

// Ensure NodeServer satisfies the interface at compile time.
var _ csi.NodeServer = (*NodeServer)(nil)

// NewNodeServerWithConnector constructs a NodeServer with the given node
// identity and a legacy Connector backend.  The staging state directory
// defaults to /var/lib/pillar-csi/node.
//
//   - nodeID     – Kubernetes node name returned verbatim by NodeGetInfo.NodeId
//     (RFC §5.1 stable node handle, e.g. "worker-1").
//   - connector  – NVMe-oF connect/disconnect implementation.
//   - mounter    – filesystem format/mount/unmount implementation.
//
// Deprecated: use NewNodeServer(nodeID, handlers, mounter) instead.
func NewNodeServerWithConnector(nodeID string, connector Connector, mounter Mounter) *NodeServer {
	return NewNodeServerWithStateDir(nodeID, connector, mounter, defaultStateDir)
}

// NewNodeServerWithStateDir constructs a NodeServer with an explicit staging
// state directory.  Use this variant in tests to point the state dir at a
// t.TempDir() so that staging state is isolated between test cases and does
// not require /var/lib to exist.
//
// The connector is automatically wrapped in a connectorProtocolHandlerAdapter
// and stored as the "nvmeof-tcp" handler.  Existing tests that pass a legacy
// Connector mock continue to work unchanged: their Connect/Disconnect/GetDevicePath
// calls are transparently delegated by the adapter.
//
//   - nodeID    – unique node name used in NodeGetInfo.
//   - connector – NVMe-oF connect/disconnect implementation.
//   - mounter   – filesystem format/mount/unmount implementation.
//   - stateDir  – directory for per-volume JSON state files; created on first
//     use if absent.
func NewNodeServerWithStateDir(nodeID string, connector Connector, mounter Mounter, stateDir string) *NodeServer {
	return &NodeServer{
		nodeID:   nodeID,
		handlers: handlersFromConnector(connector),
		mounter:  mounter,
		stateDir: stateDir,
	}
}

// NewNodeServerWithStateMachine constructs a NodeServer that shares the given
// VolumeStateMachine with the ControllerServer.  With a shared SM every node
// operation validates the volume's current lifecycle state before executing
// privileged work, returning FailedPrecondition for out-of-order requests
// (e.g. NodeStageVolume before ControllerPublishVolume).
//
// The connector is automatically wrapped in a connectorProtocolHandlerAdapter
// and stored as the "nvmeof-tcp" handler (see NewNodeServerWithStateDir).
//
// Use this constructor in end-to-end tests that verify cross-component
// ordering.  For unit tests that exercise the node in isolation, use
// NewNodeServerWithStateDir (sm = nil → no ordering validation).
//
//   - nodeID    – unique node name used in NodeGetInfo.
//   - connector – NVMe-oF connect/disconnect implementation.
//   - mounter   – filesystem format/mount/unmount implementation.
//   - stateDir  – directory for per-volume JSON state files.
//   - sm        – shared VolumeStateMachine; must not be nil.
func NewNodeServerWithStateMachine(
	nodeID string,
	connector Connector,
	mounter Mounter,
	stateDir string,
	sm *VolumeStateMachine,
) *NodeServer {
	return &NodeServer{
		nodeID:   nodeID,
		handlers: handlersFromConnector(connector),
		mounter:  mounter,
		stateDir: stateDir,
		sm:       sm,
	}
}

// NewNodeServer constructs a NodeServer accepting a protocol-handler map.
// This overload replaces the legacy Connector-based signature and is used by
// the production node binary (cmd/node/main.go) after the Phase 2 migration.
//
//   - nodeID   – Kubernetes node name returned verbatim by NodeGetInfo.
//   - handlers – map from protocol-type string to ProtocolHandler implementation.
//   - mounter  – filesystem format/mount/unmount implementation.
//
// Deprecated overloads (Connector-based) remain available for existing unit
// tests; they are removed once all callers migrate to the handlers map.
func NewNodeServer(nodeID string, handlers map[string]ProtocolHandler, mounter Mounter) *NodeServer {
	return &NodeServer{
		nodeID:   nodeID,
		handlers: handlers,
		mounter:  mounter,
		// Prefer the E2E suite workspace state dir when running under tests;
		// falls back to defaultStateDir (/var/lib/pillar-csi/node) in production.
		stateDir: runtimepaths.ResolveNodeStateDir(defaultStateDir),
	}
}

// WithStateDir scopes every stage-state reader and writer, including session
// restore, trim and transfer-limit reconciliation, to one driver's directory.
func (n *NodeServer) WithStateDir(dir string) *NodeServer {
	n.stateDir = dir
	return n
}

// WithDriverName scopes the node runtime to the identity selected by the
// binary.  Existing constructors intentionally retain the old driver identity;
// callers that serve files.pillar-csi.bhyoo.com must opt in explicitly.
func (n *NodeServer) WithDriverName(name string) *NodeServer {
	n.driverName = name
	return n
}

// WithFilesystemHostRoot configures the host mount root visible to the node
// container.  The root is never inferred from a CSI request.
func (n *NodeServer) WithFilesystemHostRoot(root string) *NodeServer {
	n.filesystemHostRoot = root
	return n
}

// WithFilesystemStatsReader injects the read-only identity/quota gateway used
// by NodeGetVolumeStats for adopted filesystems. A missing reader is surfaced
// as an error rather than falling back to pool capacity or stale statfs data.
func (n *NodeServer) WithFilesystemStatsReader(
	reader func(context.Context, string, *FileStageState) (*csi.NodeGetVolumeStatsResponse, error),
) *NodeServer {
	n.fileStatsFn = reader
	return n
}

// Register wires the NodeServer into the provided gRPC server.
func (n *NodeServer) Register(g *grpc.Server) {
	csi.RegisterNodeServer(g, n)
}

// NodeGetCapabilities returns the set of Node service capabilities that this
// plugin supports per the CSI specification.
//
// Advertised capabilities:
//   - STAGE_UNSTAGE_VOLUME: NodeStageVolume / NodeUnstageVolume are implemented
//     (NVMe-oF connect + format at a staging path).
//   - EXPAND_VOLUME: NodeExpandVolume is implemented (resize filesystem after
//     a ControllerExpandVolume call).
func (*NodeServer) NodeGetCapabilities(
	_ context.Context,
	_ *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	}

	nodeCaps := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		nodeCaps = append(nodeCaps, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: c,
				},
			},
		})
	}

	return &csi.NodeGetCapabilitiesResponse{Capabilities: nodeCaps}, nil
}

// NodeGetInfo returns identifying information about this node that the CO
// (Container Orchestrator) uses for topology-aware scheduling and volume
// placement decisions.
//
// The response contains:
//   - NodeId: the Kubernetes node name — the stable node handle used by this
//     driver.  Per RFC §5.1, node_id is NOT a transport-level identity (NVMe
//     host NQN, iSCSI initiator IQN, etc.).  Protocol-specific identities are
//     published separately as CSINode annotations so that the controller can
//     look them up by node name.
//
// MaxVolumesPerNode is left at 0 (unlimited).
// AccessibleTopology reports which storage protocols are available on this node.
func (n *NodeServer) NodeGetInfo(
	_ context.Context,
	_ *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	if n.nodeID == "" {
		return nil, status.Error(codes.Internal, "node server has no node ID configured") //nolint:wrapcheck
	}

	// ── Resolve topology prober ──────────────────────────────────────────────
	// Use the injected prober when available (tests); fall back to the
	// production sysfsProber that inspects kernel modules and system binaries.
	prober := n.topologyProber
	if prober == nil {
		prober = &sysfsProber{}
	}

	// ── Build AccessibleTopology ─────────────────────────────────────────────
	// RFC §5.8: report which storage protocols are available on this node so
	// that the CO can schedule volumes only on protocol-capable nodes.
	// Only include topology keys for protocols that are actually available;
	// omit unavailable protocols so StorageClass allowedTopologies selectors
	// (using In/NotIn operators) work correctly with sparse maps.
	segs := buildNodeTopologySegments(prober, n.effectiveDriverName(), n.nodeID)

	var topology *csi.Topology
	if len(segs) > 0 {
		topology = &csi.Topology{Segments: segs}
	}

	return &csi.NodeGetInfoResponse{
		NodeId: n.nodeID,
		// MaxVolumesPerNode: 0 means unlimited (CSI spec default).
		AccessibleTopology: topology,
	}, nil
}

// fileStageContext is the trusted, immutable portion of an adopted filesystem
// handoff.  It is parsed from controller-produced context rather than from the
// CSI volume ID or user-supplied target paths.
type fileStageContext struct {
	adoption filesystemContextAdoption
	capacity int64
	proxy    string
	local    bool
	state    *FileStageState
}

func (n *NodeServer) effectiveDriverName() string {
	if n.driverName == "" {
		return "pillar-csi.bhyoo.com"
	}
	return n.driverName
}

func (n *NodeServer) parseFileStageContext(req *csi.NodeStageVolumeRequest) (*fileStageContext, error) {
	vc, pc := req.GetVolumeContext(), req.GetPublishContext()
	raw := filesystemAdoptionContext(vc, pc)
	if raw == "" {
		return nil, fmt.Errorf("filesystem adoption context is missing")
	}
	driverErr := n.validateFilesystemAdoptionDriver(vc, pc)
	if driverErr != nil {
		return nil, driverErr
	}
	adoption, err := parseFilesystemContextAdoption(raw)
	if err != nil {
		return nil, err
	}
	capacity, err := parseFileStageCapacity(vc, pc)
	if err != nil {
		return nil, err
	}
	layout, err := parseFileStageLayout(vc, pc)
	if err != nil {
		return nil, err
	}
	fileState, err := newFileStageState(req, pc, adoption, capacity, layout)
	if err != nil {
		return nil, err
	}
	if fileState.AgentEndpoint != "" {
		err = validateFilesystemAgentEndpoint(fileState.AgentEndpoint)
		if err != nil {
			return nil, fmt.Errorf("filesystem inspection endpoint: %w", err)
		}
	}
	proxy, err := n.parseFilesystemProxy(pc)
	if err != nil {
		return nil, err
	}
	if proxy == "" {
		return &fileStageContext{adoption: adoption, capacity: capacity, state: fileState}, nil
	}
	fileState.ProxyPath = proxy
	fileState.Local = true
	return &fileStageContext{
		adoption: adoption, capacity: capacity, proxy: proxy, local: true, state: fileState,
	}, nil
}

func filesystemAdoptionContext(vc, pc map[string]string) string {
	raw := pc[PublishContextKeyFilesystemAdoption]
	if raw == "" {
		raw = vc[VolumeContextKeyFilesystemAdoption]
	}
	return raw
}

func (n *NodeServer) validateFilesystemAdoptionDriver(vc, pc map[string]string) error {
	driver := vc[VolumeContextKeyCSIDriver]
	if driver == "" {
		driver = pc[VolumeContextKeyCSIDriver]
	}
	if driver != "" && driver != "files.pillar-csi.bhyoo.com" {
		return status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: filesystem adoption belongs to CSI driver %q", driver)
	}
	if n.effectiveDriverName() != "files.pillar-csi.bhyoo.com" {
		return status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: filesystem adoption requires CSI driver %q, node serves %q",
			"files.pillar-csi.bhyoo.com", n.effectiveDriverName())
	}
	return nil
}

func newFileStageState(
	req *csi.NodeStageVolumeRequest,
	pc map[string]string,
	adoption filesystemContextAdoption,
	capacity int64,
	layout *agentv1.BackendParams,
) (*FileStageState, error) {
	fileState := &FileStageState{
		VolumeID: req.GetVolumeId(), Kind: adoption.Kind, HostPath: adoption.HostPath,
		CanonicalSource: adoption.CanonicalSource, ResourceID: adoption.ResourceID,
		FilesystemType: adoption.FilesystemType, FilesystemID: adoption.FilesystemID,
		Inode: adoption.Inode, ProjectID: adoption.ProjectID, CapacityBytes: capacity,
		AgentEndpoint: pc[fileContextAgentAddress], AgentName: pc[fileContextAgentName],
		AgentVolumeID: pc[fileContextAgentVolumeID],
	}
	err := populateFileStageBackend(fileState, adoption.Kind, layout)
	return fileState, err
}

func (n *NodeServer) parseFilesystemProxy(pc map[string]string) (string, error) {
	proxy := pc[PublishContextKeyFilesystemProxyPath]
	if proxy == "" {
		return "", nil
	}
	if !filepath.IsAbs(proxy) || filepath.Clean(proxy) != proxy || strings.ContainsRune(proxy, '\x00') {
		return "", fmt.Errorf("filesystem proxy path %q is not canonical", proxy)
	}
	node := pc[PublishContextKeyFilesystemLocalNode]
	if node != "" && node != n.nodeID {
		return "", status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: filesystem publish targets node %q, this node is %q", node, n.nodeID)
	}
	return proxy, nil
}

func parseFilesystemContextAdoption(raw string) (filesystemContextAdoption, error) {
	var adoption filesystemContextAdoption
	err := json.Unmarshal([]byte(raw), &adoption)
	if err != nil {
		return adoption, fmt.Errorf("decode filesystem adoption context: %w", err)
	}
	if adoption.Kind == "" || adoption.CanonicalSource == "" || adoption.ResourceID == "" ||
		adoption.FilesystemType == "" {
		return adoption, fmt.Errorf("filesystem adoption context has incomplete native identity")
	}
	return adoption, nil
}

func parseFileStageCapacity(vc, pc map[string]string) (int64, error) {
	rawCapacity := pc[PublishContextKeyFilesystemCapacity]
	if rawCapacity == "" {
		rawCapacity = vc[VolumeContextKeyFilesystemCapacity]
	}
	capacity, err := strconv.ParseInt(rawCapacity, 10, 64)
	if err != nil || capacity <= 0 {
		return 0, fmt.Errorf("filesystem adoption exact capacity %q is invalid", rawCapacity)
	}
	return capacity, nil
}

func parseFileStageLayout(vc, pc map[string]string) (*agentv1.BackendParams, error) {
	layoutJSON := pc[PublishContextKeyFilesystemLayout]
	if layoutJSON == "" {
		layoutJSON = vc[VolumeContextKeyFilesystemLayout]
	}
	if layoutJSON == "" {
		return nil, fmt.Errorf("filesystem adoption context is missing trusted backend layout")
	}
	var layout agentv1.BackendParams
	err := protojson.Unmarshal([]byte(layoutJSON), &layout)
	if err != nil {
		return nil, fmt.Errorf("decode trusted filesystem backend layout: %w", err)
	}
	if layout.GetDirectory() == nil && layout.GetZfs() == nil {
		return nil, fmt.Errorf("trusted filesystem backend layout is empty")
	}
	return &layout, nil
}

func populateFileStageBackend(
	state *FileStageState, kind string, layout *agentv1.BackendParams,
) error {
	switch kind {
	case "directory":
		params := layout.GetDirectory()
		if params == nil || params.GetLogicalPool() == "" || params.GetHostRoot() == "" {
			return fmt.Errorf("directory adoption requires its trusted logical pool and host root")
		}
		state.BackendType = "directory"
		state.PoolName = params.GetLogicalPool()
		state.ExpectedHostRoot = params.GetHostRoot()
	case "zfs-dataset":
		params := layout.GetZfs()
		if params == nil || params.GetPool() == "" {
			return fmt.Errorf("ZFS adoption requires its trusted pool and parent dataset")
		}
		state.BackendType = "zfs-dataset"
		state.PoolName = params.GetPool()
		state.ExpectedParentDataset = params.GetParentDataset()
	default:
		return fmt.Errorf("unsupported filesystem adoption kind %q", kind)
	}
	return nil
}

// refreshFileStageMetadata backfills additive inspection routing on a verified
// stage without allowing a restage to change its native identity or bound.
func refreshFileStageMetadata(existing *nodeStageState, current *FileStageState) error {
	previous := existing.File
	if previous != nil && !sameFileStageMetadata(previous, current) {
		return fmt.Errorf(
			"recorded filesystem identity, exact capacity or backend layout differs from the current publish context",
		)
	}
	existing.File = current
	return nil
}

func sameFileStageMetadata(previous, current *FileStageState) bool {
	if !sameFileStageIdentity(previous, current) {
		return false
	}
	if previous.Kind != "" && !sameFileStageKindMetadata(previous, current) {
		return false
	}
	if previous.PoolName != "" && !sameFileStageBackendMetadata(previous, current) {
		return false
	}
	return previous.AgentVolumeID == "" || previous.AgentVolumeID == current.AgentVolumeID
}

func sameFileStageIdentity(previous, current *FileStageState) bool {
	return previous.CanonicalSource == current.CanonicalSource &&
		previous.ResourceID == current.ResourceID &&
		previous.FilesystemType == current.FilesystemType &&
		previous.FilesystemID == current.FilesystemID &&
		previous.Inode == current.Inode &&
		previous.ProjectID == current.ProjectID &&
		previous.CapacityBytes == current.CapacityBytes
}

func sameFileStageKindMetadata(previous, current *FileStageState) bool {
	return previous.Kind == current.Kind && previous.HostPath == current.HostPath
}

func sameFileStageBackendMetadata(previous, current *FileStageState) bool {
	return previous.PoolName == current.PoolName &&
		previous.BackendType == current.BackendType &&
		previous.ExpectedParentDataset == current.ExpectedParentDataset &&
		previous.ExpectedHostRoot == current.ExpectedHostRoot
}

func (n *NodeServer) stageLocalFilesystem(
	req *csi.NodeStageVolumeRequest, fileCtx *fileStageContext,
) (*csi.NodeStageVolumeResponse, error) {
	err := validateLocalFilesystemStage(n, req, fileCtx)
	if err != nil {
		return nil, err
	}
	volumeID, target := req.GetVolumeId(), req.GetStagingTargetPath()
	existing, err := n.readStageState(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeStageVolume: read file stage state: %v", err)
	}
	reused, err := n.reuseLocalFilesystemStage(existing, fileCtx, target, volumeID)
	if err != nil {
		return nil, err
	}
	if reused {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	err = n.mountLocalFilesystemStage(fileCtx, target)
	if err != nil {
		return nil, err
	}
	state := &nodeStageState{
		ProtocolType: ProtocolNFS,
		AccessType:   AccessTypeFilesystem,
		VolumeID:     volumeID, StagingPath: target,
		File: fileCtx.state,
	}
	err = n.writeStageState(volumeID, state)
	if err != nil {
		cleanupErr := n.mounter.Unmount(target)
		if cleanupErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: persist file stage: %v; cleanup failed: %v", err, cleanupErr)
		}
		return nil, status.Errorf(codes.Internal, "NodeStageVolume: persist file stage: %v", err)
	}
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateNodeStaged)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func validateLocalFilesystemStage(
	n *NodeServer, req *csi.NodeStageVolumeRequest, fileCtx *fileStageContext,
) error {
	if !fileCtx.local {
		return status.Errorf(codes.FailedPrecondition,
			"%s", "NodeStageVolume: file adoption requires a controller-provided local proxy path")
	}
	if req.GetVolumeCapability().GetMount() == nil || req.GetVolumeCapability().GetBlock() != nil {
		return status.Errorf(codes.InvalidArgument,
			"%s", "NodeStageVolume: adopted filesystems require mount access")
	}
	if fsType := req.GetVolumeCapability().GetMount().GetFsType(); fsType != "" &&
		fsType != fileCtx.adoption.FilesystemType && !nfsTransportFsTypeHint(req) {
		return status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: filesystem type %q does not match adopted type %q",
			fsType, fileCtx.adoption.FilesystemType)
	}
	if n.sm != nil {
		state := n.sm.GetState(req.GetVolumeId())
		switch state {
		case StateControllerPublished, StateNodeStagePartial, StateNodeStaged:
		default:
			return status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeStageVolume is not valid in state %s", req.GetVolumeId(), state)
		}
	}
	return nil
}

// nfsTransportFsTypeHint reports whether the capability fsType is the "nfs"
// transport type of a network-shareable adoption. One PV serves the storage
// node through a local bind and remote consumers through NFS, so its csi.fsType
// names the remote transport, not the native filesystem the bind exposes. The
// hint is accepted only when the volume routes to NFS and satisfies the same
// contract a remote NFS stage enforces; the native type and identity are still
// verified against the adoption record before and after the bind.
func nfsTransportFsTypeHint(req *csi.NodeStageVolumeRequest) bool {
	mount := req.GetVolumeCapability().GetMount()
	return mount.GetFsType() == ProtocolNFS &&
		resolveProtocolType(req.GetVolumeId(), req.GetVolumeContext()) == ProtocolNFS &&
		validateNFSStageFilesystem(req.GetVolumeContext(), mount) == nil
}

func (n *NodeServer) reuseLocalFilesystemStage(
	existing *nodeStageState, fileCtx *fileStageContext,
	target, volumeID string,
) (bool, error) {
	present, err := validateExistingLocalFilesystemStage(existing, target)
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	mounted, err := n.existingLocalFilesystemMounted(target)
	if err != nil {
		return false, err
	}
	if !mounted {
		return false, nil
	}
	err = validateExistingLocalFilesystemIdentity(existing, fileCtx)
	if err != nil {
		return false, err
	}
	verifyErr := n.verifyExistingLocalFilesystemStage(existing, fileCtx, target)
	if verifyErr != nil {
		return false, status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: existing file stage mount changed: %v", verifyErr)
	}
	err = refreshFileStageMetadata(existing, fileCtx.state)
	if err != nil {
		return false, status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: volume %q: %v", volumeID, err)
	}
	err = n.writeStageState(volumeID, existing)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"NodeStageVolume: re-persist file stage %q: %v", volumeID, err)
	}
	return true, nil
}

func validateExistingLocalFilesystemStage(existing *nodeStageState, target string) (bool, error) {
	if existing == nil || existing.File == nil {
		return false, nil
	}
	if existing.StagingPath != "" && existing.StagingPath != target {
		return false, status.Errorf(codes.FailedPrecondition,
			"%s", "NodeStageVolume: existing filesystem stage uses a different staging path")
	}
	return true, nil
}

// existingLocalFilesystemMounted decides presence from the mount table and,
// like the other configurations that skip the write probe, never reports a
// dead stage as mounted: an unreadable bind is an Internal error.
func (n *NodeServer) existingLocalFilesystemMounted(target string) (bool, error) {
	mounted, err := n.mounter.MountEntryExists(target)
	if err != nil {
		return false, status.Errorf(codes.Internal,
			"NodeStageVolume: check file stage mount: %v", err)
	}
	if !mounted {
		return false, nil
	}
	readErr := n.mounter.CheckMountReadable(target)
	if readErr != nil {
		return false, status.Errorf(codes.Internal,
			"NodeStageVolume: file stage mount %q is not usable: %v", target, readErr)
	}
	return true, nil
}

func validateExistingLocalFilesystemIdentity(
	existing *nodeStageState, fileCtx *fileStageContext,
) error {
	if existing.File.ResourceID != fileCtx.adoption.ResourceID ||
		existing.File.CapacityBytes != fileCtx.capacity {
		return status.Errorf(codes.FailedPrecondition,
			"%s", "NodeStageVolume: existing file stage identity or capacity differs")
	}
	return nil
}

func (n *NodeServer) verifyExistingLocalFilesystemStage(
	existing *nodeStageState, fileCtx *fileStageContext, target string,
) error {
	source, verifyErr := verifyFilesystemProxyMount(fileCtx.proxy, &fileCtx.adoption)
	if verifyErr == nil && existing.File.ProxyPath != fileCtx.proxy {
		verifyErr = errors.New("controller-owned proxy path changed")
	}
	if verifyErr == nil {
		verifyErr = n.verifyFilesystemHostSource(source, &fileCtx.adoption)
	}
	if verifyErr == nil {
		verifyErr = verifyFilesystemBindMount(fileCtx.proxy, target, &fileCtx.adoption, source)
	}
	return verifyErr
}

func (n *NodeServer) mountLocalFilesystemStage(
	fileCtx *fileStageContext, target string,
) error {
	source, err := verifyFilesystemProxyMount(fileCtx.proxy, &fileCtx.adoption)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: verify adopted filesystem source: %v", err)
	}
	err = n.verifyFilesystemHostSource(source, &fileCtx.adoption)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: verify host filesystem source: %v", err)
	}
	err = n.mounter.Mount(fileCtx.proxy, target, "", []string{"bind"})
	if err != nil {
		return status.Errorf(codes.Internal,
			"NodeStageVolume: bind adopted filesystem %q → %q: %v", fileCtx.proxy, target, err)
	}
	verifyErr := verifyFilesystemBindMount(fileCtx.proxy, target, &fileCtx.adoption, source)
	if verifyErr == nil {
		return nil
	}
	cleanupErr := n.mounter.Unmount(target)
	if cleanupErr != nil {
		return status.Errorf(codes.Internal,
			"NodeStageVolume: post-mount identity failed: %v; cleanup failed: %v",
			verifyErr, cleanupErr)
	}
	return status.Errorf(codes.FailedPrecondition,
		"NodeStageVolume: post-mount identity verification failed: %v", verifyErr)
}

func (n *NodeServer) verifyFilesystemHostSource(source os.FileInfo, adoption *filesystemContextAdoption) error {
	if n.filesystemHostRoot == "" || adoption.Kind != "directory" {
		return nil
	}
	path := filepath.Join(n.filesystemHostRoot, adoption.CanonicalSource)
	err := verifyFilesystemSourceIdentity(path, adoption)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat adopted host filesystem source: %w", err)
	}
	if !os.SameFile(source, info) {
		return errors.New("owned proxy mount differs from the adopted host filesystem root")
	}
	return nil
}

// verifyFilesystemProxyMount rejects an unmounted durable proxy directory,
// including when it happens to live on the expected filesystem. For ZFS the
// kernel mount source must also identify the adopted dataset.
func verifyFilesystemProxyMount(path string, adoption *filesystemContextAdoption) (os.FileInfo, error) {
	err := verifyFilesystemSourceIdentity(path, adoption)
	if err != nil {
		return nil, err
	}
	mounts, err := readMountInfoFile(procMountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read filesystem mount table: %w", err)
	}
	mount, mounted := findMount(mounts, path)
	if !mounted {
		return nil, fmt.Errorf("filesystem proxy %q is not a mount root", path)
	}
	if mount.FsType != adoption.FilesystemType {
		return nil, fmt.Errorf("filesystem mount %q has type %q, expected %q",
			path, mount.FsType, adoption.FilesystemType)
	}
	if adoption.Kind == "zfs-dataset" && mount.Source != adoption.CanonicalSource {
		return nil, fmt.Errorf("filesystem mount %q belongs to dataset %q, expected %q",
			path, mount.Source, adoption.CanonicalSource)
	}
	if adoption.Kind == "zfs-dataset" && mount.Root != "/" {
		return nil, fmt.Errorf("filesystem mount %q exposes dataset subdirectory %q, not its root", path, mount.Root)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat filesystem mount %q: %w", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	var device uint64
	if ok {
		device, ok = filesystemStatDevice(stat)
	}
	if !ok || device != unix.Mkdev(mount.Major, mount.Minor) {
		return nil, fmt.Errorf("filesystem mount %q device no longer matches its mount root", path)
	}
	return info, nil
}

func verifyFilesystemBindMount(
	proxy, target string, adoption *filesystemContextAdoption, source os.FileInfo,
) error {
	current, err := verifyFilesystemProxyMount(proxy, adoption)
	if err != nil {
		return fmt.Errorf("verify owned proxy after bind: %w", err)
	}
	if !os.SameFile(source, current) {
		return errors.New("owned filesystem proxy was replaced during bind")
	}
	staged, err := verifyFilesystemProxyMount(target, adoption)
	if err != nil {
		return fmt.Errorf("verify filesystem bind target: %w", err)
	}
	if !os.SameFile(source, staged) {
		return errors.New("filesystem bind target differs from the owned proxy mount root")
	}
	return nil
}

func verifyFilesystemSourceIdentity(path string, adoption *filesystemContextAdoption) error {
	if path == "" {
		return errors.New("empty source path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve filesystem source %q: %w", path, err)
	}
	if resolved != path {
		return fmt.Errorf("source resolves through symlink to %q", resolved)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat filesystem source %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("filesystem source %q is not a directory", path)
	}
	var filesystem syscall.Statfs_t
	err = syscall.Statfs(path, &filesystem)
	if err != nil {
		return fmt.Errorf("statfs filesystem source %q: %w", path, err)
	}
	var matchesType bool
	switch adoption.FilesystemType {
	case "ext4":
		matchesType = filesystem.Type == 0xef53
	case "xfs":
		matchesType = filesystem.Type == 0x58465342
	case "zfs":
		matchesType = filesystem.Type == 0x2fc12fc1
	default:
		return fmt.Errorf("unsupported adopted filesystem type %q", adoption.FilesystemType)
	}
	if !matchesType {
		return fmt.Errorf("filesystem source %q does not have expected type %q", path, adoption.FilesystemType)
	}
	if adoption.Inode != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Ino != adoption.Inode {
			return fmt.Errorf("inode %d does not match recorded inode %d", statIno(info), adoption.Inode)
		}
	}
	return nil
}

func statIno(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeStageVolume
// ─────────────────────────────────────────────────────────────────────────────.

// NodeStageVolume connects this node to a volume and prepares it for use by pods.
//
// Sequence:
//  1. Validate required fields.
//  2. Resolve the protocol type from VolumeContext or volumeID (RFC §5.4.3).
//  3. Look up the ProtocolHandler registered for the resolved protocol type.
//  4. Perform protocol-specific VolumeContext validation.
//  5. Check idempotency: if the volume is already staged and its staging path
//     is currently mounted, return success immediately.
//  6. Call handler.Attach to establish the transport connection.  Attach is
//     idempotent: calling it on an already-connected target is a no-op.
//  7. Format-and-mount (for MOUNT access type) or bind-mount the raw device
//     (for BLOCK access type) at the staging target path.
//  8. Persist a JSON state file in stateDir recording the protocol state so
//     that NodeUnstageVolume can disconnect the correct target without
//     re-reading the VolumeContext.
//
// Local attach: when the PublishContext carries attach-mode=local (the
// volume was published to its storage node), steps 3, 4 and 6 are replaced
// by claiming the backend device through a device-mapper linear target
// (see DeviceMapper) and then verifying that no nvmet namespace of the
// volume's subsystem (VolumeContext target_id) is enabled — an enabled one
// refuses the stage with FailedPrecondition.  The device-mapper device is
// then mounted or bound, and the state file records the target for
// NodeUnstageVolume/NodeExpandVolume.  Any failure after the claim rolls the
// stage back: the staged surface is unmounted before the claim is removed
// (so no mount outlives its dm device), then the state file is deleted.  A
// failed unmount keeps the claim and the state file.  The local node
// named in the PublishContext must be this node.
//
// Per CSI spec §4.7 the staging_target_path is guaranteed to be a pre-created
// directory (for MOUNT) or a pre-created file (for BLOCK) by the CO.
func (n *NodeServer) NodeStageVolume( //nolint:gocognit,gocyclo,funlen // multi-step attach/mount/persist
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	telemetry.SetVolumeAttributes(ctx, req.GetVolumeId())

	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: staging_target_path is required") //nolint:wrapcheck
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: volume_capability is required") //nolint:wrapcheck
	}
	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	volCtx := req.GetVolumeContext()
	volCap := req.GetVolumeCapability()

	// Serialize with NodeUnstageVolume, NodeExpandVolume and the periodic
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	setSpanAccessType(ctx, volCap)
	var fileCtx *fileStageContext
	var fileErr error
	if n.effectiveDriverName() == v1alpha1.FileCSIDriver ||
		req.GetPublishContext()[PublishContextKeyFilesystemAdoption] != "" ||
		volCtx[VolumeContextKeyFilesystemAdoption] != "" {
		fileCtx, fileErr = n.parseFileStageContext(req)
	}
	if fileErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: filesystem context: %v", fileErr)
	}
	if fileCtx != nil && !fileCtx.local &&
		(req.GetPublishContext()[PublishContextKeyAttachMode] == AttachModeLocal ||
			volCtx[VolumeContextKeyFilesystemLocalAttach] == "true") {
		return nil, status.Errorf(codes.FailedPrecondition,
			"%s", "NodeStageVolume: adopted filesystem local attach is missing its controller-owned proxy path")
	}
	if fileCtx != nil && fileCtx.local {
		return n.stageLocalFilesystem(req, fileCtx)
	}

	// ── Step 1: Resolve attach mode ─────────────────────────────────────────
	// A PublishContext carrying attach-mode=local means ControllerPublishVolume
	// published the volume to the storage node itself: the backend device is
	// attached directly through a device-mapper claim, and the protocol
	// handler is never involved.  Without the key this is a protocol attach.
	local, localDevice, localErr := n.localAttachRequest(req.GetPublishContext())
	if localErr != nil {
		return nil, localErr
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyAttachMode.String(spanAttachMode(local)))

	// ── Step 2: Resolve protocol type ───────────────────────────────────────
	// Derive the protocol type from VolumeContext["pillar-csi.bhyoo.com/protocol-type"]
	// (preferred; set by the controller for all new volumes) or from the
	// volumeID path component.  Falls back to "nvmeof-tcp" for backward
	// compatibility with volumes provisioned before Phase 2.
	protocolType := resolveProtocolType(volumeID, volCtx)

	// Extract common VolumeContext parameters used across protocols.
	targetID := volCtx[VolumeContextKeyTargetID]
	address := volCtx[VolumeContextKeyAddress]
	port := volCtx[VolumeContextKeyPort]
	parsedServerAddr := ""

	if protocolType == ProtocolNFS && local {
		return nil, status.Error(codes.FailedPrecondition, //nolint:wrapcheck
			"NodeStageVolume: localAttach is not supported for NFS; use the NFS client path")
	}
	if protocolType == ProtocolNFS && volCap.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, //nolint:wrapcheck
			"NodeStageVolume: NFS volumes do not support block access")
	}
	attachParams := AttachParams{
		ProtocolType: protocolType,
		ConnectionID: targetID,
		Address:      address,
		Port:         port,
		VolumeRef:    volCtx[vcVolumeRef],
		Extra:        volCtx,
		Secrets:      req.GetSecrets(),
	}

	var handler ProtocolHandler
	var nvmeofMaxTransfer int32
	if !local {
		// ── Step 3: Protocol handler dispatch ───────────────────────────────
		// Look up the handler registered for this protocol type.  A nil handlers
		// map means no handlers were registered (e.g., a state-only test server).
		handler = n.handlers[protocolType]
		if handler == nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"NodeStageVolume: no handler registered for protocol %q%s",
				protocolType, missingHandlerHint(protocolType))
		}

		// NVMe-oF TCP and iSCSI require target_id (NQN / IQN), address, and port.
		if protocolType == ProtocolNVMeoFTCP || protocolType == ProtocolISCSI {
			if targetID == "" {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyTargetID)
			}
			if address == "" {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyAddress)
			}
			if port == "" {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyPort)
			}
		}
		if protocolType == ProtocolNFS {
			if local {
				return nil, status.Error(codes.FailedPrecondition, //nolint:wrapcheck
					"NodeStageVolume: localAttach is not supported for NFS; use the NFS client path")
			}
			if volCap.GetBlock() != nil {
				return nil, status.Error(codes.InvalidArgument, //nolint:wrapcheck
					"NodeStageVolume: NFS volumes do not support block access")
			}
			fsErr := validateNFSStageFilesystem(volCtx, volCap.GetMount())
			if fsErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, fsErr)
			}
			nfsState, stateErr := nfsStateFromParams(attachParams)
			if stateErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, stateErr)
			}
			parsedServerAddr = nfsState.Address
		}
		// The NVMe-oF max data transfer size is validated before any attach
		// side effect, including the page-size floor of max_sectors_kb.
		if protocolType == ProtocolNVMeoFTCP {
			size, sizeErr := ParseNVMeoFMaxDataTransferSize(volCtx)
			if sizeErr == nil {
				sizeErr = CheckNVMeoFTransferSizePageSize(size, n.workerPageSize())
			}
			if sizeErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, sizeErr)
			}
			nvmeofMaxTransfer = size
		}
		// iSCSI port, LUN and session timeouts are validated before any
		// attach side effect so a malformed value is InvalidArgument.
		if protocolType == ProtocolISCSI {
			_, parseErr := parseISCSIAttachParams(attachParams)
			if parseErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: invalid iSCSI volume_context or secrets: %v", volumeID, parseErr)
			}
		}
	} else if targetID == "" {
		// A local attach reads the export state of the volume's network
		// export (NVMe-oF subsystem or iSCSI target, named by target_id) to
		// fence against remote initiators; the controller always sets it.
		return nil, status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: volume_context missing required key %q for local attach", VolumeContextKeyTargetID)
	}

	// ── State machine ordering guard ────────────────────────────────────────
	// When a shared VolumeStateMachine is present, validate that the volume is
	// in a state that permits NodeStageVolume before executing any privileged
	// work.  This prevents out-of-order invocations (e.g. NodeStageVolume
	// before ControllerPublishVolume) from silently proceeding.
	if n.sm != nil {
		smState := n.sm.GetState(volumeID)
		switch smState {
		case StateControllerPublished:
			// Happy path: ControllerPublishVolume was called — proceed.
		case StateNodeStagePartial:
			// Retry after a partial failure (connect succeeded but
			// mount failed on a prior attempt).  Fall through to re-attempt.
		case StateNodeStaged:
			// Already staged: fall through to the file-based idempotency check
			// below, which will detect the existing mount and return success.
		case StateNodePublished:
			// Published but staged filesystem may be dead: allow re-entry so
			// the idempotency/health check can repair it or fail with a
			// retryable error (issue #168).  HasOtherMounts keeps a mount
			// still referenced by live binds from being dropped.
		default:
			// Volume is not in a state that permits NodeStageVolume.
			// ControllerPublishVolume must be called first.
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeStageVolume is not valid in state %s; "+
					"ControllerPublishVolume must be called before NodeStageVolume",
				volumeID, smState)
		}
	}

	// A filesystem staged with the "ro" mount flag is read-only by request:
	// the write-based health probe must not run against it — EROFS is the
	// expected answer, not the ext4 remount-ro shutdown signature (issue
	// #168).  Resolution only matters for filesystem mounts of non-NFS
	// protocols, the configurations that are probed below.
	stagedRO := false
	if volCap.GetMount() != nil && protocolType != ProtocolNFS {
		var roErr error
		stagedRO, roErr = stagedMountReadOnly(volCtx, volCap)
		if roErr != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"NodeStageVolume: volume %q: %v", volumeID, roErr)
		}
	}

	// deadDropped records that this call unmounted a kernel-shutdown staged
	// filesystem: a successful repair then proves no other mount references
	// the old superblock (HasOtherMounts gates the drop), so demoting the SM
	// out of NodePublished cannot strand a live publish bind.
	deadDropped := false

	// ── Idempotency check ───────────────────────────────────────────────────
	// If the volume was already fully staged (state file exists + path mounted),
	// return success per CSI spec §4.7 — but only after re-acknowledging the
	// record's durability.  A previous writeStageState may have completed the
	// rename yet failed the directory sync (e.g. crash or fsync error): the
	// record is on disk but was never acknowledged durable, so success must not
	// be reported without retrying the durable write.
	existingState, stateErr := n.readStageState(volumeID)
	if stateErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: read stage state for %q: %v", volumeID, stateErr)
	}
	if existingState != nil {
		if protocolType == ProtocolNFS {
			expected, expectedErr := nfsStateFromParams(attachParams)
			if expectedErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, expectedErr)
			}
			if existingState.ProtocolType != ProtocolNFS || existingState.NFS == nil ||
				(existingState.StagingPath != "" && existingState.StagingPath != stagingPath) ||
				existingState.NFS.MountSource != expected.MountSource {
				return nil, status.Errorf(codes.FailedPrecondition,
					"NodeStageVolume: existing NFS stage state for %q does not match the requested source or staging path",
					volumeID)
			}
		}
		bindTarget := stageBindTarget(stagingPath, volCap)
		mounted, mountCheckErr := n.mounter.MountEntryExists(bindTarget)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", bindTarget, mountCheckErr)
		}
		writeProbed := volCap.GetMount() != nil && protocolType != ProtocolNFS && !stagedRO
		switch {
		case mounted && writeProbed:
			// A mount table entry says nothing about filesystem health: a
			// kernel-shutdown XFS or an ext4 remount-ro abort keeps its
			// mount entry — only the mountinfo check survives the shutdown,
			// stat itself answers EIO (issues #168, #175).  Reporting success
			// here would publish a dead filesystem forever — pod bind mounts
			// fail on kernels that reject them, or worse, succeed and hand
			// the dead fs to containers on kernels that don't.  Probe the
			// filesystem instead and, when it is dead, drop the staging
			// mount and fall through to re-attach and re-mount from scratch.
			usable, deadErr := n.dropUnusableStagedMount(volumeID, stagingPath)
			if deadErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: volume %q: %v", volumeID, deadErr)
			}
			mounted = usable
			deadDropped = deadDropped || !usable
		case mounted:
			// NFS, raw block, and filesystems staged read-only (stagedRO)
			// skip the write probe — EROFS is a read-only stage's expected
			// answer, not a death signature — but a dead mount must still
			// not be reported staged: the non-writing probe fails it so the
			// CO retries.
			readErr := n.mounter.CheckMountReadable(bindTarget)
			if readErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: volume %q: staged mount %q is not usable: %v",
					volumeID, bindTarget, readErr)
			}
		}
		if mounted {
			if fileCtx != nil {
				err := refreshFileStageMetadata(existingState, fileCtx.state)
				if err != nil {
					return nil, status.Errorf(codes.FailedPrecondition,
						"NodeStageVolume: volume %q: %v", volumeID, err)
				}
			}
			// Records written before the periodic trim existed lack the
			// staging path; backfill it so the trim loop need not derive it.
			if existingState.StagingPath == "" {
				existingState.VolumeID = volumeID
				existingState.StagingPath = stagingPath
			}
			// A volume staged by a release without the transfer limit
			// stays mounted across the upgrade, so a repeated NodeStage
			// is the chance to cap it; the handler's Attach never runs.
			if !local && protocolType == ProtocolNVMeoFTCP {
				limitErr := n.limitStagedNVMeoF(handler, existingState, targetID, nvmeofMaxTransfer)
				if limitErr != nil {
					return nil, status.Errorf(codes.Internal,
						"NodeStageVolume: volume %q: %v", volumeID, limitErr)
				}
			}
			// Re-persist the committed record so this success is acknowledged
			// only after the file and directory syncs complete.
			rewriteErr := n.writeStageState(volumeID, existingState)
			if rewriteErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: re-persist stage state for %q: %v", volumeID, rewriteErr)
			}
			// Already fully staged — idempotent success.
			trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyStageAlreadyStaged.Bool(true))
			return &csi.NodeStageVolumeResponse{}, nil
		}
		// State file exists but the staged surface is not mounted (node
		// reboot), or its filesystem was dead and has just been dropped.
		// Fall through to re-connect and re-mount below.
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyStageAlreadyStaged.Bool(false))

	// ── Filesystem settings ─────────────────────────────────────────────────
	// Resolve the filesystem type and mkfs options of a MOUNT volume before
	// any attach side effect, so a malformed VolumeContext fails fast.
	var fsType string
	var mkfsOpts, mountFlags []string
	var periodicTrim *bool
	if volCap.GetMount() != nil {
		if protocolType == ProtocolNFS {
			fsErr := validateNFSStageFilesystem(volCtx, volCap.GetMount())
			if fsErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, fsErr)
			}
			var flagsErr error
			mountFlags, flagsErr = nfsMountFlags(volCtx, volCap.GetMount(), parsedServerAddr)
			if flagsErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, flagsErr)
			}
			fsType = "nfs"
		} else {
			staged, fsErr := stageFilesystem(volCtx, volCap)
			if fsErr != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"NodeStageVolume: volume %q: %v", volumeID, fsErr)
			}
			fsType, mkfsOpts, mountFlags = staged.fsType, staged.mkfsOptions, staged.mountFlags
			periodicTrim = staged.PeriodicTrim
		}
		trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyFSType.String(fsType))
	}

	// ── Step 5: Attach ──────────────────────────────────────────────────────
	// Local attach claims the backend device through a device-mapper linear
	// target; protocol attach performs transport-level connection setup
	// (RFC §5.4.2 Layer 1).  Both yield the device path for Layer 2
	// presentation.
	var devicePath string
	var attachResult *AttachResult
	if local {
		dmPath, dmErr := n.attachLocal(ctx, volumeID, protocolType, targetID, localDevice, stagingPath, volCap)
		if dmErr != nil {
			return nil, dmErr
		}
		devicePath = dmPath
	} else {
		var attachErr error
		attachResult, attachErr = handler.Attach(ctx, attachParams)
		if attachErr != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, status.Errorf(codes.DeadlineExceeded,
					"NodeStageVolume: timed out waiting for device for volume %q (protocol %q)",
					volumeID, protocolType)
			}
			if isISCSIAuthenticationError(attachErr) {
				return nil, status.Errorf(codes.Unauthenticated,
					"NodeStageVolume: attach volume %q (protocol %q): %v", volumeID, protocolType, attachErr)
			}
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: attach volume %q (protocol %q): %v", volumeID, protocolType, attachErr)
		}
		if attachResult == nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: protocol handler returned no attach result for volume %q", volumeID)
		}
		if protocolType == ProtocolNFS && attachResult.MountSource == "" {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: NFS handler returned an empty mount source for volume %q", volumeID)
		}
		devicePath = attachResult.DevicePath
	}

	// failStaged returns err for a failure after the attach.  A local stage
	// is rolled back (see abortLocal). NFS has no persistent transport session,
	// but its mount must still be removed before the error is returned.
	failStaged := func(err error) error {
		if local {
			return n.abortLocal(ctx, volumeID, stagingPath, volCap, err)
		}
		if protocolType == ProtocolNFS {
			cleanupErr := n.mounter.Unmount(stagingPath)
			if attachResult != nil && attachResult.State != nil {
				cleanupErr = errors.Join(cleanupErr, handler.Detach(ctx, attachResult.State))
			}
			if cleanupErr != nil {
				return errors.Join(err, fmt.Errorf("NFS stage cleanup: %w", cleanupErr))
			}
		}
		return err
	}

	// Record partial state: transport attached; mount not yet started.
	// This drives the volume into NodeStagePartial so that a subsequent mount
	// failure leaves the SM in a recoverable state.  Only transition if the
	// SM is currently at ControllerPublished (not already at NodeStagePartial
	// from a prior retry attempt).
	if n.sm != nil && n.sm.GetState(volumeID) == StateControllerPublished {
		_, _ = n.sm.Transition(volumeID, OpNodeStageConnected) //nolint:errcheck // best-effort; does not affect mount
	}

	// ── Step 6: Mount or bind-mount depending on access type ───────────────
	switch {
	case volCap.GetMount() != nil:
		alreadyMounted, mountCheckErr := n.mounter.MountEntryExists(stagingPath)
		if mountCheckErr != nil {
			return nil, failStaged(status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr))
		}
		if alreadyMounted {
			// A leftover mount whose state file is missing (or whose dead
			// filesystem was just dropped above) is re-adopted only when the
			// filesystem is actually usable; a dead one is dropped and
			// re-mounted below.  See the idempotency check for the kernel
			// shutdown case (issue #168).
			if protocolType != ProtocolNFS && !stagedRO {
				var deadErr error
				alreadyMounted, deadErr = n.dropUnusableStagedMount(volumeID, stagingPath)
				if deadErr != nil {
					// Return deadErr directly, never via failStaged: the
					// helper kept the mount in place on purpose (inconclusive
					// probe, other mounts still pin the dead filesystem, or
					// the unmount itself failed), and abortLocal would tear it
					// down.  The attach/claim from this call stays valid for
					// the next stage retry.
					return nil, status.Errorf(codes.Internal,
						"NodeStageVolume: volume %q: %v", volumeID, deadErr)
				}
				deadDropped = deadDropped || !alreadyMounted
			}
		}
		if !alreadyMounted {
			if protocolType == ProtocolNFS {
				mountErr := n.mounter.Mount(attachResult.MountSource, stagingPath, fsType, mountFlags)
				if mountErr != nil {
					return nil, failStaged(status.Errorf(codes.Internal,
						"NodeStageVolume: mount NFS %q → %q: %v",
						attachResult.MountSource, stagingPath, mountErr))
				}
			} else {
				formatErr := n.formatAndMount(ctx, devicePath, stagingPath, fsType, mountFlags, mkfsOpts)
				if formatErr != nil {
					return nil, failStaged(status.Errorf(codes.Internal,
						"NodeStageVolume: format-and-mount %q → %q (fs=%s): %v",
						devicePath, stagingPath, fsType, formatErr))
				}
			}
		}
		if protocolType != ProtocolNFS && !stagedRO {
			// Verify the staged filesystem after every (re)mount: a mount
			// that re-attached a still-pinned dead superblock reports
			// success at mount(2) yet fails every write (issue #168).
			healthErr := n.mounter.CheckMountHealth(stagingPath)
			if healthErr != nil {
				return nil, failStaged(status.Errorf(codes.Internal,
					"NodeStageVolume: staged filesystem %q for volume %q failed its health probe: %v",
					stagingPath, volumeID, healthErr))
			}
		} else {
			// NFS and filesystems staged with "ro" skip the write probe —
			// EROFS is a read-only stage's expected answer — but an adopted
			// dead mount must still fail the non-writing probe.
			readErr := n.mounter.CheckMountReadable(stagingPath)
			if readErr != nil {
				return nil, failStaged(status.Errorf(codes.Internal,
					"NodeStageVolume: staged mount %q for volume %q is not usable: %v",
					stagingPath, volumeID, readErr))
			}
		}

	case volCap.GetBlock() != nil:
		// BLOCK access: bind-mount the raw device onto a regular file inside
		// the kubelet-created stagingTargetPath directory.  See
		// blockStagingDeviceFile for the kernel rationale.
		bindTarget := blockStagingDevicePath(stagingPath)
		alreadyMounted, mountCheckErr := n.mounter.MountEntryExists(bindTarget)
		if mountCheckErr != nil {
			return nil, failStaged(status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", bindTarget, mountCheckErr))
		}
		if !alreadyMounted {
			bindErr := n.mounter.Mount(devicePath, bindTarget, "", []string{"bind"})
			if bindErr != nil {
				return nil, failStaged(status.Errorf(codes.Internal,
					"NodeStageVolume: bind-mount block device %q → %q: %v",
					devicePath, bindTarget, bindErr))
			}
		}
		readErr := n.mounter.CheckMountReadable(bindTarget)
		if readErr != nil {
			return nil, failStaged(status.Errorf(codes.Internal,
				"NodeStageVolume: block bind %q for volume %q is not usable: %v",
				bindTarget, volumeID, readErr))
		}

	default:
		return nil, failStaged(status.Error(codes.InvalidArgument,
			"NodeStageVolume: volume_capability must specify mount or block access type"))
	}

	// ── Step 7: Persist stage state ─────────────────────────────────────────
	// Write the protocol state to a state file so NodeUnstageVolume can
	// disconnect the correct target even though it does not receive
	// VolumeContext.  AccessType is captured here for the same reason: the
	// unstage RPC has no VolumeCapability, so the access mode it should
	// unmount is read back from this file instead of being guessed.
	accessType := AccessTypeFilesystem
	if volCap.GetBlock() != nil {
		accessType = AccessTypeBlock
	}
	var stageState *nodeStageState
	if local {
		stageState = localStageState(protocolType, accessType, LocalDMName(volumeID), localDevice)
	} else {
		stageState = stageStateFromAttachResult(protocolType, accessType, targetID, address, port, attachResult)
	}
	if fileCtx != nil {
		stageState.File = fileCtx.state
	}
	stageState.FsType = fsType
	stageState.DevicePath = devicePath
	stageState.VolumeID = volumeID
	stageState.StagingPath = stagingPath
	stageState.PeriodicTrim = periodicTrim
	if stageState.NVMeoF != nil && !local {
		stageState.NVMeoF.MaxDataTransferSize = &nvmeofMaxTransfer
	}
	writeErr := n.writeStageState(volumeID, stageState)
	if writeErr != nil {
		return nil, failStaged(status.Errorf(codes.Internal,
			"NodeStageVolume: persist stage state for %q: %v", volumeID, writeErr))
	}

	// ── Step 8: Advance state machine to NodeStaged ──────────────────────────
	// All staging work (connect + mount + state file) completed successfully.
	// Force the SM directly to NodeStaged regardless of whether we entered
	// from ControllerPublished (→ NodeStagePartial via Step 1 above) or from
	// NodeStagePartial (retry).  Entered from NodePublished, the state is left
	// alone unless this call dropped a dead staged filesystem: only the drop
	// (gated by HasOtherMounts) proves no publish bind still references the
	// staged superblock, so a re-adopted healthy mount must not demote a
	// volume whose binds are still live (issue #168).
	if n.sm != nil && (deadDropped || n.sm.GetState(volumeID) != StateNodePublished) {
		n.sm.ForceState(volumeID, StateNodeStaged)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnstageVolume
// ─────────────────────────────────────────────────────────────────────────────.

// NodeUnstageVolume unmounts the staging path and disconnects the storage
// target for the given volume.
//
// Sequence:
//  1. Validate required fields.
//  2. Read the persisted stage state file to recover the access type and
//     protocol-specific teardown state.
//  3. If no state file exists, probe the staging surfaces: a live mount means
//     the state was lost while the volume is still staged, so the call fails
//     instead of reporting success over a leaked mount and transport session.
//     Only when nothing is mounted does the call succeed idempotently, after
//     removing an orphan local-attach device-mapper target of the volume.
//  4. Unmount the staged target via the idempotent Mounter.Unmount, which
//     also tears down corrupted mounts whose probe returns EIO.  A failed
//     unmount aborts here, before transport detach and state cleanup.
//  5. Dispatch to the ProtocolHandler registered for the persisted protocol
//     type and call Detach.  Detach is idempotent: disconnecting an already-
//     disconnected target is a no-op.
//  6. Remove the stage state file to mark the volume as fully unstaged.
//
// The operation is idempotent per CSI spec §4.7.
//
//nolint:gocyclo,gocognit,funlen // SM guard + dual-mode unmount + dispatch + state cleanup
func (n *NodeServer) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	telemetry.SetVolumeAttributes(ctx, req.GetVolumeId())

	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetStagingTargetPath() == "" {
		stagingPathErr := status.Error(codes.InvalidArgument,
			"NodeUnstageVolume: staging_target_path is required")
		return nil, stagingPathErr //nolint:wrapcheck // gRPC status; must not be wrapped
	}

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()

	// Serialize with NodeStageVolume, NodeExpandVolume and the periodic trim
	// of the same volume: trim holds this lock while it trims one chunk.
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	// ── State machine ordering guard ────────────────────────────────────────
	if n.sm != nil {
		smState := n.sm.GetState(volumeID)
		switch smState {
		case StateNodeStaged:
			// Happy path: NodeStageVolume was called — proceed.
		case StateNodeStagePartial:
			// Cleanup of a partially staged volume is permitted.
		case StateControllerPublished, StateNonExistent,
			StateCreated, StateCreatePartial:
			// Volume was never staged (or was already cleanly unstaged).
			// Return success idempotently per CSI spec §4.7.
			return &csi.NodeUnstageVolumeResponse{}, nil
		case StateNodePublished:
			// NodeUnpublishVolume must be called before NodeUnstageVolume.
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeUnstageVolume is not valid in state %s; "+
					"NodeUnpublishVolume must be called before NodeUnstageVolume",
				volumeID, smState)
		default:
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeUnstageVolume is not valid in state %s",
				volumeID, smState)
		}
	}

	// ── Step 1: Read stage state ────────────────────────────────────────────
	// AccessType in the persisted state names the single mount/bind target
	// to unmount; probing both Block and Filesystem paths would touch
	// <stagingPath>/device, which sits INSIDE the Filesystem-mode mount
	// and can return EIO during post-ControllerExpand NVMe namespace
	// re-identify.  Reading state first avoids that whole class of
	// false-positive errors.
	state, readErr := n.readStageState(volumeID)
	if readErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: read stage state for %q: %v", volumeID, readErr)
	}
	setSpanAttachMode(ctx, state)
	if state == nil {
		// CSI spec §4.7: NodeUnstageVolume must succeed if the volume was
		// never staged (or was already cleanly unstaged on a prior call).
		// A missing state file alone does not prove that: if the state dir
		// was lost while the volume stayed staged, returning OK would leave
		// the mount and the transport session behind with nothing left to
		// tear them down.  Fail unless no staged surface remains mounted.
		guardErr := n.checkUnstagedWithoutState(volumeID, stagingPath)
		if guardErr != nil {
			return nil, guardErr
		}
		// Nothing is mounted, but a local stage that failed without its
		// state file (or whose state directory was lost) may still hold
		// the backend device through its deterministically named
		// device-mapper target, which keeps the network export fenced.
		// Release it before reporting success.
		orphanErr := n.removeOrphanLocal(ctx, volumeID)
		if orphanErr != nil {
			return nil, orphanErr
		}
		return &csi.NodeUnstageVolumeResponse{}, nil
	}
	if state.ProtocolType == ProtocolNFS && state.StagingPath != "" && state.StagingPath != stagingPath {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodeUnstageVolume: NFS stage state for %q records staging path %q, got %q",
			volumeID, state.StagingPath, stagingPath)
	}

	// ── Step 2: Unmount the staged target ───────────────────────────────────
	// Filesystem-mode mounts the formatted device at stagingPath itself;
	// Block-mode binds /dev/nvmeXnY onto blockStagingDevicePath(stagingPath)
	// (a regular file inside the kubelet-created staging directory).  The
	// AccessType discriminator selects which surface to unmount.
	//
	// Mounter.Unmount is contractually idempotent and handles corrupted
	// mounts (e.g. stat EIO on an aborted XFS after NVMe device loss):
	// no mount → no-op; corrupted mount → unmount attempted anyway;
	// any other failure → error.  A separate mount-check gate cannot express
	// that third case, so unmount unconditionally and let the mounter
	// discriminate.  Any error aborts before Detach and before the stage
	// state file is removed, keeping the transport session and its
	// teardown parameters intact for the retry.
	unmountTarget := stagingPath
	isBlock := state.AccessType == AccessTypeBlock
	if isBlock {
		unmountTarget = blockStagingDevicePath(stagingPath)
	}
	unmountErr := n.mounter.Unmount(unmountTarget)
	if unmountErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: unmount %q: %v", unmountTarget, unmountErr)
	}
	if isBlock {
		// Block-mode leaves a regular-file sentinel that kubelet's
		// subsequent rmdir on stagingPath would refuse; remove it
		// best-effort so the directory can be reaped.
		_ = os.Remove(unmountTarget) //nolint:errcheck // best-effort cleanup
	}

	// ── Step 3: Release the local claim or disconnect the storage target ───
	// A local stage holds the backend device through a device-mapper target
	// and never touched the protocol handler; removing the target releases
	// the exclusive claim the agent checks before re-enabling the export.
	// Otherwise dispatch to the ProtocolHandler registered for the persisted
	// protocol type.  Detach is idempotent: disconnecting an
	// already-disconnected target is a no-op.
	if state.File == nil {
		if state.isLocalAttach() {
			releaseErr := n.releaseLocal(ctx, volumeID, state)
			if releaseErr != nil {
				return nil, releaseErr
			}
		} else if n.handlers != nil {
			handler, ok := n.handlers[state.ProtocolType]
			if !ok {
				if state.ProtocolType != ProtocolNFS {
					return nil, status.Errorf(codes.Internal,
						"NodeUnstageVolume: no handler registered for protocol %q%s",
						state.ProtocolType, missingHandlerHint(state.ProtocolType))
				}
			} else {
				protoState, protoErr := state.ToProtocolState()
				if protoErr != nil {
					return nil, status.Errorf(codes.Internal,
						"NodeUnstageVolume: convert stage state for %q: %v", volumeID, protoErr)
				}
				detachErr := handler.Detach(ctx, protoState)
				if detachErr != nil {
					return nil, status.Errorf(codes.Internal,
						"NodeUnstageVolume: detach (protocol %q): %v",
						state.ProtocolType, detachErr)
				}
			}
		}
	}

	// ── Step 4: Remove stage state file ─────────────────────────────────────
	deleteErr := n.deleteStageState(volumeID)
	if deleteErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: delete stage state for %q: %v", volumeID, deleteErr)
	}

	// ── Step 5: Revert state machine to ControllerPublished ─────────────────
	// The node is no longer connected to the volume.  The initiator ACL grant
	// from ControllerPublishVolume is still in effect; the volume can be
	// re-staged without re-publishing at the controller level.
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateControllerPublished)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// dropUnusableStagedMount health-checks the filesystem mounted at target
// and, when the kernel has shut it down (ErrMountUnhealthy), unmounts it so
// the caller can re-stage from scratch.  It must not run against a mount
// that is read-only by request — the probe write answers EROFS, which is
// the expected outcome, not a death signature; callers gate it off via
// stagedMountReadOnly.
//
// Return value: usable is true when the filesystem passed the probe — the
// mount is kept and the caller should take its "already mounted" path.  A
// nil error with usable=false means the dead mount was removed and nothing
// is mounted at target anymore.
//
// A dead filesystem is only dropped when no other mount references it.  Pod
// bind mounts share the staged superblock: while one survives, unmounting
// the staging path does not detach the filesystem and a fresh mount(2) just
// re-attaches the same dead superblock (verified with xfs_io shutdown on
// kernel 7.0).  In that case the mount is left in place and an error is
// returned — kubelet retries, and recovery proceeds once teardown removes
// the binds (pod deletion drives NodeUnpublishVolume for each target).
//
// Probe failures that are not ErrMountUnhealthy are inconclusive: the mount
// is never dropped on an inconclusive probe.
func (n *NodeServer) dropUnusableStagedMount(volumeID, target string) (usable bool, err error) {
	healthErr := n.mounter.CheckMountHealth(target)
	if healthErr == nil {
		return true, nil
	}
	if !errors.Is(healthErr, ErrMountUnhealthy) {
		return false, fmt.Errorf("health-check staged filesystem %q: %w", target, healthErr)
	}
	others, mountsErr := n.mounter.HasOtherMounts(target)
	if mountsErr != nil {
		return false, fmt.Errorf("volume %q: check mounts sharing the dead filesystem at %q: %w",
			volumeID, target, mountsErr)
	}
	if others {
		return false, fmt.Errorf("volume %q: staged filesystem at %q is dead but still referenced "+
			"by other mounts; it cannot be re-staged until pod teardown removes them",
			volumeID, target)
	}
	unmountErr := n.mounter.Unmount(target)
	if unmountErr != nil {
		return false, fmt.Errorf("volume %q: unmount dead staged filesystem %q: %w",
			volumeID, target, unmountErr)
	}
	return false, nil
}

// ensureHealthyStagedMount guarantees the staged filesystem at stagingPath
// is mounted and writable before NodePublishVolume bind-mounts it (issue
// #172).  Kubelet retries NodePublishVolume — never NodeStageVolume — while
// the VolumeAttachment persists, so the repair must live here or a pod
// whose filesystem entered kernel shutdown stays ContainerCreating forever.
// Called only for configurations nodePublishProbePlan marks probeable
// (filesystem access, non-NFS protocol, not staged read-only), under the
// per-volume lock, so it cannot race a stage repair or another publish.
//
// A nil return means the staged filesystem answers writes.  Whether the
// staged mount exists is decided by MountEntryExists — mountinfo, never
// stat: a dead staged filesystem answers stat with EIO, which is the exact
// symptom being repaired, so a stat probe would wedge repair forever
// (issue #175).  A staged mount that is
// dead (ErrMountUnhealthy) and unpinned is dropped and re-mounted from the
// device the stage state file recorded — mount re-attaching a still-pinned
// dead superblock would report success yet stay dead, so a pinned mount is
// never touched and an Internal error asks the CO to retry until teardown
// removes the binds.  Any probe, read, or mount failure is an Internal
// error; nothing is unmounted on an inconclusive result.
func (n *NodeServer) ensureHealthyStagedMount(
	ctx context.Context, volumeID, stagingPath string,
	volCtx map[string]string, volCap *csi.VolumeCapability,
) error {
	staged, fsErr := stageFilesystem(volCtx, volCap)
	if fsErr != nil {
		return status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: volume %q: %v", volumeID, fsErr)
	}
	mounted, mountCheckErr := n.mounter.MountEntryExists(stagingPath)
	if mountCheckErr != nil {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: volume %q: check if staged path %q is mounted: %v",
			volumeID, stagingPath, mountCheckErr)
	}
	if mounted {
		healthErr := n.mounter.CheckMountHealth(stagingPath)
		if healthErr == nil {
			return nil
		}
		if !errors.Is(healthErr, ErrMountUnhealthy) {
			return status.Errorf(codes.Internal,
				"NodePublishVolume: volume %q: health-check staged filesystem %q: %v",
				volumeID, stagingPath, healthErr)
		}
	}
	state, stateErr := n.readStageState(volumeID)
	if stateErr != nil {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: read stage state for %q: %v", volumeID, stateErr)
	}
	if !mounted && state == nil {
		// No stage record: the volume was never staged on this node (unit
		// tests and out-of-band flows bind the path directly).  Preserve the
		// historical behavior — bind whatever is there.
		return nil
	}
	return n.repairDeadStagedMount(ctx, volumeID, stagingPath, mounted, state, staged)
}

// repairDeadStagedMount re-mounts the staged filesystem at stagingPath:
// when mounted is true the staged mount is known dead (ErrMountUnhealthy)
// and is dropped first; a missing mount goes straight to the re-mount so a
// previous repair attempt that already unmounted still completes.  The
// device comes from the stage record's DevicePath, or — for state files
// written before the field existed — the staged mount's mountinfo source.
//
// A dead mount still referenced by other mounts (pod binds that share the
// superblock) is never unmounted: the re-mount would re-attach the same
// dead filesystem, so an Internal error asks the CO to retry after
// teardown.  The re-mount runs formatAndMount, which detects the existing
// filesystem signature and mounts without mkfs, letting the journal replay
// — the same repair NodeStageVolume performs.  A filesystem that still
// fails its health probe after the re-mount is reported rather than
// silently bound.
func (n *NodeServer) repairDeadStagedMount(
	ctx context.Context, volumeID, stagingPath string, mounted bool,
	state *nodeStageState, staged stagedFilesystem,
) error {
	device := ""
	if state != nil {
		device = state.DevicePath
	}
	if mounted {
		others, mountsErr := n.mounter.HasOtherMounts(stagingPath)
		if mountsErr != nil {
			return status.Errorf(codes.Internal,
				"NodePublishVolume: volume %q: check mounts sharing the dead filesystem at %q: %v",
				volumeID, stagingPath, mountsErr)
		}
		if others {
			return status.Errorf(codes.Internal,
				"NodePublishVolume: volume %q: staged filesystem at %q is dead but still "+
					"referenced by other mounts; it cannot be repaired until pod teardown removes them",
				volumeID, stagingPath)
		}
		if device == "" {
			source, sourceErr := n.mounter.MountSource(stagingPath)
			if sourceErr != nil {
				return status.Errorf(codes.Internal,
					"NodePublishVolume: volume %q: no device path in the stage record and the "+
						"mountinfo source of %q is unreadable: %v",
					volumeID, stagingPath, sourceErr)
			}
			device = source
		}
		unmountErr := n.mounter.Unmount(stagingPath)
		if unmountErr != nil {
			return status.Errorf(codes.Internal,
				"NodePublishVolume: unmount dead staged filesystem %q for volume %q: %v",
				stagingPath, volumeID, unmountErr)
		}
	}
	if device == "" {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: volume %q: staged filesystem at %q is %s but the stage "+
				"record has no device path to re-mount",
			volumeID, stagingPath, map[bool]string{true: "dead", false: "not mounted"}[mounted])
	}
	formatErr := n.formatAndMount(ctx, device, stagingPath, staged.fsType, staged.mountFlags, staged.mkfsOptions)
	if formatErr != nil {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: re-mount staged filesystem %q -> %q for volume %q: %v",
			device, stagingPath, volumeID, formatErr)
	}
	healthErr := n.mounter.CheckMountHealth(stagingPath)
	if healthErr != nil {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: re-mounted staged filesystem %q for volume %q failed its "+
				"health probe: %v", stagingPath, volumeID, healthErr)
	}
	return nil
}

// checkUnstagedWithoutState verifies that nothing is still mounted for a
// volume whose stage state file is missing.  It returns nil only when neither
// staged surface is mounted; a live mount or a failed probe yields an error.
//
// Both surfaces are checked through the mount table (MountEntryExists), not
// stat: a kernel-shutdown staged filesystem answers stat with EIO, which
// would wedge unstage retries forever without distinguishing "dead but
// mounted" from a probe failure (issue #175).
//
// Without the state file the transport parameters needed for Detach are
// unknown, so the mount is not torn down here; the volume must be re-staged
// (which rewrites the state file) before NodeUnstageVolume can complete.
func (n *NodeServer) checkUnstagedWithoutState(volumeID, stagingPath string) error {
	for _, target := range []string{stagingPath, blockStagingDevicePath(stagingPath)} {
		mounted, err := n.mounter.MountEntryExists(target)
		if err != nil {
			return status.Errorf(codes.Internal,
				"NodeUnstageVolume: stage state for %q is missing; check if %q is mounted: %v",
				volumeID, target, err)
		}
		if mounted {
			return status.Errorf(codes.Internal,
				"NodeUnstageVolume: stage state for %q is missing but %q is still mounted; "+
					"refusing to report the volume unstaged (re-stage the volume to restore its state)",
				volumeID, target)
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

func validateNodePublishRequest(req *csi.NodePublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetStagingTargetPath() == "" {
		err := status.Error(codes.InvalidArgument, "NodePublishVolume: staging_target_path is required")
		return err //nolint:wrapcheck // gRPC status; must not be wrapped
	}
	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: target_path is required") //nolint:wrapcheck
	}
	if req.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: volume_capability is required") //nolint:wrapcheck
	}
	return nil
}

func validateNodePublishNFS(req *csi.NodePublishVolumeRequest) error {
	if resolveProtocolType(req.GetVolumeId(), req.GetVolumeContext()) != ProtocolNFS {
		return nil
	}
	if req.GetVolumeCapability().GetBlock() != nil {
		return status.Error(codes.InvalidArgument, //nolint:wrapcheck // gRPC status must not be wrapped
			"NodePublishVolume: NFS volumes do not support block access")
	}
	err := validateNFSStageFilesystem(req.GetVolumeContext(), req.GetVolumeCapability().GetMount())
	if err != nil {
		return status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: volume %q: %v", req.GetVolumeId(), err)
	}
	return nil
}

func (n *NodeServer) validateNodePublishState(volumeID string) error {
	if n.sm == nil {
		return nil
	}
	switch smState := n.sm.GetState(volumeID); smState {
	case StateNodeStaged, StateNodePublished:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition,
			"volume %q: NodePublishVolume is not valid in state %s; "+
				"NodeStageVolume must be called before NodePublishVolume",
			volumeID, smState)
	}
}

func resolveNodePublishMount(req *csi.NodePublishVolumeRequest) (fsType string, mountOptions []string, err error) {
	mount := req.GetVolumeCapability().GetMount()
	if mount == nil {
		return "", []string{"bind"}, nil
	}
	flags, err := resolveMountFlags(req.GetVolumeContext(), req.GetVolumeCapability())
	if err != nil {
		return "", nil, err
	}
	mountOptions = make([]string, 1, 1+len(flags))
	mountOptions[0] = "bind"
	mountOptions = append(mountOptions, flags...)
	return mount.GetFsType(), mountOptions, nil
}

func (n *NodeServer) mountNodePublish(
	stagingPath, targetPath, fsType string, volCap *csi.VolumeCapability, mountOptions []string,
) error {
	switch {
	case volCap.GetMount() != nil, volCap.GetBlock() != nil:
		bindSource := stageBindTarget(stagingPath, volCap)
		err := n.mounter.Mount(bindSource, targetPath, fsType, mountOptions)
		if err != nil {
			return status.Errorf(codes.Internal,
				"NodePublishVolume: bind-mount %q → %q: %v",
				bindSource, targetPath, err)
		}
		return nil
	default:
		return status.Error(codes.InvalidArgument, //nolint:wrapcheck
			"NodePublishVolume: volume_capability must specify mount or block access type")
	}
}

// bindWithStagedRepair probes the staged filesystem before bind-mounting it
// and, when the staged filesystem entered kernel shutdown, repairs it in
// place (unmount + re-mount so the journal replays) before the bind
// proceeds (issue #172).
//
// The probe belongs here, not only in NodeStageVolume: while a
// VolumeAttachment persists kubelet retries NodePublishVolume without ever
// re-issuing NodeStageVolume, so a repair gated on the stage RPC is
// unreachable during the pod-delete flow.  On kernels that reject
// bind-mounting a shut-down superblock the bind fails before any post-bind
// health check could run — a bind error also triggers one repair-and-retry
// before the error is reported.
func (n *NodeServer) bindWithStagedRepair(
	ctx context.Context, volumeID, stagingPath, targetPath, fsType string,
	volCap *csi.VolumeCapability, mountOptions []string, probeHealth bool,
	volCtx map[string]string,
) error {
	if probeHealth {
		healthErr := n.ensureHealthyStagedMount(ctx, volumeID, stagingPath, volCtx, volCap)
		if healthErr != nil {
			return healthErr
		}
	}
	err := n.mountNodePublish(stagingPath, targetPath, fsType, volCap, mountOptions)
	if err == nil || !probeHealth {
		return err
	}
	// The staged filesystem may have died between the probe and the bind.
	healthErr := n.ensureHealthyStagedMount(ctx, volumeID, stagingPath, volCtx, volCap)
	if healthErr != nil {
		//nolint:wrapcheck // both operands are annotated; Join preserves the gRPC code
		return errors.Join(err, healthErr)
	}
	return n.mountNodePublish(stagingPath, targetPath, fsType, volCap, mountOptions)
}

// NodePublishVolume bind-mounts the staged volume from the staging path to the
// pod-specific target path.
//
// This is called by the CO (kubelet) once per pod that uses the volume.  The
// node plugin must have already completed NodeStageVolume for this volume before
// NodePublishVolume is called.
//
// Sequence:
//  1. Validate required fields (volume_id, staging_target_path, target_path,
//     volume_capability).
//  2. Check idempotency: if target_path is already mounted, return success.
//  3. For MOUNT access type: bind-mount from staging_target_path to target_path,
//     adding any mount flags from the VolumeCapability plus "bind".
//  4. For BLOCK access type: bind-mount the staging_target_path (which holds
//     the raw block device bind) to target_path.
//
// Per CSI spec §4.7 the target_path is pre-created by the CO before this call.
func (n *NodeServer) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	setPublishSpanAttributes(ctx, req)

	err := validateNodePublishRequest(req)
	if err != nil {
		return nil, err
	}
	err = validateNodePublishNFS(req)
	if err != nil {
		return nil, err
	}

	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()
	volCap := req.GetVolumeCapability()
	volCtx := req.GetVolumeContext()

	// Serialize with NodeStageVolume / NodeUnstageVolume / NodeExpandVolume
	// of the same volume: a stage that drops a dead staged filesystem and
	// re-mounts it must not race a publish bind-mount (issue #168).  The
	// ordering guard runs under the lock so an unstage completing between
	// the check and the bind cannot leave a bind of the reaped directory.
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	err = n.validateNodePublishState(volumeID)
	if err != nil {
		return nil, err
	}

	probeHealth, probePath, probeErr := nodePublishProbePlan(volumeID, stagingPath, targetPath, req)
	if probeErr != nil {
		return nil, probeErr
	}

	alreadyMounted, mountCheckErr := n.mounter.MountEntryExists(targetPath)
	if mountCheckErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodePublishVolume: check if %q is mounted: %v", targetPath, mountCheckErr)
	}
	if alreadyMounted {
		usable, usableErr := n.ensureUsablePublishBind(volumeID, targetPath, probePath, probeHealth)
		if usableErr != nil {
			return nil, usableErr
		}
		if usable {
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// The dead bind was dropped; re-bind below so a staged filesystem
		// that was repaired since is re-published.  A still-dead staged
		// filesystem fails the post-bind probe.  The SM stays NodePublished
		// (it is a per-volume aggregate and other targets may still be
		// bound); NodeStageVolume accepts that state for repair (issue #168).
	}

	fsType, mountOptions, mountErr := resolveNodePublishMount(req)
	if mountErr != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: volume %q: %v", volumeID, mountErr)
	}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	bindErr := n.bindWithStagedRepair(ctx, volumeID, stagingPath, targetPath,
		fsType, volCap, mountOptions, probeHealth, volCtx)
	if bindErr != nil {
		return nil, bindErr
	}

	if probeHealth {
		// Verify the fresh bind: bind-mounting a dead staged filesystem
		// succeeds on kernels that accept it, so mounting is not proof of
		// health (issue #168).  Report the dead filesystem instead of
		// publishing it; kubelet retries while NodeStageVolume re-stages.
		// Read-only binds are probed through the staged mount (probePath).
		healthErr := n.mounter.CheckMountHealth(probePath)
		if healthErr != nil {
			cleanupErr := n.mounter.Unmount(targetPath)
			//nolint:wrapcheck // both operands are annotated; Join preserves the gRPC code
			return nil, errors.Join(status.Errorf(codes.Internal,
				"NodePublishVolume: bind-mounted filesystem at %q for volume %q failed its health probe: %v",
				targetPath, volumeID, healthErr), cleanupErr)
		}
	}

	if n.sm != nil {
		n.sm.ForceState(volumeID, StateNodePublished)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// nodePublishProbePlan decides whether NodePublishVolume verifies mount
// health and which path the probe runs against (issue #168).
//
// Two configurations must not be write-probed: filesystem protocols without
// a staged filesystem (NFS, block access) have none to probe, and a
// filesystem staged with the "ro" mount flag answers the write probe with
// EROFS — the expected outcome there, not the remount-ro shutdown
// signature.  Every other mount publish probes the filesystem serving the
// bind: the bind itself for a writable publish, the read-write staged
// mount for a read-only one — a read-only bind would answer every write
// probe with EROFS while sharing the probed superblock.
func nodePublishProbePlan(
	volumeID, stagingPath, targetPath string, req *csi.NodePublishVolumeRequest,
) (probeHealth bool, probePath string, err error) {
	volCap := req.GetVolumeCapability()
	if volCap.GetMount() == nil ||
		resolveProtocolType(volumeID, req.GetVolumeContext()) == ProtocolNFS {
		return false, "", nil
	}
	stagedRO, roErr := stagedMountReadOnly(req.GetVolumeContext(), volCap)
	if roErr != nil {
		return false, "", status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: volume %q: %v", volumeID, roErr)
	}
	if stagedRO {
		return false, "", nil
	}
	if req.GetReadonly() {
		return true, stagingPath, nil
	}
	return true, targetPath, nil
}

// ensureUsablePublishBind health-checks an existing publish bind mount and
// drops it when it references a kernel-shutdown filesystem (issue #168).
//
// A bind mount of a dead filesystem is itself dead, yet keeps its mount
// table entry.  Usable is true only when the mount passed the probe; a nil
// error with usable=false means the dead bind was unmounted and the caller
// may bind afresh.  Configurations that skip the write probe (NFS, raw
// block, a read-only stage) get the non-writing CheckMountReadable: a dead
// bind there is an Internal error, as nothing in those paths repairs the
// staged mount.  Unlike
// dropUnusableStagedMount there is no HasOtherMounts gate: the bind only
// references the dead superblock — the pod's own mount-namespace copy is
// unaffected by removing it here — and dropping it never makes repair
// harder.  Probe failures that are not ErrMountUnhealthy are inconclusive
// and returned without touching the mount.
//
// ProbePath is the mount the health probe runs against: the bind itself
// for a writable publish, the staged mount for a read-only one — a
// read-only bind answers every write probe with EROFS, so its verdict is
// taken from the read-write mount that shares the superblock.
func (n *NodeServer) ensureUsablePublishBind(
	volumeID, targetPath, probePath string, probeHealth bool,
) (usable bool, err error) {
	if !probeHealth {
		readErr := n.mounter.CheckMountReadable(targetPath)
		if readErr != nil {
			return false, status.Errorf(codes.Internal,
				"NodePublishVolume: bind %q for volume %q is not usable: %v",
				targetPath, volumeID, readErr)
		}
		return true, nil
	}
	healthErr := n.mounter.CheckMountHealth(probePath)
	switch {
	case healthErr == nil:
		return true, nil
	case errors.Is(healthErr, ErrMountUnhealthy):
		unmountErr := n.mounter.Unmount(targetPath)
		if unmountErr != nil {
			return false, status.Errorf(codes.Internal,
				"NodePublishVolume: unmount dead bind %q for volume %q: %v",
				targetPath, volumeID, unmountErr)
		}
		return false, nil
	default:
		return false, status.Errorf(codes.Internal,
			"NodePublishVolume: health-check bind %q for volume %q: %v",
			targetPath, volumeID, healthErr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeUnpublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// NodeUnpublishVolume unmounts the bind mount at the pod-specific target path.
//
// This is the inverse of NodePublishVolume and is called by the CO once per pod
// that has finished using the volume.  After this call the target_path should no
// longer be mounted.
//
// The operation is idempotent per CSI spec §4.7: if the target_path is not
// currently mounted (or does not exist) the call succeeds silently.
//
// Sequence:
//  1. Validate required fields (volume_id, target_path).
//  2. Call the idempotent Mounter.Unmount on target_path.  It no-ops on an
//     unmounted or missing path and still removes a corrupted mount whose
//     stat fails (e.g. EIO on an aborted filesystem); a real unmount
//     failure is returned as Internal so the CO retries.
func (n *NodeServer) NodeUnpublishVolume(
	ctx context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	telemetry.SetVolumeAttributes(ctx, req.GetVolumeId())

	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: target_path is required") //nolint:wrapcheck
	}

	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()

	// Serialize with NodeStageVolume, which may drop and re-mount the staged
	// filesystem while deciding whether pod binds still pin it (issue #168).
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	// ── State machine ordering guard ────────────────────────────────────────
	if n.sm != nil {
		smState := n.sm.GetState(volumeID)
		switch smState {
		case StateNodePublished:
			// Happy path: NodePublishVolume was called — proceed to unmount.
		case StateNodeStaged, StateControllerPublished,
			StateNonExistent, StateCreated, StateCreatePartial, StateNodeStagePartial:
			// Volume is not currently published.  Return success idempotently
			// per CSI spec §5.4.2: "NodeUnpublishVolume MUST succeed if the
			// volume is not currently NodePublished".
			return &csi.NodeUnpublishVolumeResponse{}, nil
		default:
			// Unexpected state — fail safe.
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeUnpublishVolume is not valid in state %s",
				volumeID, smState)
		}
	}

	// ── Unmount the bind mount ──────────────────────────────────────────────
	// Mounter.Unmount is contractually idempotent: an unmounted or missing
	// target is a no-op success, so repeat NodeUnpublishVolume calls converge
	// without a separate probe.  Corrupted mounts (e.g. stat EIO on an
	// aborted filesystem after NVMe device loss) are unmounted rather than
	// reported as an unrecoverable probe error — gating on a stat-based
	// mount check here
	// would trap kubelet in a teardown loop.  An actual unmount failure
	// propagates as Internal so kubelet retries, and the state machine is
	// not reverted until the unmount has genuinely succeeded.
	unmountErr := n.mounter.Unmount(targetPath)
	if unmountErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnpublishVolume: unmount %q: %v", targetPath, unmountErr)
	}

	// ── Revert state machine to NodeStaged ──────────────────────────────────
	// The state machine aggregates every publish target of the volume: it
	// may demote to NodeStaged only when this unpublish removed the last
	// mount sharing the staged filesystem.  While another target's bind
	// still references it the volume stays NodePublished — demoting anyway
	// would make the surviving bind's own NodeUnpublishVolume return early
	// without unmounting it, pinning a dead staged superblock forever so
	// the stage repair could never proceed (issue #168).
	if n.sm != nil && n.lastPublishRemoved(volumeID) {
		n.sm.ForceState(volumeID, StateNodeStaged)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// lastPublishRemoved reports whether the unpublish that just completed
// removed the last mount sharing the volume's staged filesystem: with no
// mounts left, the volume's per-volume aggregate state may revert from
// NodePublished to NodeStaged.  A surviving bind of another publish target
// keeps the answer false.
//
// The check mirrors dropUnusableStagedMount's pinning test against the
// staged filesystem recorded in the stage state.  Whenever sharing cannot
// be proven the answer is true — the pre-existing unconditional demotion:
//   - no stage record (or one without a staging path) leaves no staged
//     filesystem to compare against;
//   - a Block-mode volume has no staged filesystem at all: its staging
//     surface is a bind of a device node, whose mountinfo device number is
//     that of the filesystem holding the node (devtmpfs), shared by /dev
//     itself and every other device-node bind;
//   - HasOtherMounts errs only when the staged mount is gone or the mount
//     table is unreadable.
//
// Holding NodePublished on an unprovable bind would block
// NodeUnstageVolume instead.
func (n *NodeServer) lastPublishRemoved(volumeID string) bool {
	state, stateErr := n.readStageState(volumeID)
	if stateErr != nil || state == nil || state.StagingPath == "" ||
		state.AccessType == AccessTypeBlock {
		return true
	}
	others, mountsErr := n.mounter.HasOtherMounts(state.StagingPath)
	if mountsErr != nil {
		return true
	}
	return !others
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage state helpers
// ─────────────────────────────────────────────────────────────────────────────.

// stateFilePath returns the filesystem path for the JSON state file of the
// given volumeID.  Path separators in the volumeID are replaced with
// underscores to produce a valid single-file filename.
func (n *NodeServer) stateFilePath(volumeID string) string {
	return filepath.Join(n.stateDir, stateFileKey(volumeID)+stateFileExt)
}

// stateFileExt is the extension of every stage state file.
const stateFileExt = ".json"

// stateFileKey is the stage state file name of volumeID without extension:
// path separators become underscores.
func stateFileKey(volumeID string) string {
	return strings.ReplaceAll(volumeID, "/", "_")
}

// writeStageState serializes state to the JSON file for volumeID under
// stateDir.  The directory is created if it does not yet exist.
//
// The write is atomic and durable: the payload is written to a sibling
// temporary file, fsynced, then renamed over the target, and every directory
// on the stateDir path is fsynced so the rename and any newly created
// directories survive a host crash.  Success is acknowledged only after the
// file contents and the directory entries have been synced; a failed
// replacement before the rename preserves the previous record.  This mirrors
// the fencing-mark write in internal/agent/fencing.go.
func (n *NodeServer) writeStageState(volumeID string, state *nodeStageState) error {
	stateFile := n.stateFilePath(volumeID)

	// StagedAt anchors the first periodic trim of the record, so it is
	// written once — at the NodeStageVolume that creates the record — and
	// preserved by every rewrite.  For a record written before the field
	// existed, the existing file's modification time is the closest durable
	// approximation of the stage time and does not restart the initial
	// delay on every rewrite.
	if state.StagedAt == nil {
		stagedAt := time.Now()
		info, statErr := os.Stat(stateFile)
		switch {
		case statErr == nil:
			stagedAt = info.ModTime()
		case !errors.Is(statErr, os.ErrNotExist):
			return fmt.Errorf("stat stage state file %q: %w", stateFile, statErr)
		}
		state.StagedAt = &stagedAt
	}

	data, marshalErr := json.Marshal(state)
	if marshalErr != nil {
		return fmt.Errorf("marshal stage state: %w", marshalErr)
	}

	mkdirErr := os.MkdirAll(n.stateDir, 0o700) // #nosec G703 -- driver-configured stateDir, not per-request input
	if mkdirErr != nil {
		return fmt.Errorf("create state directory %q: %w", n.stateDir, mkdirErr)
	}

	// A unique temp file per attempt: concurrent NodeStageVolume calls for the
	// same volumeID must not share a temp path, and a stale temp file left by
	// a crash must never be reused.  CreateTemp applies mode 0600, which the
	// rename keeps: iSCSI records hold CHAP credentials (ISCSIStageCHAP).
	f, openErr := os.CreateTemp(n.stateDir, filepath.Base(stateFile)+".*.tmp")
	if openErr != nil {
		return fmt.Errorf("create temp state file in %q: %w", n.stateDir, openErr)
	}
	tmpFile := f.Name()
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	joined := errors.Join(writeErr, syncErr, closeErr)
	if joined != nil {
		removeErr := os.Remove(tmpFile) // #nosec G703 -- CreateTemp path under controlled stateDir
		if removeErr != nil {
			joined = errors.Join(joined, fmt.Errorf("remove temp state file %q: %w", tmpFile, removeErr))
		}
		return fmt.Errorf("write/sync/close temp state file %q: %w", tmpFile, joined)
	}

	renameErr := os.Rename(tmpFile, stateFile) // #nosec G703 -- paths derived from controlled stateDir
	if renameErr != nil {
		removeErr := os.Remove(tmpFile) // #nosec G703 -- CreateTemp path under controlled stateDir
		if removeErr != nil {
			renameErr = errors.Join(renameErr, fmt.Errorf("remove temp state file %q: %w", tmpFile, removeErr))
		}
		return fmt.Errorf("rename temp state file %q to %q: %w", tmpFile, stateFile, renameErr)
	}

	// fsync every directory on the stateDir path, from stateDir up to the
	// filesystem root: stateDir holds the renamed state file, and each parent
	// holds the entry of the directory below it.  Directory existence does not
	// prove durability — a directory created by MkdirAll above, by the
	// readiness probe, or by a crashed earlier attempt may not yet be synced —
	// so the chain is synced unconditionally.  Staging is infrequent and the
	// chain is short, so the extra fsyncs are cheap.
	for dir := n.stateDir; ; dir = filepath.Dir(dir) {
		dirErr := syncDir(dir)
		if dirErr != nil {
			return fmt.Errorf("sync state directory %q: %w", dir, dirErr)
		}
		if filepath.Dir(dir) == dir {
			break // filesystem root reached
		}
	}
	return nil
}

// syncDir fsyncs a directory so that file and subdirectory entries created or
// renamed inside it are committed to durable storage.
func syncDir(dir string) error {
	d, openErr := os.Open(dir) //nolint:gosec // G304: directory path derived from controlled stateDir
	if openErr != nil {
		return fmt.Errorf("open: %w", openErr)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	joined := errors.Join(syncErr, closeErr)
	if joined != nil {
		return fmt.Errorf("sync/close: %w", joined)
	}
	return nil
}

// readStageState reads and deserialises the stage state for volumeID.
// Returns (nil, nil) when no state file exists (volume not yet staged or
// already cleanly unstaged).
//
// Legacy migration (RFC §5.5.2): state files written by the pre-discriminated-
// union code have the format {"subsys_nqn": "nqn.…"} with no protocol_type
// field.  When such a file is detected, readStageState converts it in-place to
// the new discriminated union format so that NodeUnstageVolume can use the
// typed ProtocolHandler path on the very next call.
func (n *NodeServer) readStageState(volumeID string) (*nodeStageState, error) {
	stateFile := n.stateFilePath(volumeID)
	data, readErr := os.ReadFile(stateFile) //nolint:gosec // G304: sanitized path under controlled stateDir
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, nil //nolint:nilnil // (nil, nil) intentionally signals "not staged"; callers check for nil state
		}
		return nil, fmt.Errorf("read state file %q: %w", stateFile, readErr)
	}
	var state nodeStageState
	unmarshalErr := json.Unmarshal(data, &state)
	if unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal state file %q: %w", stateFile, unmarshalErr)
	}

	// Legacy migration (RFC §5.5.2): state files written by pre-Phase2 code
	// have the format {"subsys_nqn":"nqn.…"} with no "protocol_type" field.
	// Detect this shape and upgrade in place so that NodeUnstageVolume and
	// node-restart scenarios use the discriminated union path going forward.
	if state.ProtocolType == "" {
		var raw legacyNodeStageState
		legErr := json.Unmarshal(data, &raw)
		if legErr == nil && isLegacyFormat(&raw) {
			migrated := migrateFromLegacy(&raw)
			state = *migrated
			// In-place migration: rewrite the file in the new format.
			// Non-fatal: if the write fails we still return the migrated in-memory state.
			_ = n.writeStageState(volumeID, migrated) //nolint:errcheck // best-effort; non-fatal on write failure
		}
	}

	// Phase 3 backfill: AccessType joined the schema in the same change that
	// introduced Block-mode staging, so any state file that already
	// deserialised but lacks AccessType belongs to a Filesystem-mode volume.
	// Backfill in memory and rewrite best-effort so subsequent reads see the
	// canonical form.
	if state.AccessType == "" {
		state.AccessType = AccessTypeFilesystem
		_ = n.writeStageState(volumeID, &state) //nolint:errcheck // best-effort; non-fatal on write failure
	}

	return &state, nil
}

// sessionRestorer is implemented by protocol handlers whose sessions carry
// userspace-only parameters that must be re-applied after pillar-node
// restarts (see ISCSIHandler.RestoreSession).
type sessionRestorer interface {
	RestoreSession(state ProtocolState) error
}

// RestoreProtocolSessions re-applies the persisted userspace-only session
// parameters of every staged volume whose handler needs it.  The pillar-node
// process calls it once at startup, after the handlers adopted the kernel
// sessions that survived the restart: kubelet does not repeat
// NodeStageVolume for a volume that stays mounted, so the stage state files
// are the only record of those parameters.  The logf callback reports
// records restored with a fallback; the
// returned error joins one error per volume that could not be restored.
func (n *NodeServer) RestoreProtocolSessions(logf func(format string, args ...any)) error {
	entries, err := os.ReadDir(n.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("restore protocol sessions: read stage state dir %q: %w", n.stateDir, err)
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		stateFile := filepath.Join(n.stateDir, e.Name())
		restoreErr := n.restoreProtocolSession(stateFile, logf)
		if restoreErr != nil {
			errs = append(errs, fmt.Errorf("restore protocol session of %q: %w", stateFile, restoreErr))
		}
	}
	return errors.Join(errs...) //nolint:wrapcheck // every item is wrapped with its state file
}

// restoreProtocolSession re-applies the session parameters of one stage
// state file; records of other protocols and local attaches are skipped.
func (n *NodeServer) restoreProtocolSession(stateFile string, logf func(format string, args ...any)) error {
	data, err := os.ReadFile(stateFile) //nolint:gosec // G304: entry of the controlled stateDir
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var state nodeStageState
	err = json.Unmarshal(data, &state)
	if err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if state.ProtocolType != ProtocolISCSI || state.isLocalAttach() {
		return nil
	}
	handler, ok := n.handlers[state.ProtocolType].(sessionRestorer)
	if !ok {
		return fmt.Errorf("no session-restoring handler registered for protocol %q%s",
			state.ProtocolType, missingHandlerHint(state.ProtocolType))
	}
	protoState, err := state.ToProtocolState()
	if err != nil {
		return fmt.Errorf("convert stage state: %w", err)
	}
	if state.ISCSI.LoginTimeoutSeconds == 0 {
		logf("stage state %q of iSCSI target %s has no login timeout (written by an older pillar-node); "+
			"its session uses the default login timeout", stateFile, state.ISCSI.TargetIQN)
	}
	err = handler.RestoreSession(protoState)
	if err != nil {
		return fmt.Errorf("target %s: %w", state.ISCSI.TargetIQN, err)
	}
	return nil
}

// deleteStageState removes the stage state file for volumeID.  It is
// idempotent: if the file does not exist, the call succeeds silently.
func (n *NodeServer) deleteStageState(volumeID string) error {
	stateFile := n.stateFilePath(volumeID)
	removeErr := os.Remove(stateFile) // #nosec G703 -- path derived from controlled stateDir
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove state file %q: %w", stateFile, removeErr)
	}
	return nil
}
