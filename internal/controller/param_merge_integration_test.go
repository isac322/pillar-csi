//go:build integration

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

package controller

// These specs drive provisioning end to end through a StorageClass the
// PillarStorageClass reconciler generated (issue #112): the reconciler
// writes the StorageClass into the suite's envtest API server, CreateVolume
// receives exactly its parameters plus the claim metadata external-provisioner
// adds with --extra-create-metadata, and a real agent.Server hands the merged
// backend parameters to a recording backend.  The PillarStore, the
// PillarStorageClass overrides and the PVC annotations must all reach the
// backend with precedence PVC > PillarStorageClass > PillarStore.

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	pillarcsi "github.com/bhyoo/pillar-csi/internal/csi"
)

const (
	mergeAgent    = "merge-agent"
	mergeProtocol = "merge-protocol"
	mergeZFSPool  = "tank"
	mergeLVMVG    = "data-vg"
)

// recordingBackend records the BackendParams of every Create call.
type recordingBackend struct {
	backendType agentv1.BackendType
	mu          sync.Mutex
	created     map[string]*agentv1.BackendParams
}

func newRecordingBackend(bt agentv1.BackendType) *recordingBackend {
	return &recordingBackend{backendType: bt, created: map[string]*agentv1.BackendParams{}}
}

func (b *recordingBackend) Create(
	_ context.Context, volumeID string, capacity int64, params *agentv1.BackendParams,
) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created[volumeID] = proto.Clone(params).(*agentv1.BackendParams)
	return b.DevicePath(volumeID), capacity, nil
}

// params returns the BackendParams of the volume named name, whatever pool
// or dataset prefix the agent volume ID carries.
func (b *recordingBackend) params(name string) *agentv1.BackendParams {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, params := range b.created {
		if id == name || strings.HasSuffix(id, "/"+name) {
			return params
		}
	}
	return nil
}

func (*recordingBackend) Delete(context.Context, string) error { return nil }
func (*recordingBackend) Expand(_ context.Context, _ string, size int64) (int64, error) {
	return size, nil
}
func (*recordingBackend) Capacity(context.Context) (int64, int64, error) {
	return 1 << 40, 1 << 40, nil
}
func (*recordingBackend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}
func (*recordingBackend) DevicePath(volumeID string) string { return "/dev/fake/" + volumeID }
func (b *recordingBackend) Type() agentv1.BackendType       { return b.backendType }
func (*recordingBackend) Layout() backend.Layout            { return backend.Layout{} }

// mergeFixture is one storage node (real agent.Server over bufconn with a
// recording ZFS and LVM backend) and the CSI controller server dialing it.
type mergeFixture struct {
	zfs, lvm *recordingBackend
	csi      *pillarcsi.ControllerServer
}

func newMergeFixture() *mergeFixture {
	f := &mergeFixture{
		zfs: newRecordingBackend(agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL),
		lvm: newRecordingBackend(agentv1.BackendType_BACKEND_TYPE_LVM),
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, agent.NewServer(
		map[string]backend.VolumeBackend{mergeZFSPool: f.zfs, mergeLVMVG: f.lvm},
		GinkgoT().TempDir(), agent.WithDrainStateDir(GinkgoT().TempDir())))
	go func() { _ = gs.Serve(lis) }()
	DeferCleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	agentClient := agentv1.NewAgentServiceClient(conn)

	f.csi = pillarcsi.NewControllerServerWithDialer(k8sClient, "pillar-csi.bhyoo.com",
		func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agentClient, nopClose{}, nil
		})
	return f
}

// createReady creates obj and marks it Ready, removing its finalizers and
// deleting it at cleanup.
func createReady(obj client.Object, conditions *[]metav1.Condition) {
	Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	DeferCleanup(func() {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			return
		}
		obj.SetFinalizers(nil)
		Expect(client.IgnoreNotFound(k8sClient.Update(ctx, obj))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
	})
	if conditions == nil {
		return
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Test", Message: "ready",
	})
	Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
}

func createMergeAgentAndProtocol() {
	pa := &pillarcsiv1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: mergeAgent},
		Spec: pillarcsiv1alpha1.PillarAgentSpec{
			External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.56", Port: 9500},
		},
	}
	Expect(k8sClient.Create(ctx, pa)).To(Succeed())
	DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pa))).To(Succeed()) })
	pa.Status.ResolvedAddress = "192.0.2.56:9500"
	Expect(k8sClient.Status().Update(ctx, pa)).To(Succeed())

	ctrlLossTmo, reconnectDelay := int32(1800), int32(5)
	protocol := &pillarcsiv1alpha1.PillarProtocol{
		ObjectMeta: metav1.ObjectMeta{Name: mergeProtocol},
		Spec: pillarcsiv1alpha1.PillarProtocolSpec{
			Type: pillarcsiv1alpha1.ProtocolTypeNVMeOFTCP,
			NVMeOFTCP: &pillarcsiv1alpha1.NVMeOFTCPConfig{
				Port: 4420, ACL: true, CtrlLossTmo: &ctrlLossTmo, ReconnectDelay: &reconnectDelay,
			},
		},
	}
	createReady(protocol, &protocol.Status.Conditions)
}

