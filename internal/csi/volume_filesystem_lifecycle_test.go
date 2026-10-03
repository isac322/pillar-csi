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
	"errors"
	"reflect"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

func filesystemLifecycleState(t *testing.T) *v1alpha1.PillarVolumeState {
	t.Helper()
	adoption, resolved := filesystemDirectoryFixture()
	resolved.Protocol = v1alpha1.ProtocolSpec{NFS: &v1alpha1.NFSConfig{}}
	leaf, err := filesystemImportLeaf(adoption)
	lifecycleRequireSuccess(t, err)
	agentID := resolved.Backend.PoolName() + "/" + leaf
	return &v1alpha1.PillarVolumeState{
		Name:        "pvc-" + reapClaimUID,
		UID:         types.UID("11111111-2222-3333-4444-555566667777"),
		Annotations: map[string]string{annotationSuccessRecorded: annotationValueTrue},
		Spec: v1alpha1.PillarVolumeStateSpec{
			VolumeID:      "storage-node-1/nfs/directory/" + agentID + ".0123456789abcdef0123456789abcdef",
			AgentVolumeID: agentID, AgentRef: "storage-node-1",
			BackendType: string(v1alpha1.BackendIDDirectory), ProtocolType: string(v1alpha1.ProtocolIDNFS),
			CapacityBytes: 4096, FilesystemAdoption: adoption, Resolved: resolved,
			ClaimRef: &v1alpha1.VolumeClaimRef{UID: reapClaimUID, Namespace: "default", Name: "data"},
		},
		Status: v1alpha1.PillarVolumeStateStatus{Phase: v1alpha1.PillarVolumeStatePhaseProvisioning},
	}
}

func filesystemLifecycleEnv(t *testing.T, pvs *v1alpha1.PillarVolumeState) *controllerTestEnv {
	t.Helper()
	env := newControllerTestEnv(t)
	env.srv.driverName = v1alpha1.FileCSIDriver
	if pvs != nil {
		if err := env.srv.k8sClient.Create(context.Background(), pvs); err != nil {
			t.Fatal(err)
		}
		pvs = volumeState(t, env, pvs.Name)
		resourceID, err := filesystemResourceID(pvs.Spec.FilesystemAdoption)
		lifecycleRequireSuccess(t, err)
		err = env.srv.reserveBackendVolume(context.Background(), pvs.Name, pvs.Spec.AgentRef,
			pvs.Spec.BackendType, pvs.Spec.AgentVolumeID, pvs.Spec.FilesystemAdoption.CanonicalSource,
			pvs.Spec.ClaimRef, resourceID)
		lifecycleRequireSuccess(t, err)
	}
	return env
}

func filesystemLifecycleReservation(
	t *testing.T, env *controllerTestEnv, pvs *v1alpha1.PillarVolumeState,
) *v1alpha1.PillarVolumeReservation {
	t.Helper()
	res := &v1alpha1.PillarVolumeReservation{}
	resourceID, err := filesystemResourceID(pvs.Spec.FilesystemAdoption)
	lifecycleRequireSuccess(t, err)
	name := reservationName(pvs.Spec.AgentRef, pvs.Spec.BackendType, pvs.Spec.AgentVolumeID, resourceID)
	err = env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, res)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// lifecycleConflictClient changes authoritative state before rejecting a CAS.
// It models a different controller winning the write, not a source-text check.
type lifecycleConflictClient struct {
	ctrlclient.Client
	beforeStatus func(context.Context, ctrlclient.Object) error
	beforeCreate func(context.Context, ctrlclient.Object) error
}

