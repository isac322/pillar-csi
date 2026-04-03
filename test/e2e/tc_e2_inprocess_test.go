package e2e

// tc_e2_inprocess_test.go — Per-TC assertions for E2: ControllerPublish/Unpublish.

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	// CSI spec §4.3.4: empty NodeId = "all nodes" = no-op
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-empty-node",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
		NodeId:   "",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: empty NodeId should succeed (no-op)", tc.tcNodeLabel())
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

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e2-deny-err",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred())

	_, err = env.controller.ControllerUnpublishVolume(env.ctx, &csiapi.ControllerUnpublishVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
		NodeId:   "worker-1",
	})
	Expect(err).To(HaveOccurred(), "%s: Internal deny error should propagate", tc.tcNodeLabel())
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

	_, err = env.controller.ControllerPublishVolume(env.ctx, &csiapi.ControllerPublishVolumeRequest{
		VolumeId:         volumeID,
		NodeId:           "worker-node-b",
		VolumeCapability: mountCapability("ext4"),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: publish to node-b", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.AllowInitiator).To(Equal(2), "%s: allowInitiator called for each node", tc.tcNodeLabel())

	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.allowInitiatorReqs
	env.agentSrv.mu.Unlock()
	if len(reqs) == 2 {
		Expect(reqs[0].GetInitiatorId()).NotTo(Equal(reqs[1].GetInitiatorId()),
			"%s: different nodes must have different initiator IDs", tc.tcNodeLabel())
	}
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
