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

// Controller-side ownership recovery (spec.recovery, phase RecoveryPending).
//
// The Kubernetes API is the controller-runtime fake client; the agent is a
// scripted AgentServiceClient whose InspectVolume reports a durable fence
// mark and whose TransferVolumeOwnership moves that mark like the real agent
// does on commit.  Snapshots and authorizations are really signed with
// ephemeral ECDSA keys through internal/recoveryauth, so digests and
// encodings are the production ones.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"

	"github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/isac322/pillar-csi/internal/recoveryauth"
)

const (
	recoveryPVName   = "pvc-recover"
	recoveryOldUID   = "11111111-2222-3333-4444-555555555555"
	recoveryOldGen   = int64(7)
	recoveryNewGen   = int64(3)
	recoveryVG       = "data-vg"
	recoveryLV       = "legacy"
	recoveryVGUUID   = "AAAAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAAAA"
	recoveryLVUUID   = "BBBBBB-BBBB-BBBB-BBBB-BBBB-BBBB-BBBBBB"
	recoveryAgentVol = recoveryVG + "/" + recoveryLV
	recoveryVolumeID = "storage-node-1/nvmeof-tcp/lvm-lv/" + recoveryAgentVol
	recoveryDevPath  = "/dev/" + recoveryAgentVol
	recoveryLVBytes  = int64(2 << 30)
)

// recoveryAgent scripts the recovery RPCs on top of mockAgentClient.  Its
// fence is the agent's durable mark; a committed transfer rewrites it the
// way the real agent does (new uid/generation, old uid retired).
type recoveryAgent struct {
	*mockAgentClient

	fence     *agentv1.FenceObservation
	consumers []*agentv1.DeviceConsumer

	inspectCalls int

	// outcomes is consumed one per TransferVolumeOwnership call; when empty
	// the call commits.  transferErr fails every call.
	outcomes     []agentv1.TransferOutcome
	transferErr  error
	transferReqs []*agentv1.TransferVolumeOwnershipRequest
	// landOnUnknown makes an UNKNOWN outcome still move the mark.
	landOnUnknown bool
}

func (a *recoveryAgent) InspectVolume(
	_ context.Context,
	req *agentv1.InspectVolumeRequest,
	_ ...grpc.CallOption,
) (*agentv1.InspectVolumeResponse, error) {
	a.inspectCalls++
	if req.GetBackendType() != agentv1.BackendType_BACKEND_TYPE_LVM || req.GetVolumeId() != recoveryAgentVol {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected inspect %v", req)
	}
	fence, ok := proto.Clone(a.fence).(*agentv1.FenceObservation)
	if !ok {
		return nil, status.Errorf(codes.Internal, "clone fence observation %T", a.fence)
	}
	return &agentv1.InspectVolumeResponse{
		Lvm: &agentv1.LvmObservation{
			Identity:       recoverySourceIdentity(),
			SizeBytes:      recoveryLVBytes,
			Active:         true,
			DevicePath:     recoveryDevPath,
			ExclusiveClaim: "free",
		},
		Consumers: a.consumers,
		Fence:     fence,
	}, nil
}

func (a *recoveryAgent) TransferVolumeOwnership(
	_ context.Context,
	req *agentv1.TransferVolumeOwnershipRequest,
	_ ...grpc.CallOption,
) (*agentv1.TransferVolumeOwnershipResponse, error) {
	cloned, ok := proto.Clone(req).(*agentv1.TransferVolumeOwnershipRequest)
	if !ok {
		return nil, status.Errorf(codes.Internal, "clone transfer request %T", req)
	}
	a.transferReqs = append(a.transferReqs, cloned)
	if a.transferErr != nil {
		return nil, a.transferErr
	}
	outcome := agentv1.TransferOutcome_TRANSFER_OUTCOME_COMMITTED
	if len(a.outcomes) > 0 {
		outcome = a.outcomes[0]
		a.outcomes = a.outcomes[1:]
	}
	auth := req.GetAuthorization()
	digest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if outcome != agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN || a.landOnUnknown {
		a.fence = &agentv1.FenceObservation{
			Exists:                      true,
			VolumeUid:                   auth.GetNewVolumeUid(),
			Generation:                  auth.GetNewGeneration(),
			EndedUids:                   []string{auth.GetOldVolumeUid()},
			PreserveOriginal:            auth.GetPreserveOriginal(),
			LvmSource:                   auth.GetLvmSource(),
			TransferAuthorizationDigest: recoveryauth.DigestHex(digest),
			TransferFromUid:             auth.GetOldVolumeUid(),
			TransferFromGeneration:      auth.GetOldGeneration(),
			TransferToUid:               auth.GetNewVolumeUid(),
			TransferToGeneration:        auth.GetNewGeneration(),
		}
	}
	return &agentv1.TransferVolumeOwnershipResponse{Outcome: outcome, AuthorizationDigest: digest[:]}, nil
}

