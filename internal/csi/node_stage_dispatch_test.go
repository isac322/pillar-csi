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

package csi

// Tests for NodeStageVolume protocol-handler dispatch using direct
// ProtocolHandler injection (AC 13d).
//
// Unlike the tests in node_stage_test.go (which use the legacy Connector
// adapter via newNodeTestEnv), these tests use newHandlerNodeTestEnv to inject
// fakeProtocolHandler instances directly into the NodeServer handlers map.
// This verifies the dispatch layer independently of the Connector adapter.
//
// Coverage:
//   - VolumeContextKeyProtocolType selects the correct handler.
//   - volumeID path component selects the correct handler (no context key).
//   - Default "nvmeof-tcp" fallback used when neither source provides a type.
//   - FailedPrecondition returned when no handler is registered.
//   - VolumeContextKeyProtocolType takes precedence over volumeID component.
//   - Attach error propagates as Internal.
//   - AttachParams fields (ConnectionID, Address, Port, ProtocolType) forwarded.
//   - Discriminated union nodeStageState written after successful Attach.
//   - Idempotency: Attach not called when volume already staged and mounted.
//   - Nil handlers map returns FailedPrecondition (no panic).
//
// Run with:
//
//	go test ./internal/csi/ -v -run TestNodeStageVolume_Dispatch

