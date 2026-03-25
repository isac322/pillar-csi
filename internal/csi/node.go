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
)

// ─────────────────────────────────────────────────────────────────────────────
// VolumeContext keys
// ─────────────────────────────────────────────────────────────────────────────

// VolumeContext keys set by the controller in CreateVolume and stored in the
// PersistentVolume spec.csi.volumeAttributes map.  The CO propagates these
// to NodeStageVolume so the node knows how to connect to the NVMe-oF target.
const (
	// VolumeContextKeyTargetNQN is the NVMe Qualified Name of the target
	// subsystem.  Corresponds to ExportInfo.TargetId returned by the agent.
	VolumeContextKeyTargetNQN = "target_id"

	// VolumeContextKeyAddress is the IP address (or hostname) of the NVMe-oF
	// TCP target.  Corresponds to ExportInfo.Address.
	VolumeContextKeyAddress = "address"

	// VolumeContextKeyPort is the TCP port of the NVMe-oF target encoded as a
	// decimal string, e.g. "4420".  Derived from ExportInfo.Port.
	VolumeContextKeyPort = "port"
)

// ─────────────────────────────────────────────────────────────────────────────
// Staging constants and state types
// ─────────────────────────────────────────────────────────────────────────────

// deviceWaitTimeout is the maximum time NodeStageVolume waits for the NVMe
// block device to appear after a successful Connect call.
const deviceWaitTimeout = 30 * time.Second

// devicePollInterval is the sleep between successive GetDevicePath polls.
const devicePollInterval = 500 * time.Millisecond

// defaultFsType is the filesystem type used by NodeStageVolume when the
// VolumeCapability does not specify an explicit fsType.
const defaultFsType = "ext4"

// defaultStateDir is the directory used to persist per-volume staging state
// when NodeServer is created via NewNodeServer.
const defaultStateDir = "/var/lib/pillar-csi/node"

// nodeStageState is the on-disk structure persisted during NodeStageVolume.
// It records just enough information for NodeUnstageVolume to disconnect the
// NVMe-oF subsystem without re-reading the PersistentVolume attributes
// (which NodeUnstageVolume does not receive).
type nodeStageState struct {
	// SubsysNQN is the NVMe Qualified Name of the connected subsystem.
	SubsysNQN string `json:"subsys_nqn"`
}

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
// the CSI lifecycle: connecting volumes via NVMe-oF, formatting filesystems,
// and bind-mounting into pod target paths.
//
// All privileged operations are delegated to the injectable Connector and
// Mounter interfaces so that the server can be tested without kernel modules
// or root privileges.
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

	// connector performs NVMe-oF connect / disconnect / device-path resolution.
	connector Connector

	// mounter performs filesystem format and bind-mount operations.
	mounter Mounter

	// stateDir is the directory in which per-volume staging state files are
	// persisted.  Each staged volume has a JSON file named after its (sanitised)
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
}

// Ensure NodeServer satisfies the interface at compile time.
var _ csi.NodeServer = (*NodeServer)(nil)

// NewNodeServer constructs a NodeServer with the given node identity and
// injectable operation backends.  The staging state directory defaults to
// /var/lib/pillar-csi/node.
//
//   - nodeID     – unique node name used in NodeGetInfo (typically the
//                  Kubernetes node name, e.g. "worker-1").
//   - connector  – NVMe-oF connect/disconnect implementation.
//   - mounter    – filesystem format/mount/unmount implementation.
func NewNodeServer(nodeID string, connector Connector, mounter Mounter) *NodeServer {
	return NewNodeServerWithStateDir(nodeID, connector, mounter, defaultStateDir)
}

// NewNodeServerWithStateDir constructs a NodeServer with an explicit staging
// state directory.  Use this variant in tests to point the state dir at a
// t.TempDir() so that staging state is isolated between test cases and does
// not require /var/lib to exist.
//
//   - nodeID    – unique node name used in NodeGetInfo.
//   - connector – NVMe-oF connect/disconnect implementation.
//   - mounter   – filesystem format/mount/unmount implementation.
//   - stateDir  – directory for per-volume JSON state files; created on first
//                 use if absent.
func NewNodeServerWithStateDir(nodeID string, connector Connector, mounter Mounter, stateDir string) *NodeServer {
	return &NodeServer{
		nodeID:    nodeID,
		connector: connector,
		mounter:   mounter,
		stateDir:  stateDir,
	}
}

