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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const reapClaimUID = "0f3c2a5e-7b41-4c8e-9d3a-1e2f3a4b5c6d"

func createClaim(t *testing.T, env *controllerTestEnv, finalizers ...string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "default", UID: types.UID(reapClaimUID), Finalizers: finalizers,
	}}
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
				ObjectMeta: metav1.ObjectMeta{Name: pvName},
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
				pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
					Name: "data", Namespace: "default", UID: "11111111-2222-3333-4444-555555555555",
				}}
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
