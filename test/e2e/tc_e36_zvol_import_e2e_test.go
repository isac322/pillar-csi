//go:build e2e && e2e_helm

package e2e

// tc_e36_zvol_import_e2e_test.go — E36: adopting an existing ZFS zvol through
// the pillar-csi.bhyoo.com/import-zvol PVC annotation on a real multi-node
// Kind cluster.
//
// The zvol is created, formatted and filled on the storage node outside
// pillar-csi, then claimed by a PVC.  The data written before the import must
// be readable unchanged from Pods on two different nodes (the adopted zvol is
// never reformatted), and the refusals — dataset missing, dataset mounted on
// the storage node, dataset outside the store's parentDataset — must leave
// the PVC Pending with a ProvisioningFailed event naming the reason.
//
// Prerequisites (each is gated with Fail, never Skip):
//   - Multi-node Kind cluster (KUBECONFIG or the suite cluster) with at least
//     two Ready, schedulable nodes running the pillar-csi node plugin.
//   - pillar-csi deployed via Helm with an agent backend entry
//     agent.backends[].zfs {pool: $PILLAR_E2E_ZFS_POOL, parentDataset: <non-empty>}.
//   - PILLAR_E2E_ZFS_POOL: ZFS pool on the storage node.
//   - PILLAR_E2E_BACKEND_CONTAINER: Docker container of the Kind storage node
//     (its name is also the Kubernetes node name).
//   - The storage node container has zfs, mkfs.xfs (xfsprogs), mount and
//     sha256sum, and sees the zvol block devices under /dev/zvol.
//   - NVMe-oF TCP kernel modules loaded on the host.
//
// TC IDs covered: E36.318 – E36.325 (E36.1 subsection)
//
// Run with: go test -tags=e2e,e2e_helm ./test/e2e/ --ginkgo.label-filter="e36"

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
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// e36ZvolSize is the volsize of every zvol the TCs create.  512 MiB keeps
	// mkfs.xfs above its 300 MiB minimum; the zvols are sparse so the pool
	// only pays for the written blocks.
	e36ZvolSize = "512M"
	// e36PVCRequest is the claim size; import accepts any volsize >= request.
	e36PVCRequest = "256Mi"
	// e36ProofFile is the file written on the storage node before the import.
	e36ProofFile = "e36-import-proof.bin"
)

