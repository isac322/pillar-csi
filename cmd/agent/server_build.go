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
	"google.golang.org/grpc"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/bhyoo/pillar-csi/internal/agent"
)

// newAgentGRPCServer constructs the agent gRPC server with the
// DrainGuard interceptor installed and the standard gRPC health service
// registered.  Extracted from main() so the wiring is end-to-end
// testable via bufconn: a Drain call followed by any other RPC must
// surface codes.Unavailable, proving that the interceptor is in effect.
func newAgentGRPCServer(srv *agent.Server, grpcOpts []grpc.ServerOption) (g *grpc.Server, health *healthsrv.Server) {
	opts := append([]grpc.ServerOption{grpc.UnaryInterceptor(agent.DrainGuardInterceptor(srv))}, grpcOpts...)
	g = grpc.NewServer(opts...)
	srv.Register(g)
	health = healthsrv.NewServer()
	healthpb.RegisterHealthServer(g, health)
	health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return g, health
}
