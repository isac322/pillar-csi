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

// Package csi implements the Container Storage Interface (CSI) Controller and
// Node services for pillar-csi.
//
// Architecture overview:
//
//	CSI Controller (this package)
//	  └─► pillar-agent (gRPC) on each storage node
//	        ├─ CreateVolume / DeleteVolume / ExpandVolume
//	        ├─ ExportVolume / UnexportVolume
//	        └─ AllowInitiator / DenyInitiator
//
// Routing: each CSI request carries a volumeID that encodes the
// PillarBinding name; the controller uses the Kubernetes client to look up the
// corresponding PillarPool and PillarTarget, resolves the agent address from
// PillarTarget.Status.ResolvedAddress, and dials the agent via AgentDialer.
package csi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ─────────────────────────────────────────────────────────────────────────────
// PVC annotation validation error sentinel
// ─────────────────────────────────────────────────────────────────────────────.

// pvcAnnotationValidationError wraps a ParsePVCAnnotations error so that
// CreateVolume can distinguish annotation validation failures (InvalidArgument)
// from infrastructure errors (Internal).
type pvcAnnotationValidationError struct {
	pvcNamespace string
	pvcName      string
	cause        error
}

func (e *pvcAnnotationValidationError) Error() string {
	return fmt.Sprintf("PVC %s/%s annotation validation failed: %v",
		e.pvcNamespace, e.pvcName, e.cause)
}

func (e *pvcAnnotationValidationError) Unwrap() error { return e.cause }

// ─────────────────────────────────────────────────────────────────────────────

// AgentDialer creates a gRPC client connection to the pillar-agent at addr and
// returns the typed client along with an io.Closer to release the connection.
//
// # Trust boundary
//
// TODO(Phase 2): replace insecure.NewCredentials() with mTLS once the PKI
// infrastructure is in place.  Both the controller and the agent must present
// certificates signed by a shared CA.  Until then the controller-to-agent
// channel is unencrypted and unauthenticated; it MUST be deployed on an
// isolated storage management network that is not reachable from the workload
// VLAN.
type AgentDialer func(ctx context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error)

// DefaultAgentDialer is the production AgentDialer.  It opens a plain-text
// gRPC connection to addr.  MTLS is tracked as a Phase 2 item; see the
// AgentDialer doc comment for the trust boundary note.
func DefaultAgentDialer(_ context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
	// grpc.NewClient (not grpc.Dial) is the non-deprecated entry point as of
	// gRPC-Go v1.64.  The connection is lazy; the first RPC attempt triggers
	// the actual TCP handshake.
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent at %q: %w", addr, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn, nil
}

// ControllerServer implements the CSI Controller service
// (csi.ControllerServer).  It translates CSI RPC calls into sequences of
// pillar-agent RPCs routed to the appropriate storage node.
//
// Field invariants:
//   - k8sClient is never nil after construction.
//   - dialAgent is never nil after construction.
//   - driverName is the CSI provisioner name registered in the cluster
//     (pillar-csi.bhyoo.com).
//   - sm is never nil after construction; it tracks the in-memory lifecycle
//     state of every volume managed by this controller instance.
type ControllerServer struct {
	csi.UnimplementedControllerServer

	// k8sClient is used to look up PillarTarget, PillarPool, and PillarBinding
	// resources to route volume operations to the correct storage node.
	// It is also used to create/update PillarVolume CRDs for durable
	// partial-failure state tracking.
	k8sClient client.Client

	// dialAgent creates an AgentServiceClient for the given agent address.
	// Injected at construction time; tests may supply a mock dialer.
	dialAgent AgentDialer

	// driverName is the CSI driver name announced in GetPluginInfo and
	// embedded in PersistentVolume.spec.csi.driver.
	driverName string

	// sm tracks the in-memory lifecycle state of every volume managed by
	// this controller instance.  It is initialized from persisted PillarVolume
	// CRDs at startup (via LoadStateFromPillarVolumes) and updated at each
	// lifecycle step.
	sm *VolumeStateMachine
}

// Ensure ControllerServer satisfies the CSI interface at compile time.
var _ csi.ControllerServer = (*ControllerServer)(nil)

// NewControllerServer constructs a ControllerServer backed by the
// DefaultAgentDialer (plain-text gRPC, mTLS deferred to Phase 2).
//
// K8sClient must not be nil. DriverName is typically "pillar-csi.bhyoo.com".
func NewControllerServer(k8sClient client.Client, driverName string) *ControllerServer {
	return NewControllerServerWithDialer(k8sClient, driverName, DefaultAgentDialer)
}

// NewControllerServerWithDialer constructs a ControllerServer using the
// provided AgentDialer.  This variant is used in tests to inject a mock
// dialer that serves a real gRPC server backed by a mock agent.
func NewControllerServerWithDialer(
	k8sClient client.Client,
	driverName string,
	dialer AgentDialer,
) *ControllerServer {
	return &ControllerServer{
		k8sClient:  k8sClient,
		dialAgent:  dialer,
		driverName: driverName,
		sm:         NewVolumeStateMachine(),
	}
}

// GetStateMachine returns the VolumeStateMachine used by this ControllerServer.
//
// The returned machine is safe for concurrent use.  Callers (typically test
// helpers) may share the same machine with a NodeServer so that both services
// consult a single in-memory state store, enabling cross-component ordering
// validation in end-to-end tests.
func (s *ControllerServer) GetStateMachine() *VolumeStateMachine {
	return s.sm
}

// LoadStateFromPillarVolumes restores the in-memory VolumeStateMachine from
// all PillarVolume CRDs currently persisted in the cluster.  This should be
// called once at controller startup so that the state machine reflects any
// partial-failure states that survived a controller restart.
//
// Errors listing the CRDs are returned; individual volumes with unknown phases
// are skipped with a warning rather than causing a fatal error, because the
// CO will retry any in-progress operations.
func (s *ControllerServer) LoadStateFromPillarVolumes(ctx context.Context) error {
	pvList := &v1alpha1.PillarVolumeList{}
	err := s.k8sClient.List(ctx, pvList)
	if err != nil {
		return fmt.Errorf("list PillarVolumes: %w", err)
	}
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		state := pillarVolumePhaseToVolumeState(pv.Status.Phase)
		if state != StateNonExistent {
			s.sm.ForceState(pv.Spec.VolumeID, state)
		}
	}
	return nil
}

