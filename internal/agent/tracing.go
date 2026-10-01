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
	"errors"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// Span-specific error.type values of the nvmet spans (SP7).
const (
	errorTypePortInlineDataSizeConflict = "port_inline_data_size_conflict"
	errorTypePortMDTSConflict           = "port_mdts_conflict"
	errorTypeDeviceHeld                 = "device_held"
)

// ReconcileState phases (pillar_csi.reconcile.phase of the item_failed event).
const (
	reconcilePhaseResolve = "resolve"
	reconcilePhasePrepare = "prepare"
	reconcilePhaseLink    = "link"
)

// recordReconcileItemFailed adds a pillar_csi.reconcile.item_failed event for
// one failed ReconcileState item to the span in ctx.
func recordReconcileItemFailed(ctx context.Context, volumeID, phase string, err error) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		telemetry.KeyReconcilePhase.String(phase),
		semconv.ErrorTypeKey.String(telemetry.ErrorType(err)),
	}
	for _, kv := range telemetry.AgentVolumeAttributes(volumeID) {
		if kv.Key == telemetry.KeyPVName {
			attrs = append(attrs, kv)
		}
	}
	span.AddEvent(telemetry.EventReconcileItemFailed, trace.WithAttributes(attrs...))
}

// endDeviceWaitSpan ends the device_wait span (SP6): error.type is timeout
// when the device did not appear in time, else the INTERNAL rule.
func endDeviceWaitSpan(span trace.Span, waitErr error) {
	if waitErr != nil {
		errorType := ""
		if errors.Is(waitErr, context.DeadlineExceeded) {
			errorType = telemetry.ErrorTypeTimeout
		}
		telemetry.SetSpanError(span, waitErr, errorType)
	}
	span.End()
}

// startChildSpan starts an INTERNAL span under the span in ctx.  Without a
// valid parent span context it returns a no-op span, so work outside a traced
// RPC never creates a root span.  Nothing runs under these leaf spans, so
// their context is not returned.
func startChildSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) trace.Span {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return noop.Span{}
	}
	_, span := telemetry.Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	return span
}

// setVolumeSpanAttributes sets the common volume attributes of an agent RPC
// span (SP5): pillar_csi.pv.name and pillar_csi.pool.name from the agent
// volume ID, and pillar_csi.backend.type when the pool has a backend.
func (s *Server) setVolumeSpanAttributes(ctx context.Context, volumeID string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(telemetry.AgentVolumeAttributes(volumeID)...)
	pool, err := poolFromVolumeID(volumeID)
	if err != nil {
		return
	}
	b, ok := s.backends[pool]
	if ok {
		span.SetAttributes(telemetry.KeyBackendType.String(backendLabel(b.Type())))
	}
}

// traceNvmet runs op, one NvmetTarget call, inside the SP7 nvmet span name.
// HostNQN is the initiator NQN of allow/deny and empty otherwise.  The
// target's port and ACL setting are recorded only when the target carries
// them (targets built from export parameters).
func traceNvmet(ctx context.Context, name string, target *nvmeof.NvmetTarget, hostNQN string, op func() error) error {
	attrs := []attribute.KeyValue{telemetry.KeyNVMeSubsystemNQN.String(target.SubsystemNQN)}
	if target.Port != 0 {
		attrs = append(attrs,
			telemetry.KeyNVMetPort.Int(int(target.Port)),
			telemetry.KeyNVMetACLEnabled.Bool(target.ACLEnabled))
	}
	if hostNQN != "" {
		attrs = append(attrs, telemetry.KeyNVMeHostNQN.String(hostNQN))
	}
	span := startChildSpan(ctx, name, attrs...)
	defer span.End()

	err := op()
	if err != nil {
		setNvmetSpanError(span, target.ConfigfsRoot, err)
	}
	return err
}

// setNvmetSpanError applies the SP7 error rule: the configfs operation and
// the path relative to the configfs root from the error chain, and error.type
// from the nvmeof sentinels, else the errno name, else the INTERNAL rule.
// Written configfs values are never recorded.
func setNvmetSpanError(span trace.Span, configfsRoot string, err error) {
	if !span.IsRecording() {
		return
	}
	if configfsRoot == "" {
		configfsRoot = nvmeof.DefaultConfigfsRoot
	}
	var (
		op, path string
		pathErr  *os.PathError
		linkErr  *os.LinkError
	)
	switch {
	case errors.As(err, &pathErr):
		op, path = pathErr.Op, pathErr.Path
	case errors.As(err, &linkErr):
		op, path = linkErr.Op, linkErr.New
	}
	if op != "" {
		span.SetAttributes(telemetry.KeyConfigfsOp.String(op))
		rel, relErr := filepath.Rel(configfsRoot, path)
		if relErr == nil {
			span.SetAttributes(telemetry.KeyConfigfsPath.String(rel))
		}
	}
	errorType := ""
	switch {
	case errors.Is(err, nvmeof.ErrPortInlineDataSizeConflict):
		errorType = errorTypePortInlineDataSizeConflict
	case errors.Is(err, nvmeof.ErrPortMDTSConflict):
		errorType = errorTypePortMDTSConflict
	case errors.Is(err, nvmeof.ErrDeviceHeld):
		errorType = errorTypeDeviceHeld
	}
	telemetry.SetSpanError(span, err, errorType)
}
