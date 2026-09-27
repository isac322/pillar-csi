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

	v1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
)

const reapClaimUID = "0f3c2a5e-7b41-4c8e-9d3a-1e2f3a4b5c6d"

// failedAttempt runs a CreateVolume for the claim reapClaimUID whose backend
// creation fails at the agent (issue #97), leaving its PillarVolumeState in
// phase Provisioning, and returns the request for retries.
func failedAttempt(t *testing.T, env *controllerTestEnv) *csi.CreateVolumeRequest {
	t.Helper()
	env.agent.createVolumeErr = status.Error(codes.Internal, "zfs create: out of space")
	req := baseCreateVolumeRequest()
	req.Name = "pvc-" + reapClaimUID
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume: want Internal, got %v", err)
	}
	pvs, found, err := env.srv.readVolumeState(context.Background(), req.Name)
	if err != nil || !found {
		t.Fatalf("PillarVolumeState of the failed attempt: found=%t err=%v", found, err)
	}
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseProvisioning {
		t.Fatalf("phase = %q, want Provisioning", pvs.Status.Phase)
	}
	env.agent.createVolumeErr = nil
	return req
}

func createClaim(t *testing.T, env *controllerTestEnv, uid string, finalizers ...string) *corev1.PersistentVolumeClaim {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "default", UID: types.UID(uid), Finalizers: finalizers,
	}}
	if err := env.srv.k8sClient.Create(context.Background(), pvc); err != nil {
		t.Fatalf("create PVC: %v", err)
	}
	return pvc
}

