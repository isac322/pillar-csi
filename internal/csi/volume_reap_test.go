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
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const reapClaimUID = "0f3c2a5e-7b41-4c8e-9d3a-1e2f3a4b5c6d"

func createClaim(t *testing.T, env *controllerTestEnv, finalizers ...string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		Name: "data", Namespace: "default", UID: types.UID(reapClaimUID), Finalizers: finalizers,
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	return pvc
}

func deleteClaim(t *testing.T, env *controllerTestEnv, pvc *corev1.PersistentVolumeClaim) {
	t.Helper()
	if err := env.srv.k8sClient.Delete(context.Background(), pvc); err != nil {
		t.Fatalf("delete PVC: %v", err)
	}
}

// failedAttempt creates the claim reapClaimUID and runs a CreateVolume for it
// whose backend creation fails at the agent (issue #97), leaving its
// PillarVolumeState in phase Provisioning.  It returns the request for
// retries and the claim.
func failedAttempt(
	t *testing.T,
	env *controllerTestEnv,
	finalizers ...string,
) (*csi.CreateVolumeRequest, *corev1.PersistentVolumeClaim) {
	t.Helper()
	pvc := createClaim(t, env, finalizers...)
	env.agent.createVolumeErr = status.Error(codes.Internal, "zfs create: out of space")
	req := baseCreateVolumeRequest()
	req.Name = "pvc-" + reapClaimUID
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume: want Internal, got %v", err)
	}
	pvs := volumeState(t, env, req.Name)
	if pvs == nil || pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseProvisioning {
		t.Fatalf("PillarVolumeState of the failed attempt = %+v, want phase Provisioning", pvs)
	}
	env.agent.createVolumeErr = nil
	return req, pvc
}

func volumeState(t *testing.T, env *controllerTestEnv, name string) *v1alpha1.PillarVolumeState {
	t.Helper()
	pvs, found, err := env.srv.readVolumeState(context.Background(), name)
	if err != nil {
		t.Fatalf("read PillarVolumeState %q: %v", name, err)
	}
	if !found {
		return nil
	}
	return pvs
}

func reap(t *testing.T, env *controllerTestEnv, name string) bool {
	t.Helper()
	reaped, err := env.srv.ReapAbandonedVolume(context.Background(), name)
	if err != nil {
		t.Fatalf("ReapAbandonedVolume(%q): %v", name, err)
	}
	return reaped
}

func agentTeardownCalls(env *controllerTestEnv) int {
	return env.agent.unexportVolumeCalls + env.agent.deleteVolumeCalls
}

// TestReapAbandonedVolume_FailedAttemptWithoutClaim is the issue #97
// regression: once the claim of a failed attempt is gone, the lifecycle is
// ended through the fenced agent teardown and the record is removed.
func TestReapAbandonedVolume_FailedAttemptWithoutClaim(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)

	if !reap(t, env, req.Name) {
		t.Fatal("abandoned attempt was not reaped")
	}
	if env.agent.unexportVolumeCalls != 1 || env.agent.deleteVolumeCalls != 1 {
		t.Fatalf("agent teardown: unexport=%d delete=%d, want 1 and 1 (the backend may exist)",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	if volumeState(t, env, req.Name) != nil {
		t.Fatal("PillarVolumeState still exists after the teardown")
	}
}

// TestReapAbandonedVolume_LiveClaimKeepsRetryIdempotent: while the claim
// exists the provisioner retries, so the record must survive and the retry
// must still succeed on it.
func TestReapAbandonedVolume_LiveClaimKeepsRetryIdempotent(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, _ := failedAttempt(t, env)
	before := volumeState(t, env, req.Name)

	if reap(t, env, req.Name) {
		t.Fatal("volume of a live claim was reaped")
	}
	if agentTeardownCalls(env) != 0 {
		t.Fatal("agent teardown called for a live claim")
	}
	after := volumeState(t, env, req.Name)
	if after == nil || after.UID != before.UID || after.Status.Deleting {
		t.Fatalf("record of a live claim changed: %+v", after)
	}

	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume retry: %v", err)
	}
}

