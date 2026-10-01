/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const testISCSIProtocolName = "iscsi-proto"

// iscsiProtocol is a PillarProtocol with the iscsi member.
func iscsiProtocol(cfg *v1alpha1.ISCSIConfig) *v1alpha1.PillarProtocol {
	return &v1alpha1.PillarProtocol{
		Name: testISCSIProtocolName,
		Spec: v1alpha1.PillarProtocolSpec{Protocol: v1alpha1.ProtocolSpec{ISCSI: cfg}},
	}
}

// iscsiExportResponse is the agent ExportVolume answer for an iSCSI target.
func iscsiExportResponse() *agentv1.ExportVolumeResponse {
	return &agentv1.ExportVolumeResponse{ExportInfo: &agentv1.ExportInfo{
		TargetId:  "iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc123",
		Address:   "192.168.1.10",
		Port:      3260,
		VolumeRef: "0",
	}}
}

// TestCreateVolume_ISCSI verifies that an iscsi PillarProtocol provisions a
// volume routed as iscsi end to end: the volume ID token, the agent protocol
// enum and IscsiExportParams (default port, ACL), the durable exportSpec, and
// the iSCSI session tuning handed to the node — PVC-document overrides on top
// of the protocol, unset timeouts left to the node defaults.
func TestCreateVolume_ISCSI(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, iscsiProtocol(&v1alpha1.ISCSIConfig{
		ACL:             true,
		LoginTimeout:    testInt32(20),
		NoopOutInterval: testInt32(10),
	}))
	env.agent.exportVolumeResp = iscsiExportResponse()
	req := baseCreateVolumeRequest()
	req.Parameters[paramProtocolRef] = testISCSIProtocolName
	req.Parameters[paramProtocolDoc] = "iscsi:\n  loginTimeout: 30\n  replacementTimeout: 0\n"

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	const wantID = "storage-node-1/iscsi/zfs-zvol/tank/pvc-abc123"
	if got := resp.GetVolume().GetVolumeId(); got != wantID {
		t.Errorf("VolumeId = %q, want %q", got, wantID)
	}

	assertISCSIExportRequest(t, env.agent.lastExportVolumeReq)
	assertISCSIVolumeContext(t, resp.GetVolume().GetVolumeContext())

	pvs, _, err := env.srv.loadPillarVolumeState(context.Background(), req.GetName())
	if err != nil {
		t.Fatalf("load PillarVolumeState: %v", err)
	}
	if pvs.Spec.ProtocolType != "iscsi" {
		t.Errorf("spec.protocolType = %q, want iscsi", pvs.Spec.ProtocolType)
	}
	if spec := pvs.Status.ExportSpec; spec == nil || !spec.ACLEnabled || spec.Port != 3260 ||
		spec.BindAddress != "192.168.1.10" {
		t.Errorf("status.exportSpec = %+v, want aclEnabled / 192.168.1.10:3260", spec)
	}
}

// assertISCSIExportRequest checks the agent ExportVolume request of
// TestCreateVolume_ISCSI: iscsi protocol enum, default port, ACL enabled.
func assertISCSIExportRequest(t *testing.T, exp *agentv1.ExportVolumeRequest) {
	t.Helper()
	if exp.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI {
		t.Errorf("ExportVolume protocol = %v, want PROTOCOL_TYPE_ISCSI", exp.GetProtocolType())
	}
	params := exp.GetExportParams().GetIscsi()
	if params == nil || params.GetPort() != 3260 || params.GetBindAddress() != "192.168.1.10" {
		t.Errorf("ExportVolume iscsi params = %+v, want bind 192.168.1.10 port 3260 (default)", params)
	}
	if !exp.GetAclEnabled() {
		t.Error("ExportVolume AclEnabled = false, want true from iscsi.acl")
	}
}

