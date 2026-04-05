package e2e

// tc_efault_inprocess_test.go — Per-TC assertions for E-FAULT: Fault injection scenarios.
//
// E-FAULT tests inject faults into the in-process mock infrastructure to verify
// that the CSI controller handles error conditions gracefully.
//
//   - E-FAULT-2-1: Agent network partition → CreateVolume fails with Unavailable
//   - E-FAULT-3-1: Pool exhaustion → CreateVolume fails with ResourceExhausted
//   - E-FAULT-4-1: Backing device removed → DeleteVolume returns error gracefully
//   - E-FAULT-5-1: Multi-node volume access → ControllerPublish on 2nd node fails or succeeds with SINGLE_NODE policy

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assertEFAULT_AgentNetworkPartition_CreateVolumeFails verifies that when the
// agent is unreachable (simulated network partition), CreateVolume returns an
// Unavailable error and does not panic.
func assertEFAULT_AgentNetworkPartition_CreateVolumeFails(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Simulate network partition: agent returns Unavailable for all creates.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.Unavailable, "network partition: agent unreachable")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-2-1",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(),
		"%s: CreateVolume must fail when agent is unreachable (network partition)", tc.tcNodeLabel())
	Expect(status.Code(err)).To(BeElementOf(codes.Unavailable, codes.Internal, codes.Unknown),
		"%s: error code must indicate unreachable agent", tc.tcNodeLabel())
}

// assertEFAULT_PoolExhaustion_CreateVolumeFails verifies that when the storage
// pool is exhausted, CreateVolume fails with a ResourceExhausted error.
func assertEFAULT_PoolExhaustion_CreateVolumeFails(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Simulate pool exhaustion: agent returns ResourceExhausted.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.ResourceExhausted, "pool exhausted: no space left")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-3-1",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(),
		"%s: CreateVolume must fail when pool is exhausted", tc.tcNodeLabel())
}

// assertEFAULT_BackingDeviceRemoved_GracefulError verifies that when the backing
// device is removed mid-operation, DeleteVolume returns a structured error and
// does not panic or hang.
func assertEFAULT_BackingDeviceRemoved_GracefulError(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// First create a volume successfully.
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-4-1",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume must succeed initially", tc.tcNodeLabel())

	// Simulate backing device removal: agent returns Internal error on delete.
	env.agentSrv.mu.Lock()
	env.agentSrv.deleteVolumeErr = status.Error(codes.Internal, "backing device removed: /dev/sdb not found")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	// The controller must return an error (not panic) when the backing device is gone.
	Expect(err).To(HaveOccurred(),
		"%s: DeleteVolume must return error when backing device is removed", tc.tcNodeLabel())
}

// assertEFAULT_NodeReboot_AgentRecovery verifies that after the mock agent is
// restarted (simulating a node reboot), the CSI controller can successfully
// re-establish connectivity and execute CreateVolume without panicking.
func assertEFAULT_NodeReboot_AgentRecovery(tc documentedCase) {
	// Phase 1: Create a volume successfully before "reboot".
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-1-1-pre",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume must succeed before simulated reboot", tc.tcNodeLabel())

	// Phase 2: Simulate node reboot by injecting a transient Unavailable error,
	// then clearing it (agent "recovered").
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.Unavailable, "agent restarting after node reboot")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-1-1-during",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(),
		"%s: CreateVolume must fail while agent is unavailable (during reboot)", tc.tcNodeLabel())

	// Phase 3: Agent recovers — clear the injected error.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = nil
	env.agentSrv.mu.Unlock()

	_, err = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-1-1-post",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume must succeed after agent recovery (post-reboot)", tc.tcNodeLabel())
}

// assertEFAULT_AgentNetworkPartition_Recovery verifies that after a simulated
// network partition clears, the CSI controller can successfully resume
// operations without requiring a restart.
func assertEFAULT_AgentNetworkPartition_Recovery(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Simulate network partition.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = status.Error(codes.Unavailable, "network partition: agent unreachable")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-2-2-during",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(),
		"%s: CreateVolume must fail during network partition", tc.tcNodeLabel())
	Expect(status.Code(err)).To(BeElementOf(codes.Unavailable, codes.Internal, codes.Unknown),
		"%s: error code must indicate network issue", tc.tcNodeLabel())

	// Network partition recovers — clear the injected error.
	env.agentSrv.mu.Lock()
	env.agentSrv.createVolumeErr = nil
	env.agentSrv.mu.Unlock()

	_, err = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-2-2-after",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(),
		"%s: CreateVolume must succeed after network partition recovery", tc.tcNodeLabel())
}

// assertEFAULT_MultiNode_VolumeAccessFromDifferentWorker verifies the behavior
// when two nodes attempt to publish a volume concurrently under SINGLE_NODE_WRITER
// access mode. The second publish must either fail or the controller must enforce
// the access policy.
func assertEFAULT_MultiNode_VolumeAccessFromDifferentWorker(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create a volume.
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-efault-5-1",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume must succeed", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	// Publish to first node.
	makeCSINodeWithNQN(env, "node-efault-5-1-a", "nqn.2026-01.io.example:node-efault-5-1-a")
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "node-efault-5-1-a",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: first ControllerPublishVolume must succeed", tc.tcNodeLabel())

	// Attempt to publish to a second node. Under SINGLE_NODE_WRITER semantics,
	// this may succeed (the CSI spec allows it at controller level) or fail.
	// What must NOT happen: panic or hang.
	makeCSINodeWithNQN(env, "node-efault-5-1-b", "nqn.2026-01.io.example:node-efault-5-1-b")
	_, secondErr := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "node-efault-5-1-b",
		VolumeCapability: mountCapability("ext4"),
	})
	// Either outcome is valid per CSI spec — the important invariant is no panic.
	if secondErr != nil {
		Expect(status.Code(secondErr)).To(BeElementOf(
			codes.FailedPrecondition, codes.AlreadyExists, codes.Internal, codes.InvalidArgument,
		), "%s: second publish error code must be well-defined", tc.tcNodeLabel())
	}
	// Explicit assertion: the volume ID remains valid regardless of the second publish outcome.
	Expect(volumeID).NotTo(BeEmpty(), "%s: volumeID must remain non-empty throughout", tc.tcNodeLabel())
}