func (c *lifecycleConflictClient) Create(
	ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.CreateOption,
) error {
	if c.beforeCreate != nil {
		return c.beforeCreate(ctx, obj)
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *lifecycleConflictClient) Status() ctrlclient.SubResourceWriter {
	return lifecycleConflictStatus{SubResourceWriter: c.Client.Status(), owner: c}
}

type lifecycleConflictStatus struct {
	ctrlclient.SubResourceWriter
	owner *lifecycleConflictClient
}

func (w lifecycleConflictStatus) Update(
	ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.SubResourceUpdateOption,
) error {
	if hook := w.owner.beforeStatus; hook != nil {
		w.owner.beforeStatus = nil
		return hook(ctx, obj)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func lifecycleRequireSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func lifecycleRequireUnchanged(t *testing.T, env *controllerTestEnv, before *v1alpha1.PillarVolumeState) {
	t.Helper()
	after := volumeState(t, env, before.Name)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("lifecycle changed: before=%+v after=%+v", before, after)
	}
}

func lifecycleRequireAcquired(t *testing.T, pvs *v1alpha1.PillarVolumeState) {
	t.Helper()
	if !pvs.Status.ImportAcquired || importNeverAdopted(pvs) {
		t.Fatalf("adoption not durable: %+v", pvs.Status)
	}
}

func lifecycleCreateWinner(
	ctx context.Context, base ctrlclient.Client, obj ctrlclient.Object,
	mutate func(*v1alpha1.PillarVolumeState),
) error {
	pvs, ok := obj.(*v1alpha1.PillarVolumeState)
	if !ok {
		return errors.New("lifecycle create hook requires a PillarVolumeState")
	}
	winner := pvs.DeepCopy()
	mutate(winner)
	if err := base.Create(ctx, winner); err != nil {
		return err
	}
	resource := schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "pillarvolumestates"}
	return k8serrors.NewAlreadyExists(resource, winner.Name)
}

func lifecycleStatusWinner(
	ctx context.Context, base ctrlclient.Client, obj ctrlclient.Object,
	mutate func(*v1alpha1.PillarVolumeState),
) error {
	winner := &v1alpha1.PillarVolumeState{}
	if err := base.Get(ctx, ctrlclient.ObjectKeyFromObject(obj), winner); err != nil {
		return err
	}
	mutate(winner)
	if err := base.Update(ctx, winner); err != nil {
		return err
	}
	return k8serrors.NewConflict(
		schema.GroupResource{Resource: "pillarvolumestates"}, winner.Name, errors.New("another controller won"),
	)
}

func TestEnsureVolumeState_FilesystemImmutableRetry(t *testing.T) {
	t.Parallel()
	mutations := map[string]func(*v1alpha1.PillarVolumeStateSpec){
		"source":          func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.CanonicalSource += "-other" },
		"resource":        func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.ResourceID += "-other" },
		"filesystem":      func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.FilesystemID += "-other" },
		"inode":           func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.Inode = "43" },
		"quota scope":     func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.ProjectID++ },
		"mount source":    func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.HostPath = "/another/mount" },
		"filesystem type": func(s *v1alpha1.PillarVolumeStateSpec) { s.FilesystemAdoption.FilesystemType = "xfs" },
		"kind": func(s *v1alpha1.PillarVolumeStateSpec) {
			s.FilesystemAdoption.Kind = v1alpha1.FilesystemAdoptionKindZFSDataset
		},
		"capacity":    func(s *v1alpha1.PillarVolumeStateSpec) { s.CapacityBytes++ },
		"local scope": func(s *v1alpha1.PillarVolumeStateSpec) { s.Resolved.LocalAttach = !s.Resolved.LocalAttach },
		"layout":      func(s *v1alpha1.PillarVolumeStateSpec) { s.Resolved.Backend.Directory.HostRoot = "/other-root" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			env := filesystemLifecycleEnv(t, pvs)
			before := volumeState(t, env, pvs.Name)
			requested := before.DeepCopy().Spec
			mutate(&requested)
			_, err := env.srv.ensureVolumeState(context.Background(), pvs.Name, requested)
			want := codes.FailedPrecondition
			if name == "capacity" {
				want = codes.AlreadyExists
			}
			if status.Code(err) != want {
				t.Fatalf("retry error = %v, want %s", err, want)
			}
			after := volumeState(t, env, pvs.Name)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected source retry mutated owner: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestEnsureVolumeState_FilesystemRaceRechecksWinner(t *testing.T) {
	t.Parallel()
	for _, race := range []string{"create winner", "initial phase CAS"} {
		t.Run(race, func(t *testing.T) {
			t.Parallel()
			env := filesystemLifecycleEnv(t, nil)
			requested := filesystemLifecycleState(t)
			base := env.srv.k8sClient
			hook := &lifecycleConflictClient{Client: base}
			env.srv.k8sClient = hook
			if race == "create winner" {
				hook.beforeCreate = func(ctx context.Context, obj ctrlclient.Object) error {
					return lifecycleCreateWinner(ctx, base, obj, func(winner *v1alpha1.PillarVolumeState) {
						winner.Spec.FilesystemAdoption.ProjectID++
					})
				}
			} else {
				hook.beforeStatus = func(ctx context.Context, obj ctrlclient.Object) error {
					return lifecycleStatusWinner(ctx, base, obj, func(winner *v1alpha1.PillarVolumeState) {
						winner.Spec.FilesystemAdoption.CanonicalSource += "-replacement"
					})
				}
			}
			_, err := env.srv.ensureVolumeState(context.Background(), requested.Name, requested.Spec)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("source race error = %v", err)
			}
			owner := volumeState(t, env, requested.Name)
			if owner.Status.Phase != "" || owner.Status.PublicationGeneration != 0 {
				t.Fatalf("loser advanced winner status: %+v", owner.Status)
			}
		})
	}
}