// assertISCSIVolumeContext checks the VolumeContext of TestCreateVolume_ISCSI:
// PVC-document overrides on top of the protocol, unset timeouts absent, and
// no NVMe-oF keys.
func assertISCSIVolumeContext(t *testing.T, vc map[string]string) {
	t.Helper()
	for key, want := range map[string]string{
		VolumeContextKeyTargetID:                "iqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-abc123",
		VolumeContextKeyPort:                    "3260",
		vcProtocolType:                          "iscsi",
		VolumeContextKeyISCSILoginTimeout:       "30",
		VolumeContextKeyISCSIReplacementTimeout: "0",
		VolumeContextKeyISCSINoopOutInterval:    "10",
	} {
		if got := vc[key]; got != want {
			t.Errorf("VolumeContext[%s] = %q, want %q", key, got, want)
		}
	}
	if v, ok := vc[VolumeContextKeyISCSINoopOutTimeout]; ok {
		t.Errorf("VolumeContext[%s] = %q, want absent (unset keeps the node default)",
			VolumeContextKeyISCSINoopOutTimeout, v)
	}
	for _, k := range []string{paramNVMeOFCtrlLossTmo, paramNVMeOFReconnectDelay, paramNVMeOFMaxQueueSize} {
		if _, ok := vc[k]; ok {
			t.Errorf("VolumeContext carries NVMe-oF key %s for an iscsi volume", k)
		}
	}
}

// TestCreateVolume_ProtocolOverrideMemberMismatch verifies that an override
// document for one protocol member is rejected against a protocol of the
// other member, before any agent call.
func TestCreateVolume_ProtocolOverrideMemberMismatch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		protocolRef string
		doc         string
		fragment    string
	}{
		"iscsi document on nvmeof protocol": {
			protocolRef: testProtocolName,
			doc:         "iscsi:\n  loginTimeout: 30\n",
			fragment:    "iscsi overrides do not apply to a nvmeof-tcp protocol",
		},
		"nvmeofTcp document on iscsi protocol": {
			protocolRef: testISCSIProtocolName,
			doc:         "nvmeofTcp:\n  ctrlLossTmo: 30\n",
			fragment:    "nvmeofTcp overrides do not apply to a iscsi protocol",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t, iscsiProtocol(&v1alpha1.ISCSIConfig{}))
			req := baseCreateVolumeRequest()
			req.Parameters[paramProtocolRef] = tc.protocolRef
			req.Parameters[paramProtocolDoc] = tc.doc
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), tc.fragment) {
				t.Fatalf("CreateVolume err = %v, want InvalidArgument containing %q", err, tc.fragment)
			}
			if env.agent.createVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
				t.Error("agent contacted despite mismatched protocol document")
			}
		})
	}
}

// TestControllerPublishVolume_ISCSIResolvesInitiatorIQN verifies that
// publishing an iscsi volume with ACL on grants the node's initiator IQN from
// the CSINode iSCSI annotation, never the NVMe-oF host NQN of the same node.
func TestControllerPublishVolume_ISCSIResolvesInitiatorIQN(t *testing.T) {
	t.Parallel()

	const initiatorIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.0123456789abcdef0123456789abcdef"
	req := basePublishRequest()
	req.VolumeId = "storage-node-1/iscsi/zfs-zvol/tank/pvc-iscsi"
	pvs := volumeStateFor(req.GetVolumeId())
	pvs.Status.ExportSpec = &v1alpha1.VolumeExportSpec{BindAddress: "192.168.1.10", Port: 3260, ACLEnabled: true}
	csiNode := &storagev1.CSINode{
		Name: req.GetNodeId(),
		Annotations: map[string]string{
			AnnotationNVMeOFHostNQN:     "nqn.2014-08.org.nvmexpress:uuid:w1",
			AnnotationISCSIInitiatorIQN: initiatorIQN,
		},
	}
	env := newPublishTestEnv(t, csiNode, pvs)

	if _, err := env.srv.ControllerPublishVolume(context.Background(), req); err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	allow := env.agent.lastAllowInitiator
	if env.agent.allowInitiatorCalls != 1 || allow == nil {
		t.Fatalf("AllowInitiator calls = %d, want 1", env.agent.allowInitiatorCalls)
	}
	if allow.GetInitiatorId() != initiatorIQN {
		t.Errorf("AllowInitiator.InitiatorId = %q, want %q", allow.GetInitiatorId(), initiatorIQN)
	}
	if allow.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI {
		t.Errorf("AllowInitiator.ProtocolType = %v, want PROTOCOL_TYPE_ISCSI", allow.GetProtocolType())
	}
}

