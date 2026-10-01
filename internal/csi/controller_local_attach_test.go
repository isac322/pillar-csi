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
	"maps"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

const localAttachSNW = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER

// nodeRefAgent is the spec of an in-cluster PillarAgent running on node.
func nodeRefAgent(node string) v1alpha1.PillarAgentSpec {
	return v1alpha1.PillarAgentSpec{NodeRef: &v1alpha1.NodeRefSpec{Name: node}}
}

// setAgentSpec replaces the spec of the publish environment's PillarAgent
// (storage-node-1); its resolved address is kept.
func setAgentSpec(t *testing.T, env *controllerTestEnv, spec v1alpha1.PillarAgentSpec) {
	t.Helper()
	ctx := context.Background()
	agent := &v1alpha1.PillarAgent{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: "storage-node-1"}, agent); err != nil {
		t.Fatalf("get PillarAgent: %v", err)
	}
	agent.Spec = spec
	if err := env.srv.k8sClient.Update(ctx, agent); err != nil {
		t.Fatalf("update PillarAgent: %v", err)
	}
}

// localAttachVolumeState is a Ready volume with an ACL-enforcing export whose
// resolution recorded localAttach.
func localAttachVolumeState(
	volumeID string,
	localAttach bool,
	pubs ...v1alpha1.VolumePublication,
) *v1alpha1.PillarVolumeState {
	pvs := volumeStateFor(volumeID, pubs...)
	pvs.Spec.Resolved = &v1alpha1.ResolvedVolumeConfig{LocalAttach: localAttach}
	pvs.Status.ExportSpec = &v1alpha1.VolumeExportSpec{BindAddress: "192.168.1.10", Port: 4420, ACLEnabled: true}
	return pvs
}

// localPub is the publication record of a local attach on the storage node.
func localPub() v1alpha1.VolumePublication {
	return v1alpha1.VolumePublication{
		NodeID:      exclNode1,
		InitiatorID: exclNode1,
		AccessMode:  localAttachSNW.String(),
		Local:       true,
	}
}

func loadVolumeState(t *testing.T, env *controllerTestEnv, volumeID string) *v1alpha1.PillarVolumeState {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Name: pillarVolumeStateNameFromVolumeID(volumeID)}, pvs); err != nil {
		t.Fatalf("get PillarVolumeState: %v", err)
	}
	return pvs
}

// TestControllerPublishVolume_LocalAttachDecision verifies which publishes
// attach locally: only a localAttach volume published with a SINGLE_NODE_*
// mode to the node its in-cluster agent runs on.  Every other publish keeps
// today's protocol path (CSINode initiator, AllowInitiator, no PublishContext
// keys); a local publish needs no CSINode identity, records a local
// publication together with status.localAttachNode, fences the export with the
// committed token and hands the agent's device path to the node.
func TestControllerPublishVolume_LocalAttachDecision(t *testing.T) {
	t.Parallel()

	external := v1alpha1.PillarAgentSpec{External: &v1alpha1.ExternalSpec{Address: "192.168.1.10", Port: 9500}}
	tests := []struct {
		name        string
		localAttach bool
		agent       v1alpha1.PillarAgentSpec
		mode        csi.VolumeCapability_AccessMode_Mode
		readonly    bool
		wantLocal   bool
	}{
		{"flag off on the storage node", false, nodeRefAgent(exclNode1), localAttachSNW, false, false},
		{"flag on, another node", true, nodeRefAgent(exclNode2), localAttachSNW, false, false},
		{"flag on, external agent", true, external, localAttachSNW, false, false},
		{"flag on, multi-node mode on the storage node", true, nodeRefAgent(exclNode1),
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, true, false},
		{"flag on, storage node", true, nodeRefAgent(exclNode1), localAttachSNW, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			volumeID := basePublishRequest().GetVolumeId()
			objs := []ctrlclient.Object{localAttachVolumeState(volumeID, tc.localAttach)}
			if !tc.wantLocal {
				objs = append(objs, exclCSINodes()...)
			}
			env := newPublishTestEnv(t, objs...)
			setAgentSpec(t, env, tc.agent)

			resp, err := env.srv.ControllerPublishVolume(context.Background(),
				exclPublishReq(volumeID, exclNode1, tc.mode, tc.readonly))
			if err != nil {
				t.Fatalf("ControllerPublishVolume: %v", err)
			}
			pvs := loadVolumeState(t, env, volumeID)
			pubs := pvs.Status.PublishedNodes
			if len(pubs) != 1 {
				t.Fatalf("publishedNodes = %+v, want one record", pubs)
			}
			if tc.wantLocal {
				requireLocalPublish(t, env, resp, pvs)
			} else {
				requireProtocolPublish(t, env, resp, pvs)
			}
		})
	}
}

