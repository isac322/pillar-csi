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
	"os"
	"path/filepath"
	"slices"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ─────────────────────────────────────────────────────────────────────────────
// Direct (stageless) publish of adopted filesystems — file driver only
// ─────────────────────────────────────────────────────────────────────────────
//
// The file driver does not advertise STAGE_UNSTAGE_VOLUME, so kubelet keeps no
// global device mount of the shared host filesystem and calls
// NodePublishVolume with an empty staging_target_path.  Each publish mounts the
// adoption straight onto the pod target:
//
//   - local (same node as the agent): a bind of the controller/agent-owned
//     proxy mount, verified against the immutable adoption identity before
//     and after the bind;
//   - remote: a direct NFSv4.2 mount of the controller-published export.
//
// The node owns only the target mounts.  Each NodeUnpublishVolume unmounts
// its own recorded, ownership-verified target; an unrecorded mounted path
// (the owned proxy, a peer or a foreign mount) is refused, and the proxy
// mount, the NFS export and every peer target stay in place.  One durable
// record per volume (nodeStageState.File, stored at the usual state file
// path) carries the immutable identity used by NodeGetVolumeStats plus the
// list of targets; a target is recorded before its mount is made (durable
// intent, so a crash never strands an unrecorded mount) and the record is
// deleted when the last target is unpublished.
//
// Every adopted or freshly made target mount is checked through
// Mounter.ObserveMount: the kernel's read-only flag must equal the target's
// readonly, and a remote mount must be NFS from the recorded export.

// filePublishPlan is the resolved mount of one direct publish.
type filePublishPlan struct {
	source  string
	fsType  string
	options []string
	nfs     *NFSStageState
}

func (n *NodeServer) nodePublishFilesystem(req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID, targetPath := req.GetVolumeId(), req.GetTargetPath()
	err := validateFilePublishRequest(req)
	if err != nil {
		return nil, err
	}

	// Serialize with every other publish/unpublish of the volume: the record
	// holds all targets and is rewritten as a whole.
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	fileCtx, err := n.parseFilePublishContext(volumeID, req.GetVolumeContext(), req.GetPublishContext())
	if err != nil {
		if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
			return nil, err
		}
		return nil, status.Errorf(codes.FailedPrecondition, "NodePublishVolume: filesystem context: %v", err)
	}
	err = validateFilePublishContext(req, fileCtx)
	if err != nil {
		return nil, err
	}
	plan, err := resolveFilePublishPlan(req, fileCtx)
	if err != nil {
		return nil, err
	}
	err = n.validateFilePublishState(volumeID)
	if err != nil {
		return nil, err
	}
	state, err := n.filePublishRecord(volumeID, fileCtx, plan)
	if err != nil {
		return nil, err
	}

	if n.fileTargetAliasesSource(state.File, targetPath) {
		return nil, status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: target_path %q is the adopted source or its owned proxy", targetPath)
	}
	readOnly := req.GetReadonly()
	idx := findFilePublishTarget(state.File.Targets, targetPath)
	if idx >= 0 && state.File.Targets[idx].ReadOnly != readOnly {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q is already published at %q with readonly=%t",
			volumeID, targetPath, state.File.Targets[idx].ReadOnly)
	}

	err = n.ensureFilePublishMount(volumeID, targetPath, readOnly, plan, state)
	if err != nil {
		return nil, err
	}
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateNodePublished)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func validateFilePublishRequest(req *csi.NodePublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: target_path is required") //nolint:wrapcheck
	}
	if req.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument, "NodePublishVolume: volume_capability is required") //nolint:wrapcheck
	}
	if req.GetVolumeCapability().GetMount() == nil || req.GetVolumeCapability().GetBlock() != nil {
		return status.Error(codes.InvalidArgument, //nolint:wrapcheck
			"NodePublishVolume: adopted filesystems require mount access")
	}
	return nil
}

// validateFilePublishContext checks the request against the trusted adoption
// before any mount side effect.
func validateFilePublishContext(req *csi.NodePublishVolumeRequest, fileCtx *filePublishContext) error {
	volCtx, mount := req.GetVolumeContext(), req.GetVolumeCapability().GetMount()
	if !fileCtx.local &&
		(req.GetPublishContext()[PublishContextKeyAttachMode] == AttachModeLocal ||
			volCtx[VolumeContextKeyFilesystemLocalAttach] == topologyValueTrue) {
		return status.Errorf(codes.FailedPrecondition,
			"%s", "NodePublishVolume: adopted filesystem local attach is missing its controller-owned proxy path")
	}
	if fsType := mount.GetFsType(); fsType != "" &&
		fsType != fileCtx.adoption.FilesystemType && !nfsTransportFsTypeHint(req.GetVolumeId(), volCtx, mount) {
		return status.Errorf(codes.InvalidArgument,
			"NodePublishVolume: filesystem type %q does not match adopted type %q",
			fsType, fileCtx.adoption.FilesystemType)
	}
	return nil
}

