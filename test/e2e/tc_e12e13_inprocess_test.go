package e2e

// tc_e12e13_inprocess_test.go — Per-TC assertions for E12 (snapshot not implemented) and E13 (clone source).

import (
	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── E12: Snapshot / ValidateVolumeCapabilities not implemented ────────────────

func assertE12_NotImplemented(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// CreateSnapshot is not implemented — verify it returns Unimplemented
	_, err := env.controller.CreateSnapshot(env.ctx, &csiapi.CreateSnapshotRequest{
		SourceVolumeId: "some-volume",
		Name:           "snap-1",
	})
	Expect(err).To(HaveOccurred(), "%s: expected Unimplemented for CreateSnapshot", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Unimplemented),
		"%s: expected Unimplemented, got %v", tc.tcNodeLabel(), status.Code(err))
}

func assertE12_CreateReturnsUnimplemented(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateSnapshot(env.ctx, &csiapi.CreateSnapshotRequest{
		SourceVolumeId: "vol-1",
		Name:           "snap-create",
	})
	Expect(err).To(HaveOccurred(), "%s: CreateSnapshot should be unimplemented", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Unimplemented),
		"%s: CreateSnapshot code", tc.tcNodeLabel())
}

func assertE12_DeleteReturnsUnimplemented(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.DeleteSnapshot(env.ctx, &csiapi.DeleteSnapshotRequest{
		SnapshotId: "snap-1",
	})
	Expect(err).To(HaveOccurred(), "%s: DeleteSnapshot should be unimplemented", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Unimplemented),
		"%s: DeleteSnapshot code", tc.tcNodeLabel())
}

func assertE12_ListReturnsUnimplemented(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.ListSnapshots(env.ctx, &csiapi.ListSnapshotsRequest{})
	Expect(err).To(HaveOccurred(), "%s: ListSnapshots should be unimplemented", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.Unimplemented),
		"%s: ListSnapshots code", tc.tcNodeLabel())
}

// ── E13: Volume content source (clone) — unsupported source rejection ────────
//
// pillar-csi does not advertise CREATE_DELETE_SNAPSHOT or CLONE_VOLUME, so a
// CreateVolume request carrying any volume_content_source MUST be rejected
// with codes.InvalidArgument per CSI spec §5.1.1.  A success would mean an
// empty volume was silently provisioned in place of the requested source.

func assertE13_CreateVolume_SnapshotSourceRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e13-snap-restore",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
		VolumeContentSource: &csiapi.VolumeContentSource{
			Type: &csiapi.VolumeContentSource_Snapshot{
				Snapshot: &csiapi.VolumeContentSource_SnapshotSource{
					SnapshotId: "snap-A",
				},
			},
		},
	})
	Expect(err).To(HaveOccurred(), "%s: snapshot source must be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: snapshot source unexpected error code", tc.tcNodeLabel())

	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(0), "%s: agent.CreateVolume must not be called", tc.tcNodeLabel())
	Expect(c.ExportVolume).To(Equal(0), "%s: agent.ExportVolume must not be called", tc.tcNodeLabel())
}

func assertE13_CreateVolume_VolumeSourceRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create a source volume first so the clone request references a real ID.
	sourceResp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e13-source",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume source", tc.tcNodeLabel())
	sourceID := sourceResp.GetVolume().GetVolumeId()
	Expect(sourceID).NotTo(BeEmpty(), "%s: source volume ID", tc.tcNodeLabel())

	_, err = env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e13-clone",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
		VolumeContentSource: &csiapi.VolumeContentSource{
			Type: &csiapi.VolumeContentSource_Volume{
				Volume: &csiapi.VolumeContentSource_VolumeSource{
					VolumeId: sourceID,
				},
			},
		},
	})
	Expect(err).To(HaveOccurred(), "%s: clone source must be rejected", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: clone source unexpected error code", tc.tcNodeLabel())

	// Only the initial ordinary create may have reached the agent.
	c := env.agentSrv.counts()
	Expect(c.CreateVolume).To(Equal(1), "%s: agent.CreateVolume calls", tc.tcNodeLabel())
	Expect(c.ExportVolume).To(Equal(1), "%s: agent.ExportVolume calls", tc.tcNodeLabel())
}
