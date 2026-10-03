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
	"strconv"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	if err := env.srv.reserveBackendVolume(context.Background(), pvName, agent, backendType, agentVolumeID,
		a.CanonicalSource, claim, native); err != nil {
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
	if err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID); err != nil {
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
			err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, expected)
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
			err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID)
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
		context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID,
	)
	if status.Code(releaseErr) != codes.Aborted {
		t.Fatalf("reservation conflict was not retained: %v", releaseErr)
	}
	requireFilesystemReservationExists(t, env, res, true)
	conflicting.conflict = false
	if err := env.srv.releaseBackendVolume(context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID); err != nil {
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
				context.Background(), owner.Name, owner.Spec.VolumeID, owner.UID,
			)
			if status.Code(releaseErr) != codes.FailedPrecondition {
				t.Fatalf("invalid candidate cleanup continued: %v", releaseErr)
			}
			requireFilesystemReservationExists(t, env, res, true)
		})
	}
}