// TestControllerPublishVolume_ISCSIMissingIQNAnnotation verifies that an
// iscsi publish to a node that has only published its NVMe-oF identity fails
// with FailedPrecondition naming the iSCSI annotation, so the external
// attacher retries until the node plugin publishes its IQN.
func TestControllerPublishVolume_ISCSIMissingIQNAnnotation(t *testing.T) {
	t.Parallel()

	req := basePublishRequest()
	req.VolumeId = "storage-node-1/iscsi/zfs-zvol/tank/pvc-iscsi"
	pvs := volumeStateFor(req.GetVolumeId())
	pvs.Status.ExportSpec = &v1alpha1.VolumeExportSpec{BindAddress: "192.168.1.10", Port: 3260, ACLEnabled: true}
	csiNode := &storagev1.CSINode{
		Name:        req.GetNodeId(),
		Annotations: map[string]string{AnnotationNVMeOFHostNQN: "nqn.2014-08.org.nvmexpress:uuid:w1"},
	}
	env := newPublishTestEnv(t, csiNode, pvs)

	_, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), AnnotationISCSIInitiatorIQN) {
		t.Fatalf("ControllerPublishVolume err = %v, want FailedPrecondition naming %s", err, AnnotationISCSIInitiatorIQN)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0", env.agent.allowInitiatorCalls)
	}
}

// TestDesiredVolumeState_ISCSI verifies that the export resync rebuilds an
// iscsi volume's target from its durable exportSpec: the iSCSI protocol and
// portal (not NVMe-oF parameters) and the ACL of the published initiators.
func TestDesiredVolumeState_ISCSI(t *testing.T) {
	t.Parallel()

	const initiatorIQN = "iqn.2026-01.com.bhyoo.pillar-csi:node.w1"
	pvs := volumeStateFor("storage-node-1/iscsi/zfs-zvol/tank/pvc-iscsi",
		v1alpha1.VolumePublication{NodeID: "worker-node-1", InitiatorID: initiatorIQN})
	pvs.UID = "uid-iscsi"
	pvs.Status.ExportSpec = exportSpecFor(&agentv1.ExportParams{Params: &agentv1.ExportParams_Iscsi{
		Iscsi: &agentv1.IscsiExportParams{BindAddress: "192.168.1.10", Port: 3261},
	}}, true)

	desired, err := desiredVolumeState(pvs, nil)
	if err != nil {
		t.Fatalf("desiredVolumeState: %v", err)
	}
	if len(desired.GetExports()) != 1 {
		t.Fatalf("exports = %v, want one", desired.GetExports())
	}
	exp := desired.GetExports()[0]
	if exp.GetProtocolType() != agentv1.ProtocolType_PROTOCOL_TYPE_ISCSI {
		t.Errorf("export protocol = %v, want PROTOCOL_TYPE_ISCSI", exp.GetProtocolType())
	}
	p := exp.GetExportParams().GetIscsi()
	if p == nil || p.GetBindAddress() != "192.168.1.10" || p.GetPort() != 3261 {
		t.Errorf("export params = %+v, want iscsi 192.168.1.10:3261", exp.GetExportParams())
	}
	if !exp.GetAclEnabled() || len(exp.GetAllowedInitiators()) != 1 || exp.GetAllowedInitiators()[0] != initiatorIQN {
		t.Errorf("export acl=%v initiators=%v, want ACL with [%s]",
			exp.GetAclEnabled(), exp.GetAllowedInitiators(), initiatorIQN)
	}
}