func (a *recoveryAgent) ExportVolume(
	ctx context.Context,
	req *agentv1.ExportVolumeRequest,
	opts ...grpc.CallOption,
) (*agentv1.ExportVolumeResponse, error) {
	resp, err := a.mockAgentClient.ExportVolume(ctx, req, opts...)
	if a.fence != nil && req.GetFence().GetVolumeUid() == a.fence.GetVolumeUid() &&
		req.GetFence().GetGeneration() > a.fence.GetGeneration() {
		a.fence.Generation = req.GetFence().GetGeneration()
	}
	return resp, err
}

func recoverySourceIdentity() *agentv1.LvmSourceIdentity {
	return &agentv1.LvmSourceIdentity{
		VolumeGroup:       recoveryVG,
		LogicalVolume:     recoveryLV,
		VolumeGroupUuid:   recoveryVGUUID,
		LogicalVolumeUuid: recoveryLVUUID,
	}
}

// oldMark is the retired lifecycle's durable mark the agent still holds.
func oldMark(preserve bool) *agentv1.FenceObservation {
	return &agentv1.FenceObservation{
		Exists:           true,
		VolumeUid:        recoveryOldUID,
		Generation:       uint64(recoveryOldGen),
		PreserveOriginal: preserve,
		LvmSource:        recoverySourceIdentity(),
	}
}

type recoveryEnv struct {
	*controllerTestEnv
	agent *recoveryAgent
	req   *csi.CreateVolumeRequest
}

// newRecoveryEnv seeds the LVM store config and an operator-created
// recovery record (spec.recovery only, no status, no authorization yet).
func newRecoveryEnv(t *testing.T, preserve bool, extra ...func(*v1alpha1.PillarVolumeState)) *recoveryEnv {
	t.Helper()
	env := newControllerTestEnv(t)
	ra := &recoveryAgent{mockAgentClient: env.agent, fence: oldMark(preserve)}
	env.srv.dialAgent = func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
		return ra, nopCloser{}, nil
	}
	pvs := &v1alpha1.PillarVolumeState{
		Name: recoveryPVName,
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      recoveryVolumeID,
			AgentVolumeID: recoveryAgentVol,
			AgentRef:      "storage-node-1",
			BackendType:   string(v1alpha1.BackendIDLVMLV),
			ProtocolType:  string(v1alpha1.ProtocolIDNVMeOFTCP),
			CapacityBytes: 1 << 30,
			Recovery: &v1alpha1.VolumeRecoveryIntent{
				OldVolumeUID:  recoveryOldUID,
				OldGeneration: recoveryOldGen,
				NewGeneration: recoveryNewGen,
				Source: &v1alpha1.LVMSourceRef{
					VolumeGroup:       recoveryVG,
					LogicalVolume:     recoveryLV,
					VolumeGroupUUID:   recoveryVGUUID,
					LogicalVolumeUUID: recoveryLVUUID,
					PreserveOriginal:  preserve,
				},
			},
		},
	}
	for _, f := range extra {
		f(pvs)
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvs); err != nil {
		t.Fatalf("seed recovery record: %v", err)
	}
	req := baseCreateVolumeRequest()
	req.Name = recoveryPVName
	req.Parameters[paramStoreRef] = testLVMStoreName
	return &recoveryEnv{controllerTestEnv: env, agent: ra, req: req}
}

func (e *recoveryEnv) record(t *testing.T) *v1alpha1.PillarVolumeState {
	t.Helper()
	pvs, found, err := e.srv.readVolumeState(context.Background(), recoveryPVName)
	if err != nil || !found {
		t.Fatalf("read recovery record: found=%v err=%v", found, err)
	}
	return pvs
}

func mustECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

// grantOpts tweaks what the operator signs and patches.
type grantOpts struct {
	auth   func(*agentv1.RecoveryAuthorization)
	snap   func(*agentv1.RecoverySnapshot)
	record func(*v1alpha1.PillarVolumeState)
}

// signedSnapshot is the agent's healthy-time self-attestation of the old
// lifecycle, signed with the agent's identity key.
func signedSnapshot(
	t *testing.T,
	preserve bool,
	issued time.Time,
	mutate func(*agentv1.RecoverySnapshot),
) *agentv1.RecoverySnapshot {
	t.Helper()
	snap := &agentv1.RecoverySnapshot{
		VolumeId:         recoveryAgentVol,
		BackendType:      agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource:        recoverySourceIdentity(),
		OldVolumeUid:     recoveryOldUID,
		OldGeneration:    uint64(recoveryOldGen),
		PreserveOriginal: preserve,
		ExclusiveClaim:   "free",
		Fence:            oldMark(preserve),
		AgentIdentity:    "storage-node-1",
		IssuedAt:         timestamppb.New(issued),
	}
	if mutate != nil {
		mutate(snap)
	}
	if err := recoveryauth.SignSnapshot(snap, mustECDSAKey(t)); err != nil {
		t.Fatalf("sign snapshot: %v", err)
	}
	return snap
}