func TestFilesystemLifecycle_DriverMutationBoundary(t *testing.T) {
	t.Parallel()
	for _, file := range []bool{false, true} {
		t.Run(map[bool]string{false: "files rejects old", true: "old rejects files"}[file], func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			if !file {
				pvs.Spec.FilesystemAdoption = nil
				pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
			}
			env := newControllerTestEnv(t, pvs)
			if !file {
				env.srv.driverName = v1alpha1.FileCSIDriver
			}
			before := volumeState(t, env, pvs.Name)
			ctx := context.Background()
			operations := []func() error{
				func() error { _, err := env.srv.ensureVolumeState(ctx, pvs.Name, pvs.Spec); return err },
				func() error {
					publication := v1alpha1.VolumePublication{NodeID: "worker", AccessMode: "MULTI_NODE_MULTI_WRITER"}
					_, err := env.srv.reservePublication(ctx, pvs.Name, pvs.Spec.VolumeID, before.UID, publication)
					return err
				},
				func() error {
					_, _, err := env.srv.markVolumeDeleting(ctx, pvs.Name, pvs.Spec.VolumeID, nil)
					return err
				},
				func() error {
					return env.srv.recordAllocatedCapacity(ctx, pvs.Name, before.UID, pvs.Spec.CapacityBytes+1)
				},
				func() error { return env.srv.persistCreatePartial(ctx, pvs.Name, before.UID, "/source", nil) },
			}
			for _, operation := range operations {
				requireGRPCCode(t, operation(), codes.NotFound)
			}
			if reaped := reap(t, env, pvs.Name); reaped {
				t.Fatal("wrong driver reaped owner")
			}
			lifecycleRequireUnchanged(t, env, before)
			if env.agent.releaseVolumeCalls+agentTeardownCalls(env) != 0 {
				t.Fatal("wrong driver reached teardown")
			}
		})
	}
}

func TestFilesystemLifecycle_AdoptedCapacityAndReady(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name         string
		file         bool
		wantCapacity int64
	}{
		{name: "legacy zvol", wantCapacity: 8192},
		{name: "file", file: true, wantCapacity: 4096},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			if !scenario.file {
				pvs.Spec.FilesystemAdoption = nil
				pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
				pvs.Spec.ImportedFrom = "tank/legacy"
			}
			env := newControllerTestEnv(t, pvs)
			if scenario.file {
				env.srv.driverName = v1alpha1.FileCSIDriver
			}
			pvs = volumeState(t, env, pvs.Name)
			ctx := context.Background()
			err := env.srv.recordAllocatedCapacity(ctx, pvs.Name, pvs.UID, 8192)
			if scenario.file {
				requireGRPCCode(t, err, codes.FailedPrecondition)
			} else {
				lifecycleRequireSuccess(t, err)
			}
			lifecycleRequireSuccess(t, env.srv.persistCreatePartial(ctx, pvs.Name, pvs.UID, "/adopted", nil))
			lifecycleRequireAcquired(t, volumeState(t, env, pvs.Name))
			info := &agentv1.ExportInfo{Address: "192.168.1.10", Port: 2049}
			lifecycleRequireSuccess(t, env.srv.persistVolumeReady(ctx, pvs.Name, pvs.UID, info))
			ready := volumeState(t, env, pvs.Name)
			lifecycleRequireAcquired(t, ready)
			if ready.Spec.CapacityBytes != scenario.wantCapacity {
				t.Fatalf("recorded capacity=%d want %d", ready.Spec.CapacityBytes, scenario.wantCapacity)
			}
		})
	}
}

func TestPersistVolumeReady_LocalFilesystemRequiresAcquisition(t *testing.T) {
	t.Parallel()
	var typedNil *agentv1.ExportInfo
	scenarios := []struct {
		name     string
		acquired bool
		info     exportInfoGetter
	}{
		{name: "unacquired interface nil"},
		{name: "unacquired typed nil", info: typedNil},
		{name: "acquired interface nil", acquired: true},
		{name: "acquired typed nil", acquired: true, info: typedNil},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			pvs.Spec.Resolved.LocalAttach = true
			pvs.Status.ImportAcquired = scenario.acquired
			env := filesystemLifecycleEnv(t, pvs)
			before := volumeState(t, env, pvs.Name)
			err := env.srv.persistVolumeReady(context.Background(), pvs.Name, before.UID, scenario.info)
			if !scenario.acquired {
				requireGRPCCode(t, err, codes.FailedPrecondition)
				lifecycleRequireUnchanged(t, env, before)
				return
			}
			lifecycleRequireSuccess(t, err)
			lifecycleRequireLocalReady(t, volumeState(t, env, pvs.Name))
			if reap(t, env, pvs.Name) {
				t.Fatal("successful local-only lifecycle reaped without export metadata")
			}
		})
	}
}

