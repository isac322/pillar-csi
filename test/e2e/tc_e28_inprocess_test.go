package e2e

// tc_e28_inprocess_test.go — Per-TC assertions for E28: LVM Agent gRPC.

import (
	"sync"

	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// Helper to create an LVM volume via agentTestEnv (using data-vg pool).
func lvmCreateVolume(env *agentTestEnv, volumeID string, capacityBytes int64) error {
	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/" + volumeID,
		CapacityBytes: capacityBytes,
	})
	return err
}

func assertE28_LVM_CreateVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-create",
		CapacityBytes: 1 << 30,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume", tc.tcNodeLabel())

	log := env.lvmBackend.CallLog()
	Expect(log).To(ContainElement(ContainSubstring("Create")),
		"%s: backend Create should be called", tc.tcNodeLabel())
}

func assertE28_LVM_DeleteVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-delete", 1<<30)).To(Succeed(),
		"%s: CreateVolume before delete", tc.tcNodeLabel())

	_, err := env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: "data-vg/pvc-e28-delete",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM DeleteVolume", tc.tcNodeLabel())
}

func assertE28_LVM_CreateVolume_Idempotency(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	req := &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-idem",
		CapacityBytes: 1 << 30,
	}
	_, err := env.client.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first CreateVolume", tc.tcNodeLabel())

	_, err = env.client.CreateVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second CreateVolume (idempotent)", tc.tcNodeLabel())
}

func assertE28_LVM_CreateVolume_Conflict(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// Create with one size
	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-conflict",
		CapacityBytes: 1 << 30,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: first CreateVolume", tc.tcNodeLabel())

	// Create with different size — should return AlreadyExists or Internal
	_, err = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-conflict",
		CapacityBytes: 2 << 30,
	})
	Expect(err).To(HaveOccurred(), "%s: expected conflict error", tc.tcNodeLabel())
	Expect(status.Code(err)).To(BeElementOf(codes.AlreadyExists, codes.Internal),
		"%s: expected AlreadyExists or Internal for conflict", tc.tcNodeLabel())
}

func assertE28_LVM_ExpandVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-expand", 1<<30)).To(Succeed(),
		"%s: CreateVolume", tc.tcNodeLabel())

	resp, err := env.client.ExpandVolume(env.ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       "data-vg/pvc-e28-expand",
		RequestedBytes: 2 << 30,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ExpandVolume", tc.tcNodeLabel())
	Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", 2<<30),
		"%s: expanded capacity", tc.tcNodeLabel())
}

func assertE28_LVM_Capacity(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: "data-vg",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM GetCapacity", tc.tcNodeLabel())
	Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
		"%s: total capacity > 0", tc.tcNodeLabel())
}

func assertE28_LVM_ListVolumes(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-list-a", 1<<30)).To(Succeed())
	Expect(lvmCreateVolume(env, "pvc-e28-list-b", 1<<30)).To(Succeed())

	resp, err := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{
		PoolName: "data-vg",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ListVolumes", tc.tcNodeLabel())
	Expect(resp.GetVolumes()).To(HaveLen(2),
		"%s: should list 2 volumes", tc.tcNodeLabel())
}

func assertE28_LVM_ExportVolume_NVMeOF(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-export-nvmeof", 1<<30)).To(Succeed())

	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-export-nvmeof",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ExportVolume NVMe-oF", tc.tcNodeLabel())
}

func assertE28_LVM_ExportVolume_Idempotency(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-export-idem", 1<<30)).To(Succeed())

	req := &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-export-idem",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	}
	_, err := env.client.ExportVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: first ExportVolume", tc.tcNodeLabel())

	_, err = env.client.ExportVolume(env.ctx, req)
	Expect(err).NotTo(HaveOccurred(), "%s: second ExportVolume (idempotent)", tc.tcNodeLabel())
}

func assertE28_LVM_UnexportVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-unexport", 1<<30)).To(Succeed())

	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-unexport",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})

	_, err := env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-unexport",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM UnexportVolume", tc.tcNodeLabel())
}

