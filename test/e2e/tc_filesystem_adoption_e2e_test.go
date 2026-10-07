//go:build e2e

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
	agentbackend "github.com/isac322/pillar-csi/internal/agent/backend"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// e71Reservations lists every PillarVolumeReservation in the cluster.
func e71Reservations(ctx context.Context, f *FilesystemAdoptionFixture) []pillarv1.PillarVolumeReservation {
	var list pillarv1.PillarVolumeReservationList
	Expect(json.Unmarshal([]byte(f.Must(ctx, "get", "pillarvolumereservation", "-o", "json")), &list)).To(Succeed())
	return list.Items
}

// e71Reservation returns the one reservation held by the adopted lifecycle
// f.PVName. The owner predicate comes from the PillarVolumeState the
// controller recorded (owner volume, agent, backend routing identity, claim),
// its native key is the driver's exported FilesystemFenceID of that recorded
// adoption descriptor, and no other reservation on the same agent may hold
// that key, whatever logical pool alias asked.
func e71Reservation(ctx context.Context, f *FilesystemAdoptionFixture) pillarv1.PillarVolumeReservation {
	var state pillarv1.PillarVolumeState
	Expect(json.Unmarshal([]byte(f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "json")), &state)).To(Succeed())
	Expect(state.Spec.FilesystemAdoption).NotTo(BeNil())
	Expect(state.Spec.FilesystemAdoption.CanonicalSource).To(Equal(f.CanonicalSource))
	Expect(state.Spec.ClaimRef).NotTo(BeNil())
	items := e71Reservations(ctx, f)
	var owned []pillarv1.PillarVolumeReservation
	for _, r := range items {
		if r.Spec.OwnerVolume == f.PVName {
			owned = append(owned, r)
		}
	}
	Expect(owned).To(HaveLen(1), "PillarVolumeState %s must hold exactly one reservation", f.PVName)
	r := owned[0]
	Expect(r.Spec.AgentRef).To(Equal(state.Spec.AgentRef))
	Expect(r.Spec.BackendType).To(Equal(state.Spec.BackendType))
	Expect(r.Spec.AgentVolumeID).To(Equal(state.Spec.AgentVolumeID))
	a := state.Spec.FilesystemAdoption
	var inode uint64
	if a.Inode != "" {
		var err error
		inode, err = pillarv1.ParseFilesystemAdoptionInode(a.Inode)
		Expect(err).NotTo(HaveOccurred())
	}
	native := agentbackend.FilesystemFenceID(&agentv1.FilesystemAdoption{
		Kind: string(a.Kind), CanonicalSource: a.CanonicalSource, ResourceId: a.ResourceID,
		HostPath: a.HostPath, FilesystemType: a.FilesystemType, FilesystemId: a.FilesystemID,
		Inode: inode, ProjectId: a.ProjectID,
	})
	Expect(native).NotTo(BeEmpty(), "recorded adoption descriptor must have a native resource key")
	Expect(r.Spec.FilesystemResourceID).To(Equal(native))
	Expect(r.Spec.ClaimRef).To(Equal(state.Spec.ClaimRef))
	var holders []string
	for _, other := range items {
		if other.Spec.AgentRef == r.Spec.AgentRef && other.Spec.FilesystemResourceID == r.Spec.FilesystemResourceID {
			holders = append(holders, other.Name)
		}
	}
	Expect(holders).To(Equal([]string{r.Name}), "native resource %s must have exactly one reservation", r.Spec.FilesystemResourceID)
	return r
}

