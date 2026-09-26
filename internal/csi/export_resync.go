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

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	"github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// ConditionExportReconciled reports whether the storage node's export and ACL
// of a volume match its durable desired state.
const ConditionExportReconciled = "ExportReconciled"

// Reasons recorded on the ExportReconciled condition.
const (
	reasonExportReconciled  = "Reconciled"
	reasonExportSpecMissing = "ExportSpecMissing"
	reasonAgentUnavailable  = "AgentUnavailable"
	reasonReconcileFailed   = "ReconcileFailed"
	reasonStaleGeneration   = "StaleGeneration"
)

// errExportReconcile marks a resync attempt that reached a definite failure
// (recorded on the condition) so the caller requeues with backoff.
var errExportReconcile = errors.New("export reconcile failed")

// exportReconcileTimeout bounds one agent ReconcileState call.  The resync
// holds the volume lock shared with publish/unpublish/delete and runs on the
// reconciler's worker, so an agent that accepts the call but never answers
// must not stall either.  It exceeds the agent's device-poll timeout
// (nvmeof.DefaultDevicePollTimeout, 5s) plus its fsync'd fencing-mark writes.
const exportReconcileTimeout = 30 * time.Second

// exportRestorePerVolumeTimeout is the share of a batch restore's RPC budget
// granted to each listed volume.  The agent prepares the volumes one after
// another, each bounded by its device poll (nvmeof.DefaultDevicePollTimeout,
// 5s) plus fsync'd fencing-mark writes, so a fixed bound would time out a
// storage node with many volumes and leave its export restore pending.
const exportRestorePerVolumeTimeout = 10 * time.Second

// exportSpecFor returns the durable export configuration for the agent export
// parameters, or nil for protocols without a bind address and port.
func exportSpecFor(params *agentv1.ExportParams, aclEnabled bool) *v1alpha1.VolumeExportSpec {
	switch {
	case params.GetNvmeofTcp() != nil:
		p := params.GetNvmeofTcp()
		return &v1alpha1.VolumeExportSpec{BindAddress: p.GetBindAddress(), Port: p.GetPort(), ACLEnabled: aclEnabled}
	case params.GetIscsi() != nil:
		p := params.GetIscsi()
		return &v1alpha1.VolumeExportSpec{BindAddress: p.GetBindAddress(), Port: p.GetPort(), ACLEnabled: aclEnabled}
	default:
		return nil
	}
}

// exportParamsFor is the inverse of exportSpecFor for the volume's protocol.
func exportParamsFor(protocol agentv1.ProtocolType, spec *v1alpha1.VolumeExportSpec) *agentv1.ExportParams {
	switch protocol {
	case agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP:
		return &agentv1.ExportParams{Params: &agentv1.ExportParams_NvmeofTcp{
			NvmeofTcp: &agentv1.NvmeofTcpExportParams{BindAddress: spec.BindAddress, Port: spec.Port},
		}}
	case agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI:
		return &agentv1.ExportParams{Params: &agentv1.ExportParams_Iscsi{
			Iscsi: &agentv1.IscsiExportParams{BindAddress: spec.BindAddress, Port: spec.Port},
		}}
	default:
		return nil
	}
}

// desiredVolumeState builds the agent reconcile input for a volume from its
// durable state.  The ACL is exactly the initiators of the published nodes,
// excluding records marked revoking (an unpublish already fenced their
// revoke); the device path is left empty so the agent derives it from its
// backend.  Fence carries the lifecycle UID and the publication generation
// committed by the same status read, so the agent rejects this resync when a
// newer publication transition has already reached it.
func desiredVolumeState(pvs *v1alpha1.PillarVolumeState) (*agentv1.VolumeDesiredState, error) {
	protocol := mapProtocolType(pvs.Spec.ProtocolType)
	initiators := make([]string, 0, len(pvs.Status.PublishedNodes))
	for _, publication := range pvs.Status.PublishedNodes {
		if publication.Revoking {
			continue
		}
		initiators = append(initiators, publication.InitiatorID)
	}
	fence, err := fenceToken(pvs)
	if err != nil {
		return nil, err
	}
	return &agentv1.VolumeDesiredState{
		VolumeId:    pvs.Spec.AgentVolumeID,
		BackendType: mapBackendType(pvs.Spec.BackendType),
		Fence:       fence,
		Exports: []*agentv1.ExportDesiredState{{
			ProtocolType:      protocol,
			ExportParams:      exportParamsFor(protocol, pvs.Status.ExportSpec),
			AllowedInitiators: initiators,
			AclEnabled:        pvs.Status.ExportSpec.ACLEnabled,
		}},
	}, nil
}