// NewNodeServerWithStateMachine constructs a NodeServer that shares the given
// VolumeStateMachine with the ControllerServer.  With a shared SM every node
// operation validates the volume's current lifecycle state before executing
// privileged work, returning FailedPrecondition for out-of-order requests
// (e.g. NodeStageVolume before ControllerPublishVolume).
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
		nodeID:    nodeID,
		connector: connector,
		mounter:   mounter,
		stateDir:  stateDir,
		sm:        sm,
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
func (n *NodeServer) NodeGetCapabilities(
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
//   - NodeId: the node identifier supplied at construction time (typically the
//     Kubernetes node name).  The CO records this in the CSI node object so that
//     the Controller can target AllowInitiator / DenyInitiator calls to the
//     correct agent.
//
// MaxVolumesPerNode is left at 0 (unlimited) for Phase 1.
// AccessibleTopology is left empty for Phase 1; topology-aware provisioning
// is a future enhancement.
func (n *NodeServer) NodeGetInfo(
	_ context.Context,
	_ *csi.NodeGetInfoRequest,
) (*csi.NodeGetInfoResponse, error) {
	if n.nodeID == "" {
		return nil, status.Error(codes.Internal, "node server has no node ID configured")
	}

	return &csi.NodeGetInfoResponse{
		NodeId: n.nodeID,
		// MaxVolumesPerNode: 0 means unlimited (CSI spec default).
		// AccessibleTopology: nil means no topology constraints for Phase 1.
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeStageVolume
// ─────────────────────────────────────────────────────────────────────────────

// NodeStageVolume connects this node to a volume via NVMe-oF TCP and prepares
// it for use by pods.
//
// Sequence:
//  1. Validate required fields and extract NVMe-oF target parameters from the
//     VolumeContext (target_id, address, port) set by the controller.
//  2. Check idempotency: if the volume is already staged and its staging path
//     is currently mounted, return success immediately.
//  3. Connect to the NVMe-oF subsystem via the injected Connector.  Connect
//     is idempotent: calling it on an already-connected subsystem is a no-op.
//  4. Poll for the block-device path until it appears or the deadline expires.
//  5. Format-and-mount (for MOUNT access type) or bind-mount the raw device
//     (for BLOCK access type) at the staging target path.
//  6. Persist a JSON state file in stateDir recording the subsystem NQN so
//     that NodeUnstageVolume can disconnect the correct subsystem without
//     re-reading the VolumeContext.
//
// Per CSI spec §4.7 the staging_target_path is guaranteed to be a pre-created
// directory (for MOUNT) or a pre-created file (for BLOCK) by the CO.
func (n *NodeServer) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest,
) (*csi.NodeStageVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: volume_id is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: staging_target_path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "NodeStageVolume: volume_capability is required")
	}

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	volCtx := req.GetVolumeContext()
	cap := req.GetVolumeCapability()

	// Extract NVMe-oF connection parameters set by the controller during
	// CreateVolume (sourced from the agent's ExportInfo).
	subsysNQN := volCtx[VolumeContextKeyTargetNQN]
	trAddr := volCtx[VolumeContextKeyAddress]
	trSvcID := volCtx[VolumeContextKeyPort]

	if subsysNQN == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyTargetNQN)
	}
	if trAddr == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyAddress)
	}
	if trSvcID == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"NodeStageVolume: volume_context missing required key %q", VolumeContextKeyPort)
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
			// Retry after a partial failure (NVMe-oF connect succeeded but
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

	// ── Step 1: Connect to the NVMe-oF subsystem ───────────────────────────
	// Connector.Connect is idempotent: calling it when already connected is safe.
	if connectErr := n.connector.Connect(ctx, subsysNQN, trAddr, trSvcID); connectErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: NVMe-oF connect to %q at %s:%s: %v",
			subsysNQN, trAddr, trSvcID, connectErr)
	}

	// Record partial state: NVMe-oF connect succeeded; mount not yet started.
	// This drives the volume into NodeStagePartial so that a subsequent mount
	// failure leaves the SM in a recoverable state.  Only transition if the
	// SM is currently at ControllerPublished (not already at NodeStagePartial
	// from a prior retry attempt).
	if n.sm != nil && n.sm.GetState(volumeID) == StateControllerPublished {
		_, _ = n.sm.Transition(volumeID, OpNodeStageConnected)
	}

	// ── Step 2: Wait for the block device to appear ─────────────────────────
	// After Connect the kernel enqueues a uevent that eventually creates the
	// /dev/nvmeXnY node; we poll until it appears or the deadline is exceeded.
	var devicePath string
	pollCtx, pollCancel := context.WithTimeout(ctx, deviceWaitTimeout)
	defer pollCancel()

	for {
		devPath, devErr := n.connector.GetDevicePath(pollCtx, subsysNQN)
		if devErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: get device path for %q: %v", subsysNQN, devErr)
		}
		if devPath != "" {
			devicePath = devPath
			break
		}
		select {
		case <-pollCtx.Done():
			return nil, status.Errorf(codes.DeadlineExceeded,
				"NodeStageVolume: block device for NQN %q did not appear within %s",
				subsysNQN, deviceWaitTimeout)
		case <-time.After(devicePollInterval):
			// Poll again.
		}
	}

	// ── Step 3: Mount or bind-mount depending on access type ───────────────
	switch {
	case cap.GetMount() != nil:
		// MOUNT access: format (if not already formatted) and mount to staging path.
		fsType := cap.GetMount().GetFsType()
		if fsType == "" {
			fsType = defaultFsType
		}
		mountFlags := cap.GetMount().GetMountFlags()

		// Check IsMounted before FormatAndMount to provide an additional
		// idempotency guard (e.g., after a partial failure where the state
		// file write failed but the mount succeeded).
		alreadyMounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
		}
		if !alreadyMounted {
			if formatErr := n.mounter.FormatAndMount(devicePath, stagingPath, fsType, mountFlags); formatErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: format-and-mount %q → %q (fs=%s): %v",
					devicePath, stagingPath, fsType, formatErr)
			}
		}

	case cap.GetBlock() != nil:
		// BLOCK access: bind-mount the raw device to the staging path.
		alreadyMounted, mountCheckErr := n.mounter.IsMounted(stagingPath)
		if mountCheckErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeStageVolume: check if %q is mounted: %v", stagingPath, mountCheckErr)
		}
		if !alreadyMounted {
			if bindErr := n.mounter.Mount(devicePath, stagingPath, "" /*fsType unused for bind*/, []string{"bind"}); bindErr != nil {
				return nil, status.Errorf(codes.Internal,
					"NodeStageVolume: bind-mount block device %q → %q: %v",
					devicePath, stagingPath, bindErr)
			}
		}

	default:
		return nil, status.Error(codes.InvalidArgument,
			"NodeStageVolume: volume_capability must specify mount or block access type")
	}

	// ── Step 4: Persist stage state ─────────────────────────────────────────
	// Write the subsystem NQN to a state file so NodeUnstageVolume can
	// disconnect the correct NQN even though it does not receive VolumeContext.
	if writeErr := n.writeStageState(volumeID, &nodeStageState{SubsysNQN: subsysNQN}); writeErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeStageVolume: persist stage state for %q: %v", volumeID, writeErr)
	}

	// ── Step 5: Advance state machine to NodeStaged ──────────────────────────
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
// ─────────────────────────────────────────────────────────────────────────────

