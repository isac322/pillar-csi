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
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

func filesystemCleanupFixture(t *testing.T) (*controllerTestEnv, *v1alpha1.PillarVolumeState) {
	t.Helper()
	a, resolved := filesystemDirectoryFixture()
	env, _ := filesystemInspectionEnv(t, a)
	owner := &v1alpha1.PillarVolumeState{Name: "cleanup", Spec: v1alpha1.PillarVolumeStateSpec{
		VolumeID: "storage-node-1/nfs/directory/files-a/" + requireFilesystemImportLeaf(t, a),
		AgentRef: "storage-node-1", BackendType: "directory",
		AgentVolumeID: "files-a/" + requireFilesystemImportLeaf(t, a),
		Resolved:      resolved, FilesystemAdoption: a, ClaimRef: &v1alpha1.VolumeClaimRef{
			Namespace: "default", Name: "app", UID: "claim-owner"}}, Status: v1alpha1.PillarVolumeStateStatus{Deleting: true}}
	if err := env.srv.k8sClient.Create(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	return env, owner
}

func filesystemCleanupReservation(t *testing.T, env *controllerTestEnv, owner *v1alpha1.PillarVolumeState,
	inode uint64, pool, agent, backendType, pvName, claimUID string) *v1alpha1.PillarVolumeReservation {
	t.Helper()
	a := owner.Spec.FilesystemAdoption.DeepCopy()
	a.Inode = v1alpha1.FormatFilesystemAdoptionInode(inode)
	a.ResourceID = a.FilesystemID + ":" + a.Inode
	claim := &v1alpha1.VolumeClaimRef{Namespace: "default", Name: "app", UID: claimUID}
	agentVolumeID := pool + "/" + requireFilesystemImportLeaf(t, a)
	native := requireFilesystemResourceID(t, a)
	rsv := backendReservation{agent: agent, backendType: backendType, key: agentVolumeID, resourceID: native}
	if err := env.srv.reserveBackendVolume(context.Background(), pvName, rsv,
		filesystemImportSubject(a.CanonicalSource), claim); err != nil {
		t.Fatal(err)
	}
	res := &v1alpha1.PillarVolumeReservation{}
	if err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{
		Name: reservationName(agent, backendType, agentVolumeID, native)}, res); err != nil {
		t.Fatal(err)
	}
	return res
}

func requireFilesystemReservationExists(
	t *testing.T, env *controllerTestEnv, res *v1alpha1.PillarVolumeReservation, exists bool,
) {
	t.Helper()
	current := &v1alpha1.PillarVolumeReservation{}
	err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: res.Name}, current)
	if exists {
		if err != nil || current.UID != res.UID {
			t.Fatalf("foreign/current reservation %q changed: %+v %v", res.Name, current, err)
		}
	} else if !k8serrors.IsNotFound(err) {
		t.Fatalf("owned candidate %q remains: %v", res.Name, err)
	}
}

func TestFilesystemReservationCleanup_CollectsCandidatesOnlyForExactOwner(t *testing.T) {
	t.Parallel()
	env, owner := filesystemCleanupFixture(t)
	winner := filesystemCleanupReservation(t, env, owner,
		42, "files-a", "storage-node-1", "directory", owner.Name, "claim-owner")
	candidate := filesystemCleanupReservation(t, env, owner,
		43, "alias-pool", "storage-node-1", "directory", owner.Name, "claim-owner")
	preserved := []*v1alpha1.PillarVolumeReservation{
		filesystemCleanupReservation(t, env, owner,
			44, "files-a", "storage-node-1", "directory", owner.Name, "foreign-claim"),
		filesystemCleanupReservation(t, env, owner,
			45, "files-a", "storage-node-2", "directory", owner.Name, "claim-owner"),
		filesystemCleanupReservation(t, env, owner,
			46, "files-a", "storage-node-1", "zfs-dataset", owner.Name, "claim-owner"),
		filesystemCleanupReservation(t, env, owner, 47, "files-a", "storage-node-1", "directory", "other-pv", "claim-owner"),
	}
	if err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID,
		reservationOf(owner)); err != nil {
		t.Fatal(err)
	}
	requireFilesystemReservationExists(t, env, winner, false)
	requireFilesystemReservationExists(t, env, candidate, false)
	for _, res := range preserved {
		requireFilesystemReservationExists(t, env, res, true)
	}
	current := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: owner.Name}, current); err != nil {
		t.Fatal(err)
	}
	if current.UID != owner.UID || !current.Status.Deleting {
		t.Fatal("reservation cleanup altered lifecycle")
	}
}

