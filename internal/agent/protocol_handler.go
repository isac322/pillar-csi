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
	"log/slog"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// AgentProtocolHandler abstracts protocol-specific agent export operations.
//
// Every method that mutates target state receives the request's fencing
// token and MUST perform its mutation inside Server.fenced (while holding the
// protocol target lock), so that a stale controller request is rejected at
// the resource when it executes.  A protocol without an implementation must
// fail in handlerForProtocol before any state is touched.
//
//nolint:revive // RFC section 5.11 specifies the AgentProtocolHandler name.
type AgentProtocolHandler interface {
	// Export creates a network protocol target entry for a volume.
	Export(ctx context.Context, params ExportParams) (*ExportResult, error)
	// Unexport removes the protocol target entry.
	Unexport(ctx context.Context, volumeID string, fence *agentv1.FencingToken) error
	// AllowInitiator grants access to a specific initiator.  protocolParams
	// are the request's export params (iSCSI takes the ACL's CHAP
	// credentials from them); they may be nil.
	AllowInitiator(
		ctx context.Context,
		volumeID, initiatorID string,
		protocolParams *agentv1.ExportParams,
		fence *agentv1.FencingToken,
	) error
	// DenyInitiator revokes access for a specific initiator.
	DenyInitiator(ctx context.Context, volumeID, initiatorID string, fence *agentv1.FencingToken) error
	// SetLocalAttach fences the export for a direct attach on the storage
	// node (local=true, returning the backend device path) or returns it to
	// serving remote initiators (local=false), refusing the latter while the
	// backend device is held exclusively on the storage node.
	SetLocalAttach(ctx context.Context, volumeID string, local bool, fence *agentv1.FencingToken) (string, error)
	// Reconcile converges the given exports, which may belong to many
	// volumes, to their desired state and returns one error (nil on
	// success) per desired entry, in order.  It must make no export
	// reachable before every entry is prepared.
	Reconcile(ctx context.Context, desired []ExportDesiredState) []error
}

// ExportParams is the agent-local input passed to a protocol handler export.
type ExportParams struct {
	VolumeID       string
	DevicePath     string
	BindAddress    string
	Port           int32
	ProtocolParams *agentv1.ExportParams
	ACLEnabled     bool
	// Fence is the request's fencing token.
	Fence *agentv1.FencingToken
}

// ExportResult is the agent-local export result returned by protocol handlers.
type ExportResult struct {
	TargetID  string
	Address   string
	Port      int32
	VolumeRef string
}

// ExportDesiredState is the protocol handler's reconcile input for one export.
type ExportDesiredState struct {
	VolumeID          string
	DevicePath        string
	BindAddress       string
	Port              int32
	ProtocolParams    *agentv1.ExportParams
	AllowedInitiators []string
	// ACLEnabled makes AllowedInitiators the exact set of admitted hosts (an
	// empty set admits none); when false any host may connect.
	ACLEnabled bool
	// Fence is the fencing token of the volume's reconcile entry.
	Fence *agentv1.FencingToken
	// LocalAttach keeps the export fenced for a local attach on the storage
	// node: no remote initiator may do I/O through it.
	LocalAttach bool
}

// NVMeoFTCPAgentHandler wraps the nvmeof configfs package behind the generic
// AgentProtocolHandler contract.
type NVMeoFTCPAgentHandler struct {
	server *Server
}

// Ensure NVMeoFTCPAgentHandler satisfies the generic protocol contract.
var _ AgentProtocolHandler = (*NVMeoFTCPAgentHandler)(nil)

func (s *Server) handlerForProtocol(protocol agentv1.ProtocolType) (AgentProtocolHandler, error) {
	if s != nil && s.protocolHandlerResolver != nil {
		return s.protocolHandlerResolver(protocol)
	}

	switch protocol {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		return NewNVMeoFTCPAgentHandler(s), nil
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		return NewISCSIAgentHandler(s), nil
	case agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED:
		return nil, status.Errorf(codes.InvalidArgument, "handlerForProtocol: protocol_type is required")
	default:
		return nil, status.Errorf(codes.Unimplemented,
			"protocol %s is not supported by this agent", protocol.String())
	}
}

// NewNVMeoFTCPAgentHandler binds the NVMe-oF TCP handler to a Server so it can
// reuse backend lookup, device polling configuration, and per-target locking.
// Panics if server is nil — callers must provide a valid server reference.
func NewNVMeoFTCPAgentHandler(server *Server) *NVMeoFTCPAgentHandler {
	if server == nil {
		panic("NewNVMeoFTCPAgentHandler: server must not be nil")
	}
	return &NVMeoFTCPAgentHandler{server: server}
}

