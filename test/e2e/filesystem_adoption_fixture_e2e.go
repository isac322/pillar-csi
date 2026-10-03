//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
)

const filesystemAdoptionBytes int64 = 64 << 20

// Native sources and Kubernetes resources are separately owned. Cleanup does
// not remove a source until the CSI lifecycle is gone and its contents have
// been observed independently through the storage node.
type FilesystemAdoptionFixture struct {
	TCID, Namespace, StorageNode, AgentName, StoreName, ProtocolName   string
	LocalPSCName, RemotePSCName, LocalStorageClass, RemoteStorageClass string
	PVCName, PVName, BackendKind, Pool, Parent, SourceRoot             string
	CanonicalSource, MountPoint, NativeID, SourceMode                  string
	CapacityBytes, RequestedBytes, SourceUID, SourceGID                int64
	ProjectID                                                          uint32
	LocalAttach, CreatedSource, CreatedAgent                           bool
	OriginalProperties                                                 map[string]string
	nativeHelper                                                       string
}

type FilesystemAdoptionSnapshot struct {
	CanonicalSource string
	NativeID        string
	CapacityBytes   int64
	UID, GID        int64
	Mode            string
	Properties      map[string]string
	TreeHash        string
}

func NewFilesystemAdoptionFixture(tc string) *FilesystemAdoptionFixture {
	token := strings.ToLower(strings.NewReplacer(".", "-", "/", "-", "_", "-").Replace(tc))
	token += "-" + strconv.Itoa(GinkgoParallelProcess())
	root := os.Getenv("PILLAR_E2E_FILESYSTEM_SOURCE_ROOT")
	if root == "" {
		root = "/var/lib/pillar-csi/e71-sources"
	}
	return &FilesystemAdoptionFixture{TCID: tc, Namespace: "e71-" + token, StorageNode: os.Getenv(suiteBackendContainerEnvVar), StoreName: "e71-store-" + token, ProtocolName: "e71-proto-" + token, LocalPSCName: "e71-local-" + token, RemotePSCName: "e71-remote-" + token, LocalStorageClass: "e71-local-" + token, RemoteStorageClass: "e71-remote-" + token, PVCName: "adopted", SourceRoot: root, SourceUID: 1234, SourceGID: 2345, SourceMode: "750"}
}

func (f *FilesystemAdoptionFixture) Kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	return nfsKubectl(ctx, stdin, args...)
}
func (f *FilesystemAdoptionFixture) Must(ctx context.Context, args ...string) string {
	out, err := f.Kubectl(ctx, "", args...)
	if err != nil {
		panic(err)
	}
	return out
}
func (f *FilesystemAdoptionFixture) HostExec(ctx context.Context, args ...string) (string, error) {
	return kindContainerExec(ctx, f.StorageNode, args...)
}
func (f *FilesystemAdoptionFixture) NodeExec(ctx context.Context, node string, args ...string) (string, error) {
	return kindContainerExec(ctx, node, args...)
}