func TestFilesystemReservationCleanup_RefusesActiveOrUnidentifiedOwner(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{
		"changed UID", "empty UID", "not deleting", "nil claim", "empty claim UID", "published",
	} {
		t.Run(mutation, func(t *testing.T) {
			env, owner := filesystemCleanupFixture(t)
			res := filesystemCleanupReservation(t, env, owner,
				42, "files-a", "storage-node-1", "directory", owner.Name, "claim-owner")
			expected, want := mutateFilesystemCleanupOwner(t, env, owner, mutation)
			err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, expected,
				reservationOf(owner))
			if status.Code(err) != want {
				t.Fatalf("cleanup %s: %v, want %v", mutation, err, want)
			}
			requireFilesystemReservationExists(t, env, res, true)
		})
	}
}

func mutateFilesystemCleanupOwner(
	t *testing.T, env *controllerTestEnv, owner *v1alpha1.PillarVolumeState, mutation string,
) (types.UID, codes.Code) {
	t.Helper()
	switch mutation {
	case "changed UID":
		return "old-owner", codes.Aborted
	case "empty UID":
		return "", codes.Aborted
	case "nil claim", "empty claim UID":
		if mutation == "nil claim" {
			owner.Spec.ClaimRef = nil
		} else {
			owner.Spec.ClaimRef.UID = ""
		}
		if err := env.srv.k8sClient.Update(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
		return owner.UID, codes.FailedPrecondition
	case "not deleting":
		owner.Status.Deleting = false
	case "published":
		owner.Status.PublishedNodes = []v1alpha1.VolumePublication{{NodeID: "worker"}}
	}
	if err := env.srv.k8sClient.Status().Update(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	return owner.UID, codes.Aborted
}

type filesystemCleanupReader struct {
	ctrlclient.Reader
	ownerName          string
	reads, interruptAt int
	interrupt          func() error
}

func (r *filesystemCleanupReader) Get(
	ctx context.Context, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption,
) error {
	if _, ok := obj.(*v1alpha1.PillarVolumeState); ok && key.Name == r.ownerName {
		r.reads++
		if r.reads == r.interruptAt {
			if err := r.interrupt(); err != nil {
				return err
			}
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestFilesystemReservationCleanup_RechecksBeforeEachDeleteAndCompletion(t *testing.T) {
	t.Parallel()
	for _, readNumber := range []int{2, 3, 4} {
		t.Run(strconv.Itoa(readNumber), func(t *testing.T) {
			env, owner := filesystemCleanupFixture(t)
			first := filesystemCleanupReservation(t, env, owner,
				42, "files-a", "storage-node-1", "directory", owner.Name, "claim-owner")
			second := filesystemCleanupReservation(t, env, owner,
				43, "alias-pool", "storage-node-1", "directory", owner.Name, "claim-owner")
			base := env.srv.k8sClient
			env.srv.apiReader = &filesystemCleanupReader{
				Reader: base, ownerName: owner.Name, interruptAt: readNumber,
				interrupt: func() error { return replaceFilesystemCleanupOwner(base, owner.Name) },
			}
			err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID,
				reservationOf(owner))
			if status.Code(err) != codes.Aborted {
				t.Fatalf("replacement lifecycle cleanup continued: %v", err)
			}
			requireFilesystemCleanupInterrupted(t, env, readNumber, first, second, owner.Name)
		})
	}
}

func replaceFilesystemCleanupOwner(base ctrlclient.Client, name string) error {
	current := &v1alpha1.PillarVolumeState{}
	if err := base.Get(context.Background(), types.NamespacedName{Name: name}, current); err != nil {
		return err
	}
	if err := base.Delete(context.Background(), current); err != nil {
		return err
	}
	replacement := current.DeepCopy()
	replacement.UID = "replacement-owner"
	replacement.ResourceVersion = ""
	return base.Create(context.Background(), replacement)
}

func requireFilesystemCleanupInterrupted(
	t *testing.T, env *controllerTestEnv, readNumber int,
	first, second *v1alpha1.PillarVolumeReservation, ownerName string,
) {
	t.Helper()
	var remaining v1alpha1.PillarVolumeReservationList
	if err := env.srv.k8sClient.List(context.Background(), &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 4-readNumber {
		t.Fatalf("cleanup crossed lifecycle change: remaining %d, want %d", len(remaining.Items), 4-readNumber)
	}
	if readNumber == 2 {
		requireFilesystemReservationExists(t, env, first, true)
		requireFilesystemReservationExists(t, env, second, true)
	}
	replacement := filesystemPVS(t, env, ownerName)
	if replacement.UID != "replacement-owner" {
		t.Fatal("replacement lifecycle changed")
	}
}

type filesystemCleanupConflictClient struct {
	ctrlclient.Client
	conflict bool
}

func (c *filesystemCleanupConflictClient) Delete(
	ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption,
) error {
	if _, ok := obj.(*v1alpha1.PillarVolumeReservation); ok && c.conflict {
		return k8serrors.NewConflict(schema.GroupResource{
			Group: "pillar-csi.bhyoo.com", Resource: "pillarvolumereservations",
		}, obj.GetName(), nil)
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func TestFilesystemReservationCleanup_DeleteConflictKeepsReservationForRetry(t *testing.T) {
	t.Parallel()
	env, owner := filesystemCleanupFixture(t)
	res := filesystemCleanupReservation(t, env, owner,
		42, "files-a", "storage-node-1", "directory", owner.Name, "claim-owner")
	conflicting := &filesystemCleanupConflictClient{Client: env.srv.k8sClient, conflict: true}
	env.srv.k8sClient = conflicting
	releaseErr := env.srv.releaseBackendVolume(
		context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID, reservationOf(owner),
	)
	if status.Code(releaseErr) != codes.Aborted {
		t.Fatalf("reservation conflict was not retained: %v", releaseErr)
	}
	requireFilesystemReservationExists(t, env, res, true)
	conflicting.conflict = false
	if err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID,
		reservationOf(owner)); err != nil {
		t.Fatal(err)
	}
	requireFilesystemReservationExists(t, env, res, false)
}

func TestFilesystemReservationCleanup_RefusesNoncanonicalCandidates(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"native key", "object name"} {
		t.Run(invalid, func(t *testing.T) {
			env, owner := filesystemCleanupFixture(t)
			native := requireFilesystemResourceID(t, owner.Spec.FilesystemAdoption)
			name := reservationName(owner.Spec.AgentRef, owner.Spec.BackendType, owner.Spec.AgentVolumeID, native)
			if invalid == "native key" {
				native = "filesystem/not-a-native-digest"
			} else {
				name = "rsv-noncanonical-name"
			}
			res := &v1alpha1.PillarVolumeReservation{Name: name, Spec: v1alpha1.PillarVolumeReservationSpec{
				OwnerVolume: owner.Name, AgentRef: owner.Spec.AgentRef, BackendType: owner.Spec.BackendType,
				AgentVolumeID: owner.Spec.AgentVolumeID, ClaimRef: owner.Spec.ClaimRef, FilesystemResourceID: native}}
			if err := env.srv.k8sClient.Create(context.Background(), res); err != nil {
				t.Fatal(err)
			}
			releaseErr := env.srv.releaseBackendVolume(
				context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID, reservationOf(owner),
			)
			if status.Code(releaseErr) != codes.FailedPrecondition {
				t.Fatalf("invalid candidate cleanup continued: %v", releaseErr)
			}
			requireFilesystemReservationExists(t, env, res, true)
		})
	}
}

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
