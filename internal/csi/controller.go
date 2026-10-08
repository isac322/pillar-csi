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
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agentclient"
	"github.com/isac322/pillar-csi/internal/telemetry"
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
//
// The connection is instrumented with agentclient.TelemetryDialOptions, with
// the agent name taken from ctx (agentclient.WithAgentName); one connection is
// created per call, so the name is exact for every RPC on it.
func DefaultAgentDialer(ctx context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
	// grpc.NewClient (not grpc.Dial) is the non-deprecated entry point as of
	// gRPC-Go v1.64.  The connection is lazy; the first RPC attempt triggers
	// the actual TCP handshake.
	opts := slices.Concat(
		agentclient.TelemetryDialOptions(agentclient.AgentNameFromContext(ctx)),
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial agent at %q: %w", addr, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn, nil
}

// The CSI controller is the only writer of PillarVolumeState (durable volume
// lifecycle state); no reconciler owns it, so its RBAC is declared here.
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumestates/status,verbs=get;update;patch
// PillarVolumeReservation is the atomic backend-volume claim created before a
// PillarVolumeState exists, so its RBAC is declared here too.
// +kubebuilder:rbac:groups=pillar-csi.bhyoo.com,resources=pillarvolumereservations,verbs=get;list;create;delete
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

	// installNamespace is the pillar-csi installation namespace, where the
	// iSCSI CHAP Secrets named by PillarProtocols live.  Empty when unknown:
	// CHAP volumes then fail with FailedPrecondition.
	installNamespace string
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

// NewControllerServerWithAgentDialer constructs a ControllerServer that
// reaches agents through manager, the controller's shared agent connection
// manager, so CSI RPCs and export restoration use its transport credentials
// (mTLS when configured).
func NewControllerServerWithAgentDialer(
	k8sClient client.Client,
	apiReader client.Reader,
	driverName string,
	manager agentclient.Dialer,
) *ControllerServer {
	srv := NewControllerServerWithDialer(k8sClient, driverName, sharedAgentDialer(manager))
	srv.apiReader = apiReader
	return srv
}

// sharedAgentDialer adapts manager to AgentDialer. The manager owns and
// caches its connections, so the per-call closer is a no-op: closing the
// connection after one RPC would break every later RPC to the same agent.
func sharedAgentDialer(manager agentclient.Dialer) AgentDialer {
	return func(ctx context.Context, addr string) (agentv1.AgentServiceClient, io.Closer, error) {
		agentClient, err := manager.Dial(ctx, addr)
		if err != nil {
			return nil, nil, fmt.Errorf("agent connection manager: %w", err)
		}
		return agentClient, sharedAgentNoopCloser{}, nil
	}
}

type sharedAgentNoopCloser struct{}

func (sharedAgentNoopCloser) Close() error { return nil }

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

// SetInstallNamespace sets the pillar-csi installation namespace (the
// controller pod's namespace) from which iSCSI CHAP Secrets are read.  It
// must be called before the server handles requests.
func (s *ControllerServer) SetInstallNamespace(namespace string) {
	s.installNamespace = namespace
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
		if scopedDriverForVolume(pv) != s.effectiveDriverName() {
			continue
		}
		state := pillarVolumeStatePhaseToVolumeState(pv.Status.Phase)
		if state == StateCreated && len(pv.Status.PublishedNodes) > 0 {
			state = StateControllerPublished
		}
		// The file driver never stages: a stage phase recorded for one of
		// its volumes can only mean it was controller-published, which is
		// the state its direct NodePublishVolume follows.
		if s.effectiveDriverName() == v1alpha1.FileCSIDriver &&
			(state == StateNodeStagePartial || state == StateNodeStaged) {
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
// supports.  MULTI_NODE_MULTI_WRITER is restricted to NFS; block protocols
// retain their existing single-node and read-only-many behavior.
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
	// RWX: multiple NFS clients may mount read-write simultaneously.
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
	if s.driverName == v1alpha1.FileCSIDriver {
		rpcTypes = slices.DeleteFunc(rpcTypes, func(rpc csi.ControllerServiceCapability_RPC_Type) bool {
			return rpc == csi.ControllerServiceCapability_RPC_EXPAND_VOLUME
		})
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
	protocolType := req.GetVolumeContext()[vcProtocolType]
	var storedNFSReadonly bool
	if protocolType == "" {
		parts := strings.SplitN(req.GetVolumeId(), "/", volumeIDParts)
		if len(parts) == volumeIDParts {
			protocolType = parts[1]
		}
	}
	if s.apiReader != nil {
		_, pvs, stateErr := s.mustVolumeState(ctx, req.GetVolumeId())
		if stateErr != nil {
			return nil, stateErr
		}
		protocolType = pvs.Spec.ProtocolType
		storedNFSReadonly = v1alpha1.ProtocolID(protocolType) == v1alpha1.ProtocolIDNFS &&
			pvs.Status.ExportSpec != nil && pvs.Status.ExportSpec.NFS != nil &&
			pvs.Status.ExportSpec.NFS.Readonly
	}

	for _, cap := range req.GetVolumeCapabilities() {
		if cap.GetAccessMode() == nil {
			//nolint:wrapcheck // gRPC status errors must not be double-wrapped
			return nil, status.Error(codes.InvalidArgument,
				"each volume capability must specify an access_mode")
		}
	}
	capabilityErr := validateCapabilitiesForProtocol(protocolType, req.GetVolumeCapabilities())
	if capabilityErr != nil {
		//nolint:nilerr // CSI reports unsupported capabilities in Message, not as an RPC failure.
		return &csi.ValidateVolumeCapabilitiesResponse{Message: capabilityErr.Error()}, nil
	}
	if storedNFSReadonly && hasWritableVolumeCapability(req.GetVolumeCapabilities()) {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: "NFS volume export is read-only",
		}, nil
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

// hasWritableVolumeCapability reports whether any requested capability can
// write.  A read-only NFS export can still confirm ROX, but never a writer.
func hasWritableVolumeCapability(caps []*csi.VolumeCapability) bool {
	for _, cap := range caps {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
			return true
		}
	}
	return false
}

// isSupportedAccessMode returns true when mode is supported.
func isSupportedAccessMode(mode csi.VolumeCapability_AccessMode_Mode) bool {
	return slices.Contains(supportedAccessModes, mode)
}

// validateCapabilitiesForProtocol enforces the protocol-specific access and
// volume-mode matrix.  NFS is the only protocol that may serve RWX and it
// always serves a filesystem mount, never a raw block capability.
func validateCapabilitiesForProtocol(
	protocol string,
	caps []*csi.VolumeCapability,
) error {
	isNFS := v1alpha1.ProtocolID(protocol) == v1alpha1.ProtocolIDNFS
	hasROX, hasWritable := false, false
	for _, cap := range caps {
		err := validateCapabilityForProtocol(protocol, cap)
		if err != nil {
			return err
		}
		if cap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
			hasROX = true
		} else {
			hasWritable = true
		}
	}
	if isNFS && hasROX && hasWritable {
		return fmt.Errorf("NFS volume capabilities cannot mix read-only-many and writable access modes")
	}
	return nil
}

func validateCapabilityForProtocol(protocol string, capability *csi.VolumeCapability) error {
	mode := capability.GetAccessMode().GetMode()
	if !isSupportedAccessMode(mode) {
		return fmt.Errorf("access mode %s is not supported; supported modes: %s",
			mode, describeSupportedModes())
	}
	isNFS := v1alpha1.ProtocolID(protocol) == v1alpha1.ProtocolIDNFS
	if mode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER && !isNFS {
		return fmt.Errorf("access mode %s is supported only for protocol %q",
			mode, v1alpha1.ProtocolIDNFS)
	}
	if !isNFS {
		return nil
	}
	if capability.GetBlock() != nil {
		return fmt.Errorf("NFS volumes do not support Block volume capabilities")
	}
	mount := capability.GetMount()
	if mount == nil {
		return fmt.Errorf("NFS volumes require Filesystem volume capabilities")
	}
	if fsType := mount.GetFsType(); fsType != "" && fsType != ProtocolNFS {
		return fmt.Errorf("NFS volumes do not support fsType %q", fsType)
	}
	return nil
}

// nfsReadonlyForCapabilities reports whether an NFS volume is exclusively
// ROX.  NFS export read-only is volume-wide, so mixed ROX/writable requests
// are rejected by validateCapabilitiesForProtocol rather than changing the
// export policy per client.
func nfsReadonlyForCapabilities(protocol v1alpha1.ProtocolID, caps []*csi.VolumeCapability) bool {
	if protocol != v1alpha1.ProtocolIDNFS || len(caps) == 0 {
		return false
	}
	for _, cap := range caps {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
			return false
		}
	}
	return true
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

	// AnnotationISCSIInitiatorIQN is the CSINode annotation that stores the
	// iSCSI initiator IQN for this node (read from, or generated into,
	// /etc/iscsi/initiatorname.iscsi).
	// Example value: "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef".
	AnnotationISCSIInitiatorIQN = "pillar-csi.bhyoo.com/iscsi-initiator-iqn"
)

// ─────────────────────────────────────────────────────────────────────────────
// CreateVolume
// ─────────────────────────────────────────────────────────────────────────────.

// CreateVolume provisions or adopts storage under a durable lifecycle.
// Ordinary volumes use CreateVolume; existing zvols and filesystems use fenced
// ImportVolume. Filesystems are inspected read-only before reservation.
// Network volumes are exported through their configured protocol. Local-only
// filesystem adoption has no network export; its owned proxy is mounted when
// the storage node publishes it.
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
	setSpanAttributes(ctx, telemetry.KeyPVName.String(pvName))
	setPVCAttributes(ctx, scParams[paramPVCNameMeta], scParams[paramPVCNamespaceMeta])

	// Reject unsupported access modes before any retry fast path; the resolved
	// protocol imposes further restrictions below.
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedAccessMode(cap.GetAccessMode().GetMode()) {
			return nil, status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported; supported modes: %s",
				cap.GetAccessMode().GetMode(), describeSupportedModes())
		}
	}

	// ── Requested capacity ────────────────────────────────────────────────────
	var capacityBytes int64
	if cr := req.GetCapacityRange(); cr != nil {
		capacityBytes = cr.GetRequiredBytes()
	}
	setSpanAttributes(ctx, telemetry.KeyCapacityRequestedBytes.Int64(capacityBytes))

	// ── Load persisted state (idempotency and partial-failure recovery) ───────
	// The PillarVolumeState CRD name is the CSI volume name, which is a
	// Kubernetes-compatible identifier assigned by the CO (e.g. "pvc-abc123").
	existingPV, pvExists, pvErr := s.loadPillarVolumeState(ctx, pvName)
	if pvErr != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to load PillarVolumeState %q: %v", pvName, pvErr)
	}
	if pvExists {
		err := s.validateVolumeDriver(existingPV)
		if err != nil {
			return nil, err
		}
		if existingPV.Spec.FilesystemAdoption != nil && !isFilesystemVolumeID(existingPV) {
			return nil, status.Errorf(codes.FailedPrecondition, "file lifecycle handle is invalid")
		}
	}

	// ── Completed volume: answer from the durable record ─────────────────────
	// A Ready lifecycle is answered from its recorded routing and resolved
	// configuration without consulting the CRDs or the claim, so the retry
	// response (which becomes the PV's VolumeContext) reproduces the first
	// success even when those sources no longer exist.
	if pvExists && existingPV.Spec.Resolved != nil &&
		(existingPV.Status.ExportInfo != nil || localOnlyFilesystem(existingPV)) && !existingPV.Status.Deleting {
		volumeID := existingPV.Spec.VolumeID
		state := pillarVolumeStatePhaseToVolumeState(existingPV.Status.Phase)
		if existingPV.Spec.FilesystemAdoption != nil && state == StateCreated && len(existingPV.Status.PublishedNodes) > 0 {
			state = StateControllerPublished
		}
		s.sm.ForceState(volumeID, state)
		if s.sm.GetState(volumeID) == StateCreated || completedFilesystemAdoption(existingPV) {
			if existingPV.Spec.FilesystemAdoption != nil {
				err := refuseUnadoptedImport(existingPV, volumeID, "retry create")
				if err != nil {
					return nil, err
				}
				docs, docsErr := s.claimDocs(ctx, scParams, true)
				if docsErr != nil {
					return nil, docsErr
				}
				if docs.ImportZvol != "" {
					return nil, status.Errorf(codes.InvalidArgument, "a filesystem lifecycle cannot import a zvol")
				}
				_, _, inspectErr := s.resolveFilesystemImportRequest(ctx, pvName, true, existingPV,
					existingPV.Spec.Resolved, docs.ImportDirectory, docs.ImportZFSDataset,
					existingPV.Spec.AgentRef, capacityBytes)
				if inspectErr != nil {
					return nil, inspectErr
				}
			}
			telemetry.SetVolumeAttributes(ctx, volumeID)
			setSpanAttributes(ctx, telemetry.KeyCreateResumedFrom.String(createResumedFromReady))
			// The recorded import source stays authoritative on this path
			// too: a claim re-annotated to another LV or policy, or a late
			// import-lv, import-directory or import-zfs-dataset annotation,
			// is refused instead of answered.
			driftErr := s.refuseRecordedImportDrift(ctx, existingPV, scParams)
			if driftErr != nil {
				return nil, driftErr
			}
			resp, respErr := completedVolumeResponse(req, existingPV)
			if respErr == nil {
				topology, err := s.filesystemAccessibleTopology(ctx, existingPV, req.GetVolumeCapabilities())
				if err != nil {
					return nil, err
				}
				resp.Volume.AccessibleTopology = topology
				err = validateFilesystemTopology(req.GetAccessibilityRequirements(), topology)
				if err != nil {
					return nil, err
				}
				setSpanAttributes(ctx,
					telemetry.KeyCapacityAllocatedBytes.Int64(resp.GetVolume().GetCapacityBytes()))
			}
			return resp, respErr
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
	res, err := s.resolveVolumeConfig(ctx, scParams, req.GetVolumeCapabilities(), recordedCfg)
	if err != nil {
		return nil, err
	}
	resolved := res.resolved
	err = validateCapabilitiesForProtocol(string(resolved.Protocol.Kind()), req.GetVolumeCapabilities())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if resolved.Protocol.Kind() != v1alpha1.ProtocolIDNFS && resolved.Filesystem != nil &&
		resolved.Filesystem.FSType == ProtocolNFS {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "fsType \"nfs\" is supported only for NFS protocol volumes")
	}
	targetName := res.agentRef
	if recordedCfg != nil {
		targetName = existingPV.Spec.AgentRef
	}
	backendID := resolved.Backend.Kind()
	protocolID := resolved.Protocol.Kind()
	agentBackendType := mapBackendType(string(backendID))
	agentProtocolType := mapProtocolType(string(protocolID))

	// ── Recovery record (spec.recovery) ─────────────────────────────────────
	// An operator-created record that exists solely to receive an authorized
	// ownership transfer refuses a claim carrying an import annotation: its
	// intent already pins the adopted source, and letting the import
	// resolution below run would add a second, ambiguous source of truth (and
	// an orphaned backend reservation).
	recoveryIntent := pvExists && existingPV.Spec.Recovery != nil
	if recoveryIntent && (res.importLV != "" || res.importLVPolicy != "" || res.importDataset != "" ||
		res.importDirectory != "" || res.importZFSDataset != "") {
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot recover volume for PillarVolumeState %q: the claim carries an "+
				"import annotation but the record is a recovery record; the intent "+
				"already pins the adopted source — use a plain claim", pvName)
	}

	// The filesystem settings are frozen into the PV VolumeContext and only
	// applied when NodeStageVolume formats the volume; reject a setting that
	// cannot apply to this volume before any durable state exists, instead
	// of provisioning a volume that ignores it.
	fsErr := validateFilesystemConfig(resolved.Filesystem, res.pvcFS, req.GetVolumeCapabilities())
	if fsErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid filesystem configuration: %v", fsErr)
	}

	// A CHAP volume's export carries the credentials read from the Secret.
	// Read and validate them before any durable state or backend exists, so
	// a missing or invalid Secret provisions nothing (FailedPrecondition; the
	// provisioner retries once the Secret is fixed).
	chap, err := s.iscsiChapFor(ctx, &resolved.Protocol)
	if err != nil {
		return nil, err
	}

	// ── Zvol import (PVC annotation pillar-csi.bhyoo.com/import-zvol) ────────
	// When the claim asks to adopt an existing zvol, the leaf of the named
	// dataset replaces the volume name inside the agent volume ID, and the
	// backend step below adopts instead of creating.  resolveImportRequest
	// performs every controller-side check (well-formed name, ZFS backend,
	// pool/parent match, no second lifecycle owning the same zvol on this
	// agent) and the recorded-importedFrom consistency check for retries.
	importedFrom, importLeaf, err := s.resolveImportRequest(
		ctx, pvName, pvExists, existingPV, resolved, res.importDataset, targetName)
	if err != nil {
		return nil, err
	}
	adoption, fileLeaf, err := s.resolveFilesystemImportRequest(ctx, pvName, pvExists, existingPV,
		resolved, res.importDirectory, res.importZFSDataset, targetName, capacityBytes)
	if err != nil {
		return nil, err
	}
	if adoption != nil {
		if importedFrom != "" || protocolID != v1alpha1.ProtocolIDNFS {
			return nil, status.Errorf(codes.InvalidArgument, "filesystem adoption requires NFS and cannot import a zvol")
		}
		if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && capacityBytes > limit {
			return nil, status.Errorf(codes.InvalidArgument, "filesystem exact capacity exceeds the requested limit")
		}
		importLeaf = fileLeaf
		if !pvExists && resolved.LocalAttach && filesystemCapabilitiesMultiNode(req.GetVolumeCapabilities()) {
			return nil, status.Errorf(codes.InvalidArgument, "local-only file class cannot serve multi-node capabilities")
		}
		if !pvExists {
			resolved.LocalAttach = !filesystemCapabilitiesMultiNode(req.GetVolumeCapabilities())
		} else {
			resolved.LocalAttach = existingPV.Spec.Resolved.LocalAttach
		}
		if resolved.LocalAttach && filesystemCapabilitiesMultiNode(req.GetVolumeCapabilities()) {
			return nil, status.Errorf(codes.AlreadyExists, "local-only filesystem adoption cannot change to network access")
		}
	}

	// ── LV import (PVC annotation pillar-csi.bhyoo.com/import-lv) ────────────
	// The adopted LV's "<vg>/<lv>" is the agent volume ID, and its identity
	// and policy are pinned in spec.lvmSource before any agent call; a retry
	// replays the recorded source (see resolveLVImportRequest).
	lvmSource, lvAgentVolID, err := s.resolveLVImportRequest(
		ctx, pvName, pvExists, existingPV, resolved, res.importLV, res.importLVPolicy, targetName)
	if err != nil {
		return nil, err
	}

	// ── Build the agent-level and CSI volume IDs ─────────────────────────────
	// Agent volume ID: "<pool>/<volume-name>", pool = ZFS pool or LVM VG.
	// CSI volume ID:   "<agent>/<protocol>/<backend>/<agent-vol-id>".
	// For an imported zvol the leaf is the source dataset's name, not pvName:
	// it is what the agent resolves under pool/parentDataset.
	agentVolLeaf := pvName
	if importLeaf != "" {
		agentVolLeaf = importLeaf
	}
	agentVolID := resolved.Backend.PoolName() + "/" + agentVolLeaf
	switch {
	case lvmSource != nil:
		agentVolID = lvAgentVolID
	case recoveryIntent:
		// The record pins the adopted LV: the agent volume ID is the
		// source locator, exactly like an import-lv adoption.
		agentVolID = lvmSourceLocator(existingPV.Spec.Recovery.Source)
	}
	volumeID := strings.Join(
		[]string{targetName, string(protocolID), string(backendID), agentVolID},
		"/",
	)
	if adoption != nil && pvExists {
		volumeID = existingPV.Spec.VolumeID
	}
	telemetry.SetVolumeAttributes(ctx, volumeID)
	setSpanAttributes(ctx,
		telemetry.KeyStoreName.String(res.storeName),
		telemetry.KeyCreateResumedFrom.String(createResumedFrom(existingPV, pvExists)),
	)
	if pvExists {
		s.sm.ForceState(volumeID, pillarVolumeStatePhaseToVolumeState(existingPV.Status.Phase))
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
		VolumeID:           volumeID,
		AgentVolumeID:      agentVolID,
		AgentRef:           targetName,
		BackendType:        string(backendID),
		ProtocolType:       string(protocolID),
		CapacityBytes:      capacityBytes,
		Resolved:           resolved,
		ImportedFrom:       importedFrom,
		FilesystemAdoption: adoption,
		LVMSource:          lvmSource,
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
		attempt = &v1alpha1.PillarVolumeState{Name: pvName, Spec: spec}
	}
	err = s.refuseAbandonedClaim(ctx, attempt)
	if err != nil {
		return nil, err
	}
	accessibleTopology, topologyErr := s.filesystemAccessibleTopology(ctx, attempt, req.GetVolumeCapabilities())
	if topologyErr != nil {
		return nil, topologyErr
	}
	err = validateFilesystemTopology(req.GetAccessibilityRequirements(), accessibleTopology)
	if err != nil {
		return nil, err
	}
	// An import reserves the backend volume before the PillarVolumeState
	// exists: the reservation's deterministic name makes the claim atomic,
	// so two concurrent imports of one zvol (or one LV, keyed by its UUID, or
	// one filesystem, keyed by its canonical native resource) cannot both
	// start a lifecycle.  The reservation belongs to the lifecycle and
	// outlives refused or failed attempts until the record is retired;
	// finishDelete releases it.
	var importRsv backendReservation
	var importSubject reservationSubject
	switch {
	case adoption != nil:
		resourceID, resourceErr := filesystemResourceID(adoption)
		if resourceErr != nil {
			return nil, invalidFilesystemDescriptor(resourceErr)
		}
		importRsv = backendReservation{
			agent: targetName, backendType: string(backendID), key: agentVolID, resourceID: resourceID,
		}
		importSubject = filesystemImportSubject(adoption.CanonicalSource)
	case importedFrom != "":
		importRsv = backendReservation{agent: targetName, backendType: string(backendID), key: agentVolID}
		importSubject = reservationSubject{annotation: v1alpha1.AnnotationImportZvol, noun: "zvol", source: importedFrom}
	case lvmSource != nil:
		importRsv = reservationOf(attempt)
		importSubject = lvImportSubject(lvmSource)
	}
	if importRsv.key != "" {
		err = s.reserveBackendVolume(ctx, pvName, importRsv, importSubject, attempt.Spec.ClaimRef)
		if err != nil {
			return nil, err
		}
	}
	pvs, err := s.ensureVolumeState(ctx, pvName, spec)
	if err != nil {
		return nil, err
	}
	if pvs.Spec.FilesystemAdoption != nil {
		volumeID = pvs.Spec.VolumeID
		agentVolID = pvs.Spec.AgentVolumeID
		targetName = pvs.Spec.AgentRef
		telemetry.SetVolumeAttributes(ctx, volumeID)
	}
	err = refuseDeleting(pvs, volumeID)
	if err != nil {
		return nil, err
	}
	if recoveryIntent {
		// The record's immutable spec is authoritative; the claim only
		// supplies the export configuration.  Adopting commits the record's
		// first status write (phase RecoveryPending, declared generation)
		// and fills the write-once fields the controller can supply.
		err = recoverySpecDrift(pvs, volumeID, agentVolID, targetName,
			string(protocolID), string(backendID), agentBackendType)
		if err != nil {
			return nil, err
		}
		err = s.adoptRecoveryIntent(ctx, pvName, pvs.UID, resolved)
		if err != nil {
			return nil, err
		}
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
	ctx = withAgentName(ctx, targetName)
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// ── Step 1: Create the backend storage resource ──────────────────────────
	bindIP := extractIP(agentAddr)
	exportParams, aclEnabled := exportParamsFromResolved(
		resolved.Protocol, bindIP,
		nfsReadonlyForCapabilities(resolved.Protocol.Kind(), req.GetVolumeCapabilities()),
	)
	// The durable export spec is recorded with the partial state so the resync
	// controller can re-create the export after the storage node loses its
	// target state, independent of later parameter changes.
	exportSpec := exportSpecFor(exportParams, aclEnabled)
	if localOnlyFilesystem(pvs) {
		exportSpec = nil
	}
	// A lifecycle already in CreatePartial created its backend in an earlier
	// attempt whose export failed; the device path recorded then is reused and
	// only the export is retried, so a zvol that may hold data is never
	// re-created.
	devicePath := pvs.Status.BackendDevicePath
	actualCapacity := capacityBytes
	switch {
	case recoveryIntent:
		// Ownership transfer instead of a backend create: the record's
		// intent, the persisted snapshot and the operator's authorization
		// drive InspectVolume + TransferVolumeOwnership; the committed
		// transfer records CreatePartial so a retry only re-exports.
		devicePath, actualCapacity, err = s.recoverVolume(ctx, agentClient,
			pvName, pvs, exportSpec, capacityBytes)
		if err != nil {
			return nil, err
		}
	case pvs.Status.Phase == v1alpha1.PillarVolumeStatePhaseCreatePartial &&
		(devicePath != "" || (adoption != nil && pvs.Status.ImportAcquired)):
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
	case adoption != nil:
		resourceID, resourceErr := filesystemResourceID(adoption)
		if resourceErr != nil {
			return nil, invalidFilesystemDescriptor(resourceErr)
		}
		err = s.verifyReservation(ctx, pvName, backendReservation{
			agent: targetName, backendType: string(backendID), key: agentVolID, resourceID: resourceID,
		}, filesystemImportSubject(adoption.CanonicalSource), pvs.Spec.ClaimRef)
		if err != nil {
			return nil, err
		}
		adoptionProto, adoptionProtoErr := filesystemAdoptionProto(adoption)
		if adoptionProtoErr != nil {
			return nil, invalidFilesystemDescriptor(adoptionProtoErr)
		}
		devicePath, actualCapacity, err = s.importBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			&agentv1.ImportVolumeRequest{
				VolumeId: agentVolID, CapacityBytes: pvs.Spec.CapacityBytes,
				BackendType: agentBackendType, FilesystemAdoption: adoptionProto,
				BackendParams: backendParamsFromResolved(recorded.Backend),
			}, exportSpec)
		if err != nil {
			return nil, err
		}
	case importedFrom != "":
		// Adopt the existing zvol named by the import annotation instead of
		// creating one.  The agent refuses when the zvol is missing, wrong
		// type, too small, still in use, or resolves to a dataset other than
		// the recorded source (ExpectedDataset pins the layout).  The
		// reservation is re-read uncached right before the agent call: an
		// attempt that lost it since reserving must not bind the zvol.
		err = s.verifyReservation(ctx, pvName, importRsv, importSubject, pvs.Spec.ClaimRef)
		if err != nil {
			return nil, err
		}
		devicePath, actualCapacity, err = s.importBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			&agentv1.ImportVolumeRequest{
				VolumeId:        agentVolID,
				CapacityBytes:   capacityBytes,
				BackendType:     agentBackendType,
				ExpectedDataset: importedFrom,
			}, exportSpec)
		if err != nil {
			return nil, err
		}
	case pvs.Spec.LVMSource != nil:
		// Adopt the LV pinned in the durable record (never the live
		// annotation): the agent verifies the name and both UUIDs, refuses
		// a layout, size or consumer it cannot adopt, and pins the policy
		// in its mark.  Nothing is created, formatted or resized.  The
		// UUID-keyed reservation is re-read right before the agent call.
		err = s.verifyReservation(ctx, pvName, reservationOf(pvs), lvImportSubject(pvs.Spec.LVMSource),
			pvs.Spec.ClaimRef)
		if err != nil {
			return nil, err
		}
		devicePath, actualCapacity, err = s.importBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			lvImportRequest(agentVolID, capacityBytes, pvs.Spec.LVMSource), exportSpec)
		if err != nil {
			return nil, err
		}
	default:
		// A normal create must never silently adopt a zvol that another
		// lifecycle imported under this volume's name.
		err = s.refuseAdoptionCollision(ctx, pvName, targetName, agentVolID)
		if err != nil {
			return nil, err
		}
		devicePath, actualCapacity, err = s.createBackend(ctx, agentClient, pvName, volumeID, pvs.UID,
			&agentv1.CreateVolumeRequest{
				VolumeId:      agentVolID,
				CapacityBytes: capacityBytes,
				BackendType:   agentBackendType,
				BackendParams: backendParamsFromResolved(recorded.Backend),
				AccessType:    volumeAccessType(recorded.Backend),
			}, exportSpec)
		if err != nil {
			return nil, err
		}
	}

	// ── Step 2: Export the volume over the network protocol ───────────────────
	// The export bind address is the storage node's IP (no port).
	// agent.ExportVolume is idempotent: if the export already exists (retry
	// scenario), it returns the existing ExportInfo without error.
	var info *agentv1.ExportInfo
	if !localOnlyFilesystem(pvs) {
		exportToken, tokenErr := s.claimOperation(ctx, pvName, volumeID, pvs.UID)
		if tokenErr != nil {
			return nil, tokenErr
		}
		var adoptionProto *agentv1.FilesystemAdoption
		if adoption := pvs.Spec.FilesystemAdoption; adoption != nil {
			var adoptionProtoErr error
			adoptionProto, adoptionProtoErr = filesystemAdoptionProto(adoption)
			if adoptionProtoErr != nil {
				return nil, invalidFilesystemDescriptor(adoptionProtoErr)
			}
		}
		exportResp, exportErr := agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
			VolumeId: agentVolID, ProtocolType: agentProtocolType,
			ExportParams: withISCSIChap(exportParams, chap), DevicePath: devicePath,
			AclEnabled: aclEnabled, Fence: exportToken,
			FilesystemAdoption: adoptionProto,
			CapacityBytes:      pvs.Spec.CapacityBytes, BackendParams: backendParamsFromResolved(recorded.Backend),
		})
		if exportErr != nil {
			return nil, status.Errorf(status.Code(exportErr), "agent ExportVolume(%q) failed: %v", agentVolID, exportErr)
		}
		info = exportResp.GetExportInfo()
		if info == nil {
			return nil, status.Errorf(codes.Internal, "agent export returned no connection information")
		}
	}

	// ── Record the lifecycle Ready before reporting success ──────────────────
	// The provisioner creates the PersistentVolume from this response, so the
	// Ready record must be durable first: a lifecycle that is not Ready never
	// has a PersistentVolume, which is what lets ReapAbandonedVolume end one
	// whose claim is gone.  A failure is returned; the retry re-exports
	// idempotently from CreatePartial and records Ready again.
	err = s.persistVolumeReady(ctx, pvName, pvs.UID, info)
	if err != nil {
		return nil, err
	}
	s.sm.ForceState(volumeID, StateCreated)

	// ── Build VolumeContext from ExportInfo ───────────────────────────────────
	// These key/value pairs are stored in the PersistentVolume and forwarded to
	// NodeStageVolume so the node can connect to the volume over the network.
	volumeContext := map[string]string{vcProtocolType: string(protocolID)}
	if info != nil {
		volumeContext[vcTargetID] = info.GetTargetId()
		volumeContext[vcAddress] = info.GetAddress()
		volumeContext[vcPort] = strconv.Itoa(int(info.GetPort()))
		volumeContext[vcVolumeRef] = info.GetVolumeRef()
	}
	nodeVolumeContext(recorded, volumeContext)
	err = addFilesystemPublishContext(pvs, volumeContext, "", agentAddr)
	if err != nil {
		return nil, err
	}
	addVolumePreserveContext(pvs, volumeContext)
	setSpanAttributes(ctx, telemetry.KeyCapacityAllocatedBytes.Int64(actualCapacity))

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           volumeID,
			CapacityBytes:      actualCapacity,
			VolumeContext:      volumeContext,
			AccessibleTopology: accessibleTopology,
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
	existingCap, err := completedVolumeCapacity(req, pvs)
	if err != nil {
		return nil, err
	}

	volumeContext := map[string]string{vcProtocolType: pvs.Spec.ProtocolType}
	if ei != nil {
		volumeContext[vcTargetID] = ei.TargetID
		volumeContext[vcAddress] = ei.Address
		volumeContext[vcPort] = strconv.Itoa(int(ei.Port))
		volumeContext[vcVolumeRef] = ei.VolumeRef
	}
	nodeVolumeContext(pvs.Spec.Resolved, volumeContext)
	err = addFilesystemPublishContext(pvs, volumeContext, "", "")
	if err != nil {
		return nil, err
	}
	addVolumePreserveContext(pvs, volumeContext)
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      pvs.Spec.VolumeID,
			CapacityBytes: existingCap,
			VolumeContext: volumeContext,
		},
	}, nil
}

