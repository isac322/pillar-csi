//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	e71Quota               = int64(64 << 20)
	e71DriftQuota          = "33554432"
	e71ZFSAnnotation       = "pillar-csi.bhyoo.com/import-zfs-dataset"
	e71DirectoryAnnotation = "pillar-csi.bhyoo.com/import-directory"
)

// e71OverflowProbe writes 96 MiB of incompressible data plus fsync to a new
// file, prints the symbolic kernel errno that stopped it (or "no-error"), and
// always removes only the file it created.
const e71OverflowProbe = `import errno, os, sys
path = sys.argv[1]
fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
result = "no-error"
try:
    block = os.urandom(1 << 20)
    for _ in range(96):
        view = memoryview(block)
        while view:
            view = view[os.write(fd, view):]
    os.fsync(fd)
except OSError as e:
    result = errno.errorcode.get(e.errno, str(e.errno))
finally:
    try:
        os.close(fd)
    except OSError as e:
        if result == "no-error":
            result = errno.errorcode.get(e.errno, str(e.errno))
    os.unlink(path)
print(result)
`

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
	waitOutput, err := f.Kubectl(ctx, "", "-n", f.Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=4m")
	if err == nil {
		return
	}

	diagnosticCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	const (
		maxDiagnosticOutputBytes  = 32 * 1024
		maxDiagnosticContextBytes = 96 * 1024
	)
	var diagnostics strings.Builder
	for _, args := range [][]string{
		{"get", "pod", name, "-o", "yaml"},
		{"describe", "pod", name},
		{"get", "events", "--field-selector=involvedObject.kind=Pod,involvedObject.name=" + name, "-o", "wide"},
	} {
		output, diagnosticErr := f.Kubectl(diagnosticCtx, "", append([]string{"-n", f.Namespace}, args...)...)
		if len(output) > maxDiagnosticOutputBytes {
			output = "[earlier output truncated]\n" + output[len(output)-maxDiagnosticOutputBytes:]
		}
		fmt.Fprintf(&diagnostics, "\n\nkubectl -n %s %s:\n%s", f.Namespace, strings.Join(args, " "), output)
		if diagnosticErr != nil {
			fmt.Fprintf(&diagnostics, "\nDiagnostic command failed: %v", diagnosticErr)
		}
	}
	diagnosticText := diagnostics.String()
	if len(diagnosticText) > maxDiagnosticContextBytes {
		const truncationMarker = "[earlier diagnostics truncated]\n"
		diagnosticText = truncationMarker + diagnosticText[len(diagnosticText)-(maxDiagnosticContextBytes-len(truncationMarker)):]
	}
	Fail(fmt.Sprintf("Pod %s/%s did not become Ready: %v\nWait output:\n%s%s", f.Namespace, name, err, waitOutput, diagnosticText))
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

// e71MountEntry returns the source and filesystem type of the visible
// /proc/mounts entry for target.
func e71MountEntry(mounts, target string) (source, fsType string) {
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == target {
			source, fsType = fields[0], fields[2]
		}
	}
	return source, fsType
}