func encodeSnapshot(t *testing.T, snap *agentv1.RecoverySnapshot) string {
	t.Helper()
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// grant performs the operator's step: read the record's uid, sign an
// authorization over the snapshot digest, and patch authorization,
// authorizationDigest and the base64 snapshot annotation in one update.
func (e *recoveryEnv) grant(t *testing.T, opts grantOpts) (*agentv1.RecoverySnapshot, *agentv1.RecoveryAuthorization) {
	t.Helper()
	pvs := e.record(t)
	preserve := pvs.Spec.Recovery.Source.PreserveOriginal
	snap := signedSnapshot(t, preserve, time.Now(), opts.snap)
	snapDigest, err := recoveryauth.SnapshotDigest(snap)
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	now := time.Now()
	auth := &agentv1.RecoveryAuthorization{
		SnapshotDigest:   snapDigest[:],
		VolumeId:         recoveryAgentVol,
		BackendType:      agentv1.BackendType_BACKEND_TYPE_LVM,
		LvmSource:        recoverySourceIdentity(),
		OldVolumeUid:     recoveryOldUID,
		OldGeneration:    uint64(recoveryOldGen),
		NewVolumeUid:     string(pvs.UID),
		NewGeneration:    uint64(recoveryNewGen),
		PreserveOriginal: new(preserve),
		IssuedAt:         timestamppb.New(now.Add(-time.Minute)),
		ExpiresAt:        timestamppb.New(now.Add(time.Hour)),
	}
	if opts.auth != nil {
		opts.auth(auth)
	}
	err = recoveryauth.SignAuthorization(auth, mustECDSAKey(t))
	if err != nil {
		t.Fatalf("sign authorization: %v", err)
	}
	raw, err := proto.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal authorization: %v", err)
	}
	digest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		t.Fatalf("authorization digest: %v", err)
	}
	pvs.Spec.Recovery.Authorization = raw
	pvs.Spec.Recovery.AuthorizationDigest = recoveryauth.DigestHex(digest)
	if pvs.Annotations == nil {
		pvs.Annotations = map[string]string{}
	}
	pvs.Annotations[annotationRecoverySnapshot] = encodeSnapshot(t, snap)
	if opts.record != nil {
		opts.record(pvs)
	}
	if err := e.srv.k8sClient.Update(context.Background(), pvs); err != nil {
		t.Fatalf("operator patch: %v", err)
	}
	return snap, auth
}

func wantCode(t *testing.T, err error, code codes.Code, what string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: code = %v (%v), want %v", what, status.Code(err), err, code)
	}
}

func recoveryPublishRequest() *csi.ControllerPublishVolumeRequest {
	req := basePublishRequest()
	req.VolumeId = recoveryVolumeID
	return req
}

func recoveryExpandRequest() *csi.ControllerExpandVolumeRequest {
	return &csi.ControllerExpandVolumeRequest{
		VolumeId:      recoveryVolumeID,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 4 << 30},
	}
}

// Creation window and reaper.

// A record is excluded from the reaper from its creation on — before the
// controller's first status write (empty phase) and while RecoveryPending —
// even though no claim or PersistentVolume refers to it.
func TestRecovery_ReaperNeverEndsRecoveryRecord(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	ctx := context.Background()

	if pvs := e.record(t); pvs.Status.Phase != "" {
		t.Fatalf("seeded phase = %q, want empty", pvs.Status.Phase)
	}
	reaped, err := e.srv.ReapAbandonedVolume(ctx, recoveryPVName)
	if err != nil || reaped {
		t.Fatalf("ReapAbandonedVolume(empty phase) = %v, %v; want kept", reaped, err)
	}

	_, err = e.srv.CreateVolume(ctx, e.req)
	wantCode(t, err, codes.Unavailable, "CreateVolume without authorization")
	if pvs := e.record(t); pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseRecoveryPending {
		t.Fatalf("phase = %q, want RecoveryPending", pvs.Status.Phase)
	}
	reaped, err = e.srv.ReapAbandonedVolume(ctx, recoveryPVName)
	if err != nil || reaped {
		t.Fatalf("ReapAbandonedVolume(RecoveryPending) = %v, %v; want kept", reaped, err)
	}
	pvs := e.record(t)
	if pvs.Status.Deleting {
		t.Fatal("reaper marked the recovery record deleting")
	}
	if e.agent.releaseVolumeCalls+e.agent.deleteVolumeCalls+e.agent.unexportVolumeCalls != 0 {
		t.Fatal("reaper reached the agent for a recovery record")
	}
}

// Adoption and non-serving refusals.