// e36Kubectl runs kubectl against the suite cluster, feeding stdin when
// non-empty, and returns trimmed stdout.
func e36Kubectl(ctx context.Context, stdin string, args ...string) (string, error) {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" && suiteKindCluster != nil {
		kubeconfigPath = suiteKindCluster.KubeconfigPath
	}
	if kubeconfigPath == "" {
		return "", fmt.Errorf("[E36] KUBECONFIG not set — Kind cluster not bootstrapped")
	}
	cmd := exec.CommandContext(ctx, "kubectl", //nolint:gosec
		append([]string{"--kubeconfig=" + kubeconfigPath}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %w\nstderr: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// e36Apply applies a YAML manifest.
func e36Apply(ctx context.Context, manifest string) error {
	_, err := e36Kubectl(ctx, manifest, "apply", "-f", "-")
	return err
}

// e36NodeSh runs a shell script inside the storage node container.
func e36NodeSh(ctx context.Context, node, script string) (string, error) {
	return kindContainerExec(ctx, node, "sh", "-c", script)
}

// e36FailIfNoInfra fails the spec when the E36 environment is incomplete.
func e36FailIfNoInfra() {
	if os.Getenv(suiteZFSPoolEnvVar) == "" {
		Fail("[E36] MISSING PREREQUISITE: " + suiteZFSPoolEnvVar + " not set.\n" +
			"  It must name the ZFS pool on the storage node that the Helm-deployed agent serves\n" +
			"  (agent.backends[].zfs.pool).")
	}
	if os.Getenv(suiteBackendContainerEnvVar) == "" {
		Fail("[E36] MISSING PREREQUISITE: " + suiteBackendContainerEnvVar + " not set.\n" +
			"  It must name the Docker container of the Kind storage node that holds the pool.")
	}
	if os.Getenv("KUBECONFIG") == "" && suiteKindCluster == nil {
		Fail("[E36] MISSING PREREQUISITE: no Kind cluster available (KUBECONFIG unset).")
	}
}

// e36ZvolExists reports whether dataset exists on the storage node.
func e36ZvolExists(ctx context.Context, node, dataset string) (bool, error) {
	out, err := e36NodeSh(ctx, node,
		fmt.Sprintf("zfs list -H -o name %s >/dev/null 2>&1 && echo yes || echo no", dataset))
	if err != nil {
		return false, err
	}
	return out == "yes", nil
}

// e36CreateZvol creates a sparse zvol outside pillar-csi and waits for its
// block device to appear on the storage node.
func e36CreateZvol(ctx context.Context, node, dataset string) error {
	if _, err := e36NodeSh(ctx, node,
		fmt.Sprintf("zfs create -s -V %s %s", e36ZvolSize, dataset)); err != nil {
		return fmt.Errorf("create zvol %s: %w", dataset, err)
	}
	dev := "/dev/zvol/" + dataset
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

// e36DestroyZvol destroys dataset when it still exists.
func e36DestroyZvol(ctx context.Context, node, dataset string) error {
	exists, err := e36ZvolExists(ctx, node, dataset)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = e36NodeSh(ctx, node, "zfs destroy "+dataset)
	if err != nil {
		return fmt.Errorf("destroy zvol %s: %w", dataset, err)
	}
	return nil
}

// e36FormatAndFill formats dataset with XFS, mounts it at mnt on the storage
// node, writes the proof file, and returns its sha256.  The filesystem is left
// mounted; the caller unmounts it (e36Unmount).
func e36FormatAndFill(ctx context.Context, node, dataset, mnt string) (string, error) {
	dev := "/dev/zvol/" + dataset
	script := fmt.Sprintf(
		"set -e; mkfs.xfs -q %[1]s; mkdir -p %[2]s; mount %[1]s %[2]s; "+
			"head -c 4194304 /dev/urandom > %[2]s/%[3]s; sync; "+
			"sha256sum %[2]s/%[3]s | cut -d' ' -f1",
		dev, mnt, e36ProofFile)
	out, err := e36NodeSh(ctx, node, script)
	if err != nil {
		return "", fmt.Errorf("format/mount/fill %s at %s: %w", dev, mnt, err)
	}
	return out, nil
}

// e36Unmount unmounts mnt on the storage node when it is mounted and removes
// the directory.
func e36Unmount(ctx context.Context, node, mnt string) error {
	_, err := e36NodeSh(ctx, node, fmt.Sprintf(
		"if mountpoint -q %[1]s; then umount %[1]s; fi; rmdir %[1]s 2>/dev/null || true", mnt))
	if err != nil {
		return fmt.Errorf("unmount %s: %w", mnt, err)
	}
	return nil
}

// e36PVCManifest renders a PVC that imports dataset.
func e36PVCManifest(name, namespace, storageClass, dataset string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  annotations:
    pillar-csi.bhyoo.com/import-zvol: %q
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %s
  resources:
    requests:
      storage: %s
`, name, namespace, dataset, storageClass, e36PVCRequest)
}

// e36PodManifest renders a Pod pinned to node that mounts the claim at /data.
func e36PodManifest(name, namespace, node, claim string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  nodeSelector:
    kubernetes.io/hostname: %s
  containers:
  - name: reader
    image: busybox
    command: ["/bin/sh", "-c", "sleep 3600"]
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: %s
`, name, namespace, node, claim)
}

// e36ExpectRefused creates an import PVC for dataset and asserts it stays
// Pending with a ProvisioningFailed event whose message contains reason.  The
// PVC is deleted before returning so no provisioning retry outlives the TC.
func e36ExpectRefused(tc, namespace, storageClass, pvc, dataset, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	DeferCleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer ccancel()
		_, err := e36Kubectl(cctx, "", "delete", "pvc", pvc, "-n", namespace,
			"--ignore-not-found=true", "--wait=true", "--timeout=60s")
		Expect(err).NotTo(HaveOccurred(), "[%s] delete refused PVC", tc)
	})

	By("creating the import PVC for " + dataset)
	Expect(e36Apply(ctx, e36PVCManifest(pvc, namespace, storageClass, dataset))).
		To(Succeed(), "[%s] apply PVC", tc)

	By("waiting for a ProvisioningFailed event naming the refusal")
	Eventually(func(g Gomega) {
		msgs, err := e36Kubectl(ctx, "", "get", "events", "-n", namespace,
			"--field-selector",
			"involvedObject.kind=PersistentVolumeClaim,involvedObject.name="+pvc+",reason=ProvisioningFailed",
			"-o", `jsonpath={range .items[*]}{.message}{"\n"}{end}`)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(msgs).To(ContainSubstring(reason))
	}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
		"[%s] ProvisioningFailed event must contain %q", tc, reason)

	By("verifying the PVC is still Pending and unbound")
	phase, err := e36Kubectl(ctx, "", "get", "pvc", pvc, "-n", namespace, "-o", "jsonpath={.status.phase}")
	Expect(err).NotTo(HaveOccurred(), "[%s] get PVC phase", tc)
	Expect(phase).To(Equal("Pending"), "[%s] refused PVC must stay Pending", tc)
	volName, err := e36Kubectl(ctx, "", "get", "pvc", pvc, "-n", namespace, "-o", "jsonpath={.spec.volumeName}")
	Expect(err).NotTo(HaveOccurred(), "[%s] get PVC volumeName", tc)
	Expect(volName).To(BeEmpty(), "[%s] refused PVC must not be bound to a PV", tc)
}