func assertE28_LVM_AllowInitiator(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-allow", 1<<30)).To(Succeed())
	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-allow",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})

	_, err := env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-allow",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e28",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM AllowInitiator", tc.tcNodeLabel())
}

func assertE28_LVM_DenyInitiator(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-deny", 1<<30)).To(Succeed())
	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-deny",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	_, _ = env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-deny",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e28-deny",
	})

	_, err := env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-deny",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e28-deny",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM DenyInitiator", tc.tcNodeLabel())
}

func assertE28_LVM_FullLifecycle(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	const volumeID = "data-vg/pvc-e28-full"

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      volumeID,
		CapacityBytes: 1 << 30,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExportVolume", tc.tcNodeLabel())

	_, err = env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e28-full",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: AllowInitiator", tc.tcNodeLabel())

	_, err = env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e28-full",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DenyInitiator", tc.tcNodeLabel())

	_, err = env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     volumeID,
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: UnexportVolume", tc.tcNodeLabel())

	_, err = env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())
}

func assertE28_LVM_CreateVolume_BackendError(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	env.lvmBackend.SetError("Create", status.Error(codes.Internal, "LVM backend error"))

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-backend-err",
		CapacityBytes: 1 << 30,
	})
	Expect(err).To(HaveOccurred(), "%s: expected backend error", tc.tcNodeLabel())
}

func assertE28_LVM_DeleteVolume_NotFound(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// Delete a non-existent volume — should be idempotent (no error or NotFound)
	_, err := env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: "data-vg/pvc-e28-not-found",
	})
	// NotFound is treated as success for delete
	if err != nil {
		Expect(status.Code(err)).To(BeElementOf(codes.NotFound),
			"%s: expected NotFound for non-existent volume", tc.tcNodeLabel())
	}
}

func assertE28_LVM_ExpandVolume_BackendError(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-expand-err", 1<<30)).To(Succeed())

	env.lvmBackend.SetError("Expand", status.Error(codes.Internal, "expand error"))

	_, err := env.client.ExpandVolume(env.ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       "data-vg/pvc-e28-expand-err",
		RequestedBytes: 2 << 30,
	})
	Expect(err).To(HaveOccurred(), "%s: expected backend expand error", tc.tcNodeLabel())
}

func assertE28_LVM_GetCapacity(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: "data-vg",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM GetCapacity", tc.tcNodeLabel())
	Expect(resp.GetAvailableBytes()).To(BeNumerically(">", 0),
		"%s: available capacity > 0", tc.tcNodeLabel())
}

func assertE28_LVM_GetCapacity_Error(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	env.lvmBackend.SetError("Capacity", status.Error(codes.Internal, "capacity error"))

	_, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: "data-vg",
	})
	Expect(err).To(HaveOccurred(), "%s: expected capacity error", tc.tcNodeLabel())
}

func assertE28_LVM_ExportVolume_Configfs(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-configfs-export", 1<<30)).To(Succeed())

	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-configfs-export",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM ExportVolume via configfs", tc.tcNodeLabel())
}

func assertE28_LVM_AllowInitiator_Configfs(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-allow-configfs", 1<<30)).To(Succeed())
	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-allow-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})

	_, err := env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-allow-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-configfs",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: AllowInitiator via configfs", tc.tcNodeLabel())
}

func assertE28_LVM_DenyInitiator_Configfs(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-deny-configfs", 1<<30)).To(Succeed())
	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-deny-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	_, _ = env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-deny-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-deny-configfs",
	})

	_, err := env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     "data-vg/pvc-e28-deny-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-deny-configfs",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DenyInitiator via configfs", tc.tcNodeLabel())
}

func assertE28_LVM_UnexportVolume_Configfs(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-unexport-configfs", 1<<30)).To(Succeed())
	_, _ = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-unexport-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})

	_, err := env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-unexport-configfs",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: UnexportVolume via configfs", tc.tcNodeLabel())
}