// pillarVolumePhaseToVolumeState converts a PillarVolumePhase to the
// corresponding in-memory VolumeState used by the state machine.
func pillarVolumePhaseToVolumeState(phase v1alpha1.PillarVolumePhase) VolumeState {
	switch phase {
	case v1alpha1.PillarVolumePhaseCreatePartial:
		return StateCreatePartial
	case v1alpha1.PillarVolumePhaseReady:
		return StateCreated
	case v1alpha1.PillarVolumePhaseControllerPublished:
		return StateControllerPublished
	case v1alpha1.PillarVolumePhaseNodeStagePartial:
		return StateNodeStagePartial
	case v1alpha1.PillarVolumePhaseNodeStaged:
		return StateNodeStaged
	case v1alpha1.PillarVolumePhaseNodePublished:
		return StateNodePublished
	default:
		// Provisioning and unknown phases are treated as NonExistent so that
		// the CO can restart the CreateVolume from scratch.
		return StateNonExistent
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Capability declarations
// ─────────────────────────────────────────────────────────────────────────────.

// supportedAccessModes lists every VolumeCapability access mode that
// pillar-csi can honor.
//
// Pillar-csi exposes raw block devices over NVMe-oF TCP.  A single namespace
// may be attached read-write to exactly one node (RWO / RWOP) or read-only to
// multiple nodes (ROX).  ReadWriteMany (RWX) is not supported because NVMe-oF
// block devices do not provide multi-writer coordination at the protocol level.
//
// Access-mode mapping (Kubernetes PVC → CSI constant):
//
//	ReadWriteOnce    (RWO)  → SINGLE_NODE_WRITER
//	ReadWriteOncePod (RWOP) → SINGLE_NODE_SINGLE_WRITER   (CSI spec v1.5+)
//	ReadOnlyMany     (ROX)  → MULTI_NODE_READER_ONLY
var supportedAccessModes = []csi.VolumeCapability_AccessMode_Mode{
	// RWO: one node may mount read-write.
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	// RWOP: one pod (on one node) may mount read-write.  Finer-grained than RWO.
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	// ROX: multiple nodes may mount read-only simultaneously.
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
}

// ControllerGetCapabilities reports the operations this controller supports.
func (s *ControllerServer) ControllerGetCapabilities(
	_ context.Context,
	_ *csi.ControllerGetCapabilitiesRequest,
) (*csi.ControllerGetCapabilitiesResponse, error) {
	_ = s // satisfy revive unused-receiver; method is a pure capability declaration
	rpcTypes := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		// SINGLE_NODE_MULTI_WRITER implies RWOP support per CSI spec §5.1.
		csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
		// GET_CAPACITY lets the CO schedule PVCs on nodes with sufficient space.
		csi.ControllerServiceCapability_RPC_GET_CAPACITY,
	}

	caps := make([]*csi.ControllerServiceCapability, 0, len(rpcTypes))
	for _, rpc := range rpcTypes {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: rpc,
				},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ValidateVolumeCapabilities
// ─────────────────────────────────────────────────────────────────────────────.

// ValidateVolumeCapabilities checks whether the requested volume capabilities
// are all supported by this driver.
//
// Per CSI spec §4.4:
//   - If every requested capability is supported, return a Confirmed struct
//     containing the same capabilities (plus the original parameters and
//     volumeContext so CO can verify the full parameter set).
//   - If any capability is unsupported, return an empty Confirmed field and
//     populate Message with a human-readable explanation.
//
// This implementation does not contact the agent because capability support is
// a static property of the driver, not of a specific volume.  The volume's
// existence is not verified here; that is the responsibility of CreateVolume
// and the CO's own state machine.
func (s *ControllerServer) ValidateVolumeCapabilities(
	_ context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	_ = s // satisfy revive unused-receiver; capability check is static
	if req.GetVolumeId() == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities must not be empty")
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"each volume capability must specify an access_mode")
		}
		if !isSupportedAccessMode(cap.GetAccessMode().GetMode()) {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf(
					"access mode %s is not supported by pillar-csi; supported modes: %s",
					cap.GetAccessMode().GetMode(),
					describeSupportedModes(),
				),
			}, nil
		}
	}

	// All requested capabilities are supported — echo them back in Confirmed.
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}

// isSupportedAccessMode returns true when mode is in supportedAccessModes.
func isSupportedAccessMode(mode csi.VolumeCapability_AccessMode_Mode) bool {
	return slices.Contains(supportedAccessModes, mode)
}