// The first attempt commits RecoveryPending with the declared generation and
// fills newVolumeUID; without the operator's grant it neither transfers nor
// exports, and publish, expand and delete are refused without side effects.
func TestRecovery_PendingRecordIsNonServing(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	ctx := context.Background()

	_, err := e.srv.CreateVolume(ctx, e.req)
	wantCode(t, err, codes.Unavailable, "CreateVolume without authorization")

	pvs := e.record(t)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseRecoveryPending {
		t.Errorf("phase = %q, want RecoveryPending", pvs.Status.Phase)
	}
	if pvs.Status.PublicationGeneration != recoveryNewGen {
		t.Errorf("publicationGeneration = %d, want declared newGeneration %d",
			pvs.Status.PublicationGeneration, recoveryNewGen)
	}
	if pvs.Spec.Recovery.NewVolumeUID != string(pvs.UID) {
		t.Errorf("spec.recovery.newVolumeUID = %q, want metadata.uid %q", pvs.Spec.Recovery.NewVolumeUID, pvs.UID)
	}
	if pvs.Spec.Resolved == nil {
		t.Error("spec.resolved not recorded at adoption")
	}
	if len(e.agent.transferReqs) != 0 || e.agent.exportVolumeCalls != 0 || e.agent.createVolumeCalls != 0 {
		t.Fatalf("pending record reached transfer/export/create: transfer=%d export=%d create=%d",
			len(e.agent.transferReqs), e.agent.exportVolumeCalls, e.agent.createVolumeCalls)
	}

	_, err = e.srv.ControllerPublishVolume(ctx, recoveryPublishRequest())
	wantCode(t, err, codes.FailedPrecondition, "publish of pending record")
	_, err = e.srv.ControllerExpandVolume(ctx, recoveryExpandRequest())
	wantCode(t, err, codes.FailedPrecondition, "expand of pending record")
	_, err = e.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: recoveryVolumeID})
	wantCode(t, err, codes.FailedPrecondition, "delete of pending record")

	after := e.record(t)
	if after.Status.Deleting || len(after.Status.PublishedNodes) != 0 ||
		after.Status.PublicationGeneration != recoveryNewGen {
		t.Fatalf("refusals mutated the record: deleting=%v published=%v generation=%d",
			after.Status.Deleting, after.Status.PublishedNodes, after.Status.PublicationGeneration)
	}
	if e.agent.allowInitiatorCalls+e.agent.expandVolumeCalls+e.agent.releaseVolumeCalls+
		e.agent.deleteVolumeCalls+e.agent.unexportVolumeCalls != 0 {
		t.Fatal("a refused operation reached the agent")
	}
}

// A record whose intent names another lifecycle as destination is refused
// before any status write.
func TestRecovery_ForeignDestinationUIDRefused(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true, func(p *v1alpha1.PillarVolumeState) {
		p.Spec.Recovery.NewVolumeUID = "99999999-0000-0000-0000-000000000000"
	})
	_, err := e.srv.CreateVolume(context.Background(), e.req)
	wantCode(t, err, codes.FailedPrecondition, "foreign destination uid")
	if pvs := e.record(t); pvs.Status.Phase != "" {
		t.Fatalf("phase = %q after refusal, want no status write", pvs.Status.Phase)
	}
	if e.agent.inspectCalls != 0 {
		t.Fatal("refused record reached the agent")
	}
}

// A claim carrying an import annotation never routes into the import path
// (no reservation, no agent call) for a recovery record.
func TestRecovery_ImportAnnotatedClaimRefused(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	pvc := &corev1.PersistentVolumeClaim{
		Name:      "data",
		Namespace: "default",
		Annotations: map[string]string{
			v1alpha1.AnnotationImportLV: recoveryAgentVol + ":" + recoveryVGUUID + ":" + recoveryLVUUID,
		},
	}
	if err := e.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create claim: %v", err)
	}
	e.req.Parameters[paramPVCNameMeta] = "data"
	e.req.Parameters[paramPVCNamespaceMeta] = "default"

	_, err := e.srv.CreateVolume(context.Background(), e.req)
	wantCode(t, err, codes.FailedPrecondition, "import-annotated recovery claim")
	var rsvs v1alpha1.PillarVolumeReservationList
	if err := e.srv.k8sClient.List(context.Background(), &rsvs); err != nil {
		t.Fatalf("list reservations: %v", err)
	}
	if len(rsvs.Items) != 0 || e.agent.inspectCalls != 0 || e.agent.importVolumeCalls != 0 {
		t.Fatalf("refused claim left side effects: reservations=%d inspect=%d import=%d",
			len(rsvs.Items), e.agent.inspectCalls, e.agent.importVolumeCalls)
	}
}

// Transfer, export, Ready.

