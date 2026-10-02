//go:build e2e

package e2e

// E37 runs only in the dedicated real Kind/ZFS/NFS lane (label nfs).
// Registration is unconditional so read-only catalog enumeration discovers all
// cases. Prerequisite failures are failures, never conditional skips.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	nfsNamespace    = "pillar-e2e-nfs"
	nfsStore        = "pillar-e2e-nfs-store"
	nfsAgent        = "pillar-e2e-nfs-agent"
	nfsProtocol     = "pillar-e2e-nfs-protocol"
	nfsRootProto    = "pillar-e2e-nfs-root"
	nfsPublicProto  = "pillar-e2e-nfs-public"
	nfsClass        = "pillar-e2e-nfs"
	nfsRootClass    = "pillar-e2e-nfs-root"
	nfsPublicClass  = "pillar-e2e-nfs-public"
	nfsPVC          = "dataset"
	nfsHelperLedger = "/tmp/pillar-e37-nfs-helper-ledger"
)

func nfsKubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig=" + os.Getenv("KUBECONFIG"), "--request-timeout=20s"}, args...)...) //nolint:gosec
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func nfsMust(ctx context.Context, args ...string) string {
	out, err := nfsKubectl(ctx, "", args...)
	Expect(err).NotTo(HaveOccurred())
	return out
}

func nfsApply(ctx context.Context, manifest string) {
	_, err := nfsKubectl(ctx, manifest, "apply", "-f", "-")
	Expect(err).NotTo(HaveOccurred())
}

func nfsPodExec(ctx context.Context, pod, command string) string {
	return nfsMust(ctx, "-n", nfsNamespace, "exec", pod, "--", "sh", "-ceu", command)
}

func nfsComponentPod(ctx context.Context, component, node string) string {
	args := []string{"-n", resolveHelmNamespace(), "get", "pods", "-l", "app.kubernetes.io/component=" + component}
	if node != "" {
		args = append(args, "--field-selector", "spec.nodeName="+node)
	}
	args = append(args, "-o", "jsonpath={.items[0].metadata.name}")
	pod := nfsMust(ctx, args...)
	Expect(pod).NotTo(BeEmpty())
	return pod
}

func nfsAgentExec(ctx context.Context, args ...string) string {
	pod := nfsComponentPod(ctx, "agent", os.Getenv(suiteBackendContainerEnvVar))
	return nfsMust(ctx, append([]string{"-n", resolveHelmNamespace(), "exec", pod, "-c", "agent", "--"}, args...)...)
}

func nfsNodeExec(ctx context.Context, node string, args ...string) (string, error) {
	pod := nfsComponentPod(ctx, "node", node)
	return nfsKubectl(ctx, "", append([]string{"-n", resolveHelmNamespace(), "exec", pod, "-c", "node", "--"}, args...)...)
}

func nfsDeleteWorkload(ctx context.Context, resources ...string) {
	nfsMust(ctx, append([]string{"-n", nfsNamespace, "delete", "--ignore-not-found=true", "--wait=true", "--timeout=3m"}, resources...)...)
}

func nfsWaitReady(ctx context.Context, resource string) {
	nfsMust(ctx, "wait", "--for=condition=Ready", resource, "--timeout=3m")
}