// describeSupportedModes returns a comma-separated string of the supported
// access mode names, used in diagnostic messages.
func describeSupportedModes() string {
	names := make([]string, 0, len(supportedAccessModes))
	for _, m := range supportedAccessModes {
		names = append(names, m.String())
	}
	var result strings.Builder
	for i, n := range names {
		if i > 0 {
			result.WriteString(", ")
		}
		result.WriteString(n)
	}
	return result.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// StorageClass / VolumeContext parameter key constants
// ─────────────────────────────────────────────────────────────────────────────.

const (
	// StorageClass parameter keys written by PillarBindingReconciler.
	paramPool         = "pillar-csi.bhyoo.com/pool"
	paramBinding      = "pillar-csi.bhyoo.com/binding"
	paramProtocol     = "pillar-csi.bhyoo.com/protocol"
	paramBackendType  = "pillar-csi.bhyoo.com/backend-type"
	paramProtocolType = "pillar-csi.bhyoo.com/protocol-type"
	paramTarget       = "pillar-csi.bhyoo.com/target"
	paramZFSPool      = "pillar-csi.bhyoo.com/zfs-pool"
	paramZFSParent    = "pillar-csi.bhyoo.com/zfs-parent-dataset"
	paramNVMeOFPort   = "pillar-csi.bhyoo.com/nvmeof-port"
	paramISCSIPort    = "pillar-csi.bhyoo.com/iscsi-port"
	paramNFSVersion   = "pillar-csi.bhyoo.com/nfs-version"
	paramLVMVG        = "pillar-csi.bhyoo.com/lvm-vg"

	// ParamACLEnabled controls NVMe-oF host NQN ACL enforcement.
	// Value: "true" (default, ACL enforced) or "false" (allow_any_host=1).
	// Set by the PillarBinding controller from the PillarProtocol NVMeOFTCPConfig.ACL field.
	paramACLEnabled = "pillar-csi.bhyoo.com/acl-enabled"

	// ParamZFSPropPrefix is the key prefix used to pass individual ZFS
	// properties through the merged parameter map to buildBackendParams.
	// Example: "pillar-csi.bhyoo.com/zfs-prop.compression" = "lz4".
	paramZFSPropPrefix = "pillar-csi.bhyoo.com/zfs-prop."

	// ParamPVCName / paramPVCNamespace are injected by external-provisioner
	// when the --extra-create-metadata flag is set.  They allow CreateVolume
	// to look up the originating PVC and read per-PVC annotation overrides.
	paramPVCName      = "csi.storage.k8s.io/pvc-name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc-namespace"

	// PvcAnnotationParamPrefix is the PVC annotation prefix for per-PVC
	// parameter overrides (Layer 4 of the merge hierarchy).
	// Example annotation: "pillar-csi.bhyoo.com/param.zfs-prop.compression=lz4"
	// results in param key "pillar-csi.bhyoo.com/zfs-prop.compression" = "lz4".
	pvcAnnotationParamPrefix = "pillar-csi.bhyoo.com/param."

	// VolumeContext keys stored in the PersistentVolume and read by NodeStageVolume.
	//
	// VcTargetID, vcAddress, and vcPort intentionally use the same string values
	// as the exported VolumeContextKey* constants in node.go so that the
	// VolumeContext written by CreateVolume can be passed directly to
	// NodeStageVolume by the CO without any translation.
	vcTargetID = VolumeContextKeyTargetNQN // "target_id"
	vcAddress  = VolumeContextKeyAddress   // "address"
	vcPort     = VolumeContextKeyPort      // "port"

	// VcVolumeRef and vcProtocolType are additional context keys not consumed
	// by NodeStageVolume but useful for diagnostics and future extensions.
	vcVolumeRef    = "pillar-csi.bhyoo.com/volume-ref"
	vcProtocolType = "pillar-csi.bhyoo.com/protocol-type"

	// VolumeID format: <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
	// Example: storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-abc123
	//
	// Parsing uses strings.SplitN(id, "/", 4) which always yields exactly four
	// fields (the last field may itself contain slashes, e.g. "tank/pvc-abc123").
	volumeIDParts = 4
)

// ─────────────────────────────────────────────────────────────────────────────
// CreateVolume
// ─────────────────────────────────────────────────────────────────────────────.

// CreateVolume provisions a new volume by orchestrating three agent RPCs.
//
// Lifecycle (CSI spec §4.3.1):
//  1. Call agent.CreateVolume — creates the backend storage resource
//     (ZFS zvol, LVM LV, …).
//  2. Call agent.ExportVolume — publishes the volume over the configured
//     network protocol (NVMe-oF TCP, iSCSI, NFS).
//
// The returned VolumeId encodes routing metadata in the form:
//
//	<target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
//
// This lets DeleteVolume reach the correct agent and call the right RPCs
// without additional Kubernetes API lookups of StorageClass parameters.
//
// Idempotency: both agent RPCs are idempotent; calling CreateVolume twice
// with the same name returns the same VolumeId and VolumeContext.
func (s *ControllerServer) CreateVolume( //nolint:gocognit,gocyclo,funlen // complex but necessary
	ctx context.Context,
	req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	// ── Input validation ──────────────────────────────────────────────────────
	if req.GetName() == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities must not be empty")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"each volume_capability must specify an access_mode")
		}
		if !isSupportedAccessMode(cap.GetAccessMode().GetMode()) {
			return nil, status.Errorf(codes.InvalidArgument,
				"unsupported access mode %s", cap.GetAccessMode().GetMode())
		}
	}

	scParams := req.GetParameters()

	// ── 4-level merge hierarchy: Pool → Protocol → Binding → PVC annotation ──
	// mergeParamsFromCRDs augments the StorageClass params with data fetched
	// live from the PillarBinding, PillarPool, and PillarProtocol CRDs and
	// then overlays any per-PVC annotation overrides.  Falls back gracefully
	// when the binding name is absent (e.g. manually-created StorageClasses).
	params, err := s.mergeParamsFromCRDs(ctx, scParams)
	if err != nil {
		// PVC annotation validation errors are user-facing (bad annotation
		// content); surface them as InvalidArgument so the CO can surface
		// a useful message to the user.  Infrastructure errors (CRD fetch
		// failures, etc.) are surfaced as Internal.
		var annotErr *pvcAnnotationValidationError
		if errors.As(err, &annotErr) {
			return nil, status.Errorf(codes.InvalidArgument,
				"PVC annotation validation failed: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "parameter merge failed: %v", err)
	}

	// ── Extract required routing parameters ──────────────────────────────────
	targetName := params[paramTarget]
	backendTypeStr := params[paramBackendType]
	protocolTypeStr := params[paramProtocolType]

	if targetName == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required", paramTarget)
	}
	if backendTypeStr == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required", paramBackendType)
	}
	if protocolTypeStr == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required", paramProtocolType)
	}

	agentBackendType := mapBackendType(backendTypeStr)
	agentProtocolType := mapProtocolType(protocolTypeStr)

	// ── Build the agent-level volume ID ──────────────────────────────────────
	// For ZFS backends: "<zfs-pool>/<volume-name>".
	// Fallback: "<pillar-pool-name>/<volume-name>".
	agentVolID := buildAgentVolumeID(params, req.GetName())

	// ── Build the CSI volume ID (encodes all routing metadata) ───────────────
	// Format: <target>/<protocol-type>/<backend-type>/<agent-vol-id>
	volumeID := strings.Join(
		[]string{targetName, protocolTypeStr, backendTypeStr, agentVolID},
		"/",
	)

	// ── Load persisted state (idempotency and partial-failure recovery) ───────
	// The PillarVolume CRD name is the CSI volume name, which is a
	// Kubernetes-compatible identifier assigned by the CO (e.g. "pvc-abc123").
	pvName := req.GetName()
	existingPV, pvExists, pvErr := s.loadPillarVolume(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to load PillarVolume %q: %v", pvName, pvErr)
	}
	if pvExists {
		// Restore the in-memory state machine entry from the persisted phase.
		s.sm.ForceState(volumeID, pillarVolumePhaseToVolumeState(existingPV.Status.Phase))
	}
	// If the volume is already fully provisioned, return the cached response.
	if s.sm.GetState(volumeID) == StateCreated &&
		pvExists && existingPV.Status.ExportInfo != nil {
		ei := existingPV.Status.ExportInfo
		existingCap := existingPV.Spec.CapacityBytes

		// CSI spec §5.1.1: if the existing volume doesn't satisfy the new
		// capacity range, return AlreadyExists to signal incompatibility.
		if cr := req.GetCapacityRange(); cr != nil {
			if cr.GetRequiredBytes() > 0 && existingCap < cr.GetRequiredBytes() {
				return nil, status.Errorf(codes.AlreadyExists,
					"volume %q already exists with capacity %d bytes, which is less than "+
						"the requested minimum %d bytes",
					req.GetName(), existingCap, cr.GetRequiredBytes())
			}
			if cr.GetLimitBytes() > 0 && existingCap > cr.GetLimitBytes() {
				return nil, status.Errorf(codes.AlreadyExists,
					"volume %q already exists with capacity %d bytes, which exceeds "+
						"the requested limit %d bytes",
					req.GetName(), existingCap, cr.GetLimitBytes())
			}
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      volumeID,
				CapacityBytes: existingCap,
				VolumeContext: map[string]string{
					vcTargetID:     ei.TargetID,
					vcAddress:      ei.Address,
					vcPort:         strconv.Itoa(int(ei.Port)),
					vcVolumeRef:    ei.VolumeRef,
					vcProtocolType: existingPV.Spec.ProtocolType,
				},
			},
		}, nil
	}

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErr := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErr != nil {
		if k8serrors.IsNotFound(getTargetErr) {
			return nil, status.Errorf(codes.NotFound,
				"PillarTarget %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErr)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Determine requested capacity ──────────────────────────────────────────
	var capacityBytes int64
	if cr := req.GetCapacityRange(); cr != nil {
		capacityBytes = cr.GetRequiredBytes()
	}

	// ── Idempotency: detect a prior partial backend creation ──────────────────
	// When the state machine is in StateCreatePartial, the backend storage
	// resource (zvol, LVM LV, …) was successfully created in a previous
	// CreateVolume call that failed during ExportVolume.  On retry we skip
	// Step 1 entirely — the agent would return the existing resource
	// (idempotent), but skipping the call makes the recovery path explicit,
	// avoids an unnecessary round-trip, and ensures we never attempt to
	// re-create a zvol that already holds data.
	//
	// The device path required by ExportVolume is read from the PillarVolume
	// CRD that was durably written during the prior partial attempt.
	var (
		devicePath     string
		actualCapacity = capacityBytes
		skipBackend    bool
	)
	if s.sm.GetState(volumeID) == StateCreatePartial && pvExists &&
		existingPV.Status.BackendDevicePath != "" {
		devicePath = existingPV.Status.BackendDevicePath
		if existingPV.Spec.CapacityBytes > 0 {
			actualCapacity = existingPV.Spec.CapacityBytes
		}
		skipBackend = true
	}

	if !skipBackend {
		// ── Step 1: Create the backend storage resource ──────────────────────
		createResp, createErr := agentClient.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
			VolumeId:      agentVolID,
			CapacityBytes: capacityBytes,
			BackendType:   agentBackendType,
			BackendParams: buildBackendParams(params, agentBackendType),
			AccessType:    accessTypeForProtocol(agentProtocolType),
		})
		if createErr != nil {
			grpcSt, _ := status.FromError(createErr)
			return nil, status.Errorf(grpcSt.Code(),
				"agent CreateVolume(%q) failed: %v", agentVolID, createErr)
		}

		devicePath = createResp.GetDevicePath()
		if resp := createResp.GetCapacityBytes(); resp != 0 {
			actualCapacity = resp
		}

		// ── Record backend creation (partial-failure guard) ──────────────────
		// Transition: NonExistent → CreatePartial.  We persist this state to
		// the PillarVolume CRD before calling ExportVolume so that a controller
		// crash between these two steps is recoverable: the next CreateVolume
		// call will find the CRD in CreatePartial phase, skip backend creation
		// (already done, and skipBackend will be set), then retry ExportVolume.
		//nolint:errcheck // transition errors are non-fatal; state is force-set on success
		_, _ = s.sm.Transition(volumeID, OpCreateVolumeBackend)
		persistErr := s.persistCreatePartial(ctx, pvName, volumeID, agentVolID,
			targetName, backendTypeStr, protocolTypeStr, actualCapacity, devicePath, pvExists)
		if persistErr != nil {
			// Cannot durably record the partial state.  Fail the operation so
			// that the backend resource is not left silently orphaned.
			return nil, status.Errorf(codes.Internal,
				"failed to persist partial-failure state for volume %q: %v",
				pvName, persistErr)
		}
	}

	// ── Step 2: Export the volume over the network protocol ───────────────────
	// The NVMe-oF / iSCSI bind address is the storage node's IP (no port).
	// agent.ExportVolume is idempotent: if the export already exists (retry
	// scenario), it returns the existing ExportInfo without error.
	bindIP := extractIP(agentAddr)
	exportResp, err := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		ExportParams: buildExportParams(params, agentProtocolType, bindIP),
		DevicePath:   devicePath,
		AclEnabled:   parseACLEnabled(params[paramACLEnabled]),
	})
	if err != nil {
		// ExportVolume failed.  The PillarVolume CRD already records the
		// CreatePartial state durably; the CO may retry safely.  The next
		// CreateVolume call will skip Step 1 (idempotent) and retry Step 2.
		grpcSt, _ := status.FromError(err)
		return nil, status.Errorf(grpcSt.Code(),
			"agent ExportVolume(%q) failed: %v", agentVolID, err)
	}

	// ── Advance to fully-created state ────────────────────────────────────────
	s.sm.ForceState(volumeID, StateCreated)

	// Best-effort: update the PillarVolume CRD to the Ready phase and cache
	// the export parameters for idempotent re-use on subsequent CreateVolume
	// calls.  A failure here is not fatal — the volume is already provisioned
	// and the CO will not retry a successful CreateVolume.
	info := exportResp.GetExportInfo()
	//nolint:errcheck // best-effort CRD update; volume is already provisioned and CO will not retry
	_ = s.persistVolumeReady(ctx, pvName, info, actualCapacity)

	// ── Build VolumeContext from ExportInfo ───────────────────────────────────
	// These key/value pairs are stored in the PersistentVolume and forwarded to
	// NodeStageVolume so the node can connect to the volume over the network.
	volumeContext := map[string]string{
		vcTargetID:     info.GetTargetId(),
		vcAddress:      info.GetAddress(),
		vcPort:         strconv.Itoa(int(info.GetPort())),
		vcVolumeRef:    info.GetVolumeRef(),
		vcProtocolType: protocolTypeStr,
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: actualCapacity,
			VolumeContext: volumeContext,
		},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteVolume
// ─────────────────────────────────────────────────────────────────────────────.

// DeleteVolume deprovisions a volume by orchestrating two agent RPCs.
//
// Lifecycle (CSI spec §4.3.2):
//  1. Call agent.UnexportVolume — removes the network-protocol export entry.
//  2. Call agent.DeleteVolume — destroys the backend storage resource.
//
// Both agent RPCs are idempotent:
//   - UnexportVolume on a non-existent export returns success.
//   - DeleteVolume on a non-existent volume returns success.
//
// If the PillarTarget has been decommissioned (not found in the API server)
// we treat that as the volume already being gone and return success.
func (s *ControllerServer) DeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}

	// ── Parse the encoded volume ID ───────────────────────────────────────────
	// Format: <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		// Malformed ID — could be a volume provisioned by a different driver.
		// The CSI spec requires returning success for volumes that are unknown.
		return &csi.DeleteVolumeResponse{}, nil
	}
	targetName := parts[0]
	protocolTypeStr := parts[1]
	backendTypeStr := parts[2]
	agentVolID := parts[3]

	agentProtocolType := mapProtocolType(protocolTypeStr)
	agentBackendType := mapBackendType(backendTypeStr)

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErrDV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrDV != nil {
		if k8serrors.IsNotFound(getTargetErrDV) {
			// Storage node decommissioned; the volume cannot exist any more.
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErrDV)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		// Target exists but has no address yet.  This is a transient state;
		// return Unavailable so the CO will retry.
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Step 1: Remove the network export (idempotent) ────────────────────────
	unexportResp, unexportErr := agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
	})
	_ = unexportResp
	if unexportErr != nil {
		st, _ := status.FromError(unexportErr)
		if st.Code() != codes.NotFound {
			return nil, status.Errorf(st.Code(),
				"agent UnexportVolume(%q) failed: %v", agentVolID, unexportErr)
		}
		// NotFound → already unexported; continue to backend deletion.
	}

	// ── Step 2: Destroy the backend storage resource (idempotent) ─────────────
	deleteResp, deleteErr := agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId:    agentVolID,
		BackendType: agentBackendType,
	})
	_ = deleteResp
	if deleteErr != nil {
		st, _ := status.FromError(deleteErr)
		if st.Code() != codes.NotFound {
			return nil, status.Errorf(st.Code(),
				"agent DeleteVolume(%q) failed: %v", agentVolID, deleteErr)
		}
		// NotFound → already deleted; success.
	}

	// ── Update the in-memory state machine ────────────────────────────────────
	s.sm.ForceState(volumeID, StateNonExistent)

	// ── Clean up the PillarVolume CRD (best-effort) ───────────────────────────
	// Extract the CSI volume name from the agentVolID (last path component).
	// This recovers the PillarVolume resource name that CreateVolume used.
	pvName := agentVolID
	if idx := strings.LastIndex(agentVolID, "/"); idx >= 0 {
		pvName = agentVolID[idx+1:]
	}
	//nolint:errcheck // best-effort CRD cleanup; volume is already deleted from storage
	_ = s.deletePillarVolume(ctx, pvName)

	return &csi.DeleteVolumeResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PillarVolume CRD helpers (partial-failure state persistence)
