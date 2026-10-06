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

// Issue #163 reservation tests: an import-lv lifecycle reserves its LV by
// the stable LV UUID, so a renamed locator cannot open a second lifecycle on
// the same LV, and reserve, re-verify and release all use that one key.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

// lvClaimRequest creates claim default/pvcName with annotations and returns a
// CreateVolume request for it cloned from req.
func lvClaimRequest(
	t *testing.T,
	env *controllerTestEnv,
	req *csi.CreateVolumeRequest,
	pvcName string,
	annotations map[string]string,
) *csi.CreateVolumeRequest {
	t.Helper()
	err := env.srv.k8sClient.Create(context.Background(), &corev1.PersistentVolumeClaim{
		Name:        pvcName,
		Namespace:   "default",
		Annotations: annotations,
	})
	if err != nil {
		t.Fatalf("create claim %q: %v", pvcName, err)
	}
	req2, ok := proto.Clone(req).(*csi.CreateVolumeRequest)
	if !ok {
		t.Fatal("clone CreateVolumeRequest")
	}
	req2.Name = "pvc-" + pvcName
	req2.Parameters[paramPVCNameMeta] = pvcName
	return req2
}

// replaceLVReservation deletes the LV-UUID reservation and re-creates it for
// owner with a new UID and a resourceVersion different from the replaced
// one — another lifecycle taking the reservation over on the API server.
func replaceLVReservation(t *testing.T, env *controllerTestEnv, owner string) {
	t.Helper()
	ctx := context.Background()
	name := lvReservationName()
	spec := v1alpha1.PillarVolumeReservationSpec{
		AgentRef:      "storage-node-1",
		BackendType:   "lvm-lv",
		AgentVolumeID: lvLVUUID,
	}
	oldRV := ""
	if old := reservationNamed(t, env, name); old != nil {
		spec, oldRV = old.Spec, old.ResourceVersion
		if err := env.srv.k8sClient.Delete(ctx, old); err != nil {
			t.Fatalf("delete reservation: %v", err)
		}
	}
	spec.OwnerVolume = owner
	spec.ClaimRef = &v1alpha1.VolumeClaimRef{UID: "uid-" + owner, Namespace: "default", Name: owner}
	res := &v1alpha1.PillarVolumeReservation{Name: name, Spec: spec}
	if err := env.srv.k8sClient.Create(ctx, res); err != nil {
		t.Fatalf("create replacement reservation: %v", err)
	}
	for i := 0; res.ResourceVersion == oldRV; i++ {
		res.Labels = map[string]string{"touched": strconv.Itoa(i)}
		if err := env.srv.k8sClient.Update(ctx, res); err != nil {
			t.Fatalf("update replacement reservation: %v", err)
		}
	}
}

// assertSecondLVClaimRefused checks the second claim req2 for the LV that
// req adopted lost the reservation: refused naming the owner, no agent call,
// no record, and the reservation still owned by req's lifecycle.
func assertSecondLVClaimRefused(
	t *testing.T,
	env *controllerTestEnv,
	req, req2 *csi.CreateVolumeRequest,
	err error,
) {
	t.Helper()
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), req.GetName()) {
		t.Fatalf("second claim: err = %v, want FailedPrecondition naming %q", err, req.GetName())
	}
	if env.agent.importVolumeCalls != 1 {
		t.Fatalf("second claim reached the agent: ImportVolume calls = %d", env.agent.importVolumeCalls)
	}
	if pvs := volumeState(t, env, req2.GetName()); pvs != nil {
		t.Fatalf("refused second claim left PillarVolumeState %+v", pvs.Spec)
	}
	res := reservationNamed(t, env, lvReservationName())
	if res == nil || res.Spec.OwnerVolume != req.GetName() {
		t.Fatalf("LV reservation = %+v, want still owned by %q", res, req.GetName())
	}
}

