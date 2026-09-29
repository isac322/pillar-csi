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
	"fmt"
	"io"
	"maps"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

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
// ReapAbandonedVolume reads PersistentVolumes and PersistentVolumeClaims to
// decide whether a provisioning attempt was abandoned.
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch

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

// supportedAccessModes lists the VolumeCapability access modes pillar-csi
// supports.  Every served protocol is a block protocol, which cannot satisfy
// multi-node writer semantics.
//
// Access-mode mapping (Kubernetes PVC → CSI constant):
//
//	ReadWriteOnce    (RWO)  → SINGLE_NODE_WRITER
//	ReadWriteOncePod (RWOP) → SINGLE_NODE_SINGLE_WRITER   (CSI spec v1.5+)
//	ReadOnlyMany     (ROX)  → MULTI_NODE_READER_ONLY
var supportedAccessModes = []csi.VolumeCapability_AccessMode_Mode{
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

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"each volume capability must specify an access_mode")
		}
		if !isSupportedAccessMode(cap.GetAccessMode().GetMode()) {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf(
					"access mode %s is not supported; supported modes: %s",
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

// isSupportedAccessMode returns true when mode is supported.
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
	return strings.Join(names, ", ")
}

// ─────────────────────────────────────────────────────────────────────────────
// VolumeContext and provisioner-metadata key constants
// ─────────────────────────────────────────────────────────────────────────────.
//
// StorageClass parameter keys live in resolve.go.

const (
	// ParamPVCNameMeta and paramPVCNamespaceMeta are the keys
	// external-provisioner injects with --extra-create-metadata.  They name
	// the claim a CreateVolume call provisions for: its annotations are the
	// per-volume configuration layer and its UID identifies the volume's
	// lifecycle.
	paramPVCNameMeta      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespaceMeta = "csi.storage.k8s.io/pvc/namespace"

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
//     network protocol (NVMe-oF TCP).
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
	pvName := req.GetName()

	// Access modes depend only on the request (every served protocol is a
	// block protocol), so they are checked before any retry fast path.
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedAccessMode(cap.GetAccessMode().GetMode()) {
			return nil, status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported; supported modes: %s",
				cap.GetAccessMode().GetMode(), describeSupportedModes())
		}
	}

	// ── Load persisted state (idempotency and partial-failure recovery) ───────
	// The PillarVolumeState CRD name is the CSI volume name, which is a
	// Kubernetes-compatible identifier assigned by the CO (e.g. "pvc-abc123").
	existingPV, pvExists, pvErr := s.loadPillarVolumeState(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to load PillarVolumeState %q: %v", pvName, pvErr)
	}

	// ── Completed volume: answer from the durable record ─────────────────────
	// A Ready lifecycle is answered from its recorded routing and resolved
	// configuration without consulting the CRDs or the claim, so the retry
	// response (which becomes the PV's VolumeContext) reproduces the first
	// success even when those sources no longer exist.
	if pvExists && existingPV.Spec.Resolved != nil && existingPV.Status.ExportInfo != nil &&
		!existingPV.Status.Deleting {
		volumeID := existingPV.Spec.VolumeID
		s.sm.ForceState(volumeID, pillarVolumeStatePhaseToVolumeState(existingPV.Status.Phase))
		if s.sm.GetState(volumeID) == StateCreated {
			return completedVolumeResponse(req, existingPV)
		}
	}

	// ── Resolve the effective configuration from live CRs ───────────────────
	// It runs before any durable state: when a referenced binding, store,
	// protocol or claim is missing or invalid the volume must not be
	// provisioned without its configured settings.  A retry of a lifecycle
	// that already recorded its resolution re-resolves only the export
	// settings; the backend, filesystem and agent stay as recorded.
	var recordedCfg *v1alpha1.ResolvedVolumeConfig
	if pvExists {
		recordedCfg = existingPV.Spec.Resolved
	}
	res, err := s.resolveVolumeConfig(ctx, scParams, recordedCfg)
	if err != nil {
		return nil, err
	}
	resolved := res.resolved
	targetName := res.agentRef
	if recordedCfg != nil {
		targetName = existingPV.Spec.AgentRef
	}
	backendID := resolved.Backend.Kind()
	protocolID := resolved.Protocol.Kind()
	agentBackendType := mapBackendType(string(backendID))
	agentProtocolType := mapProtocolType(string(protocolID))

	// The filesystem settings are frozen into the PV VolumeContext and only
	// applied when NodeStageVolume formats the volume; reject a setting that
	// cannot apply to this volume before any durable state exists, instead
	// of provisioning a volume that ignores it.
	fsErr := validateFilesystemConfig(resolved.Filesystem, res.pvcFS, req.GetVolumeCapabilities())
	if fsErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid filesystem configuration: %v", fsErr)
	}

	// ── Build the agent-level and CSI volume IDs ─────────────────────────────
	// Agent volume ID: "<pool>/<volume-name>", pool = ZFS pool or LVM VG.
	// CSI volume ID:   "<agent>/<protocol>/<backend>/<agent-vol-id>".
	agentVolID := resolved.Backend.PoolName() + "/" + pvName
	volumeID := strings.Join(
		[]string{targetName, string(protocolID), string(backendID), agentVolID},
		"/",
	)
	if pvExists {
		s.sm.ForceState(volumeID, pillarVolumeStatePhaseToVolumeState(existingPV.Status.Phase))
	}

	// ── Requested capacity ────────────────────────────────────────────────────
	var capacityBytes int64
	if cr := req.GetCapacityRange(); cr != nil {
		capacityBytes = cr.GetRequiredBytes()
	}

	// ── Durable lifecycle before any agent call ──────────────────────────────
	// The PillarVolumeState is created first, so every backend resource an
	// agent ever creates belongs to a lifecycle (its UID) that DeleteVolume can
	// find and fence; a volume without a PillarVolumeState owns nothing.  The
	// claim identity lets the controller tear down an attempt whose claim was
	// removed before any PersistentVolume existed.  The resolved
	// configuration is recorded with it and stays authoritative for the whole
	// lifecycle (the spec is immutable once created).
	spec := v1alpha1.PillarVolumeStateSpec{
		VolumeID:      volumeID,
		AgentVolumeID: agentVolID,
		AgentRef:      targetName,
		BackendType:   string(backendID),
		ProtocolType:  string(protocolID),
		CapacityBytes: capacityBytes,
		Resolved:      resolved,
	}
	attempt := existingPV
	if !pvExists {
		claimRef, claimFound, claimErr := s.claimRefFor(ctx, pvName, scParams)
		if claimErr != nil {
			return nil, claimErr
		}
		if claimFound {
			spec.ClaimRef = &claimRef
		}
		attempt = &v1alpha1.PillarVolumeState{ObjectMeta: metav1.ObjectMeta{Name: pvName}, Spec: spec}
	}
	err = s.refuseAbandonedClaim(ctx, attempt)
	if err != nil {
		return nil, err
	}
	pvs, err := s.ensureVolumeState(ctx, pvName, spec)
	if err != nil {
		return nil, err
	}
	err = refuseDeleting(pvs, volumeID)
	if err != nil {
		return nil, err
	}
	// The first attempt's resolution is authoritative for the backend and
	// the node-side settings; only the export parameters below come from
	// this attempt's resolution, so a retry can correct e.g. an in-capsule
	// data size that conflicted with the shared target port.
	recorded := resolved
	if pvs.Spec.Resolved != nil {
		recorded = pvs.Spec.Resolved
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
	bindIP := extractIP(agentAddr)
	exportParams, aclEnabled := exportParamsFromResolved(resolved.Protocol, bindIP)
	// The durable export spec is recorded with the partial state so the
	// resync controller can re-create the export after the storage node
	// loses its target state, independent of later parameter changes.
	exportSpec := exportSpecFor(exportParams, aclEnabled)
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
		// The retry exports with the current parameters (e.g. a corrected
		// in-capsule data size after a port conflict), so the durable spec
		// must describe them before the export exists.
		if !reflect.DeepEqual(pvs.Status.ExportSpec, exportSpec) {
			err = s.persistCreatePartial(ctx, pvName, pvs.UID, devicePath, exportSpec)
			if err != nil {
				return nil, err
			}
		}
	} else {
		devicePath, actualCapacity, err = s.createBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			&agentv1.CreateVolumeRequest{
				VolumeId:      agentVolID,
				CapacityBytes: capacityBytes,
				BackendType:   agentBackendType,
				BackendParams: backendParamsFromResolved(recorded.Backend),
				AccessType:    agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_BLOCK,
			}, exportSpec)
		if err != nil {
			return nil, err
		}
	}

	// ── Step 2: Export the volume over the network protocol ───────────────────
	// The NVMe-oF bind address is the storage node's IP (no port).
	// agent.ExportVolume is idempotent: if the export already exists (retry
	// scenario), it returns the existing ExportInfo without error.
	exportToken, err := s.claimOperation(ctx, pvName, volumeID, pvs.UID)
	if err != nil {
		return nil, err
	}
	exportResp, err := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     agentVolID,
		ProtocolType: agentProtocolType,
		ExportParams: exportParams,
		DevicePath:   devicePath,
		AclEnabled:   aclEnabled,
		Fence:        exportToken,
	})
	if err != nil {
		// The PillarVolumeState records CreatePartial durably; the CO may
		// retry safely and the next attempt only re-exports.
		grpcSt, _ := status.FromError(err)
		return nil, status.Errorf(grpcSt.Code(),
			"agent ExportVolume(%q) failed: %v", agentVolID, err)
	}

	// ── Record the lifecycle Ready before reporting success ──────────────────
	// The provisioner creates the PersistentVolume from this response, so the
	// Ready record must be durable first: a lifecycle that is not Ready never
	// has a PersistentVolume, which is what lets ReapAbandonedVolume end one
	// whose claim is gone.  A failure is returned; the retry re-exports
	// idempotently from CreatePartial and records Ready again.
	info := exportResp.GetExportInfo()
	err = s.persistVolumeReady(ctx, pvName, pvs.UID, info)
	if err != nil {
		return nil, err
	}
	s.sm.ForceState(volumeID, StateCreated)

	// ── Build VolumeContext from ExportInfo ───────────────────────────────────
	// These key/value pairs are stored in the PersistentVolume and forwarded to
	// NodeStageVolume so the node can connect to the volume over the network.
	volumeContext := map[string]string{
		vcTargetID:     info.GetTargetId(),
		vcAddress:      info.GetAddress(),
		vcPort:         strconv.Itoa(int(info.GetPort())),
		vcVolumeRef:    info.GetVolumeRef(),
		vcProtocolType: string(protocolID),
	}
	nodeVolumeContext(recorded, volumeContext)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: actualCapacity,
			VolumeContext: volumeContext,
		},
	}, nil
}

