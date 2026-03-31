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
//nolint:revive // RFC section 5.11 specifies the AgentProtocolHandler name.
type AgentProtocolHandler interface {
	// Export creates a network protocol target entry for a volume.
	Export(ctx context.Context, params ExportParams) (*ExportResult, error)
	// Unexport removes the protocol target entry.
	Unexport(ctx context.Context, volumeID string) error
	// AllowInitiator grants access to a specific initiator.
	AllowInitiator(ctx context.Context, volumeID, initiatorID string) error
	// DenyInitiator revokes access for a specific initiator.
	DenyInitiator(ctx context.Context, volumeID, initiatorID string) error
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
func NewNVMeoFTCPAgentHandler(server *Server) *NVMeoFTCPAgentHandler {
	return &NVMeoFTCPAgentHandler{server: server}
}

// Export creates the NVMe-oF TCP configfs target for a volume.
func (h *NVMeoFTCPAgentHandler) Export(
	ctx context.Context,
	params ExportParams,
) (*ExportResult, error) {
	if h.server == nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"Export: NVMeoFTCPAgentHandler requires a server")
	}

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

	applyErr := target.Apply()
	if applyErr != nil {
		return nil, status.Errorf(codes.Internal, "ExportVolume: %v", applyErr)
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
func (h *NVMeoFTCPAgentHandler) Unexport(_ context.Context, volumeID string) error {
	if h.server == nil {
		return status.Errorf(codes.FailedPrecondition,
			"Unexport: NVMeoFTCPAgentHandler requires a server")
	}

	targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, volumeID)
	if err != nil {
		return err
	}
	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, targetID)
	defer unlock()

	removeErr := target.Remove()
	if removeErr != nil {
		return status.Errorf(codes.Internal, "UnexportVolume: %v", removeErr)
	}
	return nil
}

// AllowInitiator grants NVMe-oF TCP access to the given initiator NQN.
func (h *NVMeoFTCPAgentHandler) AllowInitiator(
	_ context.Context,
	volumeID, initiatorID string,
) error {
	if h.server == nil {
		return status.Errorf(codes.FailedPrecondition,
			"AllowInitiator: NVMeoFTCPAgentHandler requires a server")
	}

	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	allowErr := target.AllowHost(initiatorID)
	if allowErr != nil {
		return status.Errorf(codes.Internal, "AllowInitiator: %v", allowErr)
	}
	return nil
}

// DenyInitiator revokes NVMe-oF TCP access for the given initiator NQN.
func (h *NVMeoFTCPAgentHandler) DenyInitiator(
	_ context.Context,
	volumeID, initiatorID string,
) error {
	if h.server == nil {
		return status.Errorf(codes.FailedPrecondition,
			"DenyInitiator: NVMeoFTCPAgentHandler requires a server")
	}

	target, err := h.targetForVolume(volumeID)
	if err != nil {
		return err
	}

	denyErr := target.DenyHost(initiatorID)
	if denyErr != nil {
		return status.Errorf(codes.Internal, "DenyInitiator: %v", denyErr)
	}
	return nil
}

// Reconcile re-applies desired NVMe-oF TCP exports after reboot.
func (h *NVMeoFTCPAgentHandler) Reconcile(
	_ context.Context,
	desired []ExportDesiredState,
) error {
	if h.server == nil {
		return status.Errorf(codes.FailedPrecondition,
			"Reconcile: NVMeoFTCPAgentHandler requires a server")
	}

	for _, export := range desired {
		bindAddress, port, err := nvmeofEndpoint(export.BindAddress, export.Port, export.ProtocolParams)
		if err != nil {
			continue
		}

		targetID, err := volumeTargetID(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, export.VolumeID)
		if err != nil {
			return err
		}
		target := &nvmeof.NvmetTarget{
			ConfigfsRoot: h.server.configfsRoot,
			SubsystemNQN: targetID,
			NamespaceID:  1,
			DevicePath:   export.DevicePath,
			BindAddress:  bindAddress,
			Port:         port,
			AllowedHosts: export.AllowedInitiators,
		}

		unlock := h.server.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, targetID)
		applyErr := target.Apply()
		unlock()
		if applyErr != nil {
			return fmt.Errorf("applyExport %q: %w", export.VolumeID, applyErr)
		}
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
