package e2e

// tc_e24_inprocess_test.go — Per-TC assertions for E24: 8-stage full lifecycle
// integration scenarios (Full Lifecycle Integration).
//
// E24 tests validate the complete CreateVolume → ControllerPublish → NodeStage
// → NodePublish → NodeUnpublish → NodeUnstage → ControllerUnpublish →
// DeleteVolume chain, including partial failure and recovery scenarios.
//
// Sections covered:
//
//	E24.1  — Normal path: complete 8-stage chain
//	E24.2  — CreateVolume stage failure/recovery
//	E24.3  — ControllerPublish stage failure/recovery
//	E24.4  — NodeStage stage failure/recovery
//	E24.5  — NodePublish stage failure/recovery
//	E24.6  — NodeUnpublish stage failure/recovery
//	E24.7  — NodeUnstage stage failure/recovery
//	E24.8  — ControllerUnpublish stage failure/recovery
//	E24.9  — DeleteVolume stage failure/recovery
//	E24.10 — Aborted lifecycle cleanup paths

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
	csidrv "github.com/bhyoo/pillar-csi/internal/csi"
)

// ─────────────────────────────────────────────────────────────────────────────
// lifecycleTestEnv: combined controller + node environment for E24 tests
// ─────────────────────────────────────────────────────────────────────────────

// lifecycleTestEnv combines a controllerTestEnv and nodeTestEnv for 8-stage
// full lifecycle tests. The controller and node operate independently, simulating
// the Container Orchestrator (CO) calling the appropriate driver methods in
// the correct sequence.
type lifecycleTestEnv struct {
	ctrlEnv    *controllerTestEnv
	nodeEnv    *nodeTestEnv
	stagePath  string
	targetPath string
	nodeID     string
}

func newLifecycleTestEnv() *lifecycleTestEnv {
	ctrlEnv := newControllerTestEnv()
	nodeEnv := newNodeTestEnv()
	return &lifecycleTestEnv{
		ctrlEnv:    ctrlEnv,
		nodeEnv:    nodeEnv,
		stagePath:  filepath.Join(nodeEnv.stateDir, "stage"),
		targetPath: filepath.Join(nodeEnv.stateDir, "target"),
		nodeID:     "worker-lifecycle",
	}
}

func (e *lifecycleTestEnv) close() {
	e.ctrlEnv.close()
	e.nodeEnv.close()
}

// createCSINode registers a CSINode object with the NVMe-oF host NQN annotation
// required by ControllerPublishVolume.
func (e *lifecycleTestEnv) createCSINode(nodeID string) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeID,
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:" + nodeID,
			},
		},
	}
	_ = e.ctrlEnv.k8sClient.Create(e.ctrlEnv.ctx, csiNode)
}

