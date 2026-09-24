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
// PillarStorageClass name; the controller uses the Kubernetes client to look up the
// corresponding PillarStore and PillarAgent, resolves the agent address from
// PillarAgent.Status.ResolvedAddress, and dials the agent via AgentDialer.
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
	"sync"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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

// The CSI controller is the only writer of PillarVolumeState (durable volume
// lifecycle state); no reconciler owns it, so its RBAC is declared here.
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates/status,verbs=get;update;patch

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

	// k8sClient is used to look up PillarAgent, PillarStore, and PillarStorageClass
	// resources to route volume operations to the correct storage node.
	// It is also used to create/update PillarVolumeState CRDs for durable
	// partial-failure state tracking.
	k8sClient client.Client

	// dialAgent creates an AgentServiceClient for the given agent address.
	// Injected at construction time; tests may supply a mock dialer.
	dialAgent AgentDialer

	// driverName is the CSI driver name announced in GetPluginInfo and
	// embedded in PersistentVolume.spec.csi.driver.
	driverName string

	// sm tracks the in-memory lifecycle state of every volume managed by
	// this controller instance.  It is initialized from persisted PillarVolumeState
	// CRDs at startup (via LoadStateFromPillarVolumeStates) and updated at each
	// lifecycle step.
	sm *VolumeStateMachine

	// volumeLocks serializes per-volume publication-record and ACL
	// read-modify-write sequences within this process; see volumeLockSet.
	volumeLocks *volumeLockSet

	// apiReader reads PillarVolumeState objects directly from the API server,
	// bypassing any informer cache.  Publication records must be read fresh:
	// a stale cached read could let ControllerUnpublishVolume miss a record
	// written moments earlier and leave the initiator's ACL granted.
	apiReader client.Reader
}

// Ensure ControllerServer satisfies the CSI interface at compile time.
var _ csi.ControllerServer = (*ControllerServer)(nil)

// NewControllerServer constructs a ControllerServer backed by the
// DefaultAgentDialer (plain-text gRPC, mTLS deferred to Phase 2).
//
// K8sClient must not be nil. APIReader must read uncached from the API server
// (controller-runtime Manager.GetAPIReader()). DriverName is typically
// "pillar-csi.bhyoo.com".
func NewControllerServer(k8sClient client.Client, apiReader client.Reader, driverName string) *ControllerServer {
	srv := NewControllerServerWithDialer(k8sClient, driverName, DefaultAgentDialer)
	srv.apiReader = apiReader
	return srv
}

