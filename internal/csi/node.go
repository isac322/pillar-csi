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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/bhyoo/pillar-csi/internal/runtimepaths"
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
	// returned by the agent.  The semantics depend on the protocol:
	//   - NVMe-oF TCP: NVMe Qualified Name (NQN), e.g. "nqn.2024-01.com.example:vol1"
	//   - iSCSI:       target IQN, e.g. "iqn.2024-01.com.example:vol1"
	//   - NFS/SMB:     server IP address (the primary connection identifier)
	VolumeContextKeyTargetID = "target_id"

	// VolumeContextKeyAddress is the IP address (or hostname) of the storage
	// target.  Corresponds to ExportInfo.Address.
	VolumeContextKeyAddress = "address"

	// VolumeContextKeyPort is the TCP port of the storage target encoded as a
	// decimal string.  Derived from ExportInfo.Port.
	// Examples: "4420" for NVMe-oF TCP, "3260" for iSCSI.
	// May be empty for file protocols (NFS, SMB).
	VolumeContextKeyPort = "port"

	// VolumeContextKeyProtocolType is the storage protocol type set by the
	// controller in CreateVolume.  It is used by NodeStageVolume to dispatch
	// the volume to the correct ProtocolHandler (e.g. NVMeoFTCPHandler).
	// Known values: "nvmeof-tcp", "iscsi", "nfs", "smb".
	// Matches the StorageClass parameter "pillar-csi.bhyoo.com/protocol-type".
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

// NodeStageState discriminated union is defined in stage_state.go.

