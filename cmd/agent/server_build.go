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
	"context"
	"crypto/x509"
	"net/url"
	"strings"

	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

const (
	fileNodeIdentityURI    = "spiffe://pillar-csi/file-node"
	agentServiceMethodRoot = "/pillar_csi.agent.v1.AgentService/"
)

type fileNodeIdentityStatus uint8

const (
	fileNodeIdentityAbsent fileNodeIdentityStatus = iota
	fileNodeIdentityPresent
	fileNodeIdentityMalformed
)

// fileNodeIdentityFromContext returns the identity of a TLS peer. Only the
// leaf certificate's URI SANs are authoritative. A URI explicitly marked as a
// file-node identity but carrying extra path or URL components is malformed
// and must not be treated as an unrestricted client identity.
func fileNodeIdentityFromContext(ctx context.Context) fileNodeIdentityStatus {
	p, ok := peer.FromContext(ctx)
	if !ok || p == nil || p.AuthInfo == nil {
		return fileNodeIdentityAbsent
	}

	info, recognized, identityStatus := fileNodeTLSInfo(p.AuthInfo)
	if !recognized || identityStatus != fileNodeIdentityAbsent {
		return identityStatus
	}
	if len(info.State.PeerCertificates) == 0 {
		return fileNodeIdentityAbsent
	}
	return fileNodeIdentityFromLeaf(info.State.PeerCertificates[0])
}

func fileNodeTLSInfo(authInfo credentials.AuthInfo) (
	credentials.TLSInfo,
	bool,
	fileNodeIdentityStatus,
) {
	switch authInfo := authInfo.(type) {
	case credentials.TLSInfo:
		return authInfo, true, fileNodeIdentityAbsent
	case *credentials.TLSInfo:
		if authInfo == nil {
			return credentials.TLSInfo{}, true, fileNodeIdentityMalformed
		}
		return *authInfo, true, fileNodeIdentityAbsent
	default:
		return credentials.TLSInfo{}, false, fileNodeIdentityAbsent
	}
}

func fileNodeIdentityFromLeaf(leaf *x509.Certificate) fileNodeIdentityStatus {
	if leaf == nil {
		return fileNodeIdentityMalformed
	}
	return fileNodeIdentityFromURIs(leaf.URIs)
}

func fileNodeIdentityFromURIs(uris []*url.URL) fileNodeIdentityStatus {
	found := false
	for _, uri := range uris {
		if uri == nil {
			return fileNodeIdentityMalformed
		}
		if !fileNodeURIIsMarked(uri) {
			continue
		}
		if !fileNodeURIIsExact(uri) || found {
			return fileNodeIdentityMalformed
		}
		found = true
	}
	if found {
		return fileNodeIdentityPresent
	}
	return fileNodeIdentityAbsent
}

func fileNodeURIIsMarked(uri *url.URL) bool {
	return uri.Scheme == "spiffe" && uri.Host == "pillar-csi" &&
		(uri.Path == "/file-node" || strings.HasPrefix(uri.Path, "/file-node/"))
}

func fileNodeURIIsExact(uri *url.URL) bool {
	return uri.String() == fileNodeIdentityURI &&
		uri.Path == "/file-node" &&
		uri.RawQuery == "" &&
		uri.Fragment == "" &&
		uri.Opaque == "" &&
		uri.User == nil
}

func fileNodeAuthorizationError(identity fileNodeIdentityStatus, fullMethod string, stream bool) error {
	if !strings.HasPrefix(fullMethod, agentServiceMethodRoot) {
		return nil
	}
	switch identity {
	case fileNodeIdentityPresent:
		if !stream && fullMethod == agentv1.AgentService_InspectImport_FullMethodName {
			return nil
		}
	case fileNodeIdentityMalformed:
		// A malformed identity in the reserved SPIFFE namespace is denied
		// rather than falling back to the unrestricted legacy behavior.
	default:
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "file-node identity is not authorized for %s", fullMethod)
}

func fileNodeUnaryAuthorizationInterceptor(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	var fullMethod string
	if info != nil {
		fullMethod = info.FullMethod
	}
	err := fileNodeAuthorizationError(fileNodeIdentityFromContext(ctx), fullMethod, false)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func fileNodeStreamAuthorizationInterceptor(
	srv any,
	stream grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) error {
	var fullMethod string
	if info != nil {
		fullMethod = info.FullMethod
	}
	err := fileNodeAuthorizationError(fileNodeIdentityFromContext(stream.Context()), fullMethod, true)
	if err != nil {
		return err
	}
	return handler(srv, stream)
}

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

// newAgentGRPCServer constructs the agent gRPC server with its telemetry, the
// file-node authorization and DrainGuard interceptors installed, and the
// standard gRPC health service registered.  Extracted from main() so the
// wiring is end-to-end testable via bufconn.
//
// The otelgrpc stats handler starts the server span before any interceptor
// runs.  The unary chain is, outermost first: the gRPC metrics, the failure
// log, file-node authorization, then DrainGuard.  Metrics and the failure log
// sit outside both authorization and DrainGuard so their rejections are
// counted and logged.
//
// The stream chain is, outermost first: the gRPC metrics, then file-node
// authorization.
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
			fileNodeUnaryAuthorizationInterceptor,
			agent.DrainGuardInterceptor(srv),
		),
		grpc.ChainStreamInterceptor(
			metrics.StreamServerInterceptor(exemplar),
			fileNodeStreamAuthorizationInterceptor,
		),
	}, grpcOpts...)
	g = grpc.NewServer(opts...)
	srv.Register(g)
	health = healthsrv.NewServer()
	healthpb.RegisterHealthServer(g, health)
	health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	metrics.InitializeMetrics(g)
	return g, health
}
