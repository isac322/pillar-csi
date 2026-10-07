//go:build e2e && e2e_helm

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"
)

var _ = Describe("E71: adopted filesystem physical network consumers", Label("e71", "nfs"), Serial, func() {
	var ctx context.Context
	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 15*time.Minute)
		DeferCleanup(cancel)
		Expect(os.Getenv("E2E_NFS_E2E")).To(Equal("true"), "requires dedicated real kernel NFS/Helm lane")
		Expect(suiteKindCluster).NotTo(BeNil())
		Expect(suiteKindCluster.clusterCreated).To(BeTrue(), "host helper isolation requires an owned ephemeral Kind cluster")
		Expect(resolveUseExistingCluster()).To(BeFalse())
	})

	It("[TC-E71.17] shares an existing bounded directory across two remote nodes through an owned NFS bind proxy", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.17", "directory")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeRemoteMounts(ctx)
		n.observeProxy(ctx)
		Expect(n.state(ctx).Status.PublishedNodes).To(HaveLen(2))
	})

	It("[TC-E71.18] shares an existing dataset without changing its GUID, mountpoint, DAC or properties", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.18", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeRemoteMounts(ctx)
		n.observeProxy(ctx)
		for property, value := range n.OriginalProperties {
			actual, err := n.HostExec(ctx, "zfs", "get", "-Hp", "-o", "value", property, n.Dataset)
			Expect(err).NotTo(HaveOccurred())
			Expect(actual).To(Equal(value), property)
		}
	})

	It("[TC-E71.19] root-squashes remote root and denies a never-published client without changing source permissions", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.19", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		n.consumer(ctx, "root", n.Clients[0], 0, 0)
		out, err := n.python(ctx, "root", `import errno,os
try:
    f=os.open('/data/root-must-not-write',os.O_CREAT|os.O_WRONLY,0o600)
except OSError as e:
    assert e.errno == errno.EACCES, e
    print('kernel-EACCES')
else:
    os.close(f)
    raise AssertionError('remote root bypassed original DAC')`)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("kernel-EACCES"))
		n.denyUnpublishedClient(ctx)
		n.mustPython(ctx, "writer-1", `import os; assert not os.path.exists('/data/unauthorized'); assert not os.path.exists('/data/root-must-not-write')`)
		n.observeProxy(ctx)
		// "root" shares Clients[0]'s publish record with writer-0. Deleting it
		// first exercises the same-node peer path: writer-0's target entry and
		// mount must survive, and AfterDelete proves the surviving peer still
		// performs real fsynced I/O after each removal.
		for j, pod := range n.Pods {
			if pod == "root" && j > 0 {
				n.Pods[0], n.Pods[j] = n.Pods[j], n.Pods[0]
				n.PodUIDs[0], n.PodUIDs[j] = n.PodUIDs[j], n.PodUIDs[0]
				n.PodNodes[0], n.PodNodes[j] = n.PodNodes[j], n.PodNodes[0]
			}
		}
		Expect(n.Pods[0]).To(Equal("root"))
		n.AfterDelete = func(hookCtx context.Context, remaining []string) {
			for _, peer := range remaining {
				if peer == "root" {
					continue // root-squashed peer is mount-witnessed, not writable
				}
				Expect(n.mustPython(hookCtx, peer, fmt.Sprintf(`import os; f=open('/data/peer-alive-%s.bin','wb'); f.write(b'alive'); f.flush(); os.fsync(f.fileno()); f.close(); print(open('/data/peer-alive-%s.bin').read())`, peer, peer))).To(Equal("alive"))
			}
		}
		n.unpublishAll(ctx)
	})

	It("[TC-E71.20] enforces real two-node POSIX byte-range locking and releases locks for the next consumer", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.20", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.mustPython(ctx, "writer-0", `import subprocess
script="import fcntl,time,os\nf=open('/data/byte-range.lock','w+b')\nfcntl.lockf(f,fcntl.LOCK_EX,1,0,os.SEEK_SET)\nopen('/tmp/lock-ready','w').close()\nwhile not os.path.exists('/tmp/release-lock'): time.sleep(.1)\nfcntl.lockf(f,fcntl.LOCK_UN,1,0,os.SEEK_SET)\nf.close()\nopen('/tmp/lock-released','w').close()\n"
open('/tmp/lock-holder.py','w').write(script)
p=subprocess.Popen(['python3','/tmp/lock-holder.py'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
open('/tmp/lock-holder.pid','w').write(str(p.pid))`)
		DeferCleanup(func() {
			_, _ = n.python(context.Background(), "writer-0", `import os,signal; open('/tmp/release-lock','w').close(); os.kill(int(open('/tmp/lock-holder.pid').read()),signal.SIGTERM)`)
		})
		Eventually(func() (string, error) {
			return n.python(ctx, "writer-0", `import os; print(os.path.exists('/tmp/lock-ready'))`)
		}, 30*time.Second, time.Second).Should(Equal("True"))
		Expect(n.mustPython(ctx, "writer-1", `import fcntl,errno,os
f=open('/data/byte-range.lock','r+b')
try: fcntl.lockf(f,fcntl.LOCK_EX|fcntl.LOCK_NB,1,0,os.SEEK_SET)
except OSError as e: assert e.errno in (errno.EAGAIN,errno.EACCES)
else: raise AssertionError('overlapping byte range was not exclusive')
fcntl.lockf(f,fcntl.LOCK_EX|fcntl.LOCK_NB,1,1,os.SEEK_SET)
fcntl.lockf(f,fcntl.LOCK_UN,1,1,os.SEEK_SET)
print('overlap-denied-disjoint-granted')`)).To(Equal("overlap-denied-disjoint-granted"))
		n.mustPython(ctx, "writer-0", `open('/tmp/release-lock','w').close()`)
		Eventually(func() (string, error) {
			return n.python(ctx, "writer-0", `import os; print(os.path.exists('/tmp/lock-released'))`)
		}, 30*time.Second, time.Second).Should(Equal("True"))
		Expect(n.mustPython(ctx, "writer-1", `import fcntl,os; f=open('/data/byte-range.lock','r+b'); fcntl.lockf(f,fcntl.LOCK_EX|fcntl.LOCK_NB,1,0,os.SEEK_SET); f.write(b'next-owner'); f.flush(); os.fsync(f.fileno()); fcntl.lockf(f,fcntl.LOCK_UN,1,0,os.SEEK_SET); print('next-owner')`)).To(Equal("next-owner"))
		Expect(n.mustPython(ctx, "writer-0", `print(open('/data/byte-range.lock').read())`)).To(Equal("next-owner"))
	})

	It("[TC-E71.21] serves a true storage-node local bind concurrently with both remote publishers", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.21", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		n.consumer(ctx, "local", n.StorageNode, n.SourceUID, n.SourceGID)
		n.mustPython(ctx, "local", `import os; f=open('/data/local-marker','wb'); f.write(b'local-direct-data'); f.flush(); os.fsync(f.fileno()); f.close()`)
		for _, pod := range []string{"writer-0", "writer-1"} {
			Expect(n.mustPython(ctx, pod, `print(open('/data/local-marker').read())`)).To(Equal("local-direct-data"))
		}
		mount := n.mustPython(ctx, "local", `print(next(line.strip() for line in open('/proc/mounts') if line.split()[1]=='/data'))`)
		Expect(strings.Fields(mount)[2]).To(Equal("zfs"))
		Expect(strings.Fields(mount)[0]).To(Equal(n.Dataset))
		Expect(mount).NotTo(ContainSubstring("/dev/mapper"))
		Expect(mount).NotTo(ContainSubstring(n.Address + ":"))
		Eventually(func() []pillarv1.VolumePublication { return n.state(ctx).Status.PublishedNodes }, 90*time.Second, 2*time.Second).Should(HaveLen(3))
		pubs := n.state(ctx).Status.PublishedNodes
		nodes := []string{}
		for _, p := range pubs {
			nodes = append(nodes, p.NodeID)
			if p.NodeID == n.StorageNode {
				Expect(p.Local).To(BeTrue())
			} else {
				Expect(p.Local).To(BeFalse())
				Expect(p.InitiatorID).To(Equal(n.Must(ctx, "get", "node", p.NodeID, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)))
			}
		}
		Expect(nodes).To(ConsistOf(n.Clients[0], n.Clients[1], n.StorageNode))
		n.exchange(ctx)
		n.observeProxy(ctx)
		// Prove the native unpublish drain of all three publishers — including
		// the storage-node local bind — before generic PV cleanup can mask a
		// stuck publication as a finalizer timeout.
		n.unpublishAll(ctx)
	})

	It("[TC-E71.22] recovers published remote mounts and data after the installed node plugin restarts", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.22", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		for _, node := range n.Clients {
			n.restart(ctx, "node", node)
		}
		n.observeRemoteMounts(ctx)
		n.verifyExchange(ctx)
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		n.unpublishAll(ctx)
	})

	It("[TC-E71.23] recovers proxy and export after agent restart while preserving complete native and old-block bootstrap profiles", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.23", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		n.restart(ctx, "agent", n.StorageNode)
		Eventually(func() map[string][]string { return nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), n.Proxy) }, 90*time.Second, 2*time.Second).Should(HaveLen(2))
		n.observeProxy(ctx)
		n.observeRemoteMounts(ctx)
		n.verifyExchange(ctx)
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		// New real adoption plus legacy block IO proves that restart restored the
		// bootstrap union rather than a successful-but-partial profile set.
		d := newFilesystemNetworkFixture(ctx, "E71.23-peer", "directory")
		d.ownerConsumers(ctx)
		d.exchange(ctx)
		d.observeProxy(ctx)
		filesystemNetworkOldBlockControl(ctx, n, "post-restart", "ext4", "ReadWriteOnce", "File")
	})

	It("[TC-E71.24] refuses unsafe native quota or GUID recovery while an independent healthy export continues", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.24", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		healthy := newFilesystemNetworkFixture(ctx, "E71.24-healthy", "zfs")
		healthy.ownerConsumers(ctx)
		healthy.exchange(ctx)
		healthy.observeProxy(ctx)
		for _, drift := range []func(context.Context) (func() error, error){n.WithQuotaDrift, n.WithIdentityDrift} {
			Expect(nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), n.Proxy)).To(HaveLen(2), "the unsafe export must be active before drift")
			restore, err := drift(ctx)
			Expect(err).NotTo(HaveOccurred(), n.exportDiagnostics())
			func() {
				defer func() { Expect(restore()).To(Succeed(), n.exportDiagnostics()) }()
				n.restart(ctx, "agent", n.StorageNode)
				Eventually(func() map[string][]string { return nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), n.Proxy) }, 90*time.Second, 2*time.Second).Should(BeEmpty(), n.exportDiagnostics())
				Expect(nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), healthy.Proxy)).To(HaveLen(2), healthy.exportDiagnostics())
				healthy.exchange(ctx)
			}()
			n.restart(ctx, "agent", n.StorageNode)
			Eventually(func() map[string][]string { return nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), n.Proxy) }, 90*time.Second, 2*time.Second).Should(HaveLen(2), n.exportDiagnostics())
			n.verifyExchange(ctx)
			n.observeProxy(ctx)
		}
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("[TC-E71.25] protects publications from deletion and cleans every client publication and owned export without deleting the source", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.25", "directory")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		n.Must(ctx, "-n", n.Namespace, "delete", "pvc", n.PVCName, "--wait=false")
		Consistently(func() string {
			return n.Must(ctx, "-n", n.Namespace, "get", "pvc", n.PVCName, "-o", "jsonpath={.metadata.deletionTimestamp}")
		}, 5*time.Second, time.Second).ShouldNot(BeEmpty())
		Expect(n.state(ctx).Status.PublishedNodes).To(HaveLen(2))
		n.verifyExchange(ctx)
		n.unpublishAll(ctx)
		Eventually(func() (string, error) {
			return n.Kubectl(ctx, "", "get", "pillarvolumestate", n.PVName, "--ignore-not-found", "-o", "name")
		}, 90*time.Second, 2*time.Second).Should(BeEmpty())
		n.assertWithdrawn(ctx)
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("[TC-E71.26] retains the actual source and ownership, and rejects an old lifecycle UID after Delete", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.26", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeProxy(ctx)
		state := n.state(ctx)
		a := state.Spec.FilesystemAdoption
		adoption := &agentv1.FilesystemAdoption{Kind: string(a.Kind), CanonicalSource: a.CanonicalSource, ResourceId: a.ResourceID, HostPath: a.HostPath, FilesystemType: a.FilesystemType}
		request := &agentv1.ImportVolumeRequest{VolumeId: state.Spec.AgentVolumeID, CapacityBytes: state.Spec.CapacityBytes, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, FilesystemAdoption: adoption, BackendParams: &agentv1.BackendParams{Params: &agentv1.BackendParams_Zfs{Zfs: &agentv1.ZfsVolumeParams{Pool: strings.Split(n.Dataset, "/")[0], ParentDataset: strings.TrimPrefix(n.Dataset[:strings.LastIndex(n.Dataset, "/")], strings.Split(n.Dataset, "/")[0]+"/")}}}, Fence: &agentv1.FencingToken{VolumeUid: "stale-" + string(state.UID), Generation: uint64(state.Status.PublicationGeneration) + 100}}
		n.withAgent(ctx, func(client agentv1.AgentServiceClient) {
			_, err := client.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{VolumeId: state.Spec.AgentVolumeID, BackendType: agentv1.BackendType_BACKEND_TYPE_ZFS_DATASET, FilesystemAdoption: adoption, Fence: &agentv1.FencingToken{VolumeUid: string(state.UID), Generation: uint64(state.Status.PublicationGeneration)}})
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "a mounted publisher prevents the actual agent terminal operation")
			_, err = client.ImportVolume(ctx, request)
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition))
		})
		n.exchange(ctx)
		n.observeProxy(ctx)
		n.Must(ctx, "patch", "pv", n.PVName, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`)
		n.unpublishAll(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		n.Must(ctx, "-n", n.Namespace, "delete", "pvc", n.PVCName, "--wait=true", "--timeout=3m")
		Expect(n.state(ctx).UID).To(Equal(state.UID))
		n.Must(ctx, "patch", "pv", n.PVName, "--type=json", "-p", `[{"op":"remove","path":"/spec/claimRef"}]`)
		// A manually rebound retained PV is the same lifecycle, not re-adoption.
		n.PVCName = "retained-rebound"
		claim := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": n.PVCName, "namespace": n.Namespace}, "spec": map[string]any{"storageClassName": n.StorageClass, "volumeName": n.PVName, "accessModes": []string{"ReadWriteMany"}, "resources": map[string]any{"requests": map[string]string{"storage": "64Mi"}}}}
		data, err := json.Marshal(claim)
		Expect(err).NotTo(HaveOccurred())
		n.apply(ctx, string(data))
		n.Must(ctx, "-n", n.Namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+n.PVCName, "--timeout=3m")
		n.Pods, n.PodUIDs, n.PodNodes = nil, nil, nil
		n.ownerConsumers(ctx)
		n.verifyExchange(ctx)
		Expect(n.state(ctx).UID).To(Equal(state.UID))
		n.unpublishAll(ctx)
		n.Must(ctx, "patch", "pv", n.PVName, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
		n.Must(ctx, "-n", n.Namespace, "delete", "pvc", n.PVCName, "--wait=true", "--timeout=3m")
		Eventually(func() (string, error) {
			return n.Kubectl(ctx, "", "get", "pillarvolumestate", n.PVName, "--ignore-not-found", "-o", "name")
		}, 90*time.Second, 2*time.Second).Should(BeEmpty())
		n.assertWithdrawn(ctx)
		n.restart(ctx, "agent", n.StorageNode)
		request.Fence = &agentv1.FencingToken{VolumeUid: string(state.UID), Generation: uint64(state.Status.PublicationGeneration) + 1000}
		n.withAgent(ctx, func(client agentv1.AgentServiceClient) {
			_, err := client.ImportVolume(ctx, request)
			Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "retired UID must not reacquire a preserved source after agent restart")
		})
		n.assertWithdrawn(ctx)
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("[TC-E71.27] uses packaged NFS helpers with isolated host userland and leaves no mounts or source damage", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.27", "zfs")
		for _, node := range append(append([]string{}, n.Clients...), n.StorageNode) {
			nfsHideClientHelpers(ctx, node)
			node := node
			DeferCleanup(func() { nfsRestoreClientHelpers(context.Background(), node) })
			_, err := n.NodeExec(ctx, node, "sh", "-ceu", `for helper in mount.nfs mount.nfs4 exportfs rpc.mountd; do if command -v "$helper" >/dev/null 2>&1; then printf 'host helper visible: %s\n' "$helper" >&2; exit 1; fi; done`)
			Expect(err).NotTo(HaveOccurred())
		}
		for _, node := range n.Clients {
			pod := nfsComponentPod(ctx, "node", node)
			out, err := n.Kubectl(ctx, "", "-n", resolveHelmNamespace(), "exec", pod, "-c", "file-node", "--", "mount.nfs", "-V")
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry("packaged client "+node, out)
		}
		Expect(nfsAgentExec(ctx, "exportfs", "-v")).NotTo(ContainSubstring("command not found"))
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		n.observeRemoteMounts(ctx)
		n.observeProxy(ctx)
		before, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		n.unpublishAll(ctx)
		n.Must(ctx, "-n", n.Namespace, "delete", "pvc", n.PVCName, "--wait=true", "--timeout=3m")
		Eventually(func() (string, error) {
			return n.Kubectl(ctx, "", "get", "pillarvolumestate", n.PVName, "--ignore-not-found", "-o", "name")
		}, 90*time.Second, 2*time.Second).Should(BeEmpty())
		n.assertWithdrawn(ctx)
		after, err := n.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
	})

	It("[TC-E71.28] keeps installed legacy block driver fsGroup, nested files, RWO and RWOP behavior healthy beside file adoption", func() {
		n := newFilesystemNetworkFixture(ctx, "E71.28", "zfs")
		n.ownerConsumers(ctx)
		n.exchange(ctx)
		Expect(n.Must(ctx, "get", "csidriver", pillarv1.DefaultCSIDriver, "-o", "jsonpath={.spec.fsGroupPolicy}")).To(Equal("File"))
		Expect(n.Must(ctx, "get", "csidriver", pillarv1.FileCSIDriver, "-o", "jsonpath={.spec.fsGroupPolicy}")).To(Equal("None"))
		for _, control := range []struct{ fs, mode string }{{"ext4", "ReadWriteOnce"}, {"xfs", "ReadWriteOnce"}, {"ext4", "ReadWriteOncePod"}} {
			filesystemNetworkOldBlockControl(ctx, n, control.fs+"-"+strings.ToLower(control.mode), control.fs, control.mode, "File")
		}
		filesystemNetworkLegacyNoneHelmControl(ctx, n)
		n.exchange(ctx)
	})
})