// Connector is the interface for NVMe-oF connect/disconnect operations.
// A real implementation issues nvme-cli commands or writes to /sys/class/nvme-fabrics.
// A test implementation returns pre-programmed responses without touching the kernel.
type Connector interface {
	// Connect establishes an NVMe-oF TCP connection to the given subsystem NQN
	// at the given transport address and service ID (port).
	// Implementations must be idempotent: connecting to an already-connected
	// subsystem must succeed without error.
	Connect(ctx context.Context, subsysNQN, trAddr, trSvcID string) error

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
	connErr := a.conn.Connect(ctx, params.ConnectionID, params.Address, params.Port)
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
	ProtocolISCSI:     {},
	ProtocolNFS:       {},
	ProtocolSMB:       {},
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
	// filesystem type (if not already formatted) and bind-mounts it at target.
	// fsType must be a kernel-supported filesystem name, e.g. "ext4" or "xfs".
	// options are passed verbatim as -o flags to mount(8).
	FormatAndMount(source, target, fsType string, options []string) error

	// Mount performs a plain mount of source at target with the given type and
	// options.  Callers use this for bind mounts (source already formatted).
	Mount(source, target, fsType string, options []string) error

	// Unmount unmounts the path at target.
	// Implementations must be idempotent: unmounting a path that is not
	// currently mounted must succeed without error.
	Unmount(target string) error

	// IsMounted returns true if target currently has an active mount.
	IsMounted(target string) (bool, error)
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
	// When nil no state-machine validation is performed (backward compatible).
	sm *VolumeStateMachine

	// resizer performs online filesystem expand operations in NodeExpandVolume.
	// When nil, NodeExpandVolume falls back to the default exec-based Resizer
	// (resize2fs for ext4, xfs_growfs for xfs).  Override in tests via
	// WithResizer to inject a mock without requiring real resize tools.
	resizer Resizer

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
	segs := buildTopologySegments(prober)

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
// Per CSI spec §4.7 the staging_target_path is guaranteed to be a pre-created
// directory (for MOUNT) or a pre-created file (for BLOCK) by the CO.
func (n *NodeServer) NodeStageVolume( //nolint:gocognit,gocyclo,funlen // multi-step attach/mount/persist
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
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

	// ── Step 1: Resolve protocol type ───────────────────────────────────────
	// Derive the protocol type from VolumeContext["pillar-csi.bhyoo.com/protocol-type"]
	// (preferred; set by the controller for all new volumes) or from the
	// volumeID path component.  Falls back to "nvmeof-tcp" for backward
	// compatibility with volumes provisioned before Phase 2.
	protocolType := resolveProtocolType(volumeID, volCtx)

	// ── Step 2: Protocol handler dispatch ───────────────────────────────────
	// Look up the handler registered for this protocol type.  A nil handlers
	// map means no handlers were registered (e.g., a state-only test server).
	handler := n.handlers[protocolType]
	if handler == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodeStageVolume: no handler registered for protocol %q", protocolType)
	}

	// ── Step 3: Protocol-specific VolumeContext validation ──────────────────
	// Extract common VolumeContext parameters used across protocols.
	// File protocols (NFS, SMB) may not require all three fields; their
	// handlers validate their own required parameters inside Attach.
	targetID := volCtx[VolumeContextKeyTargetID]
	address := volCtx[VolumeContextKeyAddress]
	port := volCtx[VolumeContextKeyPort]

	// NVMe-oF TCP requires target_id (NQN), address, and port.
	// iSCSI, NFS, SMB: handlers validate their own required parameters inside Attach.
	if protocolType == ProtocolNVMeoFTCP {
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
		default:
			// Volume is not in a state that permits NodeStageVolume.
			// ControllerPublishVolume must be called first.
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodeStageVolume is not valid in state %s; "+
					"ControllerPublishVolume must be called before NodeStageVolume",
				volumeID, smState)
		}
	}

	// ── Idempotency check ───────────────────────────────────────────────────
	// If the volume was already fully staged (state file exists + path mounted),
	// return success immediately per CSI spec §4.7.
	existingState, stateErr := n.readStageState(volumeID)
	if stateErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: read stage state for %q: %v", volumeID, stateErr)
	}
	if existingState != nil {
		mounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
		}
		if mounted {
			// Already fully staged — idempotent success.
			return &csi.NodeStageVolumeResponse{}, nil
		}
		// State file exists but mount is gone (e.g., node reboot).
		// Fall through to re-connect and re-mount below.
	}

	// ── Step 4: Attach via protocol handler ─────────────────────────────────
	// Attach performs transport-level connection setup (RFC §5.4.2 Layer 1)
	// and returns the device path (block protocols) or mount source (file
	// protocols) for Layer 2 presentation.
	attachResult, attachErr := handler.Attach(ctx, AttachParams{
		ProtocolType: protocolType,
		ConnectionID: targetID,
		Address:      address,
		Port:         port,
		Extra:        volCtx,
	})
	if attachErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, status.Errorf(codes.DeadlineExceeded,
				"NodeStageVolume: timed out waiting for device for volume %q (protocol %q)",
				volumeID, protocolType)
		}
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: attach volume %q (protocol %q): %v", volumeID, protocolType, attachErr)
	}
	devicePath := attachResult.DevicePath

	// Record partial state: transport attached; mount not yet started.
	// This drives the volume into NodeStagePartial so that a subsequent mount
	// failure leaves the SM in a recoverable state.  Only transition if the
	// SM is currently at ControllerPublished (not already at NodeStagePartial
	// from a prior retry attempt).
	if n.sm != nil && n.sm.GetState(volumeID) == StateControllerPublished {
		_, _ = n.sm.Transition(volumeID, OpNodeStageConnected) //nolint:errcheck // best-effort; does not affect mount
	}

	// ── Step 5: Mount or bind-mount depending on access type ───────────────
	switch {
	case volCap.GetMount() != nil:
		// MOUNT access: format (if not already formatted) and mount to staging path.
		fsType := volCap.GetMount().GetFsType()
		if fsType == "" {
			fsType = defaultFsType
		}
		mountFlags := volCap.GetMount().GetMountFlags()

		// Check IsMounted before FormatAndMount to provide an additional
		// idempotency guard (e.g., after a partial failure where the state
		// file write failed but the mount succeeded).
		alreadyMounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
		}
		if !alreadyMounted {
			formatErr := n.mounter.FormatAndMount(devicePath, stagingPath, fsType, mountFlags)
			if formatErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: format-and-mount %q → %q (fs=%s): %v",
					devicePath, stagingPath, fsType, formatErr)
			}
		}

	case volCap.GetBlock() != nil:
		// BLOCK access: bind-mount the raw device to the staging path.
		alreadyMounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
		}
		if !alreadyMounted {
			bindErr := n.mounter.Mount(devicePath, stagingPath, "", []string{"bind"})
			if bindErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: bind-mount block device %q → %q: %v",
					devicePath, stagingPath, bindErr)
			}
		}

	default:
		return nil, status.Error(codes.InvalidArgument, //nolint:wrapcheck
			"NodeStageVolume: volume_capability must specify mount or block access type")
	}

	// ── Step 6: Persist stage state ─────────────────────────────────────────
	// Write the protocol state to a state file so NodeUnstageVolume can
	// disconnect the correct target even though it does not receive VolumeContext.
	// The state is derived from the AttachResult so each protocol stores exactly
	// the teardown parameters its handler needs.
	stageState := stageStateFromAttachResult(protocolType, targetID, address, port, attachResult)
	writeErr := n.writeStageState(volumeID, stageState)
	if writeErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: persist stage state for %q: %v", volumeID, writeErr)
	}

	// ── Step 7: Advance state machine to NodeStaged ──────────────────────────
	// All staging work (connect + mount + state file) completed successfully.
	// Force the SM directly to NodeStaged regardless of whether we entered
	// from ControllerPublished (→ NodeStagePartial via Step 1 above) or from
	// NodeStagePartial (retry).
	if n.sm != nil {
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
//  2. Unmount the staging target path (skipped if not currently mounted).
//  3. Read the persisted stage state file to recover the protocol-specific
//     teardown state.  If no state file exists the volume was never staged
//     (or already unstaged); the call succeeds idempotently.
//  4. Dispatch to the ProtocolHandler registered for the persisted protocol
//     type and call Detach.  Detach is idempotent: disconnecting an already-
//     disconnected target is a no-op.
//  5. Remove the stage state file to mark the volume as fully unstaged.
//
// The operation is idempotent per CSI spec §4.7.
func (n *NodeServer) NodeUnstageVolume( //nolint:gocyclo,funlen // SM guard + unmount + dispatch + state cleanup
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
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

	// ── Step 1: Unmount staging path ────────────────────────────────────────
	// IsMounted check provides idempotency; calling Unmount on an already-
	// unmounted path would be a no-op in most implementations, but the check
	// makes the intent explicit.
	mounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
	if mountCheckErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
	}
	if mounted {
		unmountErr := n.mounter.Unmount(stagingPath)
		if unmountErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeUnstageVolume: unmount %q: %v", stagingPath, unmountErr)
		}
	}

	// ── Step 2: Read stage state to recover protocol teardown state ─────────
	state, readErr := n.readStageState(volumeID)
	if readErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: read stage state for %q: %v", volumeID, readErr)
	}
	if state == nil {
		// No state file: volume was never staged or was already cleanly unstaged.
		// Succeed idempotently — the CO may call NodeUnstageVolume more than once.
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	// ── Step 3: Disconnect the storage target ───────────────────────────────
	// Dispatch to the ProtocolHandler registered for the persisted protocol type.
	// Detach is idempotent: disconnecting an already-disconnected target is a no-op.
	if n.handlers != nil {
		handler, ok := n.handlers[state.ProtocolType]
		if !ok {
			return nil, status.Errorf(codes.Internal,
				"NodeUnstageVolume: no handler registered for protocol %q", state.ProtocolType)
		}
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

// ─────────────────────────────────────────────────────────────────────────────
// NodePublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

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
//  4. For BLOCK access type: bind-mount the staging_target_path (which holds the
//     raw block device bind) to target_path.
//
// Per CSI spec §4.7 the target_path is pre-created by the CO before this call.
func (n *NodeServer) NodePublishVolume( //nolint:gocyclo // SM guard + capability switch + readonly handling
	_ context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetStagingTargetPath() == "" {
		stagingPathErr := status.Error(codes.InvalidArgument,
			"NodePublishVolume: staging_target_path is required")
		return nil, stagingPathErr //nolint:wrapcheck // gRPC status; must not be wrapped
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: target_path is required") //nolint:wrapcheck
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: volume_capability is required") //nolint:wrapcheck
	}

	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()
	volCap := req.GetVolumeCapability()
	readonly := req.GetReadonly()

	// ── State machine ordering guard ────────────────────────────────────────
	if n.sm != nil {
		smState := n.sm.GetState(volumeID)
		switch smState {
		case StateNodeStaged:
			// Happy path: NodeStageVolume completed — proceed.
		case StateNodePublished:
			// Already published: fall through to the IsMounted idempotency
			// check below, which will detect the existing bind-mount and
			// return success without repeating the mount.
		default:
			// Volume is not staged: NodeStageVolume must be called first.
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %q: NodePublishVolume is not valid in state %s; "+
					"NodeStageVolume must be called before NodePublishVolume",
				volumeID, smState)
		}
	}

	// ── Idempotency check ───────────────────────────────────────────────────
	// If target_path is already mounted return success immediately per CSI spec §4.7.
	alreadyMounted, mountCheckErr := n.mounter.IsMounted(targetPath)
	if mountCheckErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodePublishVolume: check if %q is mounted: %v", targetPath, mountCheckErr)
	}
	if alreadyMounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// ── Build mount options ─────────────────────────────────────────────────
	// Start with "bind" to perform a bind mount from the staging path.
	// Append any caller-supplied mount flags, then add "ro" if readonly.
	mountOptions := []string{"bind"}
	if volCap.GetMount() != nil {
		mountOptions = append(mountOptions, volCap.GetMount().GetMountFlags()...)
	}
	if readonly {
		mountOptions = append(mountOptions, "ro")
	}

	// ── Determine fsType ────────────────────────────────────────────────────
	// For a bind mount the fsType is typically empty (kernel re-uses the
	// source's filesystem type).  We pass the explicit fsType only for MOUNT
	// access so that the mounter implementation can make use of it if needed.
	fsType := ""
	if volCap.GetMount() != nil {
		fsType = volCap.GetMount().GetFsType()
	}

	// ── Perform bind mount ──────────────────────────────────────────────────
	switch {
	case volCap.GetMount() != nil, volCap.GetBlock() != nil:
		// Both MOUNT and BLOCK access types use a bind mount from the staging
		// path to the target path.  For BLOCK the staging path holds a raw
		// device bind already established during NodeStageVolume; we simply
		// propagate it to the pod's target path.
		bindErr := n.mounter.Mount(stagingPath, targetPath, fsType, mountOptions)
		if bindErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodePublishVolume: bind-mount %q → %q: %v",
				stagingPath, targetPath, bindErr)
		}
	default:
		return nil, status.Error(codes.InvalidArgument, //nolint:wrapcheck
			"NodePublishVolume: volume_capability must specify mount or block access type")
	}

	// ── Advance state machine to NodePublished ───────────────────────────────
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateNodePublished)
	}

	return &csi.NodePublishVolumeResponse{}, nil
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
//  2. Check whether target_path is currently mounted.
//  3. If mounted, call Unmount; if not mounted, return success immediately.
func (n *NodeServer) NodeUnpublishVolume(
	_ context.Context,
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: target_path is required") //nolint:wrapcheck
	}

	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()

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

	// ── Idempotency: check if already unmounted ─────────────────────────────
	mounted, mountCheckErr := n.mounter.IsMounted(targetPath)
	if mountCheckErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnpublishVolume: check if %q is mounted: %v", targetPath, mountCheckErr)
	}
	if !mounted {
		// Target path is not mounted — already unpublished or never published.
		// Succeed idempotently.
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	// ── Unmount the bind mount ──────────────────────────────────────────────
	unmountErr := n.mounter.Unmount(targetPath)
	if unmountErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnpublishVolume: unmount %q: %v", targetPath, unmountErr)
	}

	// ── Revert state machine to NodeStaged ──────────────────────────────────
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateNodeStaged)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Stage state helpers
// ─────────────────────────────────────────────────────────────────────────────.

