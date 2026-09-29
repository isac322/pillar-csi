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
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
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

func startAgentTestServer(t *testing.T) agentTestServer {
	t.Helper()
	srv := agent.NewServer(nil, t.TempDir(), agent.WithDrainStateDir(t.TempDir()))
	metrics := newAgentServerMetrics()
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics)
	logs := &syncBuffer{}
	failureLog := telemetry.SlogFailureLogger(slog.New(slog.NewJSONHandler(logs, nil)))
	g, _ := newAgentGRPCServer(srv, metrics, failureLog, nil)

	lis := bufconn.Listen(1 << 20)
	serveErr := make(chan error, 1)
	go func() { serveErr <- g.Serve(lis) }()
	t.Cleanup(func() {
		g.Stop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve exited with unexpected error: %v", err)
		}
	})

	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

func TestNewAgentGRPCServer_DrainGuardWired(t *testing.T) {
	client := startAgentTestServer(t).client
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	_, preErr := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{PoolName: "missing"})
	if status.Code(preErr) == codes.Unavailable {
		t.Fatalf("pre-drain GetCapacity was Unavailable; the guard should be inert before Drain: %v", preErr)
	}

	if _, err := client.Drain(ctx, &agentv1.DrainRequest{}); err != nil {
		t.Fatalf("Drain RPC failed: %v", err)
	}

	_, postErr := client.GetCapacity(ctx, &agentv1.GetCapacityRequest{PoolName: "missing"})
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
	_, err := ts.client.CreateVolume(ctx, &agentv1.CreateVolumeRequest{VolumeId: "tank/pvc-drained"})
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