// ─────────────────────────────────────────────────────────────────────────────.

// loadPillarVolume returns the PillarVolume CRD for the given volume name,
// along with a boolean indicating whether it was found.  A nil k8sClient or
// a NotFound error are treated as "not found" (non-error).
func (s *ControllerServer) loadPillarVolume(
	ctx context.Context,
	pvName string,
) (*v1alpha1.PillarVolume, bool, error) {
	if s.k8sClient == nil {
		return nil, false, nil
	}
	pv := &v1alpha1.PillarVolume{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get PillarVolume %q: %w", pvName, err)
	}
	return pv, true, nil
}

// persistCreatePartial creates or updates a PillarVolume CRD with
// PillarVolumePhaseCreatePartial, recording that the backend storage resource
// has been created but ExportVolume has not yet succeeded.
//
// DevicePath is the path returned by agent.CreateVolume (e.g.
// "/dev/zvol/pool/pvc-abc123").  It is stored in Status.BackendDevicePath so
// that a retry of CreateVolume can skip the backend-creation step and call
// ExportVolume directly.
//
// PvExists must be true when a PillarVolume with pvName already exists in the
// cluster; the function then updates via Status().Update() instead of Create().
func (s *ControllerServer) persistCreatePartial(
	ctx context.Context,
	pvName, volumeID, agentVolID, targetName,
	backendType, protocolType string,
	capacity int64,
	devicePath string,
	pvExists bool,
) error {
	if s.k8sClient == nil {
		return nil
	}
	now := metav1.Now()
	partialStatus := v1alpha1.PillarVolumeStatus{
		Phase:             v1alpha1.PillarVolumePhaseCreatePartial,
		BackendDevicePath: devicePath,
		PartialFailure: &v1alpha1.PartialFailureInfo{
			FailedOperation: "ExportVolume",
			FailedAt:        now,
			Reason:          "ExportPending",
			Message: "Backend storage resource created successfully; " +
				"ExportVolume has not yet succeeded.  " +
				"Retry CreateVolume to re-attempt the export step.",
			BackendCreated: true,
		},
	}

	if !pvExists {
		pv := &v1alpha1.PillarVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: pvName,
			},
			Spec: v1alpha1.PillarVolumeSpec{
				VolumeID:      volumeID,
				AgentVolumeID: agentVolID,
				TargetRef:     targetName,
				BackendType:   backendType,
				ProtocolType:  protocolType,
				CapacityBytes: capacity,
			},
		}
		err := s.k8sClient.Create(ctx, pv)
		if err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("create PillarVolume %q: %w", pvName, err)
			}
			// Race: another instance created it; fall through to update below.
		}
		// Refresh to get the assigned resourceVersion before the status update.
		err = s.k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv)
		if err != nil {
			return fmt.Errorf("get PillarVolume %q after create: %w", pvName, err)
		}
		pv.Status = partialStatus
		updateErr := s.k8sClient.Status().Update(ctx, pv)
		if updateErr != nil {
			return fmt.Errorf("update PillarVolume %q status: %w", pvName, updateErr)
		}
		return nil
	}

	// pvExists == true: fetch the current object and update its status.
	existing := &v1alpha1.PillarVolume{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, existing)
	if err != nil {
		return fmt.Errorf("get PillarVolume %q: %w", pvName, err)
	}
	existing.Status = partialStatus
	updateErr := s.k8sClient.Status().Update(ctx, existing)
	if updateErr != nil {
		return fmt.Errorf("update PillarVolume %q status: %w", pvName, updateErr)
	}
	return nil
}

// exportInfoGetter is the minimal interface of agentv1.ExportInfo used by
// persistVolumeReady.  It allows passing a nil-safe pointer without importing
// the proto package in the interface definition.
type exportInfoGetter interface {
	GetTargetId() string
	GetAddress() string
	GetPort() int32
	GetVolumeRef() string
}

// persistVolumeReady updates the PillarVolume CRD to PillarVolumePhaseReady
// and stores the export info returned by agent.ExportVolume for idempotent
// re-use.  A NotFound error is treated as a no-op (CRD was never created).
func (s *ControllerServer) persistVolumeReady(
	ctx context.Context,
	pvName string,
	info exportInfoGetter,
	capacity int64,
) error {
	if s.k8sClient == nil || info == nil {
		return nil
	}
	pv := &v1alpha1.PillarVolume{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get PillarVolume %q: %w", pvName, err)
	}
	pv.Status.Phase = v1alpha1.PillarVolumePhaseReady
	pv.Status.PartialFailure = nil
	pv.Status.BackendDevicePath = "" // no longer needed once export is live
	pv.Status.ExportInfo = &v1alpha1.VolumeExportInfo{
		TargetID:  info.GetTargetId(),
		Address:   info.GetAddress(),
		Port:      info.GetPort(),
		VolumeRef: info.GetVolumeRef(),
	}
	if capacity > 0 && pv.Spec.CapacityBytes == 0 {
		pv.Spec.CapacityBytes = capacity
	}
	updateErr := s.k8sClient.Status().Update(ctx, pv)
	if updateErr != nil {
		return fmt.Errorf("update PillarVolume %q status to ready: %w", pvName, updateErr)
	}
	return nil
}

