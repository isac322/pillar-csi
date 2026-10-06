//go:build e2e && e2e_helm

package e2e

// tc_e38_lvm_adoption_e2e_test.go — E38: adopting pre-existing LVM logical
// volumes through the pillar-csi.bhyoo.com/import-lv PVC annotation on a real
// multi-node Kind cluster with the Helm-installed pillar-csi images.
//
// Every LV is created, formatted and filled on the storage node outside
// pillar-csi.  The cases prove, through real agent RPCs, Kubernetes objects
// and data read back by Pods on remote nodes, that:
//   - linear and thin LVs are adopted under the default PreserveOriginal
//     policy with their UUIDs, size, thin pool and data intact;
//   - wrong-UUID, in-use, already-owned and blank LVs are refused or never
//     formatted, leaving the LV bytes and the agent fence marks unchanged;
//   - InspectVolume is a read-only observation whose fence record the test
//     correlates with surviving PillarVolumeStates (claimed / absent /
//     unknown) and which changes no LVM metadata, mark file or mount;
//   - a preserving Delete only releases the lifecycle, and stale or new
//     lifecycle Delete/Expand/Create calls cannot destroy or resize the LV;
//   - a Retain claim's PV is re-bound to a static claim through the
//     documented retained-PV rebind runbook (controller quiescence, fenced
//     unexport, read-only probe, resourceVersion-guarded PV replace): the live workload,
//     volume attachments, publications and a competing import block the
//     runbook before any bind, and the rebind creates no PillarVolumeState.
//
// Prerequisites (each is gated with Fail, never Skip):
//   - Multi-node Kind cluster (KUBECONFIG or the suite cluster) with at least
//     two Ready, schedulable nodes running the pillar-csi node plugin.
//   - pillar-csi installed by its Helm chart with two LVM agent backends on
//     the storage node: agent.backends[].lvm {volumeGroup: <linear VG>}
//     without thinPool, and agent.backends[].lvm {volumeGroup: <thin VG>,
//     thinPool: <pool>}.  Both are discovered from PillarAgent
//     status.discoveredPools.  mTLS disabled (chart default) so the test can
//     call the agent's gRPC API through kubectl port-forward.
//   - PILLAR_E2E_BACKEND_CONTAINER: Docker container of the Kind storage
//     node (its name is also the Kubernetes node name).
//   - The storage node container has lvm2, mkfs.ext4, mount, mountpoint,
//     blkid, dd, sha256sum, stat and find, and sees the LV block devices
//     under /dev/<vg>/<lv>.
//   - NVMe-oF TCP target/host kernel modules loaded on the host.
//
// TC IDs covered: E38.1 – E38.17 (E38.1 subsection)
//
// Run with: go test -tags=e2e,e2e_helm ./test/e2e/ --ginkgo.label-filter="e38"

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	pillarv1 "github.com/isac322/pillar-csi/api/v1alpha1"
	agentv1 "github.com/isac322/pillar-csi/gen/go/pillar_csi/agent/v1"
)

const (
	// e38LVSize is the size of every LV the TCs create (lvcreate -L/-V).
	e38LVSize = "64m"
	// e38LVBytes is e38LVSize in bytes; an adopted PV's capacity equals it.
	e38LVBytes = int64(64 << 20)
	// e38PVCRequest is the claim size; import accepts any LV >= request.
	e38PVCRequest = "32Mi"
	// e38ExpandTo is a resize request above the LV size.
	e38ExpandTo = "128Mi"
	// e38ProofFile is written on the storage node before the import.
	e38ProofFile = "e38-import-proof.bin"
	// e38AgentStateDir is the chart's agent state hostPath on the storage
	// node; the agent keeps its durable fence marks below it.
	e38AgentStateDir = "/var/lib/pillar-csi/agent"
	// e38AnnImportLV is the import-lv PVC annotation (wire contract).
	e38AnnImportLV = "pillar-csi.bhyoo.com/import-lv"
	// e38VCPreserve is the PV volumeAttributes key of a preserving adoption.
	e38VCPreserve = "pillar-csi.bhyoo.com/preserve-original"
	// e38FakeLVUUID is a well-formed LV UUID no LV carries.
	e38FakeLVUUID = "E38WRO-NGLV-UUID-0000-0000-0000-000000"
	// e38StaleUIDPrefix names lifecycle UIDs this test invents for direct
	// agent calls; they never belong to a PillarVolumeState.
	e38StaleUIDPrefix = "e38-direct-"
)

func e38DirectFence(key string) *agentv1.FencingToken {
	return &agentv1.FencingToken{
		VolumeUid:  e38StaleUIDPrefix + key + "@" + agentFenceRunNonce,
		Generation: 1,
	}
}

// e38ForwardRE matches kubectl port-forward's "Forwarding from" line.
var e38ForwardRE = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+) ->`)

// e38LV is one row of `lvs --reportformat json`.
type e38LV struct {
	VGName  string `json:"vg_name"`
	LVName  string `json:"lv_name"`
	VGUUID  string `json:"vg_uuid"`
	LVUUID  string `json:"lv_uuid"`
	Attr    string `json:"lv_attr"`
	Segtype string `json:"segtype"`
	Pool    string `json:"pool_lv"`
	Origin  string `json:"origin"`
	Size    string `json:"lv_size"`
}

// identity is everything about an LV that adoption, release and inspection
// must preserve.  lv_attr is excluded: its open flag follows consumers.
func (r e38LV) identity() string {
	return strings.Join([]string{r.VGName, r.LVName, r.VGUUID, r.LVUUID,
		r.Segtype, r.Pool, r.Origin, r.Size}, "|")
}

// e38Fixture is an LV this test created outside pillar-csi.
type e38Fixture struct {
	vg, lv, thinPool string
	// row is the LV as observed right after it was prepared.
	row e38LV
	// proofSHA is the sha256 of the proof file; empty for a blank LV.
	proofSHA string
	// fsUUID is the ext4 UUID from blkid; empty for a blank LV.
	fsUUID string
}

func (f *e38Fixture) volumeID() string { return f.vg + "/" + f.lv }
func (f *e38Fixture) device() string   { return "/dev/" + f.vg + "/" + f.lv }

// importValue is the import-lv annotation value naming f.
func (f *e38Fixture) importValue() string {
	return fmt.Sprintf("%s/%s:%s:%s", f.vg, f.lv, f.row.VGUUID, f.row.LVUUID)
}

// e38FailIfNoInfra fails the spec when the E38 environment is incomplete.
func e38FailIfNoInfra() {
	if os.Getenv(suiteBackendContainerEnvVar) == "" {
		Fail("[E38] MISSING PREREQUISITE: " + suiteBackendContainerEnvVar + " not set.\n" +
			"  It must name the Docker container of the Kind storage node whose LVM\n" +
			"  volume groups the Helm-deployed agent serves (agent.backends[].lvm).")
	}
	if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
		Fail("[E38] MISSING PREREQUISITE: no Kind cluster available (KUBECONFIG unset).")
	}
}

// e38Kubeconfig resolves the kubeconfig the same way e36Kubectl does.
func e38Kubeconfig() (string, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" && suiteKindCluster != nil {
		path = suiteKindCluster.KubeconfigPath
	}
	if path == "" {
		return "", fmt.Errorf("[E38] KUBECONFIG not set — Kind cluster not bootstrapped")
	}
	return path, nil
}

// e38LastLine returns the last non-empty line of out.
func e38LastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ─────────────────────────────────────────────────────────────────────────────
// Storage-node helpers (docker exec into the Kind storage node)
// ─────────────────────────────────────────────────────────────────────────────

// e38LVRow reads vg/lv with one lvs JSON report.
func e38LVRow(ctx context.Context, node, vg, lv string) (e38LV, error) {
	out, err := kindContainerExec(ctx, node, "lvs", "--reportformat", "json",
		"--units", "b", "--nosuffix",
		"-o", "vg_name,lv_name,vg_uuid,lv_uuid,lv_attr,segtype,pool_lv,origin,lv_size",
		vg+"/"+lv)
	if err != nil {
		return e38LV{}, err
	}
	var doc struct {
		Report []struct {
			LV []e38LV `json:"lv"`
		} `json:"report"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return e38LV{}, fmt.Errorf("decode lvs %s/%s: %w\n%s", vg, lv, err, out)
	}
	if len(doc.Report) != 1 || len(doc.Report[0].LV) != 1 {
		return e38LV{}, fmt.Errorf("lvs %s/%s: want exactly one LV row, got %s", vg, lv, out)
	}
	return doc.Report[0].LV[0], nil
}

// e38CreateLV creates a linear LV (thinPool == "") or a thin LV in thinPool
// and waits for its block device to appear on the storage node.
func e38CreateLV(ctx context.Context, node, vg, lv, thinPool string) error {
	args := []string{"lvcreate", "-y", "-n", lv}
	if thinPool == "" {
		args = append(args, "-L", e38LVSize, vg)
	} else {
		args = append(args, "-V", e38LVSize, "-T", vg+"/"+thinPool)
	}
	if _, err := kindContainerExec(ctx, node, args...); err != nil {
		return fmt.Errorf("create LV %s/%s: %w", vg, lv, err)
	}
	dev := "/dev/" + vg + "/" + lv
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := e36NodeSh(ctx, node, "test -b "+dev); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("block device %s did not appear in node container %s within 60s "+
				"(the Kind node must see host /dev, e.g. extraMounts hostPath /dev)", dev, node)
		}
		time.Sleep(time.Second)
	}
}

// e38RemoveLV removes vg/lv when it still exists.
func e38RemoveLV(ctx context.Context, node, vg, lv string) error {
	_, err := e36NodeSh(ctx, node, fmt.Sprintf(
		"if lvs %[1]s >/dev/null 2>&1; then lvremove -y %[1]s; fi", vg+"/"+lv))
	return err
}

// e38FormatAndFill formats dev with ext4, writes the proof file through a
// temporary mount at mnt, unmounts it and returns the proof sha256.
func e38FormatAndFill(ctx context.Context, node, dev, mnt string) (string, error) {
	script := fmt.Sprintf(`set -e
mkfs.ext4 -q -F -E lazy_itable_init=0,lazy_journal_init=0 %[1]s >/dev/null
mkdir -p %[2]s
mount -t ext4 %[1]s %[2]s
if head -c 4194304 /dev/urandom > %[2]s/%[3]s && sync; then
  sha=$(sha256sum %[2]s/%[3]s | cut -d' ' -f1)
fi
umount %[2]s
rmdir %[2]s
echo "$sha"`, dev, mnt, e38ProofFile)
	out, err := e36NodeSh(ctx, node, script)
	if err != nil {
		return "", fmt.Errorf("format/fill %s at %s: %w", dev, mnt, err)
	}
	return e38LastLine(out), nil
}

// e38ReadProofRO mounts dev read-only without journal replay (ro,noload),
// returns the proof sha256 and unmounts it again.
func e38ReadProofRO(ctx context.Context, node, dev, mnt string) (string, error) {
	script := fmt.Sprintf(`set -e
mkdir -p %[2]s
mount -t ext4 -o ro,noload %[1]s %[2]s
sha=$(sha256sum %[2]s/%[3]s | cut -d' ' -f1)
umount %[2]s
rmdir %[2]s
echo "$sha"`, dev, mnt, e38ProofFile)
	out, err := e36NodeSh(ctx, node, script)
	if err != nil {
		return "", fmt.Errorf("read-only proof read of %s: %w", dev, err)
	}
	return e38LastLine(out), nil
}

// e38DeviceSHA hashes every byte of dev.
func e38DeviceSHA(ctx context.Context, node, dev string) (string, error) {
	out, err := e36NodeSh(ctx, node, fmt.Sprintf(
		"dd if=%s bs=1M status=none | sha256sum | cut -d' ' -f1", dev))
	if err != nil {
		return "", err
	}
	return e38LastLine(out), nil
}

