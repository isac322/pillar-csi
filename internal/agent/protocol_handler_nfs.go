package agent

import (
	"context"
	"net/netip"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent/nfs"
)

// NFSAgentHandler fences filesystem admission changes before kernel mutation.
type NFSAgentHandler struct {
	server  *Server
	manager *nfs.Manager
}

var _ AgentProtocolHandler = (*NFSAgentHandler)(nil)

// NewNFSAgentHandler binds fenced agent operations to the owned export manager.
func NewNFSAgentHandler(server *Server, manager *nfs.Manager) *NFSAgentHandler {
	if server == nil || manager == nil {
		panic("NewNFSAgentHandler: server and manager are required")
	}
	return &NFSAgentHandler{server: server, manager: manager}
}

func (h *NFSAgentHandler) exportSpec(params ExportParams) (nfs.Export, error) {
	e, err := nfsProtocolSpec(params)
	if err != nil {
		return e, err
	}
	_, err = poolFromVolumeID(params.VolumeID)
	if err != nil {
		return e, err
	}
	e.Path, err = h.server.resolveExportDevicePath(params.VolumeID, params.DevicePath)
	return e, err
}

func nfsProtocolSpec(params ExportParams) (nfs.Export, error) {
	p := params.ProtocolParams.GetNfs()
	if p == nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return nfs.Export{}, status.Error(codes.InvalidArgument, "NFS export parameters required")
	}
	bind, err := nfsBindAddress(params)
	if err != nil {
		return nfs.Export{}, err
	}
	version := p.GetVersion()
	if version == "" {
		version = "4.2"
	}
	if version != "4.2" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return nfs.Export{}, status.Error(codes.InvalidArgument, "NFS requires version 4.2")
	}
	squash := p.GetSquash()
	if squash == "" {
		squash = "root"
	}
	switch squash {
	case "root", "none", "all":
	default:
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return nfs.Export{}, status.Error(codes.InvalidArgument, "invalid NFS squash policy")
	}
	return nfs.Export{
		VolumeID: params.VolumeID, BindAddress: bind, Version: version,
		Squash: squash, ReadOnly: p.GetReadonly(), ACLEnabled: params.ACLEnabled,
	}, nil
}

func nfsBindAddress(params ExportParams) (string, error) {
	if params.Port != 0 && params.Port != nfs.Port {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return "", status.Error(codes.InvalidArgument, "NFS requires TCP port 2049")
	}
	bindAddress := params.ProtocolParams.GetNfs().GetBindAddress()
	if params.BindAddress != "" && bindAddress != "" && params.BindAddress != bindAddress {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return "", status.Error(codes.InvalidArgument, "conflicting NFS bind addresses")
	}
	if bindAddress == "" {
		bindAddress = params.BindAddress
	}
	ip, err := netip.ParseAddr(bindAddress)
	if err != nil || ip.Zone() != "" || ip.IsUnspecified() || ip.IsMulticast() {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return "", status.Error(codes.InvalidArgument, "NFS requires numeric unicast bind address")
	}
	return ip.Unmap().String(), nil
}

func nfsStatus(op, volume string, err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Errorf(codes.Internal, "%s: NFS volume %q: %v", op, volume, err)
}

// Export publishes a filesystem without replacing previously admitted clients.
func (h *NFSAgentHandler) Export(ctx context.Context, params ExportParams) (*ExportResult, error) {
	e, err := h.exportSpec(params)
	if err != nil {
		return nil, err
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NFS, params.VolumeID)
	defer unlock()
	err = h.server.fenced(ctx, params.VolumeID, params.Fence, fenceGrant, func() error {
		return nfsStatus("ExportVolume", params.VolumeID, h.manager.Put(ctx, e, false, true))
	})
	if err != nil {
		return nil, err
	}
	path, err := h.manager.ExportPath(e.Path)
	if err != nil {
		return nil, nfsStatus("ExportVolume", params.VolumeID, err)
	}
	return &ExportResult{TargetID: path, Address: e.BindAddress, Port: nfs.Port, VolumeRef: path}, nil
}