func lifecycleRequireLocalReady(t *testing.T, pvs *v1alpha1.PillarVolumeState) {
	t.Helper()
	lifecycleRequireAcquired(t, pvs)
	if pvs.Status.Phase != v1alpha1.PillarVolumeStatePhaseReady ||
		pvs.Status.ExportInfo != nil || pvs.Status.ExportSpec != nil {
		t.Fatalf("local-only Ready = %+v", pvs.Status)
	}
}

func TestReservePublication_FilesystemLocalRemoteCoexist(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	env := filesystemLifecycleEnv(t, pvs)
	pvs = volumeState(t, env, pvs.Name)
	local := v1alpha1.VolumePublication{
		NodeID: "storage-node", InitiatorID: "storage-node", AccessMode: "MULTI_NODE_MULTI_WRITER", Local: true,
	}
	remote := v1alpha1.VolumePublication{
		NodeID: "worker", InitiatorID: "192.168.1.20", AccessMode: "MULTI_NODE_MULTI_WRITER",
	}
	for _, pub := range []v1alpha1.VolumePublication{local, remote, local} {
		if _, err := env.srv.reservePublication(context.Background(), pvs.Name, pvs.Spec.VolumeID, pvs.UID, pub); err != nil {
			t.Fatal(err)
		}
	}
	got := volumeState(t, env, pvs.Name)
	if got.Status.LocalAttachNode != "" ||
		!reflect.DeepEqual(got.Status.PublishedNodes, []v1alpha1.VolumePublication{local, remote}) {
		t.Fatalf("direct/NFS coexistence state: %+v", got.Status)
	}
	before := got.DeepCopy()
	incompatible := remote
	incompatible.NodeID = "other-worker"
	incompatible.AccessMode = "SINGLE_NODE_WRITER"
	_, err := env.srv.reservePublication(
		context.Background(), pvs.Name, pvs.Spec.VolumeID, pvs.UID, incompatible,
	)
	requireGRPCCode(t, err, codes.FailedPrecondition)
	lifecycleRequireUnchanged(t, env, before)
}

func TestReservePublication_BlockLocalFenceUnchanged(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	pvs.Spec.FilesystemAdoption = nil
	pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
	env := newControllerTestEnv(t, pvs)
	pvs = volumeState(t, env, pvs.Name)
	pub := v1alpha1.VolumePublication{
		NodeID: "storage-node", InitiatorID: "storage-node", AccessMode: "SINGLE_NODE_WRITER", Local: true,
	}
	if _, err := env.srv.reservePublication(context.Background(), pvs.Name, pvs.Spec.VolumeID, pvs.UID, pub); err != nil {
		t.Fatal(err)
	}
	if got := volumeState(t, env, pvs.Name).Status.LocalAttachNode; got != pub.NodeID {
		t.Fatalf("block direct-attach fence=%q", got)
	}
}

func TestReapAbandonedVolume_FilesystemReleaseFailureKeepsOwner(t *testing.T) {
	t.Parallel()
	for _, acquired := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused import", true: "export failed after adoption"}[acquired], func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			if acquired {
				pvs.Status.ImportAcquired = true
				pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseCreatePartial
			}
			env := filesystemLifecycleEnv(t, pvs)
			before := volumeState(t, env, pvs.Name)
			reservation := filesystemLifecycleReservation(t, env, before)
			env.agent.releaseVolumeErr = status.Error(codes.Unavailable, "agent disconnected")
			_, err := env.srv.ReapAbandonedVolume(context.Background(), pvs.Name)
			requireGRPCCode(t, err, codes.Unavailable)
			lifecycleRequireHeldOwner(t, env, before, reservation, acquired)
			env.agent.releaseVolumeErr = nil
			if !reap(t, env, pvs.Name) {
				t.Fatal("confirmed retry failed to reap")
			}
			if volumeState(t, env, pvs.Name) != nil || env.agent.deleteVolumeCalls != 0 {
				t.Fatal("release failed to retire preserved file lifecycle")
			}
			res := &v1alpha1.PillarVolumeReservation{}
			err = env.srv.k8sClient.Get(context.Background(), ctrlclient.ObjectKeyFromObject(reservation), res)
			if !k8serrors.IsNotFound(err) {
				t.Fatalf("confirmed release retained reservation: %v", err)
			}
		})
	}
}

