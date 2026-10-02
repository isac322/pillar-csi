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

package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/agent"
	"github.com/isac322/pillar-csi/internal/agent/nvmeof"
)

// localAttachEnv carries an export test server and its claimer state: the
// claimer refuses with ErrDeviceHeld while held is true and records every
// claimed device path in claimed.
type localAttachEnv struct {
	srv     *agent.Server
	cfgRoot string
	held    *atomic.Bool
	claimed *[]string
}

func localAttachServer(t *testing.T) localAttachEnv {
	t.Helper()
	env := localAttachEnv{}
	env.srv, env.cfgRoot = newExportTestServer(t, &mockBackend{expandAllocated: 2 << 30})
	env.held = &atomic.Bool{}
	env.claimed = &[]string{}
	agent.SetDeviceClaimer(t, env.srv, func(path string) (func() error, error) {
		*env.claimed = append(*env.claimed, path)
		if env.held.Load() {
			return nil, nvmeof.ErrDeviceHeld
		}
		return func() error { return nil }, nil
	})
	return env
}

func namespaceEnable(t *testing.T, cfgRoot string) string {
	t.Helper()
	path := filepath.Join(cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1", "enable")
	//nolint:gosec // G304: test reads a file under t.TempDir().
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read namespace enable: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

func exportForLocalAttach(t *testing.T, srv *agent.Server) {
	t.Helper()
	_, err := srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		DevicePath:   testDevicePath,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if err != nil {
		t.Fatalf("ExportVolume: %v", err)
	}
}

func setLocalAttach(
	srv *agent.Server, local bool, fence *agentv1.FencingToken,
) (*agentv1.SetLocalAttachResponse, error) {
	return srv.SetLocalAttach(context.Background(), &agentv1.SetLocalAttachRequest{
		VolumeId:     testVolumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		Local:        local,
		Fence:        fence,
	})
}

func reconcileLocal(t *testing.T, srv *agent.Server, local bool) *agentv1.ReconcileItemResult {
	t.Helper()
	export := nvmeofExportState("10.0.0.1")
	export.LocalAttach = local
	resp, err := srv.ReconcileState(context.Background(), &agentv1.ReconcileStateRequest{
		Volumes: []*agentv1.VolumeDesiredState{{
			VolumeId:    testVolumeID,
			BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
			DevicePath:  testDevicePath,
			Exports:     []*agentv1.ExportDesiredState{export},
			Fence:       testFence(t),
		}},
	})
	if err != nil {
		t.Fatalf("ReconcileState: %v", err)
	}
	if len(resp.GetResults()) != 1 {
		t.Fatalf("ReconcileState results = %v, want 1", resp.GetResults())
	}
	return resp.GetResults()[0]
}

// TestSetLocalAttach_DisableThenEnable: a local attach disables the namespace
// and returns the backend device; switching back re-enables it after the
// holder check.  Both directions are idempotent.
func TestSetLocalAttach_DisableThenEnable(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	*env.claimed = nil

	for range 2 {
		resp, err := setLocalAttach(env.srv, true, testFence(t))
		if err != nil {
			t.Fatalf("SetLocalAttach(true): %v", err)
		}
		if resp.GetDevicePath() != testDevicePath {
			t.Errorf("DevicePath = %q, want %q", resp.GetDevicePath(), testDevicePath)
		}
		if got := namespaceEnable(t, env.cfgRoot); got != "0" {
			t.Fatalf("enable after local attach = %q, want 0", got)
		}
	}

	for range 2 {
		_, err := setLocalAttach(env.srv, false, testFence(t))
		if err != nil {
			t.Fatalf("SetLocalAttach(false): %v", err)
		}
	}
	// Only the actual disabled→enabled transition claims the device, once,
	// held across the enable write.
	if len(*env.claimed) != 1 || (*env.claimed)[0] != testDevicePath {
		t.Errorf("claimed devices = %v, want [%s]", *env.claimed, testDevicePath)
	}
}

// TestSetLocalAttach_EnableRefusedWhileDeviceHeld: the export is not
// re-enabled while the storage node still holds the backend device.
func TestSetLocalAttach_EnableRefusedWhileDeviceHeld(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	if _, err := setLocalAttach(env.srv, true, testFence(t)); err != nil {
		t.Fatalf("SetLocalAttach(true): %v", err)
	}
	env.held.Store(true)

	_, err := setLocalAttach(env.srv, false, testFence(t))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SetLocalAttach(false) while held: code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "still held on the storage node") {
		t.Errorf("error = %q, want holder detail", err)
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "0" {
		t.Fatalf("enable after refused switch = %q, want 0", got)
	}

	env.held.Store(false)
	if _, err := setLocalAttach(env.srv, false, testFence(t)); err != nil {
		t.Fatalf("SetLocalAttach(false) after release: %v", err)
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "1" {
		t.Fatalf("enable after release = %q, want 1", got)
	}
}

// TestSetLocalAttach_ClaimHeldAcrossEnable: the agent keeps its exclusive
// claim on the backend device for the whole enable write, so the storage
// node cannot claim the device under a still-disabled namespace.
func TestSetLocalAttach_ClaimHeldAcrossEnable(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	if _, err := setLocalAttach(env.srv, true, testFence(t)); err != nil {
		t.Fatalf("SetLocalAttach(true): %v", err)
	}
	agent.SetDeviceClaimer(t, env.srv, func(string) (func() error, error) {
		return func() error {
			if got := namespaceEnable(t, env.cfgRoot); got != "1" {
				t.Errorf("enable at claim release = %q, want 1 (claim held across the write)", got)
			}
			return nil
		}, nil
	})

	if _, err := setLocalAttach(env.srv, false, testFence(t)); err != nil {
		t.Fatalf("SetLocalAttach(false): %v", err)
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "1" {
		t.Fatalf("enable after switch = %q, want 1", got)
	}
}

func TestSetLocalAttach_MissingExportNotFound(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)

	for _, local := range []bool{true, false} {
		_, err := setLocalAttach(env.srv, local, testFence(t))
		if status.Code(err) != codes.NotFound {
			t.Errorf("SetLocalAttach(%t) without export: code = %v (%v), want NotFound", local, status.Code(err), err)
		}
	}
}

// TestSetLocalAttach_StaleFenceRejected: a request superseded by a newer
// operation neither disables nor re-enables the namespace.
func TestSetLocalAttach_StaleFenceRejected(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	newer := &agentv1.FencingToken{VolumeUid: t.Name(), Generation: 2}
	if _, err := setLocalAttach(env.srv, true, newer); err != nil {
		t.Fatalf("SetLocalAttach(true) gen 2: %v", err)
	}

	_, err := setLocalAttach(env.srv, false, testFence(t))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "stale fencing token") {
		t.Fatalf("stale SetLocalAttach(false): %v, want stale fencing FailedPrecondition", err)
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "0" {
		t.Fatalf("enable after stale request = %q, want 0", got)
	}
}

// TestReconcileState_LocalAttachKeepsNamespaceDisabled: a restore or resync
// with local_attach disables an enabled namespace and never enables it, so a
// reboot cannot reopen the export under a local attach.
func TestReconcileState_LocalAttachKeepsNamespaceDisabled(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	*env.claimed = nil

	for range 2 {
		result := reconcileLocal(t, env.srv, true)
		if !result.GetSuccess() {
			t.Fatalf("reconcile local: %s", result.GetErrorMessage())
		}
		if got := namespaceEnable(t, env.cfgRoot); got != "0" {
			t.Fatalf("enable after local reconcile = %q, want 0", got)
		}
	}
	if len(*env.claimed) != 0 {
		t.Errorf("claimed devices = %v, want none for a local reconcile", *env.claimed)
	}
}

// TestReconcileState_RefusesToEnableHeldDevice: a resync that no longer
// carries local_attach reports the volume as failed and leaves the namespace
// disabled while the device is held; once released, the next resync enables.
func TestReconcileState_RefusesToEnableHeldDevice(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	if result := reconcileLocal(t, env.srv, true); !result.GetSuccess() {
		t.Fatalf("reconcile local: %s", result.GetErrorMessage())
	}
	env.held.Store(true)

	result := reconcileLocal(t, env.srv, false)
	if result.GetSuccess() || !strings.Contains(result.GetErrorMessage(), "still held on the storage node") {
		t.Fatalf("reconcile while held: success=%t message=%q, want holder failure",
			result.GetSuccess(), result.GetErrorMessage())
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "0" {
		t.Fatalf("enable after refused reconcile = %q, want 0", got)
	}

	env.held.Store(false)
	if result := reconcileLocal(t, env.srv, false); !result.GetSuccess() {
		t.Fatalf("reconcile after release: %s", result.GetErrorMessage())
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "1" {
		t.Fatalf("enable after release = %q, want 1", got)
	}
}

// TestExportVolume_RefusedWhileDeviceHeld: re-exporting a locally attached
// volume must not re-enable its namespace under the local writer.
func TestExportVolume_RefusedWhileDeviceHeld(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	if result := reconcileLocal(t, env.srv, true); !result.GetSuccess() {
		t.Fatalf("reconcile local: %s", result.GetErrorMessage())
	}
	env.held.Store(true)

	_, err := env.srv.ExportVolume(context.Background(), &agentv1.ExportVolumeRequest{
		VolumeId:     testVolumeID,
		DevicePath:   testDevicePath,
		Fence:        testFence(t),
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofExportParams("10.0.0.1", 4420),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ExportVolume while held: code = %v (%v), want FailedPrecondition", status.Code(err), err)
	}
	if got := namespaceEnable(t, env.cfgRoot); got != "0" {
		t.Fatalf("enable after refused export = %q, want 0", got)
	}
}

// TestExpandVolume_SkipsRevalidateWhileDisabled: the backend still grows, but
// revalidate_size (rejected by the kernel on a disabled namespace) is not
// written.
func TestExpandVolume_SkipsRevalidateWhileDisabled(t *testing.T) {
	t.Parallel()
	env := localAttachServer(t)
	exportForLocalAttach(t, env.srv)
	if _, err := setLocalAttach(env.srv, true, testFence(t)); err != nil {
		t.Fatalf("SetLocalAttach(true): %v", err)
	}
	// A directory makes any revalidate_size write fail.
	nsDir := filepath.Join(env.cfgRoot, "nvmet", "subsystems", testVolumeNQN, "namespaces", "1")
	if err := os.Mkdir(filepath.Join(nsDir, "revalidate_size"), 0o750); err != nil {
		t.Fatalf("create unwritable revalidate_size: %v", err)
	}

	resp, err := env.srv.ExpandVolume(context.Background(), &agentv1.ExpandVolumeRequest{
		VolumeId:       testVolumeID,
		BackendType:    agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		Fence:          testFence(t),
		RequestedBytes: 2 << 30,
	})
	if err != nil {
		t.Fatalf("ExpandVolume on disabled namespace: %v", err)
	}
	if resp.GetCapacityBytes() != 2<<30 {
		t.Errorf("CapacityBytes = %d, want %d", resp.GetCapacityBytes(), 2<<30)
	}
}