// TestCreateVolume_ImportLV_ReservationKeyedByLVUUID: a second claim for the
// same LV — by the same locator or by a renamed one carrying the same UUIDs —
// loses the reservation and never reaches the agent; a different LV is free.
func TestCreateVolume_ImportLV_ReservationKeyedByLVUUID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		annotation string
		wantErr    bool
	}{
		"same locator":    {annotation: importLVValue("data-vg", "legacy", lvVGUUID, lvLVUUID), wantErr: true},
		"renamed locator": {annotation: importLVValue("data-vg", "renamed", lvVGUUID, lvLVUUID), wantErr: true},
		"other lv":        {annotation: importLVValue("data-vg", "other", lvVGUUID, lvOtherUUID)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportLVTestEnv(t, importLVAnnotations(""))
			adoptLV(t, env, req)

			req2 := lvClaimRequest(t, env, req, "data-2",
				map[string]string{v1alpha1.AnnotationImportLV: tc.annotation})
			_, err := env.srv.CreateVolume(context.Background(), req2)
			if tc.wantErr {
				assertSecondLVClaimRefused(t, env, req, req2, err)
				return
			}
			if err != nil {
				t.Fatalf("import of a different LV: %v", err)
			}
			if env.agent.importVolumeCalls != 2 {
				t.Fatalf("ImportVolume calls = %d, want 2", env.agent.importVolumeCalls)
			}
		})
	}
}

// TestCreateVolume_ImportLV_RetainedOwnerBlocksNewClaim: a Ready lifecycle
// (e.g. a retained PV) holding the LV reservation blocks a new claim.
func TestCreateVolume_ImportLV_RetainedOwnerBlocksNewClaim(t *testing.T) {
	t.Parallel()
	retained := &v1alpha1.PillarVolumeState{
		Name: "pvc-retained",
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      lvVolumeID,
			AgentVolumeID: "data-vg/legacy",
			AgentRef:      "storage-node-1",
			BackendType:   "lvm-lv",
			ProtocolType:  "nvmeof-tcp",
			LVMSource:     wantLVSource(true),
			ClaimRef:      &v1alpha1.VolumeClaimRef{UID: "uid-old", Namespace: "default", Name: "old"},
		},
		Status: v1alpha1.PillarVolumeStateStatus{
			Phase:          v1alpha1.PillarVolumeStatePhaseReady,
			ImportAcquired: true,
		},
	}
	held := &v1alpha1.PillarVolumeReservation{
		Name: lvReservationName(),
		Spec: v1alpha1.PillarVolumeReservationSpec{
			AgentRef:      "storage-node-1",
			BackendType:   "lvm-lv",
			AgentVolumeID: lvLVUUID,
			OwnerVolume:   "pvc-retained",
			ClaimRef:      &v1alpha1.VolumeClaimRef{UID: "uid-old", Namespace: "default", Name: "old"},
		},
	}
	env, req := newImportLVTestEnv(t, importLVAnnotations(""), retained, held)

	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "pvc-retained") {
		t.Fatalf("new claim for a retained LV: err = %v, want FailedPrecondition naming pvc-retained", err)
	}
	if env.agent.importVolumeCalls != 0 || env.agent.createVolumeCalls != 0 {
		t.Fatalf("new claim reached the agent: import=%d create=%d",
			env.agent.importVolumeCalls, env.agent.createVolumeCalls)
	}
	res := reservationNamed(t, env, lvReservationName())
	if res == nil || res.Spec.OwnerVolume != "pvc-retained" {
		t.Fatalf("retained reservation = %+v, want owner pvc-retained", res)
	}
}

// TestCreateVolume_ImportLV_ReservationRecheckedBeforeImport: the LV-UUID
// reservation is re-read right before ImportVolume; one that disappeared or
// was taken over since this attempt reserved it stops the adoption.
func TestCreateVolume_ImportLV_ReservationRecheckedBeforeImport(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change   func(t *testing.T, env *controllerTestEnv)
		wantCode codes.Code
	}{
		"deleted": {
			change: func(t *testing.T, env *controllerTestEnv) {
				t.Helper()
				res := reservationNamed(t, env, lvReservationName())
				if err := env.srv.k8sClient.Delete(context.Background(), res); err != nil {
					t.Fatalf("delete reservation: %v", err)
				}
			},
			wantCode: codes.Aborted,
		},
		"taken over": {
			change: func(t *testing.T, env *controllerTestEnv) {
				t.Helper()
				replaceLVReservation(t, env, "pvc-other")
			},
			wantCode: codes.FailedPrecondition,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, req := newImportLVTestEnv(t, importLVAnnotations(""))
			hook := &reservationHookReader{Reader: env.srv.k8sClient}
			fired := false
			// Act on the first reservation read after this attempt created
			// the LV reservation: the re-check right before the agent call.
			hook.before = func(int) {
				if !fired && reservationNamed(t, env, lvReservationName()) != nil {
					fired = true
					tc.change(t, env)
				}
			}
			env.srv.apiReader = hook

			_, err := env.srv.CreateVolume(context.Background(), req)
			if !fired {
				t.Fatal("CreateVolume never re-read the LV-UUID reservation after reserving it")
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("CreateVolume: err = %v, want %v", err, tc.wantCode)
			}
			if env.agent.importVolumeCalls != 0 {
				t.Fatal("ImportVolume ran although the reservation no longer belonged to this lifecycle")
			}
		})
	}
}

