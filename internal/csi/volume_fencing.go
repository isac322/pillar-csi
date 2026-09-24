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

package csi

// Volume lifecycle fencing.
//
// Invariant: an agent mutates a volume's resources (backend create, expand,
// delete; export, unexport; initiator grant, revoke; state reconcile) only
// for a request carrying a FencingToken the controller committed on the
// volume's PillarVolumeState:
//
//   - volume_uid is the PillarVolumeState UID.  It identifies one lifecycle of
//     the volume ID; a deleted and re-created volume gets a new UID.
//   - generation is status.publicationGeneration after a compare-and-swap that
//     bumped it for this operation (expand and reconcile send the current
//     value without a bump).
//
// Every compare-and-swap below is pinned to the UID the operation started
// with, so a stale controller can never commit (and thus never obtain a token
// for) a later lifecycle of the same name.  The agent rejects a token that is
// older than, or from a different lifecycle than, the last one it applied,
// and checks it in the same critical section as the mutation.  The
// PillarVolumeState is created before any backend resource, so a volume
// without a PillarVolumeState owns nothing on any agent.

import (
	"context"
	"errors"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// errNoStatusChange tells updateVolumeState that the mutation decided nothing
// needs to be written.
var errNoStatusChange = errors.New("no status change")

// updateVolumeState runs mutate on a fresh, uncached read of the
// PillarVolumeState pvName and writes the status back as a resourceVersion
// compare-and-swap, retrying on conflict.  When bump is true a successful
// write also increments status.publicationGeneration.
//
// The uid argument pins the lifecycle: when non-empty, a PillarVolumeState
// with another UID (or none at all) returns Aborted, because the volume this
// operation started on no longer exists.  With an empty uid a missing object
// returns (nil, nil).  The mutate callback may return errNoStatusChange to
// skip the write; the current object is then returned.
func (s *ControllerServer) updateVolumeState(
	ctx context.Context,
	pvName string,
	uid types.UID,
	bump bool,
	mutate func(pvs *v1alpha1.PillarVolumeState) error,
) (*v1alpha1.PillarVolumeState, error) {
	var result *v1alpha1.PillarVolumeState
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result = nil
		pvs, exists, getErr := s.readVolumeState(ctx, pvName)
		if getErr != nil {
			return status.Errorf(codes.Internal, "%v", getErr)
		}
		if !exists || (uid != "" && pvs.UID != uid) {
			if uid == "" {
				return nil
			}
			return status.Errorf(codes.Aborted,
				"PillarVolumeState %q (uid %s) no longer exists; the volume was deleted or re-created",
				pvName, uid)
		}
		mutateErr := mutate(pvs)
		if errors.Is(mutateErr, errNoStatusChange) {
			result = pvs
			return nil
		}
		if mutateErr != nil {
			return mutateErr
		}
		if bump {
			pvs.Status.PublicationGeneration++
		}
		updateErr := s.k8sClient.Status().Update(ctx, pvs)
		if updateErr != nil {
			return updateErr //nolint:wrapcheck // conflict detection by RetryOnConflict needs the raw error
		}
		result = pvs
		return nil
	})
	return result, publicationRecordError("update", pvName, "", err)
}

// fenceToken returns the agent fencing token for the committed state of pvs.
// An object without a UID cannot identify a lifecycle; sending an empty
// token would make the request unfenced, so it is refused.
func fenceToken(pvs *v1alpha1.PillarVolumeState) (*agentv1.FencingToken, error) {
	gen := pvs.Status.PublicationGeneration
	if pvs.UID == "" || gen < 0 {
		return nil, status.Errorf(codes.Internal,
			"PillarVolumeState %q has no usable fencing identity (uid %q, publicationGeneration %d)",
			pvs.Name, pvs.UID, gen)
	}
	return &agentv1.FencingToken{
		VolumeUid:  string(pvs.UID),
		Generation: uint64(gen),
	}, nil
}