// NewControllerServerWithDialer constructs a ControllerServer using the
// provided AgentDialer.  This variant is used in tests to inject a mock
// dialer that serves a real gRPC server backed by a mock agent.  K8sClient
// also serves as the uncached reader, so it must not be cache-backed.
func NewControllerServerWithDialer(
	k8sClient client.Client,
	driverName string,
	dialer AgentDialer,
) *ControllerServer {
	return &ControllerServer{
		k8sClient:   k8sClient,
		dialAgent:   dialer,
		driverName:  driverName,
		sm:          NewVolumeStateMachine(),
		volumeLocks: newVolumeLockSet(),
		apiReader:   k8sClient,
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

// LoadStateFromPillarVolumeStates restores the in-memory VolumeStateMachine from
// all PillarVolumeState CRDs currently persisted in the cluster.  This should be
// called once at controller startup so that the state machine reflects any
// partial-failure states that survived a controller restart.
//
// Errors listing the CRDs are returned; individual volumes with unknown phases
// are skipped with a warning rather than causing a fatal error, because the
// CO will retry any in-progress operations.
func (s *ControllerServer) LoadStateFromPillarVolumeStates(ctx context.Context) error {
	pvList := &v1alpha1.PillarVolumeStateList{}
	err := s.k8sClient.List(ctx, pvList)
	if err != nil {
		return fmt.Errorf("list PillarVolumeStates: %w", err)
	}
	for i := range pvList.Items {
		pv := &pvList.Items[i]
		state := pillarVolumeStatePhaseToVolumeState(pv.Status.Phase)
		if state == StateCreated && len(pv.Status.PublishedNodes) > 0 {
			state = StateControllerPublished
		}
		if state != StateNonExistent {
			s.sm.ForceState(pv.Spec.VolumeID, state)
		}
	}
	return nil
}

// pillarVolumeStatePhaseToVolumeState converts a PillarVolumeStatePhase to the
// corresponding in-memory VolumeState used by the state machine.
func pillarVolumeStatePhaseToVolumeState(phase v1alpha1.PillarVolumeStatePhase) VolumeState {
	switch phase {
	case v1alpha1.PillarVolumeStatePhaseCreatePartial:
		return StateCreatePartial
	case v1alpha1.PillarVolumeStatePhaseReady:
		return StateCreated
	case v1alpha1.PillarVolumeStatePhaseControllerPublished:
		return StateControllerPublished
	case v1alpha1.PillarVolumeStatePhaseNodeStagePartial:
		return StateNodeStagePartial
	case v1alpha1.PillarVolumeStatePhaseNodeStaged:
		return StateNodeStaged
	case v1alpha1.PillarVolumeStatePhaseNodePublished:
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

// baseSupportedAccessModes lists the VolumeCapability access modes pillar-csi
// supports for every protocol. File protocols add shared-writer modes on top
// of this baseline.
//
// Access-mode mapping (Kubernetes PVC → CSI constant):
//
//	ReadWriteOnce    (RWO)  → SINGLE_NODE_WRITER
//	ReadWriteOncePod (RWOP) → SINGLE_NODE_SINGLE_WRITER   (CSI spec v1.5+)
//	ReadOnlyMany     (ROX)  → MULTI_NODE_READER_ONLY
var baseSupportedAccessModes = []csi.VolumeCapability_AccessMode_Mode{
	// RWO: one node may mount read-write.
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	// RWO (K8s 1.35+): Kubernetes maps ReadWriteOnce to SINGLE_NODE_MULTI_WRITER.
	// Semantically equivalent to SINGLE_NODE_WRITER for block-protocol CSI drivers.
	csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
	// RWOP: one pod (on one node) may mount read-write.  Finer-grained than RWO.
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	// ROX: multiple nodes may mount read-only simultaneously.
	csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
}

// fileProtocolAdditionalAccessModes are only supported by file protocols. NFS
// and SMB can satisfy shared writer semantics; block protocols cannot.
var fileProtocolAdditionalAccessModes = []csi.VolumeCapability_AccessMode_Mode{
	csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
	csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
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
	ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities must not be empty")
	}

	// CSI spec §4.4: ValidateVolumeCapabilities MUST return NotFound when the
	// referenced volume does not exist.  We look up the PillarVolumeState CRD —
	// that record is the authoritative source for "does this driver own this
	// volume?".  A malformed volume_id (rejected by the same lookup as
	// not-found) is also treated as NotFound per the same paragraph: an ID
	// the driver did not issue cannot identify any volume it provisioned.
	existsErr := s.assertVolumeExists(ctx, req.GetVolumeId())
	if existsErr != nil {
		return nil, existsErr
	}

	protocolType := protocolTypeFromValidateVolumeCapabilitiesRequest(req)
	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"each volume capability must specify an access_mode")
		}
		if cap.GetBlock() != nil && isFileProtocol(protocolType) {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"raw block volume mode is not supported with file protocols (NFS/SMB)")
		}
		if !isSupportedAccessMode(protocolType, cap.GetAccessMode().GetMode()) {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf(
					"access mode %s is not supported for protocol %q; supported modes: %s",
					cap.GetAccessMode().GetMode(),
					protocolType,
					describeSupportedModes(protocolType),
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

func protocolTypeFromValidateVolumeCapabilitiesRequest(
	req *csi.ValidateVolumeCapabilitiesRequest,
) v1alpha1.ProtocolType {
	if parts := strings.SplitN(req.GetVolumeId(), "/", volumeIDParts); len(parts) == volumeIDParts {
		if protocolType := v1alpha1.ProtocolType(parts[1]); protocolType != "" {
			return protocolType
		}
	}
	if protocolType := v1alpha1.ProtocolType(req.GetVolumeContext()[vcProtocolType]); protocolType != "" {
		return protocolType
	}
	return v1alpha1.ProtocolType(req.GetParameters()[paramProtocolType])
}

// supportedAccessModesForProtocol returns the access modes supported for the
// given protocol. File protocols accept shared writer modes; block protocols
// do not.
func supportedAccessModesForProtocol(protocolType v1alpha1.ProtocolType) []csi.VolumeCapability_AccessMode_Mode {
	if !isFileProtocol(protocolType) {
		return baseSupportedAccessModes
	}

	modes := make([]csi.VolumeCapability_AccessMode_Mode, 0,
		len(baseSupportedAccessModes)+len(fileProtocolAdditionalAccessModes))
	modes = append(modes, baseSupportedAccessModes...)
	modes = append(modes, fileProtocolAdditionalAccessModes...)
	return modes
}

// isSupportedAccessMode returns true when mode is supported for the protocol.
func isSupportedAccessMode(
	protocolType v1alpha1.ProtocolType,
	mode csi.VolumeCapability_AccessMode_Mode,
) bool {
	return slices.Contains(supportedAccessModesForProtocol(protocolType), mode)
}

// describeSupportedModes returns a comma-separated string of the supported
// access mode names, used in diagnostic messages.
func describeSupportedModes(protocolType v1alpha1.ProtocolType) string {
	supportedModes := supportedAccessModesForProtocol(protocolType)
	names := make([]string, 0, len(supportedModes))
	for _, m := range supportedModes {
		names = append(names, m.String())
	}
	return strings.Join(names, ", ")
}

// ─────────────────────────────────────────────────────────────────────────────
// StorageClass / VolumeContext parameter key constants
// ─────────────────────────────────────────────────────────────────────────────.

const (
	// StorageClass parameter keys written by PillarStorageClassReconciler.
	paramPool         = "pillar-csi.bhyoo.com/store"
	paramBinding      = "pillar-csi.bhyoo.com/storage-class"
	paramProtocol     = "pillar-csi.bhyoo.com/protocol"
	paramBackendType  = "pillar-csi.bhyoo.com/backend-type"
	paramProtocolType = "pillar-csi.bhyoo.com/protocol-type"
	paramTarget       = "pillar-csi.bhyoo.com/agent"
	paramZFSParent    = "pillar-csi.bhyoo.com/zfs-parent-dataset"
	paramNVMeOFPort   = "pillar-csi.bhyoo.com/nvmeof-port"
	paramISCSIPort    = "pillar-csi.bhyoo.com/iscsi-port"
	paramNFSVersion   = "pillar-csi.bhyoo.com/nfs-version"
	paramLVMVG        = "pillar-csi.bhyoo.com/lvm-vg"

	// LVM provisioning mode parameter key for StorageClass/PillarStorageClass that selects
	// the LVM provisioning mode for new volumes.  Accepted values: "linear",
	// "thin".  When absent the LVM backend uses its compiled-in default (thin
	// when the backend was started with a thinpool= flag, linear otherwise).
	//
	// Layer 1 (PillarStore):   populated from LVMBackendConfig.ProvisioningMode.
	// Layer 3 (PillarStorageClass): overridden by LVMOverrides.ProvisioningMode.
	// Layer 4 (PVC annotation): highest-priority override via
	//   "pillar-csi.bhyoo.com/param.lvm-mode" PVC annotation.
	paramLVMMode = "pillar-csi.bhyoo.com/lvm-mode"

	// ParamACLEnabled controls NVMe-oF host NQN ACL enforcement.
	// Value: "true" (default, ACL enforced) or "false" (allow_any_host=1).
	// Set by the PillarStorageClass controller from the PillarProtocol NVMeOFTCPConfig.ACL field.
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
	vcTargetID = VolumeContextKeyTargetID // "target_id"
	vcAddress  = VolumeContextKeyAddress  // "address"
	vcPort     = VolumeContextKeyPort     // "port"

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

// CSINode annotation keys for protocol-specific initiator identity.
//
// These annotations are written by the node plugin at startup (pillar-node) and
// read by the controller during ControllerPublishVolume/ControllerUnpublishVolume
// to resolve the protocol-specific initiator identity.
//
// This decouples node_id (Kubernetes node name) from transport-level identifiers
// so that a single node can hold both an NQN and an IQN without conflating them
// with the stable node handle.
const (
	// AnnotationNVMeOFHostNQN is the CSINode annotation that stores the NVMe-oF
	// host NQN for this node (read from /etc/nvme/hostnqn).
	// Example value: "nqn.2014-08.org.nvmexpress:uuid:xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx".
	AnnotationNVMeOFHostNQN = "pillar-csi.bhyoo.com/nvmeof-host-nqn"

	// AnnotationISCSIInitiatorIQN is the CSINode annotation that stores the
	// iSCSI initiator IQN for this node (read from /etc/iscsi/initiatorname.iscsi).
	// Example value: "iqn.1993-08.org.debian:01:xxxxxxxx".
	AnnotationISCSIInitiatorIQN = "pillar-csi.bhyoo.com/iscsi-initiator-iqn"
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
	}

	// CSI spec §5.1.1 (CreateVolume Errors): a plugin that cannot create a
	// volume from the requested volume_content_source MUST return
	// INVALID_ARGUMENT.  pillar-csi does not advertise CREATE_DELETE_SNAPSHOT
	// or CLONE_VOLUME, so every content source (snapshot restore, volume
	// clone, or any future source type) is unsupported.  Rejecting here —
	// before the parameter merge, the PillarVolumeState lookup, and the
	// StateCreated cache check — prevents a silent empty-volume success and
	// prevents a same-name retry carrying a content source from being served
	// out of the idempotency cache.
	if req.GetVolumeContentSource() != nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument,
			"volume_content_source is not supported: this driver does not advertise "+
				"CREATE_DELETE_SNAPSHOT or CLONE_VOLUME")
	}

	scParams := req.GetParameters()

	// ── 4-level merge hierarchy: Pool → Protocol → Binding → PVC annotation ──
	// mergeParamsFromCRDs augments the StorageClass params with data fetched
	// live from the PillarStorageClass, PillarStore, and PillarProtocol CRDs and
	// then overlays any per-PVC annotation overrides.  Falls back gracefully
	// when the binding name is absent (e.g. manually-created StorageClasses).
	params, err := s.mergeParamsFromCRDs(ctx, scParams)
	if err != nil {
		// PVC annotation validation errors are user-facing (bad annotation
		// content); surface them as InvalidArgument so the CO can surface
		// a useful message to the user.  Infrastructure errors (CRD fetch
		// failures, etc.) are surfaced as Internal.
		if annotErr, ok := errors.AsType[*pvcAnnotationValidationError](err); ok {
			return nil, status.Errorf(codes.InvalidArgument,
				"PVC annotation validation failed: %v", annotErr)
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

	protocolType := v1alpha1.ProtocolType(protocolTypeStr)
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedAccessMode(protocolType, cap.GetAccessMode().GetMode()) {
			return nil, status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported for protocol %q; supported modes: %s",
				cap.GetAccessMode().GetMode(),
				protocolType,
				describeSupportedModes(protocolType))
		}
	}

	agentBackendType := mapBackendType(backendTypeStr)
	agentProtocolType := mapProtocolType(protocolTypeStr)

	// ── Build the agent-level volume ID ──────────────────────────────────────
	// For ZFS backends: "<pool>/<volume-name>" (pool = ZFS pool name from StorageClass params).
	// Fallback: "<pillar-pool-name>/<volume-name>".
	agentVolID := buildAgentVolumeID(params, req.GetName())

	// ── Build the CSI volume ID (encodes all routing metadata) ───────────────
	// Format: <target>/<protocol-type>/<backend-type>/<agent-vol-id>
	volumeID := strings.Join(
		[]string{targetName, protocolTypeStr, backendTypeStr, agentVolID},
		"/",
	)

	// ── Load persisted state (idempotency and partial-failure recovery) ───────
	// The PillarVolumeState CRD name is the CSI volume name, which is a
	// Kubernetes-compatible identifier assigned by the CO (e.g. "pvc-abc123").
	pvName := req.GetName()
	existingPV, pvExists, pvErr := s.loadPillarVolumeState(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to load PillarVolumeState %q: %v", pvName, pvErr)
	}
	if pvExists {
		// Restore the in-memory state machine entry from the persisted phase.
		s.sm.ForceState(volumeID, pillarVolumeStatePhaseToVolumeState(existingPV.Status.Phase))
	}
	// If the volume is already fully provisioned, return the cached response.
	if s.sm.GetState(volumeID) == StateCreated &&
		pvExists && existingPV.Status.ExportInfo != nil && !existingPV.Status.Deleting {
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

	// ── Requested capacity ────────────────────────────────────────────────────
	var capacityBytes int64
	if cr := req.GetCapacityRange(); cr != nil {
		capacityBytes = cr.GetRequiredBytes()
	}

	// ── Durable lifecycle before any agent call ──────────────────────────────
	// The PillarVolumeState is created first, so every backend resource an
	// agent ever creates belongs to a lifecycle (its UID) that DeleteVolume can
	// find and fence; a volume without a PillarVolumeState owns nothing.
	pvs, err := s.ensureVolumeState(ctx, pvName, v1alpha1.PillarVolumeStateSpec{
		VolumeID:      volumeID,
		AgentVolumeID: agentVolID,
		AgentRef:      targetName,
		BackendType:   backendTypeStr,
		ProtocolType:  protocolTypeStr,
		CapacityBytes: capacityBytes,
	})
	if err != nil {
		return nil, err
	}
	err = refuseDeleting(pvs, volumeID)
	if err != nil {
		return nil, err
	}

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErr := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErr != nil {
		if k8serrors.IsNotFound(getTargetErr) {
			return nil, status.Errorf(codes.NotFound,
				"PillarAgent %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarAgent %q: %v", targetName, getTargetErr)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Step 1: Create the backend storage resource ──────────────────────────
	// A lifecycle already in CreatePartial created its backend in an earlier
	// attempt whose export failed; the device path recorded then is reused and
	// only the export is retried, so a zvol that may hold data is never
	// re-created.
	devicePath := pvs.Status.BackendDevicePath
	actualCapacity := capacityBytes
	if pvs.Status.Phase == v1alpha1.PillarVolumeStatePhaseCreatePartial && devicePath != "" {
		if pvs.Spec.CapacityBytes > 0 {
			actualCapacity = pvs.Spec.CapacityBytes
		}
	} else {
		// The durable export spec is recorded with the partial state so the
		// resync controller can re-create the export after the storage node
		// loses its target state, independent of later parameter changes.
		exportSpec := exportSpecFor(
			buildExportParams(params, agentProtocolType, extractIP(agentAddr)),
			parseACLEnabled(params[paramACLEnabled]),
		)
		devicePath, actualCapacity, err = s.createBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			&agentv1.CreateVolumeRequest{
				VolumeId:      agentVolID,
				CapacityBytes: capacityBytes,
				BackendType:   agentBackendType,
				BackendParams: buildBackendParams(params, agentBackendType),
				AccessType:    accessTypeForBackend(agentBackendType),
			}, exportSpec)
		if err != nil {
			return nil, err
		}
	}

	// ── Step 2: Export the volume over the network protocol ───────────────────
	// The NVMe-oF / iSCSI bind address is the storage node's IP (no port).
	// agent.ExportVolume is idempotent: if the export already exists (retry
	// scenario), it returns the existing ExportInfo without error.
	exportToken, err := s.claimOperation(ctx, pvName, volumeID, pvs.UID)
	if err != nil {
		return nil, err
	}
	bindIP := extractIP(agentAddr)
	exportResp, err := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		ExportParams: buildExportParams(params, agentProtocolType, bindIP),
		DevicePath:   devicePath,
		AclEnabled:   parseACLEnabled(params[paramACLEnabled]),
		Fence:        exportToken,
	})
	if err != nil {
		// The PillarVolumeState records CreatePartial durably; the CO may
		// retry safely and the next attempt only re-exports.
		grpcSt, _ := status.FromError(err)
		return nil, status.Errorf(grpcSt.Code(),
			"agent ExportVolume(%q) failed: %v", agentVolID, err)
	}

	// ── Advance to fully-created state ────────────────────────────────────────
	s.sm.ForceState(volumeID, StateCreated)

	// Best-effort: mark the lifecycle Ready and cache the export parameters
	// for idempotent CreateVolume retries.  A failure here is not fatal — the
	// volume is provisioned and a retry re-exports idempotently.
	info := exportResp.GetExportInfo()
	//nolint:errcheck // best-effort CRD update; volume is already provisioned
	_ = s.persistVolumeReady(ctx, pvName, pvs.UID, info)

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

// createBackend commits a generation on the lifecycle uid, creates the
// backend storage resource with that fencing token, and records the
// CreatePartial state so a retry only re-exports.  It returns the device path
// and the allocated capacity; exportSpec is the durable export configuration
// persisted alongside the partial state for later resync.
func (s *ControllerServer) createBackend(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName, volumeID string,
	uid types.UID,
	req *agentv1.CreateVolumeRequest,
	exportSpec *v1alpha1.VolumeExportSpec,
) (devicePath string, capacity int64, err error) {
	req.Fence, err = s.claimOperation(ctx, pvName, volumeID, uid)
	if err != nil {
		return "", 0, err
	}
	resp, err := agentClient.CreateVolume(ctx, req)
	if err != nil {
		grpcSt, _ := status.FromError(err)
		return "", 0, status.Errorf(grpcSt.Code(),
			"agent CreateVolume(%q) failed: %v", req.GetVolumeId(), err)
	}
	capacity = req.GetCapacityBytes()
	if allocated := resp.GetCapacityBytes(); allocated != 0 {
		capacity = allocated
	}
	//nolint:errcheck // transition errors are non-fatal; state is force-set on success
	_, _ = s.sm.Transition(volumeID, OpCreateVolumeBackend)
	err = s.recordAllocatedCapacity(ctx, pvName, uid, capacity)
	if err != nil {
		return "", 0, err
	}
	err = s.persistCreatePartial(ctx, pvName, uid, resp.GetDevicePath(), exportSpec)
	if err != nil {
		// Cannot durably record the partial state; fail so the CO retries
		// instead of the backend resource being silently forgotten.
		return "", 0, err
	}
	return resp.GetDevicePath(), capacity, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DeleteVolume
// ─────────────────────────────────────────────────────────────────────────────.

// DeleteVolume deprovisions a volume by orchestrating two agent RPCs.
//
// Lifecycle (CSI spec §4.3.2):
//  0. Commit status.deleting (with a fresh fencing generation) on the
//     volume's PillarVolumeState; this succeeds only while no publication is
//     recorded, and blocks every later publish, create, or export.
//  1. Call agent.UnexportVolume — removes the network-protocol export entry.
//  2. Call agent.DeleteVolume — destroys the backend storage resource and
//     ends the lifecycle at the agent.
//  3. Delete the PillarVolumeState (UID-preconditioned).
//
// Both agent RPCs carry the deletion's fencing token, so a delayed delete
// from a former controller cannot touch a volume that a newer operation or a
// re-created lifecycle owns.  A volume without a PillarVolumeState owns
// nothing (CreateVolume creates the record before any backend resource), so
// it is deleted without any agent call.  A missing PillarAgent object does not
// prove the node's resources are gone, so the delete fails closed
// (FailedPrecondition, record kept) until the agent is reachable.
//
// A volume whose PillarVolumeState still records a publication is not
// deleted; FailedPrecondition is returned until every node is unpublished.
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

	pvName := pillarVolumeStateNameFromVolumeID(volumeID)
	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// CSI: a volume still published to a node must not be deleted.  The
	// deleting flag is committed by compare-and-swap only while no
	// publication is recorded, so a concurrent publish on another controller
	// is rejected instead of racing the deletion.
	pvs, fence, err := s.markVolumeDeleting(ctx, pvName, volumeID)
	if err != nil {
		return nil, err
	}
	if pvs == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErrDV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrDV != nil {
		if !k8serrors.IsNotFound(getTargetErrDV) {
			return nil, status.Errorf(codes.Internal,
				"failed to get PillarAgent %q: %v", targetName, getTargetErrDV)
		}
		// A missing PillarAgent object does not prove the node's backend and
		// target are gone, and without the agent the lifecycle cannot be
		// ended durably.  Keep the record (marked deleting) and fail closed;
		// the CO retries until the agent is reachable again.
		return nil, status.Errorf(codes.FailedPrecondition,
			"PillarAgent %q not found; cannot confirm deletion of volume %q on its storage node",
			targetName, volumeID)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		// Target exists but has no address yet.  This is a transient state;
		// return Unavailable so the CO will retry.
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Step 1: Remove the network export (idempotent) ────────────────────────
	_, unexportErr := agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		Fence:        fence,
	})
	unexportCode := status.Code(unexportErr)
	if unexportErr != nil && unexportCode != codes.NotFound {
		return nil, status.Errorf(unexportCode,
			"agent UnexportVolume(%q) failed: %v", agentVolID, unexportErr)
	}

	// ── Step 2: Destroy the backend storage resource (idempotent) ─────────────
	// Only a successful deletion ends the lifecycle at the agent; the record
	// is kept on any failure so the CO retries with the same token.
	_, deleteErr := agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId:    agentVolID,
		BackendType: agentBackendType,
		Fence:       fence,
	})
	if deleteErr != nil {
		st, _ := status.FromError(deleteErr)
		return nil, status.Errorf(st.Code(),
			"agent DeleteVolume(%q) failed: %v", agentVolID, deleteErr)
	}

	return s.finishDelete(ctx, volumeID, pvName, pvs.UID)
}