// PrepareFilesystemAdoptionNativeSources is called by the existing Helm
// bootstrap before starting the agent. Its privileged scratch Pod uses only
// tools packaged in the existing node image; no host package is installed.
func PrepareFilesystemAdoptionNativeSources(ctx context.Context, storageNode, sourceRoot string) error {
	f := NewFilesystemAdoptionFixture("native-bootstrap")
	f.StorageNode = storageNode
	tag := envOrDefault(imageTagEnvVar, defaultE2EImageTag)
	object := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "e71-native-source-tools", "namespace": "kube-system"}, "spec": map[string]any{"nodeName": storageNode, "restartPolicy": "Never", "tolerations": []any{map[string]any{"operator": "Exists"}}, "containers": []any{map[string]any{"name": "tools", "image": "pillar-csi/node:" + tag, "imagePullPolicy": "Never", "securityContext": map[string]any{"privileged": true}, "command": []string{"/bin/busybox", "sh", "-ceu", "sleep 86400"}, "volumeMounts": []any{map[string]any{"name": "sources", "mountPath": sourceRoot, "mountPropagation": "Bidirectional"}, map[string]any{"name": "devices", "mountPath": "/dev"}}}}, "volumes": []any{map[string]any{"name": "sources", "hostPath": map[string]any{"path": sourceRoot, "type": "Directory"}}, map[string]any{"name": "devices", "hostPath": map[string]any{"path": "/dev", "type": "Directory"}}}}}
	b, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if _, err = f.Kubectl(ctx, string(b), "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err = f.Kubectl(ctx, "", "-n", "kube-system", "wait", "--for=condition=Ready", "pod/e71-native-source-tools", "--timeout=3m"); err != nil {
		return err
	}
	script := `set -eu
root="$1"
for fs in xfs ext4; do
 mount="$root/$fs"
 image="$root/.$fs.img"
 mkdir -p "$mount"
 test ! -e "$image"
 truncate -s 512M "$image"
 if [ "$fs" = xfs ]; then
  mkfs.xfs -f "$image"
  mount -o loop,prjquota "$image" "$mount"
 else
  mkfs.ext4 -F -O project,quota "$image"
  mount -o loop,prjquota "$image" "$mount"
 fi
 for ordinal in $(seq 1 32); do
  dir="$mount/e71-$ordinal"
  mkdir "$dir"
  if [ "$fs" = xfs ]; then
   xfs_quota -x -c "project -s -p $dir $ordinal" -c "limit -p bhard=64m $ordinal" "$mount"
  else
   chattr -p "$ordinal" +P "$dir"
   setquota -P "$ordinal" 0 65536 0 0 "$mount"
  fi
  mkdir "$dir/tree"
  printf native-source > "$dir/tree/preexisting"
  printf host-writer > "$dir/tree/host"
  chown -R 1234:2345 "$dir"
  chmod 750 "$dir" "$dir/tree"
  chmod 640 "$dir/tree/preexisting"
 done
 sync
 done`
	_, err = f.Kubectl(ctx, "", "-n", "kube-system", "exec", "e71-native-source-tools", "--", "/bin/busybox", "sh", "-ceu", script, "native-sources", sourceRoot)
	return err
}

// CleanupFilesystemAdoptionNativeSources detaches only the two images and
// e71-* directories created by PrepareFilesystemAdoptionNativeSources.
func CleanupFilesystemAdoptionNativeSources(ctx context.Context, storageNode, sourceRoot string) error {
	f := NewFilesystemAdoptionFixture("native-bootstrap-cleanup")
	if _, err := f.Kubectl(ctx, "", "-n", "kube-system", "get", "pod", "e71-native-source-tools", "-o", "name"); err != nil {
		return nil
	}
	f.StorageNode = storageNode
	for _, fsType := range []string{"xfs", "ext4"} {
		script := fmt.Sprintf(`set -eu
root=%s
mount="$root/%s"
image="$root/.%s.img"
if findmnt -rn -M "$mount" >/dev/null 2>&1; then
  source=$(findmnt -rn -M "$mount" -o SOURCE)
  case "$source" in /dev/loop*) backing=$(losetup "$source" -O BACK-FILE --noheadings); [ "$backing" = "$image" ] || { echo "unexpected loop backing $backing" >&2; exit 1; };; *) echo "unexpected mount source $source" >&2; exit 1;; esac
  umount "$mount"
fi
if [ -e "$image" ]; then
  loops=$(losetup -j "$image" -O NAME --noheadings | tr -d ' ')
  if [ -n "$loops" ]; then
    for loop in $loops; do
      backing=$(losetup "$loop" -O BACK-FILE --noheadings)
      [ "$backing" = "$image" ] || { echo "unexpected loop backing $backing" >&2; exit 1; }
      losetup -d "$loop"
    done
  fi
  rm -f -- "$image"
fi
if [ -d "$mount" ]; then
  for dir in "$mount"/e71-*; do [ -e "$dir" ] || continue; rm -rf -- "$dir"; done
fi`, shellQuote(sourceRoot), fsType, fsType)
		if _, err := f.nativeExec(ctx, "/bin/busybox", "sh", "-ceu", script); err != nil {
			return err
		}
	}
	_, err := f.Kubectl(ctx, "", "-n", "kube-system", "delete", "pod", "e71-native-source-tools", "--ignore-not-found=true", "--wait=true", "--timeout=2m")
	return err
}

func (f *FilesystemAdoptionFixture) nativeExec(ctx context.Context, args ...string) (string, error) {
	return f.Kubectl(ctx, "", append([]string{"-n", "kube-system", "exec", "e71-native-source-tools", "--"}, args...)...)
}

