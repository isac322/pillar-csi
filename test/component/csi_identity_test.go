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

// Package component_test – CSI Identity Service component tests.
//
// This file covers the CSI Identity Service (internal/csi.IdentityServer) as a
// boundary-box unit.  All three Identity RPCs are tested:
//
//   - GetPluginInfo  (§6.1): driver name and version propagation
//   - GetPluginCapabilities (§6.2): capability advertisement
//   - Probe (§6.3, §6.4): readiness reporting and error propagation
//
// Error paths covered:
//
//   - Context already expired/canceled before handler starts (§6.4)
//   - readyFn returns a health-check error → codes.Internal (§6.4)
//   - readyFn propagates context.DeadlineExceeded → codes.DeadlineExceeded (§6.4)
//
// # Mock fidelity
//
// mockIdentityReadyFn is a preset test double for the readyFn parameter of
// NewIdentityServerWithReadyFn.
//
// Approximates: any production readiness check (e.g. verifying the ZFS kernel
// module is loaded, the nvmet configfs is mounted, or the agent is reachable).
//
// Omits / simplifies:
//   - No I/O; the outcome is determined by preset fields (ready bool, err error).
//   - Blocking variant uses ctx.Done() to simulate a slow check that honors
//     context cancellation; real checks may access sysfs, netlink, or gRPC.
//   - Does not validate kernel module state, configfs paths, or network routes.
//   - Call counter is not goroutine-safe; tests that need concurrent safety
//     must add external synchronization.
//
// See docs/testing/COMPONENT-TESTS.md for the authoritative test-case spec.
package component_test

