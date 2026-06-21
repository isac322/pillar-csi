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
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
)

func TestNewAgentGRPCServer_DrainGuardWired(t *testing.T) {
	srv := agent.NewServer(nil, t.TempDir(), agent.WithDrainStateDir(t.TempDir()))
	g, _ := newAgentGRPCServer(srv, nil)

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

	client := agentv1.NewAgentServiceClient(conn)
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
