package e2e

// tc_e22_inprocess_test.go — Per-TC assertions for E22: incompatible or
// unsupported backend/protocol selections.
//
// Backend and protocol are selected by the PillarStore / PillarProtocol a
// StorageClass references; only the zfs and lvm backend members and the
// nvmeofTcp protocol member exist.  A hand-written StorageClass may add
// backend / protocol override documents, and every selection of a removed
// variant (iscsi, nfs, zfs-dataset, dir, …) or a flat legacy key must be
// rejected explicitly with InvalidArgument before the agent is called.

import (
	"strings"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
)

// expectE22Rejected issues CreateVolume with params and asserts an
// InvalidArgument whose message contains every fragment, and that the agent
// was never asked to create a volume.
func expectE22Rejected(tc documentedCase, env *controllerTestEnv, name string, params map[string]string, fragments ...string) {
	GinkgoHelper()
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               name,
		Parameters:         params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).To(HaveOccurred(), "%s: expected rejection", tc.tcNodeLabel())
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument, got %v", tc.tcNodeLabel(), err)
	for _, fragment := range fragments {
		Expect(err.Error()).To(ContainSubstring(fragment), "%s", tc.tcNodeLabel())
	}
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call for a rejected selection", tc.tcNodeLabel())
}

// e22HandWrittenWithDoc returns the default hand-written StorageClass
// parameters plus one override document.
func e22HandWrittenWithDoc(env *controllerTestEnv, docKey, doc string) map[string]string {
	params := copyParams(env.params)
	params[docKey] = doc
	return params
}

func assertE22_CreateVolume_ProtocolDoc_ISCSIRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	expectE22Rejected(tc, env, "pvc-e22-iscsi",
		e22HandWrittenWithDoc(env, e2eDocProtocol, "iscsi:\n  port: 3260\n"),
		e2eDocProtocol, `unknown field "iscsi"`)
}

func assertE22_CreateVolume_ProtocolDoc_NFSRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	expectE22Rejected(tc, env, "pvc-e22-nfs",
		e22HandWrittenWithDoc(env, e2eDocProtocol, "nfs:\n  version: \"4.2\"\n"),
		e2eDocProtocol, `unknown field "nfs"`)
}

func assertE22_CreateVolume_LegacyProtocolTypeParamRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	params := copyParams(env.params)
	params["pillar-csi.bhyoo.com/protocol-type"] = "smb-v3-unknown"
	expectE22Rejected(tc, env, "pvc-e22-legacy-protocol-type", params,
		"unsupported StorageClass parameter", "pillar-csi.bhyoo.com/protocol-type")
}

func assertE22_CreateVolume_ProtocolDoc_StructuralFieldRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// acl is the security anchor of the PillarProtocol: it is structural and
	// may not be changed by a per-class or per-volume document.
	expectE22Rejected(tc, env, "pvc-e22-structural-acl",
		e22HandWrittenWithDoc(env, e2eDocProtocol, "nvmeofTcp:\n  acl: true\n"),
		"nvmeofTcp.acl is structural and cannot be set per volume")
}

func assertE22_CreateVolume_BackendDoc_RemovedVariantRejected(tc documentedCase) {
	for _, member := range []string{"zfs-dataset", "dir"} {
		env := newControllerTestEnv()
		expectE22Rejected(tc, env, "pvc-e22-removed-backend",
			e22HandWrittenWithDoc(env, e2eDocBackend, member+":\n  properties: {}\n"),
			e2eDocBackend, `unknown field "`+member+`"`)
		env.close()
	}
}