func assertE28_LVM_CreateVolume_LVMParams(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "data-vg/pvc-e28-lvm-params",
		CapacityBytes: 1 << 30,
		BackendParams: &agentv1.BackendParams{
			Params: &agentv1.BackendParams_Lvm{
				Lvm: &agentv1.LvmVolumeParams{
					VolumeGroup:   "data-vg",
					ProvisionMode: "thick",
				},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM CreateVolume with params", tc.tcNodeLabel())
}

func assertE28_LVM_MultiVolume_Isolation(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-iso-a", 1<<30)).To(Succeed())
	Expect(lvmCreateVolume(env, "pvc-e28-iso-b", 2<<30)).To(Succeed())

	// Both volumes should exist independently
	resp, err := env.client.ListVolumes(env.ctx, &agentv1.ListVolumesRequest{PoolName: "data-vg"})
	Expect(err).NotTo(HaveOccurred(), "%s: ListVolumes", tc.tcNodeLabel())
	Expect(resp.GetVolumes()).To(HaveLen(2), "%s: two volumes should exist", tc.tcNodeLabel())
}

func assertE28_LVM_CreateVolume_Capacity_Check(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{PoolName: "data-vg"})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity before create", tc.tcNodeLabel())
	initialAvail := resp.GetAvailableBytes()

	Expect(lvmCreateVolume(env, "pvc-e28-cap-check", 1<<30)).To(Succeed())

	resp2, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{PoolName: "data-vg"})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity after create", tc.tcNodeLabel())
	Expect(resp2.GetAvailableBytes()).To(BeNumerically("<", initialAvail),
		"%s: available capacity should decrease after create", tc.tcNodeLabel())
}

func assertE28_LVM_ExpandVolume_NotFound(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.ExpandVolume(env.ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       "data-vg/pvc-e28-expand-notfound",
		RequestedBytes: 2 << 30,
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for non-existent volume", tc.tcNodeLabel())
}

func assertE28_LVM_HealthCheck(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.HealthCheck(env.ctx, &agentv1.HealthCheckRequest{})
	Expect(err).NotTo(HaveOccurred(), "%s: HealthCheck", tc.tcNodeLabel())
	Expect(resp.GetHealthy()).To(BeTrue(), "%s: agent should be healthy", tc.tcNodeLabel())
}

func assertE28_LVM_ExportVolume_iSCSI_Unimplemented(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-iscsi-unimp", 1<<30)).To(Succeed())

	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-iscsi-unimp",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI,
	})
	Expect(err).To(HaveOccurred(), "%s: iSCSI export should be unimplemented", tc.tcNodeLabel())
	Expect(status.Code(err)).To(BeElementOf(codes.Unimplemented, codes.InvalidArgument),
		"%s: expected Unimplemented or InvalidArgument", tc.tcNodeLabel())
}

func assertE28_LVM_ExportVolume_NVMeOF_DeviceCheck(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	Expect(lvmCreateVolume(env, "pvc-e28-dev-check", 1<<30)).To(Succeed())

	// Device should be present (AlwaysPresentChecker is used in agentTestEnv)
	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "data-vg/pvc-e28-dev-check",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExportVolume with device check", tc.tcNodeLabel())
}

func assertE28_LVM_Concurrent_CreateVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	const parallelism = 5
	var wg sync.WaitGroup
	errs := make([]error, parallelism)

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
				VolumeId:      "data-vg/pvc-e28-concurrent-" + string(rune('a'+idx)),
				CapacityBytes: 1 << 30,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		Expect(err).NotTo(HaveOccurred(), "%s: concurrent LVM create %d failed", tc.tcNodeLabel(), i)
	}
}

func assertE28_LVM_Concurrent_ExportVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// Create one volume, export it multiple times concurrently (idempotency)
	Expect(lvmCreateVolume(env, "pvc-e28-concurrent-export", 1<<30)).To(Succeed())

	const parallelism = 3
	var wg sync.WaitGroup
	errs := make([]error, parallelism)

	for i := 0; i < parallelism; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
				VolumeId:     "data-vg/pvc-e28-concurrent-export",
				ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
				ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
			})
		}(i)
	}
	wg.Wait()

	// At least one should succeed
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	Expect(successCount).To(BeNumerically(">=", 1),
		"%s: at least one concurrent export should succeed", tc.tcNodeLabel())
}
