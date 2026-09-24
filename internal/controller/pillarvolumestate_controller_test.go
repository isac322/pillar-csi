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

// These specs drive the production resync path end to end against the
// suite's envtest API server — the PillarVolumeState reconciler, the CSI
// controller server, the agent gRPC client and a real agent.Server writing
// configfs — after the storage node lost its target state.  The
// PillarVolumeState UID and status schema are the API server's; the only
// substitutes are the in-memory gRPC transport, a temp configfs root and
// state dir, and a backend that only reports the device path.

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pillarcsiv1alpha1 "github.com/bhyoo/pillar-csi/api/v1alpha1"
	agentv1 "github.com/bhyoo/pillar-csi/gen/go/pillar_csi/agent/v1"
	"github.com/bhyoo/pillar-csi/internal/agent"
	"github.com/bhyoo/pillar-csi/internal/agent/backend"
	"github.com/bhyoo/pillar-csi/internal/csi"
)

const (
	pvsResyncAgent  = "pvs-resync-agent"
	pvsResyncName   = "pvc-pvs-resync"
	pvsResyncVolID  = "tank/pvc-pvs-resync"
	pvsResyncNQN    = "nqn.2026-01.com.bhyoo.pillar-csi:tank.pvc-pvs-resync"
	pvsResyncHost   = "nqn.2023-01.io.example:pvs-resync-host"
	pvsResyncDevice = "/dev/zvol/tank/pvc-pvs-resync"
)

type pvsResyncBackend struct{}

func (pvsResyncBackend) Create(context.Context, string, int64, *agentv1.BackendParams) (string, int64, error) {
	return pvsResyncDevice, 1 << 30, nil
}
func (pvsResyncBackend) Delete(context.Context, string) error                 { return nil }
func (pvsResyncBackend) Expand(context.Context, string, int64) (int64, error) { return 1 << 30, nil }
func (pvsResyncBackend) Capacity(context.Context) (int64, int64, error)       { return 1 << 40, 1 << 40, nil }
func (pvsResyncBackend) ListVolumes(context.Context) ([]*agentv1.VolumeInfo, error) {
	return nil, nil
}
func (pvsResyncBackend) DevicePath(string) string  { return pvsResyncDevice }
func (pvsResyncBackend) Type() agentv1.BackendType { return agentv1.BackendType_BACKEND_TYPE_ZFS_ZVOL }

type nopClose struct{}

func (nopClose) Close() error { return nil }

// pvsResyncFixture is one storage node (real agent.Server) and the CSI
// controller server the reconciler drives, sharing the suite API server.
type pvsResyncFixture struct {
	cfgRoot        string
	agentClient    agentv1.AgentServiceClient
	reconcileCalls *atomic.Int32
	reconciler     *PillarVolumeStateReconciler
}

func newPVSResyncFixture() *pvsResyncFixture {
	cfgRoot := GinkgoT().TempDir()
	calls := &atomic.Int32{}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(func(
		ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler,
	) (any, error) {
		if info.FullMethod == agentv1.AgentService_ReconcileState_FullMethodName {
			calls.Add(1)
		}
		return h(ctx, req)
	}))
	agentv1.RegisterAgentServiceServer(gs,
		agent.NewServer(map[string]backend.VolumeBackend{"tank": pvsResyncBackend{}}, cfgRoot,
			agent.WithDrainStateDir(GinkgoT().TempDir())))
	go func() { _ = gs.Serve(lis) }()
	DeferCleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///agent",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	agentClient := agentv1.NewAgentServiceClient(conn)

	exports := csi.NewControllerServerWithDialer(k8sClient, "pillar-csi.bhyoo.com",
		func(context.Context, string) (agentv1.AgentServiceClient, io.Closer, error) {
			return agentClient, nopClose{}, nil
		})
	return &pvsResyncFixture{
		cfgRoot:        cfgRoot,
		agentClient:    agentClient,
		reconcileCalls: calls,
		reconciler:     &PillarVolumeStateReconciler{Client: k8sClient, Exports: exports},
	}
}

func (f *pvsResyncFixture) subsystemPath(elem ...string) string {
	return filepath.Join(append([]string{f.cfgRoot, "nvmet", "subsystems", pvsResyncNQN}, elem...)...)
}

// createResyncAgent creates the PillarAgent with a resolved address; no
// PillarAgent reconciler runs in this suite, so no finalizer is added.
func createResyncAgent() {
	pa := &pillarcsiv1alpha1.PillarAgent{
		ObjectMeta: metav1.ObjectMeta{Name: pvsResyncAgent},
		Spec: pillarcsiv1alpha1.PillarAgentSpec{
			External: &pillarcsiv1alpha1.ExternalSpec{Address: "192.0.2.55", Port: 9500},
		},
	}
	Expect(k8sClient.Create(ctx, pa)).To(Succeed())
	DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pa))).To(Succeed()) })
	pa.Status.ResolvedAddress = "192.0.2.55:9500"
	Expect(k8sClient.Status().Update(ctx, pa)).To(Succeed())
}