// e38FSProbe returns the low-level blkid TYPE and UUID of dev ("" when the
// device carries no signature; blkid -p exits 2 then).
func e38FSProbe(ctx context.Context, node, dev string) (fsType, fsUUID string, err error) {
	out, err := e36NodeSh(ctx, node, fmt.Sprintf("blkid -p -o export %s || [ $? -eq 2 ]", dev))
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "TYPE":
			fsType = value
		case "UUID":
			fsUUID = value
		}
	}
	return fsType, fsUUID, nil
}

// e38MarkFiles lists every file below the agent state dir whose name carries
// lv (unique per run), with its mtime, size and sha256.  It treats the state
// dir as opaque: an absent mark is an empty list.
func e38MarkFiles(ctx context.Context, node, lv string) (string, error) {
	return e36NodeSh(ctx, node, fmt.Sprintf(
		`find %[1]s -type f -name '*%[2]s*' 2>/dev/null | sort | while read -r f; do
  echo "$f $(stat -c '%%y %%s' "$f") $(sha256sum < "$f" | cut -d' ' -f1)"
done`, e38AgentStateDir, lv))
}

// e38StorageSnapshot captures what a read-only inspection must never change:
// the VG metadata sequence number, the LV attributes and activation, the
// mounts and holders of the LV's kernel device and the agent mark files.
func e38StorageSnapshot(ctx context.Context, node, vg, lv string) (string, error) {
	out, err := e36NodeSh(ctx, node, fmt.Sprintf(`set -e
dev=$(lvs --noheadings -o lv_kernel_major,lv_kernel_minor %[1]s/%[2]s | awk '{print $1":"$2}')
echo "seqno=$(vgs --noheadings -o vg_seqno %[1]s | tr -d ' ')"
echo "lv=$(lvs --noheadings -o lv_attr,lv_active %[1]s/%[2]s | tr -s ' ')"
echo "dev=$dev"
echo "mounts:"
awk -v d="$dev" '$3 == d {print $5}' /proc/self/mountinfo | sort
echo "holders:"
ls /sys/dev/block/$dev/holders 2>/dev/null | sort
echo "marks:"`, vg, lv))
	if err != nil {
		return "", err
	}
	marks, err := e38MarkFiles(ctx, node, lv)
	if err != nil {
		return "", err
	}
	return out + "\n" + marks, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Kubernetes helpers
// ─────────────────────────────────────────────────────────────────────────────

// e38PVCManifest renders a PVC adopting the LV named by importValue under
// the default policy (no import-lv-policy annotation = PreserveOriginal).
func e38PVCManifest(name, namespace, storageClass, importValue string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  annotations:
    %s: %q
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, e38AnnImportLV, importValue, storageClass, e38PVCRequest)
}

// e38StaticPVCManifest renders a claim statically bound to pv.
func e38StaticPVCManifest(name, namespace, storageClass, pv string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: %s
  volumeName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, storageClass, pv, e38PVCRequest)
}

// e38GetJSON runs `kubectl get ... --ignore-not-found -o json` and decodes
// into out.  It reports false when the object does not exist.
func e38GetJSON(ctx context.Context, out any, args ...string) (bool, error) {
	args = append(append([]string{"get"}, args...), "--ignore-not-found=true", "-o", "json")
	raw, err := e36Kubectl(ctx, "", args...)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return false, fmt.Errorf("decode kubectl %s: %w", strings.Join(args, " "), err)
	}
	return true, nil
}

func e38GetPV(ctx context.Context, name string) (*corev1.PersistentVolume, error) {
	var pv corev1.PersistentVolume
	found, err := e38GetJSON(ctx, &pv, "pv", name)
	if err != nil || !found {
		return nil, err
	}
	return &pv, nil
}

func e38GetPVC(ctx context.Context, namespace, name string) (*corev1.PersistentVolumeClaim, error) {
	var pvc corev1.PersistentVolumeClaim
	found, err := e38GetJSON(ctx, &pvc, "pvc", name, "-n", namespace)
	if err != nil || !found {
		return nil, err
	}
	return &pvc, nil
}

func e38GetPVS(ctx context.Context, name string) (*pillarv1.PillarVolumeState, error) {
	var pvs pillarv1.PillarVolumeState
	found, err := e38GetJSON(ctx, &pvs, "pillarvolumestate", name)
	if err != nil || !found {
		return nil, err
	}
	return &pvs, nil
}

func e38ListPVS(ctx context.Context) ([]pillarv1.PillarVolumeState, error) {
	var list pillarv1.PillarVolumeStateList
	if _, err := e38GetJSON(ctx, &list, "pillarvolumestates"); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// e38PVSFor returns the PillarVolumeStates of agentName whose agent volume
// ID is volumeID.
func e38PVSFor(ctx context.Context, agentName, volumeID string) ([]pillarv1.PillarVolumeState, error) {
	items, err := e38ListPVS(ctx)
	if err != nil {
		return nil, err
	}
	var out []pillarv1.PillarVolumeState
	for _, p := range items {
		if p.Spec.AgentRef == agentName && p.Spec.AgentVolumeID == volumeID {
			out = append(out, p)
		}
	}
	return out, nil
}

// e38Events returns the messages of the events of reason on the object.
func e38Events(ctx context.Context, namespace, kind, name, reason string) (string, error) {
	return e36Kubectl(ctx, "", "get", "events", "-n", namespace,
		"--field-selector",
		"involvedObject.kind="+kind+",involvedObject.name="+name+",reason="+reason,
		"-o", `jsonpath={range .items[*]}{.message}{"\n"}{end}`)
}

// e38WaitBound waits for pvc to bind and returns its PV name.
func e38WaitBound(ctx context.Context, tc, namespace, pvc string) string {
	Eventually(func(g Gomega) {
		phase, err := e36Kubectl(ctx, "", "get", "pvc", pvc, "-n", namespace,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Equal("Bound"))
	}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"[%s] PVC %s must become Bound", tc, pvc)
	pv, err := e36Kubectl(ctx, "", "get", "pvc", pvc, "-n", namespace,
		"-o", "jsonpath={.spec.volumeName}")
	Expect(err).NotTo(HaveOccurred(), "[%s] get PV name of %s", tc, pvc)
	Expect(pv).NotTo(BeEmpty(), "[%s] bound PVC %s must name its PV", tc, pvc)
	return pv
}

// e38DeletePVC deletes pvc and waits until it is gone.
func e38DeletePVC(ctx context.Context, namespace, pvc string) error {
	_, err := e36Kubectl(ctx, "", "delete", "pvc", pvc, "-n", namespace,
		"--ignore-not-found=true", "--wait=true", "--timeout=120s")
	return err
}

// e38WaitVolumeGone waits until the PV and its PillarVolumeState are gone.
func e38WaitVolumeGone(ctx context.Context, tc, pvName string) {
	Eventually(func(g Gomega) {
		pv, err := e38GetPV(ctx, pvName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pv).To(BeNil(), "PV %s still exists", pvName)
		pvs, err := e38GetPVS(ctx, pvName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pvs).To(BeNil(), "PillarVolumeState %s still exists", pvName)
	}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"[%s] PV and PillarVolumeState %s must be removed", tc, pvName)
}

// e38PodSHA returns the proof sha256 as read inside pod.
func e38PodSHA(ctx context.Context, namespace, pod string) (string, error) {
	out, err := e36Kubectl(ctx, "", "exec", "-n", namespace, pod, "--",
		"sha256sum", "/data/"+e38ProofFile)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty sha256sum output from pod %s", pod)
	}
	return fields[0], nil
}

// e38RunPodAndVerify starts a Pod on node mounting claim, waits for Running
// and asserts the proof sha256 equals want.
func e38RunPodAndVerify(tc, namespace, pod, node, claim, want string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	By(fmt.Sprintf("starting Pod %s on node %s", pod, node))
	Expect(e36Apply(ctx, e36PodManifest(pod, namespace, node, claim))).
		To(Succeed(), "[%s] apply Pod", tc)
	Eventually(func(g Gomega) {
		phase, err := e36Kubectl(ctx, "", "get", "pod", pod, "-n", namespace,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Equal("Running"))
	}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"[%s] Pod %s on %s must reach Running", tc, pod, node)

	By("reading the pre-import file inside the Pod")
	got, err := e38PodSHA(ctx, namespace, pod)
	Expect(err).NotTo(HaveOccurred(), "[%s] sha256sum inside Pod", tc)
	Expect(got).To(Equal(want),
		"[%s] the file written before the import must be unchanged (the LV must not be reformatted)", tc)
}

// e38ExpectRefused creates an import-lv PVC and asserts it stays Pending and
// unbound with at least one ProvisioningFailed event.  The event messages are
// logged as diagnostics only; the refusal is proven by the event reason, the
// claim state and the caller's storage/fence checks.  whilePending runs while
// the refused claim still exists, before its deletion lets the controller
// retire the abandoned attempt.  The PVC is deleted when the spec ends.
func e38ExpectRefused(tc, namespace, storageClass, pvc, importValue string, whilePending func()) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	DeferCleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer ccancel()
		Expect(e38DeletePVC(cctx, namespace, pvc)).To(Succeed(), "[%s] delete refused PVC", tc)
	})

	By("creating the import-lv PVC " + pvc)
	Expect(e36Apply(ctx, e38PVCManifest(pvc, namespace, storageClass, importValue))).
		To(Succeed(), "[%s] apply PVC", tc)

	By("waiting for a ProvisioningFailed event")
	Eventually(func(g Gomega) {
		msgs, err := e38Events(ctx, namespace, "PersistentVolumeClaim", pvc, "ProvisioningFailed")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(msgs).NotTo(BeEmpty())
		AddReportEntry(fmt.Sprintf("%s ProvisioningFailed %s", tc, pvc), msgs)
	}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"[%s] the refused PVC must record a ProvisioningFailed event", tc)

	claim, err := e38GetPVC(ctx, namespace, pvc)
	Expect(err).NotTo(HaveOccurred(), "[%s] get refused PVC", tc)
	Expect(claim).NotTo(BeNil(), "[%s] refused PVC must exist", tc)
	Expect(claim.Status.Phase).To(Equal(corev1.ClaimPending), "[%s] refused PVC must stay Pending", tc)
	Expect(claim.Spec.VolumeName).To(BeEmpty(), "[%s] refused PVC must not be bound to a PV", tc)

	if whilePending != nil {
		whilePending()
	}
}

// e38DirectImport sends the agent the same LVM ImportVolume the controller
// builds for src under a lifecycle no PillarVolumeState owns.  An import
// refusal persists nothing, so callers assert the gRPC code and that the
// storage and fence state stayed unchanged.
func e38DirectImport(ctx context.Context, client agentv1.AgentServiceClient, vg, lv, vgUUID, lvUUID, key string) error {
	_, err := client.ImportVolume(ctx, &agentv1.ImportVolumeRequest{
		VolumeId:      vg + "/" + lv,
		CapacityBytes: e38LVBytes / 2,
		BackendType:   agentv1.BackendType_BACKEND_TYPE_LVM,
		Fence:         e38DirectFence(key),
		ExpectedLvmSource: &agentv1.LvmSourceIdentity{
			VolumeGroup:       vg,
			LogicalVolume:     lv,
			VolumeGroupUuid:   vgUUID,
			LogicalVolumeUuid: lvUUID,
		},
		PreserveOriginal: proto.Bool(true),
	})
	return err
}

// e38AttachmentsFor names the VolumeAttachments of pv.
func e38AttachmentsFor(ctx context.Context, pv string) ([]string, error) {
	var list storagev1.VolumeAttachmentList
	if _, err := e38GetJSON(ctx, &list, "volumeattachments"); err != nil {
		return nil, err
	}
	var names []string
	for _, va := range list.Items {
		if src := va.Spec.Source.PersistentVolumeName; src != nil && *src == pv {
			names = append(names, va.Name)
		}
	}
	return names, nil
}