func completedVolumeCapacity(req *csi.CreateVolumeRequest, pvs *v1alpha1.PillarVolumeState) (int64, error) {
	err := validateCompletedVolumeRequest(req, pvs)
	if err != nil {
		return 0, err
	}
	existingCap := pvs.Spec.CapacityBytes
	err = validateCompletedVolumeCapacityRange(req, pvs, existingCap)
	if err != nil {
		return 0, err
	}
	return existingCap, nil
}

func validateCompletedVolumeRequest(
	req *csi.CreateVolumeRequest,
	pvs *v1alpha1.PillarVolumeState,
) error {
	capabilityErr := validateCapabilitiesForProtocol(pvs.Spec.ProtocolType, req.GetVolumeCapabilities())
	if capabilityErr != nil {
		return status.Errorf(codes.InvalidArgument, "%v", capabilityErr)
	}
	if localOnlyFilesystem(pvs) && filesystemCapabilitiesMultiNode(req.GetVolumeCapabilities()) {
		return status.Errorf(codes.AlreadyExists,
			"local-only filesystem adoption cannot change to network access")
	}
	if v1alpha1.ProtocolID(pvs.Spec.ProtocolType) == v1alpha1.ProtocolIDNFS &&
		pvs.Status.ExportSpec != nil && pvs.Status.ExportSpec.NFS != nil &&
		pvs.Status.ExportSpec.NFS.Readonly && hasWritableVolumeCapability(req.GetVolumeCapabilities()) {
		return status.Errorf(codes.AlreadyExists,
			"volume %q already exists with a read-only NFS export", req.GetName())
	}
	return nil
}