import (
	"context"
	"errors"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_VolumeContextKey
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_VolumeContextKey verifies that
// VolumeContextKeyProtocolType selects the correct ProtocolHandler and is
// forwarded as AttachParams.ProtocolType.
func TestNodeStageVolume_Dispatch_VolumeContextKey(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:ctx-key-test"
		address = "10.10.0.1"
		port    = "4420"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: nqn,
				Address:   address,
				Port:      port,
			},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-ctx-key-test",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:     nqn,
			VolumeContextKeyAddress:      address,
			VolumeContextKeyPort:         port,
			VolumeContextKeyProtocolType: ProtocolNVMeoFTCP,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// Handler.Attach must have been called exactly once with the correct protocol.
	if len(handler.attachCalls) != 1 {
		t.Fatalf("Attach called %d times, want 1", len(handler.attachCalls))
	}
	if got := handler.attachCalls[0].ProtocolType; got != ProtocolNVMeoFTCP {
		t.Errorf("AttachParams.ProtocolType = %q, want %q", got, ProtocolNVMeoFTCP)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_VolumeIDPathComponent
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_VolumeIDPathComponent verifies that when
// VolumeContextKeyProtocolType is absent the protocol type is extracted from
// the second slash-delimited component of the volumeID.
func TestNodeStageVolume_Dispatch_VolumeIDPathComponent(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:vid-path-test"
		address = "10.10.0.2"
		port    = "4420"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: nqn,
				Address:   address,
				Port:      port,
			},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	// volumeID contains "nvmeof-tcp" as the second path component; no
	// VolumeContextKeyProtocolType is set so the path-component fallback fires.
	const volumeID = "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-vid-path-test"

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: nqn,
			VolumeContextKeyAddress:  address,
			VolumeContextKeyPort:     port,
			// No VolumeContextKeyProtocolType — derived from volumeID component.
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if len(handler.attachCalls) != 1 {
		t.Fatalf("Attach called %d times, want 1", len(handler.attachCalls))
	}
	if got := handler.attachCalls[0].ProtocolType; got != ProtocolNVMeoFTCP {
		t.Errorf("AttachParams.ProtocolType = %q, want %q", got, ProtocolNVMeoFTCP)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_DefaultFallbackHandlerCalled
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_DefaultFallbackHandlerCalled verifies that when
// neither VolumeContext nor the volumeID path contain a recognized protocol type,
// the "nvmeof-tcp" handler is invoked as the backward-compatible default.
func TestNodeStageVolume_Dispatch_DefaultFallbackHandlerCalled(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:default-fallback"
		address = "10.10.0.3"
		port    = "4420"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: nqn,
				Address:   address,
				Port:      port,
			},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	// volumeID with no known protocol component; no context key either.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-legacy-vol",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: nqn,
			VolumeContextKeyAddress:  address,
			VolumeContextKeyPort:     port,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (default fallback): %v", err)
	}

	if len(handler.attachCalls) != 1 {
		t.Fatalf("Attach called %d times, want 1", len(handler.attachCalls))
	}
	// The resolved protocol must be the backward-compatible default.
	if got := handler.attachCalls[0].ProtocolType; got != ProtocolNVMeoFTCP {
		t.Errorf("AttachParams.ProtocolType = %q, want %q", got, ProtocolNVMeoFTCP)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_NoHandlerRegistered
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_NoHandlerRegistered verifies that
// NodeStageVolume returns codes.FailedPrecondition when the resolved protocol
// type has no registered ProtocolHandler in the handlers map.
func TestNodeStageVolume_Dispatch_NoHandlerRegistered(t *testing.T) {
	t.Parallel()

	// Only NVMe-oF is registered; the volume declares "iscsi".
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: &fakeProtocolHandler{},
	})
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-iscsi-no-handler",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:     "iqn.2024-01.com.example:vol",
			VolumeContextKeyAddress:      "10.0.0.1",
			VolumeContextKeyPort:         "3260",
			VolumeContextKeyProtocolType: "iscsi",
		},
	})
	if err == nil {
		t.Fatal("expected FailedPrecondition for unregistered protocol, got nil")
	}
	requireGRPCCode(t, err, codes.FailedPrecondition)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_VolumeContextKeyPrecedence
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_VolumeContextKeyPrecedence verifies end-to-end
// that VolumeContextKeyProtocolType takes precedence over the volumeID path
// component: the iSCSI handler is invoked even though the volumeID encodes
// "nvmeof-tcp".
func TestNodeStageVolume_Dispatch_VolumeContextKeyPrecedence(t *testing.T) {
	t.Parallel()

	nvmeHandler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State:      &NVMeoFProtocolState{SubsysNQN: "nqn.test:v", Address: "10.0.0.1", Port: "4420"},
		},
	}
	iscsiHandler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/sda",
			State:      &fakeProtocolState{protocol: "iscsi"},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: nvmeHandler,
		"iscsi":           iscsiHandler,
	})
	stagingPath := t.TempDir()

	// volumeID has "nvmeof-tcp" as path component, but VolumeContext says "iscsi".
	// VolumeContext must win.
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "storage-node-1/nvmeof-tcp/zfs-zvol/tank/pvc-precedence",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:     "iqn.test:vol",
			VolumeContextKeyAddress:      "10.0.0.1",
			VolumeContextKeyPort:         "3260",
			VolumeContextKeyProtocolType: "iscsi", // overrides "nvmeof-tcp" from volumeID
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	// iSCSI handler must be called; NVMe-oF handler must not be called.
	if len(iscsiHandler.attachCalls) != 1 {
		t.Errorf("iscsiHandler.Attach called %d times, want 1", len(iscsiHandler.attachCalls))
	}
	if len(nvmeHandler.attachCalls) != 0 {
		t.Errorf("nvmeHandler.Attach called %d times, want 0 (iscsi should win)", len(nvmeHandler.attachCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_AttachError
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_AttachError verifies that a ProtocolHandler.Attach
// failure causes NodeStageVolume to return codes.Internal.
func TestNodeStageVolume_Dispatch_AttachError(t *testing.T) {
	t.Parallel()

	sentinelErr := errors.New("transport connection refused")
	handler := &fakeProtocolHandler{
		attachErr: sentinelErr,
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-attach-fail",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: "nqn.test:vol",
			VolumeContextKeyAddress:  "10.0.0.1",
			VolumeContextKeyPort:     "4420",
		},
	})
	if err == nil {
		t.Fatal("expected error from Attach failure, got nil")
	}
	requireGRPCCode(t, err, codes.Internal)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_AttachParamsForwarded
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_AttachParamsForwarded verifies that
// NodeStageVolume correctly populates AttachParams from VolumeContext fields
// (ConnectionID = target_id, Address = address, Port = port) and passes them
// to handler.Attach.
func TestNodeStageVolume_Dispatch_AttachParamsForwarded(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:params-fwd"
		address = "192.168.10.20"
		port    = "4421"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme1n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: nqn,
				Address:   address,
				Port:      port,
			},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-params-fwd",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: nqn,
			VolumeContextKeyAddress:  address,
			VolumeContextKeyPort:     port,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	if len(handler.attachCalls) != 1 {
		t.Fatalf("Attach called %d times, want 1", len(handler.attachCalls))
	}
	p := handler.attachCalls[0]
	if p.ConnectionID != nqn {
		t.Errorf("AttachParams.ConnectionID = %q, want %q", p.ConnectionID, nqn)
	}
	if p.Address != address {
		t.Errorf("AttachParams.Address = %q, want %q", p.Address, address)
	}
	if p.Port != port {
		t.Errorf("AttachParams.Port = %q, want %q", p.Port, port)
	}
	if p.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("AttachParams.ProtocolType = %q, want %q", p.ProtocolType, ProtocolNVMeoFTCP)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_StateWrittenAfterAttach
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_StateWrittenAfterAttach verifies that after a
// successful Attach, NodeStageVolume persists a nodeStageState in discriminated
// union format with the correct protocol type and NVMe-oF sub-struct.
func TestNodeStageVolume_Dispatch_StateWrittenAfterAttach(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:state-written"
		address = "192.168.1.5"
		port    = "4420"
		volID   = "tank/pvc-state-written"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: nqn,
				Address:   address,
				Port:      port,
			},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: nqn,
			VolumeContextKeyAddress:  address,
			VolumeContextKeyPort:     port,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}

	state, readErr := env.srv.readStageState(volID)
	if readErr != nil {
		t.Fatalf("readStageState: %v", readErr)
	}
	if state == nil {
		t.Fatal("stage state is nil after NodeStageVolume")
	}
	// Discriminated union: ProtocolType must identify the active sub-struct.
	if state.ProtocolType != ProtocolNVMeoFTCP {
		t.Errorf("state.ProtocolType = %q, want %q", state.ProtocolType, ProtocolNVMeoFTCP)
	}
	if state.NVMeoF == nil {
		t.Fatal("state.NVMeoF sub-struct is nil after NVMe-oF stage")
	}
	if state.NVMeoF.SubsysNQN != nqn {
		t.Errorf("NVMeoF.SubsysNQN = %q, want %q", state.NVMeoF.SubsysNQN, nqn)
	}
	if state.NVMeoF.Address != address {
		t.Errorf("NVMeoF.Address = %q, want %q", state.NVMeoF.Address, address)
	}
	if state.NVMeoF.Port != port {
		t.Errorf("NVMeoF.Port = %q, want %q", state.NVMeoF.Port, port)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_IdempotentWhenAlreadyMounted
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_IdempotentWhenAlreadyMounted verifies that
// NodeStageVolume returns success without calling Attach when the volume is
// already staged (state file present + staging path mounted).
func TestNodeStageVolume_Dispatch_IdempotentWhenAlreadyMounted(t *testing.T) {
	t.Parallel()

	const (
		nqn     = "nqn.2024-01.com.example:storage:idempotent"
		address = "10.0.0.1"
		port    = "4420"
		volID   = "tank/pvc-idempotent-dispatch"
	)

	handler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State:      &NVMeoFProtocolState{SubsysNQN: nqn, Address: address, Port: port},
		},
	}
	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: handler,
	})
	stagingPath := t.TempDir()

	// Pre-write a stage state file as if NodeStageVolume had already succeeded.
	if err := env.srv.writeStageState(volID, &nodeStageState{
		ProtocolType: ProtocolNVMeoFTCP,
		NVMeoF: &NVMeoFStageState{
			SubsysNQN: nqn,
			Address:   address,
			Port:      port,
		},
	}); err != nil {
		t.Fatalf("writeStageState: %v", err)
	}
	// Mark the staging path as already mounted.
	env.mounter.mountedPaths[stagingPath] = true

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          volID,
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: nqn,
			VolumeContextKeyAddress:  address,
			VolumeContextKeyPort:     port,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (idempotent): %v", err)
	}

	// Attach must NOT have been called — idempotency short-circuits before it.
	if len(handler.attachCalls) != 0 {
		t.Errorf("Attach called %d times on idempotent call, want 0", len(handler.attachCalls))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_NilHandlersMap
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_NilHandlersMap verifies that a NodeServer with
// a nil handlers map returns FailedPrecondition without panicking.
func TestNodeStageVolume_Dispatch_NilHandlersMap(t *testing.T) {
	t.Parallel()

	// NodeServer with no handlers at all.
	env := newHandlerNodeTestEnv(t, nil)
	stagingPath := t.TempDir()

	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-nil-handlers",
		StagingTargetPath: stagingPath,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID: "nqn.test:vol",
			VolumeContextKeyAddress:  "10.0.0.1",
			VolumeContextKeyPort:     "4420",
		},
	})
	if err == nil {
		t.Fatal("expected FailedPrecondition for nil handlers map, got nil")
	}
	requireGRPCCode(t, err, codes.FailedPrecondition)
}

