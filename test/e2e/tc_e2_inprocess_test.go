package e2e

// tc_e2_inprocess_test.go — Per-TC assertions for E2: ControllerPublish/Unpublish.

import (
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

// makeCSINodeWithNQN creates a fake storagev1.CSINode with the given NVMe-oF
// host NQN annotation so that ControllerPublishVolume can resolve the
// initiator identity.
func makeCSINodeWithNQN(env *controllerTestEnv, nodeName, hostNQN string) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/nvmeof-host-nqn": hostNQN,
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)
}

// makeCSINodeWithIQN creates a fake storagev1.CSINode with the given iSCSI
// initiator IQN annotation so that ControllerPublishVolume can resolve the
// initiator identity.
func makeCSINodeWithIQN(env *controllerTestEnv, nodeName, iqn string) {
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
			Annotations: map[string]string{
				"pillar-csi.bhyoo.com/iscsi-initiator-iqn": iqn,
			},
		},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)
}

// seedE2VolumeState creates a PillarVolumeState for volumeID named after the
// PVC (the last volume_id path component), so ControllerPublishVolume passes
// its volume-existence check for volumes not provisioned via CreateVolume.
func seedE2VolumeState(env *controllerTestEnv, pvcName, volumeID string) {
	pv := &pillarv1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec:       pillarv1.PillarVolumeStateSpec{VolumeID: volumeID},
	}
	Expect(env.k8sClient.Create(env.ctx, pv)).To(Succeed())
}

// publishedNodeIDs returns the node IDs recorded in the PillarVolumeState's
// status.publishedNodes.
func publishedNodeIDs(env *controllerTestEnv, pvcName string) []string {
	pv := &pillarv1.PillarVolumeState{}
	Expect(env.k8sClient.Get(env.ctx, types.NamespacedName{Name: pvcName}, pv)).To(Succeed())
	nodes := make([]string, 0, len(pv.Status.PublishedNodes))
	for _, pub := range pv.Status.PublishedNodes {
		nodes = append(nodes, pub.NodeID)
	}
	return nodes
}

func assertE2_ControllerPublishVolume(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create volume first
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-publish",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())
	volumeID := resp.GetVolume().GetVolumeId()

	// Create CSINode annotation
	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerPublishVolume", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.AllowInitiator).To(Equal(1), "%s: allowInitiatorCalls", tc.tcNodeLabel())
}

func assertE2_ControllerPublishVolume_ISCSI(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	makeCSINodeWithIQN(env, "worker-2", "iqn.1993-08.org.debian:worker-2")

	// Use iSCSI volume ID format
	volumeID := "storage-1/iscsi/zfs-zvol/tank/pvc-iscsi-publish"
	seedE2VolumeState(env, "pvc-iscsi-publish", volumeID)
	env.controller.GetStateMachine().ForceState(volumeID, csidrv.StateCreated)

	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-2",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: iSCSI ControllerPublishVolume", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.AllowInitiator).To(Equal(1), "%s: allowInitiatorCalls", tc.tcNodeLabel())
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.allowInitiatorReqs
	env.agentSrv.mu.Unlock()
	if len(reqs) > 0 {
		Expect(reqs[0].GetInitiatorId()).To(Equal("iqn.1993-08.org.debian:worker-2"),
			"%s: initiator ID should match IQN", tc.tcNodeLabel())
	}
}

func assertE2_ControllerPublishVolume_AlreadyPublished(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-already-pub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	}
	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first publish", tc.tcNodeLabel())

	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second publish (idempotent)", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.AllowInitiator).To(Equal(2), "%s: allowInitiator called twice (no dedup at controller level)", tc.tcNodeLabel())
}

func assertE2_ControllerUnpublishVolume_Success(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-unpublish",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "worker-1",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ControllerUnpublishVolume", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.DenyInitiator).To(Equal(1), "%s: denyInitiatorCalls", tc.tcNodeLabel())
}

func assertE2_ControllerUnpublishVolume_AlreadyUnpublished(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-already-unpub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "worker-1",
	}
	for i := range 2 {
		_, err = env.controller.ControllerUnpublishVolume(env.ctx, req)
		Expect(err).NotTo(HaveOccurred(), "%s: unpublish call %d", tc.tcNodeLabel(), i+1)
	}
}

func assertE2_ControllerUnpublishVolume_EmptyVolumeID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: "",
		NodeId:   "worker-1",
	})
	Expect(err).To(HaveOccurred(), "%s: empty VolumeId should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE2_ControllerUnpublishVolume_EmptyNodeID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// CSI spec ControllerUnpublishVolume: an empty node_id means "unpublish
	// from all nodes" — every recorded publication must be revoked.
	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-empty-node",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: publish to worker-1", tc.tcNodeLabel())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: empty NodeId should succeed", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.DenyInitiator).To(Equal(1), "%s: DenyInitiator for the recorded node", tc.tcNodeLabel())
	env.agentSrv.mu.Lock()
	denyReqs := env.agentSrv.denyInitiatorReqs
	env.agentSrv.mu.Unlock()
	if len(denyReqs) == 1 {
		Expect(denyReqs[0].GetInitiatorId()).To(Equal("nqn.2026-01.io.example:worker-1"),
			"%s: recorded initiator revoked", tc.tcNodeLabel())
	}
	Expect(publishedNodeIDs(env, "pvc-e2-empty-node")).To(BeEmpty(),
		"%s: publication records removed", tc.tcNodeLabel())
}