// The authorized attempt presents exactly the operator's snapshot and grant,
// exports under a generation strictly newer than the transferred one, and
// only then records Ready; a PreserveOriginal recovery is then released, never
// destroyed or resized, and marks the volume context as preserving.
func TestRecovery_PreserveTransferCommitsThenReady(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	ctx := context.Background()
	snap, auth := e.grant(t, grantOpts{})

	resp, err := e.srv.CreateVolume(ctx, e.req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	assertRecoveryTransferExported(t, e, snap, auth)
	assertPreservedRecoveryResponse(t, resp)

	// A retry of the completed volume answers from the record.
	retry, err := e.srv.CreateVolume(ctx, e.req)
	if err != nil || retry.GetVolume().GetVolumeId() != recoveryVolumeID {
		t.Fatalf("retry of Ready recovery = %v, %v", retry, err)
	}
	if len(e.agent.transferReqs) != 1 {
		t.Fatal("retry of a Ready recovery transferred again")
	}

	_, err = e.srv.ControllerExpandVolume(ctx, recoveryExpandRequest())
	wantCode(t, err, codes.FailedPrecondition, "expand of preserved recovery")
	if _, err := e.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: recoveryVolumeID}); err != nil {
		t.Fatalf("DeleteVolume of Ready recovery: %v", err)
	}
	if e.agent.releaseVolumeCalls != 1 || e.agent.deleteVolumeCalls != 0 || e.agent.expandVolumeCalls != 0 {
		t.Fatalf("preserved recovery teardown: release=%d delete=%d expand=%d; want release only",
			e.agent.releaseVolumeCalls, e.agent.deleteVolumeCalls, e.agent.expandVolumeCalls)
	}
}

// assertRecoveryTransferExported requires exactly one transfer presenting
// the operator's snapshot and grant, no backend creation or import, an
// export fenced strictly above the transferred generation, and a Ready
// record.
func assertRecoveryTransferExported(
	t *testing.T,
	e *recoveryEnv,
	snap *agentv1.RecoverySnapshot,
	auth *agentv1.RecoveryAuthorization,
) {
	t.Helper()
	if len(e.agent.transferReqs) != 1 {
		t.Fatalf("transfer calls = %d, want 1", len(e.agent.transferReqs))
	}
	got := e.agent.transferReqs[0]
	if !proto.Equal(got.GetSnapshot(), snap) || !proto.Equal(got.GetAuthorization(), auth) {
		t.Fatal("transfer did not present the operator's exact snapshot and authorization")
	}
	if e.agent.createVolumeCalls+e.agent.importVolumeCalls != 0 {
		t.Fatal("recovery created or imported a backend volume")
	}
	pvs := e.record(t)
	fence := e.agent.lastExportVolumeReq.GetFence()
	if fence.GetVolumeUid() != string(pvs.UID) || fence.GetGeneration() <= uint64(recoveryNewGen) {
		t.Fatalf("export fence = %v, want uid %s with generation > %d", fence, pvs.UID, recoveryNewGen)
	}
	if e.agent.lastExportVolumeReq.GetDevicePath() != recoveryDevPath {
		t.Errorf("export device path = %q, want %q", e.agent.lastExportVolumeReq.GetDevicePath(), recoveryDevPath)
	}
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady || pvs.Status.ExportInfo == nil ||
		pvs.Status.ExportSpec == nil {
		t.Fatalf("record after success: phase=%q exportInfo=%v exportSpec=%v",
			pvs.Status.Phase, pvs.Status.ExportInfo, pvs.Status.ExportSpec)
	}
}

// assertPreservedRecoveryResponse requires the CreateVolume response of a
// PreserveOriginal recovery to name the volume, report the LV's capacity
// and mark the volume context as preserving.
func assertPreservedRecoveryResponse(t *testing.T, resp *csi.CreateVolumeResponse) {
	t.Helper()
	if resp.GetVolume().GetVolumeId() != recoveryVolumeID {
		t.Errorf("volume id = %q, want %q", resp.GetVolume().GetVolumeId(), recoveryVolumeID)
	}
	if resp.GetVolume().GetCapacityBytes() != recoveryLVBytes {
		t.Errorf("capacity = %d, want the LV's %d", resp.GetVolume().GetCapacityBytes(), recoveryLVBytes)
	}
	if resp.GetVolume().GetVolumeContext()[VolumeContextKeyPreserveOriginal] != annotationValueTrue {
		t.Error("preserve recovery did not mark the volume context")
	}
}