import (
	"context"
	"errors"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// Mock: identityReadyFn
// ─────────────────────────────────────────────────────────────────────────────.

// identityMockReady is a configurable test double for the IdentityServer
// readyFn.  Field `ready` controls the boolean result; field `err` overrides
// the return when non-nil.  Field `calls` tracks how many times the function
// was invoked (not goroutine-safe).
//
// Mock fidelity: see file-level doc comment.
type identityMockReady struct {
	ready bool
	err   error
	calls int

	// blockUntilCtxDone, when true, causes the function to block until the
	// provided context is canceled/expired, then return context.Err().
	// This models a slow health check that is canceled mid-flight.
	blockUntilCtxDone bool
}

func (m *identityMockReady) fn(ctx context.Context) (bool, error) {
	m.calls++
	if m.blockUntilCtxDone {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if m.err != nil {
		return false, m.err
	}
	return m.ready, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────.

const (
	testDriverName    = "pillar-csi.bhyoo.com"
	testDriverVersion = "0.1.0"
)

// newIdentityServer returns a default IdentityServer (always ready).
func newIdentityServer(t *testing.T) *pillarcsi.IdentityServer {
	t.Helper()
	return pillarcsi.NewIdentityServer(testDriverName, testDriverVersion)
}

// newIdentityServerWithMock returns an IdentityServer backed by mock readyFn.
func newIdentityServerWithMock(t *testing.T, mock *identityMockReady) *pillarcsi.IdentityServer {
	t.Helper()
	return pillarcsi.NewIdentityServerWithReadyFn(testDriverName, testDriverVersion, mock.fn)
}

// ─────────────────────────────────────────────────────────────────────────────
// § 6.1 GetPluginInfo
// docs/testing/COMPONENT-TESTS.md.1 tests 1–2
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIIdentity_GetPluginInfo_Success verifies that GetPluginInfo returns the
// correct driver name and vendor version (test case 1).
//
// See docs/testing/COMPONENT-TESTS.md.1, row 1.
func TestCSIIdentity_GetPluginInfo_Success(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	resp, err := srv.GetPluginInfo(context.Background(), &csipb.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.GetName() != testDriverName {
		t.Errorf("Name = %q, want %q", resp.GetName(), testDriverName)
	}
	if resp.GetVendorVersion() != testDriverVersion {
		t.Errorf("VendorVersion = %q, want %q", resp.GetVendorVersion(), testDriverVersion)
	}
}

// TestCSIIdentity_GetPluginInfo_NameNotEmpty verifies that the driver name is
// always non-empty in the response (test case 2).
//
// See docs/testing/COMPONENT-TESTS.md.1, row 2.
func TestCSIIdentity_GetPluginInfo_NameNotEmpty(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	resp, err := srv.GetPluginInfo(context.Background(), &csipb.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.GetName() == "" {
		t.Error("driver Name is empty; CSI spec requires a non-empty name")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 6.2 GetPluginCapabilities
// docs/testing/COMPONENT-TESTS.md.2 tests 3–4
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIIdentity_GetPluginCapabilities_IncludesControllerService verifies
// that GetPluginCapabilities includes CONTROLLER_SERVICE (test case 3).
//
// See docs/testing/COMPONENT-TESTS.md.2, row 3.
func TestCSIIdentity_GetPluginCapabilities_IncludesControllerService(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	resp, err := srv.GetPluginCapabilities(context.Background(), &csipb.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}

	found := false
	for _, cap := range resp.GetCapabilities() {
		svc := cap.GetService()
		if svc != nil && svc.GetType() == csipb.PluginCapability_Service_CONTROLLER_SERVICE {
			found = true
			break
		}
	}
	if !found {
		t.Error("CONTROLLER_SERVICE not found in GetPluginCapabilities response")
	}
}

// TestCSIIdentity_GetPluginCapabilities_IncludesVolumeExpansion verifies
// that GetPluginCapabilities includes a VOLUME_EXPANSION capability (test case 4).
//
// See docs/testing/COMPONENT-TESTS.md.2, row 4.
func TestCSIIdentity_GetPluginCapabilities_IncludesVolumeExpansion(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	resp, err := srv.GetPluginCapabilities(context.Background(), &csipb.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}

	found := false
	for _, cap := range resp.GetCapabilities() {
		if cap.GetVolumeExpansion() != nil {
			found = true
			break
		}
	}
	if !found {
		t.Error("VOLUME_EXPANSION capability not found in GetPluginCapabilities response")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 6.3 Probe
// docs/testing/COMPONENT-TESTS.md.3 tests 5–7
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIIdentity_Probe_Ready verifies that Probe returns Ready=true when the
// readyFn returns (true, nil) (test case 5).
//
// See docs/testing/COMPONENT-TESTS.md.3, row 5.
func TestCSIIdentity_Probe_Ready(t *testing.T) {
	t.Parallel()
	mock := &identityMockReady{ready: true}
	srv := newIdentityServerWithMock(t, mock)

	resp, err := srv.Probe(context.Background(), &csipb.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if resp.GetReady() == nil {
		t.Fatal("Probe returned nil Ready field")
	}
	if !resp.GetReady().GetValue() {
		t.Error("Probe.Ready = false, want true")
	}
	if mock.calls != 1 {
		t.Errorf("readyFn calls = %d, want 1", mock.calls)
	}
}

// TestCSIIdentity_Probe_NotReady verifies that Probe returns Ready=false when
// the readyFn returns (false, nil) (test case 6).
//
// See docs/testing/COMPONENT-TESTS.md.3, row 6.
func TestCSIIdentity_Probe_NotReady(t *testing.T) {
	t.Parallel()
	mock := &identityMockReady{ready: false}
	srv := newIdentityServerWithMock(t, mock)

	resp, err := srv.Probe(context.Background(), &csipb.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe returned unexpected error: %v", err)
	}
	if resp.GetReady() == nil {
		t.Fatal("Probe returned nil Ready field")
	}
	if resp.GetReady().GetValue() {
		t.Error("Probe.Ready = true, want false")
	}
}

// TestCSIIdentity_Probe_DefaultAlwaysReady verifies that the default
// IdentityServer (created via NewIdentityServer, no readyFn) always returns
// Ready=true (test case 7).
//
// See docs/testing/COMPONENT-TESTS.md.3, row 7.
func TestCSIIdentity_Probe_DefaultAlwaysReady(t *testing.T) {
	t.Parallel()
	// Use the simple constructor — no readyFn supplied.
	srv := pillarcsi.NewIdentityServer(testDriverName, testDriverVersion)

	resp, err := srv.Probe(context.Background(), &csipb.ProbeRequest{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if resp.GetReady() == nil {
		t.Fatal("Probe returned nil Ready field")
	}
	if !resp.GetReady().GetValue() {
		t.Error("default Probe.Ready = false, want true (default should always be ready)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// § 6.4 Error Paths
// docs/testing/COMPONENT-TESTS.md.4 tests 8–12
// ─────────────────────────────────────────────────────────────────────────────.

// TestCSIIdentity_GetPluginInfo_ContextDeadlineExceeded verifies that
// GetPluginInfo returns codes.DeadlineExceeded when the context has already
// expired before the handler runs (test case 8).
//
// This exercises the context-error short-circuit path: the handler checks
// ctx.Err() before doing any work and converts the context error into the
// corresponding gRPC status.
//
// See docs/testing/COMPONENT-TESTS.md.4, row 8.
func TestCSIIdentity_GetPluginInfo_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	// Build a context that is already expired.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure the deadline has passed

	_, err := srv.GetPluginInfo(ctx, &csipb.GetPluginInfoRequest{})
	if err == nil {
		t.Fatal("expected DeadlineExceeded error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("error code = %v, want DeadlineExceeded", st.Code())
	}
	t.Logf("GetPluginInfo with expired context: %v", err)
}

// TestCSIIdentity_GetPluginCapabilities_ContextCancelled verifies that
// GetPluginCapabilities returns codes.Canceled when the context is already
// canceled before the handler runs (test case 9).
//
// See docs/testing/COMPONENT-TESTS.md.4, row 9.
func TestCSIIdentity_GetPluginCapabilities_ContextCancelled(t *testing.T) {
	t.Parallel()
	srv := newIdentityServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := srv.GetPluginCapabilities(ctx, &csipb.GetPluginCapabilitiesRequest{})
	if err == nil {
		t.Fatal("expected Canceled error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Canceled {
		t.Errorf("error code = %v, want Canceled", st.Code())
	}
	t.Logf("GetPluginCapabilities with canceled context: %v", err)
}

// TestCSIIdentity_Probe_ContextDeadlineExceeded verifies that Probe returns
// codes.DeadlineExceeded before invoking readyFn when the context has already
// expired (test case 10).
//
// This validates that the context check happens BEFORE the potentially
// expensive readyFn call — a performance and correctness requirement.
//
// See docs/testing/COMPONENT-TESTS.md.4, row 10.
func TestCSIIdentity_Probe_ContextDeadlineExceeded(t *testing.T) {
	t.Parallel()

	mock := &identityMockReady{ready: true} // would succeed if called
	srv := newIdentityServerWithMock(t, mock)

	// Context already expired.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	_, err := srv.Probe(ctx, &csipb.ProbeRequest{})
	if err == nil {
		t.Fatal("expected DeadlineExceeded error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("error code = %v, want DeadlineExceeded", st.Code())
	}
	// readyFn must NOT have been called — context check is a short-circuit.
	if mock.calls != 0 {
		t.Errorf("readyFn was called %d times, want 0 (context check must short-circuit before readyFn)", mock.calls)
	}
	t.Logf("Probe with expired context: %v", err)
}

// TestCSIIdentity_Probe_ReadyFnError_ReturnsInternal verifies that Probe
// returns codes.Internal when readyFn returns a non-context error (test
// case 11).
//
// In production this models a health check that fails with a concrete error
// (e.g., "disk quota exceeded", "nvmet module not loaded") that the CO cannot
// retry away.  The CO should treat Internal as a driver problem rather than a
// transient "not ready" condition.
//
// See docs/testing/COMPONENT-TESTS.md.4, row 11.
func TestCSIIdentity_Probe_ReadyFnError_ReturnsInternal(t *testing.T) {
	t.Parallel()

	const healthMsg = "health check failed: disk quota exceeded"
	mock := &identityMockReady{err: errors.New(healthMsg)}
	srv := newIdentityServerWithMock(t, mock)

	_, err := srv.Probe(context.Background(), &csipb.ProbeRequest{})
	if err == nil {
		t.Fatal("expected Internal error from readyFn failure, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal {
		t.Errorf("error code = %v, want Internal", st.Code())
	}
	// The error message should carry the health-check detail.
	if msg := st.Message(); msg == "" {
		t.Error("gRPC error message is empty; health-check detail should be included")
	}
	t.Logf("readyFn error propagated as Internal: %v", err)
}

// TestCSIIdentity_Probe_ReadyFnContextError_PropagatesCode verifies that Probe
// propagates context.DeadlineExceeded from readyFn as codes.DeadlineExceeded
// (not as codes.Internal) (test case 12).
//
// This models a slow health check that is canceled mid-flight.  The Probe
// handler must distinguish context errors (which have well-known gRPC codes)
// from generic health-check errors.
//
// Setup:
//   - readyFn blocks until context is Done, then returns context.Err().
//   - Context deadline: 150 ms.
//
// Expected:
//   - Probe returns codes.DeadlineExceeded within ~500 ms.
//   - readyFn was called once.
//
// See docs/testing/COMPONENT-TESTS.md.4, row 12.
func TestCSIIdentity_Probe_ReadyFnContextError_PropagatesCode(t *testing.T) {
	t.Parallel()

	// blockUntilCtxDone=true makes fn() block until ctx expires, then return ctx.Err().
	mock := &identityMockReady{blockUntilCtxDone: true}
	srv := newIdentityServerWithMock(t, mock)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := srv.Probe(ctx, &csipb.ProbeRequest{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DeadlineExceeded error, got nil")
	}

	const maxElapsed = 500 * time.Millisecond
	if elapsed > maxElapsed {
		t.Errorf("Probe took %v, want < %v (should respect context deadline)", elapsed, maxElapsed)
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.DeadlineExceeded {
		t.Errorf("error code = %v, want DeadlineExceeded (context error must not map to Internal)", st.Code())
	}
	// readyFn was invoked (it blocked until the context fired).
	if mock.calls != 1 {
		t.Errorf("readyFn calls = %d, want 1", mock.calls)
	}
	t.Logf("Probe context error propagated as DeadlineExceeded in %v: %v", elapsed, err)
}

// ─────────────────────────────────────────────────────────────────────────────
// Compile-time interface check
// ─────────────────────────────────────────────────────────────────────────────.

// Verify that IdentityServer implements csi.IdentityServer at compile time.
var _ csipb.IdentityServer = (*pillarcsi.IdentityServer)(nil)
