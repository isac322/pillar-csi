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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/telemetry"
)

// syncBuffer is a bytes.Buffer safe for the server's concurrent log writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// agentTestServer is a bufconn-served agent gRPC server built by
// newAgentGRPCServer, with its metrics registry and failure log exposed.
type agentTestServer struct {
	client agentv1.AgentServiceClient
	reg    *prometheus.Registry
	logs   *syncBuffer
}

type staticAuthCredentials struct {
	authInfo credentials.AuthInfo
}

func (c staticAuthCredentials) ClientHandshake(
	_ context.Context,
	_ string,
	conn net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	return conn, c.authInfo, nil
}

func (c staticAuthCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, c.authInfo, nil
}

func (staticAuthCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "static-test-tls"}
}

func (c staticAuthCredentials) Clone() credentials.TransportCredentials { return c }
func (staticAuthCredentials) OverrideServerName(string) error           { return nil }

func startAgentTestServer(t *testing.T) agentTestServer {
	return startAgentTestServerWithCredentials(t, nil, nil)
}

func startAgentTestServerWithCredentials(
	t *testing.T,
	serverCreds, clientCreds credentials.TransportCredentials,
) agentTestServer {
	t.Helper()
	srv := agent.NewServer(nil, t.TempDir(), agent.WithDrainStateDir(t.TempDir()))
	metrics := newAgentServerMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics)
	logs := &syncBuffer{}
	failureLog := telemetry.SlogFailureLogger(slog.New(slog.NewJSONHandler(logs, nil)))
	var grpcOpts []grpc.ServerOption
	if serverCreds != nil {
		grpcOpts = []grpc.ServerOption{grpc.Creds(serverCreds)}
	}
	g, _ := newAgentGRPCServer(srv, metrics, failureLog, grpcOpts)

	lis := bufconn.Listen(1 << 20)
	serveErr := make(chan error, 1)
	go func() { serveErr <- g.Serve(lis) }()
	t.Cleanup(func() {
		g.Stop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve exited with unexpected error: %v", err)
		}
	})

	transportCreds := insecure.NewCredentials()
	if clientCreds != nil {
		transportCreds = clientCreds
	}
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(transportCreds),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("conn.Close returned unexpected error: %v", err)
		}
	})
	return agentTestServer{client: agentv1.NewAgentServiceClient(conn), reg: reg, logs: logs}
}

func tlsPeerContext(t *testing.T, identities ...string) context.Context {
	t.Helper()
	uris := make([]*url.URL, 0, len(identities))
	for _, identity := range identities {
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatalf("parse peer identity %q: %v", identity, err)
		}
		uris = append(uris, uri)
	}
	return peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{{URIs: uris}},
			},
		},
	})
}

type authorizationTestStream struct {
	ctx context.Context
}

func (*authorizationTestStream) SetHeader(metadata.MD) error  { return nil }
func (*authorizationTestStream) SendHeader(metadata.MD) error { return nil }
func (*authorizationTestStream) SetTrailer(metadata.MD)       {}
func (s *authorizationTestStream) Context() context.Context   { return s.ctx }
func (*authorizationTestStream) SendMsg(any) error            { return nil }
func (*authorizationTestStream) RecvMsg(any) error            { return nil }

func TestFileNodeAuthorizationInterceptors(t *testing.T) {
	t.Run("unary", func(t *testing.T) {
		tests := []struct {
			name       string
			ctx        context.Context
			method     string
			wantCode   codes.Code
			wantCalled bool
		}{
			{
				name:       "InspectImport allowed",
				ctx:        tlsPeerContext(t, fileNodeIdentityURI),
				method:     agentv1.AgentService_InspectImport_FullMethodName,
				wantCode:   codes.OK,
				wantCalled: true,
			},
			{
				name:     "Drain denied",
				ctx:      tlsPeerContext(t, fileNodeIdentityURI),
				method:   agentv1.AgentService_Drain_FullMethodName,
				wantCode: codes.PermissionDenied,
			},
			{
				name:     "Import denied",
				ctx:      tlsPeerContext(t, fileNodeIdentityURI),
				method:   agentv1.AgentService_ImportVolume_FullMethodName,
				wantCode: codes.PermissionDenied,
			},
			{
				name:       "plaintext unchanged",
				ctx:        context.Background(),
				method:     agentv1.AgentService_Drain_FullMethodName,
				wantCode:   codes.OK,
				wantCalled: true,
			},
			{
				name:       "other identity unchanged",
				ctx:        tlsPeerContext(t, "spiffe://pillar-csi/controller"),
				method:     agentv1.AgentService_Drain_FullMethodName,
				wantCode:   codes.OK,
				wantCalled: true,
			},
			{
				name:     "malformed reserved identity denied",
				ctx:      tlsPeerContext(t, "spiffe://pillar-csi/file-node?unexpected=true"),
				method:   agentv1.AgentService_InspectImport_FullMethodName,
				wantCode: codes.PermissionDenied,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				called := false
				_, err := fileNodeUnaryAuthorizationInterceptor(
					tt.ctx,
					nil,
					&grpc.UnaryServerInfo{FullMethod: tt.method},
					func(context.Context, any) (any, error) {
						called = true
						return struct{}{}, nil
					},
				)
				if got := status.Code(err); got != tt.wantCode {
					t.Fatalf("status.Code(err) = %v, want %v (err=%v)", got, tt.wantCode, err)
				}
				if called != tt.wantCalled {
					t.Fatalf("handler called = %t, want %t", called, tt.wantCalled)
				}
			})
		}
	})

	t.Run("stream denied", func(t *testing.T) {
		stream := &authorizationTestStream{ctx: tlsPeerContext(t, fileNodeIdentityURI)}
		called := false
		err := fileNodeStreamAuthorizationInterceptor(
			nil,
			stream,
			&grpc.StreamServerInfo{FullMethod: agentv1.AgentService_InspectImport_FullMethodName},
			func(any, grpc.ServerStream) error {
				called = true
				return nil
			},
		)
		if got := status.Code(err); got != codes.PermissionDenied {
			t.Fatalf("status.Code(err) = %v, want %v (err=%v)", got, codes.PermissionDenied, err)
		}
		if called {
			t.Fatal("stream handler was called for file-node identity")
		}
	})
}

