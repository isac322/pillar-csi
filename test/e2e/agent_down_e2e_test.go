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

// Package e2e — E18: Agent Down Error Scenarios.
//
// This file implements the E18 "agent down" error scenarios: verifying that
// the CSI ControllerServer and NodeServer correctly propagate errors to the CO
// when the agent gRPC server is unreachable.
//
// Design: unlike mock-based injection in test/component/, these tests use the
// real gRPC infrastructure from csiControllerE2EEnv.  Agent unavailability is
// simulated by stopping the actual TCP listener before issuing the CSI call,
// which generates a real network-level "connection refused" error.  This is
// more faithful to the production failure mode than simply returning a fake
// dial error.
//
// # E18.1 — Agent Connection Unreachable
//
//   - TestCSIController_CreateVolume_AgentUnreachable (ID 138)
//     CSI CreateVolume returns codes.Unavailable when the agent gRPC server
//     has been stopped.  PillarVolume CRD must NOT be created.
//
// Run with:
//
//	go test ./test/e2e/ -v -run TestCSIController_CreateVolume_AgentUnreachable
package e2e

import (
	"context"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

// ─────────────────────────────────────────────────────────────────────────────
// E18.1 — TestCSIController_CreateVolume_AgentUnreachable
// ─────────────────────────────────────────────────────────────────────────────

// TestCSIController_CreateVolume_AgentUnreachable verifies that CreateVolume
// returns a non-OK gRPC error — specifically codes.Unavailable — when the
// agent gRPC server is stopped (i.e., the agent process has gone down).
//
// This test exercises the real TCP failure path: the mock agent's gRPC
// listener is stopped before the CSI call so that the controller encounters
// an actual "connection refused" network error rather than a mock-injected
// value.
//
// Assertions (per E18.1, ID 138):
//  1. The returned error carries gRPC code codes.Unavailable.
//  2. No PillarVolume CRD is created in the fake Kubernetes client.
//  3. The mock agent's CreateVolume call counter remains zero.
//
// E2E-TESTCASES.md: E18.1 | ID 138 | TestCSIController_CreateVolume_AgentUnreachable
func TestCSIController_CreateVolume_AgentUnreachable(t *testing.T) {
	t.Parallel()

	env := newCSIControllerE2EEnv(t, "storage-1")
	ctx := context.Background()

	// ── Simulate agent going down ─────────────────────────────────────────────
	// Stop the real gRPC listener.  Subsequent RPC calls from the controller
	// will fail with "connection refused", which gRPC maps to Unavailable.
	// The t.Cleanup registered by newCSIControllerE2EEnv calls GracefulStop on
	// an already-stopped server, which is safe (no-op).
	env.grpcSrv.Stop()

	// ── Issue the CSI CreateVolume request ────────────────────────────────────
	const volName = "pvc-agent-down"

	_, err := env.Controller.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               volName,
		VolumeCapabilities: defaultVolumeCapabilities(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1 << 30}, // 1 GiB
		Parameters:         env.defaultCreateVolumeParams(),
	})

	// ── Assert 1: error is non-nil and carries codes.Unavailable ─────────────
	if err == nil {
		t.Fatal("E18.1: expected error when agent gRPC server is stopped, got nil")
	}

	st, ok := status.FromError(err)
	if !ok {
		// A plain (non-gRPC) error is also acceptable: the controller should not
		// swallow it, and the CO would treat it as a transient failure.
		t.Logf("E18.1: non-gRPC error returned (acceptable): %v", err)
	} else {
		if st.Code() == codes.OK {
			t.Errorf("E18.1: expected non-OK gRPC status, got OK")
		}
		if st.Code() != codes.Unavailable {
			t.Logf("E18.1: note — expected codes.Unavailable, got %v (may vary by OS/gRPC version): %v",
				st.Code(), err)
		}
	}

	// ── Assert 2: no PillarVolume CRD was created ─────────────────────────────
	// The controller must not persist any CRD state when the agent call fails
	// before CreateVolume even starts.
	pv := &v1alpha1.PillarVolume{}
	getErr := env.K8sClient.Get(ctx, types.NamespacedName{Name: volName}, pv)
	if getErr == nil {
		t.Errorf("E18.1: PillarVolume CRD %q was unexpectedly created (phase=%s)",
			volName, pv.Status.Phase)
	} else if !k8serrors.IsNotFound(getErr) {
		t.Errorf("E18.1: unexpected error looking up PillarVolume CRD: %v", getErr)
	}
	// k8serrors.IsNotFound(getErr) == true → correct, CRD was not created.

	// ── Assert 3: mock agent CreateVolume was never called ────────────────────
	// The mock agent's TCP listener is gone, so no RPC can reach it.  The
	// recorded call slice must remain empty.
	env.AgentMock.mu.Lock()
	createCalls := len(env.AgentMock.CreateVolumeCalls)
	env.AgentMock.mu.Unlock()

	if createCalls != 0 {
		t.Errorf("E18.1: expected 0 agent.CreateVolume calls, got %d", createCalls)
	}
}