// resolveFilePublishPlan picks the mount of one publish: a bind of the owned
// proxy for a local adoption, a direct NFS mount of the export otherwise.
// The request's readonly flag is applied to the target mount only.
func resolveFilePublishPlan(
	req *csi.NodePublishVolumeRequest, fileCtx *filePublishContext,
) (*filePublishPlan, error) {
	volumeID, volCtx, volCap := req.GetVolumeId(), req.GetVolumeContext(), req.GetVolumeCapability()
	if fileCtx.local {
		flags, err := resolveMountFlags(volCtx, volCap)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "NodePublishVolume: volume %q: %v", volumeID, err)
		}
		options := append([]string{bindMountOption}, flags...)
		if req.GetReadonly() {
			options = append(options, "ro")
		}
		return &filePublishPlan{source: fileCtx.proxy, options: options}, nil
	}

	if resolveProtocolType(volumeID, volCtx) != ProtocolNFS {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q: remote filesystem publish requires the NFS transport", volumeID)
	}
	err := validateNFSStageFilesystem(volCtx, volCap.GetMount())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "NodePublishVolume: volume %q: %v", volumeID, err)
	}
	nfsState, err := nfsStateFromParams(AttachParams{
		ProtocolType: ProtocolNFS,
		ConnectionID: volCtx[VolumeContextKeyTargetID],
		Address:      volCtx[VolumeContextKeyAddress],
		Port:         volCtx[VolumeContextKeyPort],
		VolumeRef:    volCtx[vcVolumeRef],
		Extra:        volCtx,
	})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "NodePublishVolume: volume %q: %v", volumeID, err)
	}
	options, err := nfsMountFlags(volCtx, volCap.GetMount(), nfsState.Address)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "NodePublishVolume: volume %q: %v", volumeID, err)
	}
	if req.GetReadonly() {
		options = append(options, "ro")
	}
	return &filePublishPlan{
		source: nfsState.MountSource, fsType: ProtocolNFS, options: options,
		nfs: &NFSStageState{
			Address: nfsState.Address, ExportPath: nfsState.ExportPath, Port: nfsState.Port,
			Version: nfsState.Version, MountSource: nfsState.MountSource,
		},
	}, nil
}

// validateFilePublishState is the shared state machine guard.  Without
// staging, a publish follows ControllerPublishVolume directly.
func (n *NodeServer) validateFilePublishState(volumeID string) error {
	if n.sm == nil {
		return nil
	}
	switch smState := n.sm.GetState(volumeID); smState {
	case StateControllerPublished, StateNodePublished:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition,
			"volume %q: NodePublishVolume is not valid in state %s: "+
				"ControllerPublishVolume must be called before NodePublishVolume", volumeID, smState)
	}
}

// filePublishRecord returns the volume's publish record, verifying that an
// existing one names the same immutable adoption and publish mechanism.
func (n *NodeServer) filePublishRecord(
	volumeID string, fileCtx *filePublishContext, plan *filePublishPlan,
) (*nodeStageState, error) {
	state, err := n.readStageState(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodePublishVolume: read publish state for %q: %v", volumeID, err)
	}
	if state == nil {
		return &nodeStageState{
			ProtocolType: ProtocolNFS, AccessType: AccessTypeFilesystem,
			VolumeID: volumeID, File: fileCtx.state, NFS: plan.nfs,
		}, nil
	}
	if state.File == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q has a non-filesystem node record", volumeID)
	}
	rejectErr := rejectStagedFileRecord("NodePublishVolume", volumeID, state)
	if rejectErr != nil {
		return nil, rejectErr
	}
	if state.File.Local != fileCtx.local || state.File.ProxyPath != fileCtx.proxy {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q: recorded publish route (local=%t proxy=%q) differs from "+
				"the current publish context (local=%t proxy=%q)",
			volumeID, state.File.Local, state.File.ProxyPath, fileCtx.local, fileCtx.proxy)
	}
	// The recorded transport origin is immutable: a remote record must carry
	// the exact NFS source the request resolves to, a local one none.  A
	// missing or different origin is corrupt or drifted and is never
	// backfilled from the request.
	if plan.nfs == nil && state.NFS != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q: local publish record carries an NFS source %q", volumeID, state.NFS.MountSource)
	}
	if plan.nfs != nil && (state.NFS == nil || state.NFS.MountSource == "" ||
		state.NFS.MountSource != plan.nfs.MountSource) {
		recorded := ""
		if state.NFS != nil {
			recorded = state.NFS.MountSource
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: volume %q: recorded NFS source %q differs from %q",
			volumeID, recorded, plan.nfs.MountSource)
	}
	err = refreshFilePublishMetadata(state, fileCtx.state)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "NodePublishVolume: volume %q: %v", volumeID, err)
	}
	state.VolumeID = volumeID
	return state, nil
}