// finishDelete forgets the volume in memory and removes the lifecycle's
// PillarVolumeState.  A failure is returned so the CO retries: the retry
// finds the record still marked deleting and repeats the idempotent steps.
func (s *ControllerServer) finishDelete(
	ctx context.Context,
	volumeID, pvName string,
	uid types.UID,
) (*csi.DeleteVolumeResponse, error) {
	err := s.deleteVolumeState(ctx, pvName, uid)
	if err != nil {
		return nil, err
	}
	s.sm.ForceState(volumeID, StateNonExistent)
	return &csi.DeleteVolumeResponse{}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PillarVolumeState CRD helpers (partial-failure state persistence)
// ─────────────────────────────────────────────────────────────────────────────.

// loadPillarVolumeState returns the PillarVolumeState CRD for the given volume name,
// along with a boolean indicating whether it was found.  A nil k8sClient or
// a NotFound error are treated as "not found" (non-error).
func (s *ControllerServer) loadPillarVolumeState(
	ctx context.Context,
	pvName string,
) (*v1alpha1.PillarVolumeState, bool, error) {
	if s.k8sClient == nil {
		return nil, false, nil
	}
	pv := &v1alpha1.PillarVolumeState{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get PillarVolumeState %q: %w", pvName, err)
	}
	return pv, true, nil
}

// pillarVolumeStateNameFromVolumeID extracts the PillarVolumeState object name from an
// encoded CSI volume_id "<target>/<protocol>/<backend>/<pool>/<pv-name>"
// (or the trailing "<pool>/<pv-name>" segment when the leading parts are
// already stripped).  Returns "" when the volume_id does not parse, signaling
// "this is not a volume_id this driver issued".
func pillarVolumeStateNameFromVolumeID(volumeID string) string {
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return ""
	}
	agentVolID := parts[3]
	if idx := strings.LastIndex(agentVolID, "/"); idx >= 0 {
		return agentVolID[idx+1:]
	}
	return agentVolID
}