// committedToken commits a generation bump for one lifecycle-pinned
// operation and returns its fencing token.
func (s *ControllerServer) committedToken(
	ctx context.Context,
	pvName string,
	uid types.UID,
	mutate func(pvs *v1alpha1.PillarVolumeState) error,
) (*agentv1.FencingToken, error) {
	pvs, err := s.updateVolumeState(ctx, pvName, uid, true, mutate)
	if err != nil {
		return nil, err
	}
	return fenceToken(pvs)
}

// ensureVolumeState returns the PillarVolumeState for pvName, creating it
// (phase Provisioning) when it does not exist yet.  CreateVolume calls it
// before any agent call, so that every backend resource belongs to a durable
// lifecycle that DeleteVolume can find and fence.
func (s *ControllerServer) ensureVolumeState(
	ctx context.Context,
	pvName string,
	spec v1alpha1.PillarVolumeStateSpec,
) (*v1alpha1.PillarVolumeState, error) {
	pvs, exists, err := s.readVolumeState(ctx, pvName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if exists {
		return pvs, nil
	}
	created := &v1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec:       spec,
	}
	err = s.k8sClient.Create(ctx, created)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return nil, status.Errorf(codes.Internal, "create PillarVolumeState %q: %v", pvName, err)
	}
	pvs, exists, err = s.readVolumeState(ctx, pvName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !exists {
		return nil, status.Errorf(codes.Aborted,
			"PillarVolumeState %q was deleted while it was being created", pvName)
	}
	if pvs.Status.Phase != "" {
		return pvs, nil
	}
	return s.updateVolumeState(ctx, pvName, pvs.UID, false, func(pvs *v1alpha1.PillarVolumeState) error {
		if pvs.Status.Phase != "" {
			return errNoStatusChange
		}
		pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseProvisioning
		return nil
	})
}

// refuseDeleting rejects a grant-class operation on a volume under deletion.
func refuseDeleting(pvs *v1alpha1.PillarVolumeState, volumeID string) error {
	if pvs.Status.Deleting {
		return status.Errorf(codes.FailedPrecondition, "volume %q is being deleted", volumeID)
	}
	return nil
}

// claimOperation commits a generation for a CreateVolume step (backend
// creation or export) of the lifecycle uid and returns its token.
func (s *ControllerServer) claimOperation(
	ctx context.Context,
	pvName, volumeID string,
	uid types.UID,
) (*agentv1.FencingToken, error) {
	return s.committedToken(ctx, pvName, uid, func(pvs *v1alpha1.PillarVolumeState) error {
		return refuseDeleting(pvs, volumeID)
	})
}

// currentToken returns the token of the lifecycle's current generation
// without committing a new one (expand).  A missing volume is NotFound; a
// volume under deletion is refused.
func (s *ControllerServer) currentToken(
	ctx context.Context,
	pvName, volumeID string,
) (*agentv1.FencingToken, error) {
	pvs, exists, err := s.readVolumeState(ctx, pvName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", volumeID)
	}
	err = refuseDeleting(pvs, volumeID)
	if err != nil {
		return nil, err
	}
	return fenceToken(pvs)
}

// reservePublication durably records pub before the agent grants the node
// access, so that a concurrent or later publish to an incompatible node is
// rejected even across controller restarts, and returns the fencing token
// for the AllowInitiator call.  An identical re-publish still commits a new
// generation so the re-issued grant is ordered after any operation already in
// flight.
//
// Returns NotFound for an unknown volume, FailedPrecondition when another
// node holds the volume incompatibly or the volume is being deleted,
// AlreadyExists when the node holds it with a different capability, and
// Aborted while an unpublish of the same node is still revoking it.
func (s *ControllerServer) reservePublication(
	ctx context.Context,
	pvName, volumeID string,
	uid types.UID,
	pub v1alpha1.VolumePublication,
) (*agentv1.FencingToken, error) {
	return s.committedToken(ctx, pvName, uid, func(pvs *v1alpha1.PillarVolumeState) error {
		err := refuseDeleting(pvs, volumeID)
		if err != nil {
			return err
		}
		for _, cur := range pvs.Status.PublishedNodes {
			err = checkPublicationConflict(volumeID, cur, pub)
			if err != nil {
				return err
			}
			if cur == pub {
				return nil // identical re-publish: bump only
			}
		}
		pvs.Status.PublishedNodes = append(pvs.Status.PublishedNodes, pub)
		return nil
	})
}

