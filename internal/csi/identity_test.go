package csi

import (
	"context"
	"errors"
	"testing"

	csispec "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProbe_NotReady_WhenReadyFnReturnsFalse(t *testing.T) {
	server := NewIdentityServerWithReadyFn("test.pillar-csi", "test", func(_ context.Context) (bool, error) {
		return false, nil
	})

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if response.GetReady().GetValue() {
		t.Fatal("Probe ready = true, want false")
	}
}

func TestProbe_Ready_WhenReadyFnReturnsTrue(t *testing.T) {
	server := NewIdentityServerWithReadyFn("test.pillar-csi", "test", func(_ context.Context) (bool, error) {
		return true, nil
	})

	response, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}
	if !response.GetReady().GetValue() {
		t.Fatal("Probe ready = false, want true")
	}
}

func TestProbe_Error_WhenReadyFnFails(t *testing.T) {
	errSentinel := errors.New("sentinel readiness failure")
	server := NewIdentityServerWithReadyFn("test.pillar-csi", "test", func(_ context.Context) (bool, error) {
		return false, errSentinel
	})

	_, err := server.Probe(context.Background(), &csispec.ProbeRequest{})

	if err == nil {
		t.Fatal("Probe returned nil error, want readiness error")
	}
	if got, want := status.Code(err), codes.Internal; got != want {
		t.Fatalf("Probe error code = %s, want %s", got, want)
	}
}