// assertVolumeExists returns a gRPC NotFound error when no PillarVolumeState CRD
// records the given volume_id, satisfying CSI spec §4.4 / §4.5 / §4.6 / §4.7
// for ValidateVolumeCapabilities, ControllerPublishVolume, and the equivalent
// node-side checks that mandate NotFound for unknown volumes.  A malformed
// volume_id is treated identically because the driver provably never issued
// it.  The function returns nil when the volume exists or when the client is
// nil (fake clients used in low-level unit tests skip CRD plumbing).
func (s *ControllerServer) assertVolumeExists(ctx context.Context, volumeID string) error {
	if s.k8sClient == nil {
		return nil
	}
	pvName := pillarVolumeStateNameFromVolumeID(volumeID)
	if pvName == "" {
		return status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	_, exists, err := s.loadPillarVolumeState(ctx, pvName)
	if err != nil {
		return status.Errorf(codes.Internal, "lookup PillarVolumeState %q: %v", pvName, err)
	}
	if !exists {
		return status.Errorf(codes.NotFound, "volume %q not found", volumeID)
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
//	Layer 1 (Pool)    – ZFS properties from PillarStore.spec.backend.zfs.properties
//	Layer 2 (Protocol)– (protocol params already captured in StorageClass at bind time)
//	Layer 3 (Binding) – ZFS property overrides from PillarStorageClass.spec.overrides.backend.zfs
//	Layer 4 (PVC)     – per-PVC annotation overrides prefixed with pvcAnnotationParamPrefix
//
// The StorageClass parameters (scParams) are the authoritative source for
// routing metadata (target, backend-type, protocol-type, etc.) and serve as
// the baseline.  ZFS properties (which are not stored in the StorageClass)
// are fetched from the PillarStore CRD and layered on top, then Binding
// overrides are applied, and finally per-PVC annotation overrides win over
// everything else.
//
// When the binding name is absent from scParams the function returns a shallow
// copy of scParams unchanged, preserving backward compatibility with
// manually-crafted StorageClasses that do not reference a PillarStorageClass.
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

	// ── Fetch PillarStorageClass ───────────────────────────────────────────────────
	binding := &v1alpha1.PillarStorageClass{}
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
		return nil, fmt.Errorf("fetch PillarStorageClass %q: %w", bindingName, err)
	}

	// ── Layer 1: fetch PillarStore and apply ZFS properties ───────────────────
	pool := &v1alpha1.PillarStore{}
	err = s.k8sClient.Get(ctx, types.NamespacedName{Name: binding.Spec.StoreRef}, pool)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			err2 := s.applyPVCAnnotationOverrides(ctx, merged, scParams)
			if err2 != nil {
				return nil, err2
			}
			return merged, nil
		}
		return nil, fmt.Errorf("fetch PillarStore %q: %w", binding.Spec.StoreRef, err)
	}

	// Apply Pool-level ZFS properties as the lowest-priority ZFS property layer.
	if pool.Spec.Backend.ZFS != nil {
		for k, v := range pool.Spec.Backend.ZFS.Properties {
			merged[paramZFSPropPrefix+k] = v
		}
	}

	// Apply Pool-level LVM provisioning mode as the lowest-priority LVM mode layer.
	// Only set when a non-empty mode is configured AND the StorageClass has not
	// already supplied an explicit override.  An absent key in the final merged
	// map lets the agent backend use its compiled-in default.
	if pool.Spec.Backend.LVM != nil && pool.Spec.Backend.LVM.ProvisioningMode != "" {
		if _, alreadySet := merged[paramLVMMode]; !alreadySet {
			merged[paramLVMMode] = string(pool.Spec.Backend.LVM.ProvisioningMode)
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

	// Apply Binding-level LVM provisioning mode override (Layer 3).
	// A non-empty value here wins over the Pool-level default applied above.
	if binding.Spec.Overrides != nil &&
		binding.Spec.Overrides.Backend != nil &&
		binding.Spec.Overrides.Backend.LVM != nil &&
		binding.Spec.Overrides.Backend.LVM.ProvisioningMode != "" {
		merged[paramLVMMode] = string(binding.Spec.Overrides.Backend.LVM.ProvisioningMode)
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
// The format is "<pool>/<volume-name>" where pool is the storage pool name
// from the pillar-csi.bhyoo.com/store StorageClass parameter.  For ZFS
// backends this matches the agent's internal naming convention
// (/dev/zvol/<pool>/<name>); for other backends it is the pool name passed
// to --backend type=<t>,pool=<name>.
func buildAgentVolumeID(params map[string]string, volumeName string) string {
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
	switch v1alpha1.BackendType(s) {
	case v1alpha1.BackendTypeZFSZvol:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
	case v1alpha1.BackendTypeZFSDataset:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	case v1alpha1.BackendTypeLVMLV:
		return agentv1.BackendType_BACKEND_TYPE_LVM
	case v1alpha1.BackendTypeDir:
		return agentv1.BackendType_BACKEND_TYPE_DIRECTORY
	default:
		return agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}

// resolveInitiatorID looks up the protocol-specific initiator identity for the
// given Kubernetes node by reading the appropriate CSINode annotation.
//
// Identity resolution by protocol:
//
//	NVMe-oF TCP → CSINode.annotations["pillar-csi.bhyoo.com/nvmeof-host-nqn"]
//	iSCSI       → CSINode.annotations["pillar-csi.bhyoo.com/iscsi-initiator-iqn"]
//	NFS/SMB     → nodeID (annotation-based resolution is a future Phase 2 item)
//
// Returns FailedPrecondition if the CSINode does not exist or the required
// annotation is absent.  This causes the CO (external-attacher) to retry with
// exponential backoff, giving the node plugin time to publish its identity
// after a fresh node bootstrap.
func (s *ControllerServer) resolveInitiatorID(ctx context.Context, nodeID, protocolTypeStr string) (string, error) {
	var annotationKey string
	switch v1alpha1.ProtocolType(protocolTypeStr) {
	case v1alpha1.ProtocolTypeNVMeOFTCP:
		annotationKey = AnnotationNVMeOFHostNQN
	case v1alpha1.ProtocolTypeISCSI:
		annotationKey = AnnotationISCSIInitiatorIQN
	default:
		// NFS, SMB and unknown protocols: initiator identity is not stored in a
		// CSINode annotation in Phase 1.  Return nodeID as-is so that the caller
		// can pass it unchanged to AllowInitiator/DenyInitiator.
		return nodeID, nil
	}

	csiNode := &storagev1.CSINode{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: nodeID}, csiNode)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", status.Errorf(codes.FailedPrecondition,
				"CSINode %q not found; node plugin may not have registered yet", nodeID)
		}
		return "", status.Errorf(codes.Internal,
			"failed to get CSINode %q: %v", nodeID, err)
	}

	initiatorID := csiNode.Annotations[annotationKey]
	if initiatorID == "" {
		return "", status.Errorf(codes.FailedPrecondition,
			"CSINode %q is missing annotation %q; node plugin may not have published its identity yet",
			nodeID, annotationKey)
	}

	return initiatorID, nil
}

