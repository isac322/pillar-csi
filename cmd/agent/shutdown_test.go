package main

import (
	"context"
	"testing"
	"time"

	healthsrv "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestRunAgentShutdown_DrainsAfterHealthNotServing(t *testing.T) {
	t.Parallel()

	const grace = 50 * time.Millisecond
	healthSrv := healthsrv.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	events := make(chan string, 3)

	var drainCalls int
	drainFn := func(ctx context.Context) error {
		drainCalls++
		response, err := healthSrv.Check(ctx, &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("health Check during Drain: %v", err)
		}
		if got, want := response.GetStatus(), healthpb.HealthCheckResponse_NOT_SERVING; got != want {
			t.Fatalf("health status during Drain = %s, want %s", got, want)
		}
		events <- "health-not-serving-before-drain"
		events <- "drain"
		return nil
	}

	var gracefulStopCalls int
	var gracefulStopAt time.Duration
	start := time.Now()
	gracefulStopFn := func() {
		gracefulStopCalls++
		gracefulStopAt = time.Since(start)
		events <- "graceful-stop"
	}

	runAgentShutdown(healthSrv, drainFn, gracefulStopFn, grace)
	elapsed := time.Since(start)
	close(events)

	gotEvents := make([]string, 0, 3)
	for event := range events {
		gotEvents = append(gotEvents, event)
	}
	wantEvents := []string{"health-not-serving-before-drain", "drain", "graceful-stop"}
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("shutdown events = %v, want %v", gotEvents, wantEvents)
	}
	for i, want := range wantEvents {
		if gotEvents[i] != want {
			t.Fatalf("shutdown events = %v, want %v", gotEvents, wantEvents)
		}
	}
	if drainCalls != 1 {
		t.Fatalf("Drain calls = %d, want 1", drainCalls)
	}
	if gracefulStopCalls != 1 {
		t.Fatalf("GracefulStop calls = %d, want 1", gracefulStopCalls)
	}
	if gracefulStopAt < grace {
		t.Fatalf("GracefulStop called after %s, want at least %s", gracefulStopAt, grace)
	}
	if elapsed < grace {
		t.Fatalf("shutdown elapsed = %s, want at least %s", elapsed, grace)
	}
	if elapsed > grace+500*time.Millisecond {
		t.Fatalf("shutdown elapsed = %s, want roughly %s", elapsed, grace)
	}
}