// checkPublicationConflict reports whether the recorded publication cur
// forbids publishing pub.  It returns nil for a compatible publication on
// another node and for an identical publication on the same node.
func checkPublicationConflict(volumeID string, cur, pub v1alpha1.VolumePublication) error {
	if cur.NodeID != pub.NodeID {
		if canSharePublication(cur, pub) {
			return nil
		}
		return status.Errorf(codes.FailedPrecondition,
			"volume %q is published to another node %q (access mode %s, readonly=%t); "+
				"cannot publish to node %q with access mode %s (readonly=%t)",
			volumeID, cur.NodeID, cur.AccessMode, cur.Readonly,
			pub.NodeID, pub.AccessMode, pub.Readonly)
	}
	switch {
	case cur.Revoking:
		// An unpublish of this node fenced its revoke and has not dropped the
		// record yet; granting now would race that revoke.
		return status.Errorf(codes.Aborted,
			"volume %q is being unpublished from node %q; retry", volumeID, pub.NodeID)
	case cur == pub:
		return nil
	case cur.InitiatorID != pub.InitiatorID:
		return status.Errorf(codes.FailedPrecondition,
			"volume %q is published to node %q as initiator %q but the node now reports "+
				"initiator %q; unpublish the volume from the node first",
			volumeID, pub.NodeID, cur.InitiatorID, pub.InitiatorID)
	default:
		return status.Errorf(codes.AlreadyExists,
			"volume %q is already published to node %q with access mode %s (readonly=%t); "+
				"requested access mode %s (readonly=%t)",
			volumeID, pub.NodeID, cur.AccessMode, cur.Readonly, pub.AccessMode, pub.Readonly)
	}
}

// fencePublications selects the publications of nodeID (every publication
// when nodeID is empty), marks them revoking, and commits a generation bump
// in the same compare-and-swap; the records themselves stay (fail-closed).
// The token orders the DenyInitiator calls after every grant that could still
// be in flight, and because revoking records are excluded from the initiator
// set state recovery grants, a recovery pass at this generation agrees with
// the revoke.  An empty selection commits nothing and returns no records.
func (s *ControllerServer) fencePublications(
	ctx context.Context,
	pvName string,
	uid types.UID,
	nodeID string,
) (token *agentv1.FencingToken, revoke []v1alpha1.VolumePublication, err error) {
	pvs, err := s.updateVolumeState(ctx, pvName, uid, true, func(pvs *v1alpha1.PillarVolumeState) error {
		revoke = revoke[:0]
		for i := range pvs.Status.PublishedNodes {
			pub := &pvs.Status.PublishedNodes[i]
			if nodeID == "" || pub.NodeID == nodeID {
				pub.Revoking = true
				revoke = append(revoke, *pub)
			}
		}
		if len(revoke) == 0 {
			return errNoStatusChange
		}
		return nil
	})
	if err != nil || len(revoke) == 0 {
		return nil, nil, err
	}
	token, err = fenceToken(pvs)
	return token, revoke, err
}