// TestReapAbandonedVolume_TerminatingClaimKept: external-provisioner keeps
// provisioning a claim until it is removed (a Pod using it holds the
// pvc-protection finalizer), so a terminating claim may still retry
// CreateVolume; once it is removed the attempt is reaped.
func TestReapAbandonedVolume_TerminatingClaimKept(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()
	req, pvc := failedAttempt(t, env, "kubernetes.io/pvc-protection")
	deleteClaim(t, env, pvc)

	if reap(t, env, req.Name) {
		t.Fatal("volume of a terminating claim was reaped while provisioning may still retry")
	}
	env.agent.createVolumeErr = status.Error(codes.Internal, "zfs create: out of space")
	if _, err := env.srv.CreateVolume(ctx, req); status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume retry of a terminating claim: want the agent's Internal, got %v", err)
	}

	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, pvc); err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	pvc.Finalizers = nil
	if err := env.srv.k8sClient.Update(ctx, pvc); err != nil {
		t.Fatalf("remove PVC finalizer: %v", err)
	}
	if !reap(t, env, req.Name) {
		t.Fatal("volume of a removed claim was not reaped")
	}
}

// TestCreateVolume_RefusesAbandonedClaim: external-provisioner keeps retrying
// a deleted claim after a timeout; once the claim is gone CreateVolume must
// neither continue the attempt nor start a new lifecycle after the reap, and
// its error must be final for the provisioner.
func TestCreateVolume_RefusesAbandonedClaim(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	ctx := context.Background()
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	agentCreates := env.agent.createVolumeCalls

	if _, err := env.srv.CreateVolume(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retry of an abandoned attempt: want FailedPrecondition, got %v", err)
	}
	if !reap(t, env, req.Name) {
		t.Fatal("abandoned attempt was not reaped")
	}
	if _, err := env.srv.CreateVolume(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retry after the reap: want FailedPrecondition, got %v", err)
	}
	if env.agent.createVolumeCalls != agentCreates {
		t.Fatal("agent CreateVolume called for an abandoned claim")
	}
	if volumeState(t, env, req.Name) != nil {
		t.Fatal("retry after the reap re-created the PillarVolumeState")
	}
}

// TestReapAbandonedVolume_ReadyVolumeKept: CreateVolume reported a Ready
// volume created, so a PersistentVolume and its reclaim policy own it, even
// once the claim and the PersistentVolume object are gone (Retain).
func TestReapAbandonedVolume_ReadyVolumeKept(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	pvc := createClaim(t, env)
	req := baseCreateVolumeRequest()
	req.Name = "pvc-" + reapClaimUID
	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if pvs := volumeState(t, env, req.Name); pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatalf("phase after a successful CreateVolume = %q, want Ready", pvs.Status.Phase)
	}
	deleteClaim(t, env, pvc)

	if reap(t, env, req.Name) {
		t.Fatal("Ready volume was reaped")
	}
	if agentTeardownCalls(env) != 0 {
		t.Fatal("agent teardown called for a Ready volume")
	}
}

// TestPersistVolumeReady_RefusesDeletingLifecycle: an attempt the reaper
// already marked deleting must not be reported created.
func TestPersistVolumeReady_RefusesDeletingLifecycle(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, _ := failedAttempt(t, env)
	pvs := volumeState(t, env, req.Name)
	pvs.Status.Deleting = true
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}

	err := env.srv.persistVolumeReady(context.Background(), req.Name, pvs.UID, &agentv1.ExportInfo{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("persistVolumeReady on a deleting lifecycle: want FailedPrecondition, got %v", err)
	}
	if got := volumeState(t, env, req.Name); got.Status.Phase == v1alpha1.PillarVolumeStatePhaseReady {
		t.Fatal("lifecycle under deletion recorded Ready")
	}
}

// TestReapAbandonedVolume_PersistentVolumeOwnsLifecycle: a volume a
// PersistentVolume refers to is deleted through CSI DeleteVolume only,
// whether the PersistentVolume is named after it or refers to its handle.
func TestReapAbandonedVolume_PersistentVolumeOwnsLifecycle(t *testing.T) {
	t.Parallel()
	for name, pvName := range map[string]string{"by name": "pvc-" + reapClaimUID, "by handle": "static-pv"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			req, pvc := failedAttempt(t, env)
			deleteClaim(t, env, pvc)
			pvs := volumeState(t, env, req.Name)
			pv := &corev1.PersistentVolume{
				Name: pvName,
				Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "pillar-csi.bhyoo.com", VolumeHandle: pvs.Spec.VolumeID},
				}},
			}
			if err := env.srv.k8sClient.Create(context.Background(), pv); err != nil {
				t.Fatalf("create PV: %v", err)
			}

			if reap(t, env, req.Name) {
				t.Fatal("volume with a PersistentVolume was reaped")
			}
			if volumeState(t, env, req.Name) == nil {
				t.Fatal("PillarVolumeState removed")
			}
		})
	}
}