// mapProtocolType converts the StorageClass protocol-type string to the agent
// protobuf enum value.
func mapProtocolType(s string) agentv1.ProtocolType {
	switch v1alpha1.ProtocolType(s) {
	case v1alpha1.ProtocolTypeNVMeOFTCP:
		return agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP
	case v1alpha1.ProtocolTypeISCSI:
		return agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI
	case v1alpha1.ProtocolTypeNFS:
		return agentv1.ProtocolType_PROTOCOL_TYPE_NFS
	case v1alpha1.ProtocolTypeSMB:
		return agentv1.ProtocolType_PROTOCOL_TYPE_SMB
	default:
		return agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED
	}
}

// accessTypeForBackend returns the VolumeAccessType to request from the agent
// for the given storage backend.
//
// Filesystem backends (ZFS datasets, directories) produce mounted filesystems,
// so the agent creates a MOUNT resource. Block-oriented backends (zvols, LVM
// LVs, and future raw block backends) produce block devices, so the agent
// creates a BLOCK resource.
func accessTypeForBackend(bt agentv1.BackendType) agentv1.VolumeAccessType {
	switch bt {
	case agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, agentv1.BackendType_BACKEND_TYPE_DIRECTORY:
		return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_MOUNT
	default:
		return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_BLOCK
	}
}