// ensureFilePublishMount makes targetPath carry a verified mount of the
// adoption with the requested access mode and records it as a target.
//
// The target is recorded durably BEFORE a new mount is made, so a crash
// between mount(2) and the final record write never leaves an unrecorded
// mount that NodeUnpublishVolume would refuse: the next publish or
// unpublish finds the recorded target and verifies or removes it.  A
// failed mount or verification rolls the intent back; a mount this call
// created that cannot be unmounted keeps the intent so a retry converges.
// An existing mount is adopted only after its ownership, type, access mode
// and readability are proven; it is never replaced or torn down here.
func (n *NodeServer) ensureFilePublishMount(
	volumeID, targetPath string, readOnly bool, plan *filePublishPlan, state *nodeStageState,
) error {
	mounted, err := n.mounter.MountEntryExists(targetPath)
	if err != nil {
		return status.Errorf(codes.Internal, "NodePublishVolume: check if %q is mounted: %v", targetPath, err)
	}
	added := findFilePublishTarget(state.File.Targets, targetPath) < 0
	if mounted {
		err = n.verifyFilePublishTarget(state, targetPath, readOnly)
		if err != nil {
			return err
		}
		if added {
			state.File.Targets = append(state.File.Targets, FilePublishTarget{TargetPath: targetPath, ReadOnly: readOnly})
		}
		return n.persistFilePublish(volumeID, state)
	}

	err = n.verifyFilePublishSource(state)
	if err != nil {
		return err
	}
	if added {
		state.File.Targets = append(state.File.Targets, FilePublishTarget{TargetPath: targetPath, ReadOnly: readOnly})
	}
	// Durable intent (or metadata refresh of an already recorded target).
	err = n.persistFilePublish(volumeID, state)
	if err != nil {
		return err
	}
	err = n.mounter.Mount(plan.source, targetPath, plan.fsType, plan.options)
	if err != nil {
		mountErr := status.Errorf(codes.Internal,
			"NodePublishVolume: mount %q -> %q for volume %q: %v", plan.source, targetPath, volumeID, err)
		return n.rollbackFilePublishIntent(volumeID, state, targetPath, added, mountErr)
	}
	verifyErr := n.verifyFilePublishTarget(state, targetPath, readOnly)
	if verifyErr == nil {
		return nil
	}
	cleanupErr := n.mounter.Unmount(targetPath)
	if cleanupErr != nil {
		// The recorded intent stays so unpublish or a retry can converge.
		return status.Errorf(codes.Internal,
			"NodePublishVolume: post-mount verification failed: %v; cleanup failed: %v", verifyErr, cleanupErr)
	}
	return n.rollbackFilePublishIntent(volumeID, state, targetPath, added, verifyErr)
}

// persistFilePublish writes the publish record.
func (n *NodeServer) persistFilePublish(volumeID string, state *nodeStageState) error {
	err := n.writeStageState(volumeID, state)
	if err != nil {
		return status.Errorf(codes.Internal, "NodePublishVolume: persist publish state for %q: %v", volumeID, err)
	}
	return nil
}

