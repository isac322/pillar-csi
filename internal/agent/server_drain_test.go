package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
)

const drainTestPool = "tank"

type drainTestBackend struct {
	capacityTotal     int64
	capacityAvailable int64
}

func (*drainTestBackend) Create(
	_ context.Context,
	_ string,
	_ int64,
	_ *agentv1.BackendParams,
) (devicePath string, allocatedBytes int64, err error) {
	return "", 0, nil
}

func (*drainTestBackend) Delete(_ context.Context, _ string) error {
	return nil
}

func (*drainTestBackend) Expand(_ context.Context, _ string, _ int64) (allocatedBytes int64, err error) {
	return 0, nil
}

func (b *drainTestBackend) Capacity(_ context.Context) (totalBytes, availableBytes int64, err error) {
	return b.capacityTotal, b.capacityAvailable, nil
}

func (*drainTestBackend) ListVolumes(_ context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}

func (*drainTestBackend) DevicePath(_ string) string {
	return ""
}

func (*drainTestBackend) Type() agentv1.BackendType {
	return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL
}

var _ backend.VolumeBackend = (*drainTestBackend)(nil)

func newDrainTestServer(stateDir string) *Server {
	backends := map[string]backend.VolumeBackend{
		drainTestPool: &drainTestBackend{
			capacityTotal:     10 << 30,
			capacityAvailable: 7 << 30,
		},
	}
	return NewServer(backends, "", WithDrainStateDir(stateDir))
}

func TestDrain_BlocksNewMutatingRPCs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	srv := newDrainTestServer(t.TempDir())

	resp, err := srv.Drain(ctx, &agentv1.DrainRequest{})
	if err != nil {
		t.Fatalf("Drain unexpected error: %v", err)
	}
	if resp.GetWasAlreadyDrained() {
		t.Fatal("first Drain returned was_already_drained=true")
	}

	handlerCalled := false
	req := &agentv1.GetCapacityRequest{PoolName: drainTestPool}
	got, err := DrainGuardInterceptor(srv)(
		ctx,
		req,
		&grpc.UnaryServerInfo{FullMethod: agentv1.AgentService_GetCapacity_FullMethodName},
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			capacityReq, ok := req.(*agentv1.GetCapacityRequest)
			if !ok {
				return nil, status.Error(codes.Internal, "unexpected GetCapacity request type")
			}
			return srv.GetCapacity(ctx, capacityReq)
		},
	)
	if err == nil {
		t.Fatal("expected GetCapacity to be rejected after Drain")
	}
	if got != nil {
		t.Fatalf("got response %v, want nil", got)
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", st.Code())
	}
	if st.Message() != "agent draining" {
		t.Fatalf("message = %q, want %q", st.Message(), "agent draining")
	}
	if handlerCalled {
		t.Fatal("DrainGuardInterceptor called handler after Drain")
	}
}

func TestDrain_Idempotent(t *testing.T) {
	t.Parallel()

	srv := newDrainTestServer(t.TempDir())

	first, err := srv.Drain(context.Background(), &agentv1.DrainRequest{})
	if err != nil {
		t.Fatalf("first Drain unexpected error: %v", err)
	}
	if first.GetWasAlreadyDrained() {
		t.Fatal("first Drain returned was_already_drained=true")
	}

	second, err := srv.Drain(context.Background(), &agentv1.DrainRequest{})
	if err != nil {
		t.Fatalf("second Drain unexpected error: %v", err)
	}
	if !second.GetWasAlreadyDrained() {
		t.Fatal("second Drain returned was_already_drained=false")
	}
}

func TestDrain_StateFlushMarker(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	srv := newDrainTestServer(stateDir)

	_, err := srv.Drain(context.Background(), &agentv1.DrainRequest{})
	if err != nil {
		t.Fatalf("Drain unexpected error: %v", err)
	}

	markerPath := filepath.Join(stateDir, drainMarkerFilename)
	payload, err := os.ReadFile(markerPath) //nolint:gosec // G304: test reads marker under t.TempDir().
	if err != nil {
		t.Fatalf("read drain marker: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, string(payload)); err != nil {
		t.Fatalf("drain marker timestamp %q is not RFC3339: %v", string(payload), err)
	}
}

func TestDrain_WaitsForInFlight(t *testing.T) {
	stateDir := t.TempDir()
	srv := newDrainTestServer(stateDir)
	unlock := srv.lockTarget(agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP, "test-target")

	done := make(chan error, 1)
	go func() {
		_, err := srv.Drain(context.Background(), &agentv1.DrainRequest{})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Drain returned early with error: %v", err)
		}
		t.Fatal("Drain completed before in-flight target lock was released")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Drain returned error after lock release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Drain did not complete after in-flight target lock was released")
	}
}