func lifecycleRequireHeldOwner(
	t *testing.T, env *controllerTestEnv, before *v1alpha1.PillarVolumeState,
	reservation *v1alpha1.PillarVolumeReservation, acquired bool,
) {
	t.Helper()
	held := volumeState(t, env, before.Name)
	if held == nil || held.UID != before.UID || !held.Status.Deleting {
		t.Fatal("failed release forgot or replaced lifecycle")
	}
	if got := filesystemLifecycleReservation(t, env, held); got.UID != reservation.UID {
		t.Fatal("failed release discarded reservation")
	}
	if env.agent.deleteVolumeCalls != 0 {
		t.Fatal("file reaper destroyed original")
	}
	if env.agent.unexportVolumeCalls != map[bool]int{false: 0, true: 1}[acquired] {
		t.Fatal("refused/adopted teardown ordering differs")
	}
	request := env.agent.lastReleaseVolumeReq
	if request.GetFence().GetVolumeUid() != string(before.UID) ||
		request.GetFilesystemAdoption().GetResourceId() != before.Spec.FilesystemAdoption.ResourceID {
		t.Fatalf("release did not retire original owner: %+v", request)
	}
}

func TestReapAbandonedVolume_PVDriverAndHandleScope(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name       string
		file       bool
		byName     bool
		sameDriver bool
	}{
		{name: "old handle foreign"},
		{name: "old handle matching", sameDriver: true},
		{name: "old name foreign", byName: true},
		{name: "old name matching", byName: true, sameDriver: true},
		{name: "file handle foreign", file: true},
		{name: "file handle matching", file: true, sameDriver: true},
		{name: "file name foreign", file: true, byName: true},
		{name: "file name matching", file: true, byName: true, sameDriver: true},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			var env *controllerTestEnv
			if scenario.file {
				env = filesystemLifecycleEnv(t, pvs)
			} else {
				pvs.Spec.FilesystemAdoption = nil
				pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
				env = newControllerTestEnv(t, pvs)
			}
			pvName := "static-retain-pv"
			if scenario.byName {
				pvName = pvs.Name
			}
			driver := env.srv.effectiveDriverName()
			if !scenario.sameDriver {
				driver = map[bool]string{false: v1alpha1.FileCSIDriver, true: v1alpha1.DefaultCSIDriver}[scenario.file]
			}
			pv := &corev1.PersistentVolume{
				Name: pvName,
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: pvs.Spec.VolumeID},
					},
				},
			}
			lifecycleRequireSuccess(t, env.srv.k8sClient.Create(context.Background(), pv))
			if got := reap(t, env, pvs.Name); got == scenario.sameDriver {
				t.Fatalf("reaped=%t matching driver=%t", got, scenario.sameDriver)
			}
		})
	}
}

func TestFilesystemRetain_ManualSamePVClaimRebindKeepsLifecycle(t *testing.T) {
	t.Parallel()
	env, req := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	ctx := context.Background()
	agent := &v1alpha1.PillarAgent{}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: "storage-node-1"}, agent))
	agent.Spec.External = nil
	agent.Spec.NodeRef = &v1alpha1.NodeRefSpec{Name: "storage-node"}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Update(ctx, agent))
	req.VolumeCapabilities = []*csipb.VolumeCapability{
		nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	_, createErr := env.srv.CreateVolume(ctx, req)
	lifecycleRequireSuccess(t, createErr)
	original := volumeState(t, env, req.Name)
	lifecycleRequireLocalReady(t, original)
	reservation := filesystemLifecycleReservation(t, env, original)
	imports, creates := env.agent.importVolumeCalls, env.agent.createVolumeCalls
	oldClaim := &corev1.PersistentVolumeClaim{}
	claimKey := types.NamespacedName{Namespace: "default", Name: "filesystem-import"}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Get(ctx, claimKey, oldClaim))
	pv := &corev1.PersistentVolume{Name: req.Name, Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		ClaimRef: &corev1.ObjectReference{
			Namespace: oldClaim.Namespace, Name: oldClaim.Name, UID: oldClaim.UID,
		},
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
			Driver: v1alpha1.FileCSIDriver, VolumeHandle: original.Spec.VolumeID,
		}},
	}}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Create(ctx, pv))
	deleteClaim(t, env, oldClaim)
	if reap(t, env, req.Name) {
		t.Fatal("retained successful file lifecycle was reaped")
	}
	newClaim := &corev1.PersistentVolumeClaim{Name: "replacement", Namespace: "default", UID: "replacement-uid",
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name}}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Create(ctx, newClaim))
	pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: newClaim.Namespace, Name: newClaim.Name, UID: newClaim.UID}
	lifecycleRequireSuccess(t, env.srv.k8sClient.Update(ctx, pv))
	if reap(t, env, req.Name) {
		t.Fatal("manual claimRef replacement reaped retained owner")
	}
	// Publishing the same handle is the rebind path, not dynamic CreateVolume.
	_, publishErr := env.srv.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: original.Spec.VolumeID, NodeId: "storage-node",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	lifecycleRequireSuccess(t, publishErr)
	lifecycleRequireRetainedRebind(t, env, original, reservation, imports, creates)
}

