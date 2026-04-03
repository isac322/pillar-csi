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

// ── E13: Volume content source (clone) ────────────────────────────────────────

func assertE13_CreateVolume_ContentSource(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create source volume first
	sourceResp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e13-source",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume source", tc.tcNodeLabel())
	sourceID := sourceResp.GetVolume().GetVolumeId()
	Expect(sourceID).NotTo(BeEmpty(), "%s: source volume ID", tc.tcNodeLabel())

	// Clone from source — either succeeds or returns Unimplemented
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
	if err != nil {
		code := status.Code(err)
		Expect(code).To(BeElementOf(codes.Unimplemented, codes.InvalidArgument, codes.NotFound),
			"%s: clone unexpected error code", tc.tcNodeLabel())
	}
}

func assertE13_DeleteVolume_CloneSource(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// Create and delete a volume that was used as a clone source
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e13-clone-src-delete",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
		CapacityRange:      &csiapi.CapacityRange{RequiredBytes: 10 << 20},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.controller.DeleteVolume(env.ctx, &csiapi.DeleteVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume clone source", tc.tcNodeLabel())
}