func validateCompletedVolumeCapacityRange(
	req *csi.CreateVolumeRequest,
	pvs *v1alpha1.PillarVolumeState,
	existingCap int64,
) error {
	if cr := req.GetCapacityRange(); cr != nil {
		if cr.GetRequiredBytes() > 0 && existingCap < cr.GetRequiredBytes() {
			return status.Errorf(codes.AlreadyExists,
				"volume %q already exists with capacity %d bytes, which is less than "+
					"the requested minimum %d bytes",
				req.GetName(), existingCap, cr.GetRequiredBytes())
		}
		if cr.GetLimitBytes() > 0 && existingCap > cr.GetLimitBytes() {
			return status.Errorf(codes.AlreadyExists,
				"volume %q already exists with capacity %d bytes, which exceeds "+
					"the requested limit %d bytes",
				req.GetName(), existingCap, cr.GetLimitBytes())
		}
		if pvs.Spec.FilesystemAdoption != nil && cr.GetRequiredBytes() != existingCap {
			return status.Errorf(codes.AlreadyExists,
				"adopted filesystem capacity is fixed at %d bytes", existingCap)
		}
	}
	return nil
}

// commitBackendVolume commits a generation on the lifecycle uid, runs the
// backend RPC (create or import) with that fencing token, and records the
// CreatePartial state so a retry only re-exports.  It returns the device path
// and the allocated capacity; exportSpec is the durable export configuration
// persisted alongside the partial state for later resync.
func (s *ControllerServer) commitBackendVolume(
	ctx context.Context,
	pvName, volumeID string,
	uid types.UID,
	exportSpec *v1alpha1.VolumeExportSpec,
	call func(fence *agentv1.FencingToken) (devicePath string, capacity int64, err error),
) (devicePath string, capacity int64, err error) {
	fence, err := s.claimOperation(ctx, pvName, volumeID, uid)
	if err != nil {
		return "", 0, err
	}
	devicePath, capacity, err = call(fence)
	if err != nil {
		return "", 0, err
	}
	//nolint:errcheck // transition errors are non-fatal; state is force-set on success
	_, _ = s.sm.Transition(volumeID, OpCreateVolumeBackend)
	err = s.recordAllocatedCapacity(ctx, pvName, uid, capacity)
	if err != nil {
		return "", 0, err
	}
	err = s.persistCreatePartial(ctx, pvName, uid, devicePath, exportSpec)
	if err != nil {
		// Cannot durably record the partial state; fail so the CO retries
		// instead of the backend resource being silently forgotten.
		return "", 0, err
	}
	return devicePath, capacity, nil
}

