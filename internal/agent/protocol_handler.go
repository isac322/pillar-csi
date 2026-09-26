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
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
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
	// AllowInitiator grants access to a specific initiator.
	AllowInitiator(ctx context.Context, volumeID, initiatorID string, fence *agentv1.FencingToken) error
	// DenyInitiator revokes access for a specific initiator.
	DenyInitiator(ctx context.Context, volumeID, initiatorID string, fence *agentv1.FencingToken) error
	// Reconcile re-creates protocol state after reboot.
	Reconcile(ctx context.Context, desired []ExportDesiredState) error
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

	devicePath, err := h.server.resolveExportDevicePath(params.VolumeID, params.DevicePath)
	if err != nil {
		return nil, err
	}
	waitErr := h.waitForDeviceReady(ctx, devicePath)
	if waitErr != nil {
		return nil, waitErr
	}

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, params.VolumeID)
	if err != nil {
		return nil, err
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot: h.server.configfsRoot,
		SubsystemNQN: targetID,
		NamespaceID:  1,
		DevicePath:   devicePath,
		BindAddress:  bindAddress,
		Port:         port,
		ACLEnabled:   params.ACLEnabled,
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, targetID)
	defer unlock()

	err = h.server.fenced(params.VolumeID, params.Fence, fenceGrant, func() error {
		identity, identityErr := h.server.resolveNVMeIdentity(params.VolumeID, params.Fence, target)
		if identityErr != nil {
			return identityErr
		}
		target.Identity = identity
		applyErr := target.Apply()
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
func (h *NVMeoFTCPAgentHandler) Unexport(_ context.Context, volumeID string, fence *agentv1.FencingToken) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(volumeID, fence, fenceRevoke, func() error {
		removeErr := target.Remove()
		if removeErr != nil {
			return status.Errorf(codes.Internal, "UnexportVolume: %v", removeErr)
		}
		// The namespace is gone, so no host holds its identity any more; a
		// later export of this volume uses the derived identity.
		return h.server.removeIdentityRecord(volumeID)
	})
}

// AllowInitiator grants NVMe-oF TCP access to the given initiator NQN.
func (h *NVMeoFTCPAgentHandler) AllowInitiator(
	_ context.Context,
	volumeID, initiatorID string,
	fence *agentv1.FencingToken,
) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(volumeID, fence, fenceGrant, func() error {
		allowErr := target.AllowHost(initiatorID)
		if allowErr != nil {
			return status.Errorf(codes.Internal, "AllowInitiator: %v", allowErr)
		}
		return nil
	})
}

// DenyInitiator revokes NVMe-oF TCP access for the given initiator NQN.
func (h *NVMeoFTCPAgentHandler) DenyInitiator(
	_ context.Context,
	volumeID, initiatorID string,
	fence *agentv1.FencingToken,
) error {
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, target.SubsystemNQN)
	defer unlock()

	return h.server.fenced(volumeID, fence, fenceRevoke, func() error {
		denyErr := target.DenyHost(initiatorID)
		if denyErr != nil {
			return status.Errorf(codes.Internal, "DenyInitiator: %v", denyErr)
		}
		return nil
	})
}

// Reconcile converges NVMe-oF TCP exports to the desired state.  The device
// check runs inside the fenced mutation so a destroyed backend never gets a
// configfs subsystem (a stale resync can arrive after the volume's fencing
// mark ended).  With ACL enabled the subsystem admits exactly
// AllowedInitiators — an empty set admits nobody and hosts outside the set
// are revoked.  Only the volume's own subsystem is modified.
func (h *NVMeoFTCPAgentHandler) Reconcile(
	ctx context.Context,
	desired []ExportDesiredState,
) error {
	for _, export := range desired {
		err := h.reconcileExport(ctx, export)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *NVMeoFTCPAgentHandler) reconcileExport(ctx context.Context, export ExportDesiredState) error {
	bindAddress, port, err := nvmeofEndpoint(export.BindAddress, export.Port, export.ProtocolParams)
	if err != nil {
		return fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, err)
	}

	devicePath, err := h.server.resolveExportDevicePath(export.VolumeID, export.DevicePath)
	if err != nil {
		return fmt.Errorf("Reconcile: volume %q: resolve device path: %w", export.VolumeID, err)
	}

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, export.VolumeID)
	if err != nil {
		return err
	}
	target := &nvmeof.NvmetTarget{
		ConfigfsRoot: h.server.configfsRoot,
		SubsystemNQN: targetID,
		NamespaceID:  1,
		DevicePath:   devicePath,
		BindAddress:  bindAddress,
		Port:         port,
		ACLEnabled:   export.ACLEnabled,
	}
	// Without ACL enforcement allowed_hosts has no effect, and Apply would
	// close the subsystem (attr_allow_any_host=0) for a non-empty host list.
	if export.ACLEnabled {
		target.AllowedHosts = export.AllowedInitiators
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, targetID)
	defer unlock()

	// The device check and the configfs mutation run inside the same fenced
	// critical section: the fencing mark is persisted first, then the device
	// must exist before any subsystem is written.
	reconcileErr := h.server.fenced(export.VolumeID, export.Fence, fenceGrant, func() error {
		waitErr := h.waitForDeviceReady(ctx, devicePath)
		if waitErr != nil {
			return fmt.Errorf("Reconcile: volume %q: %w", export.VolumeID, waitErr)
		}
		identity, identityErr := h.server.resolveNVMeIdentity(export.VolumeID, export.Fence, target)
		if identityErr != nil {
			return fmt.Errorf("applyExport %q: %w", export.VolumeID, identityErr)
		}
		target.Identity = identity
		applyErr := target.Apply()
		if applyErr != nil {
			return fmt.Errorf("applyExport %q: %w", export.VolumeID, applyErr)
		}
		if export.ACLEnabled {
			revokeErr := target.RevokeHostsExcept(export.AllowedInitiators)
			if revokeErr != nil {
				return fmt.Errorf("applyExport %q: %w", export.VolumeID, revokeErr)
			}
		}
		return nil
	})
	if reconcileErr != nil {
		return reconcileErr
	}
	return nil
}

func (h *NVMeoFTCPAgentHandler) targetForVolume(volumeID string) (*nvmeof.NvmetTarget, error) {
	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if err != nil {
		return nil, err
	}

	return &nvmeof.NvmetTarget{
		ConfigfsRoot: h.server.configfsRoot,
		SubsystemNQN: targetID,
		NamespaceID:  1,
	}, nil
}

func (h *NVMeoFTCPAgentHandler) waitForDeviceReady(ctx context.Context, devicePath string) error {
	realConfigfs := h.server.configfsRoot == "" || h.server.configfsRoot == nvmeof.DefaultConfigfsRoot
	if !realConfigfs && h.server.deviceChecker == nil {
		return nil
	}

	pollInterval := h.server.devicePollInterval
	if pollInterval == 0 {
		pollInterval = nvmeof.DefaultDevicePollInterval
	}
	pollTimeout := h.server.devicePollTimeout
	if pollTimeout == 0 {
		pollTimeout = nvmeof.DefaultDevicePollTimeout
	}

	waitErr := nvmeof.WaitForDevice(ctx, devicePath, pollInterval, pollTimeout, h.server.deviceChecker)
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