// deletePillarVolume removes the PillarVolume CRD for the given CSI volume
// name.  NotFound is treated as success (idempotent).
func (s *ControllerServer) deletePillarVolume(ctx context.Context, pvName string) error {
	if s.k8sClient == nil {
		return nil
	}
	pv := &v1alpha1.PillarVolume{}
	pv.Name = pvName
	err := s.k8sClient.Delete(ctx, pv)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete PillarVolume %q: %w", pvName, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Volume ID helpers
// ─────────────────────────────────────────────────────────────────────────────.

// ─────────────────────────────────────────────────────────────────────────────
// 4-level parameter merge hierarchy
// ─────────────────────────────────────────────────────────────────────────────.

// mergeParamsFromCRDs builds the 4-level parameter merge hierarchy:
//
//	Layer 1 (Pool)    – ZFS properties from PillarPool.spec.backend.zfs.properties
//	Layer 2 (Protocol)– (protocol params already captured in StorageClass at bind time)
//	Layer 3 (Binding) – ZFS property overrides from PillarBinding.spec.overrides.backend.zfs
//	Layer 4 (PVC)     – per-PVC annotation overrides prefixed with pvcAnnotationParamPrefix
//
// The StorageClass parameters (scParams) are the authoritative source for
// routing metadata (target, backend-type, protocol-type, etc.) and serve as
// the baseline.  ZFS properties (which are not stored in the StorageClass)
// are fetched from the PillarPool CRD and layered on top, then Binding
// overrides are applied, and finally per-PVC annotation overrides win over
// everything else.
//
// When the binding name is absent from scParams the function returns a shallow
// copy of scParams unchanged, preserving backward compatibility with
// manually-crafted StorageClasses that do not reference a PillarBinding.
//
//nolint:gocognit,gocyclo // complex but necessary parameter merge hierarchy
func (s *ControllerServer) mergeParamsFromCRDs(
	ctx context.Context,
	scParams map[string]string,
) (map[string]string, error) {
	// Start with a copy of the StorageClass params so callers can freely mutate
	// the returned map without affecting the original request parameters.
	merged := make(map[string]string, len(scParams))
	maps.Copy(merged, scParams)

	bindingName := scParams[paramBinding]
	if bindingName == "" {
		// No binding reference — skip CRD lookups and go straight to PVC
		// annotations (still useful even without a binding name).
		err := s.applyPVCAnnotationOverrides(ctx, merged, scParams)
		if err != nil {
			return nil, err
		}
		return merged, nil
	}

	// ── Fetch PillarBinding ───────────────────────────────────────────────────
	binding := &v1alpha1.PillarBinding{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: bindingName}, binding)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Binding was deleted after the StorageClass was created — fall back
			// to StorageClass params only.
			err2 := s.applyPVCAnnotationOverrides(ctx, merged, scParams)
			if err2 != nil {
				return nil, err2
			}
			return merged, nil
		}
		return nil, fmt.Errorf("fetch PillarBinding %q: %w", bindingName, err)
	}

	// ── Layer 1: fetch PillarPool and apply ZFS properties ───────────────────
	pool := &v1alpha1.PillarPool{}
	err = s.k8sClient.Get(ctx, types.NamespacedName{Name: binding.Spec.PoolRef}, pool)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			err2 := s.applyPVCAnnotationOverrides(ctx, merged, scParams)
			if err2 != nil {
				return nil, err2
			}
			return merged, nil
		}
		return nil, fmt.Errorf("fetch PillarPool %q: %w", binding.Spec.PoolRef, err)
	}

	// Apply Pool-level ZFS properties as the lowest-priority ZFS property layer.
	if pool.Spec.Backend.ZFS != nil {
		for k, v := range pool.Spec.Backend.ZFS.Properties {
			merged[paramZFSPropPrefix+k] = v
		}
	}

	// ── Layer 3: apply Binding ZFS property overrides ────────────────────────
	// (Layer 2 — protocol params — are already embedded in the StorageClass.)
	if binding.Spec.Overrides != nil &&
		binding.Spec.Overrides.Backend != nil &&
		binding.Spec.Overrides.Backend.ZFS != nil {
		for k, v := range binding.Spec.Overrides.Backend.ZFS.Properties {
			merged[paramZFSPropPrefix+k] = v
		}
	}

	// ── Layer 4: PVC annotation overrides (highest priority) ─────────────────
	err = s.applyPVCAnnotationOverrides(ctx, merged, scParams)
	if err != nil {
		return nil, err
	}

	return merged, nil
}

// applyPVCAnnotationOverrides looks up the PVC identified by the
// csi.storage.k8s.io/pvc-name and csi.storage.k8s.io/pvc-namespace
// parameters (injected by external-provisioner --extra-create-metadata) and
// merges PVC-level annotation overrides into merged using ParsePVCAnnotations.
// This is the highest-priority override layer.
//
// PVC lookup failures are silently ignored (the annotation override is
// optional and a missing PVC or API error should not fail provisioning).
// Annotation validation errors (e.g. structural field overrides) are returned
// as errors so that CreateVolume can reject them with InvalidArgument.
func (s *ControllerServer) applyPVCAnnotationOverrides(
	ctx context.Context,
	merged map[string]string,
	scParams map[string]string,
) error {
	pvcName := scParams[paramPVCName]
	pvcNamespace := scParams[paramPVCNamespace]
	if pvcName == "" || pvcNamespace == "" {
		return nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	pvcGetErr := s.k8sClient.Get(ctx, types.NamespacedName{
		Name:      pvcName,
		Namespace: pvcNamespace,
	}, pvc)
	if pvcGetErr != nil {
		// PVC lookup failure is non-fatal; skip annotation overrides.
		return nil //nolint:nilerr // intentional: PVC lookup failure is non-fatal
	}

	overrides, err := ParsePVCAnnotations(pvc.Annotations)
	if err != nil {
		// Annotation validation failure (e.g. structural field override
		// attempt) is surfaced to the caller so CreateVolume can reject it
		// with InvalidArgument (not Internal).
		return &pvcAnnotationValidationError{
			pvcNamespace: pvcNamespace,
			pvcName:      pvcName,
			cause:        err,
		}
	}

	maps.Copy(merged, overrides)
	return nil
}

// buildAgentVolumeID constructs the volume identifier used in all agent RPCs.
//
// For ZFS backends the format is "<zfs-pool>/<volume-name>" which matches the
// agent's internal naming convention (the zvol lives at /dev/zvol/<pool>/<name>).
// For other backends it falls back to "<pillar-pool-name>/<volume-name>".
func buildAgentVolumeID(params map[string]string, volumeName string) string {
	if zfsPool := params[paramZFSPool]; zfsPool != "" {
		return zfsPool + "/" + volumeName
	}
	if pool := params[paramPool]; pool != "" {
		return pool + "/" + volumeName
	}
	return volumeName
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend / protocol type mappers
// ─────────────────────────────────────────────────────────────────────────────.

// mapBackendType converts the StorageClass backend-type string to the agent
// protobuf enum value.
func mapBackendType(s string) agentv1.BackendType {
	switch s {
	case "zfs-zvol":
		return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
	case "zfs-dataset":
		return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	case "lvm-lv":
		return agentv1.BackendType_BACKEND_TYPE_LVM
	case "dir":
		return agentv1.BackendType_BACKEND_TYPE_DIRECTORY
	default:
		return agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}

// mapProtocolType converts the StorageClass protocol-type string to the agent
// protobuf enum value.
func mapProtocolType(s string) agentv1.ProtocolType {
	switch s {
	case "nvmeof-tcp":
		return agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP
	case "iscsi":
		return agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI
	case "nfs":
		return agentv1.ProtocolType_PROTOCOL_TYPE_NFS
	default:
		return agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED
	}
}

// accessTypeForProtocol returns the VolumeAccessType to request from the agent
// for the given network storage protocol.
//
// Block protocols (NVMe-oF TCP, iSCSI) transport raw block devices, so the
// agent creates a BLOCK resource.  File protocols (NFS, SMB) export
// filesystems, so the agent creates a MOUNT resource.
func accessTypeForProtocol(p agentv1.ProtocolType) agentv1.VolumeAccessType {
	switch p {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NFS, agentv1.ProtocolType_PROTOCOL_TYPE_SMB:
		return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_MOUNT
	default:
		return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_BLOCK
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Backend / export parameter builders
// ─────────────────────────────────────────────────────────────────────────────.

// buildBackendParams constructs the backend-specific creation parameters for
// the agent.CreateVolume RPC from the StorageClass parameter map.
//
// For ZFS backends the Properties map is populated from all params that carry
// the paramZFSPropPrefix prefix (e.g. "pillar-csi.bhyoo.com/zfs-prop.compression").
// These originate from PillarPool.spec.backend.zfs.properties (Layer 1),
// PillarBinding.spec.overrides.backend.zfs.properties (Layer 3), or per-PVC
// annotation overrides (Layer 4) and have already been merged into params by
// mergeParamsFromCRDs before CreateVolume calls buildBackendParams.
func buildBackendParams(params map[string]string, backendType agentv1.BackendType) *agentv1.BackendParams {
	switch backendType {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL, agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET:
		// Collect ZFS properties from the merged parameter map.
		var zfsProps map[string]string
		for k, v := range params {
			if after, ok := strings.CutPrefix(k, paramZFSPropPrefix); ok && after != "" {
				if zfsProps == nil {
					zfsProps = make(map[string]string)
				}
				zfsProps[after] = v
			}
		}
		return &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Zfs{
				Zfs: &agentv1.ZfsVolumeParams{
					Pool:          params[paramZFSPool],
					ParentDataset: params[paramZFSParent],
					Properties:    zfsProps,
				},
			},
		}
	case agentv1.BackendType_BACKEND_TYPE_LVM:
		return &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{
					VolumeGroup: params["pillar-csi.bhyoo.com/lvm-vg"],
				},
			},
		}
	default:
		return nil
	}
}

// buildExportParams constructs the protocol-specific export parameters for the
// agent.ExportVolume RPC.
//
// BindAddress is the raw IP address of the storage node — specifically,
// PillarTarget.Status.ResolvedAddress with the ":port" suffix stripped.
// The NVMe-oF / iSCSI kernel targets bind to an IP, not an IP:port pair.
func buildExportParams(
	params map[string]string,
	protocolType agentv1.ProtocolType,
	bindAddress string,
) *agentv1.ExportParams {
	switch protocolType {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		port := int32(4420)
		if portStr := params[paramNVMeOFPort]; portStr != "" {
			p, parseErr := strconv.ParseInt(portStr, 10, 32)
			if parseErr == nil {
				port = int32(p)
			}
		}
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_NvmeofTcp{
				NvmeofTcp: &agentv1.NvmeofTcpExportParams{
					BindAddress: bindAddress,
					Port:        port,
				},
			},
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		port := int32(3260)
		if portStr := params[paramISCSIPort]; portStr != "" {
			p, parseErr := strconv.ParseInt(portStr, 10, 32)
			if parseErr == nil {
				port = int32(p)
			}
		}
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_Iscsi{
				Iscsi: &agentv1.IscsiExportParams{
					BindAddress: bindAddress,
					Port:        port,
				},
			},
		}
	case agentv1.ProtocolType_PROTOCOL_TYPE_NFS:
		version := "4.2"
		if v := params[paramNFSVersion]; v != "" {
			version = v
		}
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_Nfs{
				Nfs: &agentv1.NfsExportParams{
					Version: version,
				},
			},
		}
	default:
		return nil
	}
}