// createBackend provisions the backend storage resource via agent.CreateVolume.
func (s *ControllerServer) createBackend(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	pvName, volumeID string,
	uid types.UID,
	req *agentv1.CreateVolumeRequest,
	exportSpec *v1alpha1.VolumeExportSpec,
) (devicePath string, capacity int64, err error) {
	return s.commitBackendVolume(ctx, pvName, volumeID, uid, exportSpec,
		func(fence *agentv1.FencingToken) (string, int64, error) {
			req.Fence = fence
			resp, callErr := agentClient.CreateVolume(ctx, req)
			if callErr != nil {
				grpcSt, _ := status.FromError(callErr)
				return "", 0, status.Errorf(grpcSt.Code(),
					"agent CreateVolume(%q) failed: %v", req.GetVolumeId(), callErr)
			}
			capacity = req.GetCapacityBytes()
			if allocated := resp.GetCapacityBytes(); allocated != 0 {
				capacity = allocated
			}
			return resp.GetDevicePath(), capacity, nil
		})
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
//  2. Destroy provisioned storage through agent.DeleteVolume, or retire an
//     adopted filesystem through agent.ReleaseVolume without touching its source.
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
	telemetry.SetVolumeAttributes(ctx, volumeID)

	// ── Parse the encoded volume ID ───────────────────────────────────────────
	// Format: <target-name>/<protocol-type>/<backend-type>/<agent-vol-id>
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		// Malformed ID — could be a volume provisioned by a different driver.
		// The CSI spec requires returning success for volumes that are unknown.
		return &csi.DeleteVolumeResponse{}, nil
	}
	targetName := parts[0]

	// Resolve the owning PillarVolumeState name; for an imported volume the
	// leaf is the source dataset name, so the owner is found by agentVolID.
	pvName, nameErr := s.volumeStateNameForID(ctx, volumeID)
	if nameErr != nil {
		return nil, nameErr
	}
	if pvName == "" {
		// No PillarVolumeState owns this backend volume: the driver never
		// issued the ID, and unknown volumes delete successfully.
		return &csi.DeleteVolumeResponse{}, nil
	}
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
	setClaimAttributes(ctx, pvs.Spec.ClaimRef)

	var adoptionProto *agentv1.FilesystemAdoption
	if adoption := pvs.Spec.FilesystemAdoption; adoption != nil {
		var adoptionProtoErr error
		adoptionProto, adoptionProtoErr = filesystemAdoptionProto(adoption)
		if adoptionProtoErr != nil {
			return nil, invalidFilesystemDescriptor(adoptionProtoErr)
		}
	}
	teardown := volumeTeardown{
		volumeID: volumeID, pvName: pvName, uid: pvs.UID, targetName: targetName,
		protocolType: mapProtocolType(parts[1]), backendType: mapBackendType(parts[2]),
		agentVolID: parts[3], fence: fence, releaseOnly: releaseOnlyTeardown(pvs),
		reservationKey: reservationOf(pvs), filesystemAdoption: adoptionProto,
	}
	if pvs.Spec.FilesystemAdoption != nil {
		teardown.targetName = pvs.Spec.AgentRef
		teardown.protocolType = mapProtocolType(pvs.Spec.ProtocolType)
		teardown.backendType = mapBackendType(pvs.Spec.BackendType)
		teardown.agentVolID = pvs.Spec.AgentVolumeID
	}
	err = s.teardownMarkedVolume(ctx, teardown)
	if err != nil {
		return nil, err
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// volumeTeardown identifies one lifecycle whose PillarVolumeState is already
// marked deleting, and routes its agent RPCs.
type volumeTeardown struct {
	volumeID           string
	pvName             string
	uid                types.UID
	targetName         string
	protocolType       agentv1.ProtocolType
	backendType        agentv1.BackendType
	agentVolID         string
	fence              *agentv1.FencingToken
	filesystemAdoption *agentv1.FilesystemAdoption
	// releaseOnly ends the lifecycle at the agent with ReleaseVolume instead
	// of UnexportVolume + DeleteVolume: the volume was an import whose agent
	// never durably adopted the backend resource, or an LV adopted under
	// PreserveOriginal, so its backend resource is pre-existing data this
	// driver must never destroy (see releaseOnlyTeardown).
	releaseOnly bool
	// reservationKey is the reservation of the lifecycle (reservationKey of
	// the marked record) that finishDelete releases.
	reservationKey backendReservation
}

// releaseOnlyTeardown reports whether the lifecycle pvs must end with
// ReleaseVolume only: an import never durably adopted, or an LV adoption or
// recovery whose policy preserves the original.  The agent then verifies
// consumer absence before it retires the lifecycle, and refuses otherwise.
func releaseOnlyTeardown(pvs *v1alpha1.PillarVolumeState) bool {
	return importNeverAdopted(pvs) || volumePreservesOriginal(pvs)
}

// importNeverAdopted reports whether pvs is an import lifecycle (zvol, LV or
// filesystem) that never durably recorded adoption: status.importAcquired is
// the explicit record written after a successful agent.ImportVolume;
// backendDevicePath and exportInfo are set only after the backend call
// succeeded, so states written by older versions still count as adopted.
func importNeverAdopted(pvs *v1alpha1.PillarVolumeState) bool {
	if pvs.Spec.ImportedFrom == "" && pvs.Spec.LVMSource == nil && pvs.Spec.FilesystemAdoption == nil {
		return false
	}
	return !pvs.Status.ImportAcquired &&
		pvs.Status.BackendDevicePath == "" && pvs.Status.ExportInfo == nil
}

// refuseUnadoptedImport refuses a mutating operation op on an import
// lifecycle that never durably adopted its zvol or LV (see
// importNeverAdopted): the source is still pre-existing data this driver
// does not own, so it must never be resized or exposed to a node, and a
// fenced grant for the lifecycle would bind the volume ID at the agent
// without an adoption.
func refuseUnadoptedImport(pvs *v1alpha1.PillarVolumeState, volumeID, op string) error {
	if !importNeverAdopted(pvs) {
		return nil
	}
	if src := pvs.Spec.LVMSource; src != nil {
		return status.Errorf(codes.FailedPrecondition,
			"cannot %s volume %q: its import of LV %q was never adopted "+
				"(status.importAcquired is unset); the pre-existing LV is left untouched",
			op, volumeID, lvmSourceLocator(src))
	}
	return status.Errorf(codes.FailedPrecondition,
		"cannot %s volume %q: its import of zvol %q was never adopted "+
			"(status.importAcquired is unset); the pre-existing zvol is left untouched",
		op, volumeID, pvs.Spec.ImportedFrom)
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
	// Every teardown, including the release of an import that was never
	// adopted, needs the agent: an import whose response was lost may have
	// bound the volume ID to this lifecycle, and only the agent can retire
	// that binding so a delayed ImportVolume cannot land after the record is
	// gone.  An unreachable agent keeps the record for the retry.
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
	ctx = withAgentName(ctx, t.targetName)
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; dial errors already handled

	// An import that was never durably adopted owns no backend resource, and
	// an LV adopted under PreserveOriginal must outlive the lifecycle:
	// ReleaseVolume removes this lifecycle's export if one landed and
	// retires the lifecycle at the agent, which first verifies no local
	// consumer remains, without touching the zvol or LV.  DeleteVolume
	// would destroy data this driver must keep.  A refusal or an unreachable
	// agent keeps the record (still marked deleting) and its reservation.
	if t.releaseOnly {
		_, releaseErr := agentClient.ReleaseVolume(ctx, &agentv1.ReleaseVolumeRequest{
			VolumeId:           t.agentVolID,
			ProtocolType:       t.protocolType,
			Fence:              t.fence,
			FilesystemAdoption: t.filesystemAdoption,
		})
		if releaseErr != nil {
			return status.Errorf(status.Code(releaseErr),
				"agent ReleaseVolume(%q) failed: %v", t.agentVolID, releaseErr)
		}
		return s.finishDelete(ctx, t.volumeID, t.pvName, t.uid, t.reservationKey)
	}

	// ── Step 1: Remove the network export (idempotent) ────────────────────────
	_, unexportErr := agentClient.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:           t.agentVolID,
		ProtocolType:       t.protocolType,
		Fence:              t.fence,
		FilesystemAdoption: t.filesystemAdoption,
	})
	unexportCode := status.Code(unexportErr)
	if unexportErr != nil && unexportCode != codes.NotFound {
		return status.Errorf(unexportCode,
			"agent UnexportVolume(%q) failed: %v", t.agentVolID, unexportErr)
	}
	if t.filesystemAdoption != nil {
		_, releaseErr := agentClient.ReleaseVolume(ctx, &agentv1.ReleaseVolumeRequest{
			VolumeId: t.agentVolID, ProtocolType: t.protocolType, Fence: t.fence,
			FilesystemAdoption: t.filesystemAdoption,
		})
		if releaseErr != nil {
			return status.Errorf(status.Code(releaseErr),
				"agent ReleaseVolume(%q) failed: %v", t.agentVolID, releaseErr)
		}
		return s.finishDelete(ctx, t.volumeID, t.pvName, t.uid, t.reservationKey)
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

	return s.finishDelete(ctx, t.volumeID, t.pvName, t.uid, t.reservationKey)
}