// completedVolumeResponse answers a CreateVolume retry for a Ready
// lifecycle from its durable record (see CreateVolume).
func completedVolumeResponse(
	req *csi.CreateVolumeRequest,
	pvs *v1alpha1.PillarVolumeState,
) (*csi.CreateVolumeResponse, error) {
	ei := pvs.Status.ExportInfo
	existingCap := pvs.Spec.CapacityBytes

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

	volumeContext := map[string]string{
		vcTargetID:     ei.TargetID,
		vcAddress:      ei.Address,
		vcPort:         strconv.Itoa(int(ei.Port)),
		vcVolumeRef:    ei.VolumeRef,
		vcProtocolType: pvs.Spec.ProtocolType,
	}
	nodeVolumeContext(pvs.Spec.Resolved, volumeContext)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      pvs.Spec.VolumeID,
			CapacityBytes: existingCap,
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

	pvName := pillarVolumeStateNameFromVolumeID(volumeID)
	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	// CSI: a volume still published to a node must not be deleted.  The
	// deleting flag is committed by compare-and-swap only while no
	// publication is recorded, so a concurrent publish on another controller
	// is rejected instead of racing the deletion.
	pvs, fence, err := s.markVolumeDeleting(ctx, pvName, volumeID, nil)
	if err != nil {
		return nil, err
	}
	if pvs == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}

	err = s.teardownMarkedVolume(ctx, volumeTeardown{
		volumeID:     volumeID,
		pvName:       pvName,
		uid:          pvs.UID,
		targetName:   targetName,
		protocolType: mapProtocolType(parts[1]),
		backendType:  mapBackendType(parts[2]),
		agentVolID:   parts[3],
		fence:        fence,
	})
	if err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// volumeTeardown identifies one lifecycle whose PillarVolumeState is already