// e71VolumeState reads the adopted PillarVolumeState and its ExportReconciled
// condition (nil when not yet recorded). The controller reports agent-side
// export restore and resync outcomes on that condition; phase is the
// creation marker and stays Ready.
func e71VolumeState(ctx context.Context, f *FilesystemAdoptionFixture) (pillarv1.PillarVolumeState, *metav1.Condition) {
	var state pillarv1.PillarVolumeState
	Expect(json.Unmarshal([]byte(f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "json")), &state)).To(Succeed())
	for i := range state.Status.Conditions {
		if state.Status.Conditions[i].Type == "ExportReconciled" {
			return state, &state.Status.Conditions[i]
		}
	}
	return state, nil
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
		DeferCleanup(func() {
			if f != nil {
				cleanup, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()
				err := f.Cleanup(cleanup)
				if err != nil {
					// Bounded teardown facts for the failing cleanup: the PVS,
					// VolumeAttachments, file-node publish records and mounts on the
					// storage node and workers, and agent/controller logs. Optional
					// PV metadata failures are recorded, never asserted.
					var handle, target string
					if f.PVName != "" {
						metadataCtx, metadataCancel := context.WithTimeout(context.Background(), time.Minute)
						for _, field := range []struct {
							Into *string
							Path string
						}{{&handle, "{.spec.csi.volumeHandle}"}, {&target, "{.spec.csi.volumeAttributes.target_id}"}} {
							value, readErr := f.Kubectl(metadataCtx, "", "get", "pv", f.PVName, "--ignore-not-found=true", "-o", "jsonpath="+field.Path)
							if readErr != nil {
								fmt.Fprintf(GinkgoWriter, "E71 diagnostic read of pv %s %s failed: %v\n", f.PVName, field.Path, readErr)
								continue
							}
							*field.Into = strings.TrimSpace(value)
						}
						metadataCancel()
					}
					fmt.Fprintf(GinkgoWriter, "E71 ordered fixture cleanup of %s failed: %v\n%s\n", f.PVName, err, f.teardownDiagnostics(handle, target, append(append([]string{}, workers...), f.StorageNode)...)())
				}
				Expect(err).To(Succeed())
			}
		})
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
		// Two agent logical pools, e71-files-alias and e71-files, share one
		// hostRoot, so both fixtures address the same native directory.
		a := NewFilesystemAdoptionFixture("E71.7-alias")
		Expect(a.PrepareDirectory(ctx, "xfs", e71Quota)).To(Succeed())
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			Expect(a.Cleanup(cleanupCtx)).To(Succeed())
		})
		a.Pool = "e71-files-alias"
		Expect(a.ApplyObjects(ctx, false)).To(Succeed())
		Expect(a.AdoptPVC(ctx, e71DirectoryAnnotation, "ReadWriteMany", e71Quota)).To(Succeed())
		e71WaitBound(ctx, a)
		held := e71Reservation(ctx, a)

		b := NewFilesystemAdoptionFixture("E71.7-second")
		Expect(b.PrepareDirectory(ctx, "xfs", e71Quota)).To(Succeed())
		DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			Expect(b.Cleanup(cleanupCtx)).To(Succeed())
		})
		Expect(b.CanonicalSource).To(Equal(a.CanonicalSource))
		Expect(b.Pool).NotTo(Equal(a.Pool))
		// Same agent as the owner, so only the logical pool differs.
		b.AgentName = a.AgentName
		Expect(b.ApplyObjects(ctx, false)).To(Succeed())
		Expect(b.AgentName).To(Equal(a.AgentName))
		Expect(b.AdoptPVC(ctx, e71DirectoryAnnotation, "ReadWriteMany", e71Quota)).To(Succeed())
		// The controller's refusal names the owning lifecycle, whether the
		// native ownership scan or the reservation itself stops the alias.
		Eventually(func() string {
			return b.Must(ctx, "-n", b.Namespace, "get", "events", "--field-selector", "involvedObject.kind=PersistentVolumeClaim,involvedObject.name="+b.PVCName+",reason=ProvisioningFailed", "-o", "jsonpath={.items[*].message}")
		}, 2*time.Minute, 2*time.Second).Should(ContainSubstring(a.PVName))
		Expect(b.Must(ctx, "-n", b.Namespace, "get", "pvc", b.PVCName, "-o", "jsonpath={.status.phase}/{.spec.volumeName}")).To(Equal("Pending/"))
		after := e71Reservation(ctx, a)
		Expect(after.UID).To(Equal(held.UID))
		Expect(after.Spec).To(Equal(held.Spec))
		for _, r := range e71Reservations(ctx, a) {
			if r.Spec.ClaimRef != nil {
				Expect(r.Spec.ClaimRef.Namespace).NotTo(Equal(b.Namespace), "second owner %s/%s must not hold a reservation", b.Namespace, b.PVCName)
			}
		}
	})
	It("[TC-E71.8] preserves UID, GID, modes, and source properties with fsGroup 5555 and reports mismatched access by kernel", func() {
		pod := "e71-fsgroup"
		e71ApplyPod(ctx, f, pod, f.PVCName, f.StorageNode, false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", pod, "--", "stat", "-c", "%u:%g:%a", "/data/tree/preexisting")).To(Equal(fmt.Sprintf("%d:%d:640", before.UID, before.GID)))
		// Witness the real publication before deleting its last consumer, so
		// the drain barrier below cannot pass vacuously on a wrong record path:
		// the PVS publication, the node's publish record serializing this pod's
		// target, the pod's native bind, the VolumeAttachment, kubelet
		// volumesInUse, and no staging mount.
		handle := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeHandle}")
		Expect(handle).NotTo(BeEmpty())
		target := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeAttributes.target_id}")
		podUID := f.Must(ctx, "-n", f.Namespace, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
		Expect(podUID).NotTo(BeEmpty())
		Expect(strings.Fields(f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.status.publishedNodes[*].nodeID}"))).To(ContainElement(f.StorageNode))
		f.witnessFilePublication(ctx, f.StorageNode, handle, podUID, "")
		e71Delete(ctx, f, "pod", pod)
		// Precondition of TC-E71.9 (Retain, claimRef replacement) and TC-E71.10
		// (unpublished Delete PV): the last consumer's publication is fully
		// drained. This barrier distinguishes an undrained publication from a
		// later teardown failure; it is not a runtime repair.
		nodes := append(append([]string{}, workers...), f.StorageNode)
		diagnostics := f.teardownDiagnostics(handle, target, nodes...)
		Eventually(func() string {
			return f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.status.publishedNodes[*].nodeID}")
		}, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		f.expectFilePublicationDrained(ctx, handle, []string{podUID}, nodes, 2*time.Minute, diagnostics)
		s, _ := f.Snapshot(ctx)
		Expect(s.Properties).To(Equal(before.Properties))
	})
	It("[TC-E71.9] retains the source, PVS, reservation, and live fence across PV Retain and same-PV claimRef replacement", func() {
		policy := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.persistentVolumeReclaimPolicy}")
		Expect(policy).To(Equal("Retain"))
		old := f.PVCName
		state := f.Must(ctx, "get", "pillarvolumestate", f.PVName, "-o", "jsonpath={.metadata.name}")
		Expect(state).To(Equal(f.PVName))
		held := e71Reservation(ctx, f)
		e71Command(ctx, f, "-n", f.Namespace, "delete", "pvc", old, "--wait=true", "--timeout=3m")
		Eventually(func() string { return f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.status.phase}") }, 2*time.Minute, 2*time.Second).Should(Equal("Released"))
		e71Command(ctx, f, "patch", "pv", f.PVName, "--type=merge", "-p", `{"spec":{"claimRef":null}}`)
		replacement := "e71-replacement"
		manifest := fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  accessModes: [ReadWriteOnce]\n  volumeName: %s\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %d\n", replacement, f.Namespace, f.PVName, f.LocalStorageClass, e71Quota)
		_, err := f.Kubectl(ctx, manifest, "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		f.PVCName = replacement
		e71WaitBound(ctx, f)
		after := e71Reservation(ctx, f)
		Expect(after.Name).To(Equal(held.Name))
		Expect(after.UID).To(Equal(held.UID))
		Expect(after.Spec).To(Equal(held.Spec))
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
		// Restore only once the refused consumer's VolumeAttachment is gone:
		// while it remains, the external-attacher keeps retrying the refused
		// publish, and an earlier restore would let a retried publish pin the
		// original source.
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty())
		Expect(cleanup()).To(Succeed())
		restored = true
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
		helm := resolveHelmNamespace()
		handle := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeHandle}")
		target := f.Must(ctx, "get", "pv", f.PVName, "-o", "jsonpath={.spec.csi.volumeAttributes.target_id}")
		// Bounded, lazily evaluated evidence of the guard's mechanism, captured
		// while drift is still in place when an assertion below fails: PVS
		// conditions/phase, VolumeAttachment status, reservations, and the
		// common teardown collector (agent/controller/node logs).
		diagnostics := func() string {
			diagCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var b strings.Builder
			for _, args := range [][]string{
				{"get", "pillarvolumestate", f.PVName, "--ignore-not-found", "-o", `jsonpath=phase={.status.phase} uid={.metadata.uid} generation={.metadata.generation} publishedNodes={.status.publishedNodes} conditions={.status.conditions}`},
				{"get", "volumeattachments", "-o", `jsonpath={range .items[?(@.spec.source.persistentVolumeName=="` + f.PVName + `")]}{.metadata.name} node={.spec.nodeName} status={.status}{"\n"}{end}`},
				{"get", "pillarvolumereservation", "-o", `jsonpath={range .items[*]}{.metadata.name} uid={.metadata.uid} spec={.spec}{"\n"}{end}`},
			} {
				out, err := f.Kubectl(diagCtx, "", args...)
				if len(out) > 16*1024 {
					out = "[earlier output truncated]\n" + out[len(out)-16*1024:]
				}
				fmt.Fprintf(&b, "\nkubectl %s:\n%s", strings.Join(args, " "), out)
				if err != nil {
					fmt.Fprintf(&b, "\nDiagnostic command failed: %v", err)
				}
			}
			return "E71.13 identity-drift guard state:" + b.String() + "\n" + f.teardownDiagnostics(handle, target, append(append([]string{}, workers...), f.StorageNode)...)()
		}
		// E71.12 deleted its consumer without waiting for detach; drain the
		// prior attachment and publication intent before capturing baselines
		// so neither an old intent nor an in-flight unpublish races the swap.
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		Eventually(func() []pillarv1.VolumePublication {
			s, _ := e71VolumeState(ctx, f)
			return s.Status.PublishedNodes
		}, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		// Healthy precondition: the export guard currently reports the
		// recorded identity reconciled, so a later False is caused by drift.
		var reconciledAt metav1.Time
		Eventually(func() string {
			_, cond := e71VolumeState(ctx, f)
			if cond == nil {
				return "absent"
			}
			reconciledAt = cond.LastTransitionTime
			return string(cond.Status) + "/" + cond.Reason
		}, 2*time.Minute, 2*time.Second).Should(Equal("True/Reconciled"), diagnostics)
		held := e71Reservation(ctx, f)
		stateBefore, _ := e71VolumeState(ctx, f)
		stable, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		cleanup, err := f.WithIdentityDrift(ctx)
		Expect(err).NotTo(HaveOccurred())
		restored := false
		defer func() {
			if !restored {
				Expect(cleanup()).To(Succeed())
			}
		}()
		e71Command(ctx, f, "-n", helm, "rollout", "restart", "daemonset/pillar-csi-agent")
		e71Command(ctx, f, "-n", helm, "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=4m")
		// The restarted agent's export restore re-verifies the recorded native
		// identity and refuses the replaced dataset; the controller records it
		// as a fresh ExportReconciled=False transition naming the volume and
		// the identity/GUID mismatch. Phase is the creation marker and stays.
		var refusedAt metav1.Time
		Eventually(func(g Gomega) {
			state, cond := e71VolumeState(ctx, f)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(string(cond.Status)).To(Equal("False"))
			g.Expect(cond.Reason).To(Equal("ReconcileFailed"))
			g.Expect(cond.LastTransitionTime.After(reconciledAt.Time)).To(BeTrue(), "condition must transition after the healthy observation")
			g.Expect(cond.Message).To(ContainSubstring(state.Spec.AgentVolumeID))
			g.Expect(cond.Message).To(Or(ContainSubstring("GUID"), ContainSubstring("identity")))
			refusedAt = cond.LastTransitionTime
		}, 2*time.Minute, 2*time.Second).Should(Succeed(), diagnostics)
		state, _ := e71VolumeState(ctx, f)
		Expect(string(state.Status.Phase)).To(Equal("Ready"))
		Expect(state.Status.PublishedNodes).To(BeEmpty(), diagnostics)
		// A new consumer is refused by the same guard at attach time.
		pod := "e71-identity-drift"
		_, err = f.Kubectl(ctx, f.PodManifest(pod, f.PVCName, f.StorageNode, false, 1234, 2345), "apply", "-f", "-")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() string { return e71Attachment(ctx, f, ".status.attachError.message") }, 2*time.Minute, 2*time.Second).Should(And(
			ContainSubstring("code = FailedPrecondition"),
			Or(ContainSubstring("GUID"), ContainSubstring("identity")),
		), diagnostics)
		Expect(e71Attachment(ctx, f, ".status.attached")).NotTo(Equal("true"), diagnostics)
		Expect(f.Must(ctx, "-n", f.Namespace, "get", "pod", pod, "-o", "jsonpath={.status.phase}")).To(Equal("Pending"), diagnostics)
		// No new owner or lifecycle was created by the refusal. A refused
		// local attach may keep its reserved publication intent until the
		// attachment is withdrawn, so usability is proven by attached!=true
		// and Pending above, and the intent must drain after deletion below.
		state, _ = e71VolumeState(ctx, f)
		for _, pub := range state.Status.PublishedNodes {
			Expect(pub.NodeID).To(Equal(f.StorageNode), "only the refused consumer's node may hold publication intent")
		}
		Expect(len(state.Status.PublishedNodes)).To(BeNumerically("<=", 1), diagnostics)
		Expect(state.UID).To(Equal(stateBefore.UID))
		Expect(state.Generation).To(Equal(stateBefore.Generation))
		Expect(state.Spec).To(Equal(stateBefore.Spec))
		after := e71Reservation(ctx, f)
		Expect(after.UID).To(Equal(held.UID))
		Expect(after.Spec).To(Equal(held.Spec))
		e71Delete(ctx, f, "pod", pod)
		// The refused consumer's attach retries until its VolumeAttachment is
		// withdrawn, and a refused local attach may keep its reserved
		// publication intent until then. Hold the drift until both are gone;
		// restoring earlier would let a retried publish pin the original
		// source.
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		Eventually(func() []pillarv1.VolumePublication {
			s, _ := e71VolumeState(ctx, f)
			return s.Status.PublishedNodes
		}, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		Expect(cleanup()).To(Succeed())
		restored = true
		s, err := f.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(s.NativeID).To(Equal(before.NativeID))
		Expect(s.NativeID).To(Equal(stable.NativeID))
		Expect(s.Properties).To(Equal(stable.Properties))
		Expect(s.TreeHash).To(Equal(stable.TreeHash))
		// The restored original identity reconciles again on the next agent
		// restore, and a healthy consumer reads the original marker.
		e71Command(ctx, f, "-n", helm, "rollout", "restart", "daemonset/pillar-csi-agent")
		e71Command(ctx, f, "-n", helm, "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=4m")
		Eventually(func(g Gomega) {
			_, cond := e71VolumeState(ctx, f)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(string(cond.Status) + "/" + cond.Reason).To(Equal("True/Reconciled"))
			g.Expect(cond.LastTransitionTime.After(refusedAt.Time)).To(BeTrue())
		}, 2*time.Minute, 2*time.Second).Should(Succeed(), diagnostics)
		healthy := "e71-identity-restored"
		e71ApplyPod(ctx, f, healthy, f.PVCName, f.StorageNode, false, 1234, 2345)
		Expect(f.Must(ctx, "-n", f.Namespace, "exec", healthy, "--", "cat", "/data/tree/preexisting")).To(Equal("native-source"))
		e71Delete(ctx, f, "pod", healthy)
		Eventually(func() string { return e71Attachment(ctx, f, ".metadata.name") }, 2*time.Minute, 2*time.Second).Should(BeEmpty(), diagnostics)
		Expect(e71Reservation(ctx, f).UID).To(Equal(held.UID))
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