// e36ReadyCondition returns the status of the Ready condition of a
// cluster-scoped pillar-csi resource.
func e36ReadyCondition(ctx context.Context, kind, name string) (string, error) {
	return e36Kubectl(ctx, "", "get", kind, name,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
}

// e36PickWorkers returns two Ready, schedulable nodes, preferring nodes other
// than the storage node so both mounts cross the NVMe/TCP fabric.
func e36PickWorkers(ctx context.Context, storageNode string) ([]string, error) {
	out, err := e36Kubectl(ctx, "", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
				Taints        []struct {
					Effect string `json:"effect"`
				} `json:"taints"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode nodes: %w", err)
	}
	var others, storage []string
	for _, n := range list.Items {
		if n.Spec.Unschedulable {
			continue
		}
		blocked := false
		for _, t := range n.Spec.Taints {
			if t.Effect == "NoSchedule" || t.Effect == "NoExecute" {
				blocked = true
			}
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
		}
		if blocked || !ready {
			continue
		}
		if n.Metadata.Name == storageNode {
			storage = append(storage, n.Metadata.Name)
		} else {
			others = append(others, n.Metadata.Name)
		}
	}
	return append(others, storage...), nil
}

// e36ExistingAgentFor returns the PillarAgent whose nodeRef is node, or "".
func e36ExistingAgentFor(ctx context.Context, node string) (string, error) {
	out, err := e36Kubectl(ctx, "", "get", "pillaragents", "-o", "json")
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeRef *struct {
					Name string `json:"name"`
				} `json:"nodeRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return "", fmt.Errorf("decode pillaragents: %w", err)
	}
	for _, a := range list.Items {
		if a.Spec.NodeRef != nil && a.Spec.NodeRef.Name == node {
			return a.Metadata.Name, nil
		}
	}
	return "", nil
}

// e36PodSHA returns the sha256 of the proof file as read inside pod.
func e36PodSHA(ctx context.Context, namespace, pod string) (string, error) {
	out, err := e36Kubectl(ctx, "", "exec", "-n", namespace, pod, "--",
		"sha256sum", "/data/"+e36ProofFile)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty sha256sum output from pod %s", pod)
	}
	return fields[0], nil
}

// e36RunPodAndVerify starts a Pod on node mounting claim, waits for Running and
// asserts the proof file hash equals want.
func e36RunPodAndVerify(tc, namespace, pod, node, claim, want string) {
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
	got, err := e36PodSHA(ctx, namespace, pod)
	Expect(err).NotTo(HaveOccurred(), "[%s] sha256sum inside Pod", tc)
	Expect(got).To(Equal(want),
		"[%s] file written before the import must be unchanged (zvol must not be reformatted)", tc)
}

// e36DeletePod deletes pod and waits until it is gone.
func e36DeletePod(ctx context.Context, namespace, pod string) error {
	_, err := e36Kubectl(ctx, "", "delete", "pod", pod, "-n", namespace,
		"--ignore-not-found=true", "--wait=true", "--timeout=120s")
	return err
}