// Export creates the NVMe-oF TCP configfs target for a volume.
func (h *NVMeoFTCPAgentHandler) Export(
	ctx context.Context,
	params ExportParams,
) (*ExportResult, error) {
	bindAddress, port, err := nvmeofEndpoint(params.BindAddress, params.Port, params.ProtocolParams)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Export: nvmeof_tcp export params required")
	}
	inlineDataSize, maxDataTransferSize, err := nvmeofPortParams(params.VolumeID, params.ProtocolParams)
	if err != nil {
		return nil, err
	}

	devicePath, err := h.server.resolveExportDevicePath(params.VolumeID, params.DevicePath)
	if err != nil {
		return nil, err
	}
	waitErr := h.server.waitForDeviceReady(ctx, devicePath, true)
	if waitErr != nil {
		return nil, waitErr
	}

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, params.VolumeID)
	if err != nil {
		return nil, err
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot:        h.server.configfsRoot,
		SubsystemNQN:        targetID,
		NamespaceID:         1,
		DevicePath:          devicePath,
		BindAddress:         bindAddress,
		Port:                port,
		ACLEnabled:          params.ACLEnabled,
		InlineDataSize:      inlineDataSize,
		MaxDataTransferSize: maxDataTransferSize,
		MDTSUnsupported:     h.server.reportMDTSUnsupported,
		DeviceClaimer:       h.server.deviceClaimer,
	}

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, targetID)
	defer unlock()

	err = h.server.fenced(ctx, params.VolumeID, params.Fence, fenceGrant, func() error {
		identity, identityErr := h.server.resolveNVMeIdentity(params.VolumeID, params.Fence, target)
		if identityErr != nil {
			return identityErr
		}
		target.Identity = identity
		applyErr := traceNvmet(ctx, telemetry.SpanAgentNVMetApply, target, "", target.Apply)
		if errors.Is(applyErr, nvmeof.ErrPortInlineDataSizeConflict) ||
			errors.Is(applyErr, nvmeof.ErrPortMDTSConflict) ||
			errors.Is(applyErr, nvmeof.ErrDeviceHeld) {
			return status.Errorf(codes.FailedPrecondition, "ExportVolume: %v", applyErr)
		}
		if applyErr != nil {
			return status.Errorf(codes.Internal, "ExportVolume: %v", applyErr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if port == 0 {
		port = nvmeof.DefaultPort
	}

	return &ExportResult{
		TargetID:  targetID,
		Address:   bindAddress,
		Port:      port,
		VolumeRef: "1",
	}, nil
}

// Unexport removes the NVMe-oF TCP configfs target for a volume.
func (h *NVMeoFTCPAgentHandler) Unexport(ctx context.Context, volumeID string, fence *agentv1.FencingToken) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		removeErr := traceNvmet(ctx, telemetry.SpanAgentNVMetRemove, target, "", target.Remove)
		if removeErr != nil {
			return status.Errorf(codes.Internal, "UnexportVolume: %v", removeErr)
		}
		// The namespace is gone, so no host holds its identity any more; a
		// later export of this volume uses the derived identity.
		return h.server.removeIdentityRecord(volumeID)
	})
}

// AllowInitiator grants NVMe-oF TCP access to the given initiator NQN.  The
// export params carry nothing NVMe-oF needs for a grant.
func (h *NVMeoFTCPAgentHandler) AllowInitiator(
	ctx context.Context,
	volumeID, initiatorID string,
	_ *agentv1.ExportParams,
	fence *agentv1.FencingToken,
) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceGrant, func() error {
		allowErr := traceNvmet(ctx, telemetry.SpanAgentNVMetAllowHost, target, initiatorID, func() error {
			return target.AllowHost(initiatorID)
		})
		if allowErr != nil {
			return status.Errorf(codes.Internal, "AllowInitiator: %v", allowErr)
		}
		return nil
	})
}

// DenyInitiator revokes NVMe-oF TCP access for the given initiator NQN.
func (h *NVMeoFTCPAgentHandler) DenyInitiator(
	ctx context.Context,
	volumeID, initiatorID string,
	fence *agentv1.FencingToken,
) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		denyErr := traceNvmet(ctx, telemetry.SpanAgentNVMetDenyHost, target, initiatorID, func() error {
			return target.DenyHost(initiatorID)
		})
		if denyErr != nil {
			return status.Errorf(codes.Internal, "DenyInitiator: %v", denyErr)
		}
		return nil
	})
}