// Unexport fences and durably withdraws the volume's owned admissions.
func (h *NFSAgentHandler) Unexport(ctx context.Context, volumeID string, fence *agentv1.FencingToken) error {
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NFS, volumeID)
	defer unlock()
	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		return nfsStatus("UnexportVolume", volumeID, h.manager.Remove(ctx, volumeID))
	})
}

// AllowInitiator fences an exact client grant.
func (h *NFSAgentHandler) AllowInitiator(
	ctx context.Context, volumeID, initiatorID string, _ *agentv1.ExportParams, fence *agentv1.FencingToken,
) error {
	err := h.manager.ValidateClient(initiatorID)
	if err != nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NFS, volumeID)
	defer unlock()
	return h.server.fenced(ctx, volumeID, fence, fenceGrant, func() error {
		return nfsStatus("AllowInitiator", volumeID, h.manager.ChangeClient(ctx, volumeID, initiatorID, true))
	})
}

// DenyInitiator fences an exact client revocation while preserving peers.
func (h *NFSAgentHandler) DenyInitiator(
	ctx context.Context, volumeID, initiatorID string, fence *agentv1.FencingToken,
) error {
	err := h.manager.ValidateClient(initiatorID)
	if err != nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped.
		return status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NFS, volumeID)
	defer unlock()
	return h.server.fenced(ctx, volumeID, fence, fenceRevoke, func() error {
		return nfsStatus("DenyInitiator", volumeID, h.manager.ChangeClient(ctx, volumeID, initiatorID, false))
	})
}

// SetLocalAttach rejects bypassing NFS admission with a storage-node bind mount.
func (*NFSAgentHandler) SetLocalAttach(context.Context, string, bool, *agentv1.FencingToken) (string, error) {
	//nolint:wrapcheck // Return the intended RPC status directly at the protocol boundary.
	return "", status.Error(codes.FailedPrecondition,
		"NFS does not support localAttach; storage-node clients must mount NFS")
}

// Reconcile applies each exact fenced admission set independently.
func (h *NFSAgentHandler) Reconcile(ctx context.Context, desired []ExportDesiredState) []error {
	specs, errs, ids := h.reconcileSpecs(desired)
	unlocks := make([]func(), 0, len(ids))
	for _, id := range ids {
		unlock := h.server.lockTarget(ctx, agentv1.ProtocolType_PROTOCOL_TYPE_NFS, id)
		unlocks = append(unlocks, unlock)
	}
	defer func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}()
	for i, d := range desired {
		if errs[i] != nil {
			continue
		}
		errs[i] = h.server.fenced(ctx, d.VolumeID, d.Fence, fenceGrant, func() error {
			return nfsStatus("Reconcile", d.VolumeID, h.manager.Put(ctx, specs[i], true, true))
		})
	}
	return errs
}

func (h *NFSAgentHandler) reconcileSpecs(desired []ExportDesiredState) ([]nfs.Export, []error, []string) {
	errs := make([]error, len(desired))
	specs := make([]nfs.Export, len(desired))
	ids := make([]string, 0, len(desired))
	seen := make(map[string]int, len(desired))
	for i, d := range desired {
		if first, exists := seen[d.VolumeID]; exists {
			errs[first] = status.Error(codes.InvalidArgument, "duplicate NFS volume in reconcile batch")
			errs[i] = errs[first]
			continue
		}
		seen[d.VolumeID] = i
		if d.LocalAttach {
			errs[i] = status.Error(codes.FailedPrecondition, "NFS does not support localAttach")
			continue
		}
		specs[i], errs[i] = h.exportSpec(ExportParams{
			VolumeID: d.VolumeID, DevicePath: d.DevicePath, BindAddress: d.BindAddress,
			Port: d.Port, ProtocolParams: d.ProtocolParams, ACLEnabled: d.ACLEnabled, Fence: d.Fence,
		})
		if errs[i] == nil {
			specs[i].Clients = slices.Clone(d.AllowedInitiators)
			ids = append(ids, d.VolumeID)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	return specs, errs, ids
}
