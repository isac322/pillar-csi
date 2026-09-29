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
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	"github.com/isac322/pillar-csi/internal/agentclient"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// Values of the result label of pillar_csi_volumes_reaped_total and of
// pillar_csi.reap.result (SP4).
const (
	reapResultReaped = "reaped"
	reapResultKept   = "kept"
	reapResultError  = "error"
)

// volumesReaped is M15, pillar_csi_volumes_reaped_total.
var volumesReaped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pillar_csi_volumes_reaped_total",
	Help: "Abandoned provisioning attempts the controller tried to reap, by agent and result.",
}, []string{"agent", "result"})

// RegisterControllerMetrics registers the CSI controller's metrics
// (pillar_csi_volumes_reaped_total) on reg.
func RegisterControllerMetrics(reg prometheus.Registerer) error {
	err := reg.Register(volumesReaped)
	if err != nil {
		return fmt.Errorf("register pillar_csi_volumes_reaped_total: %w", err)
	}
	return nil
}

// Values of pillar_csi.create.resumed_from: the durable lifecycle state a
// CreateVolume call found (SP1).
const (
	createResumedFromNew           = "new"
	createResumedFromProvisioning  = "provisioning"
	createResumedFromCreatePartial = "create_partial"
	createResumedFromReady         = "ready"
)

// Values of pillar_csi.attach_mode.
const (
	attachModeLocal   = "local"
	attachModeNetwork = "network"
)

// setPublishTargetAttributes sets the common volume attributes and
// pillar_csi.target_node.name (when set) for Publish/Unpublish.
func setPublishTargetAttributes(ctx context.Context, volumeID, nodeID string) {
	telemetry.SetVolumeAttributes(ctx, volumeID)
	if nodeID != "" {
		setSpanAttributes(ctx, telemetry.KeyTargetNodeName.String(nodeID))
	}
}

// setAttachAttributes sets pillar_csi.attach_mode and pillar_csi.readonly for
// a publish.
func setAttachAttributes(ctx context.Context, local, readonly bool) {
	setSpanAttributes(ctx,
		telemetry.KeyAttachMode.String(attachModeLabel(local)),
		telemetry.KeyReadonly.Bool(readonly),
	)
}

// attachModeLabel returns pillar_csi.attach_mode for a publish.
func attachModeLabel(local bool) string {
	if local {
		return attachModeLocal
	}
	return attachModeNetwork
}

// createResumedFrom classifies the PillarVolumeState a CreateVolume call
// found; a missing record is "new".
func createResumedFrom(pvs *v1alpha1.PillarVolumeState, exists bool) string {
	if !exists || pvs == nil {
		return createResumedFromNew
	}
	switch pvs.Status.Phase {
	case v1alpha1.PillarVolumeStatePhaseCreatePartial:
		return createResumedFromCreatePartial
	case v1alpha1.PillarVolumeStatePhaseProvisioning, "":
		return createResumedFromProvisioning
	default:
		return createResumedFromReady
	}
}

// setSpanAttributes sets attrs on the span in ctx when it is recording.
func setSpanAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// setClaimAttributes sets pillar_csi.pvc.name / pillar_csi.pvc.namespace from
// a PillarVolumeState's claim reference; empty fields are skipped.
func setClaimAttributes(ctx context.Context, claim *v1alpha1.VolumeClaimRef) {
	if claim == nil {
		return
	}
	setPVCAttributes(ctx, claim.Name, claim.Namespace)
}

// setPVCAttributes sets pillar_csi.pvc.name / pillar_csi.pvc.namespace;
// empty values are skipped.
func setPVCAttributes(ctx context.Context, name, namespace string) {
	var attrs []attribute.KeyValue
	if name != "" {
		attrs = append(attrs, telemetry.KeyPVCName.String(name))
	}
	if namespace != "" {
		attrs = append(attrs, telemetry.KeyPVCNamespace.String(namespace))
	}
	if len(attrs) > 0 {
		setSpanAttributes(ctx, attrs...)
	}
}

// withAgentName records the PillarAgent an RPC is routed to on the ctx used
// for the agent calls, so the client span and
// pillar_csi_agent_client_requests_total carry it.
func withAgentName(ctx context.Context, agentName string) context.Context {
	return agentclient.WithAgentName(ctx, agentName)
}

// addPVSUpdateEvent records pillar_csi.pvs.update after a successful
// PillarVolumeState status write.
func addPVSUpdateEvent(ctx context.Context, from, to v1alpha1.PillarVolumeStatePhase, gen int64, retries int) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.AddEvent(telemetry.EventPVSUpdate, trace.WithAttributes(
		telemetry.KeyPVSPhaseFrom.String(string(from)),
		telemetry.KeyPVSPhaseTo.String(string(to)),
		telemetry.KeyPVSPublicationGeneration.Int64(gen),
		telemetry.KeyPVSConflictRetries.Int(retries),
	))
}

// addPVSAbortedEvent records pillar_csi.pvs.aborted when the lifecycle an
// operation started on no longer exists.
func addPVSAbortedEvent(ctx context.Context) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.AddEvent(telemetry.EventPVSAborted)
	}
}
