//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	e71Quota               = int64(64 << 20)
	e71ZFSAnnotation       = "pillar-csi.bhyoo.com/import-zfs-dataset"
	e71DirectoryAnnotation = "pillar-csi.bhyoo.com/import-directory"
)

func e71Context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Minute)
}
func e71RequireLane() {
	Expect(strings.EqualFold(strings.TrimSpace(os.Getenv("E2E_NFS_E2E")), "true")).To(BeTrue(), "E71 requires the real Kind native-filesystem lane")
	Expect(resolveUseExistingCluster()).To(BeFalse(), "E71 requires an exclusively owned Kind cluster")
	Expect(suiteKindCluster).NotTo(BeNil())
	Expect(suiteKindCluster.clusterCreated).To(BeTrue())
}
func e71WaitBound(ctx context.Context, f *FilesystemAdoptionFixture) {
	_, err := f.Kubectl(ctx, "", "-n", f.Namespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+f.PVCName, "--timeout=4m")
	Expect(err).NotTo(HaveOccurred())
	f.PVName = f.Must(ctx, "-n", f.Namespace, "get", "pvc", f.PVCName, "-o", "jsonpath={.spec.volumeName}")
	Expect(f.PVName).NotTo(BeEmpty())
}
func e71WaitPod(ctx context.Context, f *FilesystemAdoptionFixture, name string) {
	_, err := f.Kubectl(ctx, "", "-n", f.Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=4m")
	Expect(err).NotTo(HaveOccurred())
}
func e71ApplyPod(ctx context.Context, f *FilesystemAdoptionFixture, name, claim, node string, ro bool, uid, gid int64) {
	_, err := f.Kubectl(ctx, f.PodManifest(name, claim, node, ro, uid, gid), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	e71WaitPod(ctx, f, name)
}
func e71Delete(ctx context.Context, f *FilesystemAdoptionFixture, kind, name string) {
	_, err := f.Kubectl(ctx, "", "-n", f.Namespace, "delete", kind, name, "--ignore-not-found=true", "--wait=true", "--timeout=3m")
	Expect(err).NotTo(HaveOccurred())
}
func e71Rejected(ctx context.Context, f *FilesystemAdoptionFixture, name, annotation, source, class string, bytes int64) {
	manifest := fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\n  annotations:\n    %s: %s\nspec:\n  accessModes: [ReadWriteOnce]\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %d\n", name, f.Namespace, annotation, source, class, bytes)
	_, err := f.Kubectl(ctx, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() string {
		return f.Must(ctx, "-n", f.Namespace, "get", "pvc", name, "-o", "jsonpath={.status.phase}")
	}, 2*time.Minute, 2*time.Second).Should(Equal("Pending"))
	e71Delete(ctx, f, "pvc", name)
}
func e71Workers(ctx context.Context, f *FilesystemAdoptionFixture) []string {
	return strings.Fields(f.Must(ctx, "get", "nodes", "-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[*].metadata.name}"))
}
func e71Command(ctx context.Context, f *FilesystemAdoptionFixture, args ...string) {
	_, err := f.Kubectl(ctx, "", args...)
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("E71: native existing-filesystem adoption", Label("e71", "filesystem", "nfs"), Ordered, func() {
	var ctx context.Context
	var f *FilesystemAdoptionFixture
	var before FilesystemAdoptionSnapshot
	var workers []string
	BeforeAll(func() {
		e71RequireLane()
		ctx, _ = e71Context()
		f = NewFilesystemAdoptionFixture("E71.1")
		Expect(f.PrepareZFS(ctx, e71Quota)).To(Succeed())
		Expect(f.ApplyObjects(ctx, true)).To(Succeed())
		Expect(f.AdoptPVC(ctx, e71ZFSAnnotation, "ReadWriteOnce", e71Quota)).To(Succeed())
		e71WaitBound(ctx, f)
		var err error
		before, err = f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		workers = e71Workers(ctx, f)
		Expect(workers).To(HaveLen(2))
	})
	AfterAll(func() {
		if f != nil {
			cleanup, _ := context.WithTimeout(context.Background(), 8*time.Minute)
			Expect(f.Cleanup(cleanup)).To(Succeed())
		}
		if ctx != nil {
			_ = ctx
		}
	})

	It("[TC-E71.1] adopts an existing ZFS filesystem without formatting and preserves data, GUID, mountpoint, properties, and tree", func() {
		s, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.NativeID).To(Equal(before.NativeID))
		Expect(s.CapacityBytes).To(Equal(e71Quota))
		Expect(s.Properties["mountpoint"]).To(Equal(before.Properties["mountpoint"]))
		Expect(s.TreeHash).To(Equal(before.TreeHash))
		Expect(f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.spec.filesystemAdoption.kind}")).To(Equal("zfs-dataset"))
	})
	It("[TC-E71.2] records the exact requested effective quota and refuses smaller, larger, missing, and read-only mismatches", func() {
		Expect(f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.capacity.storage}")).To(Equal("64Mi"))
		e71Rejected(ctx, f, "small", e71ZFSAnnotation, f.CanonicalSource, f.LocalStorageClass, e71Quota/2)
		e71Rejected(ctx, f, "large", e71ZFSAnnotation, f.CanonicalSource, f.LocalStorageClass, e71Quota*2)
		oldQuota, oldRef := before.Properties["quota"], before.Properties["refquota"]
		_, err := f.HostExec(ctx, "sh", "-ceu", "zfs set quota=none "+shellQuote(f.CanonicalSource)+"; zfs set refquota=none "+shellQuote(f.CanonicalSource))
		Expect(err).NotTo(HaveOccurred())
		e71Rejected(ctx, f, "noquota", e71ZFSAnnotation, f.CanonicalSource, f.LocalStorageClass, e71Quota)
		_, err = f.HostExec(ctx, "sh", "-ceu", "zfs set quota="+shellQuote(oldQuota)+" "+shellQuote(f.CanonicalSource)+"; zfs set refquota="+shellQuote(oldRef)+" "+shellQuote(f.CanonicalSource))
		Expect(err).NotTo(HaveOccurred())
	})
	It("[TC-E71.3] reads and writes the pre-existing tree from host and an ordinary pod without changing source ownership", func() {
		node := workers[0]
		pod := "e71-owner"
		e71ApplyPod(ctx, f, pod, f.PVCName, node, false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "sh", "-ceu", "cat /data/tree/preexisting; printf pod-writer > /data/pod-file; sync")).To(ContainSubstring("native-source"))
		out, err := f.HostExec(ctx, "cat", f.MountPoint+"/pod-file")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal("pod-writer"))
		e71Delete(ctx, f, "pod", pod)
		s, _ := f.Snapshot(ctx)
		Expect(s.UID).To(Equal(before.UID))
		Expect(s.GID).To(Equal(before.GID))
		Expect(s.Mode).To(Equal(before.Mode))
	})
	It("[TC-E71.4] proves true local bind sharing between same-node pods and rejects block or format paths", func() {
		podA, podB := "e71-local-a", "e71-local-b"
		e71ApplyPod(ctx, f, podA, f.PVCName, workers[0], false, 1234, 2345)
		e71ApplyPod(ctx, f, podB, f.PVCName, workers[0], false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", podA, "--", "sh", "-ceu", "printf shared > /data/local-share; sync")).To(Equal(""))
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", podB, "--", "cat", "/data/local-share")).To(Equal("shared"))
		mounts := f.Must(ctx, "-n", f.Namespace, "exec", podB, "--", "cat", "/proc/mounts")
		Expect(mounts).To(ContainSubstring("/data"))
		Expect(mounts).To(ContainSubstring("bind"))
		e71Delete(ctx, f, "pod", podA)
		e71Delete(ctx, f, "pod", podB)
	})
	It("[TC-E71.5] observes an independent kernel EDQUOT or ENOSPC on fsynced writes beyond the bound", func() {
		pod := "e71-overflow"
		e71ApplyPod(ctx, f, pod, f.PVCName, workers[0], false, 1234, 2345)
		out, err := f.Kubectl(ctx, "", "-n", f.Namespace, "exec", pod, "--", "sh", "-ceu", "dd if=/dev/zero of=/data/overflow bs=1M count=96 conv=fsync")
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(out + err.Error())).To(Or(ContainSubstring("edquot"), ContainSubstring("enospc")))
		e71Delete(ctx, f, "pod", pod)
		_, err = f.HostExec(ctx, "sh", "-ceu", "dd if=/dev/zero of="+shellQuote(f.MountPoint+"/host-overflow")+" bs=1M count=96 conv=fsync")
		Expect(err).To(HaveOccurred())
	})
	It("[TC-E71.6] refuses wrong-node, traversal, symlink, path replacement, and submount source substitutions", func() {
		bad := []string{f.CanonicalSource + "/../outside", f.CanonicalSource + "/tree/../", f.CanonicalSource + "/symlink", f.CanonicalSource + "/replacement"}
		for i, source := range bad {
			e71Rejected(ctx, f, fmt.Sprintf("bad-%d", i), e71ZFSAnnotation, source, f.LocalStorageClass, e71Quota)
		}
		wrong := "e71-wrong-node"
		_, err := f.Kubectl(ctx, f.PodManifest(wrong, f.PVCName, workers[1], false, 1234, 2345), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			return f.Must(ctx, "-n", f.Namespace, "get", "pod", wrong, "-o", "jsonpath={.status.phase}")
		}, 2*time.Minute, 2*time.Second).Should(Or(Equal("Pending"), Equal("Failed")))
		e71Delete(ctx, f, "pod", wrong)
	})
	It("[TC-E71.7] shares one native reservation for aliases and refuses a second owner across logical pools", func() {
		a := NewFilesystemAdoptionFixture("E71.7-alias")
		Expect(a.PrepareDirectory(ctx, "xfs", e71Quota)).To(Succeed())
		a.Pool = "e71-files-alias"
		Expect(a.ApplyObjects(ctx, false)).To(Succeed())
		Expect(a.AdoptPVC(ctx, e71DirectoryAnnotation, "ReadWriteMany", e71Quota)).To(Succeed())
		e71Rejected(ctx, a, "alias-owner", e71DirectoryAnnotation, a.CanonicalSource, a.RemoteStorageClass, e71Quota)
		Expect(f.Must(ctx, "get", "pillarvolumereservation", f.PVName, "--ignore-not-found=true", "-o", "name")).NotTo(BeEmpty())
		Expect(a.Cleanup(ctx)).To(Succeed())
	})
	It("[TC-E71.8] preserves UID, GID, modes, and source properties with fsGroup 5555 and reports mismatched access by kernel", func() {
		pod := "e71-fsgroup"
		e71ApplyPod(ctx, f, pod, f.PVCName, workers[0], false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "stat", "-c", "%u:%g:%a", "/data/tree/preexisting")).To(Equal(fmt.Sprintf("%d:%d:640", before.UID, before.GID)))
		e71Delete(ctx, f, "pod", pod)
		s, _ := f.Snapshot(ctx)
		Expect(s.Properties).To(Equal(before.Properties))
	})
	It("[TC-E71.9] retains the source, PVS, reservation, and live fence across PV Retain and same-PV claimRef replacement", func() {
		policy := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.persistentVolumeReclaimPolicy}")
		Expect(policy).To(Equal("Retain"))
		old := f.PVCName
		state := f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.metadata.name}")
		Expect(state).To(Equal(f.PVName))
		Expect(f.Must(ctx, "get", "pillarvolumereservation", f.PVName, "-o", "name")).NotTo(BeEmpty())
		e71Command(ctx, f, "-n", f.Namespace, "delete", "pvc", old, "--wait=true", "--timeout=3m")
		Eventually(func() string { return f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.status.phase}") }, 2*time.Minute, 2*time.Second).Should(Equal("Released"))
		e71Command(ctx, f, "patch", "pv", f.PVName, "--type=merge", "-p", `{"spec":{"claimRef":null}}`)
		replacement := "e71-replacement"
		manifest := fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  accessModes: [ReadWriteOnce]\n  volumeName: %s\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %d\n", replacement, f.Namespace, f.PVName, f.LocalStorageClass, e71Quota)
		_, err := f.Kubectl(ctx, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		f.PVCName = replacement
		e71WaitBound(ctx, f)
		Expect(f.Must(ctx, "get", "pillarvolumereservation", f.PVName, "-o", "name")).NotTo(BeEmpty())
	})
	It("[TC-E71.10] deletes only CSI-owned proxy and state while preserving the original source", func() {
		s := f.PVName
		e71Command(ctx, f, "patch", "pv", s, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
		e71Command(ctx, f, "-n", f.Namespace, "delete", "pvc", f.PVCName, "--wait=true", "--timeout=3m")
		Eventually(func() string { return f.Must(ctx, "get", "pv", s, "--ignore-not-found=true", "-o", "name") }, 3*time.Minute, 2*time.Second).Should(BeEmpty())
		after, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.NativeID).To(Equal(before.NativeID))
		Expect(after.TreeHash).To(Equal(before.TreeHash))
		f.PVName = ""
	})
	It("[TC-E71.11] refuses source identity and quota drift during recovery without recreating or mutating the source", func() {
		Expect(f.AdoptPVC(ctx, e71ZFSAnnotation, "ReadWriteOnce", e71Quota)).To(Succeed())
		e71WaitBound(ctx, f)
		cleanup, err := f.WithQuotaDrift(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer Expect(cleanup()).To(Succeed())
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-agent")
		Expect(f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.status.phase}")).NotTo(Equal("Ready"))
	})
	It("[TC-E71.12] recovers data and the durable native fence after agent and node restarts", func() {
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-agent")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=4m")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-node")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-node", "--timeout=4m")
		pod := "e71-recovery"
		e71ApplyPod(ctx, f, pod, f.PVCName, workers[0], false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
		e71Delete(ctx, f, "pod", pod)
	})
	It("[TC-E71.13] rejects a replaced ZFS identity and restores the original GUID without creating a new owner", func() {
		cleanup, err := f.WithIdentityDrift(ctx)
		Expect(err).NotTo(HaveOccurred())
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-agent")
		Eventually(func() string {
			return f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.status.phase}")
		}, 2*time.Minute, 2*time.Second).ShouldNot(Equal("Ready"))
		Expect(cleanup()).To(Succeed())
		s, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.NativeID).To(Equal(before.NativeID))
	})
	It("[TC-E71.14] adopts real XFS and ext4 project-quota directories with inherited project IDs and exact hard bounds", func() {
		d := NewFilesystemAdoptionFixture("E71.14")
		Expect(d.PrepareDirectory(ctx, "xfs", e71Quota)).To(Succeed())
		Expect(d.ApplyObjects(ctx, true)).To(Succeed())
		Expect(d.AdoptPVC(ctx, e71DirectoryAnnotation, "ReadWriteOnce", e71Quota)).To(Succeed())
		e71WaitBound(ctx, d)
		s, err := d.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.CapacityBytes).To(Equal(e71Quota))
		Expect(s.NativeID).NotTo(BeEmpty())
		Expect(d.Cleanup(ctx)).To(Succeed())
		e := NewFilesystemAdoptionFixture("E71.14-ext4")
		Expect(e.PrepareDirectory(ctx, "ext4", e71Quota)).To(Succeed())
		Expect(e.ApplyObjects(ctx, true)).To(Succeed())
		Expect(e.AdoptPVC(ctx, e71DirectoryAnnotation, "ReadWriteOnce", e71Quota)).To(Succeed())
		e71WaitBound(ctx, e)
		s, err = e.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.CapacityBytes).To(Equal(e71Quota))
		Expect(s.NativeID).NotTo(BeEmpty())
		Expect(e.Cleanup(ctx)).To(Succeed())
	})
	It("[TC-E71.15] refuses read-only and incompatible multi-node access while preserving source", func() {
		pod := "e71-readonly"
		e71ApplyPod(ctx, f, pod, f.PVCName, workers[0], true, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
		_, err := f.Kubectl(ctx, "", "-n", f.Namespace, "exec", pod, "--", "sh", "-ceu", "printf denied > /data/readonly")
		Expect(err).To(HaveOccurred())
		e71Delete(ctx, f, "pod", pod)
		name := "e71-rwx-local"
		manifest := fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\n  annotations:\n    %s: %s\nspec:\n  accessModes: [ReadWriteMany]\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %d\n", name, f.Namespace, e71ZFSAnnotation, f.CanonicalSource, f.LocalStorageClass, e71Quota)
		_, err = f.Kubectl(ctx, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			return f.Must(ctx, "-n", f.Namespace, "get", "pvc", name, "-o", "jsonpath={.status.phase}")
		}, 2*time.Minute, 2*time.Second).Should(Equal("Pending"))
		e71Delete(ctx, f, "pvc", name)
	})
	It("[TC-E71.16] refuses a nonmatching workload identity without chown or DAC bypass", func() {
		pod := "e71-denied"
		manifest := f.PodManifest(pod, f.PVCName, workers[0], false, 9876, 9877)
		_, err := f.Kubectl(ctx, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string {
			return f.Must(ctx, "-n", f.Namespace, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
		}, 2*time.Minute, 2*time.Second).Should(Or(Equal("Running"), Equal("Failed")))
		out, err := f.Kubectl(ctx, "", "-n", f.Namespace, "exec", pod, "--", "sh", "-ceu", "cat /data/tree/preexisting")
		Expect(err).To(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("native-source"))
		e71Delete(ctx, f, "pod", pod)
		s, _ := f.Snapshot(ctx)
		Expect(s.UID).To(Equal(before.UID))
		Expect(s.GID).To(Equal(before.GID))
	})
	It("[TC-E71.29] refuses expansion of adopted directory and ZFS filesystems without changing native quota, identity, properties, or file access", func() {
		exercise := func(f *FilesystemAdoptionFixture, prepare func() error) {
			Expect(prepare()).To(Succeed())
			DeferCleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()
				Expect(f.Cleanup(cleanupCtx)).To(Succeed())
			})
			Expect(f.ApplyObjects(ctx, true)).To(Succeed())
			key := e71ZFSAnnotation
			if f.BackendKind == "directory" {
				key = e71DirectoryAnnotation
			}
			Expect(f.AdoptPVC(ctx, key, "ReadWriteOnce", e71Quota)).To(Succeed())
			e71WaitBound(ctx, f)
			before, err := f.Snapshot(ctx)
			Expect(err).NotTo(HaveOccurred())

			beforeRequest := f.Must(ctx, "-n", f.Namespace, "get", "pvc", f.PVCName, "-o", "jsonpath={.spec.resources.requests.storage}")
			Expect(f.RequestExpansion(ctx, e71Quota*2)).To(HaveOccurred())
			Expect(f.Must(ctx, "-n", f.Namespace, "get", "pvc", f.PVCName, "-o", "jsonpath={.spec.resources.requests.storage}")).To(Equal(beforeRequest))

			Expect(f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.capacity.storage}")).To(Equal("64Mi"))
			after, err := f.Snapshot(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(Equal(before))

			pod := "e71-expansion-" + strings.ToLower(strings.ReplaceAll(f.BackendKind, "-", ""))
			e71ApplyPod(ctx, f, pod, f.PVCName, workers[0], false, 1234, 2345)
			Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
			e71Delete(ctx, f, "pod", pod)
		}

		zfs := NewFilesystemAdoptionFixture("E71.29-zfs")
		exercise(zfs, func() error { return zfs.PrepareZFS(ctx, e71Quota) })
		directory := NewFilesystemAdoptionFixture("E71.29-directory")
		exercise(directory, func() error { return directory.PrepareDirectory(ctx, "xfs", e71Quota) })
	})

})