// SetLocalAttach disables the NVMe-oF TCP namespace for a local attach
// (local=true) and returns its backend device path, or enables it again
// (local=false) only while the backend device is not held exclusively on the
// storage node.  Both directions are idempotent and read back the written
// enable value.  A missing export is NotFound; a held device is
// FailedPrecondition, so the controller retries the remote publish.
func (h *NVMeoFTCPAgentHandler) SetLocalAttach(
	ctx context.Context,
	volumeID string,
	local bool,
	fence *agentv1.FencingToken,
) (string, error) {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return "", err
	}

	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	var devicePath string
	err = h.server.fenced(ctx, volumeID, fence, fenceGrant, func() error {
		var opErr error
		if local {
			opErr = traceNvmet(ctx, telemetry.SpanAgentNVMetDisableNS, target, "", func() error {
				var disableErr error
				devicePath, disableErr = target.DisableNamespace()
				if disableErr != nil {
					return fmt.Errorf("disable namespace of %q: %w", target.SubsystemNQN, disableErr)
				}
				return nil
			})
		} else {
			opErr = traceNvmet(ctx, telemetry.SpanAgentNVMetEnableNS, target, "", target.EnableNamespace)
		}
		return localAttachStatus(volumeID, local, opErr)
	})
	if err != nil {
		return "", err
	}
	return devicePath, nil
}

// localAttachStatus maps a namespace enable/disable error onto a gRPC status.
func localAttachStatus(volumeID string, local bool, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, nvmeof.ErrNamespaceNotFound):
		return status.Errorf(codes.NotFound, "SetLocalAttach(local=%t) volume %q: not exported: %v", local, volumeID, err)
	case errors.Is(err, nvmeof.ErrDeviceHeld):
		return status.Errorf(codes.FailedPrecondition, "SetLocalAttach(local=%t) volume %q: %v", local, volumeID, err)
	default:
		return status.Errorf(codes.Internal, "SetLocalAttach(local=%t) volume %q: %v", local, volumeID, err)
	}
}

// Reconcile converges NVMe-oF TCP exports to the desired state in two phases,
// so that a port shared by several exports starts listening only once all of
// them are ready (see nvmeof's port ordering contract):
//
//  1. prepare: for every export, inside its fenced section, wait for the
//     device, pin the identity and nvmeof.Prepare the target (subsystem, ACL,
//     namespace) without creating or linking its port;
//  2. link: create the port if needed and link every prepared export in one
//     tight loop, in request order.  The first link enables a port and
//     freezes its param_inline_data_size and param_mdts, so the caller
//     orders exports whose in-capsule data size and maximum data transfer
//     size must win first; a later export requiring another value fails with
//     nvmeof.ErrPortInlineDataSizeConflict or nvmeof.ErrPortMDTSConflict.
//
// The target locks of all entries are held across both phases.  The device
// check runs inside the fenced mutation so a destroyed backend never gets a
// configfs subsystem (a stale resync can arrive after the volume's fencing
// mark ended).  With ACL enabled the subsystem admits exactly
// AllowedInitiators — an empty set admits nobody and hosts outside the set
// are revoked.  Only the listed volumes' own subsystems are modified.
func (h *NVMeoFTCPAgentHandler) Reconcile(
	ctx context.Context,
	desired []ExportDesiredState,
) []error {
	errs := make([]error, len(desired))
	targets := make([]*nvmeof.NvmetTarget, len(desired))
	for i, export := range desired {
		targets[i], errs[i] = h.reconcileTarget(export)
		if errs[i] != nil {
			recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhasePrepare, errs[i])
		}
	}

	unlock := h.lockTargets(ctx, targets)
	defer unlock()

	prepared := make([]nvmeof.PreparedTarget, len(desired))
	for i, export := range desired {
		if errs[i] == nil {
			prepared[i], errs[i] = h.prepareExport(ctx, export, targets[i])
			if errs[i] != nil {
				recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhasePrepare, errs[i])
			}
		}
	}
	for i, export := range desired {
		if errs[i] == nil {
			errs[i] = h.server.recheckFence(ctx, export.VolumeID, export.Fence, fenceGrant, prepared[i].Link)
			if errs[i] != nil {
				recordReconcileItemFailed(ctx, export.VolumeID, reconcilePhaseLink, errs[i])
			}
		}
	}
	return errs
}