// generateStorageClass creates binding, reconciles it and returns the
// StorageClass the reconciler generated.
func generateStorageClass(binding *pillarcsiv1alpha1.PillarStorageClass) *storagev1.StorageClass {
	createReady(binding, nil)
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx,
			&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: binding.Name}}))).To(Succeed())
	})
	r := &PillarStorageClassReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	sc := &storagev1.StorageClass{}
	Eventually(func() error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: binding.Name}})
		if err != nil {
			return err
		}
		return k8sClient.Get(ctx, types.NamespacedName{Name: binding.Name}, sc)
	}).Should(Succeed())
	return sc
}

// provision calls CreateVolume the way external-provisioner does for claim
// pvc of StorageClass sc, and returns the response.
func provision(f *mergeFixture, sc *storagev1.StorageClass, pvc *corev1.PersistentVolumeClaim) (
	*csi.CreateVolumeResponse, error,
) {
	params := map[string]string{
		"csi.storage.k8s.io/pvc/name":      pvc.Name,
		"csi.storage.k8s.io/pvc/namespace": pvc.Namespace,
		"csi.storage.k8s.io/pv/name":       "pvc-" + string(pvc.UID),
	}
	for k, v := range sc.Parameters {
		params[k] = v
	}
	return f.csi.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:          "pvc-" + string(pvc.UID),
		Parameters:    params,
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
	})
}

func createClaim(name, storageClass string, annotations map[string]string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClass,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvc))).To(Succeed()) })
	return pvc
}

func cleanupVolumeState(name string) {
	DeferCleanup(func() {
		pvs := &pillarcsiv1alpha1.PillarVolumeState{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, pvs); err != nil {
			return
		}
		pvs.SetFinalizers(nil)
		Expect(client.IgnoreNotFound(k8sClient.Update(ctx, pvs))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvs))).To(Succeed())
	})
}

