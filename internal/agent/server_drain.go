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

// Drain stops new RPCs, waits for in-flight per-target work, and writes the clean-shutdown marker.
func (s *Server) Drain(_ context.Context, _ *agentv1.DrainRequest) (*agentv1.DrainResponse, error) {
	alreadyDrained := s.drained.Swap(true)
	if alreadyDrained {
		return &agentv1.DrainResponse{WasAlreadyDrained: true}, nil
	}

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

// DrainGuardInterceptor rejects non-Drain unary RPCs after the server is drained.
func DrainGuardInterceptor(s *Server) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if s.drained.Load() && info.FullMethod != drainMethodFullName {
			return nil, status.Error(codes.Unavailable, "agent draining")
		}
		return handler(ctx, req)
	}
}