// finishDelete forgets the volume in memory and removes the lifecycle's
// PillarVolumeState.  A failure is returned so the caller retries: the retry
// finds the record still marked deleting and repeats the idempotent steps.
func (s *ControllerServer) finishDelete(
	ctx context.Context,
	volumeID, pvName string,
	uid types.UID,
	rsv backendReservation,
) error {
	// Release the backend-volume reservation before the record goes away:
	// once the PillarVolumeState is gone a leftover reservation is orphaned
	// and would block a re-import of the same zvol or LV until a contender
	// reclaims it.  Releasing only while this lifecycle owns it keeps a
	// reservation a contender already took.
	err := s.releaseBackendVolume(ctx, pvName, volumeID, uid, rsv)
	if err != nil {
		return err
	}
	if s.effectiveDriverName() == v1alpha1.FileCSIDriver {
		current, exists, readErr := s.readVolumeState(ctx, pvName)
		if readErr != nil {
			return status.Errorf(codes.Internal, "recheck file cleanup lifecycle: %v", readErr)
		}
		if exists && (current.UID != uid || !current.Status.Deleting ||
			scopedDriverForVolume(current) != v1alpha1.FileCSIDriver) {
			return status.Errorf(
				codes.Aborted,
				"file cleanup lifecycle changed before final deletion; retry",
			)
		}
	}
	err = s.deleteVolumeState(ctx, pvName, uid)
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
	if _, suffix, found := strings.CutLast(agentVolID, "/"); found {
		return suffix
	}
	return agentVolID
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
	case v1alpha1.BackendIDZFSDataset:
		return agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET
	case v1alpha1.BackendIDDirectory:
		return agentv1.BackendType_BACKEND_TYPE_DIRECTORY
	case v1alpha1.BackendIDLVMLV:
		return agentv1.BackendType_BACKEND_TYPE_LVM
	default:
		return agentv1.BackendType_BACKEND_TYPE_UNSPECIFIED
	}
}

func volumeAccessType(b v1alpha1.BackendSpec) agentv1.VolumeAccessType {
	if b.Directory != nil || (b.ZFS != nil && b.ZFS.VolumeType == v1alpha1.ZFSVolumeTypeDataset) {
		return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_MOUNT
	}
	return agentv1.VolumeAccessType_VOLUME_ACCESS_TYPE_BLOCK
}

// mapProtocolType converts a protocol routing token (the protocol segment of
// a volume ID, v1alpha1.ProtocolID) to the agent protobuf enum value.  An
// unsupported token maps to UNSPECIFIED, which the agent rejects explicitly.
func mapProtocolType(s string) agentv1.ProtocolType {
	switch v1alpha1.ProtocolID(s) {
	case v1alpha1.ProtocolIDNVMeOFTCP:
		return agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP
	case v1alpha1.ProtocolIDISCSI:
		return agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI
	case v1alpha1.ProtocolIDNFS:
		return agentv1.ProtocolType_PROTOCOL_TYPE_NFS
	default:
		return agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED
	}
}

// resolveInitiatorID looks up the protocol-specific initiator identity for a
// Kubernetes node.
//
// Identity resolution by protocol:
//
//	NVMe-oF TCP → CSINode.annotations["pillar-csi.bhyoo.com/nvmeof-host-nqn"]
//	iSCSI       → CSINode.annotations["pillar-csi.bhyoo.com/iscsi-initiator-iqn"]
//	NFS         → Node.status.addresses[InternalIP] (numeric IP)
//	other       → nodeID unchanged (the agent rejects an unsupported protocol)
//
// Returns FailedPrecondition if the node identity is not available yet:
// a missing CSINode/annotation, or a missing Node/numeric InternalIP.
// The CO retries with backoff while the node registers.
func (s *ControllerServer) resolveInitiatorID(ctx context.Context, nodeID, protocolTypeStr string) (string, error) {
	if v1alpha1.ProtocolID(protocolTypeStr) == v1alpha1.ProtocolIDNFS {
		return s.resolveNFSInitiatorID(ctx, nodeID, "")
	}

	var annotationKey string
	switch v1alpha1.ProtocolID(protocolTypeStr) {
	case v1alpha1.ProtocolIDNVMeOFTCP:
		annotationKey = AnnotationNVMeOFHostNQN
	case v1alpha1.ProtocolIDISCSI:
		annotationKey = AnnotationISCSIInitiatorIQN
	default:
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

// resolveNFSInitiatorID selects a numeric InternalIP matching the storage
// address family. An empty bindAddress resolves the first InternalIP for
// callers that have not yet selected a storage agent.
func (s *ControllerServer) resolveNFSInitiatorID(
	ctx context.Context,
	nodeID, bindAddress string,
) (string, error) {
	node := &corev1.Node{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: nodeID}, node)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return "", status.Errorf(codes.FailedPrecondition,
				"Node %q not found; node plugin may not have registered yet", nodeID)
		}
		return "", status.Errorf(codes.Internal, "failed to get Node %q: %v", nodeID, err)
	}
	var firstIP string
	for _, addr := range node.Status.Addresses {
		if addr.Type != corev1.NodeInternalIP {
			continue
		}
		if net.ParseIP(addr.Address) == nil {
			return "", status.Errorf(codes.FailedPrecondition,
				"Node %q has non-numeric InternalIP %q", nodeID, addr.Address)
		}
		if firstIP == "" {
			firstIP = addr.Address
		}
		if bindAddress == "" {
			return addr.Address, nil
		}
		familyErr := validateNFSInitiatorFamily(addr.Address, bindAddress)
		if familyErr == nil {
			return addr.Address, nil
		}
		if status.Code(familyErr) == codes.FailedPrecondition {
			return "", familyErr
		}
	}
	if firstIP == "" {
		return "", status.Errorf(codes.FailedPrecondition, "Node %q has no InternalIP", nodeID)
	}
	return "", validateNFSInitiatorFamily(firstIP, bindAddress)
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
	case b.Directory != nil:
		return &agentv1.BackendParams{Params: &agentv1.BackendParams_Directory{
			Directory: &agentv1.DirectoryVolumeParams{
				LogicalPool: b.Directory.LogicalPool, HostRoot: b.Directory.HostRoot,
			},
		}}
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

// defaultISCSIPort is the iSCSI target portal port when the protocol does not
// set one (the CRD default).
const defaultISCSIPort = 3260

// exportParamsFromResolved constructs the agent ExportVolume parameters and
// the ACL flag from the resolved protocol configuration.  The bind address is
// the storage node's IP (PillarAgent.status.resolvedAddress without its port).
func exportParamsFromResolved(
	p v1alpha1.ProtocolSpec,
	bindAddress string,
	readonly bool,
) (*agentv1.ExportParams, bool) {
	switch {
	case p.NVMeOFTCP != nil:
		n := p.NVMeOFTCP
		port := n.Port
		if port == 0 {
			port = defaultNVMeOFPort
		}
		var inCapsuleDataSize int32
		if n.InCapsuleDataSize != nil {
			inCapsuleDataSize = *n.InCapsuleDataSize
		}
		var maxDataTransferSize *int32
		if n.MaxDataTransferSize != nil {
			v := *n.MaxDataTransferSize
			maxDataTransferSize = &v
		}
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_NvmeofTcp{
				NvmeofTcp: &agentv1.NvmeofTcpExportParams{
					BindAddress:         bindAddress,
					Port:                port,
					InCapsuleDataSize:   inCapsuleDataSize,
					MaxDataTransferSize: maxDataTransferSize,
				},
			},
		}, n.ACL
	case p.ISCSI != nil:
		i := p.ISCSI
		port := i.Port
		if port == 0 {
			port = defaultISCSIPort
		}
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_Iscsi{
				Iscsi: &agentv1.IscsiExportParams{
					BindAddress: bindAddress,
					Port:        port,
				},
			},
		}, i.ACL
	case p.NFS != nil:
		n := p.NFS
		return &agentv1.ExportParams{
			Params: &agentv1.ExportParams_Nfs{
				Nfs: &agentv1.NfsExportParams{
					Version:     n.EffectiveVersion(),
					BindAddress: bindAddress,
					Squash:      string(n.EffectiveSquash()),
					Readonly:    readonly,
				},
			},
		}, n.ACL
	default:
		return nil, false
	}
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
//
// NodeServer keeps its own set that serializes NodeStageVolume,
// NodeUnstageVolume, NodeExpandVolume and each periodic trim chunk.
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
	vl := l.acquireRef(volumeID)
	vl.mu.Lock()
	return func() {
		vl.mu.Unlock()
		l.releaseRef(volumeID, vl)
	}
}

// tryLock takes the lock for volumeID only when no other caller holds it.
// It returns ok false (and a nil unlock) when the lock is busy.
func (l *volumeLockSet) tryLock(volumeID string) (unlock func(), ok bool) {
	vl := l.acquireRef(volumeID)
	if !vl.mu.TryLock() {
		l.releaseRef(volumeID, vl)
		return nil, false
	}
	return func() {
		vl.mu.Unlock()
		l.releaseRef(volumeID, vl)
	}, true
}

// acquireRef returns the entry of volumeID, creating it, with one more
// reference.  The zero volumeLockSet is ready to use.
func (l *volumeLockSet) acquireRef(volumeID string) *volumeLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = make(map[string]*volumeLock)
	}
	vl, ok := l.locks[volumeID]
	if !ok {
		vl = &volumeLock{}
		l.locks[volumeID] = vl
	}
	vl.refs++
	return vl
}

// releaseRef drops one reference to vl and removes the idle entry.
func (l *volumeLockSet) releaseRef(volumeID string, vl *volumeLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	vl.refs--
	if vl.refs == 0 {
		delete(l.locks, volumeID)
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
	if !isSupportedAccessMode(mode) ||
		(mode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER &&
			v1alpha1.ProtocolID(protocolTypeStr) != v1alpha1.ProtocolIDNFS) {
		return status.Errorf(codes.InvalidArgument,
			"access mode %s is not supported for protocol %q; supported modes: %s",
			mode, protocolTypeStr, describeSupportedModes())
	}
	return nil
}

// resolvePublishInitiator resolves the node's protocol initiator identity for
// ControllerPublishVolume.  CSI spec §4.5.1: an unknown node_id must return
// NotFound. Missing Node and CSINode objects produce FailedPrecondition
// during identity resolution ("retry later, still registering"); publish
// promotes that signal to NotFound. A missing annotation or InternalIP
// remains FailedPrecondition.
func (s *ControllerServer) resolvePublishInitiator(
	ctx context.Context,
	nodeID, protocolTypeStr string,
) (string, error) {
	initiatorID, err := s.resolveInitiatorID(ctx, nodeID, protocolTypeStr)
	if err == nil {
		return initiatorID, nil
	}
	return "", publishInitiatorError(nodeID, err)
}

func publishInitiatorError(nodeID string, err error) error {
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition &&
		strings.Contains(st.Message(), "node plugin may not have registered yet") {
		return status.Errorf(codes.NotFound, "node %q not found", nodeID)
	}
	return err
}

// ControllerPublishVolume grants a specific node access to a volume by
// calling agent.AllowInitiator on the storage node.
//
// The node_id in the request is the Kubernetes node name (stable handle).
// Block initiators use the CSINode annotation written by the node plugin;
// NFS uses a numeric Node InternalIP matching the storage address family:
//
//	NVMe-oF TCP → CSINode["pillar-csi.bhyoo.com/nvmeof-host-nqn"] (host NQN)
//	iSCSI       → CSINode["pillar-csi.bhyoo.com/iscsi-initiator-iqn"] (initiator IQN)
//	NFS         → Node.status.addresses[InternalIP]
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
//
// Adopted filesystems always use an owned direct proxy on their storage node,
// including RWX publications. Their direct attachment does not disable remote
// NFS or set the block-exclusive status.localAttachNode. A local-only adoption
// cannot be published remotely.
func (s *ControllerServer) ControllerPublishVolume(
	ctx context.Context,
	req *csi.ControllerPublishVolumeRequest,
) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	setPublishTargetAttributes(ctx, volumeID, nodeID)

	publishReq, err := parsePublishVolumeRequest(req)
	if err != nil {
		return nil, err
	}
	unlock := s.volumeLocks.lock(publishReq.volumeID)
	defer unlock()

	target, err := s.resolvePublishTarget(ctx, publishReq, req)
	if err != nil {
		return nil, err
	}
	publication := v1alpha1.VolumePublication{
		NodeID:      publishReq.nodeID,
		InitiatorID: target.initiatorID,
		AccessMode:  publishReq.mode.String(),
		Readonly:    req.GetReadonly(),
		Local:       target.local,
	}
	fence, newPublication, err := s.reservePublicationState(
		ctx, target.pvName, publishReq.volumeID, target.pvs.UID, publication,
	)
	if err != nil {
		return nil, err
	}

	ctx = withAgentName(ctx, target.targetName)
	return s.finishPublish(ctx, target.local, target.pvName, target.agentAddr,
		publishReq.volumeID, target.agentVolID, target.agentProtocolType,
		publishReq.nodeID, target.initiatorID, target.pvs, fence, target.chap,
		publication, newPublication, target.digestContext)
}