func nfsWaitPod(ctx context.Context, name string) {
	nfsMust(ctx, "-n", nfsNamespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=3m")
}

func nfsBoundPV(ctx context.Context, name string) string {
	nfsMust(ctx, "-n", nfsNamespace, "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+name, "--timeout=3m")
	pv := nfsMust(ctx, "-n", nfsNamespace, "get", "pvc", name, "-o", "jsonpath={.spec.volumeName}")
	Expect(pv).NotTo(BeEmpty())
	return pv
}
func nfsWaitDeleted(ctx context.Context, resource string) {
	Eventually(func() (string, error) {
		return nfsKubectl(ctx, "", "get", resource, "--ignore-not-found=true", "-o", "name")
	}, 90*time.Second, 2*time.Second).Should(BeEmpty())
}

func nfsPVCManifest(name, class, mode, volumeMode string, annotations map[string]string) string {
	object := map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": nfsNamespace, "annotations": annotations},
		"spec":     map[string]any{"accessModes": []string{mode}, "volumeMode": volumeMode, "storageClassName": class, "resources": map[string]any{"requests": map[string]string{"storage": "64Mi"}}},
	}
	data, err := json.Marshal(object)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func nfsPodManifest(name, claim, node string, readOnly bool, user, group int64) string {
	security := map[string]any{"runAsUser": user, "runAsGroup": group}
	if user != 0 {
		security["fsGroup"] = group
	}
	object := map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]string{"name": name, "namespace": nfsNamespace},
		"spec": map[string]any{
			"restartPolicy": "Never", "terminationGracePeriodSeconds": 5,
			"nodeSelector": map[string]string{"kubernetes.io/hostname": node}, "securityContext": security,
			"containers": []any{map[string]any{"name": "workload", "image": "busybox:1.38.0", "command": []string{"sh", "-c", "trap 'exit 0' TERM; sleep 3600 & wait"}, "volumeMounts": []any{map[string]any{"name": "data", "mountPath": "/data", "readOnly": readOnly}}}},
			"volumes":    []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": claim, "readOnly": readOnly}}},
		},
	}
	data, err := json.Marshal(object)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

func nfsBinding(name, store, protocol, extra string) string {
	return fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %s
spec:
  storeRef: %s
  protocolRef: %s
%s  storageClass:
    name: %s
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
`, name, store, protocol, extra, name)
}

func nfsState(ctx context.Context, pv, path string) string {
	return nfsMust(ctx, "get", "pillarvolumestate", pv, "-o", "jsonpath={"+path+"}")
}

func nfsDataset(ctx context.Context, pv string) string {
	target := nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.target_id}")
	Expect(target).To(HavePrefix("/"))
	return strings.TrimPrefix(target, "/")
}

func nfsPublicationNodes(ctx context.Context, pv string) []string {
	return strings.Fields(nfsState(ctx, pv, ".status.publishedNodes[*].nodeID"))
}

// exportfs -v may wrap a long filesystem path onto its own line. Parse both
// forms and keep policies scoped to the exact physical dataset, never a
// sibling or the wildcard pseudoroot.
func nfsExportPolicies(exports, path string) map[string][]string {
	policies := map[string][]string{}
	current := ""
	for _, line := range strings.Split(exports, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "/") {
			current = fields[0]
			fields = fields[1:]
		}
		if current != path {
			continue
		}
		for _, field := range fields {
			client, options, ok := strings.Cut(field, "(")
			if !ok {
				continue
			}
			policies[client] = strings.Split(strings.TrimSuffix(options, ")"), ",")
		}
	}
	return policies
}

func nfsHideClientHelpers(ctx context.Context, node string) {
	_, err := kindContainerExec(ctx, node, "sh", "-ceu", `
ledger="$1"
tmp="${ledger}.tmp.$$"
[ ! -e "$ledger" ] || { printf 'owned helper ledger already exists: %s\n' "$ledger" >&2; exit 1; }
[ ! -e "$tmp" ] || { printf 'stale helper ledger transaction exists: %s\n' "$tmp" >&2; exit 1; }

	# Inspect all PATH entries so backups remain detectable even when the
	# corresponding helper itself is already hidden by a stale transaction.
	oldIFS="$IFS"
	IFS=:
	for dir in ${PATH:-}; do
	  [ -n "$dir" ] || dir=.
	  for helper in mount.nfs mount.nfs4; do
	    backup="$dir/$helper.pillar-e37-hidden"
	    [ ! -e "$backup" ] || { printf 'stale owned helper backup exists: %s\n' "$backup" >&2; exit 1; }
	  done
	done
	IFS="$oldIFS"

	# Inspect all currently visible helpers before creating or truncating any
	# ownership ledger.
	for helper in mount.nfs mount.nfs4; do
	  path="$(command -v "$helper" 2>/dev/null || true)"
	  [ -n "$path" ] || continue
	  backup="${path}.pillar-e37-hidden"
	  [ ! -e "$backup" ] || { printf 'stale owned helper backup exists: %s\n' "$backup" >&2; exit 1; }
	done

rollback() {
  status=0
  while IFS="$(printf '\t')" read -r path backup; do
    [ -n "$path" ] || continue
    if [ ! -e "$backup" ]; then
      printf 'rollback missing owned helper backup: %s\n' "$backup" >&2
      status=1
      continue
    fi
    if [ -e "$path" ]; then
      printf 'rollback helper path unexpectedly occupied: %s\n' "$path" >&2
      status=1
      continue
    fi
    if ! mv "$backup" "$path"; then
      printf 'rollback failed for helper: %s\n' "$path" >&2
      status=1
    fi
  done <"$tmp"
  return "$status"
}

rollback_or_preserve_ledger() {
  if rollback; then
    rm -f "$tmp" || {
      printf 'remove helper rollback ledger failed: %s\n' "$tmp" >&2
      return 1
    }
    return 0
  fi
  if ! mv "$tmp" "$ledger"; then
    printf 'preserve helper ownership ledger failed: %s\n' "$ledger" >&2
  fi
  return 1
}

: >"$tmp"
for helper in mount.nfs mount.nfs4; do
  path="$(command -v "$helper" 2>/dev/null || true)"
  [ -n "$path" ] || continue
  backup="${path}.pillar-e37-hidden"
  if ! mv "$path" "$backup"; then
    printf 'hide helper failed: %s\n' "$path" >&2
    rollback_or_preserve_ledger
    exit 1
  fi
  printf '%s\t%s\n' "$path" "$backup" >>"$tmp"
done
if command -v mount.nfs >/dev/null 2>&1 || command -v mount.nfs4 >/dev/null 2>&1; then
  printf 'client mount.nfs helper remains visible after isolation\n' >&2
  rollback_or_preserve_ledger
  exit 1
fi
if ! mv "$tmp" "$ledger"; then
  printf 'publish helper ownership ledger failed: %s\n' "$ledger" >&2
  rollback_or_preserve_ledger
  exit 1
fi
`, "nfs-helper-isolation", nfsHelperLedger)
	Expect(err).NotTo(HaveOccurred(), "hide baseline NFS client helpers on dedicated node %s", node)
}

func nfsRestoreClientHelpers(ctx context.Context, node string) {
	_, err := kindContainerExec(ctx, node, "sh", "-ceu", `
ledger="$1"
[ -f "$ledger" ] || exit 0
restored="${ledger}.restore.$$"
[ ! -e "$restored" ] || { printf 'stale helper restore ledger exists: %s\n' "$restored" >&2; exit 1; }
: >"$restored"
status=0
while IFS="$(printf '\t')" read -r path backup; do
  [ -n "$path" ] || continue
  if [ -e "$backup" ] && [ ! -e "$path" ]; then
    if ! mv "$backup" "$path"; then
      printf 'restore failed for helper: %s\n' "$path" >&2
      status=1
      break
    fi
  elif [ ! -e "$backup" ] && [ -e "$path" ]; then
    printf 'helper already restored: %s\n' "$path" >&2
  elif [ -e "$backup" ] && [ -e "$path" ]; then
    printf 'helper path unexpectedly occupied during restore: %s\n' "$path" >&2
    status=1
    break
  else
    printf 'missing owned helper backup: %s\n' "$backup" >&2
    status=1
    break
  fi
  printf '%s\t%s\n' "$path" "$backup" >>"$restored"
done <"$ledger"

if [ "$status" -ne 0 ]; then
  rollback_status=0
  while IFS="$(printf '\t')" read -r path backup; do
    [ -n "$path" ] || continue
    if [ -e "$backup" ]; then
      printf 'restore rollback backup unexpectedly exists: %s\n' "$backup" >&2
      rollback_status=1
    elif [ ! -e "$path" ] || ! mv "$path" "$backup"; then
      printf 'restore rollback failed for helper: %s\n' "$path" >&2
      rollback_status=1
    fi
  done <"$restored"
  rm -f "$restored" || rollback_status=1
  [ "$rollback_status" -eq 0 ] || printf 'restore rollback incomplete; ownership ledger preserved: %s\n' "$ledger" >&2
  exit 1
fi
if ! rm -f "$restored"; then
  printf 'remove helper restore ledger failed: %s\n' "$restored" >&2
  exit 1
fi
if ! rm -f "$ledger"; then
  printf 'remove helper ownership ledger failed: %s\n' "$ledger" >&2
  exit 1
fi
`, "nfs-helper-restore", nfsHelperLedger)
	Expect(err).NotTo(HaveOccurred(), "restore baseline NFS client helpers on dedicated node %s", node)
}

func nfsDeniedProbe(ctx context.Context, node, source, marker string) {
	out, err := nfsNodeExec(ctx, node, "/bin/busybox", "nc", "-z", "-w", "3", strings.Split(source, ":")[0], "2049")
	Expect(err).NotTo(HaveOccurred(), "NFS listener must be reachable: %s", out)
	mountPath := "/tmp/pillar-e37-acl-probe"
	_, err = nfsNodeExec(ctx, node, "/bin/busybox", "mkdir", "-p", mountPath)
	Expect(err).NotTo(HaveOccurred())
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err = nfsNodeExec(probeCtx, node, "mount.nfs", "-o", "vers=4.2,proto=tcp,hard,retry=0", source, mountPath)
	if err != nil {
		Expect(probeCtx.Err()).NotTo(HaveOccurred(), "timeout is not ACL denial evidence")
		Expect(strings.ToLower(err.Error())).To(Or(ContainSubstring("access denied"), ContainSubstring("permission denied"), ContainSubstring("operation not permitted")))
		return
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := nfsNodeExec(cleanupCtx, node, "umount", mountPath)
		Expect(err).NotTo(HaveOccurred())
	}()
	_, readErr := nfsNodeExec(probeCtx, node, "/bin/busybox", "cat", mountPath+"/"+marker)
	Expect(readErr).To(HaveOccurred(), "unpublished/revoked IP reached private dataset marker")
	_, writeErr := nfsNodeExec(probeCtx, node, "/bin/busybox", "sh", "-c", "printf unauthorized > \"$1/unauthorized\"", "probe", mountPath)
	Expect(writeErr).To(HaveOccurred(), "unpublished/revoked IP wrote into private dataset")
	Expect(probeCtx.Err()).NotTo(HaveOccurred(), "hung I/O is not denial evidence")
}

func nfsRejectedPVC(ctx context.Context, name, class, volumeMode string, annotations map[string]string) {
	nfsApply(ctx, nfsPVCManifest(name, class, "ReadWriteMany", volumeMode, annotations))
	Eventually(func() string {
		return nfsMust(ctx, "-n", nfsNamespace, "get", "events", "--field-selector", "involvedObject.name="+name+",reason=ProvisioningFailed", "-o", "jsonpath={.items[*].message}")
	}, 90*time.Second, 2*time.Second).Should(ContainSubstring("InvalidArgument"))
	Expect(nfsMust(ctx, "-n", nfsNamespace, "get", "pvc", name, "-o", "jsonpath={.spec.volumeName}")).To(BeEmpty())
	nfsDeleteWorkload(ctx, "pvc/"+name)
}

var _ = Describe("E37: real ZFS dataset + NFS multi-node E2E", Label("nfs", "e37"), Ordered, func() {
	var ctx context.Context
	var cancel context.CancelFunc
	var workers []string
	var backend, pool, parent, address, pv, dataset, target string
	var createdPVs []string
	var stageFile string
	BeforeAll(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Minute)
		Expect(os.Getenv("E2E_NFS_E2E")).To(Equal("true"), "run the dedicated NFS lane, not the unprepared default profile")
		Expect(resolveUseExistingCluster()).To(BeFalse(),
			"TC-E37 requires a newly created, exclusively owned Kind cluster; E2E_USE_EXISTING_CLUSTER must be unset")
		Expect(suiteKindCluster).NotTo(BeNil(), "TC-E37 requires the suite Kind cluster state")
		Expect(suiteKindCluster.clusterCreated).To(BeTrue(),
			"TC-E37 refuses reused or unowned Kind fixtures because helper isolation is destructive")
		backend, pool, parent = os.Getenv(suiteBackendContainerEnvVar), os.Getenv(suiteZFSPoolEnvVar), os.Getenv(suiteNFSParentDatasetEnvVar)
		Expect(backend).NotTo(BeEmpty())
		Expect(pool).NotTo(BeEmpty())
		Expect(parent).NotTo(BeEmpty())
		workers = strings.Fields(nfsMust(ctx, "get", "nodes", "-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[*].metadata.name}"))
		Expect(workers).To(HaveLen(2))
		address = nfsMust(ctx, "get", "node", backend, "-o", "jsonpath={.status.addresses[?(@.type==\"InternalIP\")].address}")
		Expect(address).NotTo(BeEmpty())
		for _, node := range workers {
			nfsHideClientHelpers(ctx, node)
		}
		for _, node := range append(append([]string{}, workers...), backend) {
			out, err := kindContainerExec(ctx, node, "sh", "-ceu", `for helper in exportfs rpc.mountd; do if command -v "$helper" >/dev/null 2>&1; then printf 'unexpected server helper: %s\n' "$helper"; exit 1; fi; done; printf 'host NFS server helpers absent\n'`)
			Expect(err).NotTo(HaveOccurred())
			AddReportEntry("host-zero-install "+node, out)
		}
		hostHelper, err := kindContainerExec(ctx, workers[0], "sh", "-ceu", `if command -v mount.nfs >/dev/null 2>&1 || command -v mount.nfs4 >/dev/null 2>&1; then exit 1; fi; printf unavailable`)
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("isolated host mount.nfs", hostHelper)
		nodeHelper, err := nfsNodeExec(ctx, workers[0], "mount.nfs", "-V")
		Expect(err).NotTo(HaveOccurred())
		AddReportEntry("bundled node mount.nfs", nodeHelper)
		nfsApply(ctx, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+nfsNamespace+"\n")
		nfsApply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: %s
spec:
  nodeRef:
    name: %s
    addressType: InternalIP
    port: 9500
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: %s
spec:
  agentRef: %s
  backend:
    zfs:
      pool: %s
      parentDataset: %s
      volumeType: dataset
`, nfsAgent, backend, nfsStore, nfsAgent, pool, parent))
		for _, proto := range []struct {
			name, squash string
			acl          bool
		}{{nfsProtocol, "none", true}, {nfsRootProto, "", true}, {nfsPublicProto, "none", false}} {
			squash := ""
			if proto.squash != "" {
				squash = "      squash: " + proto.squash + "\n"
			}
			nfsApply(ctx, fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarProtocol\nmetadata:\n  name: %s\nspec:\n  protocol:\n    nfs:\n      version: \"4.2\"\n      port: 2049\n      acl: %t\n%s", proto.name, proto.acl, squash))
		}
		for _, binding := range []struct{ name, proto string }{{nfsClass, nfsProtocol}, {nfsRootClass, nfsRootProto}, {nfsPublicClass, nfsPublicProto}} {
			nfsApply(ctx, nfsBinding(binding.name, nfsStore, binding.proto, "  filesystem:\n    mountOptions: [hard,noatime]\n"))
		}
		nfsWaitReady(ctx, "pillaragent/"+nfsAgent)
		nfsWaitReady(ctx, "pillarstore/"+nfsStore)
		for _, class := range []string{nfsClass, nfsRootClass, nfsPublicClass} {
			nfsWaitReady(ctx, "pillarstorageclass/"+class)
		}
	})
	AfterAll(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		defer func() {
			if cancel != nil {
				cancel()
			}
		}()
		defer func() {
			for _, node := range workers {
				nfsRestoreClientHelpers(context.Background(), node)
			}
		}()
		_, err := nfsKubectl(cleanupCtx, "", "delete", "namespace", nfsNamespace, "--ignore-not-found=true", "--wait=true", "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
		for _, name := range createdPVs {
			nfsWaitDeleted(cleanupCtx, "pv/"+name)
		}
		for _, args := range [][]string{{"pillarstorageclass", nfsClass, nfsRootClass, nfsPublicClass, "pillar-e37-local", "pillar-e37-zvol-block"}, {"pillarprotocol", nfsProtocol, nfsRootProto, nfsPublicProto, "pillar-e37-nvme"}, {"pillarstore", nfsStore, "pillar-e37-zvol", "pillar-e37-lvm"}, {"pillaragent", nfsAgent}} {
			_, err = nfsKubectl(cleanupCtx, "", append([]string{"delete", "--ignore-not-found=true", "--wait=true", "--timeout=60s"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err = nfsKubectl(cleanupCtx, "", "delete", "storageclass", "pillar-e37-zvol-nfs", "pillar-e37-lvm-nfs", "pillar-e37-dataset-block", "--ignore-not-found=true")
		Expect(err).NotTo(HaveOccurred())
	})

	It("[TC-E37.1] provisions an NFS filesystem with explicit version, address, port, and dataset evidence", func() {
		nfsApply(ctx, nfsPVCManifest(nfsPVC, nfsClass, "ReadWriteMany", "Filesystem", nil))
		pv = nfsBoundPV(ctx, nfsPVC)
		createdPVs = append(createdPVs, pv)
		Expect(nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.fsType}")).To(Equal("nfs"))
		Expect(nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.address}")).To(Equal(address))
		Expect(nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeAttributes.port}")).To(Equal("2049"))
		dataset = nfsDataset(ctx, pv)
		Expect(dataset).To(HavePrefix(pool + "/" + parent + "/"))
		target = address + ":/" + dataset
		properties := nfsAgentExec(ctx, "zfs", "get", "-Hp", "-o", "property,value", "type,refquota,mountpoint,sharenfs", dataset)
		Expect(properties).To(ContainSubstring("type\tfilesystem"))
		Expect(properties).To(ContainSubstring("refquota\t67108864"))
		Expect(properties).To(ContainSubstring("sharenfs\toff"))
		Expect(properties).To(ContainSubstring(suiteNFSDatasetRoot + "/" + dataset))
		AddReportEntry("dataset evidence", properties)
	})
	It("[TC-E37.2] permits bidirectional RWX data across two worker nodes", func() {
		for i, name := range []string{"writer", "reader"} {
			nfsApply(ctx, nfsPodManifest(name, nfsPVC, workers[i], false, 0, 0))
			nfsWaitPod(ctx, name)
			Expect(nfsMust(ctx, "-n", nfsNamespace, "get", "pod", name, "-o", "jsonpath={.spec.nodeName}")).To(Equal(workers[i]))
		}
		netA, err := kindContainerExec(ctx, workers[0], "readlink", "/proc/self/ns/net")
		Expect(err).NotTo(HaveOccurred())
		netB, err := kindContainerExec(ctx, workers[1], "readlink", "/proc/self/ns/net")
		Expect(err).NotTo(HaveOccurred())
		Expect(netA).NotTo(Equal(netB))
		nfsPodExec(ctx, "writer", "printf writer-data > /data/from-writer; sync")
		Expect(nfsPodExec(ctx, "reader", "cat /data/from-writer")).To(Equal("writer-data"))
		nfsPodExec(ctx, "reader", "printf reader-data > /data/from-reader; sync")
		Expect(nfsPodExec(ctx, "writer", "cat /data/from-reader")).To(Equal("reader-data"))
		for _, pod := range []string{"writer", "reader"} {
			mount := nfsPodExec(ctx, pod, "awk '$2 == \"/data\" {print}' /proc/mounts")
			Expect(mount).To(ContainSubstring("nfs4"))
			Expect(mount).To(ContainSubstring("vers=4.2"))
			Expect(mount).To(ContainSubstring("hard"))
			Expect(mount).To(ContainSubstring("proto=tcp"))
			AddReportEntry("NFS mount "+pod, mount)
		}
		Expect(nfsPublicationNodes(ctx, pv)).To(ConsistOf(workers))
	})
	It("[TC-E37.3] keeps the surviving publisher readable after the peer is revoked", func() {
		nfsDeleteWorkload(ctx, "pod/reader")
		Eventually(func() []string { return nfsPublicationNodes(ctx, pv) }, 90*time.Second, 2*time.Second).Should(ConsistOf(workers[0]))
		nfsPodExec(ctx, "writer", "printf survivor-data > /data/survivor; sync")
		Expect(nfsPodExec(ctx, "writer", "cat /data/survivor; cat /data/from-reader")).To(Equal("survivor-datareader-data"))
	})
	It("[TC-E37.4] denies a client IP that was never published and a revoked peer", func() {
		nfsApply(ctx, nfsPVCManifest("public", nfsPublicClass, "ReadWriteMany", "Filesystem", nil))
		publicPV := nfsBoundPV(ctx, "public")
		createdPVs = append(createdPVs, publicPV)
		nfsApply(ctx, nfsPodManifest("public-writer", "public", workers[0], false, 0, 0))
		nfsWaitPod(ctx, "public-writer")
		nfsPodExec(ctx, "public-writer", "printf public-marker > /data/public-marker; sync")
		nfsPodExec(ctx, "writer", "printf private-marker > /data/private-marker; sync")
		nfsDeniedProbe(ctx, backend, target, "private-marker")
		nfsDeniedProbe(ctx, workers[1], target, "private-marker")
		Expect(nfsPodExec(ctx, "writer", "cat /data/private-marker; test ! -e /data/unauthorized")).To(Equal("private-marker"))
		Expect(nfsPodExec(ctx, "public-writer", "cat /data/public-marker")).To(Equal("public-marker"))
	})
	It("[TC-E37.5] grows the server-side quota and reports the growth through statfs", func() {
		statfs := func() int64 {
			value := nfsPodExec(ctx, "writer", "stat -f -c '%b %S' /data")
			var blocks, size int64
			_, err := fmt.Sscan(value, &blocks, &size)
			Expect(err).NotTo(HaveOccurred())
			return blocks * size
		}
		before := statfs()
		Expect(before).To(BeNumerically("<=", int64(64*1024*1024)))
		nfsMust(ctx, "-n", nfsNamespace, "patch", "pvc", nfsPVC, "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"128Mi"}}}}`)
		Eventually(func() string { return nfsAgentExec(ctx, "zfs", "get", "-Hp", "-o", "value", "refquota", dataset) }, 90*time.Second, 2*time.Second).Should(Equal("134217728"))
		Eventually(statfs, 90*time.Second, 2*time.Second).Should(BeNumerically(">", before))
		Eventually(func() string { return nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.capacity.storage}") }, 90*time.Second, 2*time.Second).Should(Equal("128Mi"))
		Expect(nfsPodExec(ctx, "writer", "cat /data/survivor")).To(Equal("survivor-data"))
		AddReportEntry("quota/statfs growth", fmt.Sprintf("before=%d after=%d quota=134217728", before, statfs()))
	})
	It("[TC-E37.6] exposes ROX as read-only while preserving previously written data", func() {
		nfsApply(ctx, nfsPVCManifest("rox", nfsRootClass, "ReadOnlyMany", "Filesystem", nil))
		roPV := nfsBoundPV(ctx, "rox")
		createdPVs = append(createdPVs, roPV)
		roDataset := nfsDataset(ctx, roPV)
		nfsAgentExec(ctx, "/bin/busybox", "sh", "-ceu", "printf rox-seed > \"$1/rox-seed\"; chmod 644 \"$1/rox-seed\"", "seed", suiteNFSDatasetRoot+"/"+roDataset)
		nfsApply(ctx, nfsPodManifest("rox-reader", "rox", workers[1], true, 0, 0))
		nfsWaitPod(ctx, "rox-reader")
		Expect(nfsPodExec(ctx, "rox-reader", "cat /data/rox-seed")).To(Equal("rox-seed"))
		_, err := nfsKubectl(ctx, "", "-n", nfsNamespace, "exec", "rox-reader", "--", "sh", "-c", "printf denied > /data/denied")
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("read-only"))
		exports := nfsAgentExec(ctx, "exportfs", "-v")
		roPolicies := nfsExportPolicies(exports, suiteNFSDatasetRoot+"/"+roDataset)
		Expect(roPolicies).NotTo(BeEmpty())
		for _, options := range roPolicies {
			Expect(options).To(ContainElements("ro", "root_squash"))
			Expect(options).NotTo(ContainElement("no_root_squash"))
		}
		AddReportEntry("ROX export", exports)
		// A readonly Pod on an otherwise writable RWX volume is a node bind
		// policy, distinct from the server-wide ROX policy proved above.
		nfsApply(ctx, nfsPodManifest("readonly-rwx", nfsPVC, workers[0], true, 0, 0))
		nfsWaitPod(ctx, "readonly-rwx")
		Expect(nfsPodExec(ctx, "readonly-rwx", "cat /data/from-writer")).To(Equal("writer-data"))
		_, err = nfsKubectl(ctx, "", "-n", nfsNamespace, "exec", "readonly-rwx", "--", "sh", "-c", "printf denied > /data/readonly-rwx-denied")
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("read-only"))
	})
	It("[TC-E37.7] permits non-root fsGroup writes with squash=none", func() {
		nfsApply(ctx, nfsPVCManifest("fs-group", nfsClass, "ReadWriteMany", "Filesystem", map[string]string{"pillar-csi.bhyoo.com/filesystem": "mountOptions: []"}))
		groupPV := nfsBoundPV(ctx, "fs-group")
		createdPVs = append(createdPVs, groupPV)
		nfsApply(ctx, nfsPodManifest("fs-group-writer", "fs-group", workers[1], false, 1000, 2000))
		nfsWaitPod(ctx, "fs-group-writer")
		Expect(nfsPodExec(ctx, "fs-group-writer", "printf fs-group-data > /data/fs-group; sync; stat -c '%u:%g' /data/fs-group")).To(Equal("1000:2000"))
		Expect(nfsPodExec(ctx, "fs-group-writer", "cat /data/fs-group")).To(Equal("fs-group-data"))
		mount := nfsPodExec(ctx, "fs-group-writer", "awk '$2 == \"/data\" {print}' /proc/mounts")
		Expect(mount).To(ContainSubstring("vers=4.2"))
		Expect(mount).To(ContainSubstring("hard"))
		Expect(mount).To(ContainSubstring("proto=tcp"))
		Expect(mount).NotTo(ContainSubstring("noatime"))
		nfsApply(ctx, nfsPVCManifest("root-default", nfsRootClass, "ReadWriteMany", "Filesystem", nil))
		rootPV := nfsBoundPV(ctx, "root-default")
		createdPVs = append(createdPVs, rootPV)
		nfsApply(ctx, nfsPodManifest("root-default", "root-default", workers[1], false, 0, 0))
		nfsWaitPod(ctx, "root-default")
		_, err := nfsKubectl(ctx, "", "-n", nfsNamespace, "exec", "root-default", "--", "sh", "-c", "printf unexpected-root > /data/root-write")
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("permission denied"))
	})
	It("[TC-E37.8] recovers NFS filehandles after the agent pod restarts", func() {
		nfsPodExec(ctx, "writer", `nohup sh -c 'exec 4>>/data/held-handle; while :; do printf "tick\n" >&4; sleep 1; done' >/tmp/held-fd.log 2>&1 & echo $! >/tmp/held-fd.pid`)
		Eventually(func() int {
			n, err := strconv.Atoi(nfsPodExec(ctx, "writer", "wc -l < /data/held-handle"))
			Expect(err).NotTo(HaveOccurred())
			return n
		}, 20*time.Second, time.Second).Should(BeNumerically(">", 1))
		before := nfsPodExec(ctx, "writer", "wc -l < /data/held-handle")
		beforePolicies := nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), suiteNFSDatasetRoot+"/"+dataset)
		Expect(beforePolicies).NotTo(BeEmpty())
		agentPod := nfsComponentPod(ctx, "agent", backend)
		oldUID := nfsMust(ctx, "-n", resolveHelmNamespace(), "get", "pod", agentPod, "-o", "jsonpath={.metadata.uid}")
		nfsMust(ctx, "-n", resolveHelmNamespace(), "delete", "pod", agentPod, "--wait=true", "--timeout=90s")
		Eventually(func() string {
			pods, err := nfsKubectl(ctx, "", "-n", resolveHelmNamespace(), "get", "pods", "-l", "app.kubernetes.io/component=agent", "-o", "jsonpath={.items[0].metadata.uid}")
			if err != nil {
				return ""
			}
			return pods
		}, 2*time.Minute, 2*time.Second).Should(And(Not(BeEmpty()), Not(Equal(oldUID))))
		nfsMust(ctx, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-agent", "--timeout=3m")
		afterPolicies := nfsExportPolicies(nfsAgentExec(ctx, "exportfs", "-v"), suiteNFSDatasetRoot+"/"+dataset)
		Expect(afterPolicies).To(Equal(beforePolicies), "restart must restore identical fsid, clients, and export policy")
		Expect(nfsPodExec(ctx, "writer", "cat /data/survivor")).To(Equal("survivor-data"))
		beforeN, err := strconv.Atoi(before)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int {
			n, err := strconv.Atoi(nfsPodExec(ctx, "writer", "wc -l < /data/held-handle"))
			Expect(err).NotTo(HaveOccurred())
			return n
		}, 90*time.Second, 2*time.Second).Should(BeNumerically(">", beforeN))
		nfsPodExec(ctx, "writer", "kill $(cat /tmp/held-fd.pid)")
		// Restart the client plugin while its workload remains mounted. Cleanup
		// below must use the durable NFS stage state, not an in-memory mapper.
		nodePod := nfsComponentPod(ctx, "node", workers[0])
		nfsMust(ctx, "-n", resolveHelmNamespace(), "delete", "pod", nodePod, "--wait=true", "--timeout=90s")
		nfsMust(ctx, "-n", resolveHelmNamespace(), "rollout", "status", "daemonset/pillar-csi-node", "--timeout=3m")
		Expect(nfsPodExec(ctx, "writer", "cat /data/private-marker")).To(Equal("private-marker"))
	})
	It("[TC-E37.9] rejects Block volumes and forbidden filesystem options for NFS", func() {
		nfsRejectedPVC(ctx, "bad-block", nfsClass, "Block", nil)
		for i, doc := range []string{"fsType: ext4", "fsType: xfs", "mkfsOptions: [-F]", "periodicTrim: true"} {
			nfsRejectedPVC(ctx, fmt.Sprintf("bad-options-%d", i), nfsClass, "Filesystem", map[string]string{"pillar-csi.bhyoo.com/filesystem": doc})
		}
		for i, doc := range []string{"mountOptions: [soft]", "mountOptions: [vers=3]", "mountOptions: [nfsvers=4.1]", "mountOptions: [proto=udp]"} {
			name := fmt.Sprintf("bad-mount-%d", i)
			nfsApply(ctx, nfsPVCManifest(name, nfsClass, "ReadWriteMany", "Filesystem", map[string]string{"pillar-csi.bhyoo.com/filesystem": doc}))
			badPV := nfsBoundPV(ctx, name)
			createdPVs = append(createdPVs, badPV)
			nfsApply(ctx, nfsPodManifest(name, name, workers[1], false, 0, 0))
			Eventually(func() string {
				return nfsMust(ctx, "-n", nfsNamespace, "get", "events", "--field-selector", "involvedObject.name="+name+",reason=FailedMount", "-o", "jsonpath={.items[*].message}")
			}, 90*time.Second, 2*time.Second).Should(ContainSubstring("InvalidArgument"))
			Expect(nfsMust(ctx, "-n", nfsNamespace, "get", "pod", name, "-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")).NotTo(Equal("True"))
			nfsDeleteWorkload(ctx, "pod/"+name, "pvc/"+name)
			nfsWaitDeleted(ctx, "pv/"+badPV)
		}
		nfsRejectedPVC(ctx, "bad-protocol", nfsClass, "Filesystem", map[string]string{"pillar-csi.bhyoo.com/protocol": "nfs: {version: '3'}"})
	})
	It("[TC-E37.10] rejects an incompatible backend/protocol binding", func() {
		for _, backendConfig := range []struct{ name, config string }{{"pillar-e37-zvol", fmt.Sprintf("zfs:\n      pool: %s\n      parentDataset: %s\n      volumeType: zvol", pool, parent)}, {"pillar-e37-lvm", fmt.Sprintf("lvm:\n      volumeGroup: %s", os.Getenv(suiteLVMVGEnvVar))}} {
			nfsApply(ctx, fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarStore\nmetadata:\n  name: %s\nspec:\n  agentRef: %s\n  backend:\n    %s\n", backendConfig.name, nfsAgent, backendConfig.config))
			class := backendConfig.name + "-nfs"
			nfsApply(ctx, fmt.Sprintf("apiVersion: storage.k8s.io/v1\nkind: StorageClass\nmetadata:\n  name: %s\nprovisioner: pillar-csi.bhyoo.com\nparameters:\n  pillar-csi.bhyoo.com/store-ref: %s\n  pillar-csi.bhyoo.com/protocol-ref: %s\n", class, backendConfig.name, nfsProtocol))
			nfsRejectedPVC(ctx, backendConfig.name, class, "Filesystem", nil)
		}
		badLocal := nfsBinding("pillar-e37-local", nfsStore, nfsProtocol, "  localAttach: true\n")
		_, err := nfsKubectl(ctx, badLocal, "apply", "--dry-run=server", "-f", "-")
		if err == nil {
			nfsApply(ctx, badLocal)
			Eventually(func() string {
				return nfsMust(ctx, "get", "pillarstorageclass", "pillar-e37-local", "-o", "jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
			}, 60*time.Second, 2*time.Second).Should(Equal("False"))
		} else {
			Expect(strings.ToLower(err.Error())).To(ContainSubstring("localattach"))
		}
	})
	It("[TC-E37.11] records exact export and publication state while mounted", func() {
		exports := nfsAgentExec(ctx, "exportfs", "-v")
		policies := nfsExportPolicies(exports, suiteNFSDatasetRoot+"/"+dataset)
		Expect(nfsPublicationNodes(ctx, pv)).To(ConsistOf(workers[0]))
		ip := nfsMust(ctx, "get", "node", workers[0], "-o", "jsonpath={.status.addresses[?(@.type==\"InternalIP\")].address}")
		Expect(policies).To(HaveLen(1))
		Expect(policies).To(HaveKey(ip))
		Expect(policies).NotTo(HaveKey("*"))
		Expect(policies[ip]).To(ContainElements("rw", "no_root_squash", "sec=sys"))
		Expect(policies[ip]).To(ContainElement(HavePrefix("fsid=")))
		AddReportEntry("exact private export policy", policies)
		volumeHandle := nfsMust(ctx, "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeHandle}")
		stageFile = "/var/lib/pillar-csi/node/" + strings.ReplaceAll(volumeHandle, "/", "_") + ".json"
		stageJSON, err := kindContainerExec(ctx, workers[0], "cat", stageFile)
		Expect(err).NotTo(HaveOccurred())
		var staged struct {
			ProtocolType string `json:"protocol_type"`
			FsType       string `json:"fs_type"`
			StagingPath  string `json:"staging_path"`
			NFS          struct {
				MountSource string `json:"mount_source"`
			} `json:"nfs"`
		}
		Expect(json.Unmarshal([]byte(stageJSON), &staged)).To(Succeed())
		Expect(staged.ProtocolType).To(Equal("nfs"))
		Expect(staged.FsType).To(Equal("nfs"))
		Expect(staged.StagingPath).NotTo(BeEmpty())
		Expect(staged.NFS.MountSource).To(Equal(target))
		AddReportEntry("durable NFS stage identity", staged)
		Expect(nfsPodExec(ctx, "writer", "cat /data/from-writer; cat /data/from-reader")).To(Equal("writer-datareader-data"))
	})
	It("[TC-E37.13] keeps dataset RWX and NVMe zvol I/O independent in the same ZFS pool", func() {
		nfsApply(ctx, fmt.Sprintf("apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarStore\nmetadata:\n  name: pillar-e37-zvol\nspec:\n  agentRef: %s\n  backend:\n    zfs:\n      pool: %s\n      parentDataset: %s\n      volumeType: zvol\n", nfsAgent, pool, parent))
		nfsApply(ctx, "apiVersion: pillar-csi.bhyoo.com/v1alpha1\nkind: PillarProtocol\nmetadata:\n  name: pillar-e37-nvme\nspec:\n  protocol:\n    nvmeofTcp:\n      port: 4442\n      acl: true\n")
		nfsApply(ctx, nfsBinding("pillar-e37-zvol-block", "pillar-e37-zvol", "pillar-e37-nvme", "  filesystem:\n    fsType: ext4\n"))
		nfsWaitReady(ctx, "pillarstorageclass/pillar-e37-zvol-block")
		nfsApply(ctx, nfsPVCManifest("block-peer", "pillar-e37-zvol-block", "ReadWriteOnce", "Filesystem", nil))
		blockPV := nfsBoundPV(ctx, "block-peer")
		createdPVs = append(createdPVs, blockPV)
		nfsApply(ctx, nfsPodManifest("block-writer", "block-peer", workers[1], false, 0, 0))
		nfsWaitPod(ctx, "block-writer")
		Expect(nfsMust(ctx, "get", "pv", blockPV, "-o", "jsonpath={.spec.csi.fsType}")).To(Equal("ext4"))
		nfsPodExec(ctx, "block-writer", "printf zvol-independent > /data/zvol-proof; sync")
		Expect(nfsPodExec(ctx, "block-writer", "cat /data/zvol-proof")).To(Equal("zvol-independent"))
		nfsPodExec(ctx, "writer", "printf dataset-independent > /data/coexistence; sync")
		Expect(nfsPodExec(ctx, "writer", "cat /data/coexistence; cat /data/private-marker")).To(Equal("dataset-independentprivate-marker"))
		Expect(nfsAgentExec(ctx, "zfs", "get", "-H", "-o", "value", "type", dataset)).To(Equal("filesystem"))
		AddReportEntry("same-pool coexistence", nfsAgentExec(ctx, "zfs", "list", "-H", "-o", "name,type", "-r", pool+"/"+parent))
		nfsDeleteWorkload(ctx, "pod/block-writer", "pvc/block-peer")
		nfsWaitDeleted(ctx, "pv/"+blockPV)
		nfsMust(ctx, "delete", "pillarstorageclass", "pillar-e37-zvol-block", "--wait=true", "--timeout=60s")
		nfsMust(ctx, "delete", "pillarprotocol", "pillar-e37-nvme", "--wait=true", "--timeout=60s")
	})
	It("[TC-E37.12] removes dataset, export, mount, and publication state on cleanup", func() {
		nfsDeleteWorkload(ctx, "pod/writer", "pod/public-writer", "pod/fs-group-writer", "pod/rox-reader", "pod/readonly-rwx", "pod/root-default")
		Eventually(func() []string { return nfsPublicationNodes(ctx, pv) }, 90*time.Second, 2*time.Second).Should(BeEmpty())
		for _, node := range workers {
			mounts, err := kindContainerExec(ctx, node, "cat", "/proc/1/mountinfo")
			Expect(err).NotTo(HaveOccurred())
			Expect(mounts).NotTo(ContainSubstring(address + ":/" + dataset))
		}
		Eventually(func() error { _, err := kindContainerExec(ctx, workers[0], "test", "!", "-e", stageFile); return err }, 90*time.Second, 2*time.Second).Should(Succeed(), "plugin restart must not leak the durable stage record")
		nfsDeleteWorkload(ctx, "pvc/"+nfsPVC, "pvc/public", "pvc/fs-group", "pvc/rox", "pvc/root-default")
		for _, name := range createdPVs {
			nfsWaitDeleted(ctx, "pv/"+name)
			nfsWaitDeleted(ctx, "pillarvolumestate/"+name)
		}
		children := nfsAgentExec(ctx, "zfs", "list", "-H", "-o", "name", "-r", pool+"/"+parent)
		Expect(children).To(Equal(pool + "/" + parent))
		exports := nfsAgentExec(ctx, "exportfs", "-v")
		Expect(exports).NotTo(ContainSubstring(suiteNFSDatasetRoot + "/" + dataset))
		owner := nfsAgentExec(ctx, "/bin/busybox", "cat", "/var/lib/pillar-csi/agent/nfs/owner.json")
		var state struct {
			Desired map[string]json.RawMessage `json:"desired"`
		}
		Expect(json.Unmarshal([]byte(owner), &state)).To(Succeed())
		Expect(state.Desired).To(HaveKey("::pillar-nfs-root::"))
		Expect(state.Desired).To(HaveLen(1))
		var root struct {
			Active bool `json:"active"`
		}
		Expect(json.Unmarshal(state.Desired["::pillar-nfs-root::"], &root)).To(Succeed())
		Expect(root.Active).To(BeFalse())
		AddReportEntry("cleanup evidence", fmt.Sprintf("datasets=%s; exports=%s; durable desired=%d", children, exports, len(state.Desired)))
	})
})
