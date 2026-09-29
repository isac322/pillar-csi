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

package main

import (
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"google.golang.org/grpc"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// agentRPCBuckets are the grpc_server_handling_seconds buckets (seconds):
// fast configfs RPCs through slow zfs/lvm commands.
var agentRPCBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// newAgentServerMetrics returns the gRPC server metrics (grpc_server_*) of
// the agent.  Register the result on the metrics registry.
func newAgentServerMetrics() *grpcprom.ServerMetrics {
	return grpcprom.NewServerMetrics(
		grpcprom.WithServerHandlingTimeHistogram(grpcprom.WithHistogramBuckets(agentRPCBuckets)),
	)
}

// newAgentGRPCServer constructs the agent gRPC server with its telemetry and
// the DrainGuard interceptor installed and the standard gRPC health service
// registered.  Extracted from main() so the wiring is end-to-end testable
// via bufconn.
//
// The otelgrpc stats handler starts the server span before any interceptor
// runs.  The unary chain is, outermost first: the gRPC metrics, the failure
// log, then DrainGuard.  Metrics and the failure log sit outside DrainGuard
// so that drain and restore-gate rejections are counted and logged.
func newAgentGRPCServer(
	srv *agent.Server,
	metrics *grpcprom.ServerMetrics,
	failureLog telemetry.FailureLogger,
	grpcOpts []grpc.ServerOption,
) (g *grpc.Server, health *healthsrv.Server) {
	exemplar := grpcprom.WithExemplarFromContext(telemetry.ExemplarFromContext)
	opts := append([]grpc.ServerOption{
		grpc.StatsHandler(telemetry.AgentServerHandler()),
		grpc.ChainUnaryInterceptor(
			metrics.UnaryServerInterceptor(exemplar),
			telemetry.UnaryServerFailureInterceptor(failureLog),
			agent.DrainGuardInterceptor(srv),
		),
		grpc.ChainStreamInterceptor(metrics.StreamServerInterceptor(exemplar)),
	}, grpcOpts...)
	g = grpc.NewServer(opts...)
	srv.Register(g)
	health = healthsrv.NewServer()
	healthpb.RegisterHealthServer(g, health)
	health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	metrics.InitializeMetrics(g)
	return g, health
}