// rollbackFilePublishIntent removes a target entry this call added once its
// mount is known absent, deleting the record when no target remains, and
// returns cause (joined with any rollback failure).
func (n *NodeServer) rollbackFilePublishIntent(
	volumeID string, state *nodeStageState, targetPath string, added bool, cause error,
) error {
	if !added {
		return cause
	}
	idx := findFilePublishTarget(state.File.Targets, targetPath)
	if idx >= 0 {
		state.File.Targets = slices.Delete(state.File.Targets, idx, idx+1)
	}
	var err error
	if len(state.File.Targets) == 0 {
		err = n.deleteFilePublishRecord(volumeID)
	} else {
		err = n.writeStageState(volumeID, state)
	}
	if err != nil {
		//nolint:wrapcheck // cause is a gRPC status; Join keeps its code
		return errors.Join(cause, fmt.Errorf("roll back publish intent of %q: %w", targetPath, err))
	}
	return cause
}

// verifyFilePublishSource proves, before a bind, that the controller-owned
// proxy still exposes the adopted native filesystem.  Remote NFS sources are
// verified against the mounted result instead (the export lives elsewhere).
func (n *NodeServer) verifyFilePublishSource(state *nodeStageState) error {
	if !state.File.Local {
		return nil
	}
	adoption := fileStateAdoption(state.File)
	source, err := verifyFilesystemProxyMount(state.File.ProxyPath, adoption)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: verify adopted filesystem source: %v", err)
	}
	err = n.verifyFilesystemHostSource(source, adoption)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: verify host filesystem source: %v", err)
	}
	return nil
}

// verifyFilePublishTarget proves the mount at targetPath is this volume's
// publish with the requested access mode.  A local bind is also probed for
// readability: its filesystem is on this host, so a dead one answers EIO
// rather than blocking.  A remote target is never stat'd here — a hard NFS
// mount whose server is lost would block the RPC indefinitely — so its
// readiness is the NFS mount's own success plus the mount-table identity
// (export, type, read-only flag) observed above.
func (n *NodeServer) verifyFilePublishTarget(state *nodeStageState, targetPath string, readOnly bool) error {
	err := n.verifyFileRecordedMount(state, targetPath, readOnly)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodePublishVolume: mount at %q is not this volume's publish: %v", targetPath, err)
	}
	if !state.File.Local {
		return nil
	}
	readErr := n.mounter.CheckMountReadable(targetPath)
	if readErr != nil {
		return status.Errorf(codes.Internal,
			"NodePublishVolume: publish mount %q is not usable: %v", targetPath, readErr)
	}
	return nil
}

// verifyFileRecordedMount checks the mount at targetPath against the durable
// record only — no request context — for publish and stats: a local publish
// must additionally prove by stat that it is a bind of the owned proxy
// exposing the adopted filesystem root; then the mount-table identity and
// read-only flag must match (see matchRecordedMountTable).
func (n *NodeServer) verifyFileRecordedMount(state *nodeStageState, targetPath string, readOnly bool) error {
	if state.File.Local {
		adoption := fileStateAdoption(state.File)
		source, err := verifyFilesystemProxyMount(state.File.ProxyPath, adoption)
		if err != nil {
			return err
		}
		err = n.verifyFilesystemHostSource(source, adoption)
		if err != nil {
			return err
		}
		err = verifyFilesystemBindMount(state.File.ProxyPath, resolvedMountTarget(targetPath), adoption, source)
		if err != nil {
			return err
		}
	}
	observed, err := n.mounter.ObserveMount(targetPath)
	if err != nil {
		return fmt.Errorf("observe mount %q: %w", targetPath, err)
	}
	return n.matchRecordedMountTable(state, observed, readOnly)
}