// ReconcileVolumeExport converges the storage node's export and ACL of the
// volume tracked by the named PillarVolumeState to its durable desired state
// (status.exportSpec + status.publishedNodes).  It is level-triggered: safe to
// call at any time and repeatedly, it re-creates target state lost to an agent
// restart, node reboot, or nvmet reload and never touches other volumes.
//
// It runs under the same per-volume lock as ControllerPublishVolume,
// ControllerUnpublishVolume and DeleteVolume, re-reading the volume state
// uncached under the lock.  That lock serializes only this process: it does
// not order this call against another controller process (e.g. a stale
// leader), whose delayed agent RPCs can still interleave.
//
// The outcome is recorded on the ExportReconciled condition.  A nil error
// means the volume is converged, gone, or cannot be reconciled until its state
// changes (missing exportSpec); a non-nil error asks the caller to requeue.
func (s *ControllerServer) ReconcileVolumeExport(ctx context.Context, pvsName string) error {
	pvs, found, err := s.readVolumeState(ctx, pvsName)
	if err != nil || !found {
		return err
	}

	unlock := s.volumeLocks.lock(pvs.Spec.VolumeID)
	defer unlock()

	pvs, found, err = s.readVolumeState(ctx, pvsName)
	if err != nil || !found || !pvs.DeletionTimestamp.IsZero() {
		return err
	}

	// The volume is being deleted: UnexportVolume already carried the fencing
	// generation, and re-creating the export here would resurrect it.
	if pvs.Status.Deleting {
		return nil
	}

	if pvs.Status.ExportSpec == nil {
		return s.setExportReconciled(ctx, pvsName, metav1.ConditionFalse, reasonExportSpecMissing,
			"status.exportSpec is not recorded; the export cannot be re-created from durable state")
	}

	reason, reconcileErr := s.reconcileVolumeOnAgent(ctx, pvs)
	if reconcileErr != nil {
		condErr := s.setExportReconciled(ctx, pvsName, metav1.ConditionFalse, reason, reconcileErr.Error())
		return fmt.Errorf("%w: volume %q: %w",
			errExportReconcile, pvs.Spec.VolumeID, errors.Join(reconcileErr, condErr))
	}
	return s.setExportReconciled(ctx, pvsName, metav1.ConditionTrue, reasonExportReconciled,
		"export and ACL match the desired state")
}

// reconcileVolumeOnAgent pushes the desired state of one volume to its agent
// and returns the condition reason to record on failure.
func (s *ControllerServer) reconcileVolumeOnAgent(
	ctx context.Context,
	pvs *v1alpha1.PillarVolumeState,
) (string, error) {
	agentAddr, err := s.agentAddress(ctx, pvs.Spec.AgentRef)
	if err != nil {
		return reasonAgentUnavailable, err
	}
	desired, err := desiredVolumeState(pvs)
	if err != nil {
		return reasonReconcileFailed, fmt.Errorf("build desired state for %q: %w", pvs.Spec.AgentVolumeID, err)
	}

	// The agent call is bounded; the caller's ctx stays unbounded so the
	// condition update and requeue still happen after a timeout.
	rpcCtx, cancel := context.WithTimeout(ctx, exportReconcileTimeout)
	defer cancel()

	resp, err := s.reconcileStatesOnAgent(rpcCtx, agentAddr, &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{desired},
	})
	if err != nil {
		return reasonAgentUnavailable, fmt.Errorf("volume %q: %w", pvs.Spec.AgentVolumeID, err)
	}
	results := resp.GetResults()
	if len(results) != 1 || results[0].GetVolumeId() != pvs.Spec.AgentVolumeID {
		return reasonReconcileFailed, fmt.Errorf("agent ReconcileState(%q): unexpected results %v",
			pvs.Spec.AgentVolumeID, results)
	}
	return reconcileItemReason(pvs.Spec.AgentVolumeID, results[0])
}