func (f *FilesystemAdoptionFixture) PrepareDirectory(ctx context.Context, fsType string, requestedBytes int64) error {
	ordinal := strings.TrimPrefix(strings.ToUpper(f.TCID), "E71.")
	if dash := strings.IndexByte(ordinal, '-'); dash >= 0 {
		ordinal = ordinal[:dash]
	}
	n, err := strconv.Atoi(ordinal)
	if err != nil || n < 1 || n > 32 {
		return fmt.Errorf("%s: directory fixture requires an E71 ordinal", f.TCID)
	}
	f.BackendKind, f.Pool, f.RequestedBytes = "directory", "e71-files", requestedBytes
	f.ProjectID = uint32(n)
	f.CanonicalSource = f.SourceRoot + "/" + fsType + "/e71-" + ordinal
	f.MountPoint = f.CanonicalSource
	s, err := f.Snapshot(ctx)
	if err != nil {
		return err
	}
	f.recordSnapshot(s)
	return nil
}

func (f *FilesystemAdoptionFixture) PrepareZFS(ctx context.Context, requestedBytes int64) error {
	f.BackendKind = "zfs-dataset"
	f.Pool = os.Getenv(suiteZFSPoolEnvVar)
	f.Parent = os.Getenv(suiteNFSParentDatasetEnvVar)
	f.RequestedBytes = requestedBytes
	if f.Pool == "" || f.Parent == "" {
		return fmt.Errorf("%s: native ZFS pool/parent missing", f.TCID)
	}
	f.CanonicalSource = f.Pool + "/" + f.Parent + "/" + f.Namespace
	f.MountPoint = f.SourceRoot + "/" + f.Namespace
	script := "set -eu; ! zfs list -H " + shellQuote(f.CanonicalSource) + " >/dev/null 2>&1; zfs create -o quota=" + strconv.FormatInt(requestedBytes, 10) + " -o refquota=" + strconv.FormatInt(requestedBytes, 10) + " -o mountpoint=" + shellQuote(f.MountPoint) + " -o compression=off " + shellQuote(f.CanonicalSource) + "; mkdir -p " + shellQuote(f.MountPoint+"/tree") + "; printf native-source > " + shellQuote(f.MountPoint+"/tree/preexisting") + "; printf host-writer > " + shellQuote(f.MountPoint+"/tree/host") + "; chown -R 1234:2345 " + shellQuote(f.MountPoint) + "; chmod 750 " + shellQuote(f.MountPoint) + " " + shellQuote(f.MountPoint+"/tree") + "; chmod 640 " + shellQuote(f.MountPoint+"/tree/preexisting") + "; sync"
	if _, err := f.HostExec(ctx, "sh", "-ceu", script); err != nil {
		return err
	}
	f.CreatedSource = true
	s, err := f.Snapshot(ctx)
	if err != nil {
		return err
	}
	f.recordSnapshot(s)
	return nil
}
func (f *FilesystemAdoptionFixture) recordSnapshot(s FilesystemAdoptionSnapshot) {
	f.NativeID = s.NativeID
	f.CapacityBytes = s.CapacityBytes
	f.SourceUID = s.UID
	f.SourceGID = s.GID
	f.SourceMode = s.Mode
	f.OriginalProperties = s.Properties
}