// isFileProtocol reports whether the given ProtocolType is a file-based
// (network filesystem) protocol.  File protocols (NFS, SMB) handle resize
// entirely on the server side; NodeExpandVolume is not needed.
func isFileProtocol(p v1alpha1.ProtocolType) bool {
	switch p {
	case v1alpha1.ProtocolTypeNFS, v1alpha1.ProtocolTypeSMB:
		return true
	default:
		return false
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
// These originate from PillarStore.spec.backend.zfs.properties (Layer 1),
// PillarStorageClass.spec.overrides.backend.zfs.properties (Layer 3), or per-PVC
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
					Pool:          params[paramPool],
					ParentDataset: params[paramZFSParent],
					Properties:    zfsProps,
				},
			},
		}
	case agentv1.BackendType_BACKEND_TYPE_LVM:
		return &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{
					VolumeGroup:   params[paramLVMVG],
					ProvisionMode: params[paramLVMMode],
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
// PillarAgent.Status.ResolvedAddress with the ":port" suffix stripped.
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
// The parameter is written by the PillarStorageClass controller using the value of
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
// Publication records (CSI publish exclusivity)
// ─────────────────────────────────────────────────────────────────────────────.

// volumeLockSet serializes, per volume ID, the controller operations that
// read-modify-write a volume's durable publication record
// (PillarVolumeState.status.publishedNodes) together with the storage-target
// ACL derived from it: ControllerPublishVolume, ControllerUnpublishVolume and
// DeleteVolume.  Any other in-process writer of the target ACL for a volume
// must hold the same lock.  The lock only removes in-process interleavings.
// Across controller processes (a former leader whose RPC is still in flight)
// the resourceVersion compare-and-swap orders the durable record, and the
// publicationGeneration fencing token it commits orders the agent RPCs: the
// agent rejects any request older than the last one it applied.
type volumeLockSet struct {
	mu    sync.Mutex
	locks map[string]*volumeLock
}

// volumeLock is one reference-counted per-volume mutex.
type volumeLock struct {
	mu   sync.Mutex
	refs int
}

func newVolumeLockSet() *volumeLockSet {
	return &volumeLockSet{locks: make(map[string]*volumeLock)}
}

// lock blocks until the caller holds the lock for volumeID and returns the
// function that releases it.  Idle entries are removed so the set does not
// grow with the number of volumes ever seen.
func (l *volumeLockSet) lock(volumeID string) (unlock func()) {
	l.mu.Lock()
	vl, ok := l.locks[volumeID]
	if !ok {
		vl = &volumeLock{}
		l.locks[volumeID] = vl
	}
	vl.refs++
	l.mu.Unlock()

	vl.mu.Lock()
	return func() {
		vl.mu.Unlock()
		l.mu.Lock()
		vl.refs--
		if vl.refs == 0 {
			delete(l.locks, volumeID)
		}
		l.mu.Unlock()
	}
}

// readVolumeState returns the PillarVolumeState for pvName read uncached
// through apiReader, and whether it exists.  Publication records are
// authoritative for publish exclusivity and ACL revocation and must never be
// decided on a stale informer-cache copy.
func (s *ControllerServer) readVolumeState(
	ctx context.Context,
	pvName string,
) (*v1alpha1.PillarVolumeState, bool, error) {
	pvs := &v1alpha1.PillarVolumeState{}
	err := s.apiReader.Get(ctx, types.NamespacedName{Name: pvName}, pvs)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get PillarVolumeState %q: %w", pvName, err)
	}
	return pvs, true, nil
}

