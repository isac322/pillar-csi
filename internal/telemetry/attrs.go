package telemetry

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Custom span attribute keys (pillar_csi.<entity>.<property>). Resource keys
// (k8s.*, service.*) are never set on spans.
const (
	// Common volume attributes, see [VolumeAttributes].
	KeyVolumeID     = attribute.Key("pillar_csi.volume.id")
	KeyPVName       = attribute.Key("pillar_csi.pv.name")
	KeyAgentName    = attribute.Key("pillar_csi.agent.name")
	KeyProtocolType = attribute.Key("pillar_csi.protocol.type")
	KeyBackendType  = attribute.Key("pillar_csi.backend.type")
	KeyPoolName     = attribute.Key("pillar_csi.pool.name")

	// Controller (SP1, SP3, SP4).
	KeyStoreName                = attribute.Key("pillar_csi.store.name")
	KeyPVCName                  = attribute.Key("pillar_csi.pvc.name")
	KeyPVCNamespace             = attribute.Key("pillar_csi.pvc.namespace")
	KeyCapacityRequestedBytes   = attribute.Key("pillar_csi.capacity.requested_bytes")
	KeyCapacityAllocatedBytes   = attribute.Key("pillar_csi.capacity.allocated_bytes")
	KeyCreateResumedFrom        = attribute.Key("pillar_csi.create.resumed_from")
	KeyTargetNodeName           = attribute.Key("pillar_csi.target_node.name")
	KeyAttachMode               = attribute.Key("pillar_csi.attach_mode")
	KeyReadonly                 = attribute.Key("pillar_csi.readonly")
	KeyPVSPhaseFrom             = attribute.Key("pillar_csi.pvs.phase.from")
	KeyPVSPhaseTo               = attribute.Key("pillar_csi.pvs.phase.to")
	KeyPVSPublicationGeneration = attribute.Key("pillar_csi.pvs.publication_generation")
	KeyPVSConflictRetries       = attribute.Key("pillar_csi.pvs.conflict_retries")
	KeyRestoreVolumes           = attribute.Key("pillar_csi.restore.volumes")
	KeyRestoreFailed            = attribute.Key("pillar_csi.restore.failed")
	KeyReapResult               = attribute.Key("pillar_csi.reap.result")

	// Agent (SP5, SP6, SP7).
	KeyFenceOp                = attribute.Key("pillar_csi.fence.op")
	KeyFenceDecision          = attribute.Key("pillar_csi.fence.decision")
	KeyLockTargetWaitDuration = attribute.Key("pillar_csi.lock.target.wait_duration")
	KeyReconcileComplete      = attribute.Key("pillar_csi.reconcile.complete")
	KeyReconcileVolumes       = attribute.Key("pillar_csi.reconcile.volumes")
	KeyReconcileFailed        = attribute.Key("pillar_csi.reconcile.failed")
	KeyReconcilePhase         = attribute.Key("pillar_csi.reconcile.phase")
	KeyDevicePath             = attribute.Key("pillar_csi.device.path")
	KeyWaitTimeout            = attribute.Key("pillar_csi.wait.timeout")
	KeyNVMeSubsystemNQN       = attribute.Key("pillar_csi.nvme.subsystem_nqn")
	KeyNVMeHostNQN            = attribute.Key("pillar_csi.nvme.host_nqn")
	KeyNVMetPort              = attribute.Key("pillar_csi.nvmet.port")
	KeyNVMetACLEnabled        = attribute.Key("pillar_csi.nvmet.acl_enabled")
	KeyConfigfsOp             = attribute.Key("pillar_csi.configfs.op")
	KeyConfigfsPath           = attribute.Key("pillar_csi.configfs.path")

	// Exec (SP8).
	KeyExecSubcommand = attribute.Key("pillar_csi.exec.subcommand")
	KeyExecOutputTail = attribute.Key("pillar_csi.exec.output_tail")

	// Node (SP9-SP13).
	KeyAccessType            = attribute.Key("pillar_csi.access_type")
	KeyFSType                = attribute.Key("pillar_csi.fs.type")
	KeyFSDetected            = attribute.Key("pillar_csi.fs.detected")
	KeyMkfsPerformed         = attribute.Key("pillar_csi.mkfs.performed")
	KeyStageAlreadyStaged    = attribute.Key("pillar_csi.stage.already_staged")
	KeyPodName               = attribute.Key("pillar_csi.pod.name")
	KeyPodNamespace          = attribute.Key("pillar_csi.pod.namespace")
	KeyNVMeAlreadyConnected  = attribute.Key("pillar_csi.nvme.already_connected")
	KeyNVMeDyingWaitDuration = attribute.Key("pillar_csi.nvme.dying_wait_duration")
	KeyNVMeHdrDigest         = attribute.Key("pillar_csi.nvme.hdr_digest")
	KeyNVMeDataDigest        = attribute.Key("pillar_csi.nvme.data_digest")
	KeyPollAttempts          = attribute.Key("pillar_csi.poll.attempts")
)

