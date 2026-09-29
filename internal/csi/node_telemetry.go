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

	"go.opentelemetry.io/otel/trace"

	csi "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// Node span attribute values (SP9).
const (
	// The pillar_csi.attach_mode value of a protocol attach; a local attach
	// uses AttachModeLocal.
	spanAttachModeNetwork = "network"

	// The pillar_csi.access_type values.
	spanAccessTypeMount = "mount"
	spanAccessTypeBlock = "block"

	// Volume context keys kubelet sets on NodePublishVolume when the
	// CSIDriver has podInfoOnMount.
	volumeContextKeyPodName      = "csi.storage.k8s.io/pod.name"
	volumeContextKeyPodNamespace = "csi.storage.k8s.io/pod.namespace"
)

// spanAttachMode returns the pillar_csi.attach_mode value.
func spanAttachMode(local bool) string {
	if local {
		return AttachModeLocal
	}
	return spanAttachModeNetwork
}

// setSpanAttachMode sets pillar_csi.attach_mode from a persisted stage
// state.  Nothing is set without one: the attach mode is unknown then.
func setSpanAttachMode(ctx context.Context, state *nodeStageState) {
	if state == nil {
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyAttachMode.String(spanAttachMode(state.isLocalAttach())))
}

// setSpanAccessType sets pillar_csi.access_type from the volume capability;
// a capability with neither access type sets nothing.
func setSpanAccessType(ctx context.Context, volCap *csi.VolumeCapability) {
	var accessType string
	switch {
	case volCap.GetMount() != nil:
		accessType = spanAccessTypeMount
	case volCap.GetBlock() != nil:
		accessType = spanAccessTypeBlock
	default:
		return
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.KeyAccessType.String(accessType))
}

// setPublishSpanAttributes sets the SP9 NodePublishVolume attributes: the
// common volume attributes, pillar_csi.readonly, and pillar_csi.pod.name /
// pillar_csi.pod.namespace from the volume context (omitted when absent).
func setPublishSpanAttributes(ctx context.Context, req *csi.NodePublishVolumeRequest) {
	telemetry.SetVolumeAttributes(ctx, req.GetVolumeId())
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(telemetry.KeyReadonly.Bool(req.GetReadonly()))
	volCtx := req.GetVolumeContext()
	if name := volCtx[volumeContextKeyPodName]; name != "" {
		span.SetAttributes(telemetry.KeyPodName.String(name))
	}
	if ns := volCtx[volumeContextKeyPodNamespace]; ns != "" {
		span.SetAttributes(telemetry.KeyPodNamespace.String(ns))
	}
}

// formatAndMount runs Mounter.FormatAndMount inside the SP13
// pillar_csi.node.format_and_mount span.  The mounter adds
// pillar_csi.fs.detected and pillar_csi.mkfs.performed to it.
func (n *NodeServer) formatAndMount(
	ctx context.Context, source, target, fsType string, options, formatOptions []string,
) error {
	ctx, span := telemetry.Tracer().Start(ctx, telemetry.SpanNodeFormatAndMount,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(telemetry.KeyFSType.String(fsType)),
	)
	defer span.End()
	err := n.mounter.FormatAndMount(ctx, source, target, fsType, options, formatOptions)
	telemetry.SetSpanError(span, err, "")
	return err //nolint:wrapcheck // the caller wraps it into a gRPC status
}