func (f *FilesystemAdoptionFixture) Snapshot(ctx context.Context) (FilesystemAdoptionSnapshot, error) {
	s := FilesystemAdoptionSnapshot{CanonicalSource: f.CanonicalSource, Properties: map[string]string{}}
	var out string
	var err error
	if f.BackendKind == "zfs-dataset" {
		out, err = f.HostExec(ctx, "zfs", "get", "-Hp", "-o", "property,value", "guid,mountpoint,quota,refquota,compression,readonly,canmount,sharenfs", f.CanonicalSource)
		if err != nil {
			return s, err
		}
		for _, line := range strings.Split(out, "\n") {
			p := strings.SplitN(line, "\t", 2)
			if len(p) == 2 {
				s.Properties[p[0]] = p[1]
			}
		}
		s.NativeID = s.Properties["guid"]
		s.CapacityBytes, _ = strconv.ParseInt(s.Properties["refquota"], 10, 64)
	} else {
		out, err = f.nativeExec(ctx, "findmnt", "-no", "UUID,FSTYPE", "-T", f.MountPoint)
		if err != nil {
			return s, err
		}
		parts := strings.Fields(out)
		if len(parts) != 2 {
			return s, fmt.Errorf("incomplete native mount identity %q", out)
		}
		s.Properties["uuid"], s.Properties["fsType"] = parts[0], parts[1]
		if parts[1] == "xfs" {
			out, err = f.nativeExec(ctx, "xfs_quota", "-x", "-c", "report -p -b", f.SourceRoot+"/xfs")
		} else {
			out, err = f.nativeExec(ctx, "repquota", "-P", "-n", "-O", "csv", f.SourceRoot+"/ext4")
		}
		if err != nil {
			return s, err
		}
		s.Properties["quotaEvidence"] = out
		// The helper's output uses KiB blocks; pick the project row and its hard
		// bound rather than substituting filesystem capacity from statfs.
		if parts[1] == "xfs" {
			fields := strings.Fields(out)
			for i, v := range fields {
				if strings.TrimPrefix(v, "#") == strconv.FormatUint(uint64(f.ProjectID), 10) && i+3 < len(fields) {
					s.CapacityBytes, _ = strconv.ParseInt(fields[i+3], 10, 64)
					s.CapacityBytes *= 1024
					break
				}
			}
		} else {
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Split(line, ",")
				if len(fields) > 5 && strings.TrimPrefix(fields[0], "#") == strconv.FormatUint(uint64(f.ProjectID), 10) {
					s.CapacityBytes, _ = strconv.ParseInt(fields[5], 10, 64)
					s.CapacityBytes *= 1024
					break
				}
			}
		}
	}
	out, err = f.HostExec(ctx, "sh", "-ceu", "stat -c '%i %u %g %a' "+shellQuote(f.MountPoint)+"; cd "+shellQuote(f.MountPoint)+"; find . -xdev -type f -exec sha256sum {} \\; | sort; find . -xdev -exec stat -c '%n %u %g %a' {} \\; | sort")
	if err != nil {
		return s, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		return s, fmt.Errorf("source metadata missing")
	}
	parts := strings.Fields(lines[0])
	if len(parts) != 4 {
		return s, fmt.Errorf("source metadata invalid: %q", lines[0])
	}
	s.UID, _ = strconv.ParseInt(parts[1], 10, 64)
	s.GID, _ = strconv.ParseInt(parts[2], 10, 64)
	s.Mode = parts[3]
	if f.BackendKind == "directory" {
		s.NativeID = s.Properties["uuid"] + ":" + parts[0]
	}
	s.TreeHash = strings.Join(lines[1:], "\n")
	return s, nil
}