// A Managed recovery becomes a normal managed volume once Ready.
func TestRecovery_ManagedRecoveryIsNormalAfterReady(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, false)
	ctx := context.Background()
	e.agent.fence = oldMark(false)
	e.grant(t, grantOpts{})

	resp, err := e.srv.CreateVolume(ctx, e.req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, ok := resp.GetVolume().GetVolumeContext()[VolumeContextKeyPreserveOriginal]; ok {
		t.Error("managed recovery marked the volume context as preserving")
	}
	if _, err := e.srv.ControllerExpandVolume(ctx, recoveryExpandRequest()); err != nil {
		t.Fatalf("expand of managed recovery: %v", err)
	}
	if _, err := e.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: recoveryVolumeID}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if e.agent.deleteVolumeCalls != 1 || e.agent.releaseVolumeCalls != 0 {
		t.Fatalf("managed recovery teardown: delete=%d release=%d; want delete",
			e.agent.deleteVolumeCalls, e.agent.releaseVolumeCalls)
	}
}

// An outcome-unknown transfer is replayed once with the identical request;
// the agent's durable transfer record answers ALREADY_COMMITTED.
func TestRecovery_UnknownOutcomeReplaysIdenticalRequest(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	e.agent.outcomes = []agentv1.TransferOutcome{
		agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN,
		agentv1.TransferOutcome_TRANSFER_OUTCOME_ALREADY_COMMITTED,
	}
	e.agent.landOnUnknown = true
	e.grant(t, grantOpts{})

	if _, err := e.srv.CreateVolume(context.Background(), e.req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if len(e.agent.transferReqs) != 2 || !proto.Equal(e.agent.transferReqs[0], e.agent.transferReqs[1]) {
		t.Fatalf("transfer replays = %d, identical=%v; want 2 identical",
			len(e.agent.transferReqs), len(e.agent.transferReqs) == 2 &&
				proto.Equal(e.agent.transferReqs[0], e.agent.transferReqs[1]))
	}
	if e.record(t).Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatal("record not Ready after an idempotent commit")
	}
}

// A still-unknown outcome is Unavailable with no rollback and no Ready; the
// next attempt that observes the landed mark resumes without transferring.
func TestRecovery_UnknownOutcomeStaysPendingThenResumes(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	ctx := context.Background()
	e.agent.outcomes = []agentv1.TransferOutcome{
		agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN,
		agentv1.TransferOutcome_TRANSFER_OUTCOME_UNKNOWN,
	}
	_, auth := e.grant(t, grantOpts{})

	_, err := e.srv.CreateVolume(ctx, e.req)
	wantCode(t, err, codes.Unavailable, "twice-unknown transfer")
	pvs := e.record(t)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseRecoveryPending || pvs.Status.ExportInfo != nil {
		t.Fatalf("unknown outcome advanced the record: phase=%q", pvs.Status.Phase)
	}
	if e.agent.exportVolumeCalls != 0 || e.agent.releaseVolumeCalls+e.agent.deleteVolumeCalls != 0 {
		t.Fatal("unknown outcome exported or rolled back")
	}
	_, err = e.srv.ControllerPublishVolume(ctx, recoveryPublishRequest())
	wantCode(t, err, codes.FailedPrecondition, "publish while outcome unknown")

	// The mark did land: the agent now records this lifecycle.
	digest, err := recoveryauth.AuthorizationDigest(auth)
	if err != nil {
		t.Fatalf("authorization digest: %v", err)
	}
	e.agent.fence = &agentv1.FenceObservation{
		Exists:                      true,
		VolumeUid:                   auth.GetNewVolumeUid(),
		Generation:                  auth.GetNewGeneration(),
		EndedUids:                   []string{recoveryOldUID},
		PreserveOriginal:            true,
		LvmSource:                   recoverySourceIdentity(),
		TransferAuthorizationDigest: recoveryauth.DigestHex(digest),
		TransferFromUid:             auth.GetOldVolumeUid(),
		TransferFromGeneration:      auth.GetOldGeneration(),
		TransferToUid:               auth.GetNewVolumeUid(),
		TransferToGeneration:        auth.GetNewGeneration(),
	}
	resp, err := e.srv.CreateVolume(ctx, e.req)
	if err != nil {
		t.Fatalf("CreateVolume after the mark landed: %v", err)
	}
	if resp.GetVolume().GetCapacityBytes() != recoveryLVBytes {
		t.Fatalf("resumed capacity = %d, want inspected LV size %d",
			resp.GetVolume().GetCapacityBytes(), recoveryLVBytes)
	}
	if len(e.agent.transferReqs) != 2 {
		t.Fatalf("transfer calls = %d, want no new transfer once the mark records this lifecycle",
			len(e.agent.transferReqs))
	}
	pvs = e.record(t)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatal("record not Ready after resuming")
	}
	if pvs.Spec.CapacityBytes != recoveryLVBytes {
		t.Fatalf("durable capacity = %d, want inspected LV size %d",
			pvs.Spec.CapacityBytes, recoveryLVBytes)
	}
}