var _ = Describe("E36: ZFS zvol import — 기존 zvol 채택 (Kind 클러스터 E2E)",
	Label("zfs", "import", "e36"),
	func() {
		Describe("E36.1 import-zvol 어노테이션으로 기존 zvol 채택", Ordered, func() {
			var (
				proc         = GinkgoParallelProcess()
				storageNode  string
				pool         string
				parent       string
				agentName    string
				namespace    = fmt.Sprintf("e36-import-%d", proc)
				storeName    = fmt.Sprintf("e36-store-%d", proc)
				protocolName = fmt.Sprintf("e36-proto-%d", proc)
				pscName      = fmt.Sprintf("e36-psc-%d", proc)
				scName       = fmt.Sprintf("e36-zvol-import-%d", proc)
				importPVC    = fmt.Sprintf("e36-import-%d", proc)
				podA         = fmt.Sprintf("e36-reader-a-%d", proc)
				podB         = fmt.Sprintf("e36-reader-b-%d", proc)
				importLeaf   = fmt.Sprintf("e36-import-%d", proc)
				importDS     string
				volsize      int64
				proofSHA     string
				workers      []string
				pvName       string
			)

			BeforeAll(func() {
				e36FailIfNoInfra()
				storageNode = os.Getenv(suiteBackendContainerEnvVar)
				pool = os.Getenv(suiteZFSPoolEnvVar)

				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
				defer cancel()

				By("checking storage-node tools")
				_, err := e36NodeSh(ctx, storageNode,
					"command -v zfs && command -v mkfs.xfs && command -v mountpoint && command -v sha256sum")
				Expect(err).NotTo(HaveOccurred(),
					"[E36] MISSING PREREQUISITE: storage node %s needs zfs, mkfs.xfs (xfsprogs), "+
						"mountpoint and sha256sum", storageNode)

				By("picking two schedulable nodes for the reader Pods")
				workers, err = e36PickWorkers(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E36] list nodes")
				Expect(len(workers)).To(BeNumerically(">=", 2),
					"[E36] MISSING PREREQUISITE: need at least two Ready, schedulable nodes, got %v", workers)

				By("creating the test namespace")
				Expect(e36Apply(ctx, fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n",
					namespace))).To(Succeed(), "[E36] create namespace")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer ccancel()
					_, derr := e36Kubectl(cctx, "", "delete", "namespace", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=150s")
					Expect(derr).NotTo(HaveOccurred(), "[E36] delete namespace")
				})

				By("resolving the PillarAgent of the storage node")
				agentName, err = e36ExistingAgentFor(ctx, storageNode)
				Expect(err).NotTo(HaveOccurred(), "[E36] list PillarAgents")
				if agentName == "" {
					agentName = fmt.Sprintf("e36-agent-%d", proc)
					Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarAgent
metadata:
  name: %s
spec:
  nodeRef:
    name: %s
    addressType: InternalIP
`, agentName, storageNode))).To(Succeed(), "[E36] create PillarAgent")
					created := agentName
					DeferCleanup(func() {
						cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer ccancel()
						_, derr := e36Kubectl(cctx, "", "delete", "pillaragent", created,
							"--ignore-not-found=true", "--wait=true", "--timeout=90s")
						Expect(derr).NotTo(HaveOccurred(), "[E36] delete PillarAgent")
					})
				}
				Eventually(func(g Gomega) {
					st, gerr := e36ReadyCondition(ctx, "pillaragent", agentName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(st).To(Equal("True"))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E36] PillarAgent %s must become Ready", agentName)

				By("reading the agent's parentDataset for the pool")
				parent, err = e36Kubectl(ctx, "", "get", "pillaragent", agentName, "-o",
					fmt.Sprintf(`jsonpath={.status.discoveredPools[?(@.name=="%s")].parentDataset}`, pool))
				Expect(err).NotTo(HaveOccurred(), "[E36] read discovered pool")
				parent = strings.Trim(parent, "/")
				Expect(parent).NotTo(BeEmpty(),
					"[E36] MISSING PREREQUISITE: the agent must serve pool %q with a non-empty "+
						"parentDataset (Helm agent.backends[].zfs.parentDataset); the outside-parent "+
						"refusal needs a dataset boundary", pool)
				importDS = pool + "/" + parent + "/" + importLeaf

				By("creating the PillarStore / PillarProtocol / PillarStorageClass stack")
				Expect(e36Apply(ctx, fmt.Sprintf(`apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStore
metadata:
  name: %s
spec:
  agentRef: %s
  backend:
    zfs:
      volumeType: zvol
      pool: %s
      parentDataset: %s
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarProtocol
metadata:
  name: %s
spec:
  protocol:
    nvmeofTcp:
      port: 4420
---
apiVersion: pillar-csi.bhyoo.com/v1alpha1
kind: PillarStorageClass
metadata:
  name: %s
spec:
  storeRef: %s
  protocolRef: %s
  storageClass:
    name: %s
    reclaimPolicy: Delete
    volumeBindingMode: Immediate
  filesystem:
    fsType: xfs
`, storeName, agentName, pool, parent, protocolName, pscName, storeName, protocolName, scName))).
					To(Succeed(), "[E36] apply CR stack")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer ccancel()
					for _, r := range [][2]string{
						{"pillarstorageclass", pscName},
						{"pillarprotocol", protocolName},
						{"pillarstore", storeName},
					} {
						_, derr := e36Kubectl(cctx, "", "delete", r[0], r[1],
							"--ignore-not-found=true", "--wait=true", "--timeout=60s")
						Expect(derr).NotTo(HaveOccurred(), "[E36] delete %s %s", r[0], r[1])
					}
				})
				Eventually(func(g Gomega) {
					st, gerr := e36ReadyCondition(ctx, "pillarstorageclass", pscName)
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(st).To(Equal("True"))
					prov, gerr := e36Kubectl(ctx, "", "get", "storageclass", scName,
						"-o", "jsonpath={.provisioner}")
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(prov).To(Equal("pillar-csi.bhyoo.com"))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[E36] PillarStorageClass %s must be Ready with StorageClass %s", pscName, scName)

				By("creating the zvol outside pillar-csi and writing the proof file")
				parentExisted, err := e36ZvolExists(ctx, storageNode, pool+"/"+parent)
				Expect(err).NotTo(HaveOccurred(), "[E36] probe parent dataset")
				if !parentExisted {
					_, err = e36NodeSh(ctx, storageNode, "zfs create -p "+pool+"/"+parent)
					Expect(err).NotTo(HaveOccurred(), "[E36] create parent dataset")
					DeferCleanup(func() {
						cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
						defer ccancel()
						_, derr := e36NodeSh(cctx, storageNode, "zfs destroy "+pool+"/"+parent)
						Expect(derr).NotTo(HaveOccurred(), "[E36] destroy parent dataset created by the TC")
					})
				}
				Expect(e36CreateZvol(ctx, storageNode, importDS)).To(Succeed(), "[E36] create import zvol")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
					defer ccancel()
					Expect(e36DestroyZvol(cctx, storageNode, importDS)).To(Succeed(),
						"[E36] destroy import zvol")
				})
				mnt := fmt.Sprintf("/tmp/e36-mnt-import-%d", proc)
				proofSHA, err = e36FormatAndFill(ctx, storageNode, importDS, mnt)
				unmountErr := e36Unmount(ctx, storageNode, mnt)
				Expect(err).NotTo(HaveOccurred(), "[E36] format and fill import zvol")
				Expect(unmountErr).NotTo(HaveOccurred(), "[E36] unmount import zvol")
				Expect(proofSHA).To(MatchRegexp(`^[0-9a-f]{64}$`), "[E36] proof sha256")

				sizeOut, err := e36NodeSh(ctx, storageNode, "zfs get -Hp -o value volsize "+importDS)
				Expect(err).NotTo(HaveOccurred(), "[E36] read volsize")
				volsize, err = strconv.ParseInt(sizeOut, 10, 64)
				Expect(err).NotTo(HaveOccurred(), "[E36] parse volsize %q", sizeOut)

				// Registered last so it runs first: Pods and the claim go away
				// (and the PV with its PillarVolumeState) before the zvol, the
				// CR stack and the namespace are removed.
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 4*time.Minute)
					defer ccancel()
					for _, p := range []string{podA, podB} {
						Expect(e36DeletePod(cctx, namespace, p)).To(Succeed(), "[E36] delete Pod %s", p)
					}
					_, derr := e36Kubectl(cctx, "", "delete", "pvc", importPVC, "-n", namespace,
						"--ignore-not-found=true", "--wait=true", "--timeout=90s")
					Expect(derr).NotTo(HaveOccurred(), "[E36] delete import PVC")
					if pvName == "" {
						return
					}
					Eventually(func(g Gomega) {
						out, gerr := e36Kubectl(cctx, "", "get", "pv", pvName, "--ignore-not-found=true",
							"-o", "name")
						g.Expect(gerr).NotTo(HaveOccurred())
						g.Expect(out).To(BeEmpty())
						out, gerr = e36Kubectl(cctx, "", "get", "pillarvolumestate", pvName,
							"--ignore-not-found=true", "-o", "name")
						g.Expect(gerr).NotTo(HaveOccurred())
						g.Expect(out).To(BeEmpty())
					}).WithContext(cctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
						"[E36] PV and PillarVolumeState %s must be removed", pvName)
				})
			})

			// ── TC-E36.318 ────────────────────────────────────────────────────
			It("[TC-E36.318] import PVC binds and the PV capacity equals the zvol volsize", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
				defer cancel()

				Expect(e36Apply(ctx, e36PVCManifest(importPVC, namespace, scName, importDS))).
					To(Succeed(), "[TC-E36.318] apply import PVC")
				Eventually(func(g Gomega) {
					phase, err := e36Kubectl(ctx, "", "get", "pvc", importPVC, "-n", namespace,
						"-o", "jsonpath={.status.phase}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(phase).To(Equal("Bound"))
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E36.318] import PVC must become Bound")

				var err error
				pvName, err = e36Kubectl(ctx, "", "get", "pvc", importPVC, "-n", namespace,
					"-o", "jsonpath={.spec.volumeName}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.318] get PV name")
				Expect(pvName).NotTo(BeEmpty(), "[TC-E36.318] bound PVC must name its PV")

				capStr, err := e36Kubectl(ctx, "", "get", "pv", pvName, "-o", "jsonpath={.spec.capacity.storage}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.318] get PV capacity")
				q, err := resource.ParseQuantity(capStr)
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.318] parse PV capacity %q", capStr)
				Expect(q.Value()).To(Equal(volsize),
					"[TC-E36.318] PV capacity must equal the adopted zvol volsize, not the request")
			})

			// ── TC-E36.319 ────────────────────────────────────────────────────
			It("[TC-E36.319] PillarVolumeState records spec.importedFrom and the adopted backend volume", func() {
				if pvName == "" {
					Fail("[TC-E36.319] MISSING PREREQUISITE: TC-E36.318 did not bind the import PVC")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				importedFrom, err := e36Kubectl(ctx, "", "get", "pillarvolumestate", pvName,
					"-o", "jsonpath={.spec.importedFrom}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.319] get PillarVolumeState")
				Expect(importedFrom).To(Equal(importDS), "[TC-E36.319] spec.importedFrom")

				agentVolID, err := e36Kubectl(ctx, "", "get", "pillarvolumestate", pvName,
					"-o", "jsonpath={.spec.agentVolumeID}")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.319] get agentVolumeID")
				Expect(agentVolID).To(Equal(pool+"/"+importLeaf),
					"[TC-E36.319] agentVolumeID must address the adopted zvol")
			})

			// ── TC-E36.320 ────────────────────────────────────────────────────
			It("[TC-E36.320] a Pod on the first node reads the file written before the import", func() {
				if pvName == "" {
					Fail("[TC-E36.320] MISSING PREREQUISITE: TC-E36.318 did not bind the import PVC")
				}
				e36RunPodAndVerify("TC-E36.320", namespace, podA, workers[0], importPVC, proofSHA)
			})

			// ── TC-E36.321 ────────────────────────────────────────────────────
			It("[TC-E36.321] after the first Pod is deleted a Pod on a second node reads the same file", func() {
				if pvName == "" {
					Fail("[TC-E36.321] MISSING PREREQUISITE: TC-E36.318 did not bind the import PVC")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				Expect(e36DeletePod(ctx, namespace, podA)).To(Succeed(), "[TC-E36.321] delete first Pod")
				e36RunPodAndVerify("TC-E36.321", namespace, podB, workers[1], importPVC, proofSHA)
			})

			// ── TC-E36.322 ────────────────────────────────────────────────────
			It("[TC-E36.322] importing a dataset that does not exist is refused", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				missing := fmt.Sprintf("%s/%s/e36-missing-%d", pool, parent, proc)
				exists, err := e36ZvolExists(ctx, storageNode, missing)
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.322] probe dataset")
				Expect(exists).To(BeFalse(), "[TC-E36.322] %s must not exist before the TC", missing)

				e36ExpectRefused("TC-E36.322", namespace, scName,
					fmt.Sprintf("e36-missing-%d", proc), missing, "refused: missing")
			})

			// ── TC-E36.323 ────────────────────────────────────────────────────
			It("[TC-E36.323] importing a zvol mounted on the storage node is refused", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				mounted := fmt.Sprintf("%s/%s/e36-mounted-%d", pool, parent, proc)
				mnt := fmt.Sprintf("/tmp/e36-mnt-mounted-%d", proc)

				Expect(e36CreateZvol(ctx, storageNode, mounted)).To(Succeed(), "[TC-E36.323] create zvol")
				// Registered before e36ExpectRefused's PVC cleanup, so it runs
				// after it: the claim is gone before the zvol is released.
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
					defer ccancel()
					Expect(e36Unmount(cctx, storageNode, mnt)).To(Succeed(), "[TC-E36.323] unmount")
					Expect(e36DestroyZvol(cctx, storageNode, mounted)).To(Succeed(), "[TC-E36.323] destroy zvol")
				})
				_, err := e36FormatAndFill(ctx, storageNode, mounted, mnt)
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.323] format and mount zvol on the storage node")

				e36ExpectRefused("TC-E36.323", namespace, scName,
					fmt.Sprintf("e36-mounted-%d", proc), mounted, "refused: in use")

				out, err := e36NodeSh(ctx, storageNode, "mountpoint -q "+mnt+" && echo mounted")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.323] mount must still be held")
				Expect(out).To(Equal("mounted"), "[TC-E36.323] mount must still be held")
			})

			// ── TC-E36.324 ────────────────────────────────────────────────────
			It("[TC-E36.324] importing a zvol outside the store's parentDataset is refused", func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				outside := fmt.Sprintf("%s/e36-outside-%d", pool, proc)

				Expect(e36CreateZvol(ctx, storageNode, outside)).To(Succeed(), "[TC-E36.324] create zvol")
				DeferCleanup(func() {
					cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
					defer ccancel()
					Expect(e36DestroyZvol(cctx, storageNode, outside)).To(Succeed(), "[TC-E36.324] destroy zvol")
				})

				e36ExpectRefused("TC-E36.324", namespace, scName,
					fmt.Sprintf("e36-outside-%d", proc), outside, "is not under the PillarStore's parent dataset")
			})

			// ── TC-E36.325 ────────────────────────────────────────────────────
			It("[TC-E36.325] deleting the import PVC removes its PV and PillarVolumeState", func() {
				if pvName == "" {
					Fail("[TC-E36.325] MISSING PREREQUISITE: TC-E36.318 did not bind the import PVC")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				Expect(e36DeletePod(ctx, namespace, podB)).To(Succeed(), "[TC-E36.325] delete second Pod")
				_, err := e36Kubectl(ctx, "", "delete", "pvc", importPVC, "-n", namespace,
					"--wait=true", "--timeout=90s")
				Expect(err).NotTo(HaveOccurred(), "[TC-E36.325] delete import PVC")

				Eventually(func(g Gomega) {
					out, gerr := e36Kubectl(ctx, "", "get", "pv", pvName, "--ignore-not-found=true", "-o", "name")
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty())
					out, gerr = e36Kubectl(ctx, "", "get", "pillarvolumestate", pvName,
						"--ignore-not-found=true", "-o", "name")
					g.Expect(gerr).NotTo(HaveOccurred())
					g.Expect(out).To(BeEmpty())
				}).WithContext(ctx).WithTimeout(3*time.Minute).WithPolling(5*time.Second).Should(Succeed(),
					"[TC-E36.325] PV and PillarVolumeState must be removed after the claim is deleted")
			})
		})
	})