// stateFilePath returns the filesystem path for the JSON state file of the
// given volumeID.  Path separators in the volumeID are replaced with
// underscores to produce a valid single-file filename.
func (n *NodeServer) stateFilePath(volumeID string) string {
	safeID := strings.ReplaceAll(volumeID, "/", "_")
	return filepath.Join(n.stateDir, safeID+".json")
}

// writeStageState serializes state to the JSON file for volumeID under
// stateDir.  The directory is created if it does not yet exist.
func (n *NodeServer) writeStageState(volumeID string, state *nodeStageState) error {
	mkdirErr := os.MkdirAll(n.stateDir, 0o700)
	if mkdirErr != nil {
		return fmt.Errorf("create state directory %q: %w", n.stateDir, mkdirErr)
	}
	data, marshalErr := json.Marshal(state)
	if marshalErr != nil {
		return fmt.Errorf("marshal stage state: %w", marshalErr)
	}
	stateFile := n.stateFilePath(volumeID)
	writeErr := os.WriteFile(stateFile, data, 0o600)
	if writeErr != nil {
		return fmt.Errorf("write state file %q: %w", stateFile, writeErr)
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

	return &state, nil
}

// deleteStageState removes the stage state file for volumeID.  It is
// idempotent: if the file does not exist, the call succeeds silently.
func (n *NodeServer) deleteStageState(volumeID string) error {
	stateFile := n.stateFilePath(volumeID)
	removeErr := os.Remove(stateFile)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove state file %q: %w", stateFile, removeErr)
	}
	return nil
}