func (n *NodeServer) nodeUnpublishFilesystem(
	req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID, targetPath := req.GetVolumeId(), req.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: volume_id is required") //nolint:wrapcheck
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume: target_path is required") //nolint:wrapcheck
	}
	unlock := n.volumeLocks.lock(volumeID)
	defer unlock()

	state, err := n.readStageState(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeUnpublishVolume: read publish state for %q: %v", volumeID, err)
	}
	rejectErr := rejectStagedFileRecord("NodeUnpublishVolume", volumeID, state)
	if rejectErr != nil {
		return nil, rejectErr
	}
	mounted, err := n.mounter.MountEntryExists(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "NodeUnpublishVolume: check if %q is mounted: %v", targetPath, err)
	}
	recorded := state != nil && state.File != nil && findFilePublishTarget(state.File.Targets, targetPath) >= 0
	if !mounted && !recorded {
		// Nothing of this volume is published at targetPath.
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	if mounted {
		err = n.unmountRecordedFileTarget(volumeID, state, targetPath, recorded)
		if err != nil {
			return nil, err
		}
	}
	err = n.dropFilePublishTarget(volumeID, state, targetPath)
	if err != nil {
		return nil, err
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// unmountRecordedFileTarget unmounts a mounted targetPath only when this
// node recorded it as the volume's target and it still proves to be it: an
// unrecorded path may be the owned proxy, a peer target or a foreign mount.
// A failed unmount keeps the record so the CO's retry converges.
func (n *NodeServer) unmountRecordedFileTarget(
	volumeID string, state *nodeStageState, targetPath string, recorded bool,
) error {
	if !recorded || n.fileTargetAliasesSource(state.File, targetPath) {
		return status.Errorf(codes.FailedPrecondition,
			"NodeUnpublishVolume: %q is mounted but is not a recorded publish target of volume %q",
			targetPath, volumeID)
	}
	recordedTarget := state.File.Targets[findFilePublishTarget(state.File.Targets, targetPath)]
	err := n.verifyFileTeardownMount(state, targetPath, recordedTarget.ReadOnly)
	if err != nil {
		return err
	}
	err = n.mounter.Unmount(targetPath)
	if err != nil {
		return status.Errorf(codes.Internal, "NodeUnpublishVolume: unmount %q: %v", targetPath, err)
	}
	return nil
}

// dropFilePublishTarget removes targetPath's entry from the record, deleting
// the record durably when it was the last target.
func (n *NodeServer) dropFilePublishTarget(volumeID string, state *nodeStageState, targetPath string) error {
	idx := findFilePublishTarget(state.File.Targets, targetPath)
	state.File.Targets = slices.Delete(state.File.Targets, idx, idx+1)
	if len(state.File.Targets) > 0 {
		err := n.writeStageState(volumeID, state)
		if err != nil {
			return status.Errorf(codes.Internal, "NodeUnpublishVolume: persist publish state for %q: %v", volumeID, err)
		}
		return nil
	}
	err := n.deleteFilePublishRecord(volumeID)
	if err != nil {
		return status.Errorf(codes.Internal, "NodeUnpublishVolume: delete publish state for %q: %v", volumeID, err)
	}
	if n.sm != nil {
		n.sm.ForceState(volumeID, StateControllerPublished)
	}
	return nil
}

// verifyFileTeardownMount proves a recorded target mount is this volume's
// before it is unmounted, from mount-table facts only (see
// matchRecordedMountTable): teardown never touches the mounted filesystem,
// so a dead filesystem or a hard NFS mount whose server is gone can still
// be removed, and a foreign mount never is.
func (n *NodeServer) verifyFileTeardownMount(state *nodeStageState, targetPath string, readOnly bool) error {
	observed, err := n.mounter.ObserveMount(targetPath)
	if err != nil {
		return status.Errorf(codes.Internal, "NodeUnpublishVolume: observe mount %q: %v", targetPath, err)
	}
	err = n.matchRecordedMountTable(state, observed, readOnly)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition,
			"NodeUnpublishVolume: mount at %q is not this volume's publish: %v", targetPath, err)
	}
	return nil
}

// matchRecordedMountTable proves from mount-table facts alone that observed
// is a target of this volume with the expected read-only flag: the recorded
// NFS export (source and type), or a bind of the owned proxy's root — same
// source, type, device number and root as the proxy mount, whose type must
// be the adopted one.  A bind of another directory of the same filesystem
// differs in root and is refused.
func (n *NodeServer) matchRecordedMountTable(state *nodeStageState, observed MountObservation, readOnly bool) error {
	if observed.ReadOnly != readOnly {
		return fmt.Errorf("mount has readonly=%t, want readonly=%t", observed.ReadOnly, readOnly)
	}
	if !state.File.Local {
		if state.NFS == nil || state.NFS.MountSource == "" {
			return errors.New("remote publish record has no NFS mount source")
		}
		if (observed.FsType != ProtocolNFS && observed.FsType != "nfs4") ||
			normalizeNFSSource(observed.Source) != normalizeNFSSource(state.NFS.MountSource) {
			return fmt.Errorf("%s mount from %q is not the recorded export %q",
				observed.FsType, observed.Source, state.NFS.MountSource)
		}
		return nil
	}
	proxy, err := n.mounter.ObserveMount(state.File.ProxyPath)
	if err != nil {
		return fmt.Errorf("observe owned proxy %q: %w", state.File.ProxyPath, err)
	}
	if proxy.FsType != state.File.FilesystemType {
		return fmt.Errorf("owned proxy %q has type %q, expected %q",
			state.File.ProxyPath, proxy.FsType, state.File.FilesystemType)
	}
	if observed.Source != proxy.Source || observed.FsType != proxy.FsType ||
		observed.Major != proxy.Major || observed.Minor != proxy.Minor || observed.Root != proxy.Root {
		return fmt.Errorf("mount (%s %q %d:%d root %q) is not a bind of the owned proxy (%s %q %d:%d root %q)",
			observed.FsType, observed.Source, observed.Major, observed.Minor, observed.Root,
			proxy.FsType, proxy.Source, proxy.Major, proxy.Minor, proxy.Root)
	}
	return nil
}

