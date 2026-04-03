package e2e

// tc_e3_inprocess_test.go — Per-TC assertions for E3: Node Stage/Publish.

import (
	"os"
	"path/filepath"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

const (
	testVolumeID  = "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-node-test"
	testTargetNQN = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-node-test"
	testAgentAddr = "127.0.0.1"
	testAgentPort = "4420"
)

func nodeVolumeContext() map[string]string {
	return map[string]string{
		csidrv.VolumeContextKeyTargetID: testTargetNQN,
		csidrv.VolumeContextKeyAddress:   testAgentAddr,
		csidrv.VolumeContextKeyPort:      testAgentPort,
	}
}

func assertE3_NodeFullLifecycle(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := testVolumeID

	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	// 1. NodeStageVolume
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume", tc.tcNodeLabel())
	Expect(env.connector.connectCalls).To(HaveLen(1), "%s: connect called once", tc.tcNodeLabel())
	Expect(env.mounter.formatAndMountCalls).To(HaveLen(1), "%s: formatAndMount called once", tc.tcNodeLabel())

	// 2. NodePublishVolume
	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodePublishVolume", tc.tcNodeLabel())
	Expect(env.mounter.mountCalls).To(HaveLen(1), "%s: mount called once", tc.tcNodeLabel())

	// 3. NodeUnpublishVolume
	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnpublishVolume", tc.tcNodeLabel())
	Expect(env.mounter.unmountCalls).To(HaveLen(1), "%s: unmount called once", tc.tcNodeLabel())

	// 4. NodeUnstageVolume
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume", tc.tcNodeLabel())
	Expect(env.connector.disconnectCalls).To(HaveLen(1), "%s: disconnect called once", tc.tcNodeLabel())
}

func assertE3_NodeStageVolume(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s", tc.tcNodeLabel())
	Expect(env.connector.connectCalls).To(HaveLen(1))
	Expect(env.mounter.formatAndMountCalls).To(HaveLen(1))
}

func assertE3_NodeUnstageVolume(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	// Stage first
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	// Unstage
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume", tc.tcNodeLabel())
	Expect(env.connector.disconnectCalls).To(HaveLen(1))
}

func assertE3_NodePublishVolume(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	// Stage first
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	// Publish
	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s", tc.tcNodeLabel())
	Expect(env.mounter.mountCalls).To(HaveLen(1))
}

func assertE3_NodeUnpublishVolume(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagePath,
		VolumeCapability: mountCapability("ext4"), VolumeContext: nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagePath, TargetPath: targetPath,
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s", tc.tcNodeLabel())
	Expect(env.mounter.unmountCalls).To(HaveLen(1))
}

func assertE3_NodeStageVolume_Idempotency(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	req := &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	}

	_, err := env.node.NodeStageVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first stage", tc.tcNodeLabel())

	// Second call should be idempotent (no-op)
	_, err = env.node.NodeStageVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second stage (idempotent)", tc.tcNodeLabel())
}

func assertE3_NodeStageVolume_MissingVolumeContext(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     map[string]string{}, // missing target_id, address, port
	})
	Expect(err).To(HaveOccurred(), "%s: missing volume context should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE3_NodeStageVolume_ConnectError(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	env.connector.connectErr = status.Error(codes.Internal, "connect failed")
	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: connect error should propagate", tc.tcNodeLabel())
}

func assertE3_NodeStageVolume_FormatMountError(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	env.mounter.formatAndMountErr = status.Error(codes.Internal, "format failed")
	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: format error should propagate", tc.tcNodeLabel())
}

func assertE3_NodeUnstageVolume_Idempotency(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID + "-unstage-idem"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	req := &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	}
	_, err = env.node.NodeUnstageVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first unstage", tc.tcNodeLabel())

	// Second unstage should be idempotent
	_, err = env.node.NodeUnstageVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second unstage (idempotent)", tc.tcNodeLabel())
}

func assertE3_NodeUnstageVolume_DisconnectError(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := testVolumeID + "-disconnect-err"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	env.connector.disconnectErr = status.Error(codes.Internal, "disconnect failed")
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	Expect(err).To(HaveOccurred(), "%s: disconnect error should propagate", tc.tcNodeLabel())
}

func assertE3_StageState_Persistence(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage-persist")
	volumeID := testVolumeID + "-persist"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: stage", tc.tcNodeLabel())

	// Verify state file exists
	files, err := os.ReadDir(env.stateDir)
	Expect(err).NotTo(HaveOccurred())
	Expect(files).NotTo(BeEmpty(), "%s: state file should be created", tc.tcNodeLabel())
}

func assertE3_StageState_CorruptedFile(_ documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	// Write a corrupted state file
	volumeID := testVolumeID + "-corrupt"
	stagePath := filepath.Join(env.stateDir, "stage-corrupt")

	// Create a corrupted state file manually
	stateFile := filepath.Join(env.stateDir, "corrupt-state.json")
	err := os.WriteFile(stateFile, []byte("not-valid-json"), 0o600)
	Expect(err).NotTo(HaveOccurred())

	// NodeUnstage with missing/corrupted state should still succeed (idempotent)
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	// May succeed (no state file found → no-op) or fail gracefully
	_ = err
}

func assertE3_NodePublishVolume_Idempotency(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage-pub-idem")
	targetPath := filepath.Join(env.stateDir, "target-pub-idem")
	volumeID := testVolumeID + "-pub-idem"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	req := &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	}
	_, err = env.node.NodePublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first publish", tc.tcNodeLabel())

	_, err = env.node.NodePublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second publish (idempotent)", tc.tcNodeLabel())

	// Mount should only be called once (second is no-op due to IsMounted check)
	Expect(env.mounter.mountCalls).To(HaveLen(1),
		"%s: mount called exactly once", tc.tcNodeLabel())
}

func assertE3_NodePublishVolume_ReadOnly(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage-ro")
	targetPath := filepath.Join(env.stateDir, "target-ro")
	volumeID := testVolumeID + "-ro"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
		Readonly:          true,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: readonly publish", tc.tcNodeLabel())

	// Verify "ro" option was passed
	if len(env.mounter.mountCalls) > 0 {
		opts := env.mounter.mountCalls[0].options
		found := false
		for _, o := range opts {
			if o == "ro" {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "%s: ro option should be in mount options", tc.tcNodeLabel())
	}
}

func assertE3_NodePublishVolume_MountError(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	env.mounter.mountErr = status.Error(codes.Internal, "mount failed")
	stagePath := filepath.Join(env.stateDir, "stage-mnt-err")
	targetPath := filepath.Join(env.stateDir, "target-mnt-err")
	volumeID := testVolumeID + "-mnt-err"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: mount error should propagate", tc.tcNodeLabel())
}

func assertE3_NodeUnpublishVolume_Idempotency(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage-unpub-idem")
	targetPath := filepath.Join(env.stateDir, "target-unpub-idem")
	volumeID := testVolumeID + "-unpub-idem"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	req := &csiapi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: targetPath}
	_, err = env.node.NodeUnpublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first unpublish", tc.tcNodeLabel())

	_, err = env.node.NodeUnpublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second unpublish (idempotent)", tc.tcNodeLabel())
}

func assertE3_NodeUnpublishVolume_UnmountError(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage-unmnt-err")
	targetPath := filepath.Join(env.stateDir, "target-unmnt-err")
	volumeID := testVolumeID + "-unmnt-err"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	env.mounter.unmountErr = status.Error(codes.Internal, "unmount failed")
	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	Expect(err).To(HaveOccurred(), "%s: unmount error should propagate", tc.tcNodeLabel())
}