// TestReapAbandonedVolume_UnattributedVolumeKept: without a recorded claim
// and without a "pvc-<claim UID>" name the volume cannot be proven
// abandoned.
func TestReapAbandonedVolume_UnattributedVolumeKept(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	env.agent.createVolumeErr = status.Error(codes.Internal, "out of space")
	req := baseCreateVolumeRequest() // "pvc-abc123": no claim UID
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume: want Internal, got %v", err)
	}

	if reap(t, env, req.Name) {
		t.Fatal("unattributed volume was reaped")
	}
	if volumeState(t, env, req.Name) == nil {
		t.Fatal("PillarVolumeState removed")
	}
}

// TestReapAbandonedVolume_ClaimRefFromProvisionerMetadata: with
// --extra-create-metadata the provisioner passes the claim name and
// namespace; the claim UID is resolved and recorded, so a volume with a
// custom name prefix is attributed through it.
func TestReapAbandonedVolume_ClaimRefFromProvisionerMetadata(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	pvc := createClaim(t, env)
	env.agent.createVolumeErr = status.Error(codes.Internal, "out of space")
	req := baseCreateVolumeRequest()
	req.Name = "custom-" + reapClaimUID
	req.Parameters[paramPVCNameMeta] = "data"
	req.Parameters[paramPVCNamespaceMeta] = "default"
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume: want Internal, got %v", err)
	}

	pvs := volumeState(t, env, req.Name)
	want := v1alpha1.VolumeClaimRef{UID: reapClaimUID, Namespace: "default", Name: "data"}
	if pvs == nil || pvs.Spec.ClaimRef == nil || *pvs.Spec.ClaimRef != want {
		t.Fatalf("claimRef = %+v, want %+v", pvs, want)
	}

	if reap(t, env, req.Name) {
		t.Fatal("volume of a live claim was reaped")
	}
	deleteClaim(t, env, pvc)
	if !reap(t, env, req.Name) {
		t.Fatal("volume of a deleted claim was not reaped")
	}
}

// TestReapAbandonedVolume_TeardownFailureFailsClosed: an agent failure keeps
// the record marked deleting and the retry completes the teardown.
func TestReapAbandonedVolume_TeardownFailureFailsClosed(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	env.agent.deleteVolumeErr = status.Error(codes.Internal, "dataset is busy")

	if _, err := env.srv.ReapAbandonedVolume(context.Background(), req.Name); err == nil {
		t.Fatal("ReapAbandonedVolume succeeded although the agent failed")
	}
	pvs := volumeState(t, env, req.Name)
	if pvs == nil || !pvs.Status.Deleting {
		t.Fatalf("record after a failed teardown = %+v, want kept and marked deleting", pvs)
	}

	env.agent.deleteVolumeErr = nil
	if !reap(t, env, req.Name) {
		t.Fatal("retry did not finish the teardown")
	}
	if volumeState(t, env, req.Name) != nil {
		t.Fatal("PillarVolumeState still exists after the retried teardown")
	}
}

// TestReapAbandonedVolume_PublishedVolumeKept: a recorded publication means a
// node may still hold the volume; the deletion is refused.
func TestReapAbandonedVolume_PublishedVolumeKept(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req, pvc := failedAttempt(t, env)
	deleteClaim(t, env, pvc)
	pvs := volumeState(t, env, req.Name)
	pvs.Status.PublishedNodes = []v1alpha1.VolumePublication{{NodeID: "worker-1", InitiatorID: "nqn.host"}}
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("record publication: %v", err)
	}

	_, err := env.srv.ReapAbandonedVolume(context.Background(), req.Name)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if agentTeardownCalls(env) != 0 {
		t.Fatal("agent teardown called for a published volume")
	}
	if volumeState(t, env, req.Name) == nil {
		t.Fatal("PillarVolumeState removed")
	}
}

// TestReapAbandonedVolume_LegacyCreatePartialKept: controllers before the
// success-recording contract could report a CreatePartial lifecycle created
// (the Ready write was best-effort), so without the marker such a record may
// back a Retain volume and is never reaped.  A legacy Provisioning record
// never reported success and is.
func TestReapAbandonedVolume_LegacyCreatePartialKept(t *testing.T) {
	t.Parallel()
	for phase, wantReaped := range map[v1alpha1.PillarVolumeStatePhase]bool{
		v1alpha1.PillarVolumeStatePhaseCreatePartial: false,
		v1alpha1.PillarVolumeStatePhaseProvisioning:  true,
	} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			req, pvc := failedAttempt(t, env)
			deleteClaim(t, env, pvc)
			pvs := volumeState(t, env, req.Name)
			delete(pvs.Annotations, annotationSuccessRecorded)
			if err := env.srv.k8sClient.Update(context.Background(), pvs); err != nil {
				t.Fatalf("drop marker: %v", err)
			}
			pvs = volumeState(t, env, req.Name)
			pvs.Status.Phase = phase
			if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
				t.Fatalf("set phase: %v", err)
			}

			if got := reap(t, env, req.Name); got != wantReaped {
				t.Fatalf("legacy %s record reaped = %t, want %t", phase, got, wantReaped)
			}
		})
	}
}