// ─────────────────────────────────────────────────────────────────────────────
// TestNodeStageVolume_Dispatch_MultipleProtocols
// ─────────────────────────────────────────────────────────────────────────────

// TestNodeStageVolume_Dispatch_MultipleProtocols verifies that each registered
// ProtocolHandler is called only for its own protocol type when multiple
// handlers are registered.
func TestNodeStageVolume_Dispatch_MultipleProtocols(t *testing.T) {
	t.Parallel()

	nvmeHandler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/nvme0n1",
			State: &NVMeoFProtocolState{
				SubsysNQN: "nqn.test:multi",
				Address:   "10.0.0.1",
				Port:      "4420",
			},
		},
	}
	iscsiHandler := &fakeProtocolHandler{
		attachResult: &AttachResult{
			DevicePath: "/dev/sda",
			State:      &fakeProtocolState{protocol: "iscsi"},
		},
	}

	env := newHandlerNodeTestEnv(t, map[string]ProtocolHandler{
		ProtocolNVMeoFTCP: nvmeHandler,
		"iscsi":           iscsiHandler,
	})

	// Stage an NVMe-oF volume.
	stagingPath1 := t.TempDir()
	_, err := env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-nvme-multi",
		StagingTargetPath: stagingPath1,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:     "nqn.test:multi",
			VolumeContextKeyAddress:      "10.0.0.1",
			VolumeContextKeyPort:         "4420",
			VolumeContextKeyProtocolType: ProtocolNVMeoFTCP,
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (nvme-multi): %v", err)
	}

	// Stage an iSCSI volume.
	stagingPath2 := t.TempDir()
	_, err = env.srv.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "tank/pvc-iscsi-multi",
		StagingTargetPath: stagingPath2,
		VolumeCapability:  mountCap("ext4"),
		VolumeContext: map[string]string{
			VolumeContextKeyTargetID:     "iqn.test:multi",
			VolumeContextKeyAddress:      "10.0.0.2",
			VolumeContextKeyPort:         "3260",
			VolumeContextKeyProtocolType: "iscsi",
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume (iscsi-multi): %v", err)
	}

	// Each handler must have been called exactly once.
	if len(nvmeHandler.attachCalls) != 1 {
		t.Errorf("nvmeHandler.Attach called %d times, want 1", len(nvmeHandler.attachCalls))
	}
	if len(iscsiHandler.attachCalls) != 1 {
		t.Errorf("iscsiHandler.Attach called %d times, want 1", len(iscsiHandler.attachCalls))
	}
}