// agentAddress returns the resolved gRPC address of the named PillarAgent.
func (s *ControllerServer) agentAddress(ctx context.Context, agentName string) (string, error) {
	target := &v1alpha1.PillarAgent{}
	err := s.k8sClient.Get(ctx, types.NamespacedName{Name: agentName}, target)
	if err != nil {
		return "", fmt.Errorf("get PillarAgent %q: %w", agentName, err)
	}
	if target.Status.ResolvedAddress == "" {
		return "", fmt.Errorf("PillarAgent %q has no resolved address", agentName)
	}
	return target.Status.ResolvedAddress, nil
}

// reconcileStatesOnAgent dials the agent at agentAddr and sends req.
func (s *ControllerServer) reconcileStatesOnAgent(
	ctx context.Context,
	agentAddr string,
	req *agentv1.ReconcileStateRequest,
) (*agentv1.ReconcileStateResponse, error) {
	agentClient, closer, err := s.dialAgent(ctx, agentAddr)
	if err != nil {
		return nil, fmt.Errorf("dial agent at %q: %w", agentAddr, err)
	}
	defer closer.Close() //nolint:errcheck // best-effort close; RPC errors are reported

	resp, err := agentClient.ReconcileState(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("agent ReconcileState at %q: %w", agentAddr, err)
	}
	return resp, nil
}

// reconcileItemReason maps the agent's result for one volume to the condition
// reason to record, or "" when the volume was reconciled.
func reconcileItemReason(agentVolumeID string, result *agentv1.ReconcileItemResult) (string, error) {
	if result.GetSuccess() {
		return "", nil
	}
	msg := result.GetErrorMessage()
	if strings.HasPrefix(msg, "stale fencing token") {
		return reasonStaleGeneration, fmt.Errorf("agent ReconcileState(%q): %s", agentVolumeID, msg)
	}
	return reasonReconcileFailed, fmt.Errorf("agent ReconcileState(%q): %s", agentVolumeID, msg)
}

// restoreEntry is one volume of a batch export restore.
type restoreEntry struct {
	pvsName string
	desired *agentv1.VolumeDesiredState
}

// RestoreAgentExports restores every export of the named agent in one
// complete ReconcileState.  The agent starts with its export restore pending
// and links no subsystem to a port until it receives this request: nvmet
// starts listening on a shared port with the first linked subsystem, and a
// host reconnecting for a subsystem not linked yet is rejected with a
// do-not-retry status and deletes its controller (issue #92).  The request is
// sent even when the agent exports nothing, because only a complete request
// clears the pending restore.
//
// It holds the per-volume lock of every volume of the agent, acquired in
// sorted volume-ID order (all other holders take a single lock, so this cannot
// deadlock), until the outcomes are recorded.  Otherwise a concurrent unpublish
// could advance a volume's fencing generation after it was read, the agent
// would reject that item as stale, and its subsystem would stay unlinked while
// the agent leaves the restore state.  Volumes are skipped under the same rules
// as ReconcileVolumeExport.  The outcome of each volume is recorded on its
// ExportReconciled condition; a non-nil error asks the caller to retry.
func (s *ControllerServer) RestoreAgentExports(ctx context.Context, agentName string) error {
	names, volumeIDs, err := s.agentVolumeStates(ctx, agentName)
	if err != nil {
		return err
	}
	unlocks := make([]func(), 0, len(volumeIDs))
	for _, volumeID := range volumeIDs {
		unlocks = append(unlocks, s.volumeLocks.lock(volumeID))
	}
	defer func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}()

	entries := make([]restoreEntry, 0, len(names))
	var errs []error
	for _, name := range names {
		pvs, found, readErr := s.readVolumeState(ctx, name)
		if readErr != nil {
			return readErr
		}
		if !found || !pvs.DeletionTimestamp.IsZero() || pvs.Status.Deleting || pvs.Spec.AgentRef != agentName {
			continue
		}
		if pvs.Status.ExportSpec == nil {
			errs = append(errs, s.setExportReconciled(ctx, name, metav1.ConditionFalse, reasonExportSpecMissing,
				"status.exportSpec is not recorded; the export cannot be re-created from durable state"))
			continue
		}
		desired, buildErr := desiredVolumeState(pvs)
		if buildErr != nil {
			buildErr = fmt.Errorf("%w: build desired state for %q: %w", errExportReconcile, pvs.Spec.AgentVolumeID, buildErr)
			errs = append(errs, buildErr,
				s.setExportReconciled(ctx, name, metav1.ConditionFalse, reasonReconcileFailed, buildErr.Error()))
			continue
		}
		entries = append(entries, restoreEntry{pvsName: name, desired: desired})
	}

	errs = append(errs, s.sendAgentRestore(ctx, agentName, entries))
	return errors.Join(errs...) //nolint:wrapcheck // multi-error is already wrapped per item
}