// reconcileTarget builds the configfs target of one desired export.
func (h *NVMeoFTCPAgentHandler) reconcileTarget(export ExportDesiredState) (*nvmeof.NvmetTarget, error) {
	bindAddress, port, err := nvmeofEndpoint(export.BindAddress, export.Port, export.ProtocolParams)
	if err != nil {
		return nil, fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, err)
	}

	devicePath, err := h.server.resolveExportDevicePath(export.VolumeID, export.DevicePath)
	if err != nil {
		return nil, fmt.Errorf("Reconcile: volume %q: resolve device path: %w", export.VolumeID, err)
	}

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, export.VolumeID)
	if err != nil {
		return nil, err
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot:        h.server.configfsRoot,
		SubsystemNQN:        targetID,
		NamespaceID:         1,
		DevicePath:          devicePath,
		BindAddress:         bindAddress,
		Port:                port,
		ACLEnabled:          export.ACLEnabled,
		InlineDataSize:      nvmeofInlineDataSize(export.ProtocolParams),
		MaxDataTransferSize: nvmeofMaxDataTransferSize(export.ProtocolParams),
		MDTSUnsupported:     h.server.reportMDTSUnsupported,
		LocalAttach:         export.LocalAttach,
		DeviceClaimer:       h.server.deviceClaimer,
	}
	// Without ACL enforcement allowed_hosts has no effect, and Prepare would
	// close the subsystem (attr_allow_any_host=0) for a non-empty host list.
	if export.ACLEnabled {
		target.AllowedHosts = export.AllowedInitiators
	}
	return target, nil
}

