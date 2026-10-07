/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package csi

import (
	"context"
	"sync"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/isac322/pillar-csi/api/v1alpha1"
)

func filesystemLocalCreateRequest(mode csipb.VolumeCapability_AccessMode_Mode) *csipb.CreateVolumeRequest {
	req := baseCreateVolumeRequest()
	req.Name = "pvc-filesystem-local"
	req.Parameters[paramStoreRef] = "nfs-datasets"
	req.Parameters[paramProtocolRef] = "nfs"
	req.Parameters[paramPVCNameMeta] = "filesystem-import"
	req.Parameters[paramPVCNamespaceMeta] = "default"
	req.VolumeCapabilities = []*csipb.VolumeCapability{nfsVolumeCapability(mode)}
	return req
}

func setFilesystemNodeRef(t *testing.T, env *controllerTestEnv) {
	t.Helper()
	ctx := context.Background()
	agent := &v1alpha1.PillarAgent{}
	if err := env.srv.k8sClient.Get(ctx, types.NamespacedName{Name: "storage-node-1"}, agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.External = nil
	agent.Spec.NodeRef = &v1alpha1.NodeRefSpec{Name: "storage-node"}
	if err := env.srv.k8sClient.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
}

func filesystemPVS(t *testing.T, env *controllerTestEnv, name string) *v1alpha1.PillarVolumeState {
	t.Helper()
	pvs := &v1alpha1.PillarVolumeState{}
	if err := env.srv.k8sClient.Get(context.Background(), types.NamespacedName{Name: name}, pvs); err != nil {
		t.Fatal(err)
	}
	return pvs
}

type filesystemSelectorClient struct {
	ctrlclient.Client
	source string
}

func (c *filesystemSelectorClient) Get(
	ctx context.Context, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption,
) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if typed, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		typed.Annotations = map[string]string{filesystemImportDirectoryKey: c.source}
	}
	return nil
}

type filesystemReservationBarrierClient struct {
	ctrlclient.Client
	mu      sync.Mutex
	creates int
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *filesystemReservationBarrierClient) Create(
	ctx context.Context, obj ctrlclient.Object, opts ...ctrlclient.CreateOption,
) error {
	err := c.Client.Create(ctx, obj, opts...)
	if _, ok := obj.(*v1alpha1.PillarVolumeReservation); !ok {
		return err
	}
	if err != nil {
		c.once.Do(func() { close(c.release) })
		return err
	}
	c.mu.Lock()
	c.creates++
	creates := c.creates
	c.mu.Unlock()
	if creates == 2 {
		close(c.reached)
	}
	select {
	case <-c.release:
	case <-time.After(2 * time.Second):
		c.once.Do(func() { close(c.release) })
	}
	return nil
}