// Span event names.
const (
	EventPVSUpdate           = "pillar_csi.pvs.update"
	EventPVSAborted          = "pillar_csi.pvs.aborted"
	EventReconcileItemFailed = "pillar_csi.reconcile.item_failed"
)

// Manual span names (pillar_csi.<component>.<op>).
const (
	SpanControllerRestoreExports = "pillar_csi.controller.restore_exports"
	SpanControllerVolumeReap     = "pillar_csi.controller.volume_reap"
	SpanAgentDeviceWait          = "pillar_csi.agent.device_wait"
	SpanAgentNVMetApply          = "pillar_csi.agent.nvmet.apply"
	SpanAgentNVMetRemove         = "pillar_csi.agent.nvmet.remove"
	SpanAgentNVMetAllowHost      = "pillar_csi.agent.nvmet.allow_host"
	SpanAgentNVMetDenyHost       = "pillar_csi.agent.nvmet.deny_host"
	SpanAgentNVMetDisableNS      = "pillar_csi.agent.nvmet.disable_namespace"
	SpanAgentNVMetEnableNS       = "pillar_csi.agent.nvmet.enable_namespace"
	SpanAgentNVMetResizeNS       = "pillar_csi.agent.nvmet.resize_namespace"
	SpanNodeNVMeoFConnect        = "pillar_csi.node.nvmeof_connect"
	SpanNodeNVMeoFDeviceWait     = "pillar_csi.node.nvmeof_device_wait"
	SpanNodeNVMeoFDisconnect     = "pillar_csi.node.nvmeof_disconnect"
	SpanNodeFormatAndMount       = "pillar_csi.node.format_and_mount"
)

// csiVolumeIDParts is the field count of a CSI volume ID
// "<agent>/<protocol>/<backend>/<pool>/<pv>" when split with a limit, so the
// agent volume ID "<pool>/<pv>" (the pool may itself contain '/') stays whole.
const csiVolumeIDParts = 4

// VolumeAttributes returns the common volume attributes for a CSI volume ID
// "<agent>/<protocol>/<backend>/<pool>/<pv>" (built at
// internal/csi/controller.go CreateVolume). The pillar_csi.volume.id
// attribute is always present for a non-empty ID; the parsed attributes are
// added only when the ID has the pillar format, so a foreign ID never yields
// guessed values.
func VolumeAttributes(volumeID string) []attribute.KeyValue {
	if volumeID == "" {
		return nil
	}
	attrs := []attribute.KeyValue{KeyVolumeID.String(volumeID)}
	parts := strings.SplitN(volumeID, "/", csiVolumeIDParts)
	if len(parts) != csiVolumeIDParts {
		return attrs
	}
	attrs = append(attrs,
		KeyAgentName.String(parts[0]),
		KeyProtocolType.String(parts[1]),
		KeyBackendType.String(parts[2]),
	)
	return append(attrs, AgentVolumeAttributes(parts[3])...)
}

// AgentVolumeAttributes returns pillar_csi.pv.name and pillar_csi.pool.name
// for an agent volume ID "<pool>/<pv>". The PV name is the last segment; the
// pool is everything before it and is omitted when absent.
func AgentVolumeAttributes(agentVolumeID string) []attribute.KeyValue {
	pool, pv := splitAgentVolumeID(agentVolumeID)
	var attrs []attribute.KeyValue
	if pv != "" {
		attrs = append(attrs, KeyPVName.String(pv))
	}
	if pool != "" {
		attrs = append(attrs, KeyPoolName.String(pool))
	}
	return attrs
}

// SetVolumeAttributes sets [VolumeAttributes] on the span in ctx.
func SetVolumeAttributes(ctx context.Context, volumeID string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(VolumeAttributes(volumeID)...)
	}
}

// SetAgentVolumeAttributes sets [AgentVolumeAttributes] on the span in ctx.
func SetAgentVolumeAttributes(ctx context.Context, agentVolumeID string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(AgentVolumeAttributes(agentVolumeID)...)
	}
}

func splitAgentVolumeID(id string) (pool, pv string) {
	if prefix, suffix, found := strings.CutLast(id, "/"); found {
		return prefix, suffix
	}
	return "", id
}