// canSharePublication reports whether two publications of the same volume on
// different nodes are compatible under CSI access-mode semantics:
//   - SINGLE_NODE_* modes never share a volume with another node;
//   - MULTI_NODE_READER_ONLY and MULTI_NODE_MULTI_WRITER share with the same mode;
//   - MULTI_NODE_SINGLE_WRITER shares with the same mode when at most one of
//     the two publications is writable.
//
// Publications with different access modes are never compatible.
func canSharePublication(a, b v1alpha1.VolumePublication) bool {
	if a.AccessMode != b.AccessMode {
		return false
	}
	switch a.AccessMode {
	case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY.String(),
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER.String():
		return true
	case csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER.String():
		return a.Readonly || b.Readonly
	default:
		return false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerPublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// validatePublishAccessMode rejects an access mode the volume's protocol
// cannot serve (for example a multi-node writer mode on a block protocol).
func validatePublishAccessMode(protocolTypeStr string, mode csi.VolumeCapability_AccessMode_Mode) error {
	protocolType := v1alpha1.ProtocolType(protocolTypeStr)
	if isSupportedAccessMode(protocolType, mode) {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"access mode %s is not supported for protocol %q; supported modes: %s",
		mode, protocolType, describeSupportedModes(protocolType))
}

// resolvePublishInitiator resolves the node's protocol initiator identity for
// ControllerPublishVolume.  CSI spec §4.5.1: an unknown node_id must return
// NotFound.  The resolveInitiatorID helper returns FailedPrecondition when the CSINode
// object is missing (used elsewhere to signal "retry later, the node plugin
// is still registering"); for publish that one signal is promoted to NotFound
// while other failure modes (annotation present but blank) stay
// FailedPrecondition.
func (s *ControllerServer) resolvePublishInitiator(
	ctx context.Context,
	nodeID, protocolTypeStr string,
) (string, error) {
	initiatorID, err := s.resolveInitiatorID(ctx, nodeID, protocolTypeStr)
	if err == nil {
		return initiatorID, nil
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition &&
		strings.Contains(st.Message(), "node plugin may not have registered yet") {
		return "", status.Errorf(codes.NotFound, "node %q not found", nodeID)
	}
	return "", err
}

// ControllerPublishVolume grants a specific node access to a volume by
// calling agent.AllowInitiator on the storage node.
//
// The node_id in the request is the Kubernetes node name (stable handle).
// The protocol-specific initiator identity is resolved by reading the
// appropriate CSINode annotation that was written by the node plugin at
// startup:
//
//	NVMe-oF TCP → CSINode["pillar-csi.bhyoo.com/nvmeof-host-nqn"] (host NQN)
//	iSCSI       → CSINode["pillar-csi.bhyoo.com/iscsi-initiator-iqn"] (IQN)
//	NFS/SMB     → nodeID used as-is (annotation-based resolution is Phase 2)
//
// If the required CSINode annotation is absent, FailedPrecondition is returned
// and the CO (external-attacher) retries with exponential backoff, giving the
// node plugin time to publish its identity after a fresh node bootstrap.
//
// Exclusivity (CSI spec ControllerPublishVolume errors): the publication is
// recorded in PillarVolumeState.status.publishedNodes before the agent grants
// access.  A publish that is incompatible with a publication on another node
// (any SINGLE_NODE_* mode, differing modes, or a second writer for
// MULTI_NODE_SINGLE_WRITER) returns FailedPrecondition; the same node with a
// different capability returns AlreadyExists.  If AllowInitiator fails the
// record is kept (fail-closed) until the CO retries or unpublishes.
//
// Idempotency: an identical publish for an already recorded node succeeds and
// re-applies AllowInitiator, which is idempotent on the agent side.
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
	// CSI spec §4.5.1: a malformed or non-existent volume_id must return
	// NotFound, not InvalidArgument.  A volume_id that does not match this
	// driver's encoded format provably cannot identify any volume that this
	// driver provisioned, so it is treated as "not found" rather than "bad
	// input" — matching the behavior csi-sanity expects.
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return nil, status.Errorf(codes.NotFound,
			"volume %q not found (unknown volume_id format)", volumeID)
	}
	targetName := parts[0]
	protocolTypeStr := parts[1]
	agentVolID := parts[3]

	mode := req.GetVolumeCapability().GetAccessMode().GetMode()
	modeErr := validatePublishAccessMode(protocolTypeStr, mode)
	if modeErr != nil {
		return nil, modeErr
	}

	agentProtocolType := mapProtocolType(protocolTypeStr)

	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// ── The volume must exist (CSI: NotFound for an unknown volume) ──────────
	pvName := pillarVolumeStateNameFromVolumeID(volumeID)
	pvs, pvExists, pvErr := s.readVolumeState(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal, "%v", pvErr)
	}
	if !pvExists {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	agentAddr, addrErr := s.resolveAgentAddress(ctx, targetName)
	if addrErr != nil {
		return nil, addrErr
	}

	// ── Resolve initiator identity from CSINode annotation ───────────────────
	initiatorID, resolveErr := s.resolvePublishInitiator(ctx, nodeID, protocolTypeStr)
	if resolveErr != nil {
		return nil, resolveErr
	}

	// ── Record the publication before granting access ────────────────────────
	// The committed token orders the grant: a stale controller whose
	// reservation was superseded (or whose lifecycle was replaced) is rejected
	// by the agent even if its AllowInitiator lands late.
	fence, reserveErr := s.reservePublication(ctx, pvName, volumeID, pvs.UID, v1alpha1.VolumePublication{
		NodeID:      nodeID,
		InitiatorID: initiatorID,
		AccessMode:  mode.String(),
		Readonly:    req.GetReadonly(),
	})
	if reserveErr != nil {
		return nil, reserveErr
	}

	grantErr := s.grantPublication(ctx, agentAddr, agentVolID, agentProtocolType, initiatorID, fence)
	if grantErr != nil {
		return nil, grantErr
	}

	// ── Advance state machine to ControllerPublished ─────────────────────────
	// The durable publication record above is authoritative for exclusivity;
	// the in-memory state machine only mirrors that at least one node is
	// published.  ForceState is used because CreateVolume may have run in a
	// previous controller process.
	s.sm.ForceState(volumeID, StateControllerPublished)

	// PublishContext is forwarded to NodeStageVolume.  No additional keys are
	// required here; the volume connection parameters are already stored in
	// the PersistentVolume's VolumeContext by CreateVolume.
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{},
	}, nil
}

// Grant access using the protocol-specific identity resolved from CSINode.
func (s *ControllerServer) grantPublication(
	ctx context.Context,
	agentAddr string,
	agentVolID string,
	agentProtocolType agentv1.ProtocolType,
	initiatorID string,
	fence *agentv1.FencingToken,
) error {
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	allowResp, allowErr := agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		InitiatorId:  initiatorID,
		Fence:        fence,
	})
	_ = allowResp
	if allowErr != nil {
		grpcSt, _ := status.FromError(allowErr)
		return status.Errorf(grpcSt.Code(),
			"agent AllowInitiator(%q, initiator=%q) failed: %v",
			agentVolID, initiatorID, allowErr)
	}
	return nil
}

// resolveAgentAddress returns the resolved address of the PillarAgent
// targetName: NotFound when the object does not exist, Unavailable while it
// has no address yet, Internal on any other API error.
func (s *ControllerServer) resolveAgentAddress(ctx context.Context, targetName string) (string, error) {
	target := &v1alpha1.PillarAgent{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", status.Errorf(codes.NotFound, "PillarAgent %q not found", targetName)
		}
		return "", status.Errorf(codes.Internal, "failed to get PillarAgent %q: %v", targetName, err)
	}
	if target.Status.ResolvedAddress == "" {
		return "", status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address; agent may not be ready", targetName)
	}
	return target.Status.ResolvedAddress, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerUnpublishVolume
// ─────────────────────────────────────────────────────────────────────────────.