// A committed transfer whose export fails records CreatePartial; the retry
// re-inspects the transferred LV, refreshes the export settings and capacity,
// and only then re-exports without a second ownership transfer.
func TestRecovery_ExportFailureRetryOnlyReexports(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	ctx := context.Background()
	e.grant(t, grantOpts{})
	e.req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  inCapsuleDataSize: 4096\n"
	e.agent.exportVolumeErr = status.Error(codes.Unavailable, "port busy")

	_, err := e.srv.CreateVolume(ctx, e.req)
	wantCode(t, err, codes.Unavailable, "export failure")
	pvs := e.record(t)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseCreatePartial ||
		pvs.Status.BackendDevicePath != recoveryDevPath {
		t.Fatalf("after export failure: phase=%q device=%q", pvs.Status.Phase, pvs.Status.BackendDevicePath)
	}
	_, err = e.srv.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: recoveryVolumeID})
	wantCode(t, err, codes.FailedPrecondition, "delete before Ready")
	reaped, err := e.srv.ReapAbandonedVolume(ctx, recoveryPVName)
	if err != nil || reaped {
		t.Fatalf("reaper on CreatePartial recovery = %v, %v; want kept", reaped, err)
	}

	e.agent.exportVolumeErr = nil
	inspects := e.agent.inspectCalls
	e.req.Parameters[paramProtocolDoc] = "nvmeofTcp:\n  inCapsuleDataSize: 8192\n"
	if _, err := e.srv.CreateVolume(ctx, e.req); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(e.agent.transferReqs) != 1 || e.agent.inspectCalls != inspects+1 {
		t.Fatalf("retry transfer=%d inspect delta=%d; want one transfer and one re-inspection",
			len(e.agent.transferReqs), e.agent.inspectCalls-inspects)
	}
	if got := e.agent.lastExportVolumeReq.GetExportParams().GetNvmeofTcp().GetInCapsuleDataSize(); got != 8192 {
		t.Fatalf("retry in-capsule data size = %d, want 8192", got)
	}
	pvs = e.record(t)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatal("record not Ready after re-export")
	}
	if pvs.Status.ExportSpec == nil || pvs.Status.ExportSpec.InCapsuleDataSize == nil ||
		*pvs.Status.ExportSpec.InCapsuleDataSize != 8192 {
		t.Fatalf("durable export spec = %+v, want in-capsule data size 8192", pvs.Status.ExportSpec)
	}
}

// An agent refusal (e.g. a different destination already committed) is
// returned as-is and leaves the record pending.
func TestRecovery_AgentRefusalKeepsPending(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	e.agent.transferErr = status.Error(codes.FailedPrecondition, "mark already transferred to another destination")
	e.grant(t, grantOpts{})

	_, err := e.srv.CreateVolume(context.Background(), e.req)
	wantCode(t, err, codes.FailedPrecondition, "agent refusal")
	if pvs := e.record(t); pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseRecoveryPending {
		t.Fatalf("phase = %q, want RecoveryPending", pvs.Status.Phase)
	}
	if e.agent.exportVolumeCalls != 0 {
		t.Fatal("refused transfer was exported")
	}
}

// Wrong source, generation, destination, grant or snapshot.

