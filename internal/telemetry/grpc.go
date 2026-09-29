package telemetry

import (
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc/stats"
)

// Full method names of the traced/untraced RPCs.
const (
	agentServicePrefix = "/pillar_csi.agent.v1.AgentService/"
	grpcHealthPrefix   = "/grpc.health.v1.Health/"
)

// controllerTraced is the SP1 allowlist.
var controllerTraced = setOf(
	"/csi.v1.Controller/CreateVolume",
	"/csi.v1.Controller/DeleteVolume",
	"/csi.v1.Controller/ControllerPublishVolume",
	"/csi.v1.Controller/ControllerUnpublishVolume",
	"/csi.v1.Controller/ControllerExpandVolume",
)

// nodeTraced is the SP9 allowlist.
var nodeTraced = setOf(
	"/csi.v1.Node/NodeStageVolume",
	"/csi.v1.Node/NodeUnstageVolume",
	"/csi.v1.Node/NodePublishVolume",
	"/csi.v1.Node/NodeUnpublishVolume",
	"/csi.v1.Node/NodeExpandVolume",
)

// agentUntraced is the SP5 denylist (plus every grpc.health.v1.Health method).
var agentUntraced = setOf(
	agentServicePrefix+"HealthCheck",
	agentServicePrefix+"GetCapabilities",
	agentServicePrefix+"GetCapacity",
)

func controllerFilter(info *stats.RPCTagInfo) bool {
	_, ok := controllerTraced[info.FullMethodName]
	return ok
}

func nodeFilter(info *stats.RPCTagInfo) bool {
	_, ok := nodeTraced[info.FullMethodName]
	return ok
}

func agentFilter(info *stats.RPCTagInfo) bool {
	if strings.HasPrefix(info.FullMethodName, grpcHealthPrefix) {
		return false
	}
	_, denied := agentUntraced[info.FullMethodName]
	return !denied
}

// ControllerServerHandler is the otelgrpc server handler for the controller
// CSI server (SP1): only the five volume-mutating Controller RPCs are traced.
// Install with grpc.StatsHandler.
func ControllerServerHandler() stats.Handler {
	return otelgrpc.NewServerHandler(otelgrpc.WithFilter(controllerFilter))
}

// NodeServerHandler is the otelgrpc server handler for the node CSI server
// (SP9): only Stage/Unstage/Publish/Unpublish/Expand are traced.
func NodeServerHandler() stats.Handler {
	return otelgrpc.NewServerHandler(otelgrpc.WithFilter(nodeFilter))
}

// AgentServerHandler is the otelgrpc server handler for the agent gRPC
// server (SP5): everything except HealthCheck, GetCapabilities, GetCapacity
// and grpc.health.v1.Health is traced.
func AgentServerHandler() stats.Handler {
	return otelgrpc.NewServerHandler(otelgrpc.WithFilter(agentFilter))
}

// AgentClientHandler is the otelgrpc client handler for a controller->agent
// connection (SP2). A non-empty agentName is set as pillar_csi.agent.name on
// every client span of the connection. Parentless client spans are dropped
// by the sampler, not by a filter.
func AgentClientHandler(agentName string) stats.Handler {
	var opts []otelgrpc.Option
	if agentName != "" {
		opts = append(opts, otelgrpc.WithSpanAttributes(KeyAgentName.String(agentName)))
	}
	return otelgrpc.NewClientHandler(opts...)
}