// parseACLEnabled interprets the acl-enabled StorageClass parameter.
//
// The parameter is written by the PillarBinding controller using the value of
// PillarProtocol.spec.nvmeofTcp.acl (or the iSCSI equivalent).  The default
// behavior when the key is absent or empty is true (ACL enforced), which
// matches the protocol-type defaults in the CRD schema.
//
// Only the literal string "false" disables ACL; any other value (including
// "true", "1", "yes", or an empty string) keeps ACL enabled.
func parseACLEnabled(val string) bool {
	return val != "false"
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerPublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// ControllerPublishVolume grants a specific node access to a volume by
// calling agent.AllowInitiator on the storage node.
//
// The CSI node_id provided in the request is used directly as the
// initiator_id passed to the agent:
//
//	NVMe-oF TCP → host NQN (e.g. "nqn.2014-08.org.nvmexpress:uuid:…")
//	iSCSI       → IQN (e.g. "iqn.1993-08.org.debian:…")
//	NFS/SMB     → client IP address or CIDR
//
// The NodeServer populates this identifier in NodeGetInfo so that the CO
// can route the publish call correctly.
//
// Idempotency: AllowInitiator is idempotent on the agent side; calling it
// twice for the same volume / initiator pair is safe.
func (s *ControllerServer) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest,
) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()

	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if nodeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}
	if req.GetVolumeCapability() == nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_capability is required")
	}

	// ── Parse the encoded volume ID ───────────────────────────────────────────
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return nil, status.Errorf(codes.InvalidArgument,
			"malformed volume_id %q: expected format <target>/<protocol>/<backend>/<vol-id>",
			volumeID)
	}
	targetName := parts[0]
	protocolTypeStr := parts[1]
	agentVolID := parts[3]

	agentProtocolType := mapProtocolType(protocolTypeStr)

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErrCPV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrCPV != nil {
		if k8serrors.IsNotFound(getTargetErrCPV) {
			return nil, status.Errorf(codes.NotFound,
				"PillarTarget %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErrCPV)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Grant initiator access ────────────────────────────────────────────────
	// node_id serves as the initiator_id:
	//   NVMe-oF → host NQN, iSCSI → IQN, NFS/SMB → client IP.
	allowResp, allowErr := agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		InitiatorId:  nodeID,
	})
	_ = allowResp
	if allowErr != nil {
		grpcSt, _ := status.FromError(allowErr)
		return nil, status.Errorf(grpcSt.Code(),
			"agent AllowInitiator(%q, initiator=%q) failed: %v",
			agentVolID, nodeID, allowErr)
	}

	// ── Advance state machine to ControllerPublished ─────────────────────────
	// Record that this node now has ACL access to the volume.  We use
	// ForceState rather than Transition so that the call succeeds even when
	// the volume was not previously tracked in the SM (e.g. when
	// ControllerPublishVolume is invoked independently of CreateVolume in
	// tests, or after a controller restart where the SM was rebuilt from CRDs
	// and a transition-table gap would otherwise block the update).
	s.sm.ForceState(volumeID, StateControllerPublished)

	// PublishContext is forwarded to NodeStageVolume.  No additional keys are
	// required here; the volume connection parameters are already stored in
	// the PersistentVolume's VolumeContext by CreateVolume.
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerUnpublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// ControllerUnpublishVolume revokes a node's access to a volume by calling
// agent.DenyInitiator.
//
// Idempotency:
//   - If the PillarTarget no longer exists (node decommissioned), return
//     success — the volume and its ACL entries cannot exist either.
//   - If the agent returns NotFound for DenyInitiator, return success — the
//     ACL entry was already absent.
func (s *ControllerServer) ControllerUnpublishVolume(
	ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest,
) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()

	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	// node_id may be empty per CSI spec §4.5.2 (controller must unpublish
	// from all nodes).  We treat an empty node_id as a no-op because
	// pillar-csi manages per-initiator ACL entries and cannot remove all of
	// them without knowing which initiators were granted access.
	// A more complete implementation would call DenyInitiator for each
	// known initiator; that is tracked as a Phase 2 item.

	// ── Parse the encoded volume ID ───────────────────────────────────────────
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		// Unknown volume ID format; treat as already unpublished.
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	targetName := parts[0]
	protocolTypeStr := parts[1]
	agentVolID := parts[3]

	agentProtocolType := mapProtocolType(protocolTypeStr)

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErrCUV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrCUV != nil {
		if k8serrors.IsNotFound(getTargetErrCUV) {
			// Storage node decommissioned; ACL entries cannot exist.
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErrCUV)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		// Transient; CO will retry.
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address", targetName)
	}

	// If node_id is empty we cannot identify which initiator to deny.
	// Return success without contacting the agent (Phase 2: deny all).
	if nodeID == "" {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Revoke initiator access (idempotent) ──────────────────────────────────
	denyResp, denyErr := agentClient.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		InitiatorId:  nodeID,
	})
	_ = denyResp
	if denyErr != nil {
		st, _ := status.FromError(denyErr)
		if st.Code() != codes.NotFound {
			return nil, status.Errorf(st.Code(),
				"agent DenyInitiator(%q, initiator=%q) failed: %v",
				agentVolID, nodeID, denyErr)
		}
		// NotFound → ACL entry already absent; success.
	}

	// ── Revert state machine to Created ──────────────────────────────────────
	// The node's initiator ACL entry has been revoked.  If the SM currently
	// tracks the volume as ControllerPublished (or any node-side state that
	// implies the controller had previously published it), revert to Created
	// so that subsequent operations require ControllerPublishVolume again.
	// We do not revert from StateNonExistent, StateCreated, or partial-create
	// states to avoid corrupting SM entries that were set up independently of
	// this ControllerUnpublishVolume call (e.g. stand-alone controller tests).
	switch s.sm.GetState(volumeID) {
	case StateControllerPublished,
		StateNodeStaged, StateNodePublished, StateNodeStagePartial:
		s.sm.ForceState(volumeID, StateCreated)
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerExpandVolume
// ─────────────────────────────────────────────────────────────────────────────.

// ControllerExpandVolume resizes a volume on the storage backend by delegating
// to agent.ExpandVolume.
//
// The method returns the actual capacity after expansion and sets
// node_expansion_required to true so that the CO will subsequently call
// NodeExpandVolume on the consuming node to resize the filesystem or
// re-detect the larger block device.
//
// Idempotency: ExpandVolume on the agent is idempotent — calling it with a
// requested_bytes ≤ current size is a no-op and returns the current size.
func (s *ControllerServer) ControllerExpandVolume(
	ctx context.Context,
	req *csi.ControllerExpandVolumeRequest,
) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetCapacityRange() == nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "capacity_range is required")
	}
	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes < 0 {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument,
			"capacity_range.required_bytes must not be negative")
	}

	// ── Parse the encoded volume ID ───────────────────────────────────────────
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return nil, status.Errorf(codes.InvalidArgument,
			"malformed volume_id %q: expected format <target>/<protocol>/<backend>/<vol-id>",
			volumeID)
	}
	targetName := parts[0]
	backendTypeStr := parts[2]
	agentVolID := parts[3]

	agentBackendType := mapBackendType(backendTypeStr)

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErrEV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrEV != nil {
		if k8serrors.IsNotFound(getTargetErrEV) {
			return nil, status.Errorf(codes.NotFound,
				"PillarTarget %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErrEV)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Expand the backend storage resource ───────────────────────────────────
	expandResp, expandErr := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       agentVolID,
		RequestedBytes: requiredBytes,
		BackendType:    agentBackendType,
	})
	if expandErr != nil {
		grpcSt, _ := status.FromError(expandErr)
		return nil, status.Errorf(grpcSt.Code(),
			"agent ExpandVolume(%q) failed: %v", agentVolID, expandErr)
	}

	actualBytes := expandResp.GetCapacityBytes()
	if actualBytes == 0 {
		// Agent did not report the new size; fall back to the requested value
		// so the CO can update the PVC status correctly.
		actualBytes = requiredBytes
	}

	// node_expansion_required=true instructs the CO to call NodeExpandVolume
	// so that the node can either:
	//   • rescan the NVMe-oF namespace to pick up the new block-device size, or
	//   • run resize2fs / xfs_growfs for mounted filesystems.
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         actualBytes,
		NodeExpansionRequired: true,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// GetCapacity
// ─────────────────────────────────────────────────────────────────────────────.

// GetCapacity returns the available storage capacity of the pool identified by
// the StorageClass parameters.
//
// The CO may call this before scheduling a PVC in order to pick a storage
// backend that has enough space.  Pillar-csi delegates the actual capacity
// query to the pillar-agent running on the storage node.
//
// Required StorageClass parameters:
//   - pillar-csi.bhyoo.com/target       — name of the PillarTarget
//   - pillar-csi.bhyoo.com/pool         — pool name on the storage node
//   - pillar-csi.bhyoo.com/backend-type — e.g. "zfs-zvol"
func (s *ControllerServer) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest,
) (*csi.GetCapacityResponse, error) {
	params := req.GetParameters()

	targetName := params[paramTarget]
	poolName := params[paramPool]
	backendTypeStr := params[paramBackendType]

	if targetName == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required for GetCapacity", paramTarget)
	}
	if poolName == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required for GetCapacity", paramPool)
	}
	if backendTypeStr == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"StorageClass parameter %q is required for GetCapacity", paramBackendType)
	}

	agentBackendType := mapBackendType(backendTypeStr)

	// ── Resolve the agent address from PillarTarget ───────────────────────────
	target := &v1alpha1.PillarTarget{}
	getTargetErrGC := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrGC != nil {
		if k8serrors.IsNotFound(getTargetErrGC) {
			return nil, status.Errorf(codes.NotFound,
				"PillarTarget %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarTarget %q: %v", targetName, getTargetErrGC)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarTarget %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Query pool capacity ───────────────────────────────────────────────────
	capResp, capErr := agentClient.GetCapacity(ctx, &agentv1.GetCapacityRequest{
		BackendType: agentBackendType,
		PoolName:    poolName,
	})
	if capErr != nil {
		grpcSt, _ := status.FromError(capErr)
		return nil, status.Errorf(grpcSt.Code(),
			"agent GetCapacity(pool=%q) failed: %v", poolName, capErr)
	}

	return &csi.GetCapacityResponse{
		AvailableCapacity: capResp.GetAvailableBytes(),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Network address helpers
// ─────────────────────────────────────────────────────────────────────────────.

// extractIP parses a "host:port" string and returns just the host component.
// If hostport does not contain a port (net.SplitHostPort returns an error) the
// input string is returned unchanged.
func extractIP(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