// Every disagreement between the grant, the snapshot, the live mark and the
// record is refused before TransferVolumeOwnership, and the record stays
// pending.
func TestRecovery_MismatchesRefusedBeforeTransfer(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		opts  grantOpts
		agent func(*recoveryAgent)
		code  codes.Code
	}{
		"grant names an older old generation": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.OldGeneration = uint64(recoveryOldGen - 1) }},
			code: codes.FailedPrecondition,
		},
		"grant names another old lifecycle": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.OldVolumeUid = "other-old" }},
			code: codes.FailedPrecondition,
		},
		"grant names another destination uid": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.NewVolumeUid = "other-new" }},
			code: codes.FailedPrecondition,
		},
		"grant names another destination generation": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.NewGeneration = uint64(recoveryNewGen + 1) }},
			code: codes.FailedPrecondition,
		},
		"grant names another LV": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) {
				a.LvmSource.LogicalVolumeUuid = "CCCCCC-CCCC-CCCC-CCCC-CCCC-CCCC-CCCCCC"
			}},
			code: codes.FailedPrecondition,
		},
		"grant flips the preserve policy": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.PreserveOriginal = new(false) }},
			code: codes.FailedPrecondition,
		},
		"grant omits the preserve policy": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) { a.PreserveOriginal = nil }},
			code: codes.FailedPrecondition,
		},
		"grant expired": {
			opts: grantOpts{auth: func(a *agentv1.RecoveryAuthorization) {
				a.IssuedAt = timestamppb.New(time.Now().Add(-3 * time.Hour))
				a.ExpiresAt = timestamppb.New(time.Now().Add(-2 * time.Hour))
			}},
			code: codes.FailedPrecondition,
		},
		"record pins a different authorization digest": {
			opts: grantOpts{record: func(p *v1alpha1.PillarVolumeState) {
				p.Spec.Recovery.AuthorizationDigest = strings.Repeat("0", 64)
			}},
			code: codes.FailedPrecondition,
		},
		"authorization bytes are corrupt": {
			opts: grantOpts{record: func(p *v1alpha1.PillarVolumeState) {
				p.Spec.Recovery.Authorization = []byte{0xff, 0xff, 0xff}
			}},
			code: codes.FailedPrecondition,
		},
		"snapshot annotation is not base64": {
			opts: grantOpts{record: func(p *v1alpha1.PillarVolumeState) {
				p.Annotations[annotationRecoverySnapshot] = "%%%not-base64%%%"
			}},
			code: codes.FailedPrecondition,
		},
		"snapshot annotation is missing": {
			opts: grantOpts{record: func(p *v1alpha1.PillarVolumeState) {
				delete(p.Annotations, annotationRecoverySnapshot)
			}},
			code: codes.Unavailable,
		},
		"grant was signed over a different snapshot": {
			opts: grantOpts{record: func(p *v1alpha1.PillarVolumeState) {
				other := signedSnapshot(t, true, time.Now().Add(-time.Second), nil)
				p.Annotations[annotationRecoverySnapshot] = encodeSnapshot(t, other)
			}},
			code: codes.FailedPrecondition,
		},
		"snapshot reports a consumer": {
			opts: grantOpts{snap: func(s *agentv1.RecoverySnapshot) {
				s.Consumers = []*agentv1.DeviceConsumer{{Kind: "mount", Detail: "/mnt/old"}}
			}},
			code: codes.FailedPrecondition,
		},
		"snapshot reports a busy device": {
			opts: grantOpts{snap: func(s *agentv1.RecoverySnapshot) { s.ExclusiveClaim = "busy" }},
			code: codes.FailedPrecondition,
		},
		"live mark moved to a newer generation": {
			agent: func(a *recoveryAgent) { a.fence.Generation = uint64(recoveryOldGen + 1) },
			code:  codes.FailedPrecondition,
		},
		"live mark belongs to another lifecycle": {
			agent: func(a *recoveryAgent) { a.fence.VolumeUid = "someone-else" },
			code:  codes.FailedPrecondition,
		},
		"live mark is missing": {
			agent: func(a *recoveryAgent) { a.fence = &agentv1.FenceObservation{} },
			code:  codes.FailedPrecondition,
		},
		"live mark pins another LV": {
			agent: func(a *recoveryAgent) { a.fence.LvmSource.VolumeGroupUuid = "DDDDDD-DDDD-DDDD-DDDD-DDDD-DDDD-DDDDDD" },
			code:  codes.FailedPrecondition,
		},
		"old consumer is still live": {
			agent: func(a *recoveryAgent) {
				a.consumers = []*agentv1.DeviceConsumer{{Kind: "holder", Detail: "dm-3"}}
			},
			code: codes.FailedPrecondition,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRecoveryEnv(t, true)
			if tc.agent != nil {
				tc.agent(e.agent)
			}
			e.grant(t, tc.opts)

			_, err := e.srv.CreateVolume(context.Background(), e.req)
			wantCode(t, err, tc.code, name)
			if len(e.agent.transferReqs) != 0 || e.agent.exportVolumeCalls != 0 {
				t.Fatalf("refused recovery reached transfer=%d export=%d",
					len(e.agent.transferReqs), e.agent.exportVolumeCalls)
			}
			pvs := e.record(t)
			if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseRecoveryPending || pvs.Status.ExportInfo != nil {
				t.Fatalf("refusal advanced the record: phase=%q", pvs.Status.Phase)
			}
		})
	}
}

// The claim must resolve to the record's declared routing; a claim that
// resolves to another backend is refused before adoption.
func TestRecovery_ClaimRoutingDriftRefused(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	e.req.Parameters[paramStoreRef] = testStoreName // ZFS store
	_, err := e.srv.CreateVolume(context.Background(), e.req)
	wantCode(t, err, codes.FailedPrecondition, "routing drift")
	if pvs := e.record(t); pvs.Status.Phase != "" {
		t.Fatalf("drifting claim adopted the record: phase=%q", pvs.Status.Phase)
	}
}

// A record whose generation already advanced past the declared one can never
// replay the grant and is refused.
func TestRecovery_GenerationAheadOfIntentRefused(t *testing.T) {
	t.Parallel()
	e := newRecoveryEnv(t, true)
	pvs := e.record(t)
	pvs.Status.PublicationGeneration = recoveryNewGen + 5
	if err := e.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("seed generation: %v", err)
	}
	_, err := e.srv.CreateVolume(context.Background(), e.req)
	wantCode(t, err, codes.FailedPrecondition, "generation ahead of intent")
	if e.agent.inspectCalls != 0 {
		t.Fatal("refused record reached the agent")
	}
}