type publishVolumeRequest struct {
	volumeID          string
	nodeID            string
	targetName        string
	protocolType      string
	agentVolID        string
	mode              csi.VolumeCapability_AccessMode_Mode
	agentProtocolType agentv1.ProtocolType
}

func parsePublishVolumeRequest(req *csi.ControllerPublishVolumeRequest) (publishVolumeRequest, error) {
	err := validatePublishRequest(req)
	if err != nil {
		return publishVolumeRequest{}, err
	}
	volumeID := req.GetVolumeId()
	// CSI §4.5.1 treats an unknown encoded ID as NotFound, not InvalidArgument.
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return publishVolumeRequest{}, status.Errorf(codes.NotFound,
			"volume %q not found (unknown volume_id format)", volumeID)
	}
	mode := req.GetVolumeCapability().GetAccessMode().GetMode()
	err = validatePublishAccessMode(parts[1], mode)
	if err != nil {
		return publishVolumeRequest{}, err
	}
	return publishVolumeRequest{
		volumeID:          volumeID,
		nodeID:            req.GetNodeId(),
		targetName:        parts[0],
		protocolType:      parts[1],
		agentVolID:        parts[3],
		mode:              mode,
		agentProtocolType: mapProtocolType(parts[1]),
	}, nil
}

type publishVolumeTarget struct {
	pvName            string
	pvs               *v1alpha1.PillarVolumeState
	targetName        string
	agentAddr         string
	agentVolID        string
	agentProtocolType agentv1.ProtocolType
	local             bool
	initiatorID       string
	chap              *agentv1.IscsiChap
	// digestContext carries the NVMe/TCP digest flags of the volume's
	// current PillarProtocol for a protocol publish (nil otherwise).
	digestContext map[string]string
}

func (s *ControllerServer) resolvePublishTarget(
	ctx context.Context,
	publishReq publishVolumeRequest,
	req *csi.ControllerPublishVolumeRequest,
) (publishVolumeTarget, error) {
	pvName, pvs, err := s.mustVolumeState(ctx, publishReq.volumeID)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	targetName := publishReq.targetName
	agentVolID := publishReq.agentVolID
	protocolType := publishReq.protocolType
	agentProtocolType := publishReq.agentProtocolType
	if pvs.Spec.FilesystemAdoption != nil {
		targetName = pvs.Spec.AgentRef
		agentVolID = pvs.Spec.AgentVolumeID
		protocolType = pvs.Spec.ProtocolType
		agentProtocolType = mapProtocolType(pvs.Spec.ProtocolType)
	}
	err = validateStoredPublishCapabilities(pvs, req)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	setClaimAttributes(ctx, pvs.Spec.ClaimRef)
	err = refuseUnadoptedImport(pvs, publishReq.volumeID, "publish")
	if err != nil {
		return publishVolumeTarget{}, err
	}
	err = refuseRecoveryPending(pvs, publishReq.volumeID)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	agent, err := s.getReadyAgent(ctx, targetName)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	agentAddr := agent.Status.ResolvedAddress
	local := isLocalAttachPublish(pvs, agent, publishReq.nodeID, publishReq.mode)
	err = s.validateFilesystemPublish(ctx, pvs, publishReq.mode, local)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	setAttachAttributes(ctx, local, req.GetReadonly())
	initiatorID, chap, err := s.resolvePublishGrant(
		ctx, local, publishReq.nodeID, protocolType, extractIP(agentAddr), pvs)
	if err != nil {
		return publishVolumeTarget{}, err
	}
	// The digests are read before any publish side effect so a failed read
	// leaves nothing to compensate.  A local attach bypasses the network.
	var digestContext map[string]string
	if !local && protocolType == ProtocolNVMeoFTCP {
		digestContext, err = s.nvmeofDigestPublishContext(ctx, pvName, publishReq.volumeID)
		if err != nil {
			return publishVolumeTarget{}, err
		}
	}
	return publishVolumeTarget{
		pvName:            pvName,
		pvs:               pvs,
		targetName:        targetName,
		agentAddr:         agentAddr,
		agentVolID:        agentVolID,
		agentProtocolType: agentProtocolType,
		local:             local,
		initiatorID:       initiatorID,
		chap:              chap,
		digestContext:     digestContext,
	}, nil
}

func (s *ControllerServer) validateFilesystemPublish(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
	mode csi.VolumeCapability_AccessMode_Mode,
	local bool,
) error {
	if pvs.Spec.FilesystemAdoption == nil {
		return nil
	}
	if !local && (localOnlyFilesystem(pvs) || !filesystemMultiNodeMode(mode)) {
		return status.Errorf(
			codes.FailedPrecondition,
			"local filesystem volume is accessible only on its storage node",
		)
	}
	return s.revalidateFilesystemImport(ctx, pvs)
}

func validatePublishRequest(req *csi.ControllerPublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetNodeId() == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return status.Error(codes.InvalidArgument, "node_id is required")
	}
	if req.GetVolumeCapability() == nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return status.Error(codes.InvalidArgument, "volume_capability is required")
	}
	return nil
}

func validateStoredPublishCapabilities(
	pvs *v1alpha1.PillarVolumeState,
	req *csi.ControllerPublishVolumeRequest,
) error {
	capabilityErr := validateCapabilityForProtocol(pvs.Spec.ProtocolType, req.GetVolumeCapability())
	if capabilityErr != nil {
		return status.Errorf(codes.InvalidArgument, "%v", capabilityErr)
	}
	if v1alpha1.ProtocolID(pvs.Spec.ProtocolType) != v1alpha1.ProtocolIDNFS {
		return nil
	}
	if pvs.Spec.FilesystemAdoption == nil && pvs.Spec.Resolved != nil && pvs.Spec.Resolved.LocalAttach {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return status.Error(codes.InvalidArgument, "localAttach is not supported for NFS volumes")
	}
	mode := req.GetVolumeCapability().GetAccessMode().GetMode()
	if pvs.Status.ExportSpec != nil && pvs.Status.ExportSpec.NFS != nil &&
		pvs.Status.ExportSpec.NFS.Readonly && mode != csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return status.Error(codes.InvalidArgument, "NFS volume export is read-only")
	}
	return nil
}

// finishPublish completes a publish whose publication is already committed
// under fence: local publishes fence the export and return the backend
// device path, protocol publishes re-enable the export of a localAttach
// volume (returning it from a previous local attach when
// status.localAttachNode is set) and grant the initiator with the volume's
// CHAP credentials (chap, nil for none). A failed grant compensates only a
// publication newly appended by this attempt; an idempotent retry keeps the
// existing record fail-closed.  A protocol publish returns digestContext
// (the NVMe/TCP digest flags) in its PublishContext.
func (s *ControllerServer) finishPublish(
	ctx context.Context,
	local bool,
	pvName, agentAddr, volumeID, agentVolID string,
	protocolType agentv1.ProtocolType,
	nodeID, initiatorID string,
	pvs *v1alpha1.PillarVolumeState,
	fence *agentv1.FencingToken,
	chap *agentv1.IscsiChap,
	publication v1alpha1.VolumePublication,
	newPublication bool,
	digestContext map[string]string,
) (*csi.ControllerPublishVolumeResponse, error) {
	if local {
		return s.finishLocalPublish(ctx, agentAddr, volumeID, agentVolID, protocolType, nodeID, fence, pvs)
	}
	fence, err := s.restorePublishedExport(ctx, pvName, agentAddr, agentVolID, protocolType, pvs, fence)
	if err != nil {
		return nil, err
	}
	err = s.grantPublishedExport(ctx, agentAddr, agentVolID, protocolType, initiatorID,
		fence, chap, pvs, pvName, newPublication, publication)
	if err != nil {
		return nil, err
	}

	// Durable records order exclusivity; the state machine only mirrors that
	// at least one node is published, including across controller restarts.
	s.sm.ForceState(volumeID, StateControllerPublished)
	publishContext := maps.Clone(digestContext)
	if publishContext == nil {
		publishContext = map[string]string{}
	}
	err = addFilesystemPublishContext(pvs, publishContext, "", agentAddr)
	if err != nil {
		return nil, err
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
}

// restorePublishedExport gates the return from local attach at the reserved
// fence, clears the durable local node under a newer generation, then re-enables
// at that new fence before granting. Even an already-cleared localAttach record
// is re-enabled at its reservation so a delayed stale resync cannot leave the
// export disabled under a successful protocol publish.
func (s *ControllerServer) restorePublishedExport(
	ctx context.Context,
	pvName, agentAddr, agentVolID string,
	protocolType agentv1.ProtocolType,
	pvs *v1alpha1.PillarVolumeState,
	fence *agentv1.FencingToken,
) (*agentv1.FencingToken, error) {
	if prevLocal := pvs.Status.LocalAttachNode; prevLocal != "" {
		_, err := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence, pvs)
		if err != nil {
			return nil, err
		}
		fence, err = s.clearLocalAttachNode(ctx, pvName, pvs.UID, prevLocal)
		if err != nil {
			return nil, err
		}
		_, err = s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence, pvs)
		if err != nil {
			return nil, err
		}
		return fence, nil
	}
	if r := pvs.Spec.Resolved; pvs.Spec.FilesystemAdoption == nil && r != nil && r.LocalAttach {
		_, err := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, false, fence, pvs)
		if err != nil {
			return nil, err
		}
	}
	return fence, nil
}

// grantPublishedExport skips block ACL-off grants but keeps the filesystem
// identity/quota check. A failed new filesystem grant is fenced and denied before
// its record can be removed; retries and failed compensation stay fail-closed.
func (s *ControllerServer) grantPublishedExport(
	ctx context.Context,
	agentAddr, agentVolID string,
	protocolType agentv1.ProtocolType,
	initiatorID string,
	fence *agentv1.FencingToken,
	chap *agentv1.IscsiChap,
	pvs *v1alpha1.PillarVolumeState,
	pvName string,
	newPublication bool,
	publication v1alpha1.VolumePublication,
) error {
	if !exportACLEnabled(pvs) && pvs.Spec.FilesystemAdoption == nil {
		return nil
	}
	grantErr := s.grantPublication(ctx, agentAddr, agentVolID, protocolType, initiatorID,
		fence, grantExportParams(protocolType, pvs.Status.ExportSpec, chap), pvs)
	if grantErr == nil {
		return nil
	}
	if newPublication && pvs.Spec.FilesystemAdoption != nil {
		rollbackErr := s.rollbackNewPublication(
			ctx, agentAddr, agentVolID, protocolType, pvName, pvs.UID, publication, pvs,
		)
		if rollbackErr != nil {
			logf.FromContext(ctx).Error(rollbackErr, "Failed to roll back new publication",
				"volume", agentVolID, "node", publication.NodeID)
		}
	}
	return grantErr
}