func lifecycleRequireRetainedRebind(
	t *testing.T, env *controllerTestEnv, original *v1alpha1.PillarVolumeState,
	reservation *v1alpha1.PillarVolumeReservation, imports, creates int,
) {
	t.Helper()
	after := volumeState(t, env, original.Name)
	if after.UID != original.UID || !reflect.DeepEqual(after.Spec, original.Spec) {
		t.Fatal("rebind minted or rewrote storage owner")
	}
	if got := filesystemLifecycleReservation(t, env, after); !reflect.DeepEqual(got, reservation) {
		t.Fatal("rebind replaced native resource reservation")
	}
	if env.agent.importVolumeCalls != imports || env.agent.createVolumeCalls != creates ||
		env.agent.deleteVolumeCalls != 0 || env.agent.releaseVolumeCalls != 0 || env.agent.exportVolumeCalls != 0 {
		t.Fatal("Retain rebind re-adopted, exported, or retired original source")
	}
	if after.Status.LocalAttachNode != "" ||
		len(after.Status.PublishedNodes) != 1 || !after.Status.PublishedNodes[0].Local {
		t.Fatalf("Retain direct mount status=%+v", after.Status)
	}
}

func TestFilesystemLifecycle_DriverCheckedAgainAfterStatusConflict(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	env := filesystemLifecycleEnv(t, pvs)
	before := volumeState(t, env, pvs.Name)
	base := env.srv.k8sClient
	hook := &lifecycleConflictClient{Client: base}
	hook.beforeStatus = func(ctx context.Context, obj ctrlclient.Object) error {
		return lifecycleStatusWinner(ctx, base, obj, func(winner *v1alpha1.PillarVolumeState) {
			winner.Spec.FilesystemAdoption = nil
		})
	}
	env.srv.k8sClient = hook
	_, err := env.srv.reservePublication(context.Background(), pvs.Name, pvs.Spec.VolumeID, before.UID,
		v1alpha1.VolumePublication{NodeID: "worker", AccessMode: "MULTI_NODE_MULTI_WRITER"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("wrong-driver CAS retry acquired a grant: %v", err)
	}
	after := volumeState(t, env, pvs.Name)
	if after.Status.PublicationGeneration != before.Status.PublicationGeneration ||
		!reflect.DeepEqual(after.Status.PublishedNodes, before.Status.PublishedNodes) {
		t.Fatal("wrong-driver retry committed publication status")
	}
}

func TestFilesystemLifecycle_ExpansionNeverChangesQuota(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	pvs.Status.ImportAcquired = true
	pvs.Status.Phase = v1alpha1.PillarVolumeStatePhaseReady
	env := filesystemLifecycleEnv(t, pvs)
	before := volumeState(t, env, pvs.Name)
	_, err := env.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      pvs.Spec.VolumeID,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: pvs.Spec.CapacityBytes + 4096},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("filesystem expansion = %v, want refusal", err)
	}
	if env.agent.expandVolumeCalls != 0 || !reflect.DeepEqual(before, volumeState(t, env, pvs.Name)) {
		t.Fatal("filesystem expansion changed native quota or owner status")
	}
}

func TestPersistVolumeReady_NilExportInvalidForNetworkAndLegacy(t *testing.T) {
	t.Parallel()
	for _, file := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "network file"}[file], func(t *testing.T) {
			t.Parallel()
			pvs := filesystemLifecycleState(t)
			pvs.Status.ImportAcquired = true
			if !file {
				pvs.Spec.FilesystemAdoption = nil
				pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
			}
			env := newControllerTestEnv(t, pvs)
			if file {
				env.srv.driverName = v1alpha1.FileCSIDriver
			}
			before := volumeState(t, env, pvs.Name)
			var info *agentv1.ExportInfo
			err := env.srv.persistVolumeReady(context.Background(), pvs.Name, before.UID, info)
			requireGRPCCode(t, err, codes.FailedPrecondition)
			lifecycleRequireUnchanged(t, env, before)
		})
	}
}

