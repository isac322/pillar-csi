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

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/nvmeof"
)

// targetForVolume builds a minimal NvmetTarget for the given volumeID using
// only the NQN.  Used for ACL and unexport operations where the bind address
// is not carried in the request; Remove() scans configfs ports internally.
func (s *Server) targetForVolume(volumeID string) *nvmeof.NvmetTarget {
	return &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: volumeNQN(volumeID),
		NamespaceID:  1,
	}
}

// ExportVolume creates the NVMe-oF TCP target entry in configfs that exposes
// the already-created volume to the network.  The operation is idempotent.
//
// Before writing any configfs state, ExportVolume waits for the zvol block
// device to appear at the expected path (using nvmeof.WaitForDevice).  If the
// device is still absent after the retry window, the call fails with gRPC
// status FailedPrecondition so the caller can retry later.
func (s *Server) ExportVolume(
	ctx context.Context,
	req *agentv1.ExportVolumeRequest,
) (*agentv1.ExportVolumeResponse, error) {
	if req.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		return nil, status.Errorf(codes.Unimplemented, errOnlyNvmeofTCP)
	}
	params := req.GetExportParams().GetNvmeofTcp()
	if params == nil {
		return nil, status.Errorf(codes.InvalidArgument, "nvmeof_tcp export params required")
	}
	devicePath, err := s.resolveDevicePath(req)
	if err != nil {
		return nil, err
	}

	// Wait for the zvol block device to appear before touching configfs.
	// After "zfs create -V …" the kernel schedules a uevent that causes udevd
	// to create the /dev/zvol/… symlink asynchronously.  We poll here so that
	// configfs never receives an invalid (non-existent) device path.
	pollInterval := s.devicePollInterval
	if pollInterval == 0 {
		pollInterval = nvmeof.DefaultDevicePollInterval
	}
	pollTimeout := s.devicePollTimeout
	if pollTimeout == 0 {
		pollTimeout = nvmeof.DefaultDevicePollTimeout
	}
	waitErr := nvmeof.WaitForDevice(ctx, devicePath, pollInterval, pollTimeout, s.deviceChecker)
	if waitErr != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"ExportVolume: zvol device %q not ready after %s: %v",
			devicePath, pollTimeout, waitErr)
	}

	nqn := volumeNQN(req.GetVolumeId())
	t := &nvmeof.NvmetTarget{
		ConfigfsRoot: s.configfsRoot,
		SubsystemNQN: nqn,
		NamespaceID:  1,
		DevicePath:   devicePath,
		BindAddress:  params.GetBindAddress(),
		Port:         params.GetPort(),
		ACLEnabled:   req.GetAclEnabled(),
	}
	applyErr := t.Apply()
	if applyErr != nil {
		return nil, status.Errorf(codes.Internal, "ExportVolume: %v", applyErr)
	}
	port := params.GetPort()
	if port == 0 {
		port = nvmeof.DefaultPort
	}
	return &agentv1.ExportVolumeResponse{
		ExportInfo: &agentv1.ExportInfo{
			TargetId:  nqn,
			Address:   params.GetBindAddress(),
			Port:      port,
			VolumeRef: "1",
		},
	}, nil
}

// resolveDevicePath returns the device path from the request, falling back to
// the backend's DevicePath if the request field is empty.
func (s *Server) resolveDevicePath(req *agentv1.ExportVolumeRequest) (string, error) {
	if req.GetDevicePath() != "" {
		return req.GetDevicePath(), nil
	}
	b, err := s.backendFor(req.GetVolumeId())
	if err != nil {
		return "", err
	}
	return b.DevicePath(req.GetVolumeId()), nil
}

// UnexportVolume removes the NVMe-oF TCP target entry from configfs.
// The operation is idempotent.
func (s *Server) UnexportVolume(
	_ context.Context,
	req *agentv1.UnexportVolumeRequest,
) (*agentv1.UnexportVolumeResponse, error) {
	if req.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		return nil, status.Errorf(codes.Unimplemented, errOnlyNvmeofTCP)
	}
	t := s.targetForVolume(req.GetVolumeId())
	removeErr := t.Remove()
	if removeErr != nil {
		return nil, status.Errorf(codes.Internal, "UnexportVolume: %v", removeErr)
	}
	return &agentv1.UnexportVolumeResponse{}, nil
}

// AllowInitiator grants NVMe-oF TCP access to the given initiator NQN.
// The operation is idempotent.
func (s *Server) AllowInitiator(
	_ context.Context,
	req *agentv1.AllowInitiatorRequest,
) (*agentv1.AllowInitiatorResponse, error) {
	if req.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		return nil, status.Errorf(codes.Unimplemented, errOnlyNvmeofTCP)
	}
	t := s.targetForVolume(req.GetVolumeId())
	allowErr := t.AllowHost(req.GetInitiatorId())
	if allowErr != nil {
		return nil, status.Errorf(codes.Internal, "AllowInitiator: %v", allowErr)
	}
	return &agentv1.AllowInitiatorResponse{}, nil
}

// DenyInitiator revokes NVMe-oF TCP access for the given initiator NQN.
// The operation is idempotent.
func (s *Server) DenyInitiator(
	_ context.Context,
	req *agentv1.DenyInitiatorRequest,
) (*agentv1.DenyInitiatorResponse, error) {
	if req.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP {
		return nil, status.Errorf(codes.Unimplemented, errOnlyNvmeofTCP)
	}
	t := s.targetForVolume(req.GetVolumeId())
	denyErr := t.DenyHost(req.GetInitiatorId())
	if denyErr != nil {
		return nil, status.Errorf(codes.Internal, "DenyInitiator: %v", denyErr)
	}
	return &agentv1.DenyInitiatorResponse{}, nil
}