func TestNewAgentGRPCServer_FileNodeAuthorization(t *testing.T) {
	identity, err := url.Parse(fileNodeIdentityURI)
	if err != nil {
		t.Fatalf("parse file-node identity: %v", err)
	}
	creds := staticAuthCredentials{
		authInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{identity}}},
			},
		},
	}
	client := startAgentTestServerWithCredentials(t, creds, creds).client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	_, inspectErr := client.InspectImport(ctx, &agentv1.InspectImportRequest{})
	if got := status.Code(inspectErr); got != codes.InvalidArgument {
		t.Fatalf("InspectImport status = %v, want %v (err=%v)", got, codes.InvalidArgument, inspectErr)
	}
	if _, err := client.Drain(ctx, &agentv1.DrainRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Drain status = %v, want %v (err=%v)", status.Code(err), codes.PermissionDenied, err)
	}
	if _, err := client.ImportVolume(ctx, &agentv1.ImportVolumeRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("ImportVolume status = %v, want %v (err=%v)", status.Code(err), codes.PermissionDenied, err)
	}
}

func TestNewAgentGRPCServer_DrainGuardWired(t *testing.T) {
	client := startAgentTestServer(t).client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	_, preErr := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
		PoolName:    "missing",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
	})
	if status.Code(preErr) == codes.Unavailable {
		t.Fatalf("pre-drain GetCapacity was Unavailable; the guard should be inert before Drain: %v", preErr)
	}

	if _, err := client.Drain(ctx, &agentv1.DrainRequest{}); err != nil {
		t.Fatalf("Drain RPC failed: %v", err)
	}

	_, postErr := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{
		PoolName:    "missing",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
	})
	if status.Code(postErr) != codes.Unavailable {
		t.Fatalf("post-drain GetCapacity should be Unavailable; got code=%v err=%v", status.Code(postErr), postErr)
	}

	if _, err := client.Drain(ctx, &agentv1.DrainRequest{}); err != nil {
		t.Fatalf("post-drain Drain (idempotent) was rejected by the guard: %v", err)
	}
}

// The gRPC metrics and the failure log sit outside DrainGuard, so an RPC
// the drained agent rejects is still counted and logged exactly once.
func TestNewAgentGRPCServer_DrainRejectionCountedAndLogged(t *testing.T) {
	ts := startAgentTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	if _, err := ts.client.Drain(ctx, &agentv1.DrainRequest{}); err != nil {
		t.Fatalf("Drain RPC failed: %v", err)
	}
	_, err := ts.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
		VolumeId:    "tank/pvc-drained",
		BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("post-drain CreateVolume: code=%v err=%v, want Unavailable", status.Code(err), err)
	}

	if got := handledCount(t, ts.reg, "CreateVolume", "Unavailable"); got != 1 {
		t.Errorf(`grpc_server_handled_total{grpc_method="CreateVolume",grpc_code="Unavailable"} = %v, want 1`, got)
	}

	var unavailable []map[string]any
	for line := range strings.Lines(ts.logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("failure log line is not JSON: %q: %v", line, err)
		}
		if rec[telemetry.LogKeyGRPCCode] == codes.Unavailable.String() {
			unavailable = append(unavailable, rec)
		}
	}
	if len(unavailable) != 1 {
		t.Fatalf("got %d failure log lines with grpc.code=Unavailable, want 1; log:\n%s",
			len(unavailable), ts.logs.String())
	}
	if m := unavailable[0][telemetry.LogKeyGRPCMethod]; m != agentv1.AgentService_CreateVolume_FullMethodName {
		t.Errorf("failure log grpc.method = %v, want %s", m, agentv1.AgentService_CreateVolume_FullMethodName)
	}
}

// handledCount returns grpc_server_handled_total for the AgentService method
// and code, or 0 when the series is absent.
func handledCount(t *testing.T, reg *prometheus.Registry, method, code string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "grpc_server_handled_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if labels["grpc_service"] == "pillar_csi.agent.v1.AgentService" &&
				labels["grpc_method"] == method && labels["grpc_code"] == code {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
