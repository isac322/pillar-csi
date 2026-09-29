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

package agentclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/isac322/pillar-csi/internal/telemetry"
)

// agentNameKey is the context key carrying the PillarAgent name of the agent
// a controller->agent RPC is addressed to.
type agentNameKey struct{}

// WithAgentName returns a copy of ctx that carries the PillarAgent name the
// next agent RPCs are addressed to. The agent label of
// pillar_csi_agent_client_requests_total and the pillar_csi.agent.name
// attribute of the client span are taken from it.
func WithAgentName(ctx context.Context, agentName string) context.Context {
	return context.WithValue(ctx, agentNameKey{}, agentName)
}

// AgentNameFromContext returns the PillarAgent name stored by
// [WithAgentName], or "" when ctx carries none.
func AgentNameFromContext(ctx context.Context) string {
	if name, ok := ctx.Value(agentNameKey{}).(string); ok {
		return name
	}
	return ""
}

// clientRequests is M10, pillar_csi_agent_client_requests_total.
var clientRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pillar_csi_agent_client_requests_total",
	Help: "Controller->agent unary RPCs as observed by the controller, by agent, method and gRPC code.",
}, []string{"agent", "method", "code"})

// RegisterMetrics registers pillar_csi_agent_client_requests_total on reg.
func RegisterMetrics(reg prometheus.Registerer) error {
	err := reg.Register(clientRequests)
	if err != nil {
		return fmt.Errorf("register pillar_csi_agent_client_requests_total: %w", err)
	}
	return nil
}

// unaryClientMetricsInterceptor counts every controller->agent unary RPC by
// agent (from ctx, falling back to the connection target), method and code.
func unaryClientMetricsInterceptor(
	ctx context.Context,
	method string,
	req, reply any,
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	agent := AgentNameFromContext(ctx)
	if agent == "" {
		agent = cc.Target()
	}
	clientRequests.WithLabelValues(agent, rpcMethodName(method), status.Code(err).String()).Inc()
	return err
}

// rpcMethodName returns the method part of a full gRPC method name
// ("/pkg.Service/Method" -> "Method").
func rpcMethodName(fullMethod string) string {
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 {
		return fullMethod[idx+1:]
	}
	return fullMethod
}

// TelemetryDialOptions returns the dial options that instrument a
// controller->agent connection: the otelgrpc client handler (SP2), with
// pillar_csi.agent.name set to agentName when non-empty, and the
// pillar_csi_agent_client_requests_total interceptor (M10).
func TelemetryDialOptions(agentName string) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithStatsHandler(telemetry.AgentClientHandler(agentName)),
		grpc.WithChainUnaryInterceptor(unaryClientMetricsInterceptor),
	}
}