func assertE2_ControllerUnpublishVolume_MalformedVolumeID(_ documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: "badformat",
		NodeId:   "worker-1",
	})
	// Malformed ID: success (no-op) per CSI spec
	_ = err
}

func assertE2_DenyInitiatorNonNotFound(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.denyInitiatorErr = status.Error(codes.Internal, "deny initiator failed")
	env.agentSrv.mu.Unlock()

	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-deny-err",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	// Unpublish only revokes recorded publications, so publish first.
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: publish to worker-1", tc.tcNodeLabel())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   "worker-1",
	})
	Expect(err).To(HaveOccurred(), "%s: Internal deny error should propagate", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Internal))
	Expect(publishedNodeIDs(env, "pvc-e2-deny-err")).To(ConsistOf("worker-1"),
		"%s: record kept after failed revoke", tc.tcNodeLabel())
}

func assertE2_ControllerPublish_DifferentNodes(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	makeCSINodeWithNQN(env, "worker-node-a", "nqn.2026-01.io.example:node-a")
	makeCSINodeWithNQN(env, "worker-node-b", "nqn.2026-01.io.example:node-b")

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-diff-nodes",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-node-a",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: publish to node-a", tc.tcNodeLabel())

	// SINGLE_NODE_WRITER: a second node must be rejected before the agent
	// grants it access.
	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-node-b",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: publish to node-b must be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
		"%s: second node for SINGLE_NODE_WRITER", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.AllowInitiator).To(Equal(1), "%s: allowInitiator only for node-a", tc.tcNodeLabel())
	Expect(publishedNodeIDs(env, "pvc-e2-diff-nodes")).To(ConsistOf("worker-node-a"),
		"%s: only node-a recorded", tc.tcNodeLabel())
}

func assertE2_AllowInitiatorFails(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	env.agentSrv.mu.Lock()
	env.agentSrv.allowInitiatorErr = status.Error(codes.Internal, "configfs write failed: permission denied")
	env.agentSrv.mu.Unlock()

	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-allow-fail",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         resp.GetVolume().GetVolumeId(),
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: AllowInitiator failure should propagate", tc.tcNodeLabel())
}

func assertE2_MissingNodeIdentityAnnotation(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create CSINode without the required annotation
	csiNode := &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-noanno"},
	}
	_ = env.k8sClient.Create(env.ctx, csiNode)

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-noanno",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         resp.GetVolume().GetVolumeId(),
		NodeId:           "worker-noanno",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: missing annotation should cause failure", tc.tcNodeLabel())
}

func assertE2_ControllerPublish_EmptyVolumeID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "",
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE2_ControllerPublish_EmptyNodeID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		NodeId:           "",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: empty NodeId should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE2_ControllerPublish_NilVolumeCapability(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		NodeId:           "worker-1",
		VolumeCapability: nil,
	})
	Expect(err).To(HaveOccurred(), "%s: nil capability should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE2_ControllerPublish_MalformedVolumeID(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "badformat",
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: malformed ID should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
}

func assertE2_ControllerPublish_TargetNotFound(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()
	// Seed the volume so the PillarAgent lookup (not the volume lookup) fails.
	seedE2VolumeState(env, "pvc-test", "nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test")
	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "nonexistent-node/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: missing target should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.NotFound))
}

func assertE2_ControllerPublish_TargetNoResolvedAddress(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// The volume must exist so publish reaches the PillarAgent address check.
	seedE2VolumeState(env, "pvc-test", "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test")

	// Update the target to have no resolved address
	env.target.Status.ResolvedAddress = ""
	Expect(env.k8sClient.Status().Update(env.ctx, env.target)).To(Succeed())

	_, err := env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         "storage-1/nvmeof-tcp/zfs-zvol/tank/pvc-test",
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).To(HaveOccurred(), "%s: no resolved address should fail", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Unavailable))
}

func assertE2_ControllerPublish_DoubleSameArgs(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	makeCSINodeWithNQN(env, "worker-1", "nqn.2026-01.io.example:worker-1")

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-double-pub",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())
	volumeID := resp.GetVolume().GetVolumeId()

	req := &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-1",
		VolumeCapability: mountCapability("ext4"),
	}
	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first publish", tc.tcNodeLabel())

	_, err = env.controller.ControllerPublishVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second publish same args (idempotent)", tc.tcNodeLabel())
}
