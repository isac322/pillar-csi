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

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/lio"
)

// iscsiVolumeRef is the LUN every iSCSI export presents its volume as.
const iscsiVolumeRef = "0"

// ISCSIAgentHandler wraps the lio configfs package behind the generic
// AgentProtocolHandler contract.  Each volume is one LIO target named by
// volumeTargetID with TPG 1, LUN 0 on an iblock backstore of the volume's
// block device, and one network portal.
type ISCSIAgentHandler struct {
	server *Server
}

// Ensure ISCSIAgentHandler satisfies the generic protocol contract.
var _ AgentProtocolHandler = (*ISCSIAgentHandler)(nil)

// NewISCSIAgentHandler binds the iSCSI handler to a Server so it can reuse
// backend lookup, device polling configuration, fencing and per-target
// locking.  Panics if server is nil.
func NewISCSIAgentHandler(server *Server) *ISCSIAgentHandler {
	if server == nil {
		panic("NewISCSIAgentHandler: server must not be nil")
	}
	return &ISCSIAgentHandler{server: server}
}

// Export creates (or converges) the LIO target of a volume: backstore, LUN,
// TPG policy, portal and TPG enable.  An export in the local-attach state is
// returned to serving remote initiators, which fails with FailedPrecondition
// while the device is still held on the storage node.
func (h *ISCSIAgentHandler) Export(ctx context.Context, params ExportParams) (*ExportResult, error) {
	bindAddress, port, err := iscsiEndpoint(params.ProtocolParams)
	if err != nil {
		return nil, err
	}
	devicePath, err := h.server.resolveExportDevicePath(params.VolumeID, params.DevicePath)
	if err != nil {
		return nil, err
	}
	err = h.server.waitForDeviceReady(ctx, devicePath, true)
	if err != nil {
		return nil, err
	}
	target, err := h.targetForVolume(params.VolumeID)
	if err != nil {
		return nil, err
	}
	target.DevicePath = devicePath
	target.BindAddress = bindAddress
	target.Port = port
	target.ACLEnabled = params.ACLEnabled

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, target.IQN)
	defer unlock()

	err = h.server.fenced(ctx, params.VolumeID, params.Fence, fenceGrant, func() error {
		return iscsiStatus("ExportVolume", params.VolumeID, target.Apply())
	})
	if err != nil {
		return nil, err
	}
	return &ExportResult{
		TargetID:  target.IQN,
		Address:   bindAddress,
		Port:      port,
		VolumeRef: iscsiVolumeRef,
	}, nil
}

// Unexport removes the LIO target and backstore of a volume.
func (h *ISCSIAgentHandler) Unexport(ctx context.Context, volumeID string, fence *agentv1.FencingToken) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, target.IQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		return iscsiStatus("UnexportVolume", volumeID, target.Remove())
	})
}

// AllowInitiator creates the node ACL of the initiator IQN (with LUN 0
// mapped while the export is not locally attached).
func (h *ISCSIAgentHandler) AllowInitiator(
	ctx context.Context,
	volumeID, initiatorID string,
	fence *agentv1.FencingToken,
) error {
	err := lio.ValidateIQN(initiatorID)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "AllowInitiator: volume %q: initiator: %v", volumeID, err)
	}
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, target.IQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceGrant, func() error {
		return iscsiStatus("AllowInitiator", volumeID, target.AllowInitiator(initiatorID))
	})
}

// DenyInitiator removes the node ACL of the initiator IQN; LIO terminates
// the initiator's sessions with it.
func (h *ISCSIAgentHandler) DenyInitiator(
	ctx context.Context,
	volumeID, initiatorID string,
	fence *agentv1.FencingToken,
) error {
	err := lio.ValidateIQN(initiatorID)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "DenyInitiator: volume %q: initiator: %v", volumeID, err)
	}
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, target.IQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		return iscsiStatus("DenyInitiator", volumeID, target.DenyInitiator(initiatorID))
	})
}