var _ = Describe("Parameter merge through a generated StorageClass", func() {
	It("hands the zvol the store, binding and PVC settings with PVC > binding > store precedence", func() {
		f := newMergeFixture()
		createMergeAgentAndProtocol()

		store := &pillarcsiv1alpha1.PillarStore{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-zfs-store"},
			Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: mergeAgent,
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					ZFS: &pillarcsiv1alpha1.ZFSBackendConfig{
						Pool: mergeZFSPool, ParentDataset: "k8s",
						Properties: map[string]string{
							"compression": "off", "dedup": "off", "refreservation": "none", "logbias": "latency",
						},
					},
				},
			},
		}
		createReady(store, &store.Status.Conditions)

		sc := generateStorageClass(&pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-zfs"},
			Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: store.Name, ProtocolRef: mergeProtocol,
				Overrides: &pillarcsiv1alpha1.StorageClassOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{
						ZFS: &pillarcsiv1alpha1.ZFSPropertyOverrides{Properties: map[string]string{
							"compression": "lz4", "logbias": "throughput", "volblocksize": "16K",
						}},
					},
				},
			},
		})

		pvc := createClaim("merge-zfs-claim", sc.Name, map[string]string{
			pillarcsi.AnnotationBackendOverride: "zfs:\n  properties:\n" +
				"    compression: zstd\n    volblocksize: 128K\n    sync: always\n",
			pillarcsi.AnnotationProtocolOverride: "nvmeofTcp:\n  ctrlLossTmo: 600\n",
		})
		cleanupVolumeState("pvc-" + string(pvc.UID))

		resp, err := provision(f, sc, pvc)
		Expect(err).NotTo(HaveOccurred())

		params := f.zfs.params("pvc-" + string(pvc.UID))
		Expect(params).NotTo(BeNil(), "the agent backend was not asked to create the zvol")
		Expect(params.GetZfs().GetParentDataset()).To(Equal("k8s"))
		Expect(params.GetZfs().GetProperties()).To(Equal(map[string]string{
			"compression":    "zstd",       // PVC > binding > store
			"volblocksize":   "128K",       // PVC > binding
			"sync":           "always",     // PVC only
			"logbias":        "throughput", // binding > store
			"dedup":          "off",        // store only
			"refreservation": "none",       // store only
		}))

		vc := resp.GetVolume().GetVolumeContext()
		Expect(vc).To(HaveKeyWithValue("pillar-csi.bhyoo.com/nvmeof-ctrl-loss-tmo", "600"),
			"PVC protocol-override must win over the PillarProtocol value")
		Expect(vc).To(HaveKeyWithValue("pillar-csi.bhyoo.com/nvmeof-reconnect-delay", "5"),
			"PillarProtocol value must reach the node when no layer overrides it")
	})

	It("hands the LV the store provisioning mode unless the binding or PVC overrides it", func() {
		f := newMergeFixture()
		createMergeAgentAndProtocol()

		store := &pillarcsiv1alpha1.PillarStore{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-lvm-store"},
			Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: mergeAgent,
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeLVMLV,
					LVM: &pillarcsiv1alpha1.LVMBackendConfig{
						VolumeGroup: mergeLVMVG, ThinPool: "thin-pool",
						ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeThin,
					},
				},
			},
		}
		createReady(store, &store.Status.Conditions)

		storeSC := generateStorageClass(&pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-lvm-store-mode"},
			Spec:       pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: store.Name, ProtocolRef: mergeProtocol},
		})
		bindingSC := generateStorageClass(&pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-lvm-binding-mode"},
			Spec: pillarcsiv1alpha1.PillarStorageClassSpec{
				StoreRef: store.Name, ProtocolRef: mergeProtocol,
				Overrides: &pillarcsiv1alpha1.StorageClassOverrides{
					Backend: &pillarcsiv1alpha1.BackendOverrides{
						LVM: &pillarcsiv1alpha1.LVMOverrides{ProvisioningMode: pillarcsiv1alpha1.LVMProvisioningModeLinear},
					},
				},
			},
		})

		cases := []struct {
			sc          *storagev1.StorageClass
			claim, want string
			annotations map[string]string
		}{
			{sc: storeSC, claim: "merge-lvm-store", want: "thin"},
			{sc: bindingSC, claim: "merge-lvm-binding", want: "linear"},
			{sc: bindingSC, claim: "merge-lvm-pvc", want: "thin", annotations: map[string]string{
				pillarcsi.AnnotationBackendOverride: "lvm:\n  provisioningMode: thin\n",
			}},
		}
		for _, c := range cases {
			pvc := createClaim(c.claim, c.sc.Name, c.annotations)
			cleanupVolumeState("pvc-" + string(pvc.UID))
			_, err := provision(f, c.sc, pvc)
			Expect(err).NotTo(HaveOccurred(), c.claim)
			params := f.lvm.params("pvc-" + string(pvc.UID))
			Expect(params).NotTo(BeNil(), "%s: the agent backend was not asked to create the LV", c.claim)
			Expect(params.GetLvm().GetProvisionMode()).To(Equal(c.want), c.claim)
		}
	})

	It("fails provisioning instead of dropping the binding settings when the binding is gone", func() {
		f := newMergeFixture()
		createMergeAgentAndProtocol()

		store := &pillarcsiv1alpha1.PillarStore{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-gone-store"},
			Spec: pillarcsiv1alpha1.PillarStoreSpec{
				AgentRef: mergeAgent,
				Backend: pillarcsiv1alpha1.BackendSpec{
					Type: pillarcsiv1alpha1.BackendTypeZFSZvol,
					ZFS:  &pillarcsiv1alpha1.ZFSBackendConfig{Pool: mergeZFSPool},
				},
			},
		}
		createReady(store, &store.Status.Conditions)
		binding := &pillarcsiv1alpha1.PillarStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "merge-gone"},
			Spec:       pillarcsiv1alpha1.PillarStorageClassSpec{StoreRef: store.Name, ProtocolRef: mergeProtocol},
		}
		sc := generateStorageClass(binding)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(binding), binding)).To(Succeed())
		binding.SetFinalizers(nil)
		Expect(k8sClient.Update(ctx, binding)).To(Succeed())
		Expect(k8sClient.Delete(ctx, binding)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(binding), &pillarcsiv1alpha1.PillarStorageClass{})
		}).ShouldNot(Succeed())

		pvc := createClaim("merge-gone-claim", sc.Name, nil)
		cleanupVolumeState("pvc-" + string(pvc.UID))
		_, err := provision(f, sc, pvc)
		Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "err = %v", err)
		Expect(f.zfs.params("pvc-"+string(pvc.UID))).To(BeNil(), "no volume may be created without its settings")
	})
})