// ControllerUnpublishVolume revokes node access to a volume by calling
// agent.DenyInitiator for every matching publication recorded in the volume's
// PillarVolumeState, then removing those records.
//
// The initiator identity is taken from the publication record, not from the
// CSINode object, so a node that has been deleted is still revoked.  An empty
// node_id revokes every recorded publication (CSI spec §4.5.2).  A record is
// removed only after DenyInitiator succeeded, so a failed revocation keeps
// the volume unavailable to other SINGLE_NODE_* publishers (fail-closed).
//
// Idempotency:
//   - If the volume has no PillarVolumeState or no matching publication,
//     there is nothing to revoke; return success.
//   - If the PillarAgent object is missing, the node's ACL entries may still
//     exist; the records are kept and FailedPrecondition is returned.
//   - If the agent returns NotFound for DenyInitiator, the ACL entry was
//     already absent.
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

	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// ── Select the publications to revoke ────────────────────────────────────
	pvName := pillarVolumeStateNameFromVolumeID(volumeID)
	existingPV, pvExists, pvErr := s.readVolumeState(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal, "%v", pvErr)
	}
	if !pvExists {
		// The volume does not exist; no access can have been granted.
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if !hasPublicationFor(existingPV.Status.PublishedNodes, nodeID) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErrCUV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrCUV != nil {
		if !k8serrors.IsNotFound(getTargetErrCUV) {
			return nil, status.Errorf(codes.Internal,
				"failed to get PillarAgent %q: %v", targetName, getTargetErrCUV)
		}
		// A missing PillarAgent object does not prove the node's ACL entries
		// are gone; dropping the records would let another node be granted
		// while the old grant may still exist.  Keep them and fail closed.
		return nil, status.Errorf(codes.FailedPrecondition,
			"PillarAgent %q not found; cannot revoke volume %q on its storage node",
			targetName, volumeID)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		// Transient; CO will retry.
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Revoke initiator access (idempotent), then drop the records ──────────
	remaining, revokeErr := s.revokePublications(ctx, agentClient, agentVolID,
		agentProtocolType, pvName, existingPV.UID, nodeID)
	if revokeErr != nil {
		return nil, revokeErr
	}

	s.syncUnpublishedState(volumeID, remaining)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// hasPublicationFor reports whether ControllerUnpublishVolume has anything to
// revoke: a publication for nodeID, or any publication when nodeID is empty
// (CSI §4.5.2).
func hasPublicationFor(pubs []v1alpha1.VolumePublication, nodeID string) bool {
	return slices.ContainsFunc(pubs, func(pub v1alpha1.VolumePublication) bool {
		return nodeID == "" || pub.NodeID == nodeID
	})
}

// revokePublications revokes the publications of nodeID (every publication
// when nodeID is empty) on the lifecycle uid and returns how many remain.
//
// Ordering is load-bearing:
//  1. fencePublications selects the records, marks them revoking, and
//     commits the fencing token in one compare-and-swap, so the selection is
//     exactly the state the token was committed for.
//  2. DenyInitiator runs with that token; the agent applies the revoke and
//     rejects any grant still in flight from an earlier generation.
//  3. releasePublication drops the records only while the generation is
//     still the fence's and only after every revoke succeeded, so neither a
//     newer re-publish nor a crash can leave an unrecorded grant (fail-closed).
func (s *ControllerServer) revokePublications(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	agentVolID string,
	protocolType agentv1.ProtocolType,
	pvName string,
	uid types.UID,
	nodeID string,
) (remaining int, err error) {
	fence, revoke, err := s.fencePublications(ctx, pvName, uid, nodeID)
	if err != nil {
		return 0, err
	}
	revokedNodes := make([]string, 0, len(revoke))
	for _, pub := range revoke {
		_, denyErr := agentClient.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
			VolumeId:     agentVolID,
			ProtocolType: protocolType,
			InitiatorId:  pub.InitiatorID,
			Fence:        fence,
		})
		// NotFound → ACL entry already absent; success.
		denyCode := status.Code(denyErr)
		if denyErr != nil && denyCode != codes.NotFound {
			return 0, status.Errorf(denyCode,
				"agent DenyInitiator(%q, node=%q, initiator=%q) failed: %v",
				agentVolID, pub.NodeID, pub.InitiatorID, denyErr)
		}
		revokedNodes = append(revokedNodes, pub.NodeID)
	}
	// With nothing selected, releasePublication commits nothing and only
	// reports the current count.
	return s.releasePublication(ctx, pvName, uid, revokedNodes, fence)
}

// syncUnpublishedState reverts the in-memory state machine to Created once no
// publication remains, so that node operations require ControllerPublishVolume
// again.  While other nodes still hold the volume it stays published.  States
// that were not reached through ControllerPublishVolume are left untouched.
func (s *ControllerServer) syncUnpublishedState(volumeID string, remaining int) {
	if remaining > 0 {
		return
	}
	switch s.sm.GetState(volumeID) {
	case StateControllerPublished,
		StateNodeStaged, StateNodePublished, StateNodeStagePartial:
		s.sm.ForceState(volumeID, StateCreated)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ControllerExpandVolume
// ─────────────────────────────────────────────────────────────────────────────.

// ControllerExpandVolume resizes a volume on the storage backend by delegating
// to agent.ExpandVolume.
//
// The method returns the actual capacity after expansion. For block protocols
// (nvmeof-tcp, iscsi) node_expansion_required is set to true so that the CO
// will subsequently call NodeExpandVolume to rescan the block device and
// resize the filesystem. For file protocols (nfs, smb) node_expansion_required
// is set to false because the resize is fully server-side.
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
	protocolTypeStr := parts[1]
	backendTypeStr := parts[2]
	agentVolID := parts[3]

	agentBackendType := mapBackendType(backendTypeStr)

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErrEV := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrEV != nil {
		if k8serrors.IsNotFound(getTargetErrEV) {
			return nil, status.Errorf(codes.NotFound,
				"PillarAgent %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarAgent %q: %v", targetName, getTargetErrEV)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address; agent may not be ready", targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Expand the backend storage resource ───────────────────────────────────
	// The request carries the lifecycle's current token: an expand of a
	// deleted, deleting, or re-created volume is refused here or by the agent.
	fence, err := s.currentToken(ctx, pillarVolumeStateNameFromVolumeID(volumeID), volumeID)
	if err != nil {
		return nil, err
	}
	expandResp, expandErr := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       agentVolID,
		RequestedBytes: requiredBytes,
		BackendType:    agentBackendType,
		Fence:          fence,
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

	// File-protocol volumes (NFS, SMB) are resized entirely on the server side;
	// the node does not need to rescan a block device or grow a filesystem.
	// Block-protocol volumes (nvmeof-tcp, iscsi) require a node-side rescan
	// to pick up the new block-device size and optionally run resize2fs/xfs_growfs.
	nodeExpansionRequired := !isFileProtocol(v1alpha1.ProtocolType(protocolTypeStr))

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         actualBytes,
		NodeExpansionRequired: nodeExpansionRequired,
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
//   - pillar-csi.bhyoo.com/agent       — name of the PillarAgent
//   - pillar-csi.bhyoo.com/store         — pool name on the storage node
//   - pillar-csi.bhyoo.com/backend-type — e.g. "zfs-zvol"
func (s *ControllerServer) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest,
) (*csi.GetCapacityResponse, error) {
	params := req.GetParameters()

	targetName := params[paramTarget]
	poolName := params[paramPool]
	backendTypeStr := params[paramBackendType]

	// CSI spec §4.1.2: GetCapacity MAY be called with empty parameters and
	// MUST NOT fail in that case — it should return zero available capacity
	// so the CO knows no pool is selectable.  Returning InvalidArgument here
	// breaks csi-sanity's "no optional values added" test and is wrong per
	// the spec; the parameters field is informational, not validated.
	if targetName == "" || poolName == "" || backendTypeStr == "" {
		return &csi.GetCapacityResponse{AvailableCapacity: 0}, nil
	}

	agentBackendType := mapBackendType(backendTypeStr)

	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErrGC := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if getTargetErrGC != nil {
		if k8serrors.IsNotFound(getTargetErrGC) {
			return nil, status.Errorf(codes.NotFound,
				"PillarAgent %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to get PillarAgent %q: %v", targetName, getTargetErrGC)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address; agent may not be ready", targetName)
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
