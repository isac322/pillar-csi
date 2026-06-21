package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	drainMarkerFilename     = ".drained"
	drainFallbackStateDir   = "pillar-csi-agent"
	drainMethodFullName     = agentv1.AgentService_Drain_FullMethodName
	drainMarkerDirPerm      = 0o755
	drainMarkerFilePerm     = 0o644
	drainTimestampPrecision = time.RFC3339
)

// Drain stops new RPCs, waits for in-flight intercepted work, and writes the clean-shutdown marker.
//
// Ordering is load-bearing:
//  1. drained.Swap(true) — DrainGuardInterceptor's drained.Load() now rejects
//     every new RPC before it can RLock drainGate.
//  2. drainGate.Lock() — waits for every already-accepted RPC to finish its
//     handler and release the RLock; once we hold WLock there is no in-flight
//     work that could create new target locks or mutate configfs.
//  3. waitForTargetLocks() — safety belt for any code path that acquires a
//     per-target mutex outside the gRPC interceptor (none today, but the
//     iteration cost is negligible and the guarantee is valuable).
//  4. writeDrainMarker() — records clean shutdown.
func (s *Server) Drain(_ context.Context, _ *agentv1.DrainRequest) (*agentv1.DrainResponse, error) {
	alreadyDrained := s.drained.Swap(true)
	if alreadyDrained {
		return &agentv1.DrainResponse{WasAlreadyDrained: true}, nil
	}

	s.drainGate.Lock()
	defer s.drainGate.Unlock()
	s.waitForTargetLocks()

	err := s.writeDrainMarker()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Drain: write marker: %v", err)
	}

	return &agentv1.DrainResponse{WasAlreadyDrained: false}, nil
}

func (s *Server) waitForTargetLocks() {
	s.targetMu.Range(func(_, value any) bool {
		mu, ok := value.(*sync.Mutex)
		if !ok {
			return true
		}
		mu.Lock()
		mu.Unlock() //nolint:gocritic,staticcheck // Drain intentionally waits for the current lock holder.
		return true
	})
}

func (s *Server) writeDrainMarker() error {
	stateDir := s.resolvedDrainStateDir()
	mkdirErr := os.MkdirAll(stateDir, drainMarkerDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("mkdir drain state dir %q: %w", stateDir, mkdirErr)
	}

	markerPath := filepath.Join(stateDir, drainMarkerFilename)
	payload := []byte(time.Now().UTC().Format(drainTimestampPrecision))
	writeErr := os.WriteFile(markerPath, payload, drainMarkerFilePerm)
	if writeErr != nil {
		return fmt.Errorf("write drain marker %q: %w", markerPath, writeErr)
	}
	return nil
}

func (s *Server) resolvedDrainStateDir() string {
	if s.drainStateDir != "" {
		return s.drainStateDir
	}
	return filepath.Join(os.TempDir(), drainFallbackStateDir)
}

// DrainGuardInterceptor rejects non-Drain unary RPCs after the server is
// drained AND holds an RLock on Server.drainGate for the entire duration of
// every accepted handler so Drain can wait deterministically for in-flight
// work to finish.  The double drained.Load() is required: an RPC can pass
// the first check, then Drain runs Swap(true) before the goroutine reaches
// RLock; the second check inside the RLock window catches that case so the
// handler never executes after Drain claimed ownership.
func DrainGuardInterceptor(s *Server) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == drainMethodFullName {
			return handler(ctx, req)
		}
		if s.drained.Load() {
			return nil, status.Error(codes.Unavailable, "agent draining")
		}
		s.drainGate.RLock()
		defer s.drainGate.RUnlock()
		if s.drained.Load() {
			return nil, status.Error(codes.Unavailable, "agent draining")
		}
		return handler(ctx, req)
	}
}