// e38NodeMountsOf returns node mountinfo lines naming pv's publish path
// (contains the PV name) or kubelet's CSI staging path (contains the
// sha256 of the volume handle).
func e38NodeMountsOf(ctx context.Context, node, pv, volumeHandle string) (string, error) {
	sum := sha256.Sum256([]byte(volumeHandle))
	return e36NodeSh(ctx, node, fmt.Sprintf("grep -F -e %q -e %q /proc/self/mountinfo || true",
		pv, hex.EncodeToString(sum[:])))
}

// e38ControllerDeployment returns the Helm controller Deployment and its
// configured replica count.
func e38ControllerDeployment(ctx context.Context) (string, int32, error) {
	var list appsv1.DeploymentList
	if _, err := e38GetJSON(ctx, &list, "deployments", "-n", resolveHelmNamespace(),
		"-l", "app.kubernetes.io/component=controller"); err != nil {
		return "", 0, err
	}
	if len(list.Items) != 1 {
		return "", 0, fmt.Errorf("want exactly one controller Deployment in %s, got %d",
			resolveHelmNamespace(), len(list.Items))
	}
	d := list.Items[0]
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return d.Name, replicas, nil
}

// e38ScaleController scales the controller Deployment to replicas and waits
// until no controller Pod remains (0) or the rollout is complete.
func e38ScaleController(ctx context.Context, name string, replicas int32) error {
	ns := resolveHelmNamespace()
	if _, err := e36Kubectl(ctx, "", "scale", "deployment/"+name, "-n", ns,
		fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		return err
	}
	if replicas > 0 {
		_, err := e36Kubectl(ctx, "", "rollout", "status", "deployment/"+name, "-n", ns, "--timeout=4m")
		return err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, err := e36Kubectl(ctx, "", "get", "pods", "-n", ns,
			"-l", "app.kubernetes.io/component=controller", "-o", "name")
		if err != nil {
			return err
		}
		if out == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("controller pods still present after scale to 0: %s", out)
		}
		time.Sleep(3 * time.Second)
	}
}