func TestCreateVolume_ConcurrentSamePVCDifferentSelectorsKeepsOneNativeWinner(t *testing.T) {
	envDirectory, reqDirectory := newFilesystemImportCreateEnv(t,
		filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	envOther, reqOther := newFilesystemImportCreateEnv(t,
		filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/other-data")
	reqDirectory.Name = "pvc-filesystem-race"
	reqOther.Name = reqDirectory.Name
	envOther.agent.inspectImportResp.FilesystemAdoption.CanonicalSource = "/var/lib/pillar-csi/datasets/other-data"
	envOther.agent.inspectImportResp.FilesystemAdoption.ResourceId = "01234567-89ab-cdef-0123-456789abcdef:43"
	envOther.agent.inspectImportResp.FilesystemAdoption.Inode = 43
	envOther.agent.importVolumeResp.DevicePath = "/var/lib/pillar-csi/datasets/other-data"
	base := envDirectory.srv.k8sClient
	barrier := &filesystemReservationBarrierClient{
		Client: base, reached: make(chan struct{}), release: make(chan struct{}),
	}
	envDirectory.srv.k8sClient = &filesystemSelectorClient{
		Client: barrier, source: "/var/lib/pillar-csi/datasets/app-data",
	}
	envDirectory.srv.apiReader = envDirectory.srv.k8sClient
	envOther.srv.k8sClient = &filesystemSelectorClient{
		Client: barrier, source: "/var/lib/pillar-csi/datasets/other-data",
	}
	envOther.srv.apiReader = envOther.srv.k8sClient

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() {
		_, err := envDirectory.srv.CreateVolume(context.Background(), reqDirectory)
		errs <- err
	})
	wg.Go(func() {
		_, err := envOther.srv.CreateVolume(context.Background(), reqOther)
		errs <- err
	})
	release := func() {
		barrier.once.Do(func() { close(barrier.release) })
		wg.Wait()
	}
	defer release()
	requireConcurrentFilesystemCandidates(t, base, barrier)
	release()
	close(errs)
	var successes, rejected int
	for err := range errs {
		switch status.Code(err) {
		case codes.OK:
			successes++
		case codes.FailedPrecondition:
			rejected++
		default:
			t.Fatalf("concurrent CreateVolume error = %v", err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent CreateVolume results = successes %d, immutable-identity rejections %d; want one of each",
			successes, rejected)
	}

	winner := filesystemPVS(t, envDirectory, reqDirectory.Name)
	if winner.Spec.FilesystemAdoption == nil || !isFilesystemVolumeID(winner) {
		t.Fatalf("concurrent winner lacks immutable filesystem lifecycle: %+v", winner)
	}
	if winner.Spec.FilesystemAdoption.Kind != v1alpha1.FilesystemAdoptionKindDirectory {
		t.Fatalf("winner kind = %q, want directory", winner.Spec.FilesystemAdoption.Kind)
	}
	if envDirectory.agent.importVolumeCalls+envOther.agent.importVolumeCalls != 1 {
		t.Fatalf("concurrent native imports = directory %d + other %d, want one winner import",
			envDirectory.agent.importVolumeCalls, envOther.agent.importVolumeCalls)
	}

	requireFilesystemWinnerCleanup(t, envDirectory, base, winner)
}

func requireFilesystemWinnerCleanup(
	t *testing.T, env *controllerTestEnv, base ctrlclient.Client, winner *v1alpha1.PillarVolumeState,
) {
	t.Helper()
	if _, err := env.srv.DeleteVolume(context.Background(),
		&csipb.DeleteVolumeRequest{VolumeId: winner.Spec.VolumeID}); err != nil {
		t.Fatalf("winner DeleteVolume: %v", err)
	}
	var reservations v1alpha1.PillarVolumeReservationList
	if err := base.List(context.Background(), &reservations); err != nil {
		t.Fatal(err)
	}
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.AgentRef == winner.Spec.AgentRef &&
			reservation.Spec.BackendType == winner.Spec.BackendType &&
			reservation.Spec.OwnerVolume == winner.Name {
			t.Fatalf("winner candidate reservation survived lifecycle delete: %+v", reservation)
		}
	}
}

func requireConcurrentFilesystemCandidates(
	t *testing.T, base ctrlclient.Client, barrier *filesystemReservationBarrierClient,
) {
	t.Helper()
	select {
	case <-barrier.reached:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent CreateVolume did not acquire both candidate reservations")
	}
	var reservations v1alpha1.PillarVolumeReservationList
	if err := base.List(context.Background(), &reservations); err != nil {
		t.Fatal(err)
	}
	if len(reservations.Items) != 2 {
		t.Fatalf("candidate reservations before winner = %d, want two", len(reservations.Items))
	}
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.AgentRef == "" || reservation.Spec.BackendType != string(v1alpha1.BackendIDDirectory) ||
			reservation.Spec.OwnerVolume == "" || reservation.Spec.FilesystemResourceID == "" {
			t.Fatalf("candidate reservation lacks owned same-scope native lease: %+v", reservation)
		}
	}
}

func TestCreateVolume_FilesystemLocalOnlyDoesNotNeedNFSExport(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	setFilesystemNodeRef(t, env)
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	env.agent.exportVolumeErr = status.Error(codes.Unavailable, "NFS server unavailable")

	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("local filesystem CreateVolume: %v", err)
	}
	if resp.GetVolume().GetAccessibleTopology() == nil || len(resp.GetVolume().GetAccessibleTopology()) != 1 {
		t.Fatalf("AccessibleTopology = %+v, want the storage node topology", resp.GetVolume().GetAccessibleTopology())
	}
	if got := resp.GetVolume().GetAccessibleTopology()[0].GetSegments()[FileTopologyNodeKey]; got != "storage-node" {
		t.Fatalf("accessible topology node = %q, want storage-node", got)
	}
	pvs := filesystemPVS(t, env, req.GetName())
	if !localOnlyFilesystem(pvs) || !pvs.Status.ImportAcquired || pvs.Status.ExportSpec != nil {
		t.Fatalf("local-only state = %+v, want acquired with no network export", pvs.Status)
	}
	if env.agent.exportVolumeCalls != 0 {
		t.Fatalf("ExportVolume calls = %d, want zero for local-only adoption", env.agent.exportVolumeCalls)
	}
	requireLocalFilesystemPublish(t, env, resp.GetVolume().GetVolumeId())
}