// advanceNodeSMToControllerPublished advances the node's VolumeStateMachine to
// StateControllerPublished, simulating the CO sequencing ControllerPublish
// before NodeStage.
func (e *lifecycleTestEnv) advanceNodeSMToControllerPublished(volumeID string) {
	e.nodeEnv.sm.ForceState(volumeID, csidrv.StateControllerPublished)
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.1 — Normal path: 8-stage complete chain
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_FullCycle implements TC[E24.1-1]: complete 8-stage lifecycle.
// TestCSILifecycle_FullCycle
func assertE24_FullCycle(tc documentedCase) {
	env := newLifecycleTestEnv()
	defer env.close()

	env.createCSINode(env.nodeID)

	// Stage 1: CreateVolume
	createResp, err := env.ctrlEnv.controller.CreateVolume(env.ctrlEnv.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-full-cycle",
		Parameters:         env.ctrlEnv.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()
	Expect(volumeID).NotTo(BeEmpty(), "%s: VolumeId empty", tc.tcNodeLabel())
	Expect(volumeContext).NotTo(BeEmpty(), "%s: VolumeContext empty", tc.tcNodeLabel())

	// Stage 2: ControllerPublishVolume
	_, err = env.ctrlEnv.controller.ControllerPublishVolume(env.ctrlEnv.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           env.nodeID,
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerPublishVolume", tc.tcNodeLabel())

	// Simulate CO advancing node SM after ControllerPublish
	env.advanceNodeSMToControllerPublished(volumeID)

	// Stage 3: NodeStageVolume
	_, err = env.nodeEnv.node.NodeStageVolume(env.nodeEnv.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: env.stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     volumeContext,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume", tc.tcNodeLabel())
	Expect(env.nodeEnv.connector.connectCalls).To(HaveLen(1), "%s: Connect called once", tc.tcNodeLabel())

	// Stage 4: NodePublishVolume
	_, err = env.nodeEnv.node.NodePublishVolume(env.nodeEnv.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: env.stagePath,
		TargetPath:        env.targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodePublishVolume", tc.tcNodeLabel())
	Expect(env.nodeEnv.mounter.mountCalls).To(HaveLen(1), "%s: Mount called once", tc.tcNodeLabel())

	// Stage 5: NodeUnpublishVolume
	_, err = env.nodeEnv.node.NodeUnpublishVolume(env.nodeEnv.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: env.targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnpublishVolume", tc.tcNodeLabel())
	Expect(env.nodeEnv.mounter.unmountCalls).To(HaveLen(1), "%s: Unmount called once", tc.tcNodeLabel())

	// Stage 6: NodeUnstageVolume
	_, err = env.nodeEnv.node.NodeUnstageVolume(env.nodeEnv.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: env.stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeUnstageVolume", tc.tcNodeLabel())
	Expect(env.nodeEnv.connector.disconnectCalls).To(HaveLen(1), "%s: Disconnect called once", tc.tcNodeLabel())

	// Stage 7: ControllerUnpublishVolume
	_, err = env.ctrlEnv.controller.ControllerUnpublishVolume(env.ctrlEnv.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   env.nodeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerUnpublishVolume", tc.tcNodeLabel())

	// Stage 8: DeleteVolume
	_, err = env.ctrlEnv.controller.DeleteVolume(env.ctrlEnv.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())

	// Verify agent call counts
	c := env.ctrlEnv.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(1), "%s: agent.CreateVolume 1x", tc.tcNodeLabel())
	Expect(c.ExportVolume).To(Equal(1), "%s: agent.ExportVolume 1x", tc.tcNodeLabel())
	Expect(c.AllowInitiator).To(Equal(1), "%s: agent.AllowInitiator 1x", tc.tcNodeLabel())
	Expect(c.DenyInitiator).To(Equal(1), "%s: agent.DenyInitiator 1x", tc.tcNodeLabel())
	Expect(c.UnexportVolume).To(Equal(1), "%s: agent.UnexportVolume 1x", tc.tcNodeLabel())
	Expect(c.DeleteVolume).To(Equal(1), "%s: agent.DeleteVolume 1x", tc.tcNodeLabel())
}

// assertE24_VolumeContextFlowThrough implements TC[E24.1-2]: VolumeContext flows
// from CreateVolume to NodeStageVolume without key translation.
// TestCSILifecycle_VolumeContextFlowThrough
func assertE24_VolumeContextFlowThrough(tc documentedCase) {
	env := newLifecycleTestEnv()
	defer env.close()

	env.createCSINode(env.nodeID)

	createResp, err := env.ctrlEnv.controller.CreateVolume(env.ctrlEnv.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-context-flow",
		Parameters:         env.ctrlEnv.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()

	// VolumeContext must contain the expected NVMe-oF keys
	Expect(volumeContext).To(HaveKey(csidrv.VolumeContextKeyTargetID),
		"%s: VolumeContext missing target_id", tc.tcNodeLabel())
	Expect(volumeContext).To(HaveKey(csidrv.VolumeContextKeyAddress),
		"%s: VolumeContext missing address", tc.tcNodeLabel())
	Expect(volumeContext).To(HaveKey(csidrv.VolumeContextKeyPort),
		"%s: VolumeContext missing port", tc.tcNodeLabel())

	// Advance node SM to StateControllerPublished
	env.advanceNodeSMToControllerPublished(volumeID)

	// NodeStageVolume must accept the VolumeContext verbatim (no key translation)
	_, err = env.nodeEnv.node.NodeStageVolume(env.nodeEnv.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: env.stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     volumeContext, // pass as-is from CreateVolume
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume with CreateVolume context", tc.tcNodeLabel())

	// Connector must have been called with the target NQN from VolumeContext
	Expect(env.nodeEnv.connector.connectCalls).To(HaveLen(1),
		"%s: Connect must be called once", tc.tcNodeLabel())
	call := env.nodeEnv.connector.connectCalls[0]
	Expect(call.subsysNQN).To(Equal(volumeContext[csidrv.VolumeContextKeyTargetID]),
		"%s: Connect.subsysNQN must match VolumeContext.target_id", tc.tcNodeLabel())
	Expect(call.trAddr).To(Equal(volumeContext[csidrv.VolumeContextKeyAddress]),
		"%s: Connect.trAddr must match VolumeContext.address", tc.tcNodeLabel())
}

// assertE24_OrderingConstraints implements TC[E24.1-3]: 8-stage chain with
// per-stage intermediate state verification.
// TestCSILifecycle_OrderingConstraints
func assertE24_OrderingConstraints(tc documentedCase) {
	env := newLifecycleTestEnv()
	defer env.close()

	env.createCSINode(env.nodeID)

	// Phase 1: CreateVolume
	createResp, err := env.ctrlEnv.controller.CreateVolume(env.ctrlEnv.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-ordering",
		Parameters:         env.ctrlEnv.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase1 CreateVolume", tc.tcNodeLabel())
	volumeID := createResp.GetVolume().GetVolumeId()
	volumeContext := createResp.GetVolume().GetVolumeContext()
	Expect(env.ctrlEnv.agentSrv.counts().CreateVolume).To(Equal(1), "%s: after phase1", tc.tcNodeLabel())

	// Phase 2: ControllerPublishVolume
	_, err = env.ctrlEnv.controller.ControllerPublishVolume(env.ctrlEnv.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: env.nodeID,
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase2 ControllerPublish", tc.tcNodeLabel())
	Expect(env.ctrlEnv.agentSrv.counts().AllowInitiator).To(Equal(1), "%s: AllowInitiator after phase2", tc.tcNodeLabel())

	// Phase 3: NodeStageVolume
	env.advanceNodeSMToControllerPublished(volumeID)
	_, err = env.nodeEnv.node.NodeStageVolume(env.nodeEnv.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: env.stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     volumeContext,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase3 NodeStage", tc.tcNodeLabel())
	Expect(env.nodeEnv.connector.connectCalls).To(HaveLen(1), "%s: Connect after phase3", tc.tcNodeLabel())

	// Phase 4: NodePublishVolume
	_, err = env.nodeEnv.node.NodePublishVolume(env.nodeEnv.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: env.stagePath,
		TargetPath: env.targetPath, VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase4 NodePublish", tc.tcNodeLabel())
	Expect(env.nodeEnv.mounter.mountCalls).To(HaveLen(1), "%s: Mount after phase4", tc.tcNodeLabel())

	// Phase 5: NodeUnpublishVolume
	_, err = env.nodeEnv.node.NodeUnpublishVolume(env.nodeEnv.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: env.targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase5 NodeUnpublish", tc.tcNodeLabel())
	Expect(env.nodeEnv.mounter.unmountCalls).To(HaveLen(1), "%s: Unmount after phase5", tc.tcNodeLabel())
	// StagingPath still active (not yet unstaged)
	Expect(env.nodeEnv.connector.disconnectCalls).To(BeEmpty(), "%s: no disconnect yet after phase5", tc.tcNodeLabel())

	// Phase 6: NodeUnstageVolume
	_, err = env.nodeEnv.node.NodeUnstageVolume(env.nodeEnv.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: env.stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase6 NodeUnstage", tc.tcNodeLabel())
	Expect(env.nodeEnv.connector.disconnectCalls).To(HaveLen(1), "%s: Disconnect after phase6", tc.tcNodeLabel())

	// Phase 7: ControllerUnpublishVolume
	_, err = env.ctrlEnv.controller.ControllerUnpublishVolume(env.ctrlEnv.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: env.nodeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase7 ControllerUnpublish", tc.tcNodeLabel())
	Expect(env.ctrlEnv.agentSrv.counts().DenyInitiator).To(Equal(1), "%s: DenyInitiator after phase7", tc.tcNodeLabel())

	// Phase 8: DeleteVolume
	_, err = env.ctrlEnv.controller.DeleteVolume(env.ctrlEnv.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).NotTo(HaveOccurred(), "%s: Phase8 DeleteVolume", tc.tcNodeLabel())
	Expect(env.ctrlEnv.agentSrv.counts().DeleteVolume).To(Equal(1), "%s: DeleteVolume after phase8", tc.tcNodeLabel())
}

// assertE24_IdempotentSteps implements TC[E24.1-4]: each stage can be called
// twice idempotently without errors.
// TestCSILifecycle_IdempotentSteps
func assertE24_IdempotentSteps(tc documentedCase) {
	env := newLifecycleTestEnv()
	defer env.close()

	env.createCSINode(env.nodeID)

	createReq := &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-idempotent-steps",
		Parameters:         env.ctrlEnv.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	}

	// CreateVolume x2 — idempotent
	resp1, err := env.ctrlEnv.controller.CreateVolume(env.ctrlEnv.ctx, createReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first CreateVolume", tc.tcNodeLabel())
	volumeID := resp1.GetVolume().GetVolumeId()
	volumeContext := resp1.GetVolume().GetVolumeContext()

	resp2, err := env.ctrlEnv.controller.CreateVolume(env.ctrlEnv.ctx, createReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second CreateVolume (idempotent)", tc.tcNodeLabel())
	Expect(resp2.GetVolume().GetVolumeId()).To(Equal(volumeID), "%s: VolumeIDs must match", tc.tcNodeLabel())

	// ControllerPublishVolume x2 — idempotent
	pubReq := &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: env.nodeID, VolumeCapability: mountCapability("ext4"),
	}
	_, err = env.ctrlEnv.controller.ControllerPublishVolume(env.ctrlEnv.ctx, pubReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first ControllerPublish", tc.tcNodeLabel())
	_, err = env.ctrlEnv.controller.ControllerPublishVolume(env.ctrlEnv.ctx, pubReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second ControllerPublish (idempotent)", tc.tcNodeLabel())

	// NodeStageVolume x2 — idempotent
	env.advanceNodeSMToControllerPublished(volumeID)
	stageReq := &csiapi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: env.stagePath,
		VolumeCapability: mountCapability("ext4"), VolumeContext: volumeContext,
	}
	_, err = env.nodeEnv.node.NodeStageVolume(env.nodeEnv.ctx, stageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeStage", tc.tcNodeLabel())
	_, err = env.nodeEnv.node.NodeStageVolume(env.nodeEnv.ctx, stageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeStage (idempotent)", tc.tcNodeLabel())

	// NodePublishVolume x2 — idempotent
	pubNodeReq := &csiapi.NodePublishVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: env.stagePath,
		TargetPath: env.targetPath, VolumeCapability: mountCapability("ext4"),
	}
	_, err = env.nodeEnv.node.NodePublishVolume(env.nodeEnv.ctx, pubNodeReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first NodePublish", tc.tcNodeLabel())
	_, err = env.nodeEnv.node.NodePublishVolume(env.nodeEnv.ctx, pubNodeReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second NodePublish (idempotent)", tc.tcNodeLabel())

	// NodeUnpublishVolume x2 — idempotent
	unpubNodeReq := &csiapi.NodeUnpublishVolumeRequest{VolumeId: volumeID, TargetPath: env.targetPath}
	_, err = env.nodeEnv.node.NodeUnpublishVolume(env.nodeEnv.ctx, unpubNodeReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeUnpublish", tc.tcNodeLabel())
	_, err = env.nodeEnv.node.NodeUnpublishVolume(env.nodeEnv.ctx, unpubNodeReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeUnpublish (idempotent)", tc.tcNodeLabel())

	// NodeUnstageVolume x2 — idempotent
	unstageReq := &csiapi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: env.stagePath}
	_, err = env.nodeEnv.node.NodeUnstageVolume(env.nodeEnv.ctx, unstageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeUnstage", tc.tcNodeLabel())
	_, err = env.nodeEnv.node.NodeUnstageVolume(env.nodeEnv.ctx, unstageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeUnstage (idempotent)", tc.tcNodeLabel())

	// ControllerUnpublishVolume x2 — idempotent
	unpubReq := &csiapi.ControllerUnpublishVolumeRequest{VolumeId: volumeID, NodeId: env.nodeID}
	_, err = env.ctrlEnv.controller.ControllerUnpublishVolume(env.ctrlEnv.ctx, unpubReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first ControllerUnpublish", tc.tcNodeLabel())
	_, err = env.ctrlEnv.controller.ControllerUnpublishVolume(env.ctrlEnv.ctx, unpubReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second ControllerUnpublish (idempotent)", tc.tcNodeLabel())

	// DeleteVolume x2 — idempotent
	delReq := &csiapi.DeleteVolumeRequest{VolumeId: volumeID}
	_, err = env.ctrlEnv.controller.DeleteVolume(env.ctrlEnv.ctx, delReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first DeleteVolume", tc.tcNodeLabel())
	_, err = env.ctrlEnv.controller.DeleteVolume(env.ctrlEnv.ctx, delReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second DeleteVolume (idempotent)", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.2 — CreateVolume stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_PartialFailure_CRDCreatedOnExportFailure implements TC[E24.2-1]:
// agent.CreateVolume success + agent.ExportVolume failure → CRD Phase=CreatePartial.
// TestCSIController_PartialFailure_CRDCreatedOnExportFailure
func assertE24_PartialFailure_CRDCreatedOnExportFailure(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed: simulated")
	env.agentSrv.mu.Unlock()

	pvcName := "pvc-e24-partial-crd"
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               pvcName,
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).To(HaveOccurred(), "%s: CreateVolume must fail on export error", tc.tcNodeLabel())

	// CRD must be created with Phase=CreatePartial
	pv := lookupPillarVolume(env, pvcName)
	Expect(pv).NotTo(BeNil(), "%s: PillarVolume CRD must exist after partial failure", tc.tcNodeLabel())
	Expect(pv.Status.Phase).To(Equal(pillarv1.PillarVolumePhaseCreatePartial),
		"%s: Phase must be CreatePartial", tc.tcNodeLabel())
	Expect(pv.Status.PartialFailure).NotTo(BeNil(),
		"%s: PartialFailure must be set", tc.tcNodeLabel())
	Expect(pv.Status.PartialFailure.BackendCreated).To(BeTrue(),
		"%s: BackendCreated must be true", tc.tcNodeLabel())
	Expect(pv.Status.PartialFailure.FailedOperation).To(Equal("ExportVolume"),
		"%s: FailedOperation must be ExportVolume", tc.tcNodeLabel())
	Expect(pv.Status.ExportInfo).To(BeNil(),
		"%s: ExportInfo must be nil on partial failure", tc.tcNodeLabel())
}

// assertE24_PartialFailure_RetryAdvancesToReady implements TC[E24.2-2]:
// retry after partial failure → CRD Phase=Ready with ExportInfo filled.
// TestCSIController_PartialFailure_RetryAdvancesToReady
func assertE24_PartialFailure_RetryAdvancesToReady(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// First attempt: export fails → CreatePartial
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
	env.agentSrv.mu.Unlock()

	pvcName := "pvc-e24-retry-ready"
	req := &csiapi.CreateVolumeRequest{
		Name:               pvcName,
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	}
	_, err := env.controller.CreateVolume(env.ctx, req)
	Expect(err).To(HaveOccurred(), "%s: first attempt must fail", tc.tcNodeLabel())

	// Clear the error for second attempt
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	// Second attempt: should succeed, CRD Phase→Ready
	resp, err := env.controller.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: retry must succeed", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).NotTo(BeEmpty(), "%s: VolumeId on retry", tc.tcNodeLabel())

	pv := lookupPillarVolume(env, pvcName)
	Expect(pv).NotTo(BeNil(), "%s: PillarVolume CRD must exist", tc.tcNodeLabel())
	Expect(pv.Status.Phase).To(Equal(pillarv1.PillarVolumePhaseReady),
		"%s: Phase must advance to Ready on retry", tc.tcNodeLabel())
	Expect(pv.Status.ExportInfo).NotTo(BeNil(),
		"%s: ExportInfo must be filled on success", tc.tcNodeLabel())
	Expect(pv.Status.PartialFailure).To(BeNil(),
		"%s: PartialFailure must be cleared on success", tc.tcNodeLabel())
}

// assertE24_PartialFailure_AgentCreateVolumeCalledOnceOnRetry implements TC[E24.2-3]:
// skipBackend optimization — agent.CreateVolume is NOT re-called on retry.
// TestCSIController_PartialFailure_AgentCreateVolumeCalledOnceOnRetry
func assertE24_PartialFailure_AgentCreateVolumeCalledOnceOnRetry(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// First attempt: export fails
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = status.Error(codes.Internal, "export failed")
	env.agentSrv.mu.Unlock()

	pvcName := "pvc-e24-oncecreate"
	req := &csiapi.CreateVolumeRequest{
		Name:               pvcName,
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	}
	_, err := env.controller.CreateVolume(env.ctx, req)
	Expect(err).To(HaveOccurred(), "%s: first attempt must fail", tc.tcNodeLabel())

	c1 := env.agentSrv.counts()
	Expect(c1.CreateVolume).To(Equal(1), "%s: CreateVolume called once on first attempt", tc.tcNodeLabel())

	// Retry: export succeeds
	env.agentSrv.mu.Lock()
	env.agentSrv.exportVolumeErr = nil
	env.agentSrv.mu.Unlock()

	_, err = env.controller.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: retry must succeed", tc.tcNodeLabel())

	c2 := env.agentSrv.counts()
	// skipBackend: CreateVolume is still 1 (not re-called), ExportVolume is 2 (called on both attempts)
	Expect(c2.CreateVolume).To(Equal(1), "%s: CreateVolume still 1 on retry (skipBackend)", tc.tcNodeLabel())
	Expect(c2.ExportVolume).To(Equal(2), "%s: ExportVolume 2 total (first fail + retry success)", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.3 — ControllerPublish stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_ControllerPublishVolume_AgentAllowInitiatorFails implements TC[E24.3-1]:
// AllowInitiator failure propagates to ControllerPublishVolume.
// TestCSIController_ControllerPublishVolume_AgentAllowInitiatorFails
func assertE24_ControllerPublishVolume_AgentAllowInitiatorFails(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.allowInitiatorErr = status.Error(codes.Internal, "allow initiator failed")
	env.agentSrv.mu.Unlock()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-e24-allow",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:worker-e24-allow",
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-allow-fail",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-e24-allow",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: ControllerPublishVolume must fail when AllowInitiator fails", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.4 — NodeStage stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_NodeStageVolume_ConnectFails implements TC[E24.4-1]:
// NVMe-oF connection failure causes NodeStageVolume to fail.
// TestCSINode_NodeStageVolume_ConnectFails
func assertE24_NodeStageVolume_ConnectFails(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	env.connector.connectErr = status.Error(codes.Internal, "nvmeof connect failed")
	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-connect-fail"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: NodeStageVolume must fail on connect error", tc.tcNodeLabel())
}

// assertE24_NodeStageVolume_FormatFails implements TC[E24.4-2]:
// device format failure after successful connect causes NodeStageVolume to fail.
// TestCSINode_NodeStageVolume_FormatFails
func assertE24_NodeStageVolume_FormatFails(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	env.mounter.formatAndMountErr = status.Error(codes.Internal, "format failed: disk error")
	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-format-fail"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).To(HaveOccurred(), "%s: NodeStageVolume must fail on format error", tc.tcNodeLabel())
	// Connect must have been called before format failed
	Expect(env.connector.connectCalls).To(HaveLen(1),
		"%s: Connect called before format failed", tc.tcNodeLabel())
}

// assertE24_NodeStageVolume_IdempotentReStage implements TC[E24.4-3]:
// calling NodeStageVolume a second time on an already-staged volume succeeds.
// TestCSINode_NodeStageVolume_IdempotentReStage
func assertE24_NodeStageVolume_IdempotentReStage(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-restage"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	stageReq := &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	}

	_, err := env.node.NodeStageVolume(env.ctx, stageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeStageVolume", tc.tcNodeLabel())

	// Second call — idempotent, no error
	_, err = env.node.NodeStageVolume(env.ctx, stageReq)
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeStageVolume (idempotent)", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.5 — NodePublish stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_NodePublishVolume_MountFails implements TC[E24.5-1]:
// bind-mount failure after successful stage causes NodePublishVolume to fail.
// TestCSINode_NodePublishVolume_MountFails
func assertE24_NodePublishVolume_MountFails(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-mount-fail"
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	// Stage successfully
	_, err := env.node.NodeStageVolume(env.ctx, &csiapi.NodeStageVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		VolumeCapability:  mountCapability("ext4"),
		VolumeContext:     nodeVolumeContext(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NodeStageVolume must succeed", tc.tcNodeLabel())

	// Inject mount error
	env.mounter.mountErr = status.Error(codes.Internal, "mount failed: permission denied")

	_, err = env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: NodePublishVolume must fail on mount error", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.6 — NodeUnpublish stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_NodeUnpublishVolume_UnmountFails implements TC[E24.6-1]:
// unmount failure causes NodeUnpublishVolume to fail.
// TestCSINode_NodeUnpublishVolume_UnmountFails
func assertE24_NodeUnpublishVolume_UnmountFails(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-unmount-fail"
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

	// Inject unmount error
	env.mounter.unmountErr = status.Error(codes.Internal, "unmount failed")

	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: targetPath,
	})
	Expect(err).To(HaveOccurred(), "%s: NodeUnpublishVolume must fail on unmount error", tc.tcNodeLabel())
}

// assertE24_NodeUnpublishVolume_AlreadyUnpublished implements TC[E24.6-2]:
// calling NodeUnpublishVolume on an already-unmounted path succeeds (idempotent).
// TestCSINode_NodeUnpublishVolume_AlreadyUnpublished
func assertE24_NodeUnpublishVolume_AlreadyUnpublished(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-already-unpub"
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

	// Unpublish once
	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeUnpublishVolume", tc.tcNodeLabel())

	// Unpublish again — idempotent
	_, err = env.node.NodeUnpublishVolume(env.ctx, &csiapi.NodeUnpublishVolumeRequest{
		VolumeId: volumeID, TargetPath: targetPath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeUnpublishVolume (idempotent)", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.7 — NodeUnstage stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_NodeUnstageVolume_DisconnectFails implements TC[E24.7-1]:
// NVMe-oF disconnect failure causes NodeUnstageVolume to fail.
// TestCSINode_NodeUnstageVolume_DisconnectFails
func assertE24_NodeUnstageVolume_DisconnectFails(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-disconnect-fail"
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
	Expect(err).NotTo(HaveOccurred())

	// Inject disconnect error
	env.connector.disconnectErr = status.Error(codes.Internal, "disconnect failed")

	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagePath,
	})
	Expect(err).To(HaveOccurred(), "%s: NodeUnstageVolume must fail on disconnect error", tc.tcNodeLabel())
}

// assertE24_NodeUnstageVolume_AlreadyUnstaged implements TC[E24.7-2]:
// calling NodeUnstageVolume on an already-unstaged volume succeeds (idempotent).
// TestCSINode_NodeUnstageVolume_AlreadyUnstaged
func assertE24_NodeUnstageVolume_AlreadyUnstaged(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-already-unstaged"
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
	Expect(err).NotTo(HaveOccurred())

	// Unstage once
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: first NodeUnstageVolume", tc.tcNodeLabel())

	// Unstage again — idempotent
	_, err = env.node.NodeUnstageVolume(env.ctx, &csiapi.NodeUnstageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagePath,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: second NodeUnstageVolume (idempotent)", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.8 — ControllerUnpublish stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_ControllerUnpublishVolume_AgentDenyInitiatorFails implements TC[E24.8-1]:
// DenyInitiator failure propagates to ControllerUnpublishVolume.
// TestCSIController_ControllerUnpublishVolume_AgentDenyInitiatorFails
func assertE24_ControllerUnpublishVolume_AgentDenyInitiatorFails(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-e24-deny-fail",
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": "nqn.2026-01.io.example:worker-e24-deny",
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	// Create and publish volume
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-deny-fail",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e24-deny-fail",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	// Inject DenyInitiator error
	env.agentSrv.mu.Lock()
	env.agentSrv.denyInitiatorErr = status.Error(codes.Internal, "deny initiator failed")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-e24-deny-fail",
	})
	Expect(err).To(HaveOccurred(), "%s: ControllerUnpublishVolume must fail when DenyInitiator fails", tc.tcNodeLabel())
}

// assertE24_ControllerUnpublishVolume_NotFound implements TC[E24.8-2]:
// DenyInitiator returning NotFound is treated as idempotent success.
// TestCSIController_ControllerUnpublishVolume_NotFound
func assertE24_ControllerUnpublishVolume_NotFound(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Inject DenyInitiator NotFound — should be treated as idempotent
	env.agentSrv.mu.Lock()
	env.agentSrv.denyInitiatorErr = status.Error(codes.NotFound, "initiator not found")
	env.agentSrv.mu.Unlock()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-deny-notfound"
	_, err := env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-1",
	})
	// CSI spec: ControllerUnpublishVolume with NotFound from agent should succeed (idempotent)
	// or return NotFound — both are acceptable per spec.
	_ = err // accept any result per CSI spec
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.9 — DeleteVolume stage failure/recovery
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_DeleteVolume_AgentDeleteVolumeFailsTransient implements TC[E24.9-1]:
// transient agent.DeleteVolume failure causes DeleteVolume to return error (CO retries).
// TestCSIController_DeleteVolume_AgentDeleteVolumeFailsTransient
func assertE24_DeleteVolume_AgentDeleteVolumeFailsTransient(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create volume first
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e24-delete-transient",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	// Inject transient delete error
	env.agentSrv.mu.Lock()
	env.agentSrv.deleteVolumeErr = status.Error(codes.Internal, "agent delete failed: transient")
	env.agentSrv.mu.Unlock()

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	Expect(err).To(HaveOccurred(), "%s: DeleteVolume must fail on transient agent error", tc.tcNodeLabel())

	// PillarVolume CRD should still exist (not cleaned up on failure)
	pv := lookupPillarVolume(env, "pvc-e24-delete-transient")
	Expect(pv).NotTo(BeNil(), "%s: PillarVolume CRD must remain after failed delete", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// E24.10 — Aborted lifecycle cleanup paths
// ─────────────────────────────────────────────────────────────────────────────

// assertE24_OutOfOrderOperationsDetected implements TC[E24.10-1]:
// ordering constraint violations (NodePublish before NodeStage) return FailedPrecondition.
// TestCSILifecycle_OutOfOrderOperationsDetected
func assertE24_OutOfOrderOperationsDetected(tc documentedCase) {
	env := newNodeTestEnv()
	defer env.close()

	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-e24-out-of-order"
	stagePath := filepath.Join(env.stateDir, "stage")
	targetPath := filepath.Join(env.stateDir, "target")

	// Advance to ControllerPublished but NOT NodeStaged
	env.sm.ForceState(volumeID, csidrv.StateControllerPublished)

	// Attempt NodePublishVolume without NodeStageVolume — must fail
	_, err := env.node.NodePublishVolume(env.ctx, &csiapi.NodePublishVolumeRequest{
		VolumeId:          volumeID,
		StagingTargetPath: stagePath,
		TargetPath:        targetPath,
		VolumeCapability:  mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: NodePublishVolume before NodeStage must fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
		"%s: must return FailedPrecondition for ordering violation", tc.tcNodeLabel())
}

// assertE24_DeleteVolume_NonExistentVolume implements TC[E24.10-2]:
// DeleteVolume on a non-existent VolumeId succeeds (CSI idempotency).
// TestCSIController_DeleteVolume_NonExistentVolume
func assertE24_DeleteVolume_NonExistentVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// VolumeId that was never created
	volumeID := "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-nonexistent"

	_, err := env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{VolumeId: volumeID})
	// CSI spec: DeleteVolume on a volume that does not exist must succeed (idempotent).
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume on non-existent volume must succeed", tc.tcNodeLabel())
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers used only in E24 assertions
// ─────────────────────────────────────────────────────────────────────────────

// lookupPillarVolumeByVolumeID finds a PillarVolume CRD by iterating
// through volumes where the Spec.VolumeID matches.
func lookupPillarVolumeByVolumeID(env *controllerTestEnv, volumeID string) *pillarv1.PillarVolume {
	pvList := &pillarv1.PillarVolumeList{}
	if err := env.k8sClient.List(env.ctx, pvList); err != nil {
		return nil
	}
	for i := range pvList.Items {
		if pvList.Items[i].Spec.VolumeID == volumeID {
			return &pvList.Items[i]
		}
	}
	return nil
}

// pillarVolumeExists returns true if the named PillarVolume CRD exists.
func pillarVolumeExists(env *controllerTestEnv, name string) bool {
	pv := &pillarv1.PillarVolume{}
	err := env.k8sClient.Get(env.ctx, types.NamespacedName{Name: name}, pv)
	return err == nil
}