func (f *FilesystemAdoptionFixture) ApplyObjects(ctx context.Context, local bool) error {
	f.LocalAttach = local
	agents, err := f.Kubectl(ctx, "", "get", "pillaragent", "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeRef struct {
					Name string `json:"name"`
				} `json:"nodeRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err = json.Unmarshal([]byte(agents), &list); err != nil {
		return err
	}
	for _, a := range list.Items {
		if a.Spec.NodeRef.Name == f.StorageNode {
			f.AgentName = a.Metadata.Name
			break
		}
	}
	if f.AgentName == "" {
		f.AgentName = f.Namespace + "-agent"
		if _, err = f.Kubectl(ctx, fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarAgent\nmetadata:\n  name: %s\nspec:\n  nodeRef:\n    name: %s\n    addressType: InternalIP\n    port: 9500\n", f.AgentName, f.StorageNode), "apply", "-f", "-"); err != nil {
			return err
		}
	}
	if f.AgentName == f.Namespace+"-agent" {
		f.CreatedAgent = true
	}
	backend := fmt.Sprintf("zfs:\n      pool: %s\n      parentDataset: %s\n      volumeType: dataset", f.Pool, f.Parent)
	if f.BackendKind == "directory" {
		backend = fmt.Sprintf("directory:\n      logicalPool: %s\n      hostRoot: %s", f.Pool, f.SourceRoot)
	}
	manifest := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n---\napiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarStore\nmetadata:\n  name: %s\nspec:\n  agentRef: %s\n  backend:\n    %s\n---\napiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarProtocol\nmetadata:\n  name: %s\nspec:\n  protocol:\n    nfs:\n      version: \"4.2\"\n      port: 2049\n      acl: true\n", f.Namespace, f.StoreName, f.AgentName, backend, f.ProtocolName)
	if _, err = f.Kubectl(ctx, manifest, "apply", "-f", "-"); err != nil {
		return err
	}
	for _, item := range []struct {
		Name  string
		Local bool
	}{{f.LocalPSCName, local}, {f.RemotePSCName, false}} {
		m := fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarStorageClass\nmetadata:\n  name: %s\nspec:\n  csiDriver: files.pillar-csi.bhyoo.com\n  storeRef: %s\n  protocolRef: %s\n  localAttach: %t\n  storageClass:\n    name: %s\n    reclaimPolicy: Retain\n    volumeBindingMode: Immediate\n    allowVolumeExpansion: false\n", item.Name, f.StoreName, f.ProtocolName, item.Local, item.Name)
		if _, err = f.Kubectl(ctx, m, "apply", "-f", "-"); err != nil {
			return err
		}
		if _, err = f.Kubectl(ctx, "", "wait", "--for=condition=Ready", "pillarstorageclass/"+item.Name, "--timeout=3m"); err != nil {
			return err
		}
	}
	return nil
}

// AdoptPVC creates the adoption PVC for the source prepared by this fixture.
// ApplyObjects selects the PillarStorageClass route: local=true uses the
// localAttach class and its storage-node topology, while local=false uses the
// NFS class. mode is written unchanged to PVC accessModes so the controller
// can distinguish single-node RWO from multi-node RWX adoption.
func (f *FilesystemAdoptionFixture) AdoptPVC(ctx context.Context, key, mode string, bytes int64) error {
	storageClass := f.RemoteStorageClass
	if f.LocalAttach {
		storageClass = f.LocalStorageClass
	}
	manifest := fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\n  annotations:\n    %s: %s\nspec:\n  accessModes: [%s]\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %d\n", f.PVCName, f.Namespace, key, f.CanonicalSource, mode, storageClass, bytes)
	_, err := f.Kubectl(ctx, manifest, "apply", "-f", "-")
	return err
}

// RequestExpansion submits a normal Kubernetes PVC resize request. A PVC
// backed by a file StorageClass is rejected by Kubernetes admission before
// any external-resizer or CSI ControllerExpandVolume call, so callers observe
// the refusal through the Kubernetes API and the original request remains
// unchanged.
func (f *FilesystemAdoptionFixture) RequestExpansion(ctx context.Context, bytes int64) error {
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":"%d"}}}}`, bytes)
	_, err := f.Kubectl(ctx, "", "-n", f.Namespace, "patch", "pvc", f.PVCName, "--type=merge", "-p", patch)
	return err
}