func requireLocalFilesystemPublish(t *testing.T, env *controllerTestEnv, volumeID string) {
	t.Helper()
	_, err := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "worker-node-1",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("remote single-node publish of local-only filesystem code = %v, want FailedPrecondition (err=%v)",
			status.Code(err), err)
	}
	if env.agent.allowInitiatorCalls != 0 {
		t.Fatalf("remote single-node refusal granted an initiator: %d calls", env.agent.allowInitiatorCalls)
	}

	pub, err := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "storage-node",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	})
	if err != nil {
		t.Fatalf("local filesystem ControllerPublishVolume: %v", err)
	}
	if pub.GetPublishContext()[PublishContextKeyAttachMode] != AttachModeLocal {
		t.Fatalf("PublishContext = %+v, want local attach", pub.GetPublishContext())
	}
	const expectedProxy = "/var/lib/pillar-csi/exports/test-owned-proxy"
	if got := pub.GetPublishContext()[PublishContextKeyFilesystemProxyPath]; got != expectedProxy {
		t.Fatalf("filesystem proxy path = %q, want controller-owned proxy", got)
	}
	if got := pub.GetPublishContext()[PublishContextKeyFilesystemCanonicalSource]; got == "" || got == expectedProxy {
		t.Fatalf("filesystem canonical source context = %q, want original source distinct from proxy", got)
	}
}

func TestCreateVolume_FilesystemCompletedRetryUsesStoredSourceWhenClaimGone(t *testing.T) {
	t.Parallel()
	env, req := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	before := filesystemPVS(t, env, req.GetName())
	uid, handle, source := before.UID, before.Spec.VolumeID, before.Spec.FilesystemAdoption.CanonicalSource
	claim := &corev1.PersistentVolumeClaim{}
	if getErr := env.srv.k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "filesystem-import"}, claim); getErr != nil {
		t.Fatal(getErr)
	}
	if deleteErr := env.srv.k8sClient.Delete(context.Background(), claim); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	retry, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("completed CreateVolume retry with retired claim: %v", err)
	}
	after := filesystemPVS(t, env, req.GetName())
	if retry.GetVolume().GetVolumeId() != handle || after.UID != uid ||
		after.Spec.VolumeID != handle || after.Spec.FilesystemAdoption.CanonicalSource != source ||
		after.Status.PublicationGeneration != before.Status.PublicationGeneration {
		t.Fatalf("completed retry changed stored lifecycle/source: before=%+v after=%+v", before, after)
	}
	if resp.GetVolume().GetVolumeId() != handle {
		t.Fatalf("initial response handle = %q, want %q", resp.GetVolume().GetVolumeId(), handle)
	}
}

func TestControllerPublishUnpublishFilesystemACLOffAgentFailuresRetainAndRetry(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	pvs := filesystemPVS(t, env, req.GetName())
	pvs.Status.ExportSpec.ACLEnabled = false
	if err := env.srv.k8sClient.Status().Update(context.Background(), pvs); err != nil {
		t.Fatal(err)
	}
	env.agent.allowInitiatorErr = status.Error(codes.FailedPrecondition, "source identity drift")
	publish := nfsPublishRequest(resp.GetVolume().GetVolumeId(),
		csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false)
	publish.NodeId = "worker-node-1"
	if _, err := env.srv.ControllerPublishVolume(context.Background(), publish); err == nil {
		t.Fatal("ACL-off filesystem publish succeeded despite source-validation failure")
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 0 {
		t.Fatalf("failed ACL-off publish recorded %d publications, want none", got)
	}
	env.agent.allowInitiatorErr = nil
	if _, err := env.srv.ControllerPublishVolume(context.Background(), publish); err != nil {
		t.Fatalf("ACL-off filesystem publish retry: %v", err)
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 1 {
		t.Fatalf("successful ACL-off publish recorded %d publications, want one", got)
	}

	env.agent.denyInitiatorErr = status.Error(codes.FailedPrecondition, "source identity drift")
	unpublish := &csipb.ControllerUnpublishVolumeRequest{VolumeId: publish.GetVolumeId(), NodeId: publish.GetNodeId()}
	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), unpublish); err == nil {
		t.Fatal("ACL-off filesystem unpublish succeeded despite source-validation failure")
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 1 {
		t.Fatalf("failed ACL-off unpublish removed publication, got %d", got)
	}
	env.agent.denyInitiatorErr = nil
	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), unpublish); err != nil {
		t.Fatalf("ACL-off filesystem unpublish retry: %v", err)
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 0 {
		t.Fatalf("successful ACL-off unpublish retained %d publications", got)
	}
}