// requireProtocolPublish asserts a publish recorded exactly one publication
// used the protocol path: empty PublishContext and AllowInitiator granted
// with the node's NQN, no status.localAttachNode.  A localAttach volume is
// additionally re-enabled once (SetLocalAttach local=false at the
// reservation fence, ordered before the grant) whether or not it was ever
// attached locally; other volumes send no SetLocalAttach at all.
func requireProtocolPublish(
	t *testing.T, env *controllerTestEnv,
	resp *csi.ControllerPublishVolumeResponse, pvs *v1alpha1.PillarVolumeState,
) {
	t.Helper()
	if len(resp.GetPublishContext()) != 0 {
		t.Errorf("PublishContext = %v, want empty (protocol attach)", resp.GetPublishContext())
	}
	wantCalls := 0
	if r := pvs.Spec.Resolved; r != nil && r.LocalAttach {
		wantCalls = 1
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != wantCalls {
		t.Fatalf("SetLocalAttach calls = %d, want %d", len(calls), wantCalls)
	}
	if wantCalls == 1 {
		call := calls[0]
		if call.GetLocal() {
			t.Errorf("SetLocalAttach request = %+v, want local=false (re-enable)", call)
		}
		fence := call.GetFence()
		if fence.GetVolumeUid() != string(pvs.UID) ||
			fence.GetGeneration() != generationOf(pvs) {
			t.Errorf("SetLocalAttach fence = %+v, want the reservation (uid %s, generation %d)",
				fence, pvs.UID, pvs.Status.PublicationGeneration)
		}
		if order := env.agent.callOrder; !slices.Equal(order, []string{"SetLocalAttach", "AllowInitiator"}) {
			t.Errorf("agent call order = %v, want SetLocalAttach before AllowInitiator", order)
		}
	}
	if env.agent.allowInitiatorCalls != 1 {
		t.Errorf("AllowInitiator calls = %d, want 1", env.agent.allowInitiatorCalls)
	}
	if pub := pvs.Status.PublishedNodes[0]; pub.Local || pub.InitiatorID != exclNQN(exclNode1) {
		t.Errorf("publication = %+v, want protocol record with the node's NQN", pub)
	}
	if pvs.Status.LocalAttachNode != "" {
		t.Errorf("localAttachNode = %q, want empty", pvs.Status.LocalAttachNode)
	}
}

// requireLocalPublish asserts a publish recorded exactly one publication
// attached locally: the PublishContext carries attach-mode local, the node's
// name and the agent's device path; no initiator is granted, the export is
// fenced with the committed token and status.localAttachNode is set.
func requireLocalPublish(
	t *testing.T, env *controllerTestEnv,
	resp *csi.ControllerPublishVolumeResponse, pvs *v1alpha1.PillarVolumeState,
) {
	t.Helper()
	want := map[string]string{
		PublishContextKeyAttachMode:      AttachModeLocal,
		PublishContextKeyLocalNode:       exclNode1,
		PublishContextKeyLocalDevicePath: "/dev/zvol/tank/pvc-abc123",
	}
	if !maps.Equal(resp.GetPublishContext(), want) {
		t.Errorf("PublishContext = %v, want %v", resp.GetPublishContext(), want)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0 for a local attach", env.agent.allowInitiatorCalls)
	}
	if n := len(env.agent.setLocalAttachCalls); n != 1 {
		t.Fatalf("SetLocalAttach calls = %d, want 1", n)
	}
	call := env.agent.setLocalAttachCalls[0]
	if !call.GetLocal() || call.GetVolumeId() != "tank/pvc-abc123" {
		t.Errorf("SetLocalAttach request = %+v, want local=true for tank/pvc-abc123", call)
	}
	fence := call.GetFence()
	generation := uint64(pvs.Status.PublicationGeneration) //nolint:gosec // G115: test generations are small and positive
	if fence.GetVolumeUid() != string(pvs.UID) || fence.GetGeneration() != generation {
		t.Errorf("SetLocalAttach fence = %+v, want the committed token (uid %s, generation %d)",
			fence, pvs.UID, pvs.Status.PublicationGeneration)
	}
	if pubs := pvs.Status.PublishedNodes; pubs[0] != localPub() {
		t.Errorf("publication = %+v, want %+v", pubs[0], localPub())
	}
	if pvs.Status.LocalAttachNode != exclNode1 {
		t.Errorf("localAttachNode = %q, want %q", pvs.Status.LocalAttachNode, exclNode1)
	}
}

// TestControllerPublishVolume_LocalAttachRetryIdempotent verifies a retried
// local publish returns the same PublishContext, keeps a single record and
// re-fences the export with a newer token.
func TestControllerPublishVolume_LocalAttachRetryIdempotent(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	env := newPublishTestEnv(t, localAttachVolumeState(volumeID, true))
	setAgentSpec(t, env, nodeRefAgent(exclNode1))
	req := exclPublishReq(volumeID, exclNode1, localAttachSNW, false)

	first, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	second, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("retried publish: %v", err)
	}
	if !maps.Equal(first.GetPublishContext(), second.GetPublishContext()) {
		t.Errorf("retried PublishContext = %v, want %v", second.GetPublishContext(), first.GetPublishContext())
	}
	if pubs := exclPublishedNodes(t, env.srv.k8sClient, volumeID); len(pubs) != 1 {
		t.Errorf("publishedNodes = %+v, want one record", pubs)
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != 2 {
		t.Fatalf("SetLocalAttach calls = %d, want 2", len(calls))
	}
	if calls[1].GetFence().GetGeneration() <= calls[0].GetFence().GetGeneration() {
		t.Errorf("retry fence generation %d, want newer than %d",
			calls[1].GetFence().GetGeneration(), calls[0].GetFence().GetGeneration())
	}
}

// remoteAfterLocalEnv seeds a volume whose export is still fenced for a local
// attach on the storage node (status.localAttachNode set, no publication
// left) and returns the environment with a protocol publish request for the
// other node.
func remoteAfterLocalEnv(t *testing.T) (*controllerTestEnv, *csi.ControllerPublishVolumeRequest) {
	t.Helper()
	volumeID := basePublishRequest().GetVolumeId()
	pvs := localAttachVolumeState(volumeID, true)
	pvs.Status.LocalAttachNode = exclNode1
	env := newPublishTestEnv(t, append(exclCSINodes(), pvs)...)
	setAgentSpec(t, env, nodeRefAgent(exclNode1))
	return env, exclPublishReq(volumeID, exclNode2, localAttachSNW, false)
}

// generationOf is the publication generation of pvs as a fencing generation.
func generationOf(pvs *v1alpha1.PillarVolumeState) uint64 {
	return uint64(pvs.Status.PublicationGeneration) //nolint:gosec // G115: test generations are small and positive
}

// TestControllerPublishVolume_RemoteAfterLocalGateRefused verifies the gate of
// a protocol publish of a volume whose export is still fenced for a local
// attach: while the storage node still holds the device the agent's
// FailedPrecondition reaches the CO, no initiator is granted, and neither
// status.localAttachNode nor the publication generation moves beyond the
// reservation the gate was fenced with.
func TestControllerPublishVolume_RemoteAfterLocalGateRefused(t *testing.T) {
	t.Parallel()
	env, req := remoteAfterLocalEnv(t)
	env.agent.setLocalAttachErr = status.Error(codes.FailedPrecondition,
		"backend device /dev/zvol/tank/pvc-abc123 is still held on the storage node (local attach in use)")

	_, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("publish while device held: code = %v (err=%v), want FailedPrecondition", status.Code(err), err)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0 while the export is still fenced", env.agent.allowInitiatorCalls)
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != 1 || calls[0].GetLocal() {
		t.Fatalf("SetLocalAttach calls = %+v, want only the local=false gate", calls)
	}
	pvs := loadVolumeState(t, env, req.GetVolumeId())
	if pvs.Status.LocalAttachNode != exclNode1 {
		t.Errorf("localAttachNode = %q after refused unfence, want %q", pvs.Status.LocalAttachNode, exclNode1)
	}
	if got, gate := generationOf(pvs), calls[0].GetFence().GetGeneration(); got != gate {
		t.Errorf("publicationGeneration = %d after refused unfence, want the gate's reservation %d", got, gate)
	}
}

// TestControllerPublishVolume_RemoteAfterLocal verifies a protocol publish of
// a volume whose export is still fenced for a local attach once the agent
// accepts the unfence: the export is re-enabled at the reservation's
// generation G, status.localAttachNode is cleared under exactly one further
// bump to G+1, the export is re-enabled again at G+1 (so the agent's fence
// passes every generation that recorded the field) and the initiator is
// granted at G+1.
func TestControllerPublishVolume_RemoteAfterLocal(t *testing.T) {
	t.Parallel()
	env, req := remoteAfterLocalEnv(t)
	before := generationOf(loadVolumeState(t, env, req.GetVolumeId()))

	resp, err := env.srv.ControllerPublishVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("publish after device released: %v", err)
	}
	if len(resp.GetPublishContext()) != 0 {
		t.Errorf("PublishContext = %v, want empty (protocol attach)", resp.GetPublishContext())
	}
	pvs := loadVolumeState(t, env, req.GetVolumeId())
	if pvs.Status.LocalAttachNode != "" {
		t.Errorf("localAttachNode = %q after successful unfence, want cleared", pvs.Status.LocalAttachNode)
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != 2 || calls[0].GetLocal() || calls[1].GetLocal() {
		t.Fatalf("SetLocalAttach calls = %+v, want two local=false requests (gate, then re-fence)", calls)
	}
	gate, refence := calls[0].GetFence(), calls[1].GetFence()
	if gate.GetGeneration() != before+1 || refence.GetGeneration() != before+2 {
		t.Errorf("SetLocalAttach generations = %d, %d; want reservation %d then clear %d",
			gate.GetGeneration(), refence.GetGeneration(), before+1, before+2)
	}
	if got := generationOf(pvs); got != refence.GetGeneration() {
		t.Errorf("publicationGeneration = %d, want the re-fence's committed %d", got, refence.GetGeneration())
	}
	if gate.GetVolumeUid() != string(pvs.UID) || refence.GetVolumeUid() != string(pvs.UID) {
		t.Errorf("SetLocalAttach fences %+v / %+v, want lifecycle %s", gate, refence, pvs.UID)
	}
	grant := env.agent.lastAllowInitiator
	if env.agent.allowInitiatorCalls != 1 || grant.GetInitiatorId() != exclNQN(exclNode2) {
		t.Fatalf("AllowInitiator calls = %d (last %+v), want one for %s",
			env.agent.allowInitiatorCalls, grant, exclNQN(exclNode2))
	}
	if got := grant.GetFence().GetGeneration(); got != refence.GetGeneration() {
		t.Errorf("AllowInitiator fence generation %d, want the re-fence's %d", got, refence.GetGeneration())
	}
}

// TestControllerPublishVolume_RemoteAfterLocalClearedRetry verifies the
// retry of a remote publish whose previous attempt already committed the
// status.localAttachNode clear: the field is empty, so there is no gate and
// no further clear, but a localAttach volume still issues exactly one
// SetLocalAttach(local=false) — ordered before the grant — at its own
// reservation, which is newer than every generation that recorded the
// field.  A stale resync admitted just before the failed attempt's re-fence
// may have left the namespace disabled; without this call the publish would
// grant — or, with ACL off, succeed without any agent RPC — while the
// export still refuses the initiator.
func TestControllerPublishVolume_RemoteAfterLocalClearedRetry(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	pvs := localAttachVolumeState(volumeID, true, exclPub(exclNode2, exclNQN(exclNode2), localAttachSNW, false))
	pvs.Status.PublicationGeneration = 7
	env := newPublishTestEnv(t, append(exclCSINodes(), pvs)...)
	setAgentSpec(t, env, nodeRefAgent(exclNode1))

	_, err := env.srv.ControllerPublishVolume(context.Background(),
		exclPublishReq(volumeID, exclNode2, localAttachSNW, false))
	if err != nil {
		t.Fatalf("retried publish: %v", err)
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != 1 || calls[0].GetLocal() {
		t.Fatalf("SetLocalAttach calls = %+v, want exactly one local=false re-enable", calls)
	}
	if got := calls[0].GetFence().GetGeneration(); got != 8 {
		t.Errorf("SetLocalAttach fence generation = %d, want the reservation 8", got)
	}
	if order := env.agent.callOrder; !slices.Equal(order, []string{"SetLocalAttach", "AllowInitiator"}) {
		t.Errorf("agent call order = %v, want the unfence before the grant", order)
	}
	if env.agent.allowInitiatorCalls != 1 {
		t.Fatalf("AllowInitiator calls = %d, want 1", env.agent.allowInitiatorCalls)
	}
	final := loadVolumeState(t, env, volumeID)
	if final.Status.PublicationGeneration != 8 {
		t.Errorf("publicationGeneration = %d, want 8 (reservation only)", final.Status.PublicationGeneration)
	}
	if got := env.agent.lastAllowInitiator.GetFence().GetGeneration(); got != 8 {
		t.Errorf("AllowInitiator fence generation = %d, want 8", got)
	}
}

// TestControllerPublishVolume_RemoteLocalAttachACLOff verifies the unfence
// of a localAttach volume is not tied to granting an initiator: a protocol
// publish of a volume whose export enforces no per-host ACL sends no
// AllowInitiator yet still issues SetLocalAttach(local=false) at the
// reservation.  Otherwise a namespace disabled by a delayed stale resync
// could stay disabled while the publish reports success without any agent
// RPC at all.
func TestControllerPublishVolume_RemoteLocalAttachACLOff(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	pvs := localAttachVolumeState(volumeID, true)
	pvs.Status.ExportSpec.ACLEnabled = false
	pvs.Status.PublicationGeneration = 5
	env := newPublishTestEnv(t, append(exclCSINodes(), pvs)...)
	setAgentSpec(t, env, nodeRefAgent(exclNode1))

	_, err := env.srv.ControllerPublishVolume(context.Background(),
		exclPublishReq(volumeID, exclNode2, localAttachSNW, false))
	if err != nil {
		t.Fatalf("publish of an ACL-off localAttach volume: %v", err)
	}
	calls := env.agent.setLocalAttachCalls
	if len(calls) != 1 || calls[0].GetLocal() {
		t.Fatalf("SetLocalAttach calls = %+v, want exactly one local=false re-enable", calls)
	}
	if got := calls[0].GetFence().GetGeneration(); got != 6 {
		t.Errorf("SetLocalAttach fence generation = %d, want the reservation 6", got)
	}
	if got := calls[0].GetFence().GetVolumeUid(); got != string(pvs.UID) {
		t.Errorf("SetLocalAttach fence uid = %q, want lifecycle %s", got, pvs.UID)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Errorf("AllowInitiator calls = %d, want 0 for an allow-any-host export",
			env.agent.allowInitiatorCalls)
	}
}

// TestControllerPublishVolume_LocalAndProtocolSameNodeConflict verifies a
// node holding a local publication cannot also be published over the
// protocol (and vice versa) without an unpublish in between.
func TestControllerPublishVolume_LocalAndProtocolSameNodeConflict(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	env := newPublishTestEnv(t, append(exclCSINodes(),
		localAttachVolumeState(volumeID, true, exclPub(exclNode1, exclNQN(exclNode1), localAttachSNW, false)))...)
	setAgentSpec(t, env, nodeRefAgent(exclNode1))

	_, err := env.srv.ControllerPublishVolume(context.Background(),
		exclPublishReq(volumeID, exclNode1, localAttachSNW, false))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("local publish over a protocol record: code = %v (err=%v), want FailedPrecondition",
			status.Code(err), err)
	}
	if n := len(env.agent.setLocalAttachCalls); n != 0 {
		t.Errorf("SetLocalAttach calls = %d, want 0 for a rejected publish", n)
	}
	if got := loadVolumeState(t, env, volumeID).Status.LocalAttachNode; got != "" {
		t.Errorf("localAttachNode = %q, want unset by a rejected publish", got)
	}
}

// TestControllerUnpublishVolume_LocalSkipsDeny verifies unpublishing a local
// attach releases the record without DenyInitiator (nothing was granted) and
// keeps status.localAttachNode, so the export stays fenced until a protocol
// publish has the agent re-enable it.
func TestControllerUnpublishVolume_LocalSkipsDeny(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	pvs := localAttachVolumeState(volumeID, true, localPub())
	pvs.Status.LocalAttachNode = exclNode1
	env := newPublishTestEnv(t, pvs)
	setAgentSpec(t, env, nodeRefAgent(exclNode1))

	_, err := env.srv.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: volumeID,
		NodeId:   exclNode1,
	})
	if err != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", err)
	}
	if env.agent.denyInitiatorCalls != 0 {
		t.Errorf("DenyInitiator calls = %d, want 0 for a local publication", env.agent.denyInitiatorCalls)
	}
	if n := len(env.agent.setLocalAttachCalls); n != 0 {
		t.Errorf("SetLocalAttach calls = %d, want 0 on unpublish", n)
	}
	got := loadVolumeState(t, env, volumeID)
	if len(got.Status.PublishedNodes) != 0 {
		t.Errorf("publishedNodes = %+v, want released", got.Status.PublishedNodes)
	}
	if got.Status.LocalAttachNode != exclNode1 {
		t.Errorf("localAttachNode = %q, want kept %q", got.Status.LocalAttachNode, exclNode1)
	}
}