func volumeStateExists(t *testing.T, env *controllerTestEnv, name string) *v1alpha1.PillarVolumeState {
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

// TestReapAbandonedVolume_FailedAttemptWithoutClaim is the issue #97
// regression: once the claim of a failed attempt is gone, the lifecycle is
// ended through the fenced agent teardown and the record is removed.
func TestReapAbandonedVolume_FailedAttemptWithoutClaim(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := failedAttempt(t, env)

	if !reap(t, env, req.Name) {
		t.Fatal("abandoned attempt was not reaped")
	}
	if env.agent.unexportVolumeCalls != 1 || env.agent.deleteVolumeCalls != 1 {
		t.Fatalf("agent teardown: unexport=%d delete=%d, want 1 and 1 (the backend may exist)",
			env.agent.unexportVolumeCalls, env.agent.deleteVolumeCalls)
	}
	if volumeStateExists(t, env, req.Name) != nil {
		t.Fatal("PillarVolumeState still exists after the teardown")
	}
}

// TestReapAbandonedVolume_LiveClaimKeepsRetryIdempotent: while the claim
// exists the provisioner retries, so the record must survive and the retry
// must still succeed on it.
func TestReapAbandonedVolume_LiveClaimKeepsRetryIdempotent(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := failedAttempt(t, env)
	createClaim(t, env, reapClaimUID)
	before := volumeStateExists(t, env, req.Name)

	if reap(t, env, req.Name) {
		t.Fatal("volume of a live claim was reaped")
	}
	if env.agent.unexportVolumeCalls+env.agent.deleteVolumeCalls != 0 {
		t.Fatal("agent teardown called for a live claim")
	}
	after := volumeStateExists(t, env, req.Name)
	if after == nil || after.UID != before.UID || after.Status.Deleting {
		t.Fatalf("record of a live claim changed: %+v", after)
	}

	if _, err := env.srv.CreateVolume(context.Background(), req); err != nil {
		t.Fatalf("CreateVolume retry: %v", err)
	}
}

// TestReapAbandonedVolume_TerminatingClaim: a claim being deleted is never
// provisioned again.
func TestReapAbandonedVolume_TerminatingClaim(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := failedAttempt(t, env)
	pvc := createClaim(t, env, reapClaimUID, "kubernetes.io/pvc-protection")
	if err := env.srv.k8sClient.Delete(context.Background(), pvc); err != nil {
		t.Fatalf("delete PVC: %v", err)
	}

	if !reap(t, env, req.Name) {
		t.Fatal("volume of a terminating claim was not reaped")
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
			req := failedAttempt(t, env)
			pvs := volumeStateExists(t, env, req.Name)
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
			if volumeStateExists(t, env, req.Name) == nil {
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
	if volumeStateExists(t, env, req.Name) == nil {
		t.Fatal("PillarVolumeState removed")
	}
}

// TestReapAbandonedVolume_ClaimRefFromProvisionerMetadata: with
// --extra-create-metadata the claim is recorded, so a volume with a custom
// name prefix is attributed through it.
func TestReapAbandonedVolume_ClaimRefFromProvisionerMetadata(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	env.agent.createVolumeErr = status.Error(codes.Internal, "out of space")
	req := baseCreateVolumeRequest()
	req.Name = "custom-" + reapClaimUID
	req.Parameters[paramPVCUIDMeta] = reapClaimUID
	req.Parameters[paramPVCNameMeta] = "data"
	req.Parameters[paramPVCNamespaceMeta] = "default"
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume: want Internal, got %v", err)
	}

	pvs := volumeStateExists(t, env, req.Name)
	want := &v1alpha1.VolumeClaimRef{UID: reapClaimUID, Namespace: "default", Name: "data"}
	if pvs == nil || pvs.Spec.ClaimRef == nil || *pvs.Spec.ClaimRef != *want {
		t.Fatalf("claimRef = %+v, want %+v", pvs, want)
	}

	pvc := createClaim(t, env, reapClaimUID)
	if reap(t, env, req.Name) {
		t.Fatal("volume of a live claim was reaped")
	}
	if err := env.srv.k8sClient.Delete(context.Background(), pvc); err != nil {
		t.Fatalf("delete PVC: %v", err)
	}
	if !reap(t, env, req.Name) {
		t.Fatal("volume of a deleted claim was not reaped")
	}
}

// TestReapAbandonedVolume_TeardownFailureFailsClosed: an agent failure keeps
// the record marked deleting, refuses further provisioning of it, and the
// retry completes the teardown.
func TestReapAbandonedVolume_TeardownFailureFailsClosed(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := failedAttempt(t, env)
	env.agent.deleteVolumeErr = status.Error(codes.Internal, "dataset is busy")

	if _, err := env.srv.ReapAbandonedVolume(context.Background(), req.Name); err == nil {
		t.Fatal("ReapAbandonedVolume succeeded although the agent failed")
	}
	pvs := volumeStateExists(t, env, req.Name)
	if pvs == nil || !pvs.Status.Deleting {
		t.Fatalf("record after a failed teardown = %+v, want kept and marked deleting", pvs)
	}
	if _, err := env.srv.CreateVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateVolume on a volume being reaped: want FailedPrecondition, got %v", err)
	}

	env.agent.deleteVolumeErr = nil
	if !reap(t, env, req.Name) {
		t.Fatal("retry did not finish the teardown")
	}
	if volumeStateExists(t, env, req.Name) != nil {
		t.Fatal("PillarVolumeState still exists after the retried teardown")
	}
}

// TestReapAbandonedVolume_PublishedVolumeKept: a recorded publication means a
// node may still hold the volume; the deletion is refused.
func TestReapAbandonedVolume_PublishedVolumeKept(t *testing.T) {
	t.Parallel()
	env := newControllerTestEnv(t)
	req := failedAttempt(t, env)
	pvs := volumeStateExists(t, env, req.Name)
	pvs.Status.PublishedNodes = []v1alpha1.VolumePublication{{NodeID: "worker-1", InitiatorID: "nqn.host"}}
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatalf("record publication: %v", err)
	}

	_, err := env.srv.ReapAbandonedVolume(context.Background(), req.Name)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if env.agent.unexportVolumeCalls+env.agent.deleteVolumeCalls != 0 {
		t.Fatal("agent teardown called for a published volume")
	}
	if volumeStateExists(t, env, req.Name) == nil {
		t.Fatal("PillarVolumeState removed")
	}
}