// releasePublication removes the records of nodeIDs after the agent revoked
// their access and returns how many publications remain.  With a non-nil
// fence the release commits only while publicationGeneration still equals the
// fence's generation: any change in between means the records no longer
// describe what was revoked, so Aborted is returned and the CO retries the
// unpublish with a fresh fence.  A nil fence releases unconditionally; it is
// only reached with nothing selected, where it merely reports the count.
func (s *ControllerServer) releasePublication(
	ctx context.Context,
	pvName string,
	uid types.UID,
	nodeIDs []string,
	fence *agentv1.FencingToken,
) (remaining int, err error) {
	pvs, err := s.updateVolumeState(ctx, pvName, uid, true, func(pvs *v1alpha1.PillarVolumeState) error {
		if fence != nil {
			current, tokenErr := fenceToken(pvs)
			if tokenErr != nil {
				return tokenErr
			}
			if current.GetGeneration() != fence.GetGeneration() {
				return status.Errorf(codes.Aborted,
					"publication state of PillarVolumeState %q changed during unpublish "+
						"(generation %d, revoked at %d); retry",
					pvName, current.GetGeneration(), fence.GetGeneration())
			}
		}
		kept := slices.DeleteFunc(slices.Clone(pvs.Status.PublishedNodes),
			func(p v1alpha1.VolumePublication) bool { return slices.Contains(nodeIDs, p.NodeID) })
		if len(kept) == len(pvs.Status.PublishedNodes) {
			return errNoStatusChange
		}
		pvs.Status.PublishedNodes = kept
		return nil
	})
	if err != nil {
		return 0, publicationRecordError("remove publication", pvName, strings.Join(nodeIDs, ","), err)
	}
	if pvs == nil {
		return 0, nil
	}
	return len(pvs.Status.PublishedNodes), nil
}

// markVolumeDeleting sets status.deleting and commits a generation for the
// deletion, succeeding only while no publication is recorded, so delete and
// publish are mutually exclusive in the durable record.  It returns the
// lifecycle and its token, or a nil object when the volume has no
// PillarVolumeState: CreateVolume creates it before any backend resource, so
// such a volume owns nothing and there is nothing to delete.  A retry after
// deleting was set returns the already committed generation.
func (s *ControllerServer) markVolumeDeleting(
	ctx context.Context,
	pvName, volumeID string,
) (*v1alpha1.PillarVolumeState, *agentv1.FencingToken, error) {
	// Pin the lifecycle observed first: a conflict retry must never rebind to
	// a PillarVolumeState re-created under the same name in between.
	current, exists, err := s.readVolumeState(ctx, pvName)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !exists {
		return nil, nil, nil
	}
	pvs, err := s.updateVolumeState(ctx, pvName, current.UID, true, func(pvs *v1alpha1.PillarVolumeState) error {
		if pvs.Status.Deleting {
			return errNoStatusChange
		}
		if len(pvs.Status.PublishedNodes) > 0 {
			nodes := make([]string, 0, len(pvs.Status.PublishedNodes))
			for _, pub := range pvs.Status.PublishedNodes {
				nodes = append(nodes, pub.NodeID)
			}
			return status.Errorf(codes.FailedPrecondition,
				"volume %q is still published to nodes %v; unpublish it before deleting",
				volumeID, nodes)
		}
		pvs.Status.Deleting = true
		return nil
	})
	if err != nil || pvs == nil {
		return nil, nil, err
	}
	token, err := fenceToken(pvs)
	if err != nil {
		return nil, nil, err
	}
	return pvs, token, nil
}

// deleteVolumeState removes the PillarVolumeState of the lifecycle uid.  The
// UID precondition guarantees a stale controller never deletes the record of
// a later lifecycle with the same name.  A missing object is success.
func (s *ControllerServer) deleteVolumeState(ctx context.Context, pvName string, uid types.UID) error {
	pvs := &v1alpha1.PillarVolumeState{ObjectMeta: metav1.ObjectMeta{Name: pvName}}
	err := s.k8sClient.Delete(ctx, pvs, ctrlclient.Preconditions{UID: &uid})
	if err == nil || k8serrors.IsNotFound(err) {
		return nil
	}
	if k8serrors.IsConflict(err) {
		// Another lifecycle owns the name now; ours is already gone.
		return nil
	}
	return status.Errorf(codes.Internal, "delete PillarVolumeState %q: %v", pvName, err)
}