// TestDesiredVolumeState_LocalAttach verifies the export resync keeps the
// export fenced while status.localAttachNode is set and never puts a local
// publication into the ACL.
func TestDesiredVolumeState_LocalAttach(t *testing.T) {
	t.Parallel()
	volumeID := basePublishRequest().GetVolumeId()
	remote := exclPub(exclNode2, exclNQN(exclNode2), localAttachSNW, false)
	tests := []struct {
		name           string
		localNode      string
		pubs           []v1alpha1.VolumePublication
		wantInitiators []string
		wantLocal      bool
	}{
		{"local publication", exclNode1, []v1alpha1.VolumePublication{localPub()}, []string{}, true},
		{"local attach released, still fenced", exclNode1, nil, []string{}, true},
		{"protocol publish awaiting unfence", exclNode1, []v1alpha1.VolumePublication{remote},
			[]string{exclNQN(exclNode2)}, true},
		{"protocol only", "", []v1alpha1.VolumePublication{remote}, []string{exclNQN(exclNode2)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pvs := localAttachVolumeState(volumeID, true, tc.pubs...)
			pvs.ObjectMeta = metav1.ObjectMeta{Name: pvs.Name, UID: "uid-local"}
			pvs.Status.LocalAttachNode = tc.localNode

			desired, err := desiredVolumeState(pvs, nil)
			if err != nil {
				t.Fatalf("desiredVolumeState: %v", err)
			}
			exp := desired.GetExports()[0]
			if !slices.Equal(exp.GetAllowedInitiators(), tc.wantInitiators) {
				t.Errorf("allowed initiators = %v, want %v", exp.GetAllowedInitiators(), tc.wantInitiators)
			}
			if exp.GetLocalAttach() != tc.wantLocal {
				t.Errorf("local_attach = %t, want %t", exp.GetLocalAttach(), tc.wantLocal)
			}
		})
	}
}