func TestDeleteVolume_FilesystemPreservesSourceAcrossCleanupFailuresAndRetry(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	volumeID := resp.GetVolume().GetVolumeId()
	env.agent.unexportVolumeErr = status.Error(codes.Unavailable, "unexport unavailable")
	if _, err := env.srv.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: volumeID}); err == nil {
		t.Fatal("DeleteVolume succeeded while filesystem unexport failed")
	}
	pvs := filesystemPVS(t, env, req.GetName())
	if !pvs.Status.Deleting || pvs.Spec.FilesystemAdoption == nil {
		t.Fatalf("failed delete lost durable filesystem lifecycle: %+v", pvs)
	}
	source := pvs.Spec.FilesystemAdoption.CanonicalSource
	env.agent.unexportVolumeErr = nil
	env.agent.releaseVolumeErr = status.Error(codes.Unavailable, "release unavailable")
	if _, err := env.srv.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: volumeID}); err == nil {
		t.Fatal("DeleteVolume succeeded while filesystem release failed")
	}
	pvs = filesystemPVS(t, env, req.GetName())
	if pvs.Spec.FilesystemAdoption.CanonicalSource != source || !pvs.Status.Deleting {
		t.Fatalf("release failure changed source/lifecycle: %+v", pvs)
	}
	env.agent.releaseVolumeErr = nil
	if _, err := env.srv.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: volumeID}); err != nil {
		t.Fatalf("DeleteVolume retry: %v", err)
	}
	if env.agent.deleteVolumeCalls != 0 {
		t.Fatalf("filesystem DeleteVolume calls = %d, want zero (source must be preserved)", env.agent.deleteVolumeCalls)
	}
}

func TestControllerUnpublishVolume_FilesystemLocalDetachFailureRetainsPublication(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	setFilesystemNodeRef(t, env)
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	publish := &csipb.ControllerPublishVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(), NodeId: "storage-node",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}
	if _, err := env.srv.ControllerPublishVolume(context.Background(), publish); err != nil {
		t.Fatal(err)
	}
	env.agent.setLocalAttachErr = status.Error(codes.Unavailable, "proxy detach unavailable")
	unpublish := &csipb.ControllerUnpublishVolumeRequest{VolumeId: publish.GetVolumeId(), NodeId: publish.GetNodeId()}
	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), unpublish); err == nil {
		t.Fatal("local filesystem unpublish succeeded despite proxy detach failure")
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 1 {
		t.Fatalf("detach failure removed %d publications, want one retained", got)
	}
	env.agent.setLocalAttachErr = nil
	if _, err := env.srv.ControllerUnpublishVolume(context.Background(), unpublish); err != nil {
		t.Fatalf("local filesystem unpublish retry: %v", err)
	}
	if got := len(filesystemPVS(t, env, req.GetName()).Status.PublishedNodes); got != 0 {
		t.Fatalf("successful detach retained %d publications", got)
	}
}

func TestCreateVolume_UnmountedZFSDatasetKeepsNativeIdentityAndLocalProxy(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportZFSDatasetKey, "tank/app-data")
	setFilesystemNodeRef(t, env)
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	env.agent.inspectImportResp.FilesystemAdoption.HostPath = ""
	env.agent.importVolumeResp.DevicePath = ""
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("unmounted dataset CreateVolume: %v", err)
	}
	pvs := filesystemPVS(t, env, req.GetName())
	if pvs.Spec.FilesystemAdoption == nil || pvs.Spec.FilesystemAdoption.ResourceID != "12345" ||
		pvs.Status.BackendDevicePath != "" || !pvs.Status.ImportAcquired {
		t.Fatalf("unmounted dataset identity/acquisition = %+v / %+v", pvs.Spec.FilesystemAdoption, pvs.Status)
	}
	if _, err := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(), NodeId: "storage-node",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER),
	}); err != nil {
		t.Fatalf("unmounted dataset local proxy publish: %v", err)
	}
}