// TestCreateVolume_NamedClaimGoneOrReplaced: a delayed first attempt whose
// named claim no longer exists, or whose name now belongs to a different
// claim than the "pvc-<claim UID>" volume name encodes, is abandoned and
// must not start a lifecycle.
func TestCreateVolume_NamedClaimGoneOrReplaced(t *testing.T) {
	t.Parallel()
	for name, replace := range map[string]bool{"gone": false, "replaced": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newControllerTestEnv(t)
			if replace {
				pvc := &corev1.PersistentVolumeClaim{
					Name: "data", Namespace: "default", UID: "11111111-2222-3333-4444-555555555555",
				}
				if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
					t.Fatalf("create PVC: %v", err)
				}
			}
			req := baseCreateVolumeRequest()
			req.Name = "pvc-" + reapClaimUID
			req.Parameters[paramPVCNameMeta] = "data"
			req.Parameters[paramPVCNamespaceMeta] = "default"

			_, err := env.srv.CreateVolume(context.Background(), req)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("want FailedPrecondition, got %v", err)
			}
			if env.agent.createVolumeCalls != 0 || volumeState(t, env, req.Name) != nil {
				t.Fatal("abandoned attempt started a lifecycle")
			}
		})
	}
}

// lvReapClaim prepares an import-lv claim "data" adopting data-vg/legacy
// under policy whose UID is the one the "pvc-<claim UID>" volume name
// encodes, so the abandoned-attempt detection can attribute the lifecycle.
func lvReapClaim(
	t *testing.T,
	policy string,
) (*controllerTestEnv, *csi.CreateVolumeRequest, *corev1.PersistentVolumeClaim) {
	t.Helper()
	annotations := importLVAnnotations(policy)
	env, req := newImportLVTestEnv(t, annotations)
	seeded := &corev1.PersistentVolumeClaim{}
	if err := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: lvClaimName}, seeded); err != nil {
		t.Fatalf("get seeded PVC: %v", err)
	}
	deleteClaim(t, env, seeded)
	pvc := &corev1.PersistentVolumeClaim{
		Name: lvClaimName, Namespace: "default", UID: types.UID(reapClaimUID), Annotations: annotations,
	}
	if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("recreate PVC with UID: %v", err)
	}
	req.Name = "pvc-" + reapClaimUID
	return env, req, pvc
}