// SetLocalAttach fences the iSCSI export for a direct attach on the storage
// node (local=true) and returns the backend device path, or returns it to
// serving remote initiators (local=false, returning "").
//
// LIO's iblock backstore claims its device exclusively for as long as it
// exists, so local=true disables the TPG (terminating every session),
// removes the mapped LUNs, LUN 0 (tpgt_1/lun/lun_0 no longer exists) and the
// backstore, and verifies with an exclusive claim probe that the device is
// free.  Target, TPG, portal and ACLs are kept, so local=false only
// re-creates backstore, LUN 0 and mapped LUNs and re-enables the TPG; the
// backstore enable is LIO's own exclusive open and fails while the node
// still holds the device.  Both directions are idempotent.  A missing export
// is NotFound; a held device is FailedPrecondition, so the controller
// retries the remote publish.
func (h *ISCSIAgentHandler) SetLocalAttach(
	ctx context.Context,
	volumeID string,
	local bool,
	fence *agentv1.FencingToken,
) (string, error) {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return "", err
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, target.IQN)
	defer unlock()

	var devicePath string
	op := fmt.Sprintf("SetLocalAttach(local=%t)", local)
	err = h.server.fenced(ctx, volumeID, fence, fenceGrant, func() error {
		if local {
			var opErr error
			devicePath, opErr = target.EnterLocalAttach()
			if opErr != nil {
				return iscsiStatus(op, volumeID, opErr)
			}
			if devicePath == "" {
				// Already locally attached: the backstore that recorded the
				// device is gone, so the backend names it.
				devicePath, opErr = h.server.resolveExportDevicePath(volumeID, "")
			}
			return opErr
		}
		dev, resolveErr := h.server.resolveExportDevicePath(volumeID, "")
		if resolveErr != nil {
			return resolveErr
		}
		target.DevicePath = dev
		return iscsiStatus(op, volumeID, target.LeaveLocalAttach())
	})
	if err != nil {
		return "", err
	}
	return devicePath, nil
}

// Reconcile converges iSCSI exports to the desired state in two phases, so
// that no target becomes reachable before every export is configured (the
// portal socket is shared by all targets on the same address and port):
//
//  1. prepare: for every export, inside its fenced section, wait for the
//     device (unless locally attached) and lio.Target.Prepare it (fabric,
//     target, TPG, backstore and LUN or the local-attach state, TPG policy,
//     ACLs) without portal or TPG enable;
//  2. activate: create the portal and enable the TPG of every prepared
//     export, in request order, each under a fencing recheck.
//
// Each failed export is recorded as a pillar_csi.reconcile.item_failed
// event on the span in ctx: the activate phase reports as "link", the
// phase that makes an export reachable.
//
// The target locks of all entries are held across both phases.  With ACL
// enabled the TPG admits exactly AllowedInitiators: missing node ACLs are
// created and others removed.  Only the listed volumes' own targets are
// modified; targets of unlisted volumes are left as is.
func (h *ISCSIAgentHandler) Reconcile(ctx context.Context, desired []ExportDesiredState) []error {
	errs := make([]error, len(desired))
	targets := make([]*lio.Target, len(desired))
	for i, export := range desired {
		targets[i], errs[i] = h.reconcileTarget(export)
		if errs[i] != nil {
			recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhasePrepare, errs[i])
		}
	}

	unlock := h.lockTargets(ctx, targets)
	defer unlock()

	for i, export := range desired {
		if errs[i] != nil {
			continue
		}
		target := targets[i]
		errs[i] = h.server.fenced(ctx, export.VolumeID, export.Fence, fenceGrant, func() error {
			return h.prepareTarget(ctx, export.VolumeID, target)
		})
		if errs[i] != nil {
			recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhasePrepare, errs[i])
		}
	}
	for i, export := range desired {
		if errs[i] != nil {
			continue
		}
		target := targets[i]
		errs[i] = h.server.recheckFence(ctx, export.VolumeID, export.Fence, fenceGrant, func() error {
			activateErr := target.Activate()
			if activateErr != nil {
				return fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, activateErr)
			}
			return nil
		})
		if errs[i] != nil {
			recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhaseLink, errs[i])
		}
	}
	return errs
}

// prepareTarget runs the prepare phase of Reconcile for one volume's target:
// wait for the device (unless locally attached), prepare the target and,
// with ACL enabled, revoke initiators outside AllowedInitiators.
func (h *ISCSIAgentHandler) prepareTarget(ctx context.Context, volumeID string, target *lio.Target) error {
	if !target.LocalAttach {
		waitErr := h.server.waitForDeviceReady(ctx, target.DevicePath, false)
		if waitErr != nil {
			return fmt.Errorf("Reconcile: volume %q: %w", volumeID, waitErr)
		}
	}
	prepareErr := target.Prepare()
	if prepareErr == nil && target.ACLEnabled {
		prepareErr = target.RevokeInitiatorsExcept(target.AllowedInitiators)
	}
	if prepareErr != nil {
		return fmt.Errorf("Reconcile: volume %q: %w", volumeID, prepareErr)
	}
	return nil
}