func TestEnsureVolumeState_ConcurrentFilesystemNonceReturnsWinner(t *testing.T) {
	t.Parallel()
	env := filesystemLifecycleEnv(t, nil)
	requested := filesystemLifecycleState(t)
	requested.Spec.VolumeID = "storage-node-1/nfs/directory/" + requested.Spec.AgentVolumeID
	base := env.srv.k8sClient
	hook := &lifecycleConflictClient{Client: base}
	var candidateHandle, winnerHandle string
	hook.beforeCreate = func(ctx context.Context, obj ctrlclient.Object) error {
		return lifecycleCreateWinner(ctx, base, obj, func(winner *v1alpha1.PillarVolumeState) {
			candidateHandle = winner.Spec.VolumeID
			winnerHandle = requested.Spec.VolumeID + ".fedcba9876543210fedcba9876543210"
			winner.Spec.VolumeID = winnerHandle
		})
	}
	env.srv.k8sClient = hook
	got, err := env.srv.ensureVolumeState(context.Background(), requested.Name, requested.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.VolumeID != winnerHandle || got.Spec.VolumeID == candidateHandle || !isFilesystemVolumeID(got) {
		t.Fatalf("concurrent same-source create did not return winner lifecycle handle: got=%q winner=%q candidate=%q",
			got.Spec.VolumeID, winnerHandle, candidateHandle)
	}
	if got.Status.Phase != v1alpha1.PillarVolumeStatePhaseProvisioning {
		t.Fatalf("winner not initialized: %+v", got.Status)
	}
	hook.beforeCreate = nil
	retry, err := env.srv.ensureVolumeState(context.Background(), requested.Name, got.Spec)
	if err != nil || retry.UID != got.UID || retry.Spec.VolumeID != winnerHandle {
		t.Fatalf("recorded retry changed nonce or lifecycle: retry=%+v err=%v", retry, err)
	}
}

func TestFilesystemLifecycle_DeleteRecreateSameNameChangesHandle(t *testing.T) {
	t.Parallel()
	env, req := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	ctx := context.Background()
	first, err := env.srv.CreateVolume(ctx, req)
	lifecycleRequireSuccess(t, err)
	original := volumeState(t, env, req.Name)
	if !isFilesystemVolumeID(original) || first.GetVolume().GetVolumeId() != original.Spec.VolumeID {
		t.Fatal("first adoption did not persist a lifecycle-specific CSI handle")
	}
	_, err = env.srv.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: original.Spec.VolumeID})
	lifecycleRequireSuccess(t, err)
	if volumeState(t, env, req.Name) != nil {
		t.Fatal("confirmed file release kept old lifecycle")
	}
	second, err := env.srv.CreateVolume(ctx, req)
	lifecycleRequireSuccess(t, err)
	replacement := volumeState(t, env, req.Name)
	lifecycleRequireReadopted(t, env, original, replacement, second)
	before := replacement.DeepCopy()
	_, err = env.srv.ControllerPublishVolume(ctx, &csipb.ControllerPublishVolumeRequest{
		VolumeId: original.Spec.VolumeID, NodeId: "old-worker",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	})
	requireGRPCCode(t, err, codes.NotFound)
	lifecycleRequireUnchanged(t, env, before)
}

func lifecycleRequireReadopted(
	t *testing.T, env *controllerTestEnv, original, replacement *v1alpha1.PillarVolumeState,
	response *csipb.CreateVolumeResponse,
) {
	t.Helper()
	if replacement.UID == original.UID || replacement.Spec.VolumeID == original.Spec.VolumeID ||
		response.GetVolume().GetVolumeId() != replacement.Spec.VolumeID || !isFilesystemVolumeID(replacement) {
		t.Fatal("same-name recreation reused an old CSI lifecycle identity")
	}
	if replacement.Spec.AgentVolumeID != original.Spec.AgentVolumeID ||
		*replacement.Spec.FilesystemAdoption != *original.Spec.FilesystemAdoption || env.agent.deleteVolumeCalls != 0 {
		t.Fatal("lifecycle renewal changed or destroyed original backing source")
	}
}