// publicationRecordError passes gRPC status errors through and converts any
// other (Kubernetes API) error into Internal naming the operation and target.
func publicationRecordError(op, pvName, nodeID string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Errorf(codes.Internal, "%s for node %q in PillarVolumeState %q: %v", op, nodeID, pvName, err)
}

// recordAllocatedCapacity stores the capacity the backend actually allocated
// (which may exceed the request, e.g. rounded zvol sizes) in the spec of the
// lifecycle uid, so cached CreateVolume responses and CreatePartial retries
// report it.  The update is a resourceVersion compare-and-swap pinned to uid.
func (s *ControllerServer) recordAllocatedCapacity(
	ctx context.Context,
	pvName string,
	uid types.UID,
	capacity int64,
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvs, exists, getErr := s.readVolumeState(ctx, pvName)
		if getErr != nil {
			return status.Errorf(codes.Internal, "%v", getErr)
		}
		if !exists || pvs.UID != uid {
			return status.Errorf(codes.Aborted,
				"PillarVolumeState %q (uid %s) no longer exists; the volume was deleted or re-created",
				pvName, uid)
		}
		if pvs.Spec.CapacityBytes == capacity {
			return nil
		}
		pvs.Spec.CapacityBytes = capacity
		return s.k8sClient.Update(ctx, pvs)
	})
	return publicationRecordError("record allocated capacity", pvName, "", err)
}

// persistCreatePartial records, on the lifecycle uid, that the backend
// resource exists at devicePath but ExportVolume has not succeeded yet, so a
// retry of CreateVolume skips the backend step and only re-exports.  It also
// records the requested export configuration as the durable desired state the
// resync controller restores after the storage node loses its target state.
func (s *ControllerServer) persistCreatePartial(
	ctx context.Context,
	pvName string,
	uid types.UID,
	devicePath string,
	exportSpec *v1alpha1.VolumeExportSpec,
) error {
	_, err := s.updateVolumeState(ctx, pvName, uid, false, func(pvs *v1alpha1.PillarVolumeState) error {
		pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseCreatePartial
		pvs.Status.BackendDevicePath = devicePath
		pvs.Status.ExportSpec = exportSpec
		pvs.Status.PartialFailure = &v1alpha1.PartialFailureInfo{
			FailedOperation: "ExportVolume",
			FailedAt:        metav1.Now(),
			Reason:          "ExportPending",
			Message: "Backend storage resource created successfully; " +
				"ExportVolume has not yet succeeded.  " +
				"Retry CreateVolume to re-attempt the export step.",
			BackendCreated: true,
		}
		return nil
	})
	return err
}

// exportInfoGetter is the minimal interface of agentv1.ExportInfo used by
// persistVolumeReady.
type exportInfoGetter interface {
	GetTargetId() string
	GetAddress() string
	GetPort() int32
	GetVolumeRef() string
}

// persistVolumeReady marks the lifecycle uid Ready and caches the export
// parameters for idempotent CreateVolume retries.
func (s *ControllerServer) persistVolumeReady(
	ctx context.Context,
	pvName string,
	uid types.UID,
	info exportInfoGetter,
) error {
	_, err := s.updateVolumeState(ctx, pvName, uid, false, func(pvs *v1alpha1.PillarVolumeState) error {
		pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseReady
		pvs.Status.PartialFailure = nil
		pvs.Status.BackendDevicePath = ""
		pvs.Status.ExportInfo = &v1alpha1.VolumeExportInfo{
			TargetID:  info.GetTargetId(),
			Address:   info.GetAddress(),
			Port:      info.GetPort(),
			VolumeRef: info.GetVolumeRef(),
		}
		return nil
	})
	return err
}