// lockTargets acquires the target locks of every non-nil target once, in
// target ID order so that concurrent reconciles cannot deadlock, and returns
// the function releasing them.  The summed wait is recorded on the RPC span
// in ctx.
func (h *NVMeoFTCPAgentHandler) lockTargets(ctx context.Context, targets []*nvmeof.NvmetTarget) func() {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		if target != nil {
			ids = append(ids, target.SubsystemNQN)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	unlocks := make([]func(), 0, len(ids))
	var wait time.Duration
	for _, id := range ids {
		unlock, waited := h.server.acquireTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, id)
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

// prepareExport runs the prepare phase of one export: every configfs change
// except the port link, inside the volume's fenced section.
func (h *NVMeoFTCPAgentHandler) prepareExport(
	ctx context.Context,
	export ExportDesiredState,
	target *nvmeof.NvmetTarget,
) (nvmeof.PreparedTarget, error) {
	var prepared nvmeof.PreparedTarget
	err := h.server.fenced(ctx, export.VolumeID, export.Fence, fenceGrant, func() error {
		waitErr := h.server.waitForDeviceReady(ctx, target.DevicePath, false)
		if waitErr != nil {
			return fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, waitErr)
		}
		identity, identityErr := h.server.resolveNVMeIdentity(export.VolumeID, export.Fence, target)
		if identityErr != nil {
			return fmt.Errorf("applyExport %q: %w", export.VolumeID, identityErr)
		}
		target.Identity = identity
		var prepareErr error
		prepared, prepareErr = target.Prepare()
		if prepareErr != nil {
			return fmt.Errorf("applyExport %q: %w", export.VolumeID, prepareErr)
		}
		if export.ACLEnabled {
			revokeErr := target.RevokeHostsExcept(export.AllowedInitiators)
			if revokeErr != nil {
				return fmt.Errorf("applyExport %q: %w", export.VolumeID, revokeErr)
			}
		}
		return nil
	})
	return prepared, err
}

func (h *NVMeoFTCPAgentHandler) targetForVolume(volumeID string) (*nvmeof.NvmetTarget, error) {
	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if err != nil {
		return nil, err
	}

	return &nvmeof.NvmetTarget{
		ConfigfsRoot:  h.server.configfsRoot,
		SubsystemNQN:  targetID,
		NamespaceID:   1,
		DeviceClaimer: h.server.deviceClaimer,
	}, nil
}

// waitForDeviceReady waits for the backend block device of an export to
// appear before target configuration.  Against a test configfs root without
// an injected DeviceChecker it does not wait.  When traced, the wait runs
// inside a pillar_csi.agent.device_wait span (ExportVolume); the
// ReconcileState prepare loops pass false and report failures as span
// events instead.
func (s *Server) waitForDeviceReady(ctx context.Context, devicePath string, traced bool) error {
	realConfigfs := s.configfsRoot == "" || s.configfsRoot == nvmeof.DefaultConfigfsRoot
	if !realConfigfs && s.deviceChecker == nil {
		return nil
	}

	pollInterval := s.devicePollInterval
	if pollInterval == 0 {
		pollInterval = nvmeof.DefaultDevicePollInterval
	}
	pollTimeout := s.devicePollTimeout
	if pollTimeout == 0 {
		pollTimeout = nvmeof.DefaultDevicePollTimeout
	}

	var span trace.Span
	if traced {
		span = startChildSpan(ctx, telemetry.SpanAgentDeviceWait,
			telemetry.KeyDevicePath.String(devicePath),
			telemetry.KeyWaitTimeout.Float64(pollTimeout.Seconds()))
	}
	waitErr := nvmeof.WaitForDevice(ctx, devicePath, pollInterval, pollTimeout, s.deviceChecker)
	if span != nil {
		endDeviceWaitSpan(span, waitErr)
	}
	if waitErr != nil {
		return status.Errorf(
			codes.FailedPrecondition,
			"ExportVolume: zvol device %q not ready after %s: %v",
			devicePath,
			pollTimeout,
			waitErr,
		)
	}
	return nil
}

func (s *Server) resolveExportDevicePath(volumeID, devicePath string) (string, error) {
	if devicePath != "" {
		return devicePath, nil
	}

	b, err := s.backendFor(volumeID)
	if err != nil {
		return "", err
	}
	return b.DevicePath(volumeID), nil
}

// nvmeofInlineDataSize returns the in-capsule data size the export requires
// on its port, or nil when it sets none (in_capsule_data_size 0).
func nvmeofInlineDataSize(protocolParams *agentv1.ExportParams) *int32 {
	if v := protocolParams.GetNvmeofTcp().GetInCapsuleDataSize(); v != 0 {
		return &v
	}
	return nil
}

// nvmeofMaxDataTransferSize returns the maximum data transfer size the
// export requires its port to advertise, or nil when the caller predates
// max_data_transfer_size and accepts the port's value.  0 is a request for
// no limit, not an absent value.
func nvmeofMaxDataTransferSize(protocolParams *agentv1.ExportParams) *int32 {
	v := protocolParams.GetNvmeofTcp().MaxDataTransferSize
	if v == nil {
		return nil
	}
	return new(*v)
}

// nvmeofPortParams returns the port parameters an export requires, rejecting
// invalid values with InvalidArgument before any configfs change.
func nvmeofPortParams(
	volumeID string,
	protocolParams *agentv1.ExportParams,
) (inlineDataSize, maxDataTransferSize *int32, err error) {
	inlineDataSize = nvmeofInlineDataSize(protocolParams)
	if inlineDataSize != nil && *inlineDataSize < nvmeof.MinInlineDataSize {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"Export: volume %q: in_capsule_data_size %d is below the minimum %d",
			volumeID, *inlineDataSize, nvmeof.MinInlineDataSize)
	}
	maxDataTransferSize = nvmeofMaxDataTransferSize(protocolParams)
	if maxDataTransferSize != nil && !nvmeof.ValidMaxDataTransferSize(*maxDataTransferSize) {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"Export: volume %q: max_data_transfer_size %d must be 0 (no limit) or a power of two from %d to %d",
			volumeID, *maxDataTransferSize, nvmeof.MinMaxDataTransferSize, nvmeof.MaxMaxDataTransferSize)
	}
	return inlineDataSize, maxDataTransferSize, nil
}

// reportMDTSUnsupported is the nvmeof.NvmetTarget.MDTSUnsupported callback.
// It warns once per agent process, since a missing param_mdts is a fact
// about the kernel and every NVMe/TCP export would repeat it.
func (s *Server) reportMDTSUnsupported(port string) {
	s.mdtsUnsupportedOnce.Do(func() {
		slog.Warn("nvmet target cannot advertise a maximum data transfer size: the kernel has no "+
			"ports/<id>/param_mdts (Linux < 7.1), so NVMe/TCP hosts see MDTS 0 and pillar-csi nodes "+
			"cap queue/max_sectors_kb to the volume's maxDataTransferSize themselves",
			"port", port)
	})
}

func nvmeofEndpoint(
	bindAddress string,
	port int32,
	protocolParams *agentv1.ExportParams,
) (resolvedBindAddress string, resolvedPort int32, err error) {
	if nvmeParams := protocolParams.GetNvmeofTcp(); nvmeParams != nil {
		return nvmeParams.GetBindAddress(), nvmeParams.GetPort(), nil
	}
	if bindAddress == "" && port == 0 {
		return "", 0, status.Errorf(codes.InvalidArgument, "nvmeofEndpoint: nvmeof_tcp export params required")
	}
	return bindAddress, port, nil
}