// reconcileTarget builds the LIO target of one desired export.
func (h *ISCSIAgentHandler) reconcileTarget(export ExportDesiredState) (*lio.Target, error) {
	bindAddress, port, err := iscsiEndpoint(export.ProtocolParams)
	if err != nil {
		return nil, fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, err)
	}
	devicePath, err := h.server.resolveExportDevicePath(export.VolumeID, export.DevicePath)
	if err != nil {
		return nil, fmt.Errorf("Reconcile: volume %q: resolve device path: %w", export.VolumeID, err)
	}
	target, err := h.targetForVolume(export.VolumeID)
	if err != nil {
		return nil, err
	}
	target.DevicePath = devicePath
	target.BindAddress = bindAddress
	target.Port = port
	target.ACLEnabled = export.ACLEnabled
	target.LocalAttach = export.LocalAttach
	// Without ACL enforcement node ACLs have no effect on admission.
	if export.ACLEnabled {
		target.AllowedInitiators = export.AllowedInitiators
	}
	return target, nil
}

// lockTargets acquires the target locks of every non-nil target once, in
// IQN order so that concurrent reconciles cannot deadlock, and returns the
// function releasing them.  The summed wait is recorded on the RPC span in
// ctx.
func (h *ISCSIAgentHandler) lockTargets(ctx context.Context, targets []*lio.Target) func() {
	iqns := make([]string, 0, len(targets))
	for _, target := range targets {
		if target != nil {
			iqns = append(iqns, target.IQN)
		}
	}
	slices.Sort(iqns)
	iqns = slices.Compact(iqns)
	unlocks := make([]func(), 0, len(iqns))
	var wait time.Duration
	for _, iqn := range iqns {
		unlock, waited := h.server.acquireTarget(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, iqn)
		unlocks = append(unlocks, unlock)
		wait += waited
	}
	recordLockWait(ctx, wait)
	return func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}
}

func (h *ISCSIAgentHandler) targetForVolume(volumeID string) (*lio.Target, error) {
	_, err := poolFromVolumeID(volumeID)
	if err != nil {
		return nil, err
	}
	iqn, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI, volumeID)
	if err != nil {
		return nil, err
	}
	return &lio.Target{
		ConfigfsRoot:  h.server.configfsRoot,
		FS:            h.server.lioFS,
		IQN:           iqn,
		DeviceClaimer: h.server.deviceClaimer,
	}, nil
}

// iscsiEndpoint returns the advertised portal address and port of an iSCSI
// export; port 0 selects lio.DefaultPort.
func iscsiEndpoint(protocolParams *agentv1.ExportParams) (bindAddress string, port int32, err error) {
	params := protocolParams.GetIscsi()
	if params == nil {
		return "", 0, status.Errorf(codes.InvalidArgument, "iscsiEndpoint: iscsi export params required")
	}
	bindAddress = params.GetBindAddress()
	if net.ParseIP(bindAddress) == nil {
		return "", 0, status.Errorf(codes.InvalidArgument,
			"iscsiEndpoint: bind_address %q is not an IP address", bindAddress)
	}
	port = params.GetPort()
	if port == 0 {
		port = lio.DefaultPort
	}
	if port < 1 || port > 65535 {
		return "", 0, status.Errorf(codes.InvalidArgument, "iscsiEndpoint: port %d is out of range 1-65535", port)
	}
	return bindAddress, port, nil
}

// iscsiStatus maps a lio error onto a gRPC status: a missing target is
// NotFound, a held backend device FailedPrecondition, anything else Internal.
func iscsiStatus(op, volumeID string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lio.ErrTargetNotFound):
		return status.Errorf(codes.NotFound, "%s: volume %q is not exported: %v", op, volumeID, err)
	case errors.Is(err, lio.ErrDeviceHeld):
		return status.Errorf(codes.FailedPrecondition, "%s: volume %q: %v", op, volumeID, err)
	default:
		return status.Errorf(codes.Internal, "%s: volume %q: %v", op, volumeID, err)
	}
}