// e71ApplyQuotaWriter starts a Python workload (fsync and errno names from the
// standard library) with the adopted PVC at /data and, independently of CSI,
// the original native mountpoint as a raw hostPath at /source.
func e71ApplyQuotaWriter(ctx context.Context, f *FilesystemAdoptionFixture, name string) {
	var pod map[string]any
	Expect(json.Unmarshal([]byte(f.PodManifest(name, f.PVCName, f.StorageNode, false, 1234, 2345)), &pod)).To(Succeed())
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	container["image"] = "python:3.12-alpine"
	container["command"] = []string{"python3", "-c", "import time; time.sleep(3600)"}
	container["volumeMounts"] = append(container["volumeMounts"].([]any), map[string]any{"name": "source", "mountPath": "/source"})
	spec["volumes"] = append(spec["volumes"].([]any), map[string]any{"name": "source", "hostPath": map[string]any{"path": f.MountPoint, "type": "Directory"}})
	data, err := json.Marshal(pod)
	Expect(err).NotTo(HaveOccurred())
	_, err = f.Kubectl(ctx, string(data), "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
	e71WaitPod(ctx, f, name)
}

// e71OverflowErrno runs e71OverflowProbe against path inside pod.
func e71OverflowErrno(ctx context.Context, f *FilesystemAdoptionFixture, pod, path string) string {
	out, err := f.Kubectl(ctx, "", "-n", f.Namespace, "exec", pod, "--", "python3", "-c", e71OverflowProbe, path)
	Expect(err).NotTo(HaveOccurred(), out)
	return out
}

// e71Attachment reads one field of the VolumeAttachment for the fixture PV.
func e71Attachment(ctx context.Context, f *FilesystemAdoptionFixture, field string) string {
	return f.Must(ctx, "get", "volumeattachment", "-o", `jsonpath={.items[?(@.spec.source.persistentVolumeName=="`+f.PVName+`")]`+field+`}`)
}

var _ = Describe("E71: native existing-filesystem adoption", Label("e71", "filesystem", "nfs"), Ordered, func() {
	var ctx context.Context
	var f *FilesystemAdoptionFixture
	var before FilesystemAdoptionSnapshot
	var workers []string
	BeforeAll(func() {
		e71RequireLane()
		var cancel context.CancelFunc
		ctx, cancel = e71Context()
		DeferCleanup(cancel)
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
			cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			Expect(f.Cleanup(cleanup)).To(Succeed())
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
		node := f.StorageNode
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
		e71ApplyPod(ctx, f, podA, f.PVCName, f.StorageNode, false, 1234, 2345)
		e71ApplyPod(ctx, f, podB, f.PVCName, f.StorageNode, false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", podA, "--", "sh", "-ceu", "printf shared > /data/local-share; sync")).To(Equal(""))
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", podB, "--", "cat", "/data/local-share")).To(Equal("shared"))
		mounts := f.Must(ctx, "-n", f.Namespace, "exec", podB, "--", "cat", "/proc/mounts")
		source, fsType := e71MountEntry(mounts, "/data")
		Expect(source).To(Equal(f.CanonicalSource), "pod /data must be the adopted dataset itself, not a block device or NFS export")
		Expect(fsType).To(Equal("zfs"), "pod /data must be the native filesystem, not a formatted or NFS mount")
		podIdentity := f.Must(ctx, "-n", f.Namespace, "exec", podB, "--", "stat", "-c", "%d:%i", "/data/tree/preexisting")
		hostIdentity, err := f.HostExec(ctx, "stat", "-c", "%d:%i", f.MountPoint+"/tree/preexisting")
		Expect(err).NotTo(HaveOccurred())
		Expect(podIdentity).To(Equal(strings.TrimSpace(hostIdentity)), "pod and host must see the same native inode")
		e71Delete(ctx, f, "pod", podA)
		e71Delete(ctx, f, "pod", podB)
	})
	It("[TC-E71.5] observes an independent kernel EDQUOT or ENOSPC on fsynced writes beyond the bound", func() {
		pod := "e71-overflow"
		e71ApplyQuotaWriter(ctx, f, pod)
		Expect(e71OverflowErrno(ctx, f, pod, "/data/e71-overflow-probe")).To(BeElementOf("EDQUOT", "ENOSPC"))
		Expect(e71OverflowErrno(ctx, f, pod, "/source/e71-host-overflow-probe")).To(BeElementOf("EDQUOT", "ENOSPC"))
		e71Delete(ctx, f, "pod", pod)
		_, err := f.HostExec(ctx, "sh", "-ceu", "test ! -e "+shellQuote(f.MountPoint+"/e71-overflow-probe")+" && test ! -e "+shellQuote(f.MountPoint+"/e71-host-overflow-probe"))
		Expect(err).NotTo(HaveOccurred())
	})
	It("[TC-E71.6] refuses wrong-node, traversal, symlink, path replacement, and submount source substitutions", func() {
		bad := []string{f.CanonicalSource + "/../outside", f.CanonicalSource + "/tree/../", f.CanonicalSource + "/symlink", f.CanonicalSource + "/replacement"}
		for i, source := range bad {
			e71Rejected(ctx, f, fmt.Sprintf("bad-%d", i), e71ZFSAnnotation, source, f.LocalStorageClass, e71Quota)
		}
		wrongNode := ""
		for _, worker := range workers {
			if worker != f.StorageNode {
				wrongNode = worker
				break
			}
		}
		Expect(wrongNode).NotTo(BeEmpty())
		wrong := "e71-wrong-node"
		_, err := f.Kubectl(ctx, f.PodManifest(wrong, f.PVCName, wrongNode, false, 1234, 2345), "apply", "-f", "-")
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
		e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, false, 1234, 2345)
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
		current, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		e71Command(ctx, f, "patch", "pv", s, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
		e71Command(ctx, f, "-n", f.Namespace, "delete", "pvc", f.PVCName, "--wait=true", "--timeout=3m")
		Eventually(func() string { return f.Must(ctx, "get", "pv", s, "--ignore-not-found=true", "-o", "name") }, 3*time.Minute, 2*time.Second).Should(BeEmpty())
		after, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.NativeID).To(Equal(before.NativeID))
		Expect(after.Properties["mountpoint"]).To(Equal(before.Properties["mountpoint"]))
		Expect(after.Properties).To(Equal(current.Properties))
		Expect(after.UID).To(Equal(before.UID))
		Expect(after.GID).To(Equal(before.GID))
		Expect(after.Mode).To(Equal(before.Mode))
		Expect(after.TreeHash).To(Equal(current.TreeHash))
		f.PVName = ""
	})
	It("[TC-E71.11] refuses source identity and quota drift during recovery without recreating or mutating the source", func() {
		Expect(f.AdoptPVC(ctx, e71ZFSAnnotation, "ReadWriteOnce", e71Quota)).To(Succeed())
		e71WaitBound(ctx, f)
		// Healthy control: the identical consumer attaches and reads the source
		// before drift, so a later refusal is caused by the drift alone.
		pod := "e71-drift-consumer"
		e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
		e71Delete(ctx, f, "pod", pod)
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty())
		stable, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		cleanup, err := f.WithQuotaDrift(ctx)
		Expect(err).NotTo(HaveOccurred())
		restored := false
		defer func() {
			if !restored {
				Expect(cleanup()).To(Succeed())
			}
		}()
		helm := resolveHelmNamespace()
		e71Command(ctx, f, "-n", helm, "rollout", "restart", "daemonset/pillar-csi-agent")
		e71Command(ctx, f, "-n", helm, "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=4m")
		// The recovered agent re-pins the source on local attach. The ZFS
		// importer refuses the drifted effective quota as an import
		// precondition (existing vs required bytes), which importVolumeError
		// surfaces as gRPC FailedPrecondition and the controller propagates
		// with its code intact.
		_, err = f.Kubectl(ctx, f.PodManifest(pod, f.PVCName, f.StorageNode, false, 1234, 2345), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string { return e71Attachment(ctx, f, ".status.attachError.message") }, 2*time.Minute, 2*time.Second).Should(And(
			ContainSubstring("code = FailedPrecondition"),
			ContainSubstring(e71DriftQuota),
			ContainSubstring(fmt.Sprint(e71Quota)),
		))
		Expect(e71Attachment(ctx, f, ".status.attached")).NotTo(Equal("true"))
		Expect(f.Must(ctx, "-n", f.Namespace, "get", "pod", pod, "-o", "jsonpath={.status.phase}")).To(Equal("Pending"))
		drifted, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(drifted.Properties["quota"]).To(Equal(e71DriftQuota))
		Expect(drifted.Properties["refquota"]).To(Equal(e71DriftQuota))
		Expect(drifted.NativeID).To(Equal(stable.NativeID))
		Expect(drifted.TreeHash).To(Equal(stable.TreeHash))
		e71Delete(ctx, f, "pod", pod)
		Expect(cleanup()).To(Succeed())
		restored = true
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty())
		after, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.NativeID).To(Equal(stable.NativeID))
		Expect(after.Properties).To(Equal(stable.Properties))
		Expect(after.TreeHash).To(Equal(stable.TreeHash))
	})
	It("[TC-E71.12] recovers data and the durable native fence after agent and node restarts", func() {
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-agent")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=4m")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "restart", "daemonset/pillar-csi-node")
		e71Command(ctx, f, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-node", "--timeout=4m")
		pod := "e71-recovery"
		e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, false, 1234, 2345)
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
		e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, true, 1234, 2345)
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
		manifest := f.PodManifest(pod, f.PVCName, f.StorageNode, false, 9876, 9877)
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
			e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, false, 1234, 2345)
			Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
			e71Delete(ctx, f, "pod", pod)
		}

		zfs := NewFilesystemAdoptionFixture("E71.29-zfs")
		exercise(zfs, func() error { return zfs.PrepareZFS(ctx, e71Quota) })
		directory := NewFilesystemAdoptionFixture("E71.29-directory")
		exercise(directory, func() error { return directory.PrepareDirectory(ctx, "xfs", e71Quota) })
	})

})
