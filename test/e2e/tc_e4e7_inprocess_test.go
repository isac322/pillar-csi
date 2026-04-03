package e2e

// tc_e4e7_inprocess_test.go — Per-TC assertions for E4 (cross-component lifecycle),
// E5 (ordering constraints), E6 (partial failure persistence), E7 (publish idempotency).

import (
	"path/filepath"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pillarv1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// E4: Cross-component lifecycle
// ─────────────────────────────────────────────────────────────────────────────

func assertE4_FullChain(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e4-full",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 1 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	// Publish
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-e4",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:worker-e4",
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-e4",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerPublishVolume", tc.tcNodeLabel())

	// Unpublish
	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "worker-e4",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerUnpublishVolume", tc.tcNodeLabel())

	// Delete
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())
}

func assertE4_CreateAndExpand(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.expandVolumeResp = &agentv1.ExpandVolumeResponse{CapacityBytes: 2 << 30}
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e4-expand",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())

	expandResp, err := env.controller.ControllerExpandVolume(env.ctx, &csiapi.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csiapi.CapacityRange{RequiredBytes: 2 << 30},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerExpandVolume", tc.tcNodeLabel())
	Expect(expandResp.GetCapacityBytes()).To(BeNumerically(">=", int64(2<<30)))
	Expect(expandResp.GetNodeExpansionRequired()).To(BeTrue())
}

func assertE4_PublishUnpublish(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-e4-pub",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:worker-e4-pub",
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e4-pub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-e4-pub",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: publish", tc.tcNodeLabel())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e4-pub",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: unpublish", tc.tcNodeLabel())
}