func filesystemNetworkOldBlockControl(ctx context.Context, n *filesystemNetworkFixture, suffix, fs, mode, policy string) {
	name := n.Namespace + "-" + suffix
	pool, parent := os.Getenv(suiteZFSPoolEnvVar), os.Getenv(suiteNFSParentDatasetEnvVar)
	Expect(pool).NotTo(BeEmpty())
	Expect(parent).NotTo(BeEmpty())
	n.apply(ctx, fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarStore\nmetadata:\n  name: %s\nspec:\n  agentRef: %s\n  backend:\n    zfs:\n      pool: %s\n      parentDataset: %s\n      volumeType: zvol\n---\napiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarProtocol\nmetadata:\n  name: %s\nspec:\n  protocol:\n    nvmeofTcp:\n      port: 4442\n      acl: true\n", name, n.AgentName, pool, parent, name))
	n.apply(ctx, nfsBinding(name, name, name, "  filesystem:\n    fsType: "+fs+"\n"))
	claim := "block-" + suffix
	pv := ""
	workloadDone := false
	DeferCleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if !workloadDone {
			for _, prefix := range []string{"seed-", "app-", "readonly-", "mismatch-"} {
				n.Must(cleanupCtx, "-n", n.Namespace, "delete", "pod", prefix+suffix, "--ignore-not-found", "--wait=true", "--timeout=3m")
			}
			n.Must(cleanupCtx, "-n", n.Namespace, "delete", "pvc", claim, "--ignore-not-found", "--wait=true", "--timeout=3m")
			if pv != "" {
				Eventually(func() (string, error) {
					return n.Kubectl(cleanupCtx, "", "get", "pv", pv, "--ignore-not-found", "-o", "name")
				}, 90*time.Second, 2*time.Second).Should(BeEmpty())
			}
		}
		for _, kind := range []string{"pillarstorageclass", "pillarstore", "pillarprotocol", "storageclass"} {
			n.Must(cleanupCtx, "delete", kind, name, "--ignore-not-found", "--wait=true", "--timeout=60s")
		}
	})
	n.Must(ctx, "wait", "--for=condition=Ready", "pillarstorageclass/"+name, "--timeout=3m")
	Expect(n.Must(ctx, "get", "storageclass", name, "-o", "jsonpath={.provisioner}")).To(Equal(pillarv1.DefaultCSIDriver))
	manifest := strings.Replace(nfsPVCManifest(claim, name, mode, "Filesystem", nil), nfsNamespace, n.Namespace, 1)
	if fs == "xfs" {
		manifest = strings.Replace(manifest, "64Mi", "512Mi", 1)
	}
	n.apply(ctx, manifest)
	waitOutput, waitErr := n.Kubectl(ctx, "", "-n", n.Namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+claim, "--timeout=3m")
	if waitErr != nil {
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var diagnostics strings.Builder
		const maxDiagnosticBytes = 32 * 1024
		record := func(args []string, output string, diagnosticErr error) {
			if len(output) > maxDiagnosticBytes {
				output = "[earlier output truncated]\n" + output[len(output)-maxDiagnosticBytes:]
			}
			fmt.Fprintf(&diagnostics, "\n\nkubectl %s:\n%s", strings.Join(args, " "), output)
			if diagnosticErr != nil {
				fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", diagnosticErr)
			}
		}
		capture := func(args ...string) (string, error) {
			output, diagnosticErr := n.Kubectl(diagnosticCtx, "", args...)
			record(args, output, diagnosticErr)
			return output, diagnosticErr
		}
		conditions := `{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}`
		capture("-n", n.Namespace, "describe", "pvc", claim)
		capture("-n", n.Namespace, "get", "events", "--field-selector=involvedObject.kind=PersistentVolumeClaim,involvedObject.name="+claim, "-o", "wide")
		capture("get", "pillarstorageclass", name, "-o", "jsonpath="+conditions)
		capture("get", "pillarstore", name, "-o", `jsonpath=capacity={.status.capacity}{"\n"}`+conditions)
		uid, uidErr := capture("-n", n.Namespace, "get", "pvc", claim, "-o", "jsonpath={.metadata.uid}")
		if uid = strings.TrimSpace(uid); uidErr == nil && uid != "" {
			capture("get", "pillarvolumestate", "pvc-"+uid, "--ignore-not-found", "-o", `jsonpath=volumeID={.spec.volumeID} agentVolumeID={.spec.agentVolumeID} phase={.status.phase} partialFailure={.status.partialFailure}{"\n"}`+conditions)
			// Logs are filtered to the claim UID (the CSI volume name is
			// pvc-<uid>), so unrelated volumes and payloads stay out.
			for _, selector := range []string{"app.kubernetes.io/component=controller", "app.kubernetes.io/component=agent"} {
				args := []string{"-n", resolveHelmNamespace(), "logs", "-l", selector, "--all-containers=true", "--prefix=true", "--timestamps=true", "--tail=2000", "--max-log-requests=8"}
				output, logErr := n.Kubectl(diagnosticCtx, "", args...)
				var matched []string
				for _, line := range strings.Split(output, "\n") {
					if strings.Contains(line, uid) {
						matched = append(matched, line)
					}
				}
				record(append(args, "| lines containing "+uid), strings.Join(matched, "\n"), logErr)
			}
		}
		Fail(fmt.Sprintf("PVC %s/%s did not become Bound: %v\nWait output:\n%s%s", n.Namespace, claim, waitErr, waitOutput, diagnostics.String()))
	}
	pv = n.Must(ctx, "-n", n.Namespace, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
	Expect(n.Must(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.driver}")).To(Equal(pillarv1.DefaultCSIDriver))
	root := "seed-" + suffix
	n.apply(ctx, filesystemNetworkBlockPodManifest(n, root, claim, false, 0, 0, 0))
	n.Must(ctx, "-n", n.Namespace, "wait", "--for=condition=Ready", "pod/"+root, "--timeout=3m")
	n.Must(ctx, "-n", n.Namespace, "exec", root, "--", "sh", "-ceu", "mkdir -p /data/nested/deeper; printf root-owned-before-group > /data/nested/deeper/existing; chmod 770 /data/nested /data/nested/deeper; chmod 660 /data/nested/deeper/existing; sync")
	existingUID, group := int64(0), int64(2002)
	if policy == "None" {
		existingUID, group = 1001, 5555
		n.Must(ctx, "-n", n.Namespace, "exec", root, "--", "sh", "-ceu", "chown 1001:2002 /data/nested /data/nested/deeper /data/nested/deeper/existing; chmod 770 /data/nested /data/nested/deeper; chmod 660 /data/nested/deeper/existing; sync")
	}
	n.Must(ctx, "-n", n.Namespace, "delete", "pod", root, "--wait=true", "--timeout=3m")
	app := "app-" + suffix
	n.apply(ctx, filesystemNetworkBlockPodManifest(n, app, claim, false, 1001, 2002, group))
	n.Must(ctx, "-n", n.Namespace, "wait", "--for=condition=Ready", "pod/"+app, "--timeout=3m")
	out := n.Must(ctx, "-n", n.Namespace, "exec", app, "--", "sh", "-ceu", "test \"$(cat /data/nested/deeper/existing)\" = root-owned-before-group; printf app-owner > /data/nested/deeper/new; printf append >> /data/nested/deeper/existing; sync; stat -c '%u:%g' /data/nested/deeper/existing /data/nested/deeper/new; cat /data/nested/deeper/existing")
	Expect(out).To(ContainSubstring(fmt.Sprintf("%d:2002", existingUID)))
	Expect(out).To(ContainSubstring("1001:2002"))
	Expect(out).To(ContainSubstring("root-owned-before-groupappend"))
	if policy == "None" {
		Expect(n.Must(ctx, "-n", n.Namespace, "exec", app, "--", "stat", "-c", "%u:%g:%a", "/data/nested/deeper/existing")).To(Equal("1001:2002:660"))
	}
	mount := n.Must(ctx, "-n", n.Namespace, "exec", app, "--", "sh", "-ceu", `awk '$2 == "/data" { print $3 }' /proc/mounts`)
	Expect(mount).To(Equal(fs))
	n.Must(ctx, "-n", n.Namespace, "delete", "pod", app, "--wait=true", "--timeout=3m")
	reader := "readonly-" + suffix
	n.apply(ctx, filesystemNetworkBlockPodManifest(n, reader, claim, true, 1001, 2002, group))
	n.Must(ctx, "-n", n.Namespace, "wait", "--for=condition=Ready", "pod/"+reader, "--timeout=3m")
	Expect(n.Must(ctx, "-n", n.Namespace, "exec", reader, "--", "cat", "/data/nested/deeper/existing")).To(Equal("root-owned-before-groupappend"))
	_, err := n.Kubectl(ctx, "", "-n", n.Namespace, "exec", reader, "--", "sh", "-ceu", "printf forbidden >> /data/nested/deeper/existing")
	Expect(err).To(HaveOccurred())
	Expect(strings.ToLower(err.Error())).To(ContainSubstring("read-only"))
	n.Must(ctx, "-n", n.Namespace, "delete", "pod", reader, "--wait=true", "--timeout=3m")
	if policy == "None" {
		mismatch := "mismatch-" + suffix
		n.apply(ctx, filesystemNetworkBlockPodManifest(n, mismatch, claim, false, 1002, 5555, 5555))
		n.Must(ctx, "-n", n.Namespace, "wait", "--for=condition=Ready", "pod/"+mismatch, "--timeout=3m")
		_, err := n.Kubectl(ctx, "", "-n", n.Namespace, "exec", mismatch, "--", "cat", "/data/nested/deeper/existing")
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("permission denied"))
		n.Must(ctx, "-n", n.Namespace, "delete", "pod", mismatch, "--wait=true", "--timeout=3m")
	}
	n.Must(ctx, "-n", n.Namespace, "delete", "pvc", claim, "--wait=true", "--timeout=3m")
	Eventually(func() (string, error) { return n.Kubectl(ctx, "", "get", "pv", pv, "--ignore-not-found", "-o", "name") }, 90*time.Second, 2*time.Second).Should(BeEmpty())
	workloadDone = true
}