// agentVolumeStates lists, uncached, the PillarVolumeStates of the agent and
// returns their names and their sorted, de-duplicated volume IDs.
func (s *ControllerServer) agentVolumeStates(
	ctx context.Context,
	agentName string,
) (names, volumeIDs []string, err error) {
	list := &v1alpha1.PillarVolumeStateList{}
	err = s.apiReader.List(ctx, list)
	if err != nil {
		return nil, nil, fmt.Errorf("list PillarVolumeStates of agent %q: %w", agentName, err)
	}
	for i := range list.Items {
		if list.Items[i].Spec.AgentRef != agentName {
			continue
		}
		names = append(names, list.Items[i].Name)
		volumeIDs = append(volumeIDs, list.Items[i].Spec.VolumeID)
	}
	slices.Sort(volumeIDs)
	return names, slices.Compact(volumeIDs), nil
}

// sendAgentRestore sends the complete ReconcileState for entries to the agent
// and records each volume's outcome.
func (s *ControllerServer) sendAgentRestore(ctx context.Context, agentName string, entries []restoreEntry) error {
	req := &agentv1.ReconcileStateRequest{Complete: true}
	for _, entry := range entries {
		req.Volumes = append(req.Volumes, entry.desired)
	}

	agentAddr, err := s.agentAddress(ctx, agentName)
	var resp *agentv1.ReconcileStateResponse
	if err == nil {
		// Bounded like reconcileVolumeOnAgent, scaled by the volume count.
		timeout := exportReconcileTimeout + time.Duration(len(entries))*exportRestorePerVolumeTimeout
		rpcCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err = s.reconcileStatesOnAgent(rpcCtx, agentAddr, req)
		cancel()
	}
	if err != nil {
		errs := make([]error, 0, 1+len(entries))
		errs = append(errs, fmt.Errorf("%w: restore exports of agent %q: %w", errExportReconcile, agentName, err))
		for _, entry := range entries {
			errs = append(errs, s.setExportReconciled(ctx, entry.pvsName, metav1.ConditionFalse,
				reasonAgentUnavailable, err.Error()))
		}
		return errors.Join(errs...) //nolint:wrapcheck // multi-error is already wrapped per item
	}

	results := make(map[string]*agentv1.ReconcileItemResult, len(resp.GetResults()))
	for _, result := range resp.GetResults() {
		results[result.GetVolumeId()] = result
	}
	var errs []error
	for _, entry := range entries {
		volumeID := entry.desired.GetVolumeId()
		result, ok := results[volumeID]
		reason, itemErr := reasonReconcileFailed, fmt.Errorf("agent ReconcileState(%q): no result", volumeID)
		if ok {
			reason, itemErr = reconcileItemReason(volumeID, result)
		}
		if itemErr == nil {
			errs = append(errs, s.setExportReconciled(ctx, entry.pvsName, metav1.ConditionTrue,
				reasonExportReconciled, "export and ACL match the desired state"))
			continue
		}
		errs = append(errs, fmt.Errorf("%w: %w", errExportReconcile, itemErr),
			s.setExportReconciled(ctx, entry.pvsName, metav1.ConditionFalse, reason, itemErr.Error()))
	}
	return errors.Join(errs...) //nolint:wrapcheck // multi-error is already wrapped per item
}

// setExportReconciled records the ExportReconciled condition, writing only
// when it changes so periodic resyncs do not churn the object.
func (s *ControllerServer) setExportReconciled(
	ctx context.Context,
	pvsName string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pvs, found, readErr := s.readVolumeState(ctx, pvsName)
		if readErr != nil || !found {
			return readErr
		}
		changed := meta.SetStatusCondition(&pvs.Status.Conditions, metav1.Condition{
			Type:               ConditionExportReconciled,
			Status:             condStatus,
			ObservedGeneration: pvs.Generation,
			Reason:             reason,
			Message:            message,
		})
		if !changed {
			return nil
		}
		return s.k8sClient.Status().Update(ctx, pvs)
	})
	if err != nil {
		return fmt.Errorf("update PillarVolumeState %q condition %s: %w", pvsName, ConditionExportReconciled, err)
	}
	return nil
}