// isLocalAttachPublish selects a direct attachment on a real storage NodeRef.
// Filesystems qualify regardless of access mode; blocks retain their explicit
// localAttach and SINGLE_NODE requirements.
func isLocalAttachPublish(
	pvs *v1alpha1.PillarVolumeState,
	agent *v1alpha1.PillarAgent,
	nodeID string,
	mode csi.VolumeCapability_AccessMode_Mode,
) bool {
	if pvs.Spec.FilesystemAdoption == nil && (pvs.Spec.Resolved == nil || !pvs.Spec.Resolved.LocalAttach) {
		return false
	}
	if agent.Spec.External != nil || agent.Spec.NodeRef == nil || agent.Spec.NodeRef.Name != nodeID {
		return false
	}
	if pvs.Spec.FilesystemAdoption != nil {
		return true
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

// finishLocalPublish applies the fenced direct attachment and returns its real
// path. Blocks fence the network export; filesystems mount an owned proxy without
// changing the source or disabling simultaneous NFS access.
func (s *ControllerServer) finishLocalPublish(
	ctx context.Context,
	agentAddr, volumeID, agentVolID string,
	protocolType agentv1.ProtocolType,
	nodeID string,
	fence *agentv1.FencingToken,
	recorded *v1alpha1.PillarVolumeState,
) (*csi.ControllerPublishVolumeResponse, error) {
	devicePath, err := s.setLocalAttach(ctx, agentAddr, agentVolID, protocolType, true, fence, recorded)
	if err != nil {
		return nil, err
	}
	if devicePath == "" {
		return nil, status.Errorf(codes.Internal,
			"agent SetLocalAttach(%q, local=true) returned no device path", agentVolID)
	}
	s.sm.ForceState(volumeID, StateControllerPublished)
	publishContext := map[string]string{
		PublishContextKeyAttachMode:      AttachModeLocal,
		PublishContextKeyLocalNode:       nodeID,
		PublishContextKeyLocalDevicePath: devicePath,
	}
	if recordedFilesystemAdoption(recorded) != nil {
		publishContext[PublishContextKeyFilesystemLocalNode] = nodeID
	}
	err = addFilesystemPublishContext(recorded, publishContext, devicePath, agentAddr)
	if err != nil {
		return nil, err
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
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
	recorded *v1alpha1.PillarVolumeState,
) (string, error) {
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return "", status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closeAgentConnection(ctx, closer)

	var adoptionProto *agentv1.FilesystemAdoption
	if adoption := recordedFilesystemAdoption(recorded); adoption != nil {
		var adoptionProtoErr error
		adoptionProto, adoptionProtoErr = filesystemAdoptionProto(adoption)
		if adoptionProtoErr != nil {
			return "", invalidFilesystemDescriptor(adoptionProtoErr)
		}
	}
	resp, err := agentClient.SetLocalAttach(ctx, &agentv1.SetLocalAttachRequest{
		VolumeId:           agentVolID,
		ProtocolType:       protocolType,
		Local:              local,
		Fence:              fence,
		FilesystemAdoption: adoptionProto,
		CapacityBytes:      recordedFilesystemCapacity(recorded),
		BackendParams:      recordedBackendParams(recorded),
	})
	if err != nil {
		return "", status.Errorf(status.Code(err),
			"agent SetLocalAttach(%q, local=%t) failed: %v", agentVolID, local, err)
	}
	return resp.GetDevicePath(), nil
}

// closeAgentConnection keeps transport cleanup best-effort without hiding errors.
func closeAgentConnection(ctx context.Context, closer io.Closer) {
	closeErr := closer.Close()
	if closeErr != nil {
		logf.FromContext(ctx).Error(closeErr, "Failed to close agent connection")
	}
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
	if r := pvs.Spec.Resolved; r != nil && r.Protocol.ISCSI != nil {
		return r.Protocol.ISCSI.ACL
	}
	if r := pvs.Spec.Resolved; r != nil && r.Protocol.NFS != nil {
		return r.Protocol.NFS.ACL
	}
	return true
}

// Grant access using the protocol-specific identity resolved from CSINode.
// ExportParams carries the grant's protocol parameters (the iSCSI CHAP
// credentials of the new ACL); nil when the grant needs none.
func (s *ControllerServer) grantPublication(
	ctx context.Context,
	agentAddr string,
	agentVolID string,
	agentProtocolType agentv1.ProtocolType,
	initiatorID string,
	fence *agentv1.FencingToken,
	exportParams *agentv1.ExportParams,
	recorded *v1alpha1.PillarVolumeState,
) error {
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closeAgentConnection(ctx, closer)

	var adoptionProto *agentv1.FilesystemAdoption
	if adoption := recordedFilesystemAdoption(recorded); adoption != nil {
		var adoptionProtoErr error
		adoptionProto, adoptionProtoErr = filesystemAdoptionProto(adoption)
		if adoptionProtoErr != nil {
			return invalidFilesystemDescriptor(adoptionProtoErr)
		}
	}
	allowResp, allowErr := agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:           agentVolID,
		ProtocolType:       agentProtocolType,
		InitiatorId:        initiatorID,
		Fence:              fence,
		ExportParams:       exportParams,
		FilesystemAdoption: adoptionProto,
		CapacityBytes:      recordedFilesystemCapacity(recorded),
		BackendParams:      recordedBackendParams(recorded),
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

// rollbackNewPublication fences a failed new grant before issuing an
// idempotent deny. The newer token orders the deny after an AllowInitiator
// whose response was lost or whose side effects were only partially applied.
// The publication is removed only when the expected record was fenced and the
// deny succeeded; otherwise it remains revoking for a fail-closed retry.
func (s *ControllerServer) rollbackNewPublication(
	ctx context.Context,
	agentAddr, agentVolID string,
	protocolType agentv1.ProtocolType,
	pvName string,
	uid types.UID,
	publication v1alpha1.VolumePublication,
	recorded *v1alpha1.PillarVolumeState,
) error {
	fence, found, err := s.fencePublication(ctx, pvName, uid, publication)
	if err != nil {
		return err
	}
	if !found {
		// A completed revoke may have removed the expected record while the
		// failed publish was waiting. Its newer fence already orders out the
		// failed grant; do not deny by initiator alone, since that initiator may
		// belong to a later publication on the same node.
		return nil
	}
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return status.Errorf(codes.Unavailable,
			"failed to dial agent at %q for publication rollback: %v", agentAddr, err)
	}
	defer closeAgentConnection(ctx, closer)

	var adoptionProto *agentv1.FilesystemAdoption
	if adoption := recordedFilesystemAdoption(recorded); adoption != nil {
		adoptionProto, err = filesystemAdoptionProto(adoption)
		if err != nil {
			return invalidFilesystemDescriptor(err)
		}
	}
	err = denyPublication(
		ctx, agentClient, agentVolID, protocolType, fence, adoptionProto, publication,
	)
	if err != nil {
		return err
	}
	_, err = s.releasePublication(ctx, pvName, uid, []string{publication.NodeID}, fence)
	return err
}

// grantExportParams returns the AllowInitiator export parameters for a grant
// with chap: the volume's iSCSI export parameters carrying chap, or nil when
// chap is nil.
func grantExportParams(
	protocolType agentv1.ProtocolType,
	spec *v1alpha1.VolumeExportSpec,
	chap *agentv1.IscsiChap,
) *agentv1.ExportParams {
	if chap == nil {
		return nil
	}
	params := &agentv1.ExportParams{Params: &agentv1.ExportParams_Iscsi{Iscsi: &agentv1.IscsiExportParams{}}}
	if spec != nil {
		params = exportParamsFor(protocolType, spec)
	}
	return withISCSIChap(params, chap)
}

// resolvePublishGrant returns what a publish of pvs to nodeID grants: the
// initiator identity from the node's CSINode annotation or NFS InternalIP
// (the node ID itself for a local attach, which grants no initiator and
// needs no node-plugin identity) and CHAP credentials from the volume's
// Secret when the export has an ACL. Missing identities and missing or
// invalid Secrets fail before any publication is recorded or granted.
func (s *ControllerServer) resolvePublishGrant(
	ctx context.Context,
	local bool,
	nodeID, protocolTypeStr, bindAddress string,
	pvs *v1alpha1.PillarVolumeState,
) (string, *agentv1.IscsiChap, error) {
	if local {
		return nodeID, nil, nil
	}
	var initiatorID string
	var err error
	if v1alpha1.ProtocolID(protocolTypeStr) == v1alpha1.ProtocolIDNFS {
		initiatorID, err = s.resolveNFSInitiatorID(ctx, nodeID, bindAddress)
		err = publishInitiatorError(nodeID, err)
	} else {
		initiatorID, err = s.resolvePublishInitiator(ctx, nodeID, protocolTypeStr)
	}
	if err != nil {
		return "", nil, err
	}
	if !exportACLEnabled(pvs) {
		return initiatorID, nil, nil
	}
	chap, err := s.volumeISCSIChap(ctx, pvs)
	if err != nil {
		return "", nil, err
	}
	return initiatorID, chap, nil
}

func validateNFSInitiatorFamily(initiator, bindAddress string) error {
	clientIP := net.ParseIP(initiator)
	serverIP := net.ParseIP(bindAddress)
	if clientIP == nil {
		return status.Errorf(codes.InvalidArgument, "NFS initiator %q is not a numeric IP address", initiator)
	}
	if serverIP == nil {
		return status.Errorf(codes.FailedPrecondition,
			"NFS storage address %q is not a numeric IP address", bindAddress)
	}
	clientV4 := clientIP.To4() != nil
	serverV4 := serverIP.To4() != nil
	if clientV4 != serverV4 {
		return status.Errorf(codes.InvalidArgument,
			"NFS node InternalIP %q has address family incompatible with storage address %q",
			initiator, bindAddress)
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
	setPublishTargetAttributes(ctx, volumeID, nodeID)

	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	targetName := parts[0]
	ctx = withAgentName(ctx, targetName)
	unlock := s.volumeLocks.lock(volumeID)
	defer unlock()

	selection, err := s.selectUnpublishPublications(
		ctx, volumeID, nodeID, targetName, parts[1], parts[3],
	)
	if err != nil {
		return nil, err
	}
	if selection.pvs == nil {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	ctx = withAgentName(ctx, selection.targetName)

	// ACL-off block exports have no per-host ACL to revoke. Files still require
	// fenced cleanup, including detaching their owned direct proxy.
	var agentClient agentv1.AgentServiceClient
	var agentCloser io.Closer
	if selection.acl || selection.pvs.Spec.FilesystemAdoption != nil {
		agentClient, agentCloser, err = s.dialUnpublishAgent(ctx, selection.targetName, volumeID)
		if err != nil {
			return nil, err
		}
		defer closeAgentConnection(ctx, agentCloser)
	}

	remaining, err := s.revokePublications(ctx, agentClient, selection.agentVolID,
		selection.agentProtocolType, selection.pvName, selection.pvs.UID, nodeID,
		selection.acl, selection.pvs)
	if err != nil {
		return nil, err
	}
	s.syncUnpublishedState(volumeID, remaining)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

type unpublishSelection struct {
	pvName            string
	pvs               *v1alpha1.PillarVolumeState
	targetName        string
	agentVolID        string
	agentProtocolType agentv1.ProtocolType
	acl               bool
}

func (s *ControllerServer) selectUnpublishPublications(
	ctx context.Context,
	volumeID, nodeID, targetName, protocolTypeStr, agentVolID string,
) (unpublishSelection, error) {
	pvName, err := s.volumeStateNameForID(ctx, volumeID)
	if err != nil {
		return unpublishSelection{}, err
	}
	if pvName == "" {
		return unpublishSelection{}, nil
	}
	existingPV, pvExists, err := s.readVolumeState(ctx, pvName)
	if err != nil {
		return unpublishSelection{}, status.Errorf(codes.Internal, "%v", err)
	}
	if !pvExists || (existingPV.Spec.VolumeID != "" && existingPV.Spec.VolumeID != volumeID) {
		return unpublishSelection{}, nil
	}
	err = s.validateVolumeDriver(existingPV)
	if err != nil {
		return unpublishSelection{}, err
	}
	agentProtocolType := mapProtocolType(protocolTypeStr)
	if existingPV.Spec.FilesystemAdoption != nil {
		targetName = existingPV.Spec.AgentRef
		agentProtocolType = mapProtocolType(existingPV.Spec.ProtocolType)
		agentVolID = existingPV.Spec.AgentVolumeID
		ctx = withAgentName(ctx, targetName)
	}
	setClaimAttributes(ctx, existingPV.Spec.ClaimRef)
	if !hasPublicationFor(existingPV.Status.PublishedNodes, nodeID) {
		return unpublishSelection{}, nil
	}
	return unpublishSelection{
		pvName:            pvName,
		pvs:               existingPV,
		targetName:        targetName,
		agentVolID:        agentVolID,
		agentProtocolType: agentProtocolType,
		acl:               exportACLEnabled(existingPV),
	}, nil
}

func (s *ControllerServer) dialUnpublishAgent(
	ctx context.Context,
	targetName, volumeID string,
) (agentv1.AgentServiceClient, io.Closer, error) {
	target := &v1alpha1.PillarAgent{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: targetName}, target)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return nil, nil, status.Errorf(codes.Internal,
				"failed to get PillarAgent %q: %v", targetName, err)
		}
		return nil, nil, status.Errorf(codes.FailedPrecondition,
			"PillarAgent %q not found; cannot revoke volume %q on its storage node",
			targetName, volumeID)
	}
	agentAddr := target.Status.ResolvedAddress
	if agentAddr == "" {
		return nil, nil, status.Errorf(codes.Unavailable,
			"PillarAgent %q has no resolved address", targetName)
	}
	dialed, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	return dialed, closer, nil
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
	recorded *v1alpha1.PillarVolumeState,
) (remaining int, err error) {
	fence, revoke, err := s.fencePublications(ctx, pvName, uid, nodeID)
	if err != nil {
		return 0, err
	}
	var adoptionProto *agentv1.FilesystemAdoption
	if adoption := recordedFilesystemAdoption(recorded); adoption != nil {
		var adoptionProtoErr error
		adoptionProto, adoptionProtoErr = filesystemAdoptionProto(adoption)
		if adoptionProtoErr != nil {
			return 0, invalidFilesystemDescriptor(adoptionProtoErr)
		}
	}
	revokedNodes := make([]string, 0, len(revoke))
	for _, pub := range revoke {
		if pub.Local || (!acl && recordedFilesystemAdoption(recorded) == nil) {
			revokedNodes = append(revokedNodes, pub.NodeID)
			continue
		}
		err = denyPublication(ctx, agentClient, agentVolID, protocolType, fence, adoptionProto, pub)
		if err != nil {
			return 0, err
		}
		revokedNodes = append(revokedNodes, pub.NodeID)
	}
	if adoption := recordedFilesystemAdoption(recorded); adoption != nil &&
		slices.ContainsFunc(revoke, func(pub v1alpha1.VolumePublication) bool { return pub.Local }) {
		err = s.detachFilesystemPublication(
			ctx, agentClient, agentVolID, protocolType, pvName, uid, fence, adoptionProto,
		)
		if err != nil {
			return 0, err
		}
	}
	// With nothing selected, releasePublication commits nothing and only
	// reports the current count.
	return s.releasePublication(ctx, pvName, uid, revokedNodes, fence)
}

func denyPublication(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	agentVolID string,
	protocolType agentv1.ProtocolType,
	fence *agentv1.FencingToken,
	adoption *agentv1.FilesystemAdoption,
	pub v1alpha1.VolumePublication,
) error {
	_, denyErr := agentClient.DenyInitiator(ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:           agentVolID,
		ProtocolType:       protocolType,
		InitiatorId:        pub.InitiatorID,
		Fence:              fence,
		FilesystemAdoption: adoption,
	})
	denyCode := status.Code(denyErr)
	if denyErr != nil && denyCode != codes.NotFound {
		return status.Errorf(denyCode,
			"agent DenyInitiator(%q, node=%q, initiator=%q) failed: %v",
			agentVolID, pub.NodeID, pub.InitiatorID, denyErr)
	}
	return nil
}

func (s *ControllerServer) detachFilesystemPublication(
	ctx context.Context,
	agentClient agentv1.AgentServiceClient,
	agentVolID string,
	protocolType agentv1.ProtocolType,
	pvName string,
	uid types.UID,
	fence *agentv1.FencingToken,
	adoption *agentv1.FilesystemAdoption,
) error {
	current, exists, readErr := s.readVolumeState(ctx, pvName)
	if readErr != nil {
		return status.Errorf(codes.Internal, "read file publications: %v", readErr)
	}
	if !exists || current.UID != uid {
		return status.Errorf(codes.Aborted, "file publications changed during unpublish; retry")
	}
	currentFence, tokenErr := fenceToken(current)
	if tokenErr != nil {
		return tokenErr
	}
	if currentFence.GetGeneration() != fence.GetGeneration() {
		return status.Errorf(codes.Aborted, "file publications changed during unpublish; retry")
	}
	if slices.ContainsFunc(current.Status.PublishedNodes, func(pub v1alpha1.VolumePublication) bool {
		return pub.Local && !pub.Revoking
	}) {
		return nil
	}
	_, detachErr := agentClient.SetLocalAttach(ctx, &agentv1.SetLocalAttachRequest{
		VolumeId: agentVolID, ProtocolType: protocolType, Local: false, Fence: fence,
		FilesystemAdoption: adoption,
		CapacityBytes:      current.Spec.CapacityBytes,
		BackendParams:      backendParamsFromResolved(current.Spec.Resolved.Backend),
	})
	if detachErr != nil {
		return status.Errorf(status.Code(detachErr), "detach local filesystem proxy: %v", detachErr)
	}
	return nil
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
// Adopted filesystem quotas are read-only and expansion is refused before any
// mutation. Provisioned block volumes require a node rescan/filesystem resize;
// legacy provisioned NFS datasets expose their expanded quota directly.
//
// Idempotency: ExpandVolume on the agent is idempotent — calling it with a
// requested_bytes ≤ current size is a no-op and returns the current size.
func (s *ControllerServer) ControllerExpandVolume(
	ctx context.Context,
	req *csi.ControllerExpandVolumeRequest,
) (*csi.ControllerExpandVolumeResponse, error) {
	expandReq, err := s.prepareExpandVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.expandBackend(ctx, expandReq, expandReq.pvName, expandReq.pvs)
}

type expandVolumeRequest struct {
	volumeID         string
	requiredBytes    int64
	targetName       string
	protocolType     string
	agentVolID       string
	agentBackendType agentv1.BackendType
	pvName           string
	pvs              *v1alpha1.PillarVolumeState
}

func (s *ControllerServer) prepareExpandVolume(
	ctx context.Context,
	req *csi.ControllerExpandVolumeRequest,
) (expandVolumeRequest, error) {
	volumeID := req.GetVolumeId()
	telemetry.SetVolumeAttributes(ctx, volumeID)
	if volumeID == "" {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return expandVolumeRequest{}, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	if req.GetCapacityRange() == nil {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return expandVolumeRequest{}, status.Error(codes.InvalidArgument, "capacity_range is required")
	}
	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	setSpanAttributes(ctx, telemetry.KeyCapacityRequestedBytes.Int64(requiredBytes))
	if requiredBytes < 0 {
		//nolint:wrapcheck // gRPC status errors must not be double-wrapped
		return expandVolumeRequest{},
			status.Error(codes.InvalidArgument, "capacity_range.required_bytes must not be negative")
	}
	parts := strings.SplitN(volumeID, "/", volumeIDParts)
	if len(parts) != volumeIDParts {
		return expandVolumeRequest{}, status.Errorf(codes.InvalidArgument,
			"malformed volume_id %q: expected format <target>/<protocol>/<backend>/<vol-id>",
			volumeID)
	}
	pvName, pvs, err := s.mustVolumeState(ctx, volumeID)
	if err != nil {
		return expandVolumeRequest{}, err
	}
	if pvs.Spec.FilesystemAdoption != nil {
		return expandVolumeRequest{}, status.Errorf(
			codes.FailedPrecondition,
			"adopted filesystem quota is read-only; expansion is not supported",
		)
	}
	return expandVolumeRequest{
		volumeID:         volumeID,
		requiredBytes:    requiredBytes,
		targetName:       parts[0],
		protocolType:     parts[1],
		agentVolID:       parts[3],
		agentBackendType: mapBackendType(parts[2]),
		pvName:           pvName,
		pvs:              pvs,
	}, nil
}

func (s *ControllerServer) expandBackend(
	ctx context.Context,
	expandReq expandVolumeRequest,
	pvName string,
	pvs *v1alpha1.PillarVolumeState,
) (*csi.ControllerExpandVolumeResponse, error) {
	// ── Refuse a preserving adoption or pending recovery before any agent
	// dial or token ──────────────────────────────────────────────────────────
	err := refuseEarlyExpand(pvs, expandReq.volumeID)
	if err != nil {
		return nil, err
	}
	target, err := s.getReadyAgent(ctx, expandReq.targetName)
	if err != nil {
		return nil, err
	}
	agentAddr := target.Status.ResolvedAddress
	ctx = withAgentName(ctx, expandReq.targetName)
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable,
			"failed to dial agent at %q: %v", agentAddr, err)
	}
	defer closeAgentConnection(ctx, closer)
	// The request carries the lifecycle's current token: an expand of a
	// deleted, deleting, or re-created volume is refused here or by the agent.
	err = refuseUnadoptedImport(pvs, expandReq.volumeID, "expand")
	if err != nil {
		return nil, err
	}
	fence, err := s.currentToken(ctx, pvName, expandReq.volumeID)
	if err != nil {
		return nil, err
	}
	expandResp, err := agentClient.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       expandReq.agentVolID,
		RequestedBytes: expandReq.requiredBytes,
		BackendType:    expandReq.agentBackendType,
		Fence:          fence,
	})
	if err != nil {
		grpcSt, _ := status.FromError(err)
		return nil, status.Errorf(grpcSt.Code(),
			"agent ExpandVolume(%q) failed: %v", expandReq.agentVolID, err)
	}
	actualBytes := expandResp.GetCapacityBytes()
	if actualBytes == 0 {
		// Preserve the requested capacity when an older agent omits its size.
		actualBytes = expandReq.requiredBytes
	}
	setSpanAttributes(ctx, telemetry.KeyCapacityAllocatedBytes.Int64(actualBytes))
	// NFS clients observe the expanded quota through statfs without a rescan.
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         actualBytes,
		NodeExpansionRequired: v1alpha1.ProtocolID(expandReq.protocolType) != v1alpha1.ProtocolIDNFS,
	}, nil
}

// refuseEarlyExpand refuses a preserving adoption or pending recovery before
// ControllerExpandVolume resolves or dials any agent or reads a token.  A
// PreserveOriginal LV is never resized, and a recovery record is non-serving
// until its transfer commits.
func refuseEarlyExpand(pvs *v1alpha1.PillarVolumeState, volumeID string) error {
	err := refusePreservedExpand(pvs, volumeID)
	if err != nil {
		return err
	}
	return refuseRecoveryExpand(pvs, volumeID)
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
	ctx = withAgentName(ctx, targetName)
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