func filesystemNetworkBlockPodManifest(n *filesystemNetworkFixture, name, claim string, readonly bool, uid, gid, group int64) string {
	var pod map[string]any
	Expect(yaml.Unmarshal([]byte(n.PodManifest(name, claim, n.Clients[0], readonly, uid, gid)), &pod)).To(Succeed())
	spec := pod["spec"].(map[string]any)
	spec["securityContext"] = map[string]any{"runAsUser": uid, "runAsGroup": gid, "fsGroup": group}
	data, err := json.Marshal(pod)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func filesystemNetworkLegacyNoneHelmControl(ctx context.Context, n *filesystemNetworkFixture) {
	Expect(suiteHelmBootstrap).NotTo(BeNil())
	release, namespace, chart := suiteHelmBootstrap.Release, suiteHelmBootstrap.Namespace, suiteHelmBootstrap.ChartPath
	history, stderr, err := e27HelmOutput(ctx, "history", release, "--namespace", namespace, "--max", "1", "--output", "json")
	Expect(err).NotTo(HaveOccurred(), stderr)
	var revisions []struct {
		Revision int `json:"revision"`
	}
	Expect(json.Unmarshal([]byte(history), &revisions)).To(Succeed())
	Expect(revisions).To(HaveLen(1))
	revision := fmt.Sprint(revisions[0].Revision)
	restored := false
	restore := func(restoreCtx context.Context) {
		_, stderr, err := e27HelmOutput(restoreCtx, "rollback", release, revision, "--namespace", namespace, "--wait", "--timeout", "5m")
		Expect(err).NotTo(HaveOccurred(), stderr)
		restored = true
	}
	DeferCleanup(func() {
		if !restored {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			restore(cleanupCtx)
		}
	})
	_, stderr, err = e27HelmOutput(ctx, "upgrade", release, chart, "--namespace", namespace, "--reuse-values", "--set", "csiDriver.fsGroupPolicy=None", "--wait", "--timeout", "5m")
	Expect(err).NotTo(HaveOccurred(), stderr)
	Expect(n.Must(ctx, "get", "csidriver", pillarv1.DefaultCSIDriver, "-o", "jsonpath={.spec.fsGroupPolicy}")).To(Equal("None"))
	Expect(n.Must(ctx, "get", "csidriver", pillarv1.FileCSIDriver, "-o", "jsonpath={.spec.fsGroupPolicy}")).To(Equal("None"))
	filesystemNetworkOldBlockControl(ctx, n, "none-owner", "ext4", "ReadWriteOnce", "None")
	n.verifyExchange(ctx)
	restore(ctx)
	Expect(n.Must(ctx, "get", "csidriver", pillarv1.DefaultCSIDriver, "-o", "jsonpath={.spec.fsGroupPolicy}")).To(Equal("File"))
	n.verifyExchange(ctx)
}