func TestCreateVolume_FilesystemLocalTopologyMatchesRealAgentNodeRef(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	setFilesystemNodeRef(t, env)
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	req.AccessibilityRequirements = &csipb.TopologyRequirement{
		Requisite: []*csipb.Topology{{Segments: map[string]string{
			FileTopologyNodeKey:              "storage-node",
			"nvmeof.csi.storage.k8s.io/node": "nvmeof-node",
		}}},
	}
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("matching topology CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetAccessibleTopology()[0].GetSegments()[FileTopologyNodeKey]; got != "storage-node" {
		t.Fatalf("accessible topology = %q, want storage-node", got)
	}

	env2 := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	setFilesystemNodeRef(t, env2)
	req2 := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER)
	req2.AccessibilityRequirements = &csipb.TopologyRequirement{
		Requisite: []*csipb.Topology{{Segments: map[string]string{FileTopologyNodeKey: "other-node"}}},
	}
	_, err = env2.srv.CreateVolume(context.Background(), req2)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("outside topology CreateVolume code = %v, want ResourceExhausted (err=%v)", status.Code(err), err)
	}
	if env2.agent.importVolumeCalls != 0 || env2.agent.createVolumeCalls != 0 {
		t.Fatalf("outside topology reached backend: import=%d create=%d",
			env2.agent.importVolumeCalls, env2.agent.createVolumeCalls)
	}
}

func TestCreateVolume_FilesystemRemoteMultiNodePublishesConcurrently(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("remote filesystem CreateVolume: %v", err)
	}
	pvs := filesystemPVS(t, env, req.GetName())
	if localOnlyFilesystem(pvs) {
		t.Fatal("multi-node filesystem adoption unexpectedly became local-only")
	}
	for _, node := range []string{"worker-node-1", "worker-node-2"} {
		if _, err := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
			VolumeId: resp.GetVolume().GetVolumeId(), NodeId: node,
			VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
		}); err != nil {
			t.Fatalf("remote publish to %s: %v", node, err)
		}
	}
	pvs = filesystemPVS(t, env, req.GetName())
	if len(pvs.Status.PublishedNodes) != 2 {
		t.Fatalf("published nodes = %+v, want two concurrent remote publications", pvs.Status.PublishedNodes)
	}
}

func TestCreateVolume_FilesystemWrongDriverAndExpandRefuseBeforeAgent(t *testing.T) {
	t.Parallel()
	env, req := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	env.srv.driverName = v1alpha1.DefaultCSIDriver
	_, err := env.srv.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.NotFound {
		t.Fatalf("wrong-driver CreateVolume code = %v, want refusal (err=%v)", status.Code(err), err)
	}
	if env.agent.inspectImportCalls != 0 || env.agent.importVolumeCalls != 0 {
		t.Fatalf("wrong-driver request reached filesystem backend: inspect=%d import=%d",
			env.agent.inspectImportCalls, env.agent.importVolumeCalls)
	}

	env2, req2 := newFilesystemImportCreateEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	resp, err := env2.srv.CreateVolume(context.Background(), req2)
	if err != nil {
		t.Fatal(err)
	}
	before := filesystemPVS(t, env2, req2.GetName())
	_, err = env2.srv.ControllerExpandVolume(context.Background(), &csipb.ControllerExpandVolumeRequest{
		VolumeId:      resp.GetVolume().GetVolumeId(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: before.Spec.CapacityBytes + 1},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("filesystem expand code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
	if env2.agent.expandVolumeCalls != 0 {
		t.Fatalf("ExpandVolume calls = %d, want zero", env2.agent.expandVolumeCalls)
	}
}

func TestCreateVolume_FilesystemRetryRetainsPublishedLifecycle(t *testing.T) {
	t.Parallel()
	env := newFilesystemImportEnv(t, filesystemImportDirectoryKey, "/var/lib/pillar-csi/datasets/app-data")
	req := filesystemLocalCreateRequest(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER)
	resp, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, publishErr := env.srv.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: resp.GetVolume().GetVolumeId(), NodeId: "worker-node-1",
		VolumeCapability: nfsVolumeCapability(csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER),
	}); publishErr != nil {
		t.Fatal(publishErr)
	}
	before := filesystemPVS(t, env, req.GetName())
	gen, uid, source := before.Status.PublicationGeneration, before.UID, before.Spec.FilesystemAdoption.CanonicalSource
	retry, err := env.srv.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	after := filesystemPVS(t, env, req.GetName())
	if retry.GetVolume().GetVolumeId() != resp.GetVolume().GetVolumeId() || after.UID != uid ||
		after.Status.PublicationGeneration != gen || after.Spec.FilesystemAdoption.CanonicalSource != source {
		t.Fatalf("retry rewrote completed lifecycle: before=%+v after=%+v retry=%q",
			before.Status, after.Status, retry.GetVolume().GetVolumeId())
	}
}