// NodeUnstageVolume unmounts the staging path and disconnects the NVMe-oF
// subsystem for the given volume.
//
// Sequence:
//  1. Validate required fields.
//  2. Unmount the staging target path (skipped if not currently mounted).
//  3. Read the persisted stage state file to recover the subsystem NQN.
//     If no state file exists the volume was never staged (or already unstaged);
//     the call succeeds idempotently.
//  4. Disconnect the NVMe-oF subsystem via the injected Connector.  Disconnect
//     is idempotent: calling it on a non-connected NQN is a no-op.
//  5. Remove the stage state file to mark the volume as fully unstaged.
//
// The operation is idempotent per CSI spec §4.7.
func (n *NodeServer) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest,
) (*csi.NodeUnstageVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume: volume_id is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnstageVolume: staging_target_path is required")
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
		if unmountErr := n.mounter.Unmount(stagingPath); unmountErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodeUnstageVolume: unmount %q: %v", stagingPath, unmountErr)
		}
	}

	// ── Step 2: Read stage state to recover the NQN ─────────────────────────
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

	// ── Step 3: Disconnect the NVMe-oF subsystem ────────────────────────────
	// Connector.Disconnect is idempotent: disconnecting a non-connected NQN
	// must succeed without error per the Connector interface contract.
	if disconnectErr := n.connector.Disconnect(ctx, state.SubsysNQN); disconnectErr != nil {
		return nil, status.Errorf(codes.Internal,
			"NodeUnstageVolume: NVMe-oF disconnect from %q: %v",
			state.SubsysNQN, disconnectErr)
	}

	// ── Step 4: Remove stage state file ─────────────────────────────────────
	if deleteErr := n.deleteStageState(volumeID); deleteErr != nil {
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
// ─────────────────────────────────────────────────────────────────────────────

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
func (n *NodeServer) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	// ── Input validation ────────────────────────────────────────────────────
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: volume_id is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: staging_target_path is required")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: target_path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "NodePublishVolume: volume_capability is required")
	}

	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()
	cap := req.GetVolumeCapability()
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
	if cap.GetMount() != nil {
		mountOptions = append(mountOptions, cap.GetMount().GetMountFlags()...)
	}
	if readonly {
		mountOptions = append(mountOptions, "ro")
	}

	// ── Determine fsType ────────────────────────────────────────────────────
	// For a bind mount the fsType is typically empty (kernel re-uses the
	// source's filesystem type).  We pass the explicit fsType only for MOUNT
	// access so that the mounter implementation can make use of it if needed.
	fsType := ""
	if cap.GetMount() != nil {
		fsType = cap.GetMount().GetFsType()
	}

	// ── Perform bind mount ──────────────────────────────────────────────────
	switch {
	case cap.GetMount() != nil, cap.GetBlock() != nil:
		// Both MOUNT and BLOCK access types use a bind mount from the staging
		// path to the target path.  For BLOCK the staging path holds a raw
		// device bind already established during NodeStageVolume; we simply
		// propagate it to the pod's target path.
		if bindErr := n.mounter.Mount(stagingPath, targetPath, fsType, mountOptions); bindErr != nil {
			return nil, status.Errorf(codes.Internal,
				"NodePublishVolume: bind-mount %q → %q: %v",
				stagingPath, targetPath, bindErr)
		}
	default:
		return nil, status.Error(codes.InvalidArgument,
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
// ─────────────────────────────────────────────────────────────────────────────

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
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: volume_id is required")
	}
	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: target_path is required")
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
	if unmountErr := n.mounter.Unmount(targetPath); unmountErr != nil {
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
// ─────────────────────────────────────────────────────────────────────────────

// stateFilePath returns the filesystem path for the JSON state file of the
// given volumeID.  Path separators in the volumeID are replaced with
// underscores to produce a valid single-file filename.
func (n *NodeServer) stateFilePath(volumeID string) string {
	safeID := strings.ReplaceAll(volumeID, "/", "_")
	return filepath.Join(n.stateDir, safeID+".json")
}

// writeStageState serialises state to the JSON file for volumeID under
// stateDir.  The directory is created if it does not yet exist.
func (n *NodeServer) writeStageState(volumeID string, state *nodeStageState) error {
	if mkdirErr := os.MkdirAll(n.stateDir, 0o700); mkdirErr != nil {
		return fmt.Errorf("create state directory %q: %w", n.stateDir, mkdirErr)
	}
	data, marshalErr := json.Marshal(state)
	if marshalErr != nil {
		return fmt.Errorf("marshal stage state: %w", marshalErr)
	}
	stateFile := n.stateFilePath(volumeID)
	if writeErr := os.WriteFile(stateFile, data, 0o600); writeErr != nil {
		return fmt.Errorf("write state file %q: %w", stateFile, writeErr)
	}
	return nil
}

// readStageState reads and deserialises the stage state for volumeID.
// Returns (nil, nil) when no state file exists (volume not yet staged or
// already cleanly unstaged).
func (n *NodeServer) readStageState(volumeID string) (*nodeStageState, error) {
	stateFile := n.stateFilePath(volumeID)
	data, readErr := os.ReadFile(stateFile)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, nil // not staged
		}
		return nil, fmt.Errorf("read state file %q: %w", stateFile, readErr)
	}
	var state nodeStageState
	if unmarshalErr := json.Unmarshal(data, &state); unmarshalErr != nil {
		return nil, fmt.Errorf("unmarshal state file %q: %w", stateFile, unmarshalErr)
	}
	return &state, nil
}

// deleteStageState removes the stage state file for volumeID.  It is
// idempotent: if the file does not exist, the call succeeds silently.
func (n *NodeServer) deleteStageState(volumeID string) error {
	stateFile := n.stateFilePath(volumeID)
	if removeErr := os.Remove(stateFile); removeErr != nil && !os.IsNotExist(removeErr) {
		return fmt.Errorf("remove state file %q: %w", stateFile, removeErr)
	}
	return nil
}