// marked deleting, and routes its agent RPCs.
type volumeTeardown struct {
	volumeID     string
	pvName       string
	uid          types.UID
	targetName   string
	protocolType agentv1.ProtocolType
	backendType  agentv1.BackendType
	agentVolID   string
	fence        *agentv1.FencingToken
}

// teardownMarkedVolume removes the export and the backend resource of a
// lifecycle already marked deleting, then deletes its PillarVolumeState.  The
// caller holds the volume lock.  Both agent RPCs are idempotent (a missing
// export or backend resource is success) and carry the deletion's fencing
// token, so the teardown is safe whether or not the backend resource was ever
// created and is repeated unchanged on retry.  Any failure keeps the record
// (still marked deleting) for the retry.
func (s *ControllerServer) teardownMarkedVolume(ctx context.Context, t volumeTeardown) error {
	// ── Resolve the agent address from PillarAgent ───────────────────────────
	target := &v1alpha1.PillarAgent{}
	getTargetErr := s.k8sClient.Get(ctx, types.NamespacedName{Name: t.targetName}, target)
	if getTargetErr != nil {
		if !k8serrors.IsNotFound(getTargetErr) {
			return status.Errorf(codes.Internal,
				"failed to get PillarAgent %q: %v", t.targetName, getTargetErr)
		}
		// A missing PillarAgent object does not prove the node's backend and
		// target are gone, and without the agent the lifecycle cannot be
		// ended durably.  Keep the record (marked deleting) and fail closed;
		// the caller retries until the agent is reachable again.
		return status.Errorf(codes.FailedPrecondition,
			"PillarAgent %q not found; cannot confirm deletion of volume %q on its storage node",
			t.targetName, t.volumeID)
	}

	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		// Target exists but has no address yet.  This is a transient state;
		// return Unavailable so the caller retries.
		return status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address", t.targetName)
	}

	// ── Dial the agent ────────────────────────────────────────────────────────
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Step 1: Remove the network export (idempotent) ────────────────────────
	_, unexportErr := agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     t.agentVolID,
		ProtocolType: t.protocolType,
		Fence:        t.fence,
	})
	unexportCode := status.Code(unexportErr)
	if unexportErr != nil && unexportCode != codes.NotFound {
		return status.Errorf(unexportCode,
			"agent UnexportVolume(%q) failed: %v", t.agentVolID, unexportErr)
	}

	// ── Step 2: Destroy the backend storage resource (idempotent) ─────────────
	// Only a successful deletion ends the lifecycle at the agent; the record
	// is kept on any failure so the caller retries with the same token.
	_, deleteErr := agentClient.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
		VolumeId:    t.agentVolID,
		BackendType: t.backendType,
		Fence:       t.fence,
	})
	if deleteErr != nil {
		st, _ := status.FromError(deleteErr)
		return status.Errorf(st.Code(),
			"agent DeleteVolume(%q) failed: %v", t.agentVolID, deleteErr)
	}

	return s.finishDelete(ctx, t.volumeID, t.pvName, t.uid)
}

