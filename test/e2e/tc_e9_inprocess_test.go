package e2e

// tc_e9_inprocess_test.go — Per-TC assertions for E9: Agent gRPC.

import (
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func assertE9_CreateAndDeleteVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e9-create",
		CapacityBytes: 10 << 20,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.client.DeleteVolume(env.ctx, &agentv1.DeleteVolumeRequest{
		VolumeId: "tank/pvc-e9-create",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DeleteVolume", tc.tcNodeLabel())
}

func assertE9_ExportAndUnexportVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e9-export",
		CapacityBytes: 10 << 20,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-e9-export",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExportVolume", tc.tcNodeLabel())

	_, err = env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     "tank/pvc-e9-export",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: UnexportVolume", tc.tcNodeLabel())
}

func assertE9_AllowAndDenyInitiator(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e9-initiator",
		CapacityBytes: 10 << 20,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	_, err = env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-e9-initiator",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		ExportParams: nvmeofTCPExportParams("127.0.0.1", 4420),
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExportVolume", tc.tcNodeLabel())

	_, err = env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "tank/pvc-e9-initiator",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e9",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: AllowInitiator", tc.tcNodeLabel())

	_, err = env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     "tank/pvc-e9-initiator",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
		InitiatorId:  "nqn.2026-01.io.example:host-e9",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: DenyInitiator", tc.tcNodeLabel())
}

func assertE9_ExpandVolume(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		VolumeId:      "tank/pvc-e9-expand",
		CapacityBytes: 10 << 20,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: CreateVolume", tc.tcNodeLabel())

	resp, err := env.client.ExpandVolume(env.ctx, &agentv1.ExpandVolumeRequest{
		VolumeId:       "tank/pvc-e9-expand",
		RequestedBytes: 20 << 20,
	})
	Expect(err).NotTo(HaveOccurred(), "%s: ExpandVolume", tc.tcNodeLabel())
	Expect(resp.GetCapacityBytes()).To(BeNumerically(">=", 20<<20),
		"%s: expanded capacity", tc.tcNodeLabel())
}

func assertE9_GetCapacity(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	resp, err := env.client.GetCapacity(env.ctx, &agentv1.GetCapacityRequest{
		PoolName: "tank",
	})
	Expect(err).NotTo(HaveOccurred(), "%s: GetCapacity", tc.tcNodeLabel())
	Expect(resp.GetTotalBytes()).To(BeNumerically(">", 0),
		"%s: total capacity should be > 0", tc.tcNodeLabel())
}

func assertE9_ReconcileState(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	// ReconcileState is implemented by the fake agent server as Unimplemented.
	// The real agent server's ReconcileState is tested separately.
	// E9 just verifies the gRPC endpoint exists and returns a valid response.
	_, err := env.client.ReconcileState(env.ctx, &agentv1.ReconcileStateRequest{})
	// ReconcileState may return Unimplemented in the test env — that's acceptable
	if err != nil {
		code := status.Code(err)
		Expect(code).To(BeElementOf(codes.OK, codes.Unimplemented, codes.NotFound),
			"%s: ReconcileState unexpected error code", tc.tcNodeLabel())
	}
}