// TestDeleteVolume_ImportLV_ReleasesUUIDKeyedReservation: DeleteVolume of a
// preserving LV drops exactly the LV-UUID reservation it reserved, never one
// that another lifecycle holds now, and a new claim can then adopt the LV.
func TestDeleteVolume_ImportLV_ReleasesUUIDKeyedReservation(t *testing.T) {
	t.Parallel()

	t.Run("released and re-adoptable", func(t *testing.T) {
		t.Parallel()
		env, req := newImportLVTestEnv(t, importLVAnnotations(""))
		resp := adoptLV(t, env, req)
		_, err := env.srv.DeleteVolume(context.Background(),
			&csi.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()})
		if err != nil {
			t.Fatalf("DeleteVolume: %v", err)
		}
		if res := reservationNamed(t, env, lvReservationName()); res != nil {
			t.Fatalf("LV reservation outlived its lifecycle: %+v", res.Spec)
		}

		req2 := lvClaimRequest(t, env, req, "data-2", importLVAnnotations(""))
		if _, err := env.srv.CreateVolume(context.Background(), req2); err != nil {
			t.Fatalf("re-adoption by a new claim: %v", err)
		}
		res := reservationNamed(t, env, lvReservationName())
		if res == nil || res.Spec.OwnerVolume != req2.GetName() {
			t.Fatalf("LV reservation = %+v, want owned by %q", res, req2.GetName())
		}
	})

	t.Run("other owner untouched", func(t *testing.T) {
		t.Parallel()
		env, req := newImportLVTestEnv(t, importLVAnnotations(""))
		resp := adoptLV(t, env, req)
		replaceLVReservation(t, env, "pvc-other")
		_, err := env.srv.DeleteVolume(context.Background(),
			&csi.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()})
		if err != nil {
			t.Fatalf("DeleteVolume: %v", err)
		}
		res := reservationNamed(t, env, lvReservationName())
		if res == nil || res.Spec.OwnerVolume != "pvc-other" {
			t.Fatalf("another lifecycle's reservation = %+v, want kept for pvc-other", res)
		}
	})

	t.Run("replaced between read and delete", testLVReleaseOverReplacedReservation)
}

// testLVReleaseOverReplacedReservation: a reservation replaced between
// DeleteVolume's read and its delete is never removed by the stale release;
// the release is refused with Aborted.
func testLVReleaseOverReplacedReservation(t *testing.T) {
	t.Parallel()
	env, req := newImportLVTestEnv(t, importLVAnnotations(""))
	resp := adoptLV(t, env, req)
	before := reservationNamed(t, env, lvReservationName())

	hook := &reservationHookReader{Reader: env.srv.k8sClient}
	replaced := false
	hook.after = func(int) {
		if !replaced {
			replaced = true
			replaceLVReservation(t, env, req.GetName())
		}
	}
	env.srv.apiReader = hook

	_, err := env.srv.DeleteVolume(context.Background(),
		&csi.DeleteVolumeRequest{VolumeId: resp.GetVolume().GetVolumeId()})
	if !replaced {
		t.Fatal("DeleteVolume never read the LV-UUID reservation")
	}
	if status.Code(err) != codes.Aborted {
		t.Fatalf("release over a replaced reservation: err = %v, want Aborted", err)
	}
	res := reservationNamed(t, env, lvReservationName())
	if res == nil || res.UID == before.UID {
		t.Fatalf("stale release removed the replacement reservation (now %+v)", res)
	}
}