// finishDelete forgets the volume in memory and removes the lifecycle's
// PillarVolumeState.  A failure is returned so the caller retries: the retry
// finds the record still marked deleting and repeats the idempotent steps.
func (s *ControllerServer) finishDelete(
	ctx context.Context,
	volumeID, pvName string,
	uid types.UID,
) error {
	err := s.deleteVolumeState(ctx, pvName, uid)
	if err != nil {
		return err
	}
	s.sm.ForceState(volumeID, StateNonExistent)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// PillarVolumeState CRD helpers (partial-failure state persistence)
// ─────────────────────────────────────────────────────────────────────────────.

// loadPillarVolumeState returns the PillarVolumeState CRD for the given volume name,
// along with a boolean indicating whether it was found.  A nil apiReader or
// a NotFound error are treated as "not found" (non-error).
//
// The read is uncached (apiReader), matching readVolumeState: the CSI server
// runs on every replica, so an existence or idempotency answer derived from a
// lagging informer cache during a failover could resurface a volume the
// leader's pod has already deleted.
func (s *ControllerServer) loadPillarVolumeState(
	ctx context.Context,
	pvName string,
) (*v1alpha1.PillarVolumeState, bool, error) {
	if s.apiReader == nil {
		return nil, false, nil
	}
	pv := &v1alpha1.PillarVolumeState{}
	err := s.apiReader.Get(ctx, types.NamespacedName{Name: pvName}, pv)
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
// Backend / protocol token mappers
// ─────────────────────────────────────────────────────────────────────────────.

// mapBackendType converts a backend routing token (the backend segment of a
// volume ID, v1alpha1.BackendID) to the agent protobuf enum value.  An
// unsupported token maps to UNSPECIFIED, which the agent rejects explicitly.
func mapBackendType(s string) agentv1.BackendType {
	switch v1alpha1.BackendID(s) {
	case v1alpha1.BackendIDZFSZvol:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
	case v1alpha1.BackendIDLVMLV:
		return agentv1.BackendType_BACKEND_TYPE_LVM
	default:
		return agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}

// mapProtocolType converts a protocol routing token (the protocol segment of
// a volume ID, v1alpha1.ProtocolID) to the agent protobuf enum value.  An
// unsupported token maps to UNSPECIFIED, which the agent rejects explicitly.
func mapProtocolType(s string) agentv1.ProtocolType {
	switch v1alpha1.ProtocolID(s) {
	case v1alpha1.ProtocolIDNVMeOFTCP:
		return agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP
	default:
		return agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED
	}
}

// resolveInitiatorID looks up the protocol-specific initiator identity for the
// given Kubernetes node by reading the appropriate CSINode annotation.
//
// Identity resolution by protocol:
//
//	NVMe-oF TCP → CSINode.annotations["pillar-csi.bhyoo.com/nvmeof-host-nqn"]
//	other       → nodeID unchanged (the agent rejects an unsupported protocol)
//
// Returns FailedPrecondition if the CSINode does not exist or the required
// annotation is absent.  This causes the CO (external-attacher) to retry with
// exponential backoff, giving the node plugin time to publish its identity
// after a fresh node bootstrap.
func (s *ControllerServer) resolveInitiatorID(ctx context.Context, nodeID, protocolTypeStr string) (string, error) {
	if v1alpha1.ProtocolID(protocolTypeStr) != v1alpha1.ProtocolIDNVMeOFTCP {
		return nodeID, nil
	}
	annotationKey := AnnotationNVMeOFHostNQN

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

// ─────────────────────────────────────────────────────────────────────────────
// Backend / export parameter builders
// ─────────────────────────────────────────────────────────────────────────────.

// backendParamsFromResolved constructs the agent CreateVolume backend
// parameters from the resolved backend configuration.
//
// ZFS: pool, parent dataset and the merged properties (store < binding <
// StorageClass document < PVC).  LVM: volume group, the resolved
// provisioning mode (default linear) and the store's thin pool — always
// sent, "" meaning none, so the agent refuses to create a volume when its
// configured thin pool differs from the store's.
func backendParamsFromResolved(b v1alpha1.BackendSpec) *agentv1.BackendParams {
	switch {
	case b.ZFS != nil:
		var props map[string]string
		if len(b.ZFS.Properties) > 0 {
			props = maps.Clone(b.ZFS.Properties)
		}
		return &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Zfs{
				Zfs: &agentv1.ZfsVolumeParams{
					Pool:          b.ZFS.Pool,
					ParentDataset: b.ZFS.ParentDataset,
					Properties:    props,
				},
			},
		}
	case b.LVM != nil:
		mode := b.LVM.ProvisioningMode
		if mode == "" {
			mode = v1alpha1.LVMProvisioningModeLinear
		}
		thinPool := b.LVM.ThinPool
		return &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{
					VolumeGroup:   b.LVM.VolumeGroup,
					ProvisionMode: string(mode),
					ThinPool:      &thinPool,
				},
			},
		}
	default:
		return nil
	}
}

// defaultNVMeOFPort is the NVMe-oF/TCP listener port when the protocol does
// not set one (the CRD default).
const defaultNVMeOFPort = 4420

// exportParamsFromResolved constructs the agent ExportVolume parameters and
// the ACL flag from the resolved protocol configuration.  The bind address is
// the storage node's IP (PillarAgent.status.resolvedAddress without its port).
func exportParamsFromResolved(p v1alpha1.ProtocolSpec, bindAddress string) (*agentv1.ExportParams, bool) {
	n := p.NVMeOFTCP
	if n == nil {
		return nil, false
	}
	port := n.Port
	if port == 0 {
		port = defaultNVMeOFPort
	}
	var inCapsuleDataSize int32
	if n.InCapsuleDataSize != nil {
		inCapsuleDataSize = *n.InCapsuleDataSize
	}
	return &agentv1.ExportParams{
		Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{
				BindAddress:       bindAddress,
				Port:              port,
				InCapsuleDataSize: inCapsuleDataSize,
			},
		},
	}, n.ACL
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
	if isSupportedAccessMode(mode) {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"access mode %s is not supported for protocol %q; supported modes: %s",
		mode, protocolTypeStr, describeSupportedModes())
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
//
// Local attach: when the volume resolved localAttach, the access mode is a
// SINGLE_NODE_* mode and nodeID is the node hosting the volume's PillarAgent
// (spec.nodeRef; external agents never qualify), the publication is recorded
// as local, the agent fences the network export (SetLocalAttach local=true)
// instead of granting an initiator, and the PublishContext tells
// NodeStageVolume to attach the backend device directly.  A later protocol
// publish first has the agent re-enable the export (SetLocalAttach
// local=false, which fails with FailedPrecondition while the storage node
// still holds the device), then clears status.localAttachNode under a new
// fencing generation and re-enables at that generation before granting its
// initiator.  Even with status.localAttachNode already empty, a protocol
// publish of a localAttach volume re-enables at its reservation, so a stale
// resync can never leave the export disabled under a successful publish
// (see finishPublish).
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

	// ── Resolve the storage node's PillarAgent ───────────────────────────────
	agent, agentErr := s.getReadyAgent(ctx, targetName)
	if agentErr != nil {
		return nil, agentErr
	}
	agentAddr := agent.Status.ResolvedAddress
	local := isLocalAttachPublish(pvs, agent, nodeID, mode)

	// ── Resolve initiator identity from CSINode annotation ───────────────────
	// A local attach grants no initiator: the publication is identified by
	// the node itself and needs no node-plugin identity.
	initiatorID := nodeID
	if !local {
		var resolveErr error
		initiatorID, resolveErr = s.resolvePublishInitiator(ctx, nodeID, protocolTypeStr)
		if resolveErr != nil {
			return nil, resolveErr
		}
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
		Local:       local,
	})
	if reserveErr != nil {
		return nil, reserveErr
	}

	return s.finishPublish(ctx, local, pvName, agentAddr, volumeID, agentVolID, agentProtocolType,
		nodeID, initiatorID, pvs, fence)
}