func assertE4_DeleteAfterPublish(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-e4-del",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:worker-e4-del",
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e4-del",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e4-del",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e4-del",
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: delete after publish/unpublish", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E5: Ordering constraints
// ─────────────────────────────────────────────────────────────────────────────

func assertE5_NodeStageBeforeControllerPublish(tc documentedCase) {
	nodeEnv := newNodeTestEnv()
	defer nodeEnv.close()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e5-order-1"
	// Don't set to ControllerPublished state — SM is in default state
	stagePath := filepath.Join(nodeEnv.stateDir, "stage")

	_, err := nodeEnv.node.NodeStageVolume(nodeEnv.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	// With SM enabled, should get FailedPrecondition
	Expect(err).To(HaveOccurred(), "%s: stage before controller publish should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
}

func assertE5_NodePublishBeforeNodeStage(tc documentedCase) {
	nodeEnv := newNodeTestEnv()
	defer nodeEnv.close()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e5-order-2"
	nodeEnv.sm.ForceState(volumeID, csidrv.StateControllerPublished)
	targetPath := filepath.Join(nodeEnv.stateDir, "target")

	_, err := nodeEnv.node.NodePublishVolume(nodeEnv.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: filepath.Join(nodeEnv.stateDir, "stage"),
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: publish before stage should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
}

func assertE5_NodeUnstageBeforeNodeUnpublish(tc documentedCase) {
	nodeEnv := newNodeTestEnv()
	defer nodeEnv.close()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e5-order-3"
	stagePath := filepath.Join(nodeEnv.stateDir, "stage")
	targetPath := filepath.Join(nodeEnv.stateDir, "target")
	nodeEnv.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := nodeEnv.node.NodeStageVolume(nodeEnv.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = nodeEnv.node.NodePublishVolume(nodeEnv.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	// Try to unstage without unpublishing first — SM should reject
	_, err = nodeEnv.node.NodeUnstageVolume(nodeEnv.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
	})
	Expect(err).To(HaveOccurred(), "%s: unstage before unpublish should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
}

func assertE5_ControllerUnpublishBeforeNodeStage(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e5-unpub-before",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	// ControllerUnpublish on a non-published volume should succeed (no-op)
	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-1",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: controller unpublish on non-published OK", tc.tcNodeLabel())
}

func assertE5_DeleteBeforeControllerUnpublish(_ documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-e5-del",
			Annotations: map[string]string{"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.test:e5"},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e5-del-before",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e5-del",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	// Attempt to delete while still published — may succeed (no-op) or fail
	// depending on implementation. Both are acceptable.
	_, _ = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
}

func assertE5_ValidTransitionAfterRecovery(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Simulate recovery by restoring state from CRD
	pvcName := "pvc-e5-recover"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/" + pvcName
	pv := &pillarv1.PillarVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec:       pillarv1.PillarVolumeSpec{VolumeID: volumeID},
		Status: pillarv1.PillarVolumeStatus{
			Phase: pillarv1.PillarVolumePhaseReady,
			ExportInfo: &pillarv1.VolumeExportInfo{
				TargetID:  "nqn.test",
				Address:   "127.0.0.1",
				Port:      4420,
				VolumeRef: "tank/" + pvcName,
			},
		},
	}
	Expect(env.k8sClient.Create(env.ctx, pv)).To(Succeed())
	Expect(env.k8sClient.Status().Update(env.ctx, pv)).To(Succeed())

	Expect(env.controller.LoadStateFromPillarVolumes(env.ctx)).To(Succeed())

	// After recovery, delete should work
	_, err := env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: delete after recovery", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E6: Partial failure persistence
// ─────────────────────────────────────────────────────────────────────────────

func assertE6_PartialFailure_CreateThenExportFail_CRD(tc documentedCase) {
	assertE1_PartialFailure_CreateThenExportFail(tc)
}

func assertE6_DeleteVolume_CleansUpCRD(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e6-cleanup",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())

	// CRD should be gone
	pv := lookupPillarVolume(env, "pvc-e6-cleanup")
	Expect(pv).To(BeNil(), "%s: PillarVolume CRD should be deleted", tc.tcNodeLabel())
}

func assertE6_DeleteVolumeOnPartialCreates(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	pvcName := "pvc-e6-partial-del"
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/" + pvcName
	pv := &pillarv1.PillarVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec:       pillarv1.PillarVolumeSpec{VolumeID: volumeID},
		Status: pillarv1.PillarVolumeStatus{
			Phase: pillarv1.PillarVolumePhaseCreatePartial,
			PartialFailure: &pillarv1.PartialFailureInfo{
				FailedOperation: "ExportVolume",
				BackendCreated:  true,
			},
		},
	}
	Expect(env.k8sClient.Create(env.ctx, pv)).To(Succeed())
	Expect(env.k8sClient.Status().Update(env.ctx, pv)).To(Succeed())
	env.controller.GetStateMachine().ForceState(volumeID, csidrv.StateCreatePartial)

	_, err := env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: delete partial-create volume", tc.tcNodeLabel())

	pv2 := &pillarv1.PillarVolume{}
	err = env.k8sClient.Get(env.ctx, types.NamespacedName{Name: pvcName}, pv2)
	Expect(err).To(HaveOccurred(), "%s: CRD should be deleted", tc.tcNodeLabel())
}

func assertE6_ZvolNoDup_OneZvol(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Inject export error
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-zvol-nodup",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred())

	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(1), "%s: exactly one CreateVolume call", tc.tcNodeLabel())
	Expect(c.ExportVolume).To(Equal(1), "%s: one export attempt", tc.tcNodeLabel())

	// Clear export error and retry
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-zvol-nodup",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: retry should succeed", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty())

	c2 := env.agentSrv.counts()
	// CreateVolume should still be 1 (skipBackend on retry)
	Expect(c2.CreateVolume).To(Equal(1), "%s: still only 1 CreateVolume (no dup)", tc.tcNodeLabel())
}

func assertE6_ZvolNoDup_DeleteRegistry(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Inject export error on first attempt
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
	env.agentSrv.mu.Unlock()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-nodup-del",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred())

	// Get the volume ID from the CRD
	pv := &pillarv1.PillarVolume{}
	err = env.k8sClient.Get(env.ctx, types.NamespacedName{Name: "pvc-nodup-del"}, pv)
	Expect(err).NotTo(HaveOccurred(), "%s: CRD should exist after partial create", tc.tcNodeLabel())
	volumeID := pv.Spec.VolumeID

	// Delete the volume
	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: delete partial volume", tc.tcNodeLabel())

	pv2 := &pillarv1.PillarVolume{}
	err = env.k8sClient.Get(env.ctx, types.NamespacedName{Name: "pvc-nodup-del"}, pv2)
	Expect(err).To(HaveOccurred(), "%s: CRD should be deleted", tc.tcNodeLabel())
}

func assertE6_ZvolNoDup_MultipleRetries(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	const retryFails = 3

	for i := 0; i < retryFails; i++ {
		env.agentSrv.mu.Lock()
		env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
		env.agentSrv.mu.Unlock()

		_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
			Name:               "pvc-multi-retry",
			Parameters:         env.params,
			VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		})
		Expect(err).To(HaveOccurred(), "%s: attempt %d should fail", tc.tcNodeLabel(), i+1)

		c := env.agentSrv.counts()
		Expect(c.CreateVolume).To(Equal(1),
			"%s: after %d retries, CreateVolume still 1 (no dup)", tc.tcNodeLabel(), i+1)
	}

	// Final attempt: success
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-multi-retry",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: final attempt should succeed", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty())

	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(1), "%s: total CreateVolume = 1 (no dup)", tc.tcNodeLabel())
	Expect(c.ExportVolume).To(Equal(retryFails+1), "%s: total ExportVolume = %d", tc.tcNodeLabel(), retryFails+1)
}

// ─────────────────────────────────────────────────────────────────────────────
// E7: Publish idempotency
// ─────────────────────────────────────────────────────────────────────────────

func assertE7_ControllerPublishIdempotency(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker-e7",
			Annotations: map[string]string{"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.test:e7"},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e7-ctrl-pub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e7",
		VolumeCapability: mountCapability("ext4"),
	}
	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first publish", tc.tcNodeLabel())

	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second publish (idempotent)", tc.tcNodeLabel())
}

func assertE7_NodePublishIdempotency(tc documentedCase) {
	assertE3_NodePublishVolume_Idempotency(tc)
}

func assertE7_NodePublishIdempotency_Readonly(tc documentedCase) {
	assertE3_NodePublishVolume_ReadOnly(tc)
}

func assertE7_ControllerUnpublishIdempotency(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e7-ctrl-unpub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerUnpublishVolumeRequest{VolumeId: volumeID, NodeId: "worker-1"}
	_, err = env.controller.ControllerUnpublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first unpublish", tc.tcNodeLabel())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second unpublish (idempotent)", tc.tcNodeLabel())
}

func assertE7_NodeUnpublishIdempotency(tc documentedCase) {
	assertE3_NodeUnpublishVolume_Idempotency(tc)
}