// createPublishedVolume creates the PillarVolumeState of a Ready volume
// published to one node, with its export configuration, at publication
// generation 1, and returns it with its API-server-assigned UID.
func createPublishedVolume() *pillarcsiv1alpha1.PillarVolumeState {
	pvs := &pillarcsiv1alpha1.PillarVolumeState{
		ObjectMeta: metav1.ObjectMeta{Name: pvsResyncName},
		Spec: pillarcsiv1alpha1.PillarVolumeStateSpec{
			VolumeID:      pvsResyncAgent + "/nvmeof-tcp/zfs-zvol/" + pvsResyncVolID,
			AgentVolumeID: pvsResyncVolID,
			AgentRef:      pvsResyncAgent,
			BackendType:   "zfs-zvol",
			ProtocolType:  "nvmeof-tcp",
		},
	}
	Expect(k8sClient.Create(ctx, pvs)).To(Succeed())
	DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pvs))).To(Succeed()) })
	Expect(pvs.UID).NotTo(BeEmpty())

	pvs.Status = pillarcsiv1alpha1.PillarVolumeStateStatus{
		Phase:                 pillarcsiv1alpha1.PillarVolumeStatePhaseReady,
		ExportSpec:            &pillarcsiv1alpha1.VolumeExportSpec{BindAddress: "10.0.0.1", Port: 4420, ACLEnabled: true},
		PublicationGeneration: 1,
		PublishedNodes: []pillarcsiv1alpha1.VolumePublication{
			{NodeID: "worker-1", InitiatorID: pvsResyncHost, AccessMode: "SINGLE_NODE_WRITER"},
		},
	}
	Expect(k8sClient.Status().Update(ctx, pvs)).To(Succeed())
	return pvs
}

var _ = Describe("PillarVolumeState export resync", func() {
	It("restores a published volume's export and exact ACL after the storage node lost its target state", func() {
		f := newPVSResyncFixture()
		createResyncAgent()
		pvs := createPublishedVolume()

		// The storage node had the export and the published node's grant,
		// written at generation 1 of this volume lifecycle.
		fence := &agentv1.FencingToken{VolumeUid: string(pvs.UID), Generation: 1}
		_, err := f.agentClient.ExportVolume(ctx, &agentv1.ExportVolumeRequest{
			VolumeId: pvsResyncVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			DevicePath: pvsResyncDevice, AclEnabled: true, Fence: fence,
			ExportParams: &agentv1.ExportParams{Params: &agentv1.ExportParams_NvmeofTcp{
				NvmeofTcp: &agentv1.NvmeofTcpExportParams{BindAddress: "10.0.0.1", Port: 4420},
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = f.agentClient.AllowInitiator(ctx, &agentv1.AllowInitiatorRequest{
			VolumeId: pvsResyncVolID, ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
			InitiatorId: pvsResyncHost, Fence: fence,
		})
		Expect(err).NotTo(HaveOccurred())

		// Storage node reboot: configfs is volatile and comes back empty.
		Expect(os.RemoveAll(filepath.Join(f.cfgRoot, "nvmet"))).To(Succeed())

		res, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: pvsResyncName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(volumeExportResyncInterval))

		Expect(f.subsystemPath("namespaces", "1")).To(BeADirectory(), "export not restored")
		_, err = os.Lstat(f.subsystemPath("allowed_hosts", pvsResyncHost))
		Expect(err).NotTo(HaveOccurred(), "ACL of the published node not restored")
		raw, err := os.ReadFile(f.subsystemPath("attr_allow_any_host"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(HavePrefix("0"), "restored target must stay closed to unpublished hosts")

		got := &pillarcsiv1alpha1.PillarVolumeState{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvsResyncName}, got)).To(Succeed())
		Expect(got.Status.ExportSpec).To(Equal(pvs.Status.ExportSpec), "exportSpec must survive the API server schema")
		Expect(got.Status.Conditions).To(ContainElement(And(
			HaveField("Type", csi.ConditionExportReconciled),
			HaveField("Status", metav1.ConditionTrue),
		)))
	})

	It("neither calls the agent nor requeues for a volume whose PillarVolumeState is gone", func() {
		f := newPVSResyncFixture()

		res, err := f.reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "pvc-pvs-resync-gone"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())
		Expect(f.reconcileCalls.Load()).To(BeZero())
		Expect(filepath.Join(f.cfgRoot, "nvmet")).NotTo(BeADirectory())
	})
})