func assertE22_CreateVolume_BackendDoc_MemberMismatchRejected(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	// The default store is ZFS: an lvm override document does not match it.
	_, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-backend-mismatch",
		Parameters:         e22HandWrittenWithDoc(env, e2eDocBackend, "lvm:\n  provisioningMode: thin\n"),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(status.Code(err)).To(Equal(codes.InvalidArgument),
		"%s: expected InvalidArgument, got %v", tc.tcNodeLabel(), err)
	Expect(strings.Contains(err.Error(), "lvm") && strings.Contains(err.Error(), "zfs")).To(BeTrue(),
		"%s: the rejection must name both the override member and the store backend: %v", tc.tcNodeLabel(), err)
	env.agentSrv.mu.Lock()
	reqs := env.agentSrv.createVolumeReqs
	env.agentSrv.mu.Unlock()
	Expect(reqs).To(BeEmpty(), "%s: no agent call for a mismatched override", tc.tcNodeLabel())
}

func assertE22_CreateVolume_NVMeOF_TCP(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-nvmeof-tcp",
		Parameters:         env.params,
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: NVMe-oF TCP CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(ContainSubstring("/nvmeof-tcp/"),
		"%s: volume ID should contain protocol", tc.tcNodeLabel())
}

func assertE22_CreateVolume_LVMBackend_NVMeOF(tc documentedCase) {
	env := newControllerTestEnv()
	defer env.close()

	Expect(env.k8sClient.Create(env.ctx, e2eLVMStore("lvm-store", env.target.Name, "data-vg", "", ""))).To(Succeed())
	resp, err := env.controller.CreateVolume(env.ctx, &csiapi.CreateVolumeRequest{
		Name:               "pvc-e22-lvm-nvmeof",
		Parameters:         e2eHandWrittenParams("lvm-store", e2eDefaultProtocolName),
		VolumeCapabilities: []*csiapi.VolumeCapability{mountCapability("ext4")},
	})
	Expect(err).NotTo(HaveOccurred(), "%s: LVM+NVMe-oF CreateVolume", tc.tcNodeLabel())
	Expect(resp.GetVolume().GetVolumeId()).To(Equal("storage-1/nvmeof-tcp/lvm-lv/data-vg/pvc-e22-lvm-nvmeof"),
		"%s: volume ID should name the lvm-lv backend and the volume group", tc.tcNodeLabel())
}

func assertE22_AgentErrors_Export_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	fence := agentLifecycleFence("tank/pvc-e22-export-proto")
	_, _ = env.client.CreateVolume(env.ctx, &agentv1.CreateVolumeRequest{
		BackendType:   agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL,
		VolumeId:      "tank/pvc-e22-export-proto",
		CapacityBytes: 10 << 20,
		Fence:         fence,
	})

	// Use an invalid protocol type
	_, err := env.client.ExportVolume(env.ctx, &agentv1.ExportVolumeRequest{
		VolumeId:     "tank/pvc-e22-export-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		ExportParams: nil,
		Fence:        fence,
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in ExportVolume", tc.tcNodeLabel())
}

func assertE22_AgentErrors_Unexport_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.UnexportVolume(env.ctx, &agentv1.UnexportVolumeRequest{
		VolumeId:     "tank/pvc-e22-unexport-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		Fence:        agentLifecycleFence("tank/pvc-e22-unexport-proto"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in UnexportVolume", tc.tcNodeLabel())
}

func assertE22_AgentProtocol_AllowInitiator_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.AllowInitiator(env.ctx, &agentv1.AllowInitiatorRequest{
		VolumeId:     "tank/pvc-e22-allow-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		InitiatorId:  "nqn.2026-01.io.example:host",
		Fence:        agentLifecycleFence("tank/pvc-e22-allow-proto"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in AllowInitiator", tc.tcNodeLabel())
}

func assertE22_AgentProtocol_DenyInitiator_InvalidProtocol(tc documentedCase) {
	env := newAgentTestEnv()
	defer env.close()

	_, err := env.client.DenyInitiator(env.ctx, &agentv1.DenyInitiatorRequest{
		VolumeId:     "tank/pvc-e22-deny-proto",
		ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_UNSPECIFIED,
		InitiatorId:  "nqn.2026-01.io.example:host",
		Fence:        agentLifecycleFence("tank/pvc-e22-deny-proto"),
	})
	Expect(err).To(HaveOccurred(), "%s: expected error for invalid protocol in DenyInitiator", tc.tcNodeLabel())
}