func (f *FilesystemAdoptionFixture) PodManifest(name, claim, node string, ro bool, uid, gid int64) string {
	object := map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]string{"name": name, "namespace": f.Namespace}, "spec": map[string]any{"nodeName": node, "restartPolicy": "Never", "terminationGracePeriodSeconds": 5, "tolerations": []any{map[string]any{"operator": "Exists"}}, "securityContext": map[string]any{"runAsUser": uid, "runAsGroup": gid, "fsGroup": 5555}, "containers": []any{map[string]any{"name": "workload", "image": "busybox:1.38.0", "command": []string{"sh", "-ceu", "trap 'exit 0' TERM; sleep 3600 & wait"}, "volumeMounts": []any{map[string]any{"name": "data", "mountPath": "/data", "readOnly": ro}}}}, "volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": claim, "readOnly": ro}}}}}
	b, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *FilesystemAdoptionFixture) WithQuotaDrift(ctx context.Context) (func() error, error) {
	if f.BackendKind != "zfs-dataset" {
		return nil, fmt.Errorf("quota drift requires fixture-owned ZFS")
	}
	quota, refquota := f.OriginalProperties["quota"], f.OriginalProperties["refquota"]
	if _, err := f.HostExec(ctx, "sh", "-ceu", "zfs set quota=33554432 "+shellQuote(f.CanonicalSource)+"; zfs set refquota=33554432 "+shellQuote(f.CanonicalSource)); err != nil {
		return nil, err
	}
	return func() error {
		_, err := f.HostExec(context.Background(), "sh", "-ceu", "zfs set quota="+shellQuote(quota)+" "+shellQuote(f.CanonicalSource)+"; zfs set refquota="+shellQuote(refquota)+" "+shellQuote(f.CanonicalSource))
		return err
	}, nil
}
func (f *FilesystemAdoptionFixture) WithIdentityDrift(ctx context.Context) (func() error, error) {
	if !f.CreatedSource || f.BackendKind != "zfs-dataset" {
		return nil, fmt.Errorf("identity drift requires owned ZFS")
	}
	backup := f.CanonicalSource + "-original"
	replacement := f.MountPoint + "-replacement"
	script := "set -eu; zfs rename " + shellQuote(f.CanonicalSource) + " " + shellQuote(backup) + "; zfs set mountpoint=" + shellQuote(f.MountPoint) + " " + shellQuote(backup) + "; zfs mount " + shellQuote(backup) + " || true; zfs create -o mountpoint=" + shellQuote(replacement) + " -o quota=67108864 -o refquota=67108864 " + shellQuote(f.CanonicalSource)
	if _, err := f.HostExec(ctx, "sh", "-ceu", script); err != nil {
		return nil, err
	}
	return func() error {
		_, err := f.HostExec(context.Background(), "sh", "-ceu", "set -eu; zfs destroy -r "+shellQuote(f.CanonicalSource)+"; zfs rename "+shellQuote(backup)+" "+shellQuote(f.CanonicalSource)+"; zfs set mountpoint="+shellQuote(f.MountPoint)+" "+shellQuote(f.CanonicalSource)+"; zfs mount "+shellQuote(f.CanonicalSource)+" || true")
		return err
	}, nil
}
func (f *FilesystemAdoptionFixture) Cleanup(ctx context.Context) error {
	before, err := f.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("refusing source cleanup without preservation snapshot: %w", err)
	}
	if f.PVName == "" {
		f.PVName, _ = f.Kubectl(ctx, "", "-n", f.Namespace, "get", "pvc", f.PVCName, "--ignore-not-found=true", "-o", "jsonpath={.spec.volumeName}")
	}
	if _, err = f.Kubectl(ctx, "", "-n", f.Namespace, "delete", "pods", "--all", "--ignore-not-found=true", "--wait=true", "--timeout=3m"); err != nil {
		return err
	}
	if _, err = f.Kubectl(ctx, "", "-n", f.Namespace, "delete", "pvc", "--all", "--ignore-not-found=true", "--wait=true", "--timeout=3m"); err != nil {
		return err
	}
	if f.PVName != "" {
		if _, err = f.Kubectl(ctx, "", "patch", "pv", f.PVName, "--type=merge", "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`); err != nil && !strings.Contains(err.Error(), "NotFound") {
			return err
		}
		if _, err = f.Kubectl(ctx, "", "delete", "pv", f.PVName, "--ignore-not-found=true", "--wait=true", "--timeout=3m"); err != nil {
			return err
		}
		for _, kind := range []string{"pv", "pillarvolumestate"} {
			if _, err = f.Kubectl(ctx, "", "wait", "--for=delete", kind+"/"+f.PVName, "--timeout=3m"); err != nil && !strings.Contains(err.Error(), "NotFound") {
				return err
			}
		}
	}
	after, err := f.Snapshot(ctx)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		return fmt.Errorf("CSI cleanup changed original source: before=%s after=%s", a, b)
	}
	for _, r := range []struct {
		Kind  string
		Names []string
	}{{"pillarstorageclass", []string{f.LocalPSCName, f.RemotePSCName}}, {"pillarprotocol", []string{f.ProtocolName}}, {"pillarstore", []string{f.StoreName}}} {
		if _, err = f.Kubectl(ctx, "", append([]string{"delete", r.Kind, "--ignore-not-found=true", "--wait=true", "--timeout=2m"}, r.Names...)...); err != nil {
			return err
		}
	}
	if f.CreatedAgent {
		if _, err = f.Kubectl(ctx, "", "delete", "pillaragent", f.AgentName, "--ignore-not-found=true", "--wait=true", "--timeout=2m"); err != nil {
			return err
		}
	}
	if _, err = f.Kubectl(ctx, "", "delete", "namespace", f.Namespace, "--ignore-not-found=true", "--wait=true", "--timeout=2m"); err != nil {
		return err
	}
	if f.CreatedSource {
		if _, err = f.HostExec(ctx, "zfs", "destroy", f.CanonicalSource); err != nil {
			return err
		}
	}
	return nil
}
func formatBytes(n int64) string { return strconv.FormatInt(n, 10) }
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

var _ = time.Second