// assertLVReapTeardown checks the agent teardown a reap of the abandoned LV
// adoption pvs ran: ReleaseVolume only, under the lifecycle's UID, when
// wantRelease; otherwise the Managed destructive DeleteVolume and no release.
func assertLVReapTeardown(t *testing.T, env *controllerTestEnv, pvs *v1alpha1.PillarVolumeState, wantRelease bool) {
	t.Helper()
	if !wantRelease {
		if env.agent.deleteVolumeCalls != 1 || env.agent.releaseVolumeCalls != 0 {
			t.Fatalf("Managed reap: delete=%d release=%d, want 1/0",
				env.agent.deleteVolumeCalls, env.agent.releaseVolumeCalls)
		}
		return
	}
	if env.agent.releaseVolumeCalls != 1 || agentTeardownCalls(env) != 0 {
		t.Fatalf("reap: release=%d unexport=%d delete=%d, want release only",
			env.agent.releaseVolumeCalls, env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	rel := env.agent.lastReleaseVolumeReq
	if rel.GetVolumeId() != "data-vg/legacy" || rel.GetFence().GetVolumeUid() != string(pvs.UID) {
		t.Fatalf("ReleaseVolume = %+v, want data-vg/legacy under uid %s", rel, pvs.UID)
	}
}

// TestReapAbandonedVolume_ImportLV_ReleasesNeverDeletes: an abandoned LV
// adoption whose agent refused the import (importAcquired unset, under
// either policy), or a PreserveOriginal adoption that did land but never
// exported, is ended with ReleaseVolume only — UnexportVolume+DeleteVolume
// would lvremove pre-existing data — and its LV-UUID reservation is
// released with the record.  A Managed adoption that landed is a normal
// volume and keeps the destructive teardown.
func TestReapAbandonedVolume_ImportLV_ReleasesNeverDeletes(t *testing.T) {
	t.Parallel()
	refusedImport := func(env *controllerTestEnv) {
		env.agent.importVolumeErr = status.Error(codes.FailedPrecondition, "in use: mounted at /mnt/legacy")
	}
	exportFailed := func(env *controllerTestEnv) {
		env.agent.exportVolumeErr = status.Error(codes.Unavailable, "nvmet: port busy")
	}
	for name, tc := range map[string]struct {
		policy      string
		fail        func(*controllerTestEnv)
		wantAcq     bool
		wantRelease bool
	}{
		"preserve, import refused":     {"", refusedImport, false, true},
		"managed, import refused":      {v1alpha1.ImportLVPolicyManaged, refusedImport, false, true},
		"preserve, adopted unexported": {"", exportFailed, true, true},
		"managed, adopted unexported":  {v1alpha1.ImportLVPolicyManaged, exportFailed, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req, pvc := lvReapClaim(t, tc.policy)
			tc.fail(env)
			if _, err := env.srv.CreateVolume(context.Background(), req); err == nil {
				t.Fatal("CreateVolume succeeded; want the injected failure")
			}
			pvs := volumeState(t, env, req.GetName())
			if pvs == nil || pvs.Spec.LVMSource == nil || pvs.Status.ImportAcquired != tc.wantAcq {
				t.Fatalf("failed attempt left %+v, want lvmSource pinned and importAcquired=%v", pvs, tc.wantAcq)
			}
			env.agent.importVolumeErr, env.agent.exportVolumeErr = nil, nil
			importCalls := env.agent.importVolumeCalls

			deleteClaim(t, env, pvc)
			if !reap(t, env, req.GetName()) {
				t.Fatal("abandoned LV adoption was not reaped")
			}
			assertLVReapTeardown(t, env, pvs, tc.wantRelease)
			if env.agent.importVolumeCalls != importCalls {
				t.Fatalf("reap re-ran import: calls %d -> %d", importCalls, env.agent.importVolumeCalls)
			}
			if volumeState(t, env, req.GetName()) != nil {
				t.Fatal("PillarVolumeState still exists after the reap")
			}
			if res := reservationNamed(t, env, lvReservationName()); res != nil {
				t.Fatalf("LV reservation outlived its lifecycle: %+v", res.Spec)
			}
		})
	}
}

// TestReapAbandonedVolume_ReadyWithDeletedClaimKept: a Ready LV adoption
// whose claim is gone while its Retain PersistentVolume is Released (the
// state the same-PV rebind runbook starts from) is never reaped: no agent
// call, and the record, its claimRef and the reservation stay as they were.
func TestReapAbandonedVolume_ReadyWithDeletedClaimKept(t *testing.T) {
	t.Parallel()
	env, req, pvc := lvReapClaim(t, "")
	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	before := volumeState(t, env, req.GetName())
	if before.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady || before.Spec.ClaimRef == nil {
		t.Fatalf("adoption = %+v, want Ready with a claimRef", before)
	}
	deleteClaim(t, env, pvc)
	pv := &corev1.PersistentVolume{
		Name: req.GetName(),
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Namespace: "default", Name: "data", UID: types.UID(reapClaimUID),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "pillar-csi.bhyoo.com", VolumeHandle: before.Spec.VolumeID,
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	if err := env.srv.k8sClient.Create(context.Background(), pv); err != nil {
		t.Fatalf("create Released PV: %v", err)
	}
	calls := env.agent.releaseVolumeCalls + agentTeardownCalls(env) + env.agent.importVolumeCalls

	if reap(t, env, req.GetName()) {
		t.Fatal("Ready adoption with a deleted claim was reaped")
	}
	if got := env.agent.releaseVolumeCalls + agentTeardownCalls(env) + env.agent.importVolumeCalls; got != calls {
		t.Fatalf("reap reached the agent: calls %d -> %d", calls, got)
	}
	after := volumeState(t, env, req.GetName())
	if after == nil || after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("reap changed the record:\nbefore %+v\nafter  %+v", before, after)
	}
	if res := reservationNamed(t, env, lvReservationName()); res == nil || res.Spec.OwnerVolume != req.GetName() {
		t.Fatalf("LV reservation = %+v, want still owned by %q", res, req.GetName())
	}
}