// finishPublish completes a publish whose publication is already committed
// under fence: local publishes fence the export and return the backend
// device path, protocol publishes re-enable the export of a localAttach
// volume (returning it from a previous local attach when
// status.localAttachNode is set) and grant the initiator.
func (s *ControllerServer) finishPublish(
	ctx context.Context,
	local bool,
	pvName, agentAddr, volumeID, agentVolID string,
	protocolType agentv1.ProtocolType,
	nodeID, initiatorID string,
	pvs *v1alpha1.PillarVolumeState,
	fence *agentv1.FencingToken,
) (*csi.ControllerPublishVolumeResponse, error) {
	if local {
		return s.finishLocalPublish(ctx, agentAddr, volumeID, agentVolID, protocolType, nodeID, fence)
	}

	// ── Return the export of a local attach to the network ─────────────────
	// Clearing status.localAttachNode changes the desired export state the
	// resync loop derives (local_attach=false), so the clear commits a new
	// fencing generation and the unfence is issued twice:
	//
	//   1. at the reservation's generation, as a gate: the agent refuses
	//      (FailedPrecondition) while the storage node still holds the backend
	//      device, so the CO retries until the direct attach is really gone
	//      and the reservation is kept meanwhile (fail-closed);
	//   2. at the clear's generation, so the agent's applied fence advances
	//      past every generation at which status.localAttachNode was set — a
	//      resync desired-state built from a stale snapshot can no longer be
	//      admitted and re-disable the namespace, and if one slipped in
	//      between the two calls this re-enables it — and so the grant below
	//      is ordered after the export's return to the network.
	//
	// A retry that already finds status.localAttachNode empty (a previous
	// attempt committed the clear and then failed) still re-enables at its
	// own reservation, which is newer than every generation that recorded
	// the field: a stale resync admitted just before the failed attempt's
	// re-fence may have left the namespace disabled, and skipping the call
	// would grant — or, for an ACL-off export, succeed without any agent RPC
	// at all — while the export still refuses the initiator.  An empty field
	// cannot be told apart from "never attached locally" here, so every
	// protocol publish of a localAttach volume re-enables at least once; the
	// agent answers a no-op success when the export is already enabled.
	if prevLocal := pvs.Status.LocalAttachNode; prevLocal != "" {
		_, unfenceErr := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence)
		if unfenceErr != nil {
			return nil, unfenceErr
		}
		fence, unfenceErr = s.clearLocalAttachNode(ctx, pvName, pvs.UID, prevLocal)
		if unfenceErr != nil {
			return nil, unfenceErr
		}
		_, unfenceErr = s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence)
		if unfenceErr != nil {
			return nil, unfenceErr
		}
	} else if r := pvs.Spec.Resolved; r != nil && r.LocalAttach {
		_, unfenceErr := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence)
		if unfenceErr != nil {
			return nil, unfenceErr
		}
	}

	// An export with ACL off (attr_allow_any_host=1) has no per-host ACL: the
	// kernel rejects allowed_hosts links with EINVAL.  The publication record
	// above still orders exclusivity; only the grant RPC is skipped (the
	// unfence above is independent of the ACL).
	if exportACLEnabled(pvs) {
		grantErr := s.grantPublication(ctx, agentAddr, agentVolID, protocolType, initiatorID, fence)
		if grantErr != nil {
			return nil, grantErr
		}
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

// isLocalAttachPublish reports whether publishing the volume to nodeID with
// mode is a local attach: the volume resolved localAttach, the agent is an
// in-cluster agent whose spec.nodeRef names nodeID, and the access mode is a
// SINGLE_NODE_* mode (a multi-node mode always uses the protocol, which is
// what other nodes need).
func isLocalAttachPublish(
	pvs *v1alpha1.PillarVolumeState,
	agent *v1alpha1.PillarAgent,
	nodeID string,
	mode csi.VolumeCapability_AccessMode_Mode,
) bool {
	if pvs.Spec.Resolved == nil || !pvs.Spec.Resolved.LocalAttach {
		return false
	}
	if agent.Spec.External != nil || agent.Spec.NodeRef == nil || agent.Spec.NodeRef.Name != nodeID {
		return false
	}
	switch mode {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return true
	default:
		return false
	}
}

// finishLocalPublish completes a local-attach publish whose publication
// (and status.localAttachNode) is already recorded under fence: the agent
// fences the network export and reports the backend device, which the
// PublishContext hands to NodeStageVolume.  Retrying is idempotent: the agent
// call is idempotent and returns the same device path.
func (s *ControllerServer) finishLocalPublish(
	ctx context.Context,
	agentAddr, volumeID, agentVolID string,
	protocolType agentv1.ProtocolType,
	nodeID string,
	fence *agentv1.FencingToken,
) (*csi.ControllerPublishVolumeResponse, error) {
	devicePath, err := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, true, fence)
	if err != nil {
		return nil, err
	}
	if devicePath == "" {
		return nil, status.Errorf(codes.Internal,
			"agent SetLocalAttach(%q, local=true) returned no device path", agentVolID)
	}
	s.sm.ForceState(volumeID, StateControllerPublished)
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			PublishContextKeyAttachMode:      AttachModeLocal,
			PublishContextKeyLocalNode:       nodeID,
			PublishContextKeyLocalDevicePath: devicePath,
		},
	}, nil
}