func TestFilesystemLifecycle_UnsuffixedStoredHandleFailsClosed(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	pvs.Spec.VolumeID = "storage-node-1/nfs/directory/" + pvs.Spec.AgentVolumeID
	env := filesystemLifecycleEnv(t, pvs)
	before := volumeState(t, env, pvs.Name)
	_, ensureErr := env.srv.ensureVolumeState(context.Background(), pvs.Name, pvs.Spec)
	requireGRPCCode(t, ensureErr, codes.FailedPrecondition)
	_, reapErr := env.srv.ReapAbandonedVolume(context.Background(), pvs.Name)
	requireGRPCCode(t, reapErr, codes.FailedPrecondition)
	if env.agent.releaseVolumeCalls+agentTeardownCalls(env) != 0 ||
		!reflect.DeepEqual(before, volumeState(t, env, pvs.Name)) {
		t.Fatal("invalid handle changed or retired lifecycle state")
	}
}

type lifecycleLookupReader struct {
	ctrlclient.Reader
	afterList func()
}

func (r *lifecycleLookupReader) List(
	ctx context.Context, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption,
) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if _, volumeList := list.(*v1alpha1.PillarVolumeStateList); volumeList && r.afterList != nil {
		after := r.afterList
		r.afterList = nil
		after()
	}
	return nil
}

func TestFilesystemLifecycle_StaleDeleteLookupCannotMarkReplacement(t *testing.T) {
	t.Parallel()
	old := filesystemLifecycleState(t)
	old.Status.Phase = v1alpha1.PillarVolumeStatePhaseReady
	old.Status.ImportAcquired = true
	env := filesystemLifecycleEnv(t, old)
	original := volumeState(t, env, old.Name)
	reservation := filesystemLifecycleReservation(t, env, original)
	base := env.srv.k8sClient
	var replacement *v1alpha1.PillarVolumeState
	reader := &lifecycleLookupReader{Reader: base}
	reader.afterList = func() {
		ctx := context.Background()
		if err := base.Delete(ctx, original); err != nil {
			t.Fatal(err)
		}
		next := original.DeepCopy()
		next.UID = ""
		next.ResourceVersion = ""
		next.Spec.VolumeID = filesystemVolumeIDBase(&next.Spec) + ".fedcba9876543210fedcba9876543210"
		if err := base.Create(ctx, next); err != nil {
			t.Fatal(err)
		}
		replacement = volumeState(t, env, old.Name)
	}
	env.srv.apiReader = reader
	_, err := env.srv.DeleteVolume(
		context.Background(), &csipb.DeleteVolumeRequest{VolumeId: original.Spec.VolumeID},
	)
	lifecycleRequireSuccess(t, err)
	if replacement == nil || replacement.UID == original.UID {
		t.Fatal("lookup-to-mark replacement race did not occur")
	}
	if after := volumeState(t, env, old.Name); !reflect.DeepEqual(after, replacement) {
		t.Fatalf("stale Delete marked or removed new owner: before=%+v after=%+v", replacement, after)
	}
	if env.agent.releaseVolumeCalls+agentTeardownCalls(env) != 0 {
		t.Fatal("stale Delete retired replacement backing ownership")
	}
	if after := filesystemLifecycleReservation(t, env, replacement); !reflect.DeepEqual(after, reservation) {
		t.Fatal("stale Delete removed replacement resource reservation")
	}
}

func TestMarkVolumeDeleting_FilesystemHandleRecheckedAfterConflict(t *testing.T) {
	t.Parallel()
	pvs := filesystemLifecycleState(t)
	env := filesystemLifecycleEnv(t, pvs)
	original := volumeState(t, env, pvs.Name)
	base := env.srv.k8sClient
	hook := &lifecycleConflictClient{Client: base}
	hook.beforeStatus = func(ctx context.Context, obj ctrlclient.Object) error {
		return lifecycleStatusWinner(ctx, base, obj, func(changed *v1alpha1.PillarVolumeState) {
			changed.Spec.VolumeID = filesystemVolumeIDBase(&changed.Spec) + ".fedcba9876543210fedcba9876543210"
		})
	}
	env.srv.k8sClient = hook
	marked, token, err := env.srv.markVolumeDeleting(context.Background(), pvs.Name, original.Spec.VolumeID, nil)
	if status.Code(err) != codes.Aborted || marked != nil || token != nil {
		t.Fatalf("stale deletion CAS returned an owner/fence: marked=%+v token=%+v err=%v", marked, token, err)
	}
	after := volumeState(t, env, pvs.Name)
	if after.Status.Deleting || after.Status.PublicationGeneration != original.Status.PublicationGeneration {
		t.Fatal("stale handle changed deleting/generation after conflict")
	}
}