// e38AgentPod returns the Running Helm agent Pod on node.
func e38AgentPod(ctx context.Context, node string) (*corev1.Pod, error) {
	var list corev1.PodList
	if _, err := e38GetJSON(ctx, &list, "pods", "-n", resolveHelmNamespace(),
		"-l", "app.kubernetes.io/component=agent", "--field-selector", "spec.nodeName="+node); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Status.Phase == corev1.PodRunning && list.Items[i].DeletionTimestamp == nil {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no Running agent Pod on node %s in %s", node, resolveHelmNamespace())
}

// e38AgentGRPCPort returns the agent container's "grpc" port.
func e38AgentGRPCPort(pod *corev1.Pod) (int32, error) {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == "grpc" {
				return p.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("agent Pod %s exposes no port named grpc", pod.Name)
}

// e38PortForward forwards an ephemeral local port to the agent Pod's gRPC
// port and returns the local address and a stop function.
func e38PortForward(ctx context.Context, pod string, port int32) (string, func(), error) {
	kubeconfig, err := e38Kubeconfig()
	if err != nil {
		return "", nil, err
	}
	pfCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(pfCtx, "kubectl", "--kubeconfig="+kubeconfig, //nolint:gosec
		"port-forward", "-n", resolveHelmNamespace(), "pod/"+pod,
		"--address", "127.0.0.1", fmt.Sprintf(":%d", port))
	cmd.Stderr = GinkgoWriter
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, fmt.Errorf("start port-forward: %w", err)
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	ports := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := e38ForwardRE.FindStringSubmatch(scanner.Text()); m != nil {
				select {
				case ports <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case local := <-ports:
		return "127.0.0.1:" + local, stop, nil
	case <-ctx.Done():
		stop()
		return "", nil, fmt.Errorf("port-forward to %s did not report its local port: %w", pod, ctx.Err())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent-observation helpers
// ─────────────────────────────────────────────────────────────────────────────

func e38Inspect(ctx context.Context, client agentv1.AgentServiceClient, volumeID string) (*agentv1.InspectVolumeResponse, error) {
	return client.InspectVolume(ctx, &agentv1.InspectVolumeRequest{
		VolumeId:    volumeID,
		BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
	})
}

// e38SameSource reports whether id names exactly the LV src pins.
func e38SameSource(id *agentv1.LvmSourceIdentity, src *pillarv1.LVMSourceRef) bool {
	return src != nil && id != nil &&
		id.GetVolumeGroup() == src.VolumeGroup && id.GetLogicalVolume() == src.LogicalVolume &&
		id.GetVolumeGroupUuid() == src.VolumeGroupUUID && id.GetLogicalVolumeUuid() == src.LogicalVolumeUUID
}

// e38ExpectIdentity asserts the observed LV identity equals the lvs row.
func e38ExpectIdentity(tc string, resp *agentv1.InspectVolumeResponse, row e38LV) {
	id := resp.GetLvm().GetIdentity()
	Expect(id.GetVolumeGroup()).To(Equal(row.VGName), "[%s] observed VG", tc)
	Expect(id.GetLogicalVolume()).To(Equal(row.LVName), "[%s] observed LV", tc)
	Expect(id.GetVolumeGroupUuid()).To(Equal(row.VGUUID), "[%s] observed vg_uuid", tc)
	Expect(id.GetLogicalVolumeUuid()).To(Equal(row.LVUUID), "[%s] observed lv_uuid", tc)
	Expect(resp.GetLvm().GetSegtype()).To(Equal(row.Segtype), "[%s] observed segtype", tc)
	Expect(resp.GetLvm().GetPoolLv()).To(Equal(row.Pool), "[%s] observed pool_lv", tc)
	Expect(strconv.FormatInt(resp.GetLvm().GetSizeBytes(), 10)).To(Equal(row.Size), "[%s] observed size", tc)
}

// e38ClaimEvidence correlates the agent's fence record for volumeID with the
// surviving PillarVolumeStates.  It never trusts the agent alone: "absent"
// when the agent has no mark, "claimed:<pvs>" only when exactly one live
// PillarVolumeState of agentName owns the mark's lifecycle UID for this
// volume ID and pins the observed LV UUID, and "unknown" for anything else
// (an ended mark, a mark no surviving PillarVolumeState explains, or a
// source that disagrees with the observed LV).
func e38ClaimEvidence(ctx context.Context, agentName, volumeID string, resp *agentv1.InspectVolumeResponse) (string, error) {
	fence := resp.GetFence()
	if !fence.GetExists() {
		return "absent", nil
	}
	if fence.GetEnded() {
		return "unknown", nil
	}
	observed := resp.GetLvm().GetIdentity().GetLogicalVolumeUuid()
	items, err := e38ListPVS(ctx)
	if err != nil {
		return "", err
	}
	var owners []string
	for i := range items {
		p := &items[i]
		if string(p.UID) != fence.GetVolumeUid() || p.Spec.AgentRef != agentName ||
			p.Spec.AgentVolumeID != volumeID || p.Spec.LVMSource == nil ||
			p.Spec.LVMSource.LogicalVolumeUUID != observed ||
			fence.GetLvmSource().GetLogicalVolumeUuid() != observed {
			continue
		}
		owners = append(owners, p.Name)
	}
	if len(owners) == 1 {
		return "claimed:" + owners[0], nil
	}
	return "unknown", nil
}

// e38InspectBlockers is step 7 of the retained-PV rebind runbook: the
// agent's read-only observation must show the active lifecycle of pvs still
// owning the pinned LV, with no export, no local consumer and a free
// exclusive claim.  Each violation is one blocker.
func e38InspectBlockers(resp *agentv1.InspectVolumeResponse, pvs *pillarv1.PillarVolumeState) []string {
	var blockers []string
	fence := resp.GetFence()
	if !fence.GetExists() {
		blockers = append(blockers, "fence=absent")
	}
	if fence.GetVolumeUid() != string(pvs.UID) {
		blockers = append(blockers, "fence-uid="+fence.GetVolumeUid())
	}
	if fence.GetEnded() {
		blockers = append(blockers, "fence=ended")
	}
	if !e38SameSource(fence.GetLvmSource(), pvs.Spec.LVMSource) ||
		!e38SameSource(resp.GetLvm().GetIdentity(), pvs.Spec.LVMSource) {
		blockers = append(blockers, "lvm-source=mismatch")
	}
	for _, e := range resp.GetExports() {
		blockers = append(blockers, "export="+e.GetTargetId())
	}
	for _, c := range resp.GetConsumers() {
		blockers = append(blockers, "consumer="+c.GetKind()+":"+c.GetDetail())
	}
	if claim := resp.GetLvm().GetExclusiveClaim(); claim != "free" {
		blockers = append(blockers, "exclusive-claim="+claim)
	}
	return blockers
}

// e38RebindBlockers is steps 1-4 of the retained-PV rebind runbook, read
// from Kubernetes only: the PV is Released under Retain, its old claim is
// gone, no VolumeAttachment references it, and its PillarVolumeState has no
// publication and is not deleting.  Each violation is one blocker.
func e38RebindBlockers(ctx context.Context, pvName string) ([]string, error) {
	pv, err := e38GetPV(ctx, pvName)
	if err != nil {
		return nil, err
	}
	if pv == nil {
		return []string{"pv=absent"}, nil
	}
	var blockers []string
	if pv.Status.Phase != corev1.VolumeReleased {
		blockers = append(blockers, "pv-phase="+string(pv.Status.Phase))
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		blockers = append(blockers, "reclaim-policy="+string(pv.Spec.PersistentVolumeReclaimPolicy))
	}
	if ref := pv.Spec.ClaimRef; ref != nil {
		claim, err := e38GetPVC(ctx, ref.Namespace, ref.Name)
		if err != nil {
			return nil, err
		}
		if claim != nil && claim.UID == ref.UID {
			blockers = append(blockers, "claim="+ref.Namespace+"/"+ref.Name)
		}
	}
	vas, err := e38AttachmentsFor(ctx, pvName)
	if err != nil {
		return nil, err
	}
	for _, va := range vas {
		blockers = append(blockers, "volumeattachment="+va)
	}
	pvs, err := e38GetPVS(ctx, pvName)
	if err != nil {
		return nil, err
	}
	switch {
	case pvs == nil:
		blockers = append(blockers, "pillarvolumestate=absent")
	default:
		if n := len(pvs.Status.PublishedNodes); n > 0 {
			blockers = append(blockers, fmt.Sprintf("published-nodes=%d", n))
		}
		if pvs.Status.Deleting {
			blockers = append(blockers, "pillarvolumestate=deleting")
		}
	}
	return blockers, nil
}

// e38ReplaceClaimRef points pv's claimRef at namespace/claim with
// `kubectl replace`, keeping the resourceVersion pv was read at, so a PV
// changed since then is refused with a Conflict.
func e38ReplaceClaimRef(ctx context.Context, pv *corev1.PersistentVolume, namespace, claim string) error {
	next := pv.DeepCopy()
	next.Spec.ClaimRef = &corev1.ObjectReference{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Namespace:  namespace,
		Name:       claim,
	}
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = e36Kubectl(ctx, string(body), "replace", "-f", "-")
	return err
}

// e38PrepareRelease makes a test PV deletable through its claim: the reclaim
// policy becomes Delete (a preserving adoption is then released, never
// destroyed) and a PV left Available by an interrupted rebind is bound to a
// claim of its own so that deleting the claims ends its lifecycle.
func e38PrepareRelease(ctx context.Context, namespace, pvName, cleanupClaim string) error {
	pv, err := e38GetPV(ctx, pvName)
	if err != nil || pv == nil {
		return err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		if _, err := e36Kubectl(ctx, "", "patch", "pv", pvName, "--type=merge",
			"-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`); err != nil {
			return err
		}
	}
	if pv.Status.Phase != corev1.VolumeAvailable {
		return nil
	}
	fresh, err := e38GetPV(ctx, pvName)
	if err != nil || fresh == nil {
		return err
	}
	if err := e38ReplaceClaimRef(ctx, fresh, namespace, cleanupClaim); err != nil {
		return err
	}
	if err := e36Apply(ctx, e38StaticPVCManifest(cleanupClaim, namespace, fresh.Spec.StorageClassName, pvName)); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		phase, err := e36Kubectl(ctx, "", "get", "pvc", cleanupClaim, "-n", namespace,
			"-o", "jsonpath={.status.phase}")
		if err != nil {
			return err
		}
		if phase == "Bound" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cleanup claim %s for PV %s did not bind (phase %q)", cleanupClaim, pvName, phase)
		}
		time.Sleep(time.Second)
	}
}

var _ = Describe("E38: LVM import-lv — 기존 LV 채택·보존·재바인딩 (Kind 클러스터 E2E)",
	Label("lvm", "import", "e38"),
	func() {
		Describe("E38.1 import-lv 어노테이션으로 기존 LVM LV 채택", Ordered, func() {
			var (
				proc        = GinkgoParallelProcess()
				storageNode string
				workers     []string
				namespace   string
				agentName   string
				linearVG    string
				thinVG      string
				thinPool    string
				agent       agentv1.AgentServiceClient

				nonce = strings.ToLower(agentFenceRunNonce[:6])

				// Cluster-scoped object names carry the run nonce so a rerun
				// never collides with (or is shadowed by) state leaked by an
				// aborted previous run: PillarStore/StorageClass objects and,
				// crucially, any PillarVolumeState left behind still reference
				// the old LV's agentVolumeID and cannot shadow the new one.
				storeLinear  = fmt.Sprintf("e38-linear-%d-%s", proc, nonce)
				storeThin    = fmt.Sprintf("e38-thin-%d-%s", proc, nonce)
				protocolName = fmt.Sprintf("e38-proto-%d-%s", proc, nonce)
				pscDelete    = fmt.Sprintf("e38-delete-%d-%s", proc, nonce)
				pscRetain    = fmt.Sprintf("e38-retain-%d-%s", proc, nonce)
				pscThin      = fmt.Sprintf("e38-thin-%d-%s", proc, nonce)
				scDelete     = fmt.Sprintf("e38-lvm-delete-%d-%s", proc, nonce)
				scRetain     = fmt.Sprintf("e38-lvm-retain-%d-%s", proc, nonce)
				scThin       = fmt.Sprintf("e38-lvm-thin-%d-%s", proc, nonce)

				lvName  = func(role string) string { return fmt.Sprintf("e38-%s-%d-%s", role, proc, nonce) }
				busyMnt = fmt.Sprintf("/tmp/e38-busy-%d-%s", proc, nonce)

				linear, retain, thin, blank, wrong, busy *e38Fixture

				pvcLinear  = fmt.Sprintf("e38-linear-%d", proc)
				pvcBlank   = fmt.Sprintf("e38-blank-%d", proc)
				pvcThin    = fmt.Sprintf("e38-thin-%d", proc)
				pvcRetain  = fmt.Sprintf("e38-retain-%d", proc)
				pvcRebound = fmt.Sprintf("e38-rebound-%d", proc)
				podLinear  = fmt.Sprintf("e38-linear-reader-%d", proc)
				podBlank   = fmt.Sprintf("e38-blank-reader-%d", proc)
				podThin    = fmt.Sprintf("e38-thin-reader-%d", proc)
				podRetainA = fmt.Sprintf("e38-retain-reader-a-%d", proc)
				podRetainB = fmt.Sprintf("e38-retain-reader-b-%d", proc)

				pvLinear, pvBlank, pvThin, pvRetain string
				linearPVSUID                        string
				retainPVSUID                        string
				retainClaimUID                      string
				retainHandle                        string

				// trackedPVs are released by the workload cleanup.
				trackedPVs []string

				controllerDeploy   string
				controllerReplicas int32
				controllerScaled   bool
			)

			track := func(pv string) {
				for _, p := range trackedPVs {
					if p == pv {
						return
					}
				}
				trackedPVs = append(trackedPVs, pv)
			}

			// prepare creates an LV outside pillar-csi and records it.  A
			// non-blank fixture is formatted ext4 and filled with the proof
			// file; its LV is removed when the container ends.
			prepare := func(ctx context.Context, role, vg, pool string, format bool) *e38Fixture {
				f := &e38Fixture{vg: vg, lv: lvName(role), thinPool: pool}
				Expect(e38CreateLV(ctx, storageNode, f.vg, f.lv, f.thinPool)).
					To(Succeed(), "[E38] create %s LV", role)
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer ccancel()
					Expect(e38RemoveLV(cctx, storageNode, f.vg, f.lv)).
						To(Succeed(), "[E38] remove %s LV %s", role, f.volumeID())
				})
				if format {
					sha, err := e38FormatAndFill(ctx, storageNode, f.device(),
						fmt.Sprintf("/tmp/e38-fill-%s", f.lv))
					Expect(err).NotTo(HaveOccurred(), "[E38] format and fill %s LV", role)
					Expect(sha).To(MatchRegexp(`^[0-9a-f]{64}$`), "[E38] proof sha256 of %s LV", role)
					f.proofSHA = sha
					fsType, fsUUID, err := e38FSProbe(ctx, storageNode, f.device())
					Expect(err).NotTo(HaveOccurred(), "[E38] blkid %s LV", role)
					Expect(fsType).To(Equal("ext4"), "[E38] %s LV filesystem", role)
					Expect(fsUUID).NotTo(BeEmpty(), "[E38] %s LV filesystem UUID", role)
					f.fsUUID = fsUUID
				}
				row, err := e38LVRow(ctx, storageNode, f.vg, f.lv)
				Expect(err).NotTo(HaveOccurred(), "[E38] lvs %s LV", role)
				Expect(row.Size).To(Equal(strconv.FormatInt(e38LVBytes, 10)), "[E38] %s LV size", role)
				f.row = row
				return f
			}

			BeforeAll(func() {
				e38FailIfNoInfra()
				storageNode = os.Getenv(suiteBackendContainerEnvVar)
				namespace = fmt.Sprintf("e38-lvm-%d-%s", proc, nonce)

				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()

				By("checking storage-node tools")
				_, err := e36NodeSh(ctx, storageNode,
					"for c in lvs vgs lvcreate lvremove mkfs.ext4 mount umount mountpoint blkid dd sha256sum stat find awk; "+
						"do command -v $c >/dev/null || { echo missing $c; exit 1; }; done")
				Expect(err).NotTo(HaveOccurred(),
					"[E38] MISSING PREREQUISITE: storage node %s needs lvm2, e2fsprogs (mkfs.ext4), util-linux "+
						"(mount, mountpoint, blkid), coreutils (dd, sha256sum, stat), find and awk", storageNode)

				By("picking two schedulable nodes for the reader Pods")
				workers, err = e36PickWorkers(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E38] list nodes")
				Expect(len(workers)).To(BeNumerically(">=", 2),
					"[E38] MISSING PREREQUISITE: need at least two Ready, schedulable nodes, got %v", workers)

				By("creating the test namespace")
				Expect(e36Apply(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n",
					namespace))).To(Succeed(), "[E38] create namespace")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer ccancel()
					_, derr := e36Kubectl(cctx, "", "delete", "namespace", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=150s")
					Expect(derr).NotTo(HaveOccurred(), "[E38] delete namespace")
				})

				By("resolving the PillarAgent of the storage node")
				agentName, err = e36ExistingAgentFor(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E38] list PillarAgents")
				if agentName == "" {
					agentName = fmt.Sprintf("e38-agent-%d", proc)
					Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: %s
spec:
  nodeRef:
    name: %s
    addressType: InternalIP
`, agentName, storageNode))).To(Succeed(), "[E38] create PillarAgent")
					created := agentName
					DeferCleanup(func() {
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						_, derr := e36Kubectl(cctx, "", "delete", "pillaragent", created,
							"--ignore-not-found=true", "--wait=true", "--timeout=90s")
						Expect(derr).NotTo(HaveOccurred(), "[E38] delete PillarAgent")
					})
				}
				Eventually(func(g Gomega) {
					st, gerr := e36ReadyCondition(ctx, "pillaragent", agentName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(st).To(Equal("True"))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E38] PillarAgent %s must become Ready", agentName)

				By("discovering the linear and thin LVM backends the Helm values configured")
				var pa pillarv1.PillarAgent
				found, err := e38GetJSON(ctx, &pa, "pillaragent", agentName)
				Expect(err).NotTo(HaveOccurred(), "[E38] read PillarAgent")
				Expect(found).To(BeTrue(), "[E38] PillarAgent %s vanished", agentName)
				for _, p := range pa.Status.DiscoveredPools {
					if p.Type != string(pillarv1.BackendIDLVMLV) {
						continue
					}
					if p.ThinPool == "" && linearVG == "" {
						linearVG = p.Name
					}
					if p.ThinPool != "" && thinVG == "" {
						thinVG, thinPool = p.Name, p.ThinPool
					}
				}
				Expect(linearVG).NotTo(BeEmpty(),
					"[E38] MISSING PREREQUISITE: the agent must serve an LVM VG without thinPool "+
						"(Helm agent.backends[].lvm.volumeGroup); discovered %+v", pa.Status.DiscoveredPools)
				Expect(thinVG).NotTo(BeEmpty(),
					"[E38] MISSING PREREQUISITE: the agent must serve a second LVM VG with a thin pool "+
						"(Helm agent.backends[].lvm {volumeGroup, thinPool}); discovered %+v", pa.Status.DiscoveredPools)

				By("connecting to the agent's gRPC API through kubectl port-forward")
				var agentPod *corev1.Pod
				Eventually(func(g Gomega) {
					var perr error
					agentPod, perr = e38AgentPod(ctx, storageNode)
					g.Expect(perr).NotTo(HaveOccurred())
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E38] the Helm agent Pod must run on %s", storageNode)
				port, err := e38AgentGRPCPort(agentPod)
				Expect(err).NotTo(HaveOccurred(), "[E38] agent gRPC port")
				pfCtx, pfCancel := context.WithTimeout(ctx, 30*time.Second)
				addr, stopPF, err := e38PortForward(pfCtx, agentPod.Name, port)
				pfCancel()
				Expect(err).NotTo(HaveOccurred(), "[E38] port-forward to agent Pod %s", agentPod.Name)
				DeferCleanup(stopPF)
				conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
				Expect(err).NotTo(HaveOccurred(), "[E38] gRPC client for %s", addr)
				DeferCleanup(func() { _ = conn.Close() })
				agent = agentv1.NewAgentServiceClient(conn)
				Eventually(func(g Gomega) {
					_, cerr := agent.GetCapacity(ctx, &agentv1.GetCapacityRequest{
						BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
						PoolName:    linearVG,
					})
					g.Expect(cerr).NotTo(HaveOccurred())
				}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(3*time.Second).Should(Succeed(),
					"[E38] MISSING PREREQUISITE: the agent gRPC API must answer in plaintext "+
						"(chart default mtls.enabled=false)")

				By("creating the PillarStore / PillarProtocol / PillarStorageClass stack")
				Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: %[1]s
spec:
  agentRef: %[3]s
  backend:
    lvm:
      volumeGroup: %[4]s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: %[2]s
spec:
  agentRef: %[3]s
  backend:
    lvm:
      volumeGroup: %[5]s
      thinPool: %[6]s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: %[7]s
spec:
  protocol:
    nvmeofTcp:
      port: 4420
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %[8]s
spec:
  storeRef: %[1]s
  protocolRef: %[7]s
  storageClass:
    name: %[11]s
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
    allowVolumeExpansion: true
  filesystem:
    fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %[9]s
spec:
  storeRef: %[1]s
  protocolRef: %[7]s
  storageClass:
    name: %[12]s
    reclaimPolicy: Retain
    volumeBindingMode: Immediate
  filesystem:
    fsType: ext4
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %[10]s
spec:
  storeRef: %[2]s
  protocolRef: %[7]s
  storageClass:
    name: %[13]s
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
  filesystem:
    fsType: ext4
`, storeLinear, storeThin, agentName, linearVG, thinVG, thinPool, protocolName,
					pscDelete, pscRetain, pscThin, scDelete, scRetain, scThin))).
					To(Succeed(), "[E38] apply CR stack")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 4*time.Minute)
					defer ccancel()
					for _, r := range [][2]string{
						{"pillarstorageclass", pscDelete}, {"pillarstorageclass", pscRetain},
						{"pillarstorageclass", pscThin}, {"pillarprotocol", protocolName},
						{"pillarstore", storeLinear}, {"pillarstore", storeThin},
					} {
						_, derr := e36Kubectl(cctx, "", "delete", r[0], r[1],
							"--ignore-not-found=true", "--wait=true", "--timeout=60s")
						Expect(derr).NotTo(HaveOccurred(), "[E38] delete %s %s", r[0], r[1])
					}
				})
				for psc, sc := range map[string]string{pscDelete: scDelete, pscRetain: scRetain, pscThin: scThin} {
					Eventually(func(g Gomega) {
						st, gerr := e36ReadyCondition(ctx, "pillarstorageclass", psc)
						g.Expect(gerr).NotTo(HaveOccurred())
						g.Expect(st).To(Equal("True"))
						prov, gerr := e36Kubectl(ctx, "", "get", "storageclass", sc,
							"-o", "jsonpath={.provisioner}")
						g.Expect(gerr).NotTo(HaveOccurred())
						g.Expect(prov).To(Equal("pillar-csi.bhyoo.com"))
					}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
						"[E38] PillarStorageClass %s must be Ready with StorageClass %s", psc, sc)
				}

				By("creating the LVs outside pillar-csi and writing the proof files")
				linear = prepare(ctx, "linear", linearVG, "", true)
				retain = prepare(ctx, "retain", linearVG, "", true)
				wrong = prepare(ctx, "wrong", linearVG, "", true)
				busy = prepare(ctx, "busy", linearVG, "", true)
				blank = prepare(ctx, "blank", linearVG, "", false)
				thin = prepare(ctx, "thin", thinVG, thinPool, true)

				By("holding the busy LV mounted on the storage node")
				_, err = e36NodeSh(ctx, storageNode, fmt.Sprintf("mkdir -p %[2]s && mount -t ext4 %[1]s %[2]s",
					busy.device(), busyMnt))
				Expect(err).NotTo(HaveOccurred(), "[E38] mount busy LV")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
					defer ccancel()
					_, uerr := e36NodeSh(cctx, storageNode, fmt.Sprintf(
						"if mountpoint -q %[1]s; then umount %[1]s; fi; rmdir %[1]s 2>/dev/null || true", busyMnt))
					Expect(uerr).NotTo(HaveOccurred(), "[E38] unmount busy LV")
				})

				// Registered last so it runs first: workloads, claims and
				// lifecycles go before the LVs, CRs and namespace.
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 12*time.Minute)
					defer ccancel()
					if controllerScaled {
						Expect(e38ScaleController(cctx, controllerDeploy, controllerReplicas)).
							To(Succeed(), "[E38] restore controller replicas")
						controllerScaled = false
					}
					_, derr := e36Kubectl(cctx, "", "delete", "pods", "--all", "-n", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=180s")
					Expect(derr).NotTo(HaveOccurred(), "[E38] delete Pods")
					for i, pv := range trackedPVs {
						Expect(e38PrepareRelease(cctx, namespace, pv, fmt.Sprintf("e38-cleanup-%d-%d", proc, i))).
							To(Succeed(), "[E38] prepare release of PV %s", pv)
					}
					_, derr = e36Kubectl(cctx, "", "delete", "pvc", "--all", "-n", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=180s")
					Expect(derr).NotTo(HaveOccurred(), "[E38] delete PVCs")
					for _, pv := range trackedPVs {
						e38WaitVolumeGone(cctx, "E38-cleanup", pv)
					}
				})
			})

			// -- TC-E38.1 ----------------------------------------------------
			It("[TC-E38.1] the Helm-installed agent inspects an unclaimed LV read-only with its real filesystem signature", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()

				By("proving controller, agent and node come from one Helm release")
				var workloads struct {
					Items []struct {
						Kind     string `json:"kind"`
						Metadata struct {
							Name        string            `json:"name"`
							Labels      map[string]string `json:"labels"`
							Annotations map[string]string `json:"annotations"`
						} `json:"metadata"`
					} `json:"items"`
				}
				_, err := e38GetJSON(ctx, &workloads, "deployments,daemonsets", "-n", resolveHelmNamespace(),
					"-l", "app.kubernetes.io/managed-by=Helm")
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] list Helm workloads")
				kinds := map[string]string{}
				releases := map[string]bool{}
				var agentDS string
				for _, w := range workloads.Items {
					component := w.Metadata.Labels["app.kubernetes.io/component"]
					kinds[component] = w.Kind
					release := w.Metadata.Annotations["meta.helm.sh/release-name"]
					Expect(release).To(Equal(w.Metadata.Labels["app.kubernetes.io/instance"]),
						"[TC-E38.1] %s %s must be owned by its Helm release", w.Kind, w.Metadata.Name)
					releases[release] = true
					if component == "agent" {
						agentDS = w.Metadata.Name
					}
				}
				Expect(kinds).To(HaveKeyWithValue("controller", "Deployment"), "[TC-E38.1] Helm controller")
				Expect(kinds).To(HaveKeyWithValue("agent", "DaemonSet"), "[TC-E38.1] Helm agent")
				Expect(kinds).To(HaveKeyWithValue("node", "DaemonSet"), "[TC-E38.1] Helm node plugin")
				Expect(releases).To(HaveLen(1), "[TC-E38.1] one Helm release installs every component")

				agentPod, err := e38AgentPod(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] agent Pod")
				Expect(agentPod.OwnerReferences).To(ContainElement(
					HaveField("Name", agentDS)), "[TC-E38.1] the agent Pod belongs to the Helm DaemonSet")
				for _, cs := range agentPod.Status.ContainerStatuses {
					Expect(cs.ImageID).NotTo(BeEmpty(), "[TC-E38.1] agent container %s image ID", cs.Name)
					AddReportEntry(fmt.Sprintf("E38 agent container %s", cs.Name), cs.Image+" "+cs.ImageID)
				}

				By("inspecting the unclaimed linear LV twice around a storage snapshot")
				before, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] storage snapshot before")
				resp, err := e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] InspectVolume")
				_, err = e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] second InspectVolume")
				after, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] storage snapshot after")
				Expect(after).To(Equal(before),
					"[TC-E38.1] InspectVolume must not change VG seqno, LV activation, mounts, holders or marks")

				e38ExpectIdentity("TC-E38.1", resp, linear.row)
				Expect(resp.GetLvm().GetActive()).To(BeTrue(), "[TC-E38.1] LV active")
				Expect(resp.GetLvm().GetDevicePath()).To(Equal(linear.device()), "[TC-E38.1] device path")
				Expect(resp.GetFilesystemType()).To(Equal("ext4"),
					"[TC-E38.1] the packaged agent must read the real blkid signature")
				Expect(resp.GetFilesystemUuid()).To(Equal(linear.fsUUID),
					"[TC-E38.1] filesystem UUID must equal the storage node's blkid")
				Expect(resp.GetFilesystemProbeState()).To(Equal("detected"),
					"[TC-E38.1] a well-formed util-linux probe of the ext4 LV is detected")
				Expect(resp.GetFilesystemProbeError()).To(BeEmpty(), "[TC-E38.1] a detected probe has no error")
				Expect(resp.GetLvm().GetExclusiveClaim()).To(Equal("free"), "[TC-E38.1] idle LV is free")
				Expect(resp.GetConsumers()).To(BeEmpty(), "[TC-E38.1] idle LV has no consumer")
				Expect(resp.GetExports()).To(BeEmpty(), "[TC-E38.1] unclaimed LV has no export")
				Expect(resp.GetFence().GetExists()).To(BeFalse(), "[TC-E38.1] unclaimed LV has no fence mark")
				evidence, err := e38ClaimEvidence(ctx, agentName, linear.volumeID(), resp)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.1] correlate claim evidence")
				Expect(evidence).To(Equal("absent"), "[TC-E38.1] no claim may be inferred")
			})

			// -- TC-E38.2 ----------------------------------------------------
			It("[TC-E38.2] an import-lv PVC adopts a pre-created linear LV under PreserveOriginal without changing its UUID or size", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				Expect(e36Apply(ctx, e38PVCManifest(pvcLinear, namespace, scDelete, linear.importValue()))).
					To(Succeed(), "[TC-E38.2] apply import PVC")
				pvLinear = e38WaitBound(ctx, "TC-E38.2", namespace, pvcLinear)
				track(pvLinear)

				pv, err := e38GetPV(ctx, pvLinear)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.2] get PV")
				Expect(pv).NotTo(BeNil(), "[TC-E38.2] PV %s", pvLinear)
				capacity := pv.Spec.Capacity[corev1.ResourceStorage]
				Expect(capacity.Value()).To(Equal(e38LVBytes),
					"[TC-E38.2] PV capacity must equal the adopted LV size, not the request")
				Expect(pv.Spec.CSI).NotTo(BeNil(), "[TC-E38.2] CSI PV")
				Expect(pv.Spec.CSI.VolumeAttributes).To(HaveKeyWithValue(e38VCPreserve, "true"),
					"[TC-E38.2] the node must be told to preserve the original")

				pvs, err := e38GetPVS(ctx, pvLinear)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.2] get PillarVolumeState")
				Expect(pvs).NotTo(BeNil(), "[TC-E38.2] PillarVolumeState %s", pvLinear)
				Expect(pvs.Spec.AgentRef).To(Equal(agentName), "[TC-E38.2] agentRef")
				Expect(pvs.Spec.AgentVolumeID).To(Equal(linear.volumeID()), "[TC-E38.2] agentVolumeID")
				Expect(pvs.Spec.LVMSource).To(Equal(&pillarv1.LVMSourceRef{
					VolumeGroup:       linear.vg,
					LogicalVolume:     linear.lv,
					VolumeGroupUUID:   linear.row.VGUUID,
					LogicalVolumeUUID: linear.row.LVUUID,
					PreserveOriginal:  true,
				}), "[TC-E38.2] spec.lvmSource pins the adopted LV and the default PreserveOriginal policy")
				linearPVSUID = string(pvs.UID)

				row, err := e38LVRow(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.2] lvs after import")
				Expect(row.identity()).To(Equal(linear.row.identity()),
					"[TC-E38.2] adoption must not rename, resize or recreate the LV")
			})

			// -- TC-E38.3 ----------------------------------------------------
			It("[TC-E38.3] a Pod on a remote node reads the data written before the linear import", func() {
				if pvLinear == "" {
					Fail("[TC-E38.3] MISSING PREREQUISITE: TC-E38.2 did not bind the import PVC")
				}
				e38RunPodAndVerify("TC-E38.3", namespace, podLinear, workers[0], pvcLinear, linear.proofSHA)
			})

			// -- TC-E38.4 ----------------------------------------------------
			It("[TC-E38.4] InspectVolume correlates the adopted LV with its surviving PillarVolumeState and changes no metadata", func() {
				if linearPVSUID == "" {
					Fail("[TC-E38.4] MISSING PREREQUISITE: TC-E38.2 did not adopt the linear LV")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()

				before, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.4] storage snapshot before")
				resp, err := e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.4] InspectVolume")
				_, err = e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.4] second InspectVolume")
				after, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.4] storage snapshot after")
				Expect(after).To(Equal(before),
					"[TC-E38.4] InspectVolume must not change VG seqno, LV activation, mounts, holders or the mark")

				e38ExpectIdentity("TC-E38.4", resp, linear.row)
				fence := resp.GetFence()
				Expect(fence.GetExists()).To(BeTrue(), "[TC-E38.4] adopted LV has a fence mark")
				Expect(fence.GetVolumeUid()).To(Equal(linearPVSUID), "[TC-E38.4] mark records the PVS lifecycle")
				Expect(fence.GetEnded()).To(BeFalse(), "[TC-E38.4] live lifecycle")
				Expect(fence.GetPreserveOriginal()).To(BeTrue(), "[TC-E38.4] mark pins PreserveOriginal")
				Expect(fence.GetLvmSource().GetLogicalVolumeUuid()).To(Equal(linear.row.LVUUID),
					"[TC-E38.4] mark pins the LV UUID")
				Expect(fence.GetLvmSource().GetVolumeGroupUuid()).To(Equal(linear.row.VGUUID),
					"[TC-E38.4] mark pins the VG UUID")
				Expect(resp.GetExports()).To(HaveLen(1), "[TC-E38.4] the agent's own export is reported as configuration")
				Expect(resp.GetExports()[0].GetNamespaceEnabled()).To(BeTrue(), "[TC-E38.4] export namespace enabled")
				for _, c := range resp.GetConsumers() {
					Expect(c.GetKind()).NotTo(Equal("foreign_export"), "[TC-E38.4] own export is not a foreign consumer")
				}
				evidence, err := e38ClaimEvidence(ctx, agentName, linear.volumeID(), resp)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.4] correlate claim evidence")
				Expect(evidence).To(Equal("claimed:"+pvLinear),
					"[TC-E38.4] UUID and surviving PVS must correlate to exactly one claim")
			})

			// -- TC-E38.5 ----------------------------------------------------
			It("[TC-E38.5] expanding a PreserveOriginal claim is refused and the LV keeps its size", func() {
				if pvLinear == "" {
					Fail("[TC-E38.5] MISSING PREREQUISITE: TC-E38.2 did not bind the import PVC")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				_, err := e36Kubectl(ctx, "", "patch", "pvc", pvcLinear, "-n", namespace, "--type=merge",
					"-p", fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":%q}}}}`, e38ExpandTo))
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.5] request expansion")
				Eventually(func(g Gomega) {
					msgs, gerr := e38Events(ctx, namespace, "PersistentVolumeClaim", pvcLinear, "VolumeResizeFailed")
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(msgs).NotTo(BeEmpty())
					AddReportEntry("TC-E38.5 VolumeResizeFailed", msgs)
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E38.5] the resizer must record a VolumeResizeFailed event for the preserving adoption")
				claim, err := e38GetPVC(ctx, namespace, pvcLinear)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.5] get PVC")
				claimCap := claim.Status.Capacity[corev1.ResourceStorage]
				Expect(claimCap.Value()).To(Equal(e38LVBytes), "[TC-E38.5] PVC status capacity unchanged")

				row, err := e38LVRow(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.5] lvs after refused resize")
				Expect(row.identity()).To(Equal(linear.row.identity()), "[TC-E38.5] LV size and identity unchanged")
				pv, err := e38GetPV(ctx, pvLinear)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.5] get PV")
				capacity := pv.Spec.Capacity[corev1.ResourceStorage]
				Expect(capacity.Value()).To(Equal(e38LVBytes), "[TC-E38.5] PV capacity unchanged")
			})

			// -- TC-E38.6 ----------------------------------------------------
			It("[TC-E38.6] a second claim on an LV another lifecycle owns is refused and the fence is unchanged", func() {
				if linearPVSUID == "" {
					Fail("[TC-E38.6] MISSING PREREQUISITE: TC-E38.2 did not adopt the linear LV")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()

				before, err := e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.6] InspectVolume before")
				marks, err := e38MarkFiles(ctx, storageNode, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.6] mark files before")
				Expect(marks).NotTo(BeEmpty(), "[TC-E38.6] the owning lifecycle has a durable mark")

				By("the agent refuses a second lifecycle's import of the owned LV")
				err = e38DirectImport(ctx, agent, linear.vg, linear.lv, linear.row.VGUUID, linear.row.LVUUID,
					"foreign-"+linear.lv)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.6] direct ImportVolume: %v", err)

				e38ExpectRefused("TC-E38.6", namespace, scDelete, fmt.Sprintf("e38-foreign-%d", proc),
					linear.importValue(), func() {
						after, ierr := e38Inspect(ctx, agent, linear.volumeID())
						Expect(ierr).NotTo(HaveOccurred(), "[TC-E38.6] InspectVolume after")
						Expect(after.GetFence().GetVolumeUid()).To(Equal(before.GetFence().GetVolumeUid()),
							"[TC-E38.6] the owning lifecycle keeps the mark")
						Expect(after.GetFence().GetGeneration()).To(Equal(before.GetFence().GetGeneration()),
							"[TC-E38.6] the mark generation is unchanged")
						again, merr := e38MarkFiles(ctx, storageNode, linear.lv)
						Expect(merr).NotTo(HaveOccurred(), "[TC-E38.6] mark files after")
						Expect(again).To(Equal(marks), "[TC-E38.6] the refused claim must not touch the mark")
						evidence, cerr := e38ClaimEvidence(ctx, agentName, linear.volumeID(), after)
						Expect(cerr).NotTo(HaveOccurred(), "[TC-E38.6] correlate claim evidence")
						Expect(evidence).To(Equal("claimed:"+pvLinear), "[TC-E38.6] the original claim survives")
					})
			})

			// -- TC-E38.7 ----------------------------------------------------
			It("[TC-E38.7] an import naming a wrong LV UUID is refused and the LV and fences are untouched", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()

				devBefore, err := e38DeviceSHA(ctx, storageNode, wrong.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.7] device sha before")
				snapBefore, err := e38StorageSnapshot(ctx, storageNode, wrong.vg, wrong.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.7] storage snapshot before")
				value := fmt.Sprintf("%s/%s:%s:%s", wrong.vg, wrong.lv, wrong.row.VGUUID, e38FakeLVUUID)

				By("the agent refuses the mismatching identity")
				err = e38DirectImport(ctx, agent, wrong.vg, wrong.lv, wrong.row.VGUUID, e38FakeLVUUID, "wrong-"+wrong.lv)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.7] direct ImportVolume: %v", err)

				e38ExpectRefused("TC-E38.7", namespace, scDelete, fmt.Sprintf("e38-wrong-%d", proc),
					value, func() {
						devAfter, derr := e38DeviceSHA(ctx, storageNode, wrong.device())
						Expect(derr).NotTo(HaveOccurred(), "[TC-E38.7] device sha after")
						Expect(devAfter).To(Equal(devBefore), "[TC-E38.7] no byte of the LV may change")
						snapAfter, serr := e38StorageSnapshot(ctx, storageNode, wrong.vg, wrong.lv)
						Expect(serr).NotTo(HaveOccurred(), "[TC-E38.7] storage snapshot after")
						Expect(snapAfter).To(Equal(snapBefore), "[TC-E38.7] LVM metadata, mounts and marks unchanged")
						resp, ierr := e38Inspect(ctx, agent, wrong.volumeID())
						Expect(ierr).NotTo(HaveOccurred(), "[TC-E38.7] InspectVolume")
						Expect(resp.GetFence().GetExists()).To(BeFalse(), "[TC-E38.7] refused import records no fence")
						Expect(resp.GetExports()).To(BeEmpty(), "[TC-E38.7] refused import exports nothing")
						row, lerr := e38LVRow(ctx, storageNode, wrong.vg, wrong.lv)
						Expect(lerr).NotTo(HaveOccurred(), "[TC-E38.7] lvs after")
						Expect(row.identity()).To(Equal(wrong.row.identity()), "[TC-E38.7] LV identity unchanged")
					})
			})

			// -- TC-E38.8 ----------------------------------------------------
			It("[TC-E38.8] an LV mounted on the storage node is refused as in use and reported busy without a claim", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()

				marks, err := e38MarkFiles(ctx, storageNode, busy.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.8] mark files before")
				Expect(marks).To(BeEmpty(), "[TC-E38.8] the busy LV was never claimed")
				snapBefore, err := e38StorageSnapshot(ctx, storageNode, busy.vg, busy.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.8] storage snapshot before")

				By("the agent observes the held device as busy and refuses to import it")
				resp, err := e38Inspect(ctx, agent, busy.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.8] InspectVolume")
				Expect(resp.GetLvm().GetExclusiveClaim()).To(Equal("busy"),
					"[TC-E38.8] the mounted LV cannot be claimed exclusively")
				evidence, err := e38ClaimEvidence(ctx, agentName, busy.volumeID(), resp)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.8] correlate claim evidence")
				Expect(evidence).To(Equal("absent"), "[TC-E38.8] a busy LV is not a pillar-csi claim")
				err = e38DirectImport(ctx, agent, busy.vg, busy.lv, busy.row.VGUUID, busy.row.LVUUID, "busy-"+busy.lv)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.8] direct ImportVolume: %v", err)

				e38ExpectRefused("TC-E38.8", namespace, scDelete, fmt.Sprintf("e38-busy-%d", proc),
					busy.importValue(), func() {
						out, merr := e36NodeSh(ctx, storageNode, "mountpoint -q "+busyMnt+" && echo mounted")
						Expect(merr).NotTo(HaveOccurred(), "[TC-E38.8] mountpoint")
						Expect(out).To(Equal("mounted"), "[TC-E38.8] the storage-node mount must still be held")
						snapAfter, serr := e38StorageSnapshot(ctx, storageNode, busy.vg, busy.lv)
						Expect(serr).NotTo(HaveOccurred(), "[TC-E38.8] storage snapshot after")
						Expect(snapAfter).To(Equal(snapBefore),
							"[TC-E38.8] LVM metadata, mounts, holders and marks unchanged; no fence recorded")
						row, lerr := e38LVRow(ctx, storageNode, busy.vg, busy.lv)
						Expect(lerr).NotTo(HaveOccurred(), "[TC-E38.8] lvs after")
						Expect(row.identity()).To(Equal(busy.row.identity()), "[TC-E38.8] LV identity unchanged")
					})
			})

			// -- TC-E38.9 ----------------------------------------------------
			It("[TC-E38.9] a blank LV under PreserveOriginal is never formatted: staging is refused and the device bytes are unchanged", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()

				fsType, _, err := e38FSProbe(ctx, storageNode, blank.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] blkid before")
				Expect(fsType).To(BeEmpty(), "[TC-E38.9] the fixture LV is blank")
				devBefore, err := e38DeviceSHA(ctx, storageNode, blank.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] device sha before")

				Expect(e36Apply(ctx, e38PVCManifest(pvcBlank, namespace, scDelete, blank.importValue()))).
					To(Succeed(), "[TC-E38.9] apply import PVC")
				pvBlank = e38WaitBound(ctx, "TC-E38.9", namespace, pvcBlank)
				track(pvBlank)

				By("starting a Pod whose staging must refuse the blank device")
				Expect(e36Apply(ctx, e36PodManifest(podBlank, namespace, workers[0], pvcBlank))).
					To(Succeed(), "[TC-E38.9] apply Pod")
				Eventually(func(g Gomega) {
					msgs, gerr := e38Events(ctx, namespace, "Pod", podBlank, "FailedMount")
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(msgs).NotTo(BeEmpty())
					AddReportEntry("TC-E38.9 FailedMount", msgs)
				}).WithContext(ctx).WithTimeout(5*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E38.9] staging of the blank preserved LV must record FailedMount")
				phase, err := e36Kubectl(ctx, "", "get", "pod", podBlank, "-n", namespace,
					"-o", "jsonpath={.status.phase}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] Pod phase")
				Expect(phase).NotTo(Equal("Running"), "[TC-E38.9] the Pod must not start on an unformatted volume")

				By("deleting the Pod and the claim")
				Expect(e36DeletePod(ctx, namespace, podBlank)).To(Succeed(), "[TC-E38.9] delete Pod")
				Expect(e38DeletePVC(ctx, namespace, pvcBlank)).To(Succeed(), "[TC-E38.9] delete PVC")
				e38WaitVolumeGone(ctx, "TC-E38.9", pvBlank)

				fsType, _, err = e38FSProbe(ctx, storageNode, blank.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] blkid after")
				Expect(fsType).To(BeEmpty(), "[TC-E38.9] the preserved LV must never be formatted")
				devAfter, err := e38DeviceSHA(ctx, storageNode, blank.device())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] device sha after")
				Expect(devAfter).To(Equal(devBefore), "[TC-E38.9] no byte of the blank LV may change")
				row, err := e38LVRow(ctx, storageNode, blank.vg, blank.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.9] lvs after release")
				Expect(row.identity()).To(Equal(blank.row.identity()), "[TC-E38.9] LV kept after release")
			})

			// -- TC-E38.10 ---------------------------------------------------
			It("[TC-E38.10] deleting a PreserveOriginal claim releases the lifecycle and keeps the LV and its data", func() {
				if pvLinear == "" {
					Fail("[TC-E38.10] MISSING PREREQUISITE: TC-E38.2 did not bind the import PVC")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				Expect(e36DeletePod(ctx, namespace, podLinear)).To(Succeed(), "[TC-E38.10] delete Pod")
				Expect(e38DeletePVC(ctx, namespace, pvcLinear)).To(Succeed(), "[TC-E38.10] delete PVC")
				e38WaitVolumeGone(ctx, "TC-E38.10", pvLinear)

				row, err := e38LVRow(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.10] the LV must survive the preserving Delete")
				Expect(row.identity()).To(Equal(linear.row.identity()), "[TC-E38.10] LV UUID and size kept")

				resp, err := e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.10] InspectVolume")
				Expect(resp.GetExports()).To(BeEmpty(), "[TC-E38.10] release removes the export")
				Expect(resp.GetConsumers()).To(BeEmpty(), "[TC-E38.10] no consumer after release")
				Expect(resp.GetLvm().GetExclusiveClaim()).To(Equal("free"), "[TC-E38.10] released LV is free")
				fence := resp.GetFence()
				Expect(fence.GetExists()).To(BeTrue(), "[TC-E38.10] the mark survives the release")
				Expect(fence.GetVolumeUid()).To(Equal(linearPVSUID), "[TC-E38.10] the released lifecycle")
				Expect(fence.GetEnded()).To(BeTrue(), "[TC-E38.10] the lifecycle is ended")
				Expect(fence.GetPreserveOriginal()).To(BeTrue(), "[TC-E38.10] the policy stays pinned")
				Expect(fence.GetLvmSource().GetLogicalVolumeUuid()).To(Equal(linear.row.LVUUID),
					"[TC-E38.10] the source stays pinned")
				evidence, err := e38ClaimEvidence(ctx, agentName, linear.volumeID(), resp)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.10] correlate claim evidence")
				Expect(evidence).To(Equal("unknown"),
					"[TC-E38.10] a mark no surviving PVS explains is never reported as a claim")

				sha, err := e38ReadProofRO(ctx, storageNode, linear.device(), "/tmp/e38-verify-"+linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.10] read the proof file read-only")
				Expect(sha).To(Equal(linear.proofSHA), "[TC-E38.10] the original data survives the Delete")
			})

			// -- TC-E38.11 ---------------------------------------------------
			It("[TC-E38.11] stale and new-lifecycle Delete, Expand and Create on the released LV cannot destroy or resize it", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()

				resp, err := e38Inspect(ctx, agent, linear.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.11] InspectVolume")
				Expect(resp.GetFence().GetEnded()).To(BeTrue(),
					"[TC-E38.11] MISSING PREREQUISITE: TC-E38.10 did not release the linear LV")
				Expect(resp.GetFence().GetEndedUids()).To(ContainElement(resp.GetFence().GetVolumeUid()),
					"[TC-E38.11] the preserving release retired the old lifecycle UID")
				stale := &agentv1.FencingToken{
					VolumeUid:  resp.GetFence().GetVolumeUid(),
					Generation: resp.GetFence().GetGeneration(),
				}
				before, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.11] storage snapshot before")

				By("replaying the released lifecycle's DeleteVolume")
				_, err = agent.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
					VolumeId: linear.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM, Fence: stale,
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
					"[TC-E38.11] the preserving release retired the old UID, so its delayed Delete is refused: %v", err)

				By("deleting under a new lifecycle")
				_, err = agent.DeleteVolume(ctx, &agentv1.DeleteVolumeRequest{
					VolumeId: linear.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					Fence: e38DirectFence("delete-" + linear.lv),
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.11] new-lifecycle Delete: %v", err)

				By("expanding under the stale and a new lifecycle")
				_, err = agent.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
					VolumeId: linear.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					RequestedBytes: 2 * e38LVBytes, Fence: stale,
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.11] stale Expand: %v", err)
				_, err = agent.ExpandVolume(ctx, &agentv1.ExpandVolumeRequest{
					VolumeId: linear.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					RequestedBytes: 2 * e38LVBytes, Fence: e38DirectFence("expand-" + linear.lv),
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.11] new-lifecycle Expand: %v", err)

				By("re-creating the pinned volume ID")
				_, err = agent.CreateVolume(ctx, &agentv1.CreateVolumeRequest{
					VolumeId: linear.volumeID(), BackendType: agentv1.BackendType_BACKEND_TYPE_LVM,
					CapacityBytes: e38LVBytes, Fence: e38DirectFence("create-" + linear.lv),
					BackendParams: &agentv1.BackendParams{Params: &agentv1.BackendParams_Lvm{
						Lvm: &agentv1.LvmVolumeParams{VolumeGroup: linear.vg, ProvisionMode: "linear"},
					}},
				})
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition), "[TC-E38.11] Create of pinned ID: %v", err)

				after, err := e38StorageSnapshot(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.11] storage snapshot after")
				Expect(after).To(Equal(before),
					"[TC-E38.11] no VG metadata, LV state or mark byte may change")
				row, err := e38LVRow(ctx, storageNode, linear.vg, linear.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.11] lvs after")
				Expect(row.identity()).To(Equal(linear.row.identity()), "[TC-E38.11] LV neither removed nor resized")
			})

			// -- TC-E38.12 ---------------------------------------------------
			It("[TC-E38.12] a pre-created thin LV is adopted, read remotely and released with its UUID, size and thin pool intact", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()

				poolBefore, err := e38LVRow(ctx, storageNode, thin.vg, thinPool)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] lvs thin pool before")

				Expect(e36Apply(ctx, e38PVCManifest(pvcThin, namespace, scThin, thin.importValue()))).
					To(Succeed(), "[TC-E38.12] apply import PVC")
				pvThin = e38WaitBound(ctx, "TC-E38.12", namespace, pvcThin)
				track(pvThin)
				pv, err := e38GetPV(ctx, pvThin)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] get PV")
				capacity := pv.Spec.Capacity[corev1.ResourceStorage]
				Expect(capacity.Value()).To(Equal(e38LVBytes), "[TC-E38.12] PV capacity = thin LV size")
				pvs, err := e38GetPVS(ctx, pvThin)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] get PillarVolumeState")
				Expect(pvs).NotTo(BeNil(), "[TC-E38.12] PillarVolumeState")
				Expect(pvs.Spec.LVMSource).NotTo(BeNil(), "[TC-E38.12] spec.lvmSource")
				Expect(pvs.Spec.LVMSource.LogicalVolumeUUID).To(Equal(thin.row.LVUUID), "[TC-E38.12] pinned LV UUID")
				Expect(pvs.Spec.LVMSource.PreserveOriginal).To(BeTrue(), "[TC-E38.12] default PreserveOriginal")

				resp, err := e38Inspect(ctx, agent, thin.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] InspectVolume")
				e38ExpectIdentity("TC-E38.12", resp, thin.row)
				Expect(resp.GetLvm().GetSegtype()).To(Equal("thin"), "[TC-E38.12] thin segment")
				Expect(resp.GetLvm().GetPoolLv()).To(Equal(thinPool), "[TC-E38.12] thin pool")

				e38RunPodAndVerify("TC-E38.12", namespace, podThin, workers[1], pvcThin, thin.proofSHA)

				Expect(e36DeletePod(ctx, namespace, podThin)).To(Succeed(), "[TC-E38.12] delete Pod")
				Expect(e38DeletePVC(ctx, namespace, pvcThin)).To(Succeed(), "[TC-E38.12] delete PVC")
				e38WaitVolumeGone(ctx, "TC-E38.12", pvThin)

				row, err := e38LVRow(ctx, storageNode, thin.vg, thin.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] the thin LV must survive")
				Expect(row.identity()).To(Equal(thin.row.identity()),
					"[TC-E38.12] thin LV UUID, size and pool kept")
				poolAfter, err := e38LVRow(ctx, storageNode, thin.vg, thinPool)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] the thin pool must survive")
				Expect(poolAfter.identity()).To(Equal(poolBefore.identity()), "[TC-E38.12] thin pool unchanged")
				sha, err := e38ReadProofRO(ctx, storageNode, thin.device(), "/tmp/e38-verify-"+thin.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.12] read the proof file read-only")
				Expect(sha).To(Equal(thin.proofSHA), "[TC-E38.12] the thin LV data survives")
			})

			// -- TC-E38.13 ---------------------------------------------------
			It("[TC-E38.13] the rebind preflight refuses while the Retain claim's workload is live and nothing is mutated", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()

				Expect(e36Apply(ctx, e38PVCManifest(pvcRetain, namespace, scRetain, retain.importValue()))).
					To(Succeed(), "[TC-E38.13] apply import PVC")
				pvRetain = e38WaitBound(ctx, "TC-E38.13", namespace, pvcRetain)
				track(pvRetain)
				e38RunPodAndVerify("TC-E38.13", namespace, podRetainA, workers[0], pvcRetain, retain.proofSHA)

				pv, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.13] get PV")
				Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain),
					"[TC-E38.13] Retain StorageClass")
				Expect(pv.Spec.ClaimRef).NotTo(BeNil(), "[TC-E38.13] bound PV has a claimRef")
				Expect(pv.Spec.CSI).NotTo(BeNil(), "[TC-E38.13] CSI PV")
				retainClaimUID = string(pv.Spec.ClaimRef.UID)
				retainHandle = pv.Spec.CSI.VolumeHandle
				pvs, err := e38GetPVS(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.13] get PillarVolumeState")
				Expect(pvs).NotTo(BeNil(), "[TC-E38.13] PillarVolumeState")
				retainPVSUID = string(pvs.UID)

				By("running the runbook preflight against the live workload")
				blockers, err := e38RebindBlockers(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.13] preflight steps 1-4")
				Expect(blockers).To(ContainElement("pv-phase=Bound"), "[TC-E38.13] the PV is still bound")
				Expect(blockers).To(ContainElement("claim="+namespace+"/"+pvcRetain), "[TC-E38.13] the old claim is live")
				Expect(blockers).To(ContainElement(HavePrefix("volumeattachment=")), "[TC-E38.13] the PV is attached")
				Expect(blockers).To(ContainElement(HavePrefix("published-nodes=")), "[TC-E38.13] the PVS has a publication")
				resp, err := e38Inspect(ctx, agent, retain.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.13] InspectVolume")
				Expect(e38InspectBlockers(resp, pvs)).To(ContainElement(HavePrefix("export=")),
					"[TC-E38.13] step 7 refuses while the live export remains")

				after, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.13] get PV after")
				Expect(after.Spec.ClaimRef).NotTo(BeNil(), "[TC-E38.13] claimRef kept")
				Expect(string(after.Spec.ClaimRef.UID)).To(Equal(retainClaimUID), "[TC-E38.13] claimRef never changed")
				Expect(after.ResourceVersion).To(Equal(pv.ResourceVersion), "[TC-E38.13] the PV was not written")
			})

			// -- TC-E38.14 ---------------------------------------------------
			It("[TC-E38.14] the terminated Retain workload is unpublished and unstaged, the PV is Released and a competing import is refused", func() {
				if retainPVSUID == "" {
					Fail("[TC-E38.14] MISSING PREREQUISITE: TC-E38.13 did not bind the Retain claim")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				By("terminating the old workload")
				Expect(e36DeletePod(ctx, namespace, podRetainA)).To(Succeed(), "[TC-E38.14] delete Pod")
				Eventually(func(g Gomega) {
					vas, gerr := e38AttachmentsFor(ctx, pvRetain)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(vas).To(BeEmpty(), "VolumeAttachments of %s", pvRetain)
					pvs, gerr := e38GetPVS(ctx, pvRetain)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(pvs).NotTo(BeNil())
					g.Expect(pvs.Status.PublishedNodes).To(BeEmpty(), "publications of %s", pvRetain)
					mounts, gerr := e38NodeMountsOf(ctx, workers[0], pvRetain, retainHandle)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(mounts).To(BeEmpty(), "publish/staging mounts on %s", workers[0])
				}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E38.14] the old workload must be unpublished and unstaged")

				By("deleting the Retain claim")
				Expect(e38DeletePVC(ctx, namespace, pvcRetain)).To(Succeed(), "[TC-E38.14] delete PVC")
				Eventually(func(g Gomega) {
					pv, gerr := e38GetPV(ctx, pvRetain)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(pv).NotTo(BeNil())
					g.Expect(pv.Status.Phase).To(Equal(corev1.VolumeReleased))
				}).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(Succeed(),
					"[TC-E38.14] the PV must become Released")
				pvs, err := e38GetPVS(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.14] get PillarVolumeState")
				Expect(pvs).NotTo(BeNil(), "[TC-E38.14] Retain keeps the PillarVolumeState")
				Expect(string(pvs.UID)).To(Equal(retainPVSUID), "[TC-E38.14] the same lifecycle survives")
				Expect(pvs.Status.Deleting).To(BeFalse(), "[TC-E38.14] DeleteVolume never ran")
				row, err := e38LVRow(ctx, storageNode, retain.vg, retain.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.14] lvs")
				Expect(row.identity()).To(Equal(retain.row.identity()), "[TC-E38.14] LV untouched")

				blockers, err := e38RebindBlockers(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.14] preflight steps 1-4")
				Expect(blockers).To(BeEmpty(), "[TC-E38.14] steps 1-4 pass once the workload is gone")

				By("a competing import of the retained LV")
				pv, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.14] get PV")
				err = e38DirectImport(ctx, agent, retain.vg, retain.lv, retain.row.VGUUID, retain.row.LVUUID,
					"compete-"+retain.lv)
				Expect(status.Code(err)).To(Equal(codes.FailedPrecondition),
					"[TC-E38.14] the agent refuses a competing lifecycle's import: %v", err)
				e38ExpectRefused("TC-E38.14", namespace, scDelete, fmt.Sprintf("e38-compete-%d", proc),
					retain.importValue(), func() {
						after, gerr := e38GetPV(ctx, pvRetain)
						Expect(gerr).NotTo(HaveOccurred(), "[TC-E38.14] get PV after")
						Expect(after.Spec.ClaimRef).NotTo(BeNil(), "[TC-E38.14] claimRef kept")
						Expect(string(after.Spec.ClaimRef.UID)).To(Equal(retainClaimUID),
							"[TC-E38.14] a competing import never re-binds the PV")
						Expect(after.ResourceVersion).To(Equal(pv.ResourceVersion), "[TC-E38.14] the PV was not written")
						resp, ierr := e38Inspect(ctx, agent, retain.volumeID())
						Expect(ierr).NotTo(HaveOccurred(), "[TC-E38.14] InspectVolume")
						Expect(resp.GetFence().GetVolumeUid()).To(Equal(retainPVSUID),
							"[TC-E38.14] the retained lifecycle keeps the mark")
						Expect(resp.GetFence().GetEnded()).To(BeFalse(), "[TC-E38.14] the retained lifecycle is live")
					})
			})

			// -- TC-E38.15 ---------------------------------------------------
			It("[TC-E38.15] under controller quiescence a fenced UnexportVolume leaves the active lifecycle owning an idle LV", func() {
				if retainPVSUID == "" {
					Fail("[TC-E38.15] MISSING PREREQUISITE: TC-E38.13 did not bind the Retain claim")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
				defer cancel()

				By("scaling the controller to zero")
				name, replicas, err := e38ControllerDeployment(ctx)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] controller Deployment")
				Expect(replicas).To(BeNumerically(">", 0), "[TC-E38.15] the controller runs before quiescence")
				controllerDeploy, controllerReplicas = name, replicas
				controllerScaled = true
				Expect(e38ScaleController(ctx, name, 0)).To(Succeed(), "[TC-E38.15] scale controller to 0")

				By("unexporting under the active lifecycle's fence")
				pvs, err := e38GetPVS(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] get PillarVolumeState")
				Expect(pvs).NotTo(BeNil(), "[TC-E38.15] PillarVolumeState")
				_, err = agent.UnexportVolume(ctx, &agentv1.UnexportVolumeRequest{
					VolumeId:     retain.volumeID(),
					ProtocolType: agentv1.ProtocolType_PROTOCOL_TYPE_NVMEOF_TCP,
					Fence: &agentv1.FencingToken{
						VolumeUid:  string(pvs.UID),
						Generation: uint64(pvs.Status.PublicationGeneration),
					},
				})
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] fenced UnexportVolume")

				By("probing the LV read-only after the unexport")
				resp, err := e38Inspect(ctx, agent, retain.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] InspectVolume")
				Expect(e38InspectBlockers(resp, pvs)).To(BeEmpty(),
					"[TC-E38.15] step 7: same active lifecycle, pinned source, no export, no consumer, free claim")
				evidence, err := e38ClaimEvidence(ctx, agentName, retain.volumeID(), resp)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] correlate claim evidence")
				Expect(evidence).To(Equal("claimed:"+pvRetain),
					"[TC-E38.15] the unexport keeps the active lifecycle's claim")
				blockers, err := e38RebindBlockers(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.15] preflight steps 1-4")
				Expect(blockers).To(BeEmpty(), "[TC-E38.15] the runbook may proceed to the bind")
			})

			// -- TC-E38.16 ---------------------------------------------------
			It("[TC-E38.16] the same retained PV rebinds to a static claim without a new PillarVolumeState and serves the same data", func() {
				if !controllerScaled {
					Fail("[TC-E38.16] MISSING PREREQUISITE: TC-E38.15 did not quiesce the controller")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()

				By("a replace with a stale resourceVersion conflicts and changes nothing")
				stalePV, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] read PV")
				_, err = e36Kubectl(ctx, "", "annotate", "pv", pvRetain, "--overwrite",
					"e38.pillar-csi.bhyoo.com/rebind-check="+strconv.FormatInt(time.Now().UnixNano(), 10))
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] advance the PV resourceVersion")
				advanced, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] read advanced PV")
				Expect(advanced.ResourceVersion).NotTo(Equal(stalePV.ResourceVersion), "[TC-E38.16] resourceVersion advanced")
				err = e38ReplaceClaimRef(ctx, stalePV, namespace, pvcRebound)
				Expect(err).To(HaveOccurred(), "[TC-E38.16] a replace carrying the stale resourceVersion must be refused")
				kept, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] read PV after conflict")
				Expect(kept.ResourceVersion).To(Equal(advanced.ResourceVersion), "[TC-E38.16] the stale replace wrote nothing")
				Expect(string(kept.Spec.ClaimRef.UID)).To(Equal(retainClaimUID), "[TC-E38.16] claimRef unchanged")

				By("binding the PV to the new claim with the current resourceVersion")
				Expect(e38ReplaceClaimRef(ctx, kept, namespace, pvcRebound)).To(Succeed(), "[TC-E38.16] replace claimRef")
				Expect(e36Apply(ctx, e38StaticPVCManifest(pvcRebound, namespace, scRetain, pvRetain))).
					To(Succeed(), "[TC-E38.16] apply static PVC")
				bound := e38WaitBound(ctx, "TC-E38.16", namespace, pvcRebound)
				Expect(bound).To(Equal(pvRetain), "[TC-E38.16] the new claim binds the same PV")
				claim, err := e38GetPVC(ctx, namespace, pvcRebound)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] get new PVC")
				Expect(claim.Annotations).NotTo(HaveKey("volume.kubernetes.io/storage-provisioner"),
					"[TC-E38.16] a static bind never asks the provisioner for CreateVolume")
				Expect(claim.Annotations).NotTo(HaveKey("volume.beta.kubernetes.io/storage-provisioner"),
					"[TC-E38.16] a static bind never asks the provisioner for CreateVolume")
				pv, err := e38GetPV(ctx, pvRetain)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] get PV")
				Expect(pv.Spec.CSI.VolumeHandle).To(Equal(retainHandle), "[TC-E38.16] same volume handle")
				owners, err := e38PVSFor(ctx, agentName, retain.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.16] list PillarVolumeStates")
				Expect(owners).To(HaveLen(1), "[TC-E38.16] no new PillarVolumeState")
				Expect(string(owners[0].UID)).To(Equal(retainPVSUID), "[TC-E38.16] the same lifecycle")

				By("scaling the controller back so ReconcileState restores the export")
				Expect(e38ScaleController(ctx, controllerDeploy, controllerReplicas)).
					To(Succeed(), "[TC-E38.16] scale controller back")
				controllerScaled = false
				Eventually(func(g Gomega) {
					resp, ierr := e38Inspect(ctx, agent, retain.volumeID())
					g.Expect(ierr).NotTo(HaveOccurred())
					g.Expect(resp.GetExports()).NotTo(BeEmpty())
					g.Expect(resp.GetFence().GetVolumeUid()).To(Equal(retainPVSUID))
				}).WithContext(ctx).WithTimeout(4*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E38.16] the resync must re-create the export of the same lifecycle")

				e38RunPodAndVerify("TC-E38.16", namespace, podRetainB, workers[1], pvcRebound, retain.proofSHA)
			})

			// -- TC-E38.17 ---------------------------------------------------
			It("[TC-E38.17] releasing the rebound PV under Delete ends the lifecycle and keeps the LV", func() {
				if retainPVSUID == "" {
					Fail("[TC-E38.17] MISSING PREREQUISITE: TC-E38.13 did not bind the Retain claim")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()

				Expect(e36DeletePod(ctx, namespace, podRetainB)).To(Succeed(), "[TC-E38.17] delete Pod")
				_, err := e36Kubectl(ctx, "", "patch", "pv", pvRetain, "--type=merge",
					"-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.17] switch reclaim policy to Delete")
				Expect(e38DeletePVC(ctx, namespace, pvcRebound)).To(Succeed(), "[TC-E38.17] delete PVC")
				e38WaitVolumeGone(ctx, "TC-E38.17", pvRetain)

				row, err := e38LVRow(ctx, storageNode, retain.vg, retain.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.17] the LV must survive")
				Expect(row.identity()).To(Equal(retain.row.identity()), "[TC-E38.17] LV UUID and size kept")
				resp, err := e38Inspect(ctx, agent, retain.volumeID())
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.17] InspectVolume")
				Expect(resp.GetFence().GetVolumeUid()).To(Equal(retainPVSUID), "[TC-E38.17] the same lifecycle ended")
				Expect(resp.GetFence().GetEnded()).To(BeTrue(), "[TC-E38.17] the lifecycle is ended")
				Expect(resp.GetExports()).To(BeEmpty(), "[TC-E38.17] release removes the export")
				sha, err := e38ReadProofRO(ctx, storageNode, retain.device(), "/tmp/e38-verify-"+retain.lv)
				Expect(err).NotTo(HaveOccurred(), "[TC-E38.17] read the proof file read-only")
				Expect(sha).To(Equal(retain.proofSHA), "[TC-E38.17] the original data survives")
			})
		})
	})