// setLocalAttach asks the agent to fence (local=true) or re-enable
// (local=false) the volume's network export and returns the backend device
// path the agent reports.  The agent's gRPC status code is preserved, so a
// FailedPrecondition (device still held on the storage node) reaches the CO
// as such and is retried.
func (s *ControllerServer) setLocalAttach(
	ctx context.Context,
	agentAddr, agentVolID string,
	protocolType agentv1.ProtocolType,
	local bool,
	fence *agentv1.FencingToken,
) (string, error) {
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return "", status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	resp, err := agentClient.SetLocalAttach(ctx, &agentv1.SetLocalAttachRequest{
		VolumeId:     agentVolID,
		ProtocolType: protocolType,
		Local:        local,
		Fence:        fence,
	})
	if err != nil {
		return "", status.Errorf(status.Code(err),
			"agent SetLocalAttach(%q, local=%t) failed: %v", agentVolID, local, err)
	}
	return resp.GetDevicePath(), nil
}

// exportACLEnabled reports whether the volume's export enforces a per-host
// ACL, i.e. whether ControllerPublish/Unpublish must grant and revoke
// initiators on the agent.  The durable status.exportSpec is authoritative;
// without it the resolved protocol configuration decides.  A volume with
// neither record (provisioned before either existed) keeps the historical
// behavior of managing initiators.
func exportACLEnabled(pvs *v1alpha1.PillarVolumeState) bool {
	if spec := pvs.Status.ExportSpec; spec != nil {
		return spec.ACLEnabled
	}
	if r := pvs.Spec.Resolved; r != nil && r.Protocol.NVMeOFTCP != nil {
		return r.Protocol.NVMeOFTCP.ACL
	}
	return true
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

// getReadyAgent returns the PillarAgent targetName once it has a resolved
// address: NotFound when the object does not exist, Unavailable while it has
// no address yet, Internal on any other API error.
func (s *ControllerServer) getReadyAgent(ctx context.Context, targetName string) (*v1alpha1.PillarAgent, error) {
	target := &v1alpha1.PillarAgent{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "PillarAgent %q not found", targetName)
		}
		return nil, status.Errorf(codes.Internal, "failed to get PillarAgent %q: %v", targetName, err)
	}
	if target.Status.ResolvedAddress == "" {
		return nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address; agent may not be ready", targetName)
	}
	return target, nil
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

	// An export with ACL off (attr_allow_any_host=1) has no per-host ACL to
	// revoke — and the kernel rejects allowed_hosts changes with EINVAL —
	// so the records are dropped without contacting the agent.
	acl := exportACLEnabled(existingPV)
	var agentClient agentv1.AgentServiceClient
	if acl {
		// ── Resolve the agent address from PillarAgent ───────────────────────
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

		// ── Dial the agent ────────────────────────────────────────────────────
		dialed, closer, err := s.dialAgent(ctx, agentAddr)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable,
				"failed to dial agent at %q: %v", agentAddr, err)
		}
		defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled
		agentClient = dialed
	}

	// ── Revoke initiator access (idempotent), then drop the records ──────────
	remaining, revokeErr := s.revokePublications(ctx, agentClient, agentVolID,
		agentProtocolType, pvName, existingPV.UID, nodeID, acl)
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
//
// When acl is false the export allows every host, so no DenyInitiator RPCs
// run (agentClient may be nil): the fencing token still orders the records.
// A local publication granted no initiator, so it is released without a
// DenyInitiator; status.localAttachNode stays set (the export stays fenced)
// until a later protocol publish has the agent re-enable it.
func (s *ControllerServer) revokePublications(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	agentVolID string,
	protocolType agentv1.ProtocolType,
	pvName string,
	uid types.UID,
	nodeID string,
	acl bool,
) (remaining int, err error) {
	fence, revoke, err := s.fencePublications(ctx, pvName, uid, nodeID)
	if err != nil {
		return 0, err
	}
	revokedNodes := make([]string, 0, len(revoke))
	for _, pub := range revoke {
		if !acl || pub.Local {
			revokedNodes = append(revokedNodes, pub.NodeID)
			continue
		}
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
// The method returns the actual capacity after expansion.  Every served
// protocol is a block protocol, so node_expansion_required is always true:
// the CO subsequently calls NodeExpandVolume to rescan the block device and
// resize the filesystem.
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

	// Every served protocol is a block protocol: the node must rescan the
	// block device and grow the filesystem of a Filesystem-mode volume.
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
// query to the pillar-agent running on the storage node.  The PillarStore is
// identified the same way CreateVolume identifies it: through the
// PillarStorageClass named by pillar-csi.bhyoo.com/storage-class, or directly
// by pillar-csi.bhyoo.com/store-ref.
func (s *ControllerServer) GetCapacity(
	ctx context.Context,
	req *csi.GetCapacityRequest,
) (*csi.GetCapacityResponse, error) {
	params := req.GetParameters()

	// CSI spec §4.1.2: GetCapacity MAY be called with empty parameters and
	// MUST NOT fail in that case — it should return zero available capacity
	// so the CO knows no pool is selectable.  Returning InvalidArgument here
	// breaks csi-sanity's "no optional values added" test and is wrong per
	// the spec; the parameters field is informational, not validated.
	storeName := params[paramStoreRef]
	if bindingName := params[paramBinding]; bindingName != "" {
		binding := &v1alpha1.PillarStorageClass{}
		err := s.k8sClient.Get(ctx, types.NamespacedName{Name: bindingName}, binding)
		if err != nil {
			return nil, capacityLookupError("PillarStorageClass", bindingName, err)
		}
		storeName = binding.Spec.StoreRef
	}
	if storeName == "" {
		return &csi.GetCapacityResponse{AvailableCapacity: 0}, nil
	}
	store := &v1alpha1.PillarStore{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: storeName}, store)
	if err != nil {
		return nil, capacityLookupError("PillarStore", storeName, err)
	}
	targetName := store.Spec.AgentRef
	poolName := store.Spec.Backend.PoolName()
	agentBackendType := mapBackendType(string(store.Spec.Backend.Kind()))

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

// capacityLookupError converts a failed CR read in GetCapacity to a gRPC
// status: NotFound for a missing CR, Internal otherwise.
func capacityLookupError(kind, name string, err error) error {
	if k8serrors.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "%s %q not found", kind, name)
	}
	return status.Errorf(codes.Internal, "failed to get %s %q: %v", kind, name, err)
}