// TestCreateVolume_LocalAttachParameter verifies a hand-written class's
// local-attach parameter: "true"/"false" (absent is false) are recorded in
// spec.resolved, anything else is InvalidArgument before any agent call.
func TestCreateVolume_LocalAttachParameter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		value    string
		set      bool
		want     bool
		wantCode codes.Code
	}{
		{"absent", "", false, false, codes.OK},
		{"true", "true", true, true, codes.OK},
		{"false", "false", true, false, codes.OK},
		{"not a boolean", "yes", true, false, codes.InvalidArgument},
		{"empty", "", true, false, codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			req := baseCreateVolumeRequest()
			if tc.set {
				req.Parameters[paramLocalAttach] = tc.value
			}
			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != tc.wantCode {
				t.Fatalf("CreateVolume code = %v (err=%v), want %v", status.Code(err), err, tc.wantCode)
			}
			if tc.wantCode != codes.OK {
				if env.agent.createVolumeCalls != 0 {
					t.Errorf("agent CreateVolume calls = %d, want 0", env.agent.createVolumeCalls)
				}
				return
			}
			if got := loadResolved(t, env, "pvc-abc123").LocalAttach; got != tc.want {
				t.Errorf("spec.resolved.localAttach = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestCreateVolume_LocalAttachFromBinding verifies a generated class takes
// localAttach from its PillarStorageClass.
func TestCreateVolume_LocalAttachFromBinding(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t, &v1alpha1.PillarStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "local-binding"},
		Spec: v1alpha1.PillarStorageClassSpec{
			StoreRef:    testStoreName,
			ProtocolRef: testProtocolName,
			LocalAttach: true,
		},
	})
	req := baseCreateVolumeRequest()
	req.Parameters = map[string]string{paramBinding: "local-binding"}
	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if !loadResolved(t, env, "pvc-abc123").LocalAttach {
		t.Error("spec.resolved.localAttach = false, want true from the binding")
	}
}

// TestResolveVolumeConfig_ReplaysRecordedLocalAttach verifies a retry keeps
// the recorded localAttach even when the class changed since.
func TestResolveVolumeConfig_ReplaysRecordedLocalAttach(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	for _, tc := range []struct {
		recorded bool
		class    string
	}{{true, "false"}, {false, "true"}} {
		recorded := &v1alpha1.ResolvedVolumeConfig{
			Backend: v1alpha1.BackendSpec{ZFS: &v1alpha1.ZFSBackendConfig{
				VolumeType: v1alpha1.ZFSVolumeTypeZvol,
				Pool:       "tank",
			}},
			Protocol:    v1alpha1.ProtocolSpec{NVMeOFTCP: &v1alpha1.NVMeOFTCPConfig{Port: 4420}},
			LocalAttach: tc.recorded,
		}
		res, err := env.srv.resolveVolumeConfig(context.Background(), map[string]string{
			paramStoreRef:    testStoreName,
			paramProtocolRef: testProtocolName,
			paramLocalAttach: tc.class,
		}, nil, recorded)
		if err != nil {
			t.Fatalf("resolveVolumeConfig: %v", err)
		}
		if res.resolved.LocalAttach != tc.recorded {
			t.Errorf("replayed localAttach = %t with class %q, want recorded %t",
				res.resolved.LocalAttach, tc.class, tc.recorded)
		}
	}
}