// fileTargetAliasesSource reports whether targetPath names the owned proxy
// mount or the adopted host source itself: such a path is never published
// to, recorded or unmounted as a pod target.
func (n *NodeServer) fileTargetAliasesSource(state *FileStageState, targetPath string) bool {
	target := resolvedMountTarget(targetPath)
	candidates := []string{state.ProxyPath, state.HostPath}
	if n.filesystemHostRoot != "" && state.CanonicalSource != "" {
		candidates = append(candidates, filepath.Join(n.filesystemHostRoot, state.CanonicalSource))
	}
	for _, candidate := range candidates {
		if candidate != "" && resolvedMountTarget(candidate) == target {
			return true
		}
	}
	return false
}

// rejectStagedFileRecord refuses a file record written by the retired staged
// flow (it names a staging path).  Its global stage mount is outside the
// direct-publish lifecycle, so the record is preserved and reported rather
// than adopted, converted or deleted.
func rejectStagedFileRecord(rpc, volumeID string, state *nodeStageState) error {
	if state == nil || state.File == nil || state.StagingPath == "" {
		return nil
	}
	return status.Errorf(codes.FailedPrecondition,
		"%s: volume %q has a staged filesystem record (staging path %q) from the retired staged flow; "+
			"it is not managed by direct publish", rpc, volumeID, state.StagingPath)
}

// deleteFilePublishRecord removes the publish record durably: the removal
// is followed by an fsync of the state directory, like writeStageState's
// rename, so a crash cannot resurrect a record whose last target is gone.
// A missing file is still synced (a prior attempt may have removed it
// without the sync completing).
func (n *NodeServer) deleteFilePublishRecord(volumeID string) error {
	stateFile := n.stateFilePath(volumeID)
	removeErr := os.Remove(stateFile) // #nosec G703 -- path derived from controlled stateDir
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove publish record %q: %w", stateFile, removeErr)
	}
	syncErr := syncDir(n.stateDir)
	if syncErr != nil {
		return fmt.Errorf("sync state directory %q: %w", n.stateDir, syncErr)
	}
	return nil
}

// findFilePublishTarget returns the index of targetPath in targets, or -1.
func findFilePublishTarget(targets []FilePublishTarget, targetPath string) int {
	clean := filepath.Clean(targetPath)
	return slices.IndexFunc(targets, func(t FilePublishTarget) bool {
		return filepath.Clean(t.TargetPath) == clean
	})
}

// fileStateAdoption rebuilds the immutable adoption descriptor from the
// durable record, so mounts are verified without any request context.
func fileStateAdoption(state *FileStageState) *filesystemContextAdoption {
	return &filesystemContextAdoption{
		Kind: state.Kind, CanonicalSource: state.CanonicalSource, ResourceID: state.ResourceID,
		HostPath: state.HostPath, FilesystemType: state.FilesystemType, FilesystemID: state.FilesystemID,
		Inode: state.Inode, ProjectID: state.ProjectID,
	}
}

// resolvedMountTarget spells a target the way the mount table does: kubelet
// roots reached through a symlink are recorded resolved.  Only the parent is
// resolved; the mount point itself may be a dead mount.
func resolvedMountTarget(target string) string {
	clean := filepath.Clean(target)
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return clean
	}
	return filepath.Join(parent, filepath.Base(clean))
}

// normalizeNFSSource drops the brackets of a leading IPv6 host literal so
// "[fd00::1]:/x" and the kernel's "fd00::1:/x" spelling compare equal.  The
// export path is kept verbatim: brackets inside it are significant.
func normalizeNFSSource(source string) string {
	if !strings.HasPrefix(source, "[") {
		return source
	}
	end := strings.Index(source, "]:")
	if end < 0 {
		return source
	}
	return source[1:end] + source[end+1:]
}
